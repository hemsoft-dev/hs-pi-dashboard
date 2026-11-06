#!/usr/bin/env python3
"""Run scripts/mutation.py inside a proven bubblewrap sandbox.

A mutant can switch off a test's fixture and send code to its default
location: negating `len(paths) == 0` in internal/agent makes the session
handler use the default T3 database and the installed `t3` CLI, which on a
developer machine are the real ones. A throwaway HOME is not enough to stop
that, so mutants run only inside a bubblewrap sandbox:

- new user, PID, network, IPC, UTS and cgroup namespaces, no nested user
  namespaces, no capabilities, no_new_privs, a new session, killed with its
  parent;
- a read-only root tmpfs with an explicit mount allowlist: the host's /usr
  read-only (with /usr/local hidden and host tools that can reach accounts
  replaced by a refusing stub), a copied Go toolchain and gremlins binary
  read-only, a copy of the repository, a private module cache filled from a
  local proxy of the go.sum modules only, a private build cache, temp folder
  and an empty HOME. The host root, the real HOME, /run, /etc and the host's
  /proc are never mounted; /proc is the sandbox's own;
- a network namespace with only its own loopback, so tests can serve fake
  HTTP fixtures, but nothing on the host or beyond is reachable;
- an environment built from scratch: no variable of the caller passes.

Before each step that runs Go, the launcher plants synthetic canaries on the
host (a file outside every mount, an open descriptor to it, a loopback TCP
listener, a Unix and an abstract socket, a process with a marker argument, a
fake `t3` pairing executable on the caller's PATH and a dummy credential in
the caller's environment), then runs scripts/mutation-sandbox-probe with `go
test` in the same sandbox. The probe checks from inside that none of them are
reachable and that the mounts, environment, processes, descriptors and network
are exactly the declared ones, and runs positive controls: Go builds, an
in-sandbox HTTP fixture, a fake CLI and temp files work, and the gremlins
binary is the pinned build. The launcher refuses the step (exit 3) unless
every check passes and the host saw no connection to its listeners and no run
of the fake executable. The canaries stay up for the whole mutation run and
are checked again afterwards.

Steps, each in its own sandbox:

1. probe, then `go mod download` from the local proxy (GOPROXY=file://,
   verified against go.sum) into the private module cache;
2. probe with the mutation step's mounts, then scripts/mutation.py with the
   module cache read-only, then the probe again.

Usage (Linux, bubblewrap 0.8 or newer, unprivileged user namespaces):

    python3 scripts/mutation_sandbox.py --go-root "$(go env GOROOT)" \\
        --gremlins "$(go env GOPATH)/bin/gremlins" \\
        --module-cache "$(go env GOMODCACHE)" --work "$RUNNER_TEMP/mutation" \\
        --report-dir mutation-report [--packages ./internal/usage]

Commit the intended source first: only committed HEAD is staged, never dirty,
ignored or untracked files. `--module-cache` must already hold the go.sum modules (`go mod download`).
Only their .mod, .zip and .info files are copied. Exit codes: those of
scripts/mutation.py (0 pass, 1 gate failure, 2 config or tool error), and 3
when the sandbox cannot be set up or proven.
"""

from __future__ import annotations

import argparse
import hashlib
import io
import json
import os
import re
import secrets
import shutil
import socket
import subprocess
import sys
import threading
import tarfile
import time
from dataclasses import dataclass
from pathlib import Path, PurePosixPath

REPO_ROOT = Path(__file__).resolve().parent.parent
PROBE_SOURCE = Path(__file__).with_name("mutation-sandbox-probe")
SANDBOX_FAILED = 3
MIN_BWRAP = (0, 8, 0)

# Paths inside the sandbox.
S_SRC, S_GO, S_TOOLS = "/work/src", "/opt/go", "/opt/tools"
S_MODCACHE, S_GOCACHE, S_GOPATH = "/work/modcache", "/work/gocache", "/work/gopath"
S_PROXY, S_REPORT, S_PROBE = "/work/proxy", "/work/report", "/work/probe"
S_HOME, S_TMP = "/home/sandbox", "/tmp"
S_ATTEST, S_PROBE_CONFIG = "/etc/mutation-sandbox.json", "/etc/mutation-probe.json"

# The probe's checks, in its order. A probe result must list exactly these.
PROBE_CHECKS = (
    "host-paths-absent", "mounts-allowlisted", "read-only-mounts", "environment-allowlisted",
    "environment-canary-absent-everywhere", "host-processes-invisible", "inherited-descriptors-absent",
    "network-loopback-only", "host-listeners-unreachable", "pairing-executable-absent", "auth-tools-masked",
    "home-fresh", "no-privileges", "nested-user-namespace-refused", "validators-detect-planted-leaks",
    "go-toolchain", "go-build-and-run", "gremlins-version", "loopback-http-fixture", "fake-cli-fixture",
    "temp-and-writable-dirs",
)
# Host tools that could reach an account or another machine. Any of them under
# /usr is replaced by a stub that refuses to run.
MASKED_TOOLS = ("t3", "codex", "claude", "gh", "tailscale", "op", "ssh", "scp", "sftp", "ssh-agent", "ssh-add",
                "gcloud", "az", "aws", "git-credential-manager", "docker", "podman", "sudo", "pkexec", "su")
STUB = "#!/bin/sh\necho 'refused inside the mutation sandbox' >&2\nexit 126\n"
SKIP_COPY = frozenset({".git", "node_modules", "mutation-report", "crap-report", "accessibility-report", "__pycache__"})
ENV_CANARY = "MUTATION_SANDBOX_CANARY_TOKEN"
HOME_ALLOWED = (".config", ".cache", ".local")


class SandboxError(Exception):
    """The sandbox could not be set up or proven. Exit code 3."""


def log(message: str) -> None:
    print(f"mutation-sandbox: {message}", flush=True)


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            digest.update(chunk)
    return digest.hexdigest()


def go_mod_version(src: Path) -> str:
    match = re.search(r"^go\s+(\d+\.\d+(?:\.\d+)?)\s*$", (src / "go.mod").read_text(encoding="utf-8"), re.MULTILINE)
    if not match:
        raise SandboxError("go.mod has no go line")
    return "go" + match[1]


def bwrap_version(bwrap: str) -> tuple[int, ...]:
    try:
        out = subprocess.run([bwrap, "--version"], capture_output=True, text=True, env={"PATH": "/usr/bin:/bin"},
                             stdin=subprocess.DEVNULL, check=True, timeout=30).stdout
    except (OSError, subprocess.SubprocessError) as err:
        raise SandboxError(f"cannot run {bwrap}: {err}") from err
    match = re.match(r"bubblewrap (\d+)\.(\d+)\.(\d+)", out.strip())
    if not match:
        raise SandboxError(f"unrecognized bwrap version {out.strip()!r}")
    version = tuple(int(x) for x in match.groups())
    if version < MIN_BWRAP:
        raise SandboxError(f"bwrap {'.'.join(map(str, version))} is older than {'.'.join(map(str, MIN_BWRAP))}")
    return version


def copy_tree(src: Path, dst: Path, skip: frozenset[str] = frozenset()) -> int:
    """Copy regular files and folders. A symlink stops the copy unless it is
    relative and resolves inside src, so nothing outside src can be pulled in."""
    count = 0
    root = src.resolve()
    for dirpath, dirnames, filenames in os.walk(src):
        here = Path(dirpath)
        dirnames[:] = sorted(d for d in dirnames if d not in skip)
        target = dst / here.relative_to(src)
        target.mkdir(parents=True, exist_ok=True)
        for name in [*dirnames, *filenames]:
            path = here / name
            if path.is_symlink():
                link = os.readlink(path)
                resolved = path.resolve()
                if os.path.isabs(link) or not resolved.is_relative_to(root):
                    raise SandboxError(f"refusing symlink {path} -> {link}: it leaves {src}")
                if name in filenames:
                    (target / name).symlink_to(link)
                    count += 1
        for name in sorted(filenames):
            path = here / name
            if not path.is_symlink():
                shutil.copy2(path, target / name)
                count += 1
    return count


def extract_source_archive(data: bytes, dst: Path) -> int:
    """Extract regular committed files only into a new private staging tree."""
    count = 0
    with tarfile.open(fileobj=io.BytesIO(data), mode="r:") as archive:
        members = archive.getmembers()
        for item in members:
            path = PurePosixPath(item.name)
            if path.is_absolute() or ".." in path.parts or not path.parts or not (item.isfile() or item.isdir()):
                raise SandboxError(f"refusing source archive entry {item.name!r}")
        dst.mkdir(parents=True, exist_ok=True)
        for item in members:
            target = dst / item.name
            if item.isdir():
                target.mkdir(parents=True, exist_ok=True)
            else:
                target.parent.mkdir(parents=True, exist_ok=True)
                with archive.extractfile(item) as source, target.open("xb") as sink:
                    shutil.copyfileobj(source, sink)
                target.chmod(item.mode & 0o777)
                count += 1
    return count


def archive_source(source: Path, dst: Path) -> tuple[str, int]:
    """Pin HEAD and archive it; dirty, ignored and untracked files never enter."""
    env = {"PATH": "/usr/bin:/bin", "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null",
           "GIT_TERMINAL_PROMPT": "0", "GIT_OPTIONAL_LOCKS": "0"}
    git = ["/usr/bin/git", "-c", "core.fsmonitor=false", "-C", str(source.resolve())]
    try:
        oid = subprocess.run([*git, "rev-parse", "--verify", "HEAD^{commit}"], env=env,
                             stdin=subprocess.DEVNULL, capture_output=True, check=True, timeout=30).stdout.decode().strip()
        if not re.fullmatch(r"[0-9a-f]{40,64}", oid):
            raise SandboxError("source HEAD is not a commit object id")
        archive = subprocess.run([*git, "archive", "--format=tar", oid], env=env,
                                 stdin=subprocess.DEVNULL, capture_output=True, check=True, timeout=60).stdout
    except (OSError, subprocess.SubprocessError) as err:
        raise SandboxError("cannot archive committed source HEAD; commit the intended files first") from err
    return oid, extract_source_archive(archive, dst)


def escape_module(path: str) -> str:
    return re.sub(r"[A-Z]", lambda m: "!" + m[0].lower(), path)


def stage_proxy(go_sum: Path, module_cache: Path, proxy: Path) -> int:
    """Copy the go.sum modules' .mod, .zip and .info files into a file proxy."""
    download = module_cache / "cache" / "download"
    files = 0
    versions: dict[tuple[str, str], bool] = {}
    for line in go_sum.read_text(encoding="utf-8").splitlines():
        fields = line.split()
        if len(fields) != 3:
            continue
        module, version, _ = fields
        zip_needed = not version.endswith("/go.mod")
        version = version.removesuffix("/go.mod")
        versions[(module, version)] = versions.get((module, version), False) or zip_needed
    for (module, version), zip_needed in sorted(versions.items()):
        src = download / escape_module(module) / "@v"
        dst = proxy / escape_module(module) / "@v"
        dst.mkdir(parents=True, exist_ok=True)
        escaped = escape_module(version)
        for ext, required in ((".mod", True), (".info", False), (".zip", False)):
            path = src / (escaped + ext)
            if path.is_file() and not path.is_symlink():
                shutil.copy2(path, dst / path.name)
                files += 1
            elif required:
                raise SandboxError(f"{path} missing; run `go mod download` before the sandbox")
        if zip_needed and not (dst / (escaped + ".zip")).exists():
            log(f"note: no zip for {module} {version} (only needed if a build uses it)")
    return files


@dataclass
class HostMount:
    point: str
    root: str
    fstype: str


def unescape_mount(value: str) -> str:
    return re.sub(r"\\([0-7]{3})", lambda m: chr(int(m[1], 8)), value)


def host_mounts(text: str) -> list[HostMount]:
    mounts = []
    for line in text.splitlines():
        fields = line.split()
        if "-" not in fields:
            continue
        sep = fields.index("-")
        mounts.append(HostMount(unescape_mount(fields[4]), unescape_mount(fields[3]), fields[sep + 1]))
    return mounts


def expected_root(path: Path, mounts: list[HostMount]) -> tuple[str, str]:
    """The mountinfo root and fstype a bind mount of path shows in the sandbox."""
    real = os.path.realpath(path)
    best: HostMount | None = None
    for m in mounts:  # later lines over the same point win
        if real == m.point or real.startswith(m.point.rstrip("/") + "/"):
            if best is None or len(m.point) >= len(best.point):
                best = m
    if best is None:
        raise SandboxError(f"no host mount contains {real}")
    rel = os.path.relpath(real, best.point)
    root = os.path.normpath(os.path.join(best.root, rel)) if rel != "." else best.root
    return root, best.fstype


class Canaries:
    """Synthetic host-side canaries. None of them hold real data."""

    def __init__(self, outside: Path):
        self.outside = outside
        self.token = secrets.token_hex(16)
        self.file = outside / f"host-canary-{self.token[:8]}.txt"
        self.bin = outside / "bin"
        self.pairing = self.bin / "t3"
        self.ran = outside / "t3-ran"
        self.unix_path = outside / "host.sock"
        self.abstract = f"mutation-sandbox-canary-{self.token[:12]}"
        self.marker = f"mutation-sandbox-host-marker-{self.token[:12]}"
        self.env_value = f"synthetic-not-a-credential-{self.token}"
        self.connections = {"tcp": 0, "unix": 0, "abstract": 0}
        self.listeners: dict[str, socket.socket] = {}
        self.lock = threading.Lock()
        self.fd = -1
        self.sleeper: subprocess.Popen | None = None

    def start(self) -> None:
        self.outside.mkdir(parents=True)
        self.file.write_text(f"synthetic canary {self.token}\n", encoding="utf-8")
        self.bin.mkdir()
        self.pairing.write_text(f"#!/bin/sh\necho ran >> '{self.ran}'\nexit 0\n", encoding="utf-8")
        self.pairing.chmod(0o755)
        # An inheritable descriptor: the sandbox must not receive it.
        self.fd = os.open(self.file, os.O_RDONLY)
        os.set_inheritable(self.fd, True)
        tcp = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        tcp.bind(("127.0.0.1", 0))
        unix = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        unix.bind(str(self.unix_path))
        abstract = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        abstract.bind("\0" + self.abstract)
        for kind, sock in (("tcp", tcp), ("unix", unix), ("abstract", abstract)):
            sock.listen(16)
            sock.set_inheritable(True)
            self.listeners[kind] = sock
            threading.Thread(target=self._accept, args=(kind, sock), daemon=True).start()
        self.sleeper = subprocess.Popen([sys.executable, "-I", "-c", "import time; time.sleep(86400)", self.marker],
                                        stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                                        env={"PATH": "/usr/bin:/bin"})
        os.environ[ENV_CANARY] = self.env_value
        os.environ["PATH"] = f"{self.bin}{os.pathsep}{os.environ.get('PATH', '')}"

    def _accept(self, kind: str, sock: socket.socket) -> None:
        while True:
            try:
                conn, _ = sock.accept()
            except OSError:
                return
            with self.lock:
                self.connections[kind] += 1
            conn.close()

    @property
    def tcp_addr(self) -> str:
        host, port = self.listeners["tcp"].getsockname()
        return f"{host}:{port}"

    def socket_inodes(self) -> list[int]:
        return [os.fstat(s.fileno()).st_ino for s in self.listeners.values()]

    def counts(self) -> dict[str, int]:
        with self.lock:
            return dict(self.connections)

    def positive_controls(self) -> dict[str, str]:
        """Show each canary is live on the host, so a miss inside means isolation."""
        before = self.counts()
        targets = (("tcp", socket.AF_INET, ("127.0.0.1", self.listeners["tcp"].getsockname()[1])),
                   ("unix", socket.AF_UNIX, str(self.unix_path)), ("abstract", socket.AF_UNIX, "\0" + self.abstract))
        for kind, family, address in targets:
            with socket.socket(family, socket.SOCK_STREAM) as c:
                c.settimeout(5)
                c.connect(address)
            deadline = time.monotonic() + 5
            while self.counts()[kind] == before[kind]:
                if time.monotonic() > deadline:
                    raise SandboxError(f"host {kind} canary did not accept a host connection")
                time.sleep(0.01)
        if self.file.read_text(encoding="utf-8") != f"synthetic canary {self.token}\n":
            raise SandboxError("host canary file unreadable on the host")
        cmdline = Path(f"/proc/{self.sleeper.pid}/cmdline").read_bytes()
        if self.marker.encode() not in cmdline:
            raise SandboxError("host marker process not visible on the host")
        found = shutil.which("t3")
        if found != str(self.pairing):
            raise SandboxError(f"fake t3 resolves to {found} on the host, want {self.pairing}")
        subprocess.run([found], check=True, env={"PATH": "/usr/bin:/bin"}, stdin=subprocess.DEVNULL, timeout=30)
        if not self.ran.exists():
            raise SandboxError("fake t3 ran on the host without leaving its marker")
        self.ran.unlink()
        if os.environ.get(ENV_CANARY) != self.env_value:
            raise SandboxError("environment canary not set in the launcher")
        return {"tcp": self.tcp_addr, "unix": "connected", "abstract": "connected", "file": "readable",
                "marker_process": f"visible as host pid {self.sleeper.pid}", "fake_t3": "ran on the host",
                "environment": f"{ENV_CANARY} set in the launcher"}

    def post_checks(self, baseline: dict[str, int]) -> dict[str, object]:
        counts = self.counts()
        problems = [f"{kind} canary got {counts[kind] - baseline[kind]} connections" for kind in counts if counts[kind] != baseline[kind]]
        if self.ran.exists():
            problems.append("the fake t3 pairing executable ran")
        if self.file.read_text(encoding="utf-8") != f"synthetic canary {self.token}\n":
            problems.append("the host canary file changed")
        if self.sleeper.poll() is not None:
            problems.append("the host marker process died")
        extra = sorted(p.name for p in self.outside.iterdir()) != sorted(p.name for p in (self.file, self.bin, self.unix_path))
        if extra:
            problems.append(f"the outside folder changed: {sorted(p.name for p in self.outside.iterdir())}")
        if problems:
            raise SandboxError("host canaries tripped: " + "; ".join(problems))
        return {"connections_since_baseline": {k: counts[k] - baseline[k] for k in counts}, "fake_t3_runs": 0,
                "canary_file": "unchanged", "marker_process": "alive"}

    def stop(self) -> None:
        for sock in self.listeners.values():
            sock.close()
        if self.fd >= 0:
            os.close(self.fd)
        if self.sleeper and self.sleeper.poll() is None:
            self.sleeper.kill()
            self.sleeper.wait()
        os.environ.pop(ENV_CANARY, None)


class Sandbox:
    def __init__(self, args: argparse.Namespace):
        self.args = args
        self.work = args.work.resolve()
        self.bwrap = args.bwrap
        self.nonce = secrets.token_hex(16)
        self.dirs = {name: self.work / name for name in
                     ("src", "go", "tools", "proxy", "modcache", "gocache", "gopath", "home", "tmp", "report", "probe", "etc", "logs")}
        self.mounts_text = Path("/proc/self/mountinfo").read_text(encoding="utf-8")
        self.host = host_mounts(self.mounts_text)
        self.masks: list[str] = []
        self.evidence: dict[str, object] = {"stages": []}
        self.probes = 0

    # Staging, with plain file copies only: no Go runs on the host.
    def stage(self) -> None:
        if self.work.exists():
            raise SandboxError(f"{self.work} exists; give a new folder")
        self.work.mkdir(parents=True, mode=0o700)
        for path in self.dirs.values():
            path.mkdir()
        commit, src_files = archive_source(self.args.source, self.dirs["src"])
        self.evidence["source_commit"] = commit
        copy_tree(self.dirs["src"] / "scripts/mutation-sandbox-probe", self.dirs["probe"])
        self.go_version = go_mod_version(self.dirs["src"])
        go_root = self.args.go_root.resolve()
        version = (go_root / "VERSION").read_text(encoding="utf-8").splitlines()[0].strip()
        if version != self.go_version:
            raise SandboxError(f"{go_root} is {version}, go.mod wants {self.go_version}")
        copy_tree(go_root, self.dirs["go"])
        gremlins = self.args.gremlins.resolve()
        shutil.copy2(gremlins, self.dirs["tools"] / "gremlins")
        (self.dirs["tools"] / "gremlins").chmod(0o755)
        tool = json.loads((self.dirs["src"] / "scripts/mutation-gate.json").read_text(encoding="utf-8"))["tool"]
        self.tool = tool
        proxy_files = stage_proxy(self.dirs["src"] / "go.sum", self.args.module_cache.resolve(), self.dirs["proxy"])
        etc = self.dirs["etc"]
        uid, gid = os.getuid(), os.getgid()
        (etc / "passwd").write_text(f"sandbox:x:{uid}:{gid}:sandbox:{S_HOME}:/usr/sbin/nologin\n", encoding="utf-8")
        (etc / "group").write_text(f"sandbox:x:{gid}:\n", encoding="utf-8")
        (etc / "hosts").write_text("127.0.0.1 localhost\n::1 localhost\n", encoding="utf-8")
        (etc / "refuse").write_text(STUB, encoding="utf-8")
        (etc / "refuse").chmod(0o755)
        (etc / "attest.json").write_text(json.dumps({"version": 1, "nonce": self.nonce, "home": S_HOME}) + "\n", encoding="utf-8")
        for name in MASKED_TOOLS:
            for folder in ("/usr/bin", "/usr/sbin", "/usr/local/bin"):
                path = Path(folder, name)
                if os.path.lexists(path):
                    real = os.path.realpath(path)
                    if real.startswith("/usr/") and not real.startswith("/usr/local/") and os.path.isfile(real) and real not in self.masks:
                        self.masks.append(real)
        self.evidence.update({
            "source_files": src_files, "go_version": self.go_version, "go_root_copied_from": str(go_root),
            "gremlins_copied_from": str(gremlins), "gremlins_sha256": sha256_file(self.dirs["tools"] / "gremlins"),
            "proxy_files": proxy_files, "masked_tools": self.masks,
        })
        log(f"staged {src_files} source files, {self.go_version}, {proxy_files} module files, {len(self.masks)} masked tools")

    def binds(self, stage: str) -> list[tuple[str, Path, str]]:
        """(kind, host source, sandbox destination), in mount order."""
        d = self.dirs
        out = [("ro", Path("/usr"), "/usr")]
        out += [("ro", d["etc"] / "refuse", mask) for mask in self.masks]
        out += [("ro", d["etc"] / "passwd", "/etc/passwd"), ("ro", d["etc"] / "group", "/etc/group"),
                ("ro", d["etc"] / "hosts", "/etc/hosts"), ("ro", d["etc"] / "attest.json", S_ATTEST),
                ("ro", d["go"], S_GO), ("ro", d["tools"], S_TOOLS), ("ro", d["probe"], S_PROBE),
                ("rw", d["home"], S_HOME), ("rw", d["tmp"], S_TMP), ("rw", d["gocache"], S_GOCACHE),
                ("rw", d["gopath"], S_GOPATH), ("rw", d["report"], S_REPORT)]
        if stage == "download":
            out += [("ro", d["src"], S_SRC), ("ro", d["proxy"], S_PROXY), ("rw", d["modcache"], S_MODCACHE)]
        elif stage == "mutation":
            out += [("ro", d["src"], S_SRC), ("ro", d["modcache"], S_MODCACHE)]
        else:
            raise ValueError(stage)
        return out

    def env(self, stage: str) -> dict[str, str]:
        env = {
            "PATH": f"{S_GO}/bin:{S_TOOLS}:/usr/bin:/bin", "HOME": S_HOME, "TMPDIR": S_TMP, "LANG": "C.UTF-8", "TZ": "UTC",
            "XDG_CONFIG_HOME": f"{S_HOME}/.config", "XDG_CACHE_HOME": f"{S_HOME}/.cache",
            "XDG_DATA_HOME": f"{S_HOME}/.local/share", "XDG_STATE_HOME": f"{S_HOME}/.local/state",
            "GOROOT": S_GO, "GOTOOLCHAIN": "local", "GOENV": "off", "GOWORK": "off", "GOFLAGS": "-mod=readonly",
            "GOPROXY": "off", "GOSUMDB": "off", "GOMODCACHE": S_MODCACHE, "GOCACHE": S_GOCACHE, "GOPATH": S_GOPATH,
            "CGO_ENABLED": "0", "MUTATION_SANDBOX_NONCE": self.nonce,
        }
        if stage == "download":
            env["GOPROXY"] = f"file://{S_PROXY}"
        return env

    def argv(self, stage: str, env: dict[str, str], command: list[str], chdir: str, extra: list[tuple[str, Path, str]] = ()) -> list[str]:
        argv = [self.bwrap, "--unshare-user", "--unshare-ipc", "--unshare-pid", "--unshare-net", "--unshare-uts",
                "--unshare-cgroup", "--disable-userns", "--assert-userns-disabled", "--die-with-parent", "--new-session",
                "--hostname", "mutation-sandbox", "--clearenv"]
        for key, value in sorted(env.items()):
            argv += ["--setenv", key, value]
        argv += ["--proc", "/proc", "--dev", "/dev"]
        for target, link in (("usr/bin", "/bin"), ("usr/sbin", "/sbin"), ("usr/lib", "/lib"), ("usr/lib64", "/lib64")):
            argv += ["--symlink", target, link]
        for kind, src, dest in [*self.binds(stage), *extra]:
            argv += ["--ro-bind" if kind == "ro" else "--bind", str(src), dest]
            if dest == "/usr" and os.path.isdir("/usr/local"):
                argv += ["--tmpfs", "/usr/local", "--remount-ro", "/usr/local"]
        argv += ["--remount-ro", "/", "--chdir", chdir, "--", *command]
        return argv

    def expected_mounts(self, stage: str, extra) -> dict[str, dict]:
        # bwrap builds the new root in a "newroot" folder of its own tmpfs.
        want = {"/": {"root": "/newroot", "fstype": "tmpfs", "read_only": True}}
        if os.path.isdir("/usr/local"):
            want["/usr/local"] = {"root": "/", "fstype": "tmpfs", "read_only": True}
        for kind, src, dest in [*self.binds(stage), *extra]:
            root, fstype = expected_root(src, self.host)
            want[dest] = {"root": root, "fstype": fstype, "read_only": kind == "ro"}
        return want

    def execute(self, name: str, argv: list[str], timeout: float, stream: bool = False) -> int:
        logfile = self.dirs["logs"] / f"{name}.log"
        (self.dirs["logs"] / f"{name}.argv.json").write_text(json.dumps(argv, indent=1) + "\n", encoding="utf-8")
        log(f"{name}: start ({logfile})")
        started = time.monotonic()
        with logfile.open("w", encoding="utf-8") as sink:
            # A minimal environment for bwrap itself: its init process stays
            # visible in the sandbox, so it must carry nothing of the caller's.
            proc = subprocess.Popen(argv, cwd="/", env={"PATH": "/usr/bin:/bin"}, stdin=subprocess.DEVNULL,
                                    stdout=subprocess.PIPE, stderr=subprocess.STDOUT, close_fds=True, text=True,
                                    encoding="utf-8", errors="replace")
            # bwrap runs with --die-with-parent: killing it ends the sandbox.
            killed = threading.Event()

            def kill() -> None:
                killed.set()
                proc.kill()

            watchdog = threading.Timer(timeout, kill)
            watchdog.start()
            try:
                for line in proc.stdout:
                    sink.write(line)
                    if stream:
                        sys.stdout.write(line)
                        sys.stdout.flush()
                code = proc.wait()
            finally:
                watchdog.cancel()
        if killed.is_set():
            raise SandboxError(f"{name} ran past {timeout:.0f} s and was killed")
        log(f"{name}: exit {code} after {time.monotonic() - started:.1f} s")
        return code

    def probe(self, stage: str, canaries: Canaries, when: str) -> dict:
        self.probes += 1
        n = self.probes
        result_name = f"probe-{n}-{stage}-{when}.json"
        config_path = self.dirs["etc"] / f"probe-{n}.json"
        config_path.write_text("{}\n", encoding="utf-8")
        extra = [("ro", config_path, S_PROBE_CONFIG)]
        env = {**self.env(stage), "MUTATION_PROBE_CONFIG": S_PROBE_CONFIG}
        real_home = os.path.expanduser("~")
        forbidden = sorted({str(self.work), str(self.args.source.resolve()), str(self.args.go_root.resolve()),
                            str(self.args.gremlins.resolve()), str(self.args.module_cache.resolve()), str(canaries.outside),
                            str(canaries.file), str(canaries.unix_path), str(canaries.pairing), real_home,
                            "/run", "/var/run", "/root", "/var/lib", "/srv", "/mnt", "/media"})
        st = os.stat(canaries.file)
        read_only = ["/", "/etc", "/usr", "/usr/local", S_GO, S_TOOLS, S_PROBE, S_SRC]
        if stage == "mutation":
            read_only.append(S_MODCACHE)
        config = {
            "forbidden_paths": forbidden,
            "mounts": self.expected_mounts(stage, extra),
            "env_canary_name": ENV_CANARY, "env_canary_sha256": sha256_bytes(canaries.env_value.encode()),
            "allowed_env": sorted(env), "process_marker": canaries.marker,
            "canary_file": {"dev": st.st_dev, "ino": st.st_ino}, "canary_socket_inodes": canaries.socket_inodes(),
            "host_tcp_addr": canaries.tcp_addr, "host_unix_path": str(canaries.unix_path), "host_abstract": canaries.abstract,
            "pairing_names": ["t3"], "masked_tools": self.masks, "stub_sha256": sha256_bytes(STUB.encode()),
            "home": S_HOME, "home_allowed": list(HOME_ALLOWED), "temp_dir": S_TMP,
            "read_only_dirs": [d for d in read_only if d != "/usr/local" or os.path.isdir("/usr/local")],
            "writable_dirs": [S_GOCACHE, S_REPORT] + ([S_MODCACHE] if stage == "download" else []),
            "go_version": self.go_version, "gremlins": f"{S_TOOLS}/gremlins",
            "gremlins_module": self.tool["module"], "gremlins_version": self.tool["version"], "gremlins_sum": self.tool["sum"],
            "result": f"{S_REPORT}/sandbox/{result_name}",
        }
        (self.dirs["report"] / "sandbox").mkdir(exist_ok=True)
        config_path.write_text(json.dumps(config, indent=1) + "\n", encoding="utf-8")
        baseline = canaries.counts()
        code = self.execute(f"probe-{n}-{stage}-{when}", self.argv(stage, env, ["go", "test", "-count=1", "-v", "."], S_PROBE, extra), 600)
        result_path = self.dirs["report"] / "sandbox" / result_name
        try:
            results = json.loads(result_path.read_text(encoding="utf-8"))
        except (OSError, ValueError) as err:
            raise SandboxError(f"probe {n} ({stage}, {when}) wrote no result (go test exit {code}): {err}") from err
        names = tuple(r["name"] for r in results)
        failed = [f"{r['name']}: {r['detail']}" for r in results if not r["ok"]]
        host = canaries.post_checks(baseline)
        record = {"probe": n, "stage": stage, "when": when, "go_test_exit": code, "checks": results, "host_after": host}
        self.evidence["stages"].append(record)
        if names != PROBE_CHECKS:
            raise SandboxError(f"probe {n} ran checks {names}, want {PROBE_CHECKS}")
        if failed or code != 0:
            raise SandboxError(f"probe {n} ({stage}, {when}) failed (go test exit {code}): " + "; ".join(failed))
        log(f"probe {n} ({stage}, {when}): {len(results)} checks passed, host canaries untouched")
        return record

    def run(self) -> int:
        bwrap = bwrap_version(self.bwrap)
        self.evidence["bwrap_version"] = ".".join(map(str, bwrap))
        self.evidence["kernel"] = os.uname().release
        self.stage()
        canaries = Canaries(self.work / "outside")
        try:
            canaries.start()
            self.evidence["host_positive_controls"] = canaries.positive_controls()
            self.probe("download", canaries, "before")
            baseline = canaries.counts()
            code = self.execute("download", self.argv("download", self.env("download"), ["go", "mod", "download", "-x"], S_SRC), 1800)
            self.evidence["download"] = {"exit": code, "host_after": canaries.post_checks(baseline)}
            if code != 0:
                raise SandboxError(f"go mod download from the local proxy exited {code}")
            self.probe("mutation", canaries, "before")
            if self.args.probe_only:
                self.evidence["result"] = "probe-only"
                log("probe only: sandbox proven, mutation not run")
                return 0
            command = ["/usr/bin/python3", "-I", "-B", f"{S_SRC}/scripts/mutation.py", "--root", S_SRC,
                       "--report-dir", f"{S_REPORT}/mutation", "--gremlins", f"{S_TOOLS}/gremlins"]
            if self.args.packages:
                command += ["--packages", *self.args.packages]
            baseline = canaries.counts()
            started = time.monotonic()
            code = self.execute("mutation", self.argv("mutation", self.env("mutation"), command, S_SRC), self.args.timeout, stream=True)
            self.evidence["mutation"] = {"exit": code, "seconds": round(time.monotonic() - started, 1),
                                         "host_after": canaries.post_checks(baseline)}
            self.probe("mutation", canaries, "after")
            self.evidence["result"] = f"mutation exit {code}"
            return code
        except Exception as err:
            self.evidence["result"] = "refused"
            self.evidence["error"] = str(err)
            raise
        finally:
            canaries.stop()
            self.publish()

    def publish(self) -> None:
        report = self.args.report_dir
        report.mkdir(parents=True, exist_ok=True)
        mutation = self.dirs["report"] / "mutation"
        if mutation.is_dir():
            for path in mutation.iterdir():
                if path.is_file():
                    shutil.copy2(path, report / path.name)
        sandbox = report / "sandbox"
        sandbox.mkdir(exist_ok=True)
        for folder in (self.dirs["report"] / "sandbox", self.dirs["logs"]):
            if folder.is_dir():
                for path in folder.iterdir():
                    if path.is_file():
                        shutil.copy2(path, sandbox / path.name)
        (sandbox / "evidence.json").write_text(json.dumps(self.evidence, indent=1, default=str) + "\n", encoding="utf-8")
        if self.args.summary and (report / "summary.md").is_file():
            stages = self.evidence["stages"]
            expected = [("download", "before"), ("mutation", "before"), ("mutation", "after")]
            proven = (self.evidence.get("result") in ("mutation exit 0", "mutation exit 1", "mutation exit 2")
                      and not self.evidence.get("error")
                      and [(r["stage"], r["when"]) for r in stages] == expected
                      and all(r["go_test_exit"] == 0 and tuple(c["name"] for c in r["checks"]) == PROBE_CHECKS
                              and all(c["ok"] for c in r["checks"]) for r in stages))
            line = (f"Sandbox: bubblewrap {self.evidence.get('bwrap_version')}, {len(stages)} probe runs of "
                    f"{len(PROBE_CHECKS)} checks passed, host canaries untouched. See `sandbox/evidence.json`.\n"
                    if proven else "Sandbox: UNPROVEN; mutation gate results are not qualified. See `sandbox/evidence.json`.\n")
            with self.args.summary.open("a", encoding="utf-8") as sink:
                sink.write((report / "summary.md").read_text(encoding="utf-8") + "\n" + line + "\n")
                if (report / "survivors.md").is_file():
                    sink.write((report / "survivors.md").read_text(encoding="utf-8"))


def parse_args(argv: list[str] | None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--go-root", type=Path, required=True, help="Go toolchain to copy (its VERSION must match go.mod)")
    parser.add_argument("--gremlins", type=Path, required=True, help="gremlins binary to copy; its build is checked inside")
    parser.add_argument("--module-cache", type=Path, required=True, help="GOMODCACHE holding the go.sum modules")
    parser.add_argument("--work", type=Path, required=True, help="new folder for the sandbox's copies and caches")
    parser.add_argument("--source", type=Path, default=REPO_ROOT, help="Git checkout whose committed HEAD is archived (default: this one)")
    parser.add_argument("--report-dir", type=Path, default=Path("mutation-report"))
    parser.add_argument("--summary", type=Path, help="append the summary here (e.g. $GITHUB_STEP_SUMMARY)")
    parser.add_argument("--packages", nargs="+", help="run only these in-scope packages")
    parser.add_argument("--bwrap", default="/usr/bin/bwrap")
    parser.add_argument("--timeout", type=float, default=5400.0, help="seconds for the mutation step")
    parser.add_argument("--probe-only", action="store_true", help="prove the sandbox and stop before mutation")
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv)
    if sys.platform != "linux":
        print("mutation-sandbox: error: bubblewrap needs Linux", file=sys.stderr)
        return SANDBOX_FAILED
    try:
        return Sandbox(args).run()
    except (SandboxError, OSError, ValueError, KeyError) as err:
        print(f"mutation-sandbox: error: refusing to run mutants: {err}", file=sys.stderr)
        return SANDBOX_FAILED


if __name__ == "__main__":
    sys.exit(main())
