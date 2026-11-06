"""Tests for mutation_sandbox.py. They start no sandbox and no Go command.

Run: python3 -m unittest discover -s scripts -p "test_*.py"
"""

import contextlib
import io
import json
import os
import re
import socket
import sys
import tempfile
import tarfile
import subprocess
import time
import unittest
from argparse import Namespace
from pathlib import Path
from unittest import mock

import mutation_sandbox as sb

LINUX = sys.platform == "linux"


def tmpdir(test):
    folder = tempfile.TemporaryDirectory()
    test.addCleanup(folder.cleanup)
    return Path(folder.name)


def sandbox(work: Path, masks=()) -> sb.Sandbox:
    """A Sandbox with its folders named but nothing staged."""
    s = sb.Sandbox.__new__(sb.Sandbox)
    s.args = Namespace(work=work, source=work / "repo", go_root=work / "goroot", gremlins=work / "gremlins",
                       module_cache=work / "modcache-host", report_dir=work / "out", summary=None, packages=None,
                       bwrap="/usr/bin/bwrap", timeout=60.0, probe_only=True)
    s.work = work
    s.bwrap = "/usr/bin/bwrap"
    s.nonce = "n0nce"
    s.dirs = {name: work / name for name in ("src", "go", "tools", "proxy", "modcache", "gocache", "gopath", "home",
                                             "tmp", "report", "probe", "etc", "logs")}
    s.host = [sb.HostMount("/", "/", "ext4")]
    s.masks = list(masks)
    s.evidence = {"stages": []}
    s.probes = 0
    s.go_version = "go1.27.3"
    s.tool = {"module": "github.com/go-gremlins/gremlins", "version": "v0.6.0", "sum": "h1:x="}
    return s


class ProbeChecks(unittest.TestCase):
    def test_launcher_expects_exactly_the_probes_checks_in_order(self):
        source = (sb.PROBE_SOURCE / "probe_test.go").read_text(encoding="utf-8")
        table = source.split("var checks = []struct", 1)[1].split("\n}\n", 1)[0]
        self.assertEqual(tuple(re.findall(r'\{"([a-z0-9-]+)", \w+\}', table)), sb.PROBE_CHECKS)

    def test_probe_is_its_own_module(self):
        # Keeps it out of `go list ./...`, vet, deadcode and the mutation scope.
        self.assertTrue((sb.PROBE_SOURCE / "go.mod").is_file())


class Staging(unittest.TestCase):
    def test_copy_skips_git_and_refuses_symlinks_that_leave_the_tree(self):
        root = tmpdir(self)
        src = root / "src"
        (src / ".git").mkdir(parents=True)
        (src / ".git" / "config").write_text("x", encoding="utf-8")
        (src / "a").mkdir()
        (src / "a" / "f.go").write_text("package a\n", encoding="utf-8")
        self.assertEqual(sb.copy_tree(src, root / "dst", sb.SKIP_COPY), 1)
        self.assertFalse((root / "dst" / ".git").exists())
        if not LINUX:
            return
        (src / "a" / "inside").symlink_to("f.go")
        self.assertEqual(sb.copy_tree(src, root / "dst2", sb.SKIP_COPY), 2)
        self.assertEqual(os.readlink(root / "dst2" / "a" / "inside"), "f.go")
        for link in ("../../outside", str(root)):
            with self.subTest(link=link):
                (src / "a" / "bad").unlink(missing_ok=True)
                (src / "a" / "bad").symlink_to(link)
                with self.assertRaisesRegex(sb.SandboxError, "refusing symlink"):
                    sb.copy_tree(src, root / f"dst-{abs(hash(link))}", sb.SKIP_COPY)

    def test_module_paths_are_escaped_like_the_go_proxy(self):
        self.assertEqual(sb.escape_module("github.com/HemSoft/X"), "github.com/!hem!soft/!x")

    def test_proxy_holds_only_the_go_sum_modules(self):
        root = tmpdir(self)
        cache = root / "cache" / "cache" / "download"
        for module, version in (("example.com/a", "v1.0.0"), ("example.com/Up", "v2.0.0"), ("example.com/unused", "v9.0.0")):
            folder = cache / sb.escape_module(module) / "@v"
            folder.mkdir(parents=True)
            for ext in (".mod", ".zip", ".info"):
                (folder / (version + ext)).write_text(module, encoding="utf-8")
        go_sum = root / "go.sum"
        go_sum.write_text("example.com/a v1.0.0 h1:a=\nexample.com/a v1.0.0/go.mod h1:b=\nexample.com/Up v2.0.0/go.mod h1:c=\nexample.com/unused-old v0.1.0/go.mod h1:old=\n", encoding="utf-8")
        proxy = root / "proxy"
        self.assertEqual(sb.stage_proxy(go_sum, root / "cache", proxy), 6)
        self.assertTrue((proxy / "example.com/!up/@v/v2.0.0.mod").is_file())
        self.assertFalse((proxy / "example.com/unused").exists())
        go_sum.write_text("example.com/missing v1.0.0 h1:a=\n", encoding="utf-8")
        with self.assertRaisesRegex(sb.SandboxError, "go mod download"):
            sb.stage_proxy(go_sum, root / "cache", root / "proxy2")

    def test_go_version_comes_from_go_mod(self):
        root = tmpdir(self)
        (root / "go.mod").write_text("module x\n\ngo 1.27.3\n", encoding="utf-8")
        self.assertEqual(sb.go_mod_version(root), "go1.27.3")


@unittest.skipUnless(os.name == "posix", "the launcher builds POSIX sandbox paths; it runs only on Linux")
class Mounts(unittest.TestCase):
    TEXT = ("22 1 259:2 / / rw,relatime shared:1 - ext4 /dev/nvme0n1p2 rw\n"
            "30 22 259:3 / /home rw shared:2 - ext4 /dev/nvme0n1p3 rw\n"
            "31 22 259:2 /data/sub /srv/with\\040space rw - ext4 /dev/nvme0n1p2 rw\n")

    def test_bind_roots_follow_the_containing_host_mount(self):
        mounts = sb.host_mounts(self.TEXT)
        self.assertEqual(mounts[2].point, "/srv/with space")
        with mock.patch.object(sb.os.path, "realpath", side_effect=lambda p: str(p)):
            self.assertEqual(sb.expected_root(Path("/usr"), mounts), ("/usr", "ext4"))
            self.assertEqual(sb.expected_root(Path("/home/u/.cache/w/src"), mounts), ("/u/.cache/w/src", "ext4"))
            self.assertEqual(sb.expected_root(Path("/srv/with space/x"), mounts), ("/data/sub/x", "ext4"))
            self.assertEqual(sb.expected_root(Path("/home"), mounts), ("/", "ext4"))


@unittest.skipUnless(os.name == "posix", "the launcher builds POSIX sandbox paths; it runs only on Linux")
class Arguments(unittest.TestCase):
    def test_bwrap_isolates_every_namespace_and_mounts_only_the_allowlist(self):
        work = Path("/scratch/work")
        s = sandbox(work, masks=["/usr/bin/gh"])
        with mock.patch.dict(os.environ, {sb.ENV_CANARY: "synthetic-canary"}):
            for stage in ("download", "mutation"):
                argv = s.argv(stage, s.env(stage), ["go", "version"], sb.S_SRC)
                with self.subTest(stage=stage):
                    for flag in ("--unshare-user", "--unshare-ipc", "--unshare-pid", "--unshare-net", "--unshare-uts",
                                 "--unshare-cgroup", "--disable-userns", "--assert-userns-disabled", "--die-with-parent",
                                 "--new-session", "--clearenv"):
                        self.assertIn(flag, argv)
                    self.assertNotIn("--share-net", argv)
                    self.assertEqual(argv[argv.index("--proc") + 1], "/proc")
                    self.assertEqual(argv[-3:], ["--", "go", "version"])
                    self.assertNotIn("synthetic-canary", " ".join(argv))
                    binds = [(argv[i], argv[i + 1], argv[i + 2]) for i, a in enumerate(argv) if a in ("--bind", "--ro-bind")]
                    sources = {src for _, src, _ in binds}
                    self.assertEqual(sources - {"/usr"}, {src for src in sources if src.startswith(str(work) + "/")})
                    self.assertNotIn(os.path.expanduser("~"), sources)
                    self.assertFalse(any(dest in ("/", "/run", "/etc", "/proc", "/home") for _, _, dest in binds))
                    self.assertIn(("--ro-bind", "/usr", "/usr"), binds)
                    self.assertIn(("--ro-bind", f"{work}/etc/refuse", "/usr/bin/gh"), binds)
                    self.assertIn(("--ro-bind", f"{work}/src", sb.S_SRC), binds)
                    modcache = [b for b in binds if b[2] == sb.S_MODCACHE][0]
                    self.assertEqual(modcache[0], "--bind" if stage == "download" else "--ro-bind")
                    self.assertEqual(stage == "download", any(dest == sb.S_PROXY for _, _, dest in binds))
                    remounted = [argv[i + 1] for i, a in enumerate(argv) if a == "--remount-ro"]
                    self.assertEqual(remounted[-1], "/")
                    if os.path.isdir("/usr/local"):
                        self.assertIn("/usr/local", remounted)

    def test_environment_is_built_from_scratch(self):
        s = sandbox(Path("/scratch/work"))
        with mock.patch.dict(os.environ, {"GH_TOKEN": "synthetic", "HOME": "/home/real", sb.ENV_CANARY: "synthetic"}):
            env = s.env("mutation")
        self.assertEqual(env["HOME"], sb.S_HOME)
        self.assertEqual((env["GOPROXY"], env["GOFLAGS"], env["GOTOOLCHAIN"], env["GOENV"]), ("off", "-mod=readonly", "local", "off"))
        self.assertNotIn("synthetic", json.dumps(env))
        self.assertNotIn("/home/real", json.dumps(env))
        self.assertNotIn("GH_TOKEN", env)
        self.assertEqual(s.env("download")["GOPROXY"], f"file://{sb.S_PROXY}")

    def test_bwrap_starts_with_a_minimal_environment_and_no_descriptors(self):
        # bwrap's init stays visible inside the sandbox, so its own
        # environment must not carry the caller's.
        root = tmpdir(self)
        s = sandbox(root)
        s.dirs["logs"].mkdir(parents=True)
        seen = {}

        class FakeProc:
            stdout = iter(["line\n"])
            returncode = 0

            def __init__(self, argv, **kw):
                seen.update(kw)

            def wait(self):
                return 0

            def kill(self):
                pass

        with mock.patch.object(sb.subprocess, "Popen", FakeProc), mock.patch.dict(os.environ, {sb.ENV_CANARY: "synthetic"}), \
                contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(s.execute("x", ["/usr/bin/bwrap"], 60), 0)
        self.assertEqual(seen["env"], {"PATH": "/usr/bin:/bin"})
        self.assertTrue(seen["close_fds"])
        self.assertEqual(seen["stdin"], sb.subprocess.DEVNULL)
        self.assertEqual(seen["cwd"], "/")


class FakeCanaries:
    abstract = "abstract"
    marker = "marker"
    env_value = "synthetic"
    tcp_addr = "127.0.0.1:1"

    def __init__(self, outside: Path, tripped=False):
        self.outside = outside
        self.file = outside / "canary.txt"
        self.unix_path = outside / "host.sock"
        self.pairing = outside / "bin" / "t3"
        outside.mkdir(parents=True)
        self.file.write_text("synthetic canary\n", encoding="utf-8")
        self.tripped = tripped

    def counts(self):
        return {"tcp": 0, "unix": 0, "abstract": 0}

    def socket_inodes(self):
        return [1, 2, 3]

    def post_checks(self, baseline):
        if self.tripped:
            raise sb.SandboxError("host canaries tripped: tcp canary got 1 connections")
        return {"connections_since_baseline": baseline}


@unittest.skipUnless(LINUX, "these tests exercise the Linux launcher boundary")
class ProbeVerdict(unittest.TestCase):
    def run_probe(self, results, code=0, tripped=False):
        root = tmpdir(self)
        s = sandbox(root)
        for d in s.dirs.values():
            d.mkdir(parents=True, exist_ok=True)

        def fake_execute(name, argv, timeout, stream=False):
            (s.dirs["report"] / "sandbox" / f"{name}.json").write_text(json.dumps(results), encoding="utf-8")
            return code

        fake = FakeCanaries(root / "outside", tripped)
        with mock.patch.object(s, "execute", side_effect=fake_execute), \
                mock.patch.object(s, "expected_mounts", return_value={"/": {"root": "/newroot", "fstype": "tmpfs", "read_only": True}}), \
                contextlib.redirect_stdout(io.StringIO()):
            return s.probe("mutation", fake, "before"), s

    def all_ok(self):
        return [{"name": n, "ok": True, "detail": "ok"} for n in sb.PROBE_CHECKS]

    def test_all_checks_pass(self):
        record, s = self.run_probe(self.all_ok())
        self.assertEqual(record["go_test_exit"], 0)
        config = json.loads((s.dirs["etc"] / "probe-1.json").read_text(encoding="utf-8"))
        self.assertEqual(config["env_canary_sha256"], sb.sha256_bytes(b"synthetic"))
        self.assertNotIn("synthetic\"", json.dumps(config))
        self.assertIn(str(s.work), config["forbidden_paths"])
        self.assertIn(sb.S_MODCACHE, config["read_only_dirs"])

    def test_a_failed_check_a_missing_check_or_a_failed_run_refuses(self):
        failed = self.all_ok()
        failed[3] = {"name": failed[3]["name"], "ok": False, "detail": "planted"}
        for results, code, message in ((failed, 1, "planted"), (self.all_ok()[:-1], 0, "ran checks"), (self.all_ok(), 1, "go test exit 1"),
                                       (list(reversed(self.all_ok())), 0, "ran checks")):
            with self.subTest(message=message), self.assertRaisesRegex(sb.SandboxError, message):
                self.run_probe(results, code)

    def test_tripped_host_canary_refuses_even_when_the_probe_passed(self):
        with self.assertRaisesRegex(sb.SandboxError, "canaries tripped"):
            self.run_probe(self.all_ok(), tripped=True)


@unittest.skipUnless(LINUX, "the canaries use Linux abstract sockets and /proc")
class HostCanaries(unittest.TestCase):
    def test_canaries_are_live_on_the_host_and_trip_on_contact(self):
        root = tmpdir(self)
        canaries = sb.Canaries(root / "outside")
        path = os.environ.get("PATH", "")
        self.addCleanup(os.environ.__setitem__, "PATH", path)
        canaries.start()
        self.addCleanup(canaries.stop)
        controls = canaries.positive_controls()
        self.assertEqual(controls["fake_t3"], "ran on the host")
        baseline = canaries.counts()
        self.assertEqual(canaries.post_checks(baseline)["fake_t3_runs"], 0)
        with socket.create_connection(("127.0.0.1", canaries.listeners["tcp"].getsockname()[1]), timeout=5):
            pass
        for _ in range(500):
            if canaries.counts()["tcp"] > baseline["tcp"]:
                break
            time.sleep(0.01)
        with self.assertRaisesRegex(sb.SandboxError, "tcp canary got 1 connections"):
            canaries.post_checks(baseline)
        canaries.ran.write_text("ran\n", encoding="utf-8")
        with self.assertRaisesRegex(sb.SandboxError, "fake t3 pairing executable ran"):
            canaries.post_checks(canaries.counts())

    def test_the_environment_canary_leaves_with_the_canaries(self):
        root = tmpdir(self)
        canaries = sb.Canaries(root / "outside")
        path = os.environ.get("PATH", "")
        self.addCleanup(os.environ.__setitem__, "PATH", path)
        canaries.start()
        self.assertEqual(os.environ[sb.ENV_CANARY], canaries.env_value)
        canaries.stop()
        self.assertNotIn(sb.ENV_CANARY, os.environ)


class Entry(unittest.TestCase):
    def test_bwrap_version_must_be_recent(self):
        for out, ok in (("bubblewrap 0.11.1\n", True), ("bubblewrap 0.8.0\n", True), ("bubblewrap 0.7.9\n", False), ("other\n", False)):
            with self.subTest(out=out), mock.patch.object(sb.subprocess, "run", return_value=mock.Mock(stdout=out)):
                if ok:
                    self.assertGreaterEqual(sb.bwrap_version("bwrap"), sb.MIN_BWRAP)
                else:
                    with self.assertRaises(sb.SandboxError):
                        sb.bwrap_version("bwrap")

    def test_failures_exit_three_before_any_mutant(self):
        args = ["--go-root", "g", "--gremlins", "x", "--module-cache", "m", "--work", "w"]
        with mock.patch.object(sb.sys, "platform", "win32"), contextlib.redirect_stderr(io.StringIO()) as err:
            self.assertEqual(sb.main(args), sb.SANDBOX_FAILED)
        self.assertIn("needs Linux", err.getvalue())
        with mock.patch.object(sb.sys, "platform", "linux"), \
                mock.patch.object(sb, "Sandbox", side_effect=sb.SandboxError("unproven")), \
                contextlib.redirect_stderr(io.StringIO()) as err:
            self.assertEqual(sb.main(args), sb.SANDBOX_FAILED)
        self.assertIn("refusing to run mutants: unproven", err.getvalue())



@unittest.skipUnless(LINUX, "these tests exercise the Linux launcher boundary")
class CommittedSource(unittest.TestCase):
    def test_only_committed_files_are_staged(self):
        root = tmpdir(self)
        repo = root / "repo"
        repo.mkdir()
        env = {"PATH": "/usr/bin:/bin", "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null"}
        def git(*args):
            return subprocess.run(["/usr/bin/git", "-C", str(repo), *args], env=env, check=True,
                                  capture_output=True, text=True).stdout.strip()
        git("init", "-q")
        (repo / "source.go").write_text("committed fixture\n", encoding="utf-8")
        (repo / ".gitignore").write_text("ignored-fixture\n", encoding="utf-8")
        git("add", ".")
        git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "fixture")
        oid = git("rev-parse", "HEAD")
        (repo / "source.go").write_text("dirty fixture\n", encoding="utf-8")
        (repo / "ignored-fixture").write_text("synthetic private value\n", encoding="utf-8")
        (repo / "untracked-fixture").write_text("synthetic private value\n", encoding="utf-8")
        dst = root / "staged"
        with mock.patch.dict(os.environ, {"GH_TOKEN": "synthetic-caller-value", "GIT_CONFIG_COUNT": "1"}):
            commit, count = sb.archive_source(repo, dst)
        self.assertEqual(commit, oid)
        self.assertEqual(count, 2)
        self.assertEqual((dst / "source.go").read_text(), "committed fixture\n")
        self.assertEqual(sorted(x.name for x in dst.iterdir()), [".gitignore", "source.go"])

    def test_archive_rejects_links_and_paths_outside_private_root(self):
        for name, kind in (("../escape", tarfile.REGTYPE), ("/absolute", tarfile.REGTYPE),
                           ("link", tarfile.SYMTYPE), ("hardlink", tarfile.LNKTYPE)):
            with self.subTest(name=name, kind=kind):
                data = io.BytesIO()
                with tarfile.open(fileobj=data, mode="w") as archive:
                    item = tarfile.TarInfo(name)
                    item.type = kind
                    item.linkname = "/outside"
                    archive.addfile(item)
                with self.assertRaises(sb.SandboxError):
                    sb.extract_source_archive(data.getvalue(), tmpdir(self) / "dst")


@unittest.skipUnless(LINUX, "these tests exercise the Linux launcher boundary")
class Publication(unittest.TestCase):
    def test_incomplete_or_failed_final_proof_cannot_publish_success(self):
        for result in (None, "refused"):
            with self.subTest(result=result):
                s = sandbox(tmpdir(self))
                s.args.summary = s.work / "job-summary.md"
                report = s.dirs["report"] / "mutation"
                report.mkdir(parents=True)
                (report / "summary.md").write_text("Mutation gate: PASS\n", encoding="utf-8")
                if result:
                    s.evidence["result"] = result
                    s.evidence["error"] = "final host canary tripped"
                s.publish()
                summary = s.args.summary.read_text()
                self.assertNotIn("checks passed, host canaries untouched", summary)
                self.assertIn("UNPROVEN", summary)

    def test_proven_gate_failure_retains_honest_confinement_verdict(self):
        s = sandbox(tmpdir(self))
        s.args.summary = s.work / "job-summary.md"
        report = s.dirs["report"] / "mutation"
        report.mkdir(parents=True)
        (report / "summary.md").write_text("Mutation gate: FAIL\n", encoding="utf-8")
        s.evidence["result"] = "mutation exit 1"
        s.evidence["stages"] = [
            {"stage": stage, "when": when, "go_test_exit": 0,
             "checks": [{"name": name, "ok": True} for name in sb.PROBE_CHECKS]}
            for stage, when in (("download", "before"), ("mutation", "before"), ("mutation", "after"))]
        s.publish()
        self.assertIn("checks passed, host canaries untouched", s.args.summary.read_text())



@unittest.skipUnless(LINUX, "publication is a Linux launcher boundary")
class ReportBoundary(unittest.TestCase):
    def test_sandbox_report_file_symlinks_are_never_read_or_exported(self):
        for folder in ("mutation", "sandbox"):
            with self.subTest(folder=folder):
                root = tmpdir(self)
                s = sandbox(root / "run")
                source = s.dirs["report"] / folder
                source.mkdir(parents=True)
                host_only = root / "host-only-fixture"
                host_only.write_bytes(b"synthetic host-only content")
                (source / "escape.txt").symlink_to(host_only)
                with self.assertRaises((sb.SandboxError, OSError)):
                    s.publish()
                self.assertFalse((s.args.report_dir / "escape.txt").exists())
                self.assertFalse((s.args.report_dir / "sandbox" / "escape.txt").exists())

    def test_report_directory_symlinks_are_refused(self):
        for folder in ("mutation", "sandbox"):
            with self.subTest(folder=folder):
                root = tmpdir(self)
                s = sandbox(root / "run")
                s.dirs["report"].mkdir(parents=True)
                outside = root / "host-only-directory"
                outside.mkdir()
                (outside / "escape.txt").write_bytes(b"synthetic host-only content")
                (s.dirs["report"] / folder).symlink_to(outside, target_is_directory=True)
                with self.assertRaises((sb.SandboxError, OSError)):
                    s.publish()
                self.assertFalse((s.args.report_dir / "escape.txt").exists())
                self.assertFalse((s.args.report_dir / "sandbox" / "escape.txt").exists())

    def test_regular_report_export_and_summary_still_work(self):
        s = sandbox(tmpdir(self) / "run")
        s.args.summary = s.work.parent / "job-summary.md"
        for folder, filename, content in (("mutation", "summary.md", "gate fixture"),
                                           ("mutation", "survivors.md", "survivor fixture"),
                                           ("sandbox", "probe.json", "[]")):
            source = s.dirs["report"] / folder
            source.mkdir(parents=True, exist_ok=True)
            (source / filename).write_text(content)
        s.publish()
        self.assertEqual((s.args.report_dir / "summary.md").read_text(), "gate fixture")
        self.assertEqual((s.args.report_dir / "sandbox" / "probe.json").read_text(), "[]")
        self.assertIn("survivor fixture", s.args.summary.read_text())

    def test_probe_result_and_output_ancestors_never_follow_symlinks(self):
        root = tmpdir(self)
        outside = root / "outside"
        outside.mkdir()
        (outside / "probe.json").write_text("synthetic host-only content")
        link = root / "link"
        link.symlink_to(outside, target_is_directory=True)
        with self.assertRaises((sb.SandboxError, OSError)):
            sb.read_report_file(link / "probe.json")
        s = sandbox(root / "run")
        s.args.report_dir = link / "output"
        with self.assertRaises((sb.SandboxError, OSError)):
            s.publish()
        self.assertFalse((outside / "output").exists())

    def test_late_unsafe_source_refuses_before_exporting_an_earlier_valid_report(self):
        root = tmpdir(self)
        s = sandbox(root / "run")
        mutation = s.dirs["report"] / "mutation"
        mutation.mkdir(parents=True)
        (mutation / "summary.md").write_text("valid fixture")
        probes = s.dirs["report"] / "sandbox"
        probes.mkdir()
        host = root / "host-fixture"
        host.write_text("synthetic host-only content")
        (probes / "late-link").symlink_to(host)
        with self.assertRaises((sb.SandboxError, OSError)):
            s.publish()
        self.assertFalse((s.args.report_dir / "summary.md").exists())
        evidence = json.loads((s.args.report_dir / "sandbox" / "evidence.json").read_text())
        self.assertEqual(evidence["result"], "refused")

    def test_output_file_symlink_refuses_without_overwriting_target(self):
        root = tmpdir(self)
        s = sandbox(root / "run")
        source = s.dirs["report"] / "mutation"
        source.mkdir(parents=True)
        (source / "summary.md").write_text("valid fixture")
        s.args.report_dir.mkdir(parents=True)
        host = root / "host-fixture"
        host.write_text("synthetic original")
        (s.args.report_dir / "summary.md").symlink_to(host)
        with self.assertRaises((sb.SandboxError, OSError)):
            s.publish()
        self.assertEqual(host.read_text(), "synthetic original")

    def test_hardlinks_and_special_files_are_refused_without_reading_or_truncating(self):
        root = tmpdir(self)
        host = root / "host-fixture"
        host.write_bytes(b"synthetic original")
        source = root / "reports"
        source.mkdir()
        os.link(host, source / "hardlink")
        with self.assertRaises((sb.SandboxError, OSError)):
            sb.read_report_file(source / "hardlink")
        os.mkfifo(source / "fifo")
        with self.assertRaises((sb.SandboxError, OSError)):
            sb.read_report_file(source / "fifo")
        with self.assertRaises((sb.SandboxError, OSError)):
            sb.write_report_file(source / "hardlink", b"changed")
        self.assertEqual(host.read_bytes(), b"synthetic original")


if __name__ == "__main__":
    unittest.main()
