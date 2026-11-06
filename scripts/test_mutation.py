"""Tests for mutation.py. Run: python3 -m unittest discover -s scripts -p "test_*.py"."""

import contextlib
import io
import json
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import mutation as gate

SOURCE = """package usage

func check(n int) bool {
	if n > limit {
		return false
	}
	return n >= 0
}
"""


SUM = "h1:3G2ROO0I3q4bb5bxElQIUITTuEbl1iOfVYFqunGwrJI="


def config(**overrides):
    base = {
        "tool": {"module": "github.com/go-gremlins/gremlins", "version": "v0.6.0", "sum": SUM,
                 "install": "go install github.com/go-gremlins/gremlins/cmd/gremlins@v0.6.0"},
        "workers": 2,
        "timeout_coefficient": 10,
        "mutators": ["conditionals-boundary", "conditionals-negation"],
        "break_efficacy": 75.0,
        "target_efficacy": 90.0,
        "max_timed_out_share": 0.1,
        "packages": ["./internal/usage", "./internal/agent"],
        "exclusions": [{"package": "./internal/t3", "reason": "Slow tests."}],
        "required_kills": [{"file": "internal/usage/check.go", "contains": "n > limit", "reason": "Fixture bound."}],
    }
    base.update(overrides)
    return base


def report(*mutations, name="check.go"):
    """gremlins JSON with file names relative to the mutated package."""
    return {"go_module": "example", "files": [{"file_name": name, "mutations": [
        {"type": t, "status": s, "line": line, "column": col} for (t, s, line, col) in mutations]}]}


def tmpdir(test):
    folder = tempfile.TemporaryDirectory()
    test.addCleanup(folder.cleanup)
    return Path(folder.name)


def anchor(line=4):
    return gate.Anchor("internal/usage/check.go", "n > limit", "Fixture bound.", line)


class Arithmetic(unittest.TestCase):
    def test_efficacy_counts_only_killed_and_lived(self):
        counts = {gate.KILLED: 3, gate.LIVED: 1, gate.TIMED_OUT: 50, gate.NOT_COVERED: 4, gate.NOT_VIABLE: 9}
        self.assertEqual(gate.efficacy(counts), 75.0)
        self.assertEqual(gate.mutant_coverage(counts), 50.0)

    def test_no_measured_mutant_has_no_efficacy(self):
        counts = {s: 0 for s in gate.STATUSES}
        self.assertIsNone(gate.efficacy(counts))
        self.assertIsNone(gate.mutant_coverage(counts))


class ParseReport(unittest.TestCase):
    def test_file_names_are_joined_to_the_package(self):
        counts, mutants = gate.parse_report("./internal/usage", report(("CONDITIONALS_BOUNDARY", "LIVED", 4, 7)))
        self.assertEqual(mutants[0].file, "internal/usage/check.go")
        self.assertEqual(counts[gate.LIVED], 1)
        _, mutants = gate.parse_report("./internal/usage", report(("CONDITIONALS_BOUNDARY", "LIVED", 4, 7), name="internal/usage/check.go"))
        self.assertEqual(mutants[0].file, "internal/usage/check.go")

    def test_unknown_status_and_malformed_reports_are_errors(self):
        with self.assertRaises(gate.GateError):
            gate.parse_report("./internal/usage", report(("CONDITIONALS_BOUNDARY", "SURVIVED", 4, 7)))
        with self.assertRaises(gate.GateError):
            gate.parse_report("./internal/usage", {"files": [{"mutations": []}]})

    def test_empty_report_counts_nothing(self):
        counts, mutants = gate.parse_report("./internal/usage", {"files": None})
        self.assertEqual(sum(counts.values()), 0)
        self.assertEqual(mutants, [])


class Evaluate(unittest.TestCase):
    def test_break_threshold_is_inclusive(self):
        at_break = report(("A", "KILLED", 7, 1), ("A", "KILLED", 7, 2), ("A", "KILLED", 7, 3), ("A", "LIVED", 7, 4), ("B", "KILLED", 4, 7))
        # 4 killed of 5 measured is 80%.
        self.assertEqual(gate.evaluate(config(break_efficacy=80.0), {"./internal/usage": at_break}, [anchor()]).failures, [])
        failed = gate.evaluate(config(break_efficacy=80.01), {"./internal/usage": at_break}, [anchor()])
        self.assertEqual(len(failed.failures), 1)
        self.assertIn("below the break threshold 80.01%", failed.failures[0])

    def test_timeouts_do_not_dilute_the_denominator(self):
        data = report(("A", "KILLED", 4, 7), ("A", "LIVED", 7, 1), *[("A", "TIMED OUT", 7, 10 + i) for i in range(20)])
        result = gate.evaluate(config(break_efficacy=50.0, max_timed_out_share=0.95), {"./internal/usage": data}, [anchor()])
        self.assertEqual(gate.efficacy(result.totals), 50.0)
        self.assertEqual(result.failures, [])

    def test_too_many_timeouts_fail_the_package(self):
        # 1 timed out of 10 run is exactly 10%: allowed. 2 of 11 is not.
        killed = [("A", "KILLED", 7, i) for i in range(8)] + [("B", "KILLED", 4, 7)]
        ok = gate.evaluate(config(), {"./internal/usage": report(*killed, ("A", "TIMED OUT", 7, 50))}, [anchor()])
        self.assertEqual(ok.failures, [])
        bad = gate.evaluate(config(), {"./internal/usage": report(*killed, ("A", "TIMED OUT", 7, 50), ("A", "TIMED OUT", 7, 51))}, [anchor()])
        self.assertEqual(len(bad.failures), 1)
        self.assertIn("./internal/usage: 2 timed-out mutants (18.2% of those run) exceed 10.0%", bad.failures[0])

    def test_required_kill_accepts_only_killed_or_not_viable(self):
        for status, fails in (("KILLED", False), ("NOT VIABLE", False), ("LIVED", True), ("TIMED OUT", True), ("NOT COVERED", True)):
            with self.subTest(status=status):
                data = report(("CONDITIONALS_BOUNDARY", status, 4, 7), *[("A", "KILLED", 7, i) for i in range(9)])
                result = gate.evaluate(config(break_efficacy=1.0), {"./internal/usage": data}, [anchor()])
                self.assertEqual(bool(result.failures), fails, result.failures)
                if fails:
                    self.assertIn("required kill internal/usage/check.go:4:7 CONDITIONALS_BOUNDARY is " + status, result.failures[0])

    def test_required_kill_without_a_mutant_is_a_config_error(self):
        with self.assertRaisesRegex(gate.GateError, "has no mutant"):
            gate.evaluate(config(), {"./internal/usage": report(("A", "KILLED", 7, 1))}, [anchor()])

    def test_required_kill_outside_the_run_is_skipped(self):
        result = gate.evaluate(config(), {"./internal/agent": report(("A", "KILLED", 7, 1), name="cache.go")}, [anchor()])
        self.assertEqual(result.failures, [])

    def test_package_with_no_measured_mutant_is_an_error(self):
        data = report(("A", "NOT COVERED", 4, 7), ("A", "TIMED OUT", 7, 1))
        with self.assertRaisesRegex(gate.GateError, "no mutant was measured"):
            gate.evaluate(config(), {"./internal/usage": data}, [anchor()])


class Anchors(unittest.TestCase):
    def setUp(self):
        self.root = tmpdir(self)
        (self.root / "internal/usage").mkdir(parents=True)
        (self.root / "internal/usage/check.go").write_text(SOURCE, encoding="utf-8")

    def test_anchor_resolves_to_its_single_line(self):
        self.assertEqual(gate.resolve_anchors(config(), self.root)[0].line, 4)

    def test_anchor_must_match_exactly_one_line(self):
        for contains in ("n > other", "n"):
            with self.subTest(contains=contains), self.assertRaisesRegex(gate.GateError, "want exactly 1"):
                gate.resolve_anchors(config(required_kills=[{"file": "internal/usage/check.go", "contains": contains, "reason": "x"}]), self.root)

    def test_missing_anchor_file_is_an_error(self):
        with self.assertRaises(gate.GateError):
            gate.resolve_anchors(config(required_kills=[{"file": "internal/usage/gone.go", "contains": "n", "reason": "x"}]), self.root)


class Scope(unittest.TestCase):
    LISTING = {".": 0, "./internal/usage": 12, "./internal/agent": 3, "./internal/t3": 5}

    def test_every_package_is_in_scope_or_excluded(self):
        gate.check_scope(config(exclusions=[{"package": ".", "reason": "No tests."}, {"package": "./internal/t3", "reason": "Slow."}]), self.LISTING)

    def test_unlisted_package_fails(self):
        with self.assertRaisesRegex(gate.GateError, r"\. is neither in scope nor excluded"):
            gate.check_scope(config(), self.LISTING)

    def test_overlap_missing_and_untested_packages_fail(self):
        cases = [
            (config(exclusions=[{"package": ".", "reason": "x"}, {"package": "./internal/usage", "reason": "x"}, {"package": "./internal/t3", "reason": "x"}]), "both in scope and excluded"),
            (config(packages=["./internal/usage", "./internal/agent", "./internal/gone"], exclusions=[{"package": ".", "reason": "x"}, {"package": "./internal/t3", "reason": "x"}]), "not a package of the module"),
            (config(packages=[".", "./internal/usage", "./internal/agent"]), "has no tests"),
        ]
        for cfg, message in cases:
            with self.subTest(message=message), self.assertRaisesRegex(gate.GateError, message):
                gate.check_scope(cfg, self.LISTING)


class Config(unittest.TestCase):
    def write(self, data):
        path = tmpdir(self) / "gate.json"
        path.write_text(json.dumps(data), encoding="utf-8")
        return path

    def test_invalid_configs_are_rejected(self):
        bad = [
            config(break_efficacy=95.0),
            config(break_efficacy=0),
            config(workers=0),
            config(mutators=[]),
            config(mutators=["conditionals-boundary", "invert-everything"]),
            config(exclusions=[{"package": "./internal/t3", "reason": " "}]),
            config(required_kills=[{"file": "internal/usage/check.go", "contains": "n > limit"}]),
        ]
        for data in bad:
            with self.subTest(data=data), self.assertRaises(gate.GateError):
                gate.load_config(self.write(data))
        missing = config()
        del missing["tool"]
        with self.assertRaises(gate.GateError):
            gate.load_config(self.write(missing))

    def test_repository_config_is_valid_and_its_anchors_resolve(self):
        cfg = gate.load_config(gate.CONFIG_DEFAULT)
        anchors = gate.resolve_anchors(cfg, gate.REPO_ROOT)
        self.assertEqual(len(anchors), len(cfg["required_kills"]))
        for a in anchors:
            self.assertIn("./" + a.file.rsplit("/", 1)[0], cfg["packages"], a.file)
        for e in cfg["exclusions"]:
            self.assertNotIn(e["package"], cfg["packages"])


class Tool(unittest.TestCase):
    def test_tool_version_is_checked(self):
        info = "/bin/gremlins: go1.27.3\n\tpath\tgithub.com/go-gremlins/gremlins/cmd/gremlins\n\tmod\tgithub.com/go-gremlins/gremlins\t{}\t{}\n"
        with mock.patch.object(gate.shutil, "which", return_value="/bin/gremlins"):
            with mock.patch.object(gate, "run", return_value=info.format("v0.6.0", SUM)):
                self.assertEqual(gate.check_tool("gremlins", config()["tool"], Path(".")), "/bin/gremlins")
            with mock.patch.object(gate, "run", return_value=info.format("v0.5.1", SUM)), self.assertRaisesRegex(gate.GateError, "v0.5.1, want v0.6.0"):
                gate.check_tool("gremlins", config()["tool"], Path("."))
            # Same version, other source: a fork or a locally edited module.
            with mock.patch.object(gate, "run", return_value=info.format("v0.6.0", "h1:other=")), self.assertRaisesRegex(gate.GateError, "module sum h1:other="):
                gate.check_tool("gremlins", config()["tool"], Path("."))
            # A build from a replaced or local module has no sum at all.
            with mock.patch.object(gate, "run", return_value=info.format("v0.6.0", "").rstrip("\t\n") + "\n"), self.assertRaisesRegex(gate.GateError, "module sum None"):
                gate.check_tool("gremlins", config()["tool"], Path("."))
            with mock.patch.object(gate, "run", return_value="/bin/gremlins: go1.27.3\n\tmod\texample.com/other\tv1.0.0\n"), self.assertRaisesRegex(gate.GateError, "not a build of"):
                gate.check_tool("gremlins", config()["tool"], Path("."))
        with mock.patch.object(gate.shutil, "which", return_value=None), self.assertRaisesRegex(gate.GateError, "not found"):
            gate.check_tool("gremlins", config()["tool"], Path("."))

    def test_every_setting_and_mutator_is_an_explicit_flag(self):
        args = gate.gremlins_args("gremlins", "./internal/usage", config(), Path("r.json"), Path("g.yaml"))
        self.assertEqual(args[:4], ["gremlins", "unleash", "--config", "g.yaml"])
        self.assertIn("--workers", args)
        self.assertEqual(args[args.index("--workers") + 1], "2")
        self.assertEqual(args[args.index("--timeout-coefficient") + 1], "10")
        flags = [a for a in args if a.endswith(("=true", "=false"))]
        self.assertEqual(len(flags), len(gate.ALL_MUTATORS))
        self.assertIn("--conditionals-boundary=true", flags)
        self.assertIn("--arithmetic-base=false", flags)
        self.assertIn("--invert-logical=false", flags)
        self.assertEqual(args[-1], "./internal/usage")
        self.assertNotIn("-run", args)

    def test_gremlins_environment_overrides_stop_the_run(self):
        root = tmpdir(self)
        cfg = root / "gate.json"
        cfg.write_text(json.dumps(config(required_kills=[])), encoding="utf-8")
        with mock.patch.dict(gate.os.environ, {"GREMLINS_UNLEASH_WORKERS": "64"}), \
                mock.patch.object(gate, "go_list", side_effect=AssertionError("ran past the check")), \
                contextlib.redirect_stderr(io.StringIO()) as err:
            code = gate.main(["--config", str(cfg), "--root", str(root), "--report-dir", str(root / "out")])
        self.assertEqual(code, 2)
        self.assertIn("GREMLINS_UNLEASH_WORKERS", err.getvalue())

    def test_mutants_run_in_an_isolated_environment_without_the_test_cache(self):
        home = Path("/scratch/home")
        caller = {"PATH": "/bin", "TMPDIR": "/scratch/t", "HOME": "/home/real", "GOFLAGS": "-mod=readonly",
                  "GOTOOLCHAIN": "local", "XDG_CONFIG_HOME": "/home/real/.config", "CLAUDE_CONFIG_DIR": "/home/real/.claude",
                  "OPENAI_API_KEY": "synthetic", "GOOGLE_API_KEY": "synthetic", "GREMLINS_UNLEASH_WORKERS": "9"}
        dirs = {"GOPATH": "/home/real/go", "GOMODCACHE": "/home/real/go/pkg/mod", "GOCACHE": "/home/real/.cache/go-build",
                "GOENV": "/home/real/.config/go/env"}
        env = gate.gremlins_env(caller, dirs, home)
        self.assertEqual(env["GOFLAGS"], "-mod=readonly -count=1")
        self.assertEqual((env["PATH"], env["TMPDIR"], env["GOTOOLCHAIN"]), ("/bin", "/scratch/t", "local"))
        self.assertEqual(env["HOME"], str(home))
        self.assertEqual(env["XDG_CONFIG_HOME"], str(home / ".config"))
        for name, value in dirs.items():
            self.assertEqual(env[name], value)
        for name in ("CLAUDE_CONFIG_DIR", "OPENAI_API_KEY", "GOOGLE_API_KEY", "GREMLINS_UNLEASH_WORKERS"):
            self.assertNotIn(name, env)
        self.assertNotIn("synthetic", json.dumps(env))
        self.assertNotIn("/home/real/.claude", json.dumps(env))
        self.assertEqual(gate.gremlins_env({}, dirs, home)["GOFLAGS"], "-count=1")

    def test_unleash_passes_the_environment_and_an_empty_gremlins_config(self):
        root = tmpdir(self)
        out = root / "r.json"
        out.write_text(json.dumps(report(("A", "KILLED", 4, 7))), encoding="utf-8")
        seen = {}

        def fake_run(args, cwd, env, stdout, stderr, check):
            seen.update(env)
            return mock.Mock(returncode=0)

        with mock.patch.object(gate.subprocess, "run", side_effect=fake_run), contextlib.redirect_stdout(io.StringIO()):
            got = gate.unleash("gremlins", "./internal/usage", config(), out, root / "r.log", root, {"GOFLAGS": "-count=1"})
        self.assertEqual(got["files"][0]["file_name"], "check.go")
        self.assertEqual(seen, {"GOFLAGS": "-count=1"})
        self.assertEqual((root / "gremlins-config.yaml").read_text(encoding="utf-8"), gate.GREMLINS_CONFIG)

    def test_gremlins_tool_errors_fail_the_run(self):
        root = tmpdir(self)
        out = root / "r.json"
        out.write_text(json.dumps(report(("A", "KILLED", 4, 7))), encoding="utf-8")

        def fake_run(args, cwd, env, stdout, stderr, check):
            stdout.write("failed to restore mutation at check.go:4:7 - KILLED\n")
            return mock.Mock(returncode=0)

        with mock.patch.object(gate.subprocess, "run", side_effect=fake_run), contextlib.redirect_stdout(io.StringIO()):
            with self.assertRaisesRegex(gate.GateError, "could not apply or restore"):
                gate.unleash("gremlins", "./internal/usage", config(), out, root / "r.log", root, {})


class Render(unittest.TestCase):
    def test_timings_are_reported_per_package(self):
        result = gate.evaluate(config(), {"./internal/usage": report(("B", "KILLED", 4, 7), ("A", "KILLED", 7, 1))}, [anchor()])
        result.timings = {"./internal/usage": {"seconds": 12.5, "coverage_run": "1.25s"}, "./internal/agent": {"seconds": 3.0, "coverage_run": None}}
        summary, _ = gate.render(config(), result, Path("."))
        self.assertIn("| `./internal/usage` | 12.5 s | 1.25s |", summary)
        self.assertIn("| `./internal/agent` | 3.0 s | not logged |", summary)
        self.assertEqual(gate.COVERAGE_TIME.search("Gathering coverage... done in 2m3.5s\n")[1], "2m3.5s")


class Sandbox(unittest.TestCase):
    """The runner refuses to start Go anywhere but the proven sandbox."""

    def setUp(self):
        self.root = tmpdir(self)
        self.attestation = self.root / "attest.json"
        self.attestation.write_text(json.dumps({"version": 1, "nonce": "n0nce", "home": "/home/sandbox"}), encoding="utf-8")
        self.proc = self.root / "proc"
        (self.proc / "net").mkdir(parents=True)
        (self.proc / "1").mkdir()
        self.set_proc(["lo"], "bwrap")
        self.environ = {"MUTATION_SANDBOX_NONCE": "n0nce", "HOME": "/home/sandbox"}

    def set_proc(self, interfaces, init):
        header = "Inter-|   Receive\n face |bytes    packets\n"
        rows = "".join(f"  {name}:       0       0    0    0\n" for name in interfaces)
        (self.proc / "net" / "dev").write_text(header + rows, encoding="utf-8")
        (self.proc / "1" / "comm").write_text(init + "\n", encoding="utf-8")

    def check(self):
        gate.require_sandbox(self.environ, self.attestation, self.proc)

    def test_proven_sandbox_passes(self):
        self.check()

    def test_each_host_sign_refuses(self):
        cases = [
            ("no attestation", lambda: self.attestation.unlink(), "outside the mutation sandbox"),
            ("wrong nonce", lambda: self.environ.update(MUTATION_SANDBOX_NONCE="other"), "does not match"),
            ("no nonce", lambda: self.environ.pop("MUTATION_SANDBOX_NONCE"), "does not match"),
            ("host home", lambda: self.environ.update(HOME="/home/franz"), "HOME is not"),
            ("host network", lambda: self.set_proc(["lo", "enp1s0", "tailscale0"], "bwrap"), "network interfaces"),
            ("host pid 1", lambda: self.set_proc(["lo"], "systemd"), "PID 1 is 'systemd'"),
            ("no proc", lambda: (self.proc / "1" / "comm").unlink(), "cannot read"),
        ]
        for name, breaks, message in cases:
            with self.subTest(name):
                self.setUp()
                breaks()
                with self.assertRaisesRegex(gate.GateError, message):
                    self.check()

    def test_runner_refuses_outside_the_sandbox_before_any_go_command(self):
        cfg = self.root / "gate.json"
        cfg.write_text(json.dumps(config(required_kills=[])), encoding="utf-8")
        with mock.patch.object(gate, "SANDBOX_ATTESTATION", self.root / "missing.json"), \
                mock.patch.object(gate, "run", side_effect=AssertionError("ran a Go command")), \
                mock.patch.object(gate.subprocess, "run", side_effect=AssertionError("started a process")), \
                mock.patch.dict(gate.os.environ, {}, clear=True), \
                contextlib.redirect_stderr(io.StringIO()) as err:
            code = gate.main(["--config", str(cfg), "--root", str(self.root), "--report-dir", str(self.root / "out")])
        self.assertEqual(code, 2)
        self.assertIn("refusing to run outside the mutation sandbox", err.getvalue())
        self.assertIn("mutation_sandbox.py", err.getvalue())

    def test_tool_sum_is_required(self):
        data = config()
        del data["tool"]["sum"]
        path = self.root / "gate.json"
        path.write_text(json.dumps(data), encoding="utf-8")
        with self.assertRaisesRegex(gate.GateError, "tool needs module, version, sum and install"):
            gate.load_config(path)


class EndToEnd(unittest.TestCase):
    def setUp(self):
        self.root = tmpdir(self)
        (self.root / "internal/usage").mkdir(parents=True)
        (self.root / "internal/usage/check.go").write_text(SOURCE, encoding="utf-8")
        self.reports = self.root / "in"
        self.reports.mkdir()
        self.cfg = self.root / "gate.json"
        self.cfg.write_text(json.dumps(config(packages=["./internal/usage"], break_efficacy=50.0, max_timed_out_share=0.5)), encoding="utf-8")

    def main(self, data):
        (self.reports / "internal-usage.json").write_text(json.dumps(data), encoding="utf-8")
        out, step = self.root / "out", self.root / "step.md"
        with contextlib.redirect_stdout(io.StringIO()):
            code = gate.main(["--config", str(self.cfg), "--root", str(self.root), "--from-reports", str(self.reports),
                              "--report-dir", str(out), "--summary", str(step)])
        return code, out, step.read_text(encoding="utf-8")

    def test_pass_publishes_summary_and_survivors(self):
        code, out, step = self.main(report(("CONDITIONALS_BOUNDARY", "KILLED", 4, 7), ("CONDITIONALS_BOUNDARY", "LIVED", 7, 11), ("A", "TIMED OUT", 7, 9)))
        self.assertEqual(code, 0)
        survivors = (out / "survivors.md").read_text(encoding="utf-8")
        self.assertIn("## Surviving mutants (1)", survivors)
        self.assertIn("`internal/usage/check.go:7:11` CONDITIONALS_BOUNDARY: `return n >= 0`", survivors)
        self.assertIn("## Timed-out mutants (1)", survivors)
        summary = json.loads((out / "summary.json").read_text(encoding="utf-8"))
        self.assertEqual((summary["verdict"], summary["efficacy"]), ("pass", 50.0))
        self.assertIn("## Mutation testing: PASS", step)
        self.assertIn("## Surviving mutants (1)", step)

    def test_failure_exits_one_and_names_the_mutant(self):
        code, out, step = self.main(report(("CONDITIONALS_BOUNDARY", "LIVED", 4, 7), ("A", "KILLED", 7, 1)))
        self.assertEqual(code, 1)
        self.assertIn("## Mutation testing: FAIL", step)
        self.assertIn("required kill internal/usage/check.go:4:7 CONDITIONALS_BOUNDARY is LIVED", step)
        self.assertEqual(json.loads((out / "summary.json").read_text(encoding="utf-8"))["verdict"], "fail")

    def test_config_errors_exit_two(self):
        with contextlib.redirect_stderr(io.StringIO()), contextlib.redirect_stdout(io.StringIO()):
            code = gate.main(["--config", str(self.cfg), "--root", str(self.root), "--from-reports", str(self.reports),
                              "--report-dir", str(self.root / "out"), "--packages", "./internal/t3"])
        self.assertEqual(code, 2)
        with contextlib.redirect_stderr(io.StringIO()):
            code = gate.main(["--config", str(self.cfg), "--root", str(self.root), "--from-reports", str(self.reports),
                              "--report-dir", str(self.root / "out")])
        self.assertEqual(code, 2)  # no report file


if __name__ == "__main__":
    unittest.main()
