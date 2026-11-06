#!/usr/bin/env python3
"""Mutation testing with a break threshold, run on pinned gremlins.

It runs only inside the bubblewrap sandbox of scripts/mutation_sandbox.py,
which proves the sandbox and then starts it there:

    python3 scripts/mutation_sandbox.py --go-root ... --gremlins ...
        --module-cache ... --work ... --report-dir mutation-report

Run directly on a host it refuses (exit 2) before any Go command, since a
mutant can switch off a test fixture and reach the host's real HOME, T3 data
or tools. Only `--from-reports`, which runs no Go, works outside the sandbox.

It reads scripts/mutation-gate.json, checks that every Go package is either in
scope or excluded with a reason, then runs `gremlins unleash` once per package
in scope. Workers, the timeout coefficient and every mutator type are passed as
flags with an empty gremlins config file, so no gremlins config file elsewhere
changes them; GREMLINS_* environment overrides stop the run. The gremlins
binary must be the pinned module version and module sum. gremlins applies one
mutant at a time and runs that package's real tests (`go test -failfast
<package>`, no -run filter) against it. A failing run kills the mutant; a
passing run lets it live.

Statuses and the denominator:

- KILLED and LIVED are the measured mutants. Test efficacy is
  killed / (killed + lived) per package and in total.
- TIMED OUT (the run exceeded the coverage run's time multiplied by the timeout
  coefficient), NOT VIABLE (the mutant did not compile) and NOT COVERED (no test
  executes the line) are reported but are not in the efficacy denominator.
  Mutant coverage, (killed + lived) / (killed + lived + not covered), is
  reported beside it.

The gate fails (exit 1) when:

- total efficacy falls below the declared break threshold;
- a mutant on a required-kill line is anything but KILLED or NOT VIABLE (a
  survivor, a timeout or an uncovered mutant there is not a proven kill);
- a package's timed-out share of killed + lived + timed out exceeds
  max_timed_out_share, since efficacy that leaves out that many mutants means a
  slow host or hanging mutants, not a measurement.

Inside the sandbox, gremlins and every test run it starts also get an
allowlisted environment with HOME and the per-user folders in an empty
throwaway folder, the sandbox's private Go module and build caches, and test
caching off (GOFLAGS=-count=1): gremlins times mutants against its first
coverage run, and a cached result would set every deadline to milliseconds.
The report lists each package's gremlins wall time and that first run.

Exit 2 means bad config, a wrong tool version or a tool failure, or no proven
sandbox: a package
missing from both the scope and the exclusions, a package in scope without
tests, a required-kill anchor that does not match exactly one line or that has
no mutant, a package with no measured mutant, or gremlins reporting a mutation
it could not apply or restore.

`--from-reports DIR` re-evaluates gremlins JSON reports already in DIR
(`<package>.json`) without running gremlins.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import time
from dataclasses import dataclass, field
from pathlib import Path

CONFIG_DEFAULT = Path(__file__).with_name("mutation-gate.json")
REPO_ROOT = Path(__file__).resolve().parent.parent

KILLED, LIVED, TIMED_OUT = "KILLED", "LIVED", "TIMED OUT"
NOT_COVERED, NOT_VIABLE = "NOT COVERED", "NOT VIABLE"
STATUSES = (KILLED, LIVED, TIMED_OUT, NOT_COVERED, NOT_VIABLE)
# gremlins logs these when a worker cannot apply or roll back a mutant; the
# run's numbers are then unreliable (for example a full TMPDIR).
TOOL_ERRORS = re.compile(r"failed to (apply|restore) mutation")
GO_VERSION_MOD = re.compile(r"^\s*mod\s+(?P<module>\S+)\s+(?P<version>\S+)(?:[ \t]+(?P<sum>h1:\S+))?", re.MULTILINE)
# gremlins' log line for the package's first test run, which sets every
# mutant's deadline (timeout coefficient x this time).
COVERAGE_TIME = re.compile(r"Gathering coverage\.\.\. done in (\S+)")
# Written by scripts/mutation_sandbox.py inside its bubblewrap sandbox.
SANDBOX_ATTESTATION = Path("/etc/mutation-sandbox.json")
# Every mutator type gremlins v0.6.0 knows. Each run passes all of them as
# explicit flags, so the configured set applies whatever config files exist.
ALL_MUTATORS = (
    "arithmetic-base", "conditionals-boundary", "conditionals-negation", "increment-decrement",
    "invert-assignments", "invert-bitwise", "invert-bwassign", "invert-logical", "invert-loopctrl",
    "invert-negatives", "remove-self-assignments",
)
# gremlins reads /etc/gremlins, ~/.config/gremlins and .gremlins.yaml unless
# given one file. The runner gives it this empty one.
GREMLINS_CONFIG = "# Written by scripts/mutation.py. Every setting is passed as a flag.\n"
# Caller variables that pass to gremlins. Everything else, including
# credentials and tool config overrides, stays out of mutant runs.
ENV_KEEP = frozenset({
    "PATH", "TMPDIR", "TEMP", "TMP", "LANG", "LC_ALL", "LC_CTYPE", "TZ",
    "SystemRoot", "SYSTEMROOT", "ComSpec", "COMSPEC", "PATHEXT", "WINDIR", "windir",
    "GOROOT", "GOTOOLCHAIN", "GOFLAGS", "GOPROXY", "GONOPROXY", "GOPRIVATE", "GONOSUMDB", "GOSUMDB",
    "GOINSECURE", "GOVCS", "GOWORK", "GO111MODULE", "GOTMPDIR", "GOOS", "GOARCH", "GOAMD64", "GOARM64",
    "GOEXPERIMENT", "GODEBUG", "GOGC", "GOMEMLIMIT", "GOMAXPROCS", "CGO_ENABLED", "CC", "CXX",
})


class GateError(Exception):
    """Bad input, config or tool output. Exit code 2, distinct from a gate failure."""


@dataclass(frozen=True)
class Mutant:
    package: str  # ./internal/usage
    file: str  # internal/usage/claude_peer.go
    line: int
    column: int
    type: str
    status: str


@dataclass
class Anchor:
    file: str
    contains: str
    reason: str
    line: int = 0


@dataclass
class Result:
    packages: dict[str, dict[str, int]]
    mutants: list[Mutant]
    anchors: list[Anchor]
    failures: list[str] = field(default_factory=list)
    # package -> {"seconds": wall time of its gremlins run, "coverage_run": gremlins' first test run}
    timings: dict[str, dict] = field(default_factory=dict)

    @property
    def totals(self) -> dict[str, int]:
        totals = {status: 0 for status in STATUSES}
        for counts in self.packages.values():
            for status in STATUSES:
                totals[status] += counts[status]
        return totals


def efficacy(counts: dict[str, int]) -> float | None:
    measured = counts[KILLED] + counts[LIVED]
    return None if measured == 0 else 100.0 * counts[KILLED] / measured


def mutant_coverage(counts: dict[str, int]) -> float | None:
    runnable = counts[KILLED] + counts[LIVED] + counts[NOT_COVERED]
    return None if runnable == 0 else 100.0 * (counts[KILLED] + counts[LIVED]) / runnable


def load_config(path: Path) -> dict:
    try:
        config = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError) as err:
        raise GateError(f"cannot read {path}: {err}") from err
    for key in ("tool", "workers", "timeout_coefficient", "mutators", "break_efficacy", "target_efficacy", "max_timed_out_share", "packages", "exclusions", "required_kills"):
        if key not in config:
            raise GateError(f"{path}: missing {key!r}")
    if not all(str(config["tool"].get(k, "")).strip() for k in ("module", "version", "sum", "install")):
        raise GateError(f"{path}: tool needs module, version, sum and install")
    if not 0 <= config["max_timed_out_share"] < 1:
        raise GateError(f"{path}: max_timed_out_share must be a fraction from 0 up to 1")
    unknown = [m for m in config["mutators"] if m not in ALL_MUTATORS]
    if not config["mutators"] or unknown:
        raise GateError(f"{path}: mutators must be a non-empty subset of {', '.join(ALL_MUTATORS)}; unknown: {unknown}")
    if not 0 < config["break_efficacy"] <= config["target_efficacy"] <= 100:
        raise GateError(f"{path}: want 0 < break_efficacy <= target_efficacy <= 100")
    if not isinstance(config["workers"], int) or config["workers"] < 1:
        raise GateError(f"{path}: workers must be a positive integer")
    for exclusion in config["exclusions"]:
        if not str(exclusion.get("reason", "")).strip():
            raise GateError(f"{path}: exclusion {exclusion.get('package')!r} needs a reason")
    for kill in config["required_kills"]:
        if not all(str(kill.get(k, "")).strip() for k in ("file", "contains", "reason")):
            raise GateError(f"{path}: required kill {kill!r} needs file, contains and reason")
    return config


def package_name(package: str) -> str:
    """./internal/usage -> internal-usage, used for report file names."""
    return package.strip("./").replace("/", "-") or "root"


def check_scope(config: dict, listing: dict[str, int]) -> None:
    """listing maps every ./package of the module to its test file count."""
    scope = list(config["packages"])
    excluded = [e["package"] for e in config["exclusions"]]
    problems = []
    for package in sorted(set(scope) & set(excluded)):
        problems.append(f"{package} is both in scope and excluded")
    for package in sorted(set(scope) | set(excluded)):
        if package not in listing:
            problems.append(f"{package} is configured but is not a package of the module")
    for package in sorted(listing):
        if package not in scope and package not in excluded:
            problems.append(f"{package} is neither in scope nor excluded with a reason")
    for package in scope:
        if listing.get(package) == 0:
            problems.append(f"{package} is in scope but has no tests, so no mutant could be killed")
    if problems:
        raise GateError("scope: " + "; ".join(problems))


def require_sandbox(environ: dict[str, str], attestation: Path = SANDBOX_ATTESTATION, proc: Path = Path("/proc")) -> None:
    """Refuse to run Go unless this process is inside the sandbox that
    scripts/mutation_sandbox.py set up and proved: its attestation file holds
    the nonce the launcher put in the environment, HOME is the sandbox's, the
    only network interface is loopback and PID 1 is bwrap's init. On a normal
    host every one of these fails, so mutants never run with the host's HOME,
    tools or network."""
    hint = "run mutants through scripts/mutation_sandbox.py, which proves its sandbox first"
    try:
        data = json.loads(attestation.read_text(encoding="utf-8"))
    except (OSError, ValueError) as err:
        raise GateError(f"refusing to run outside the mutation sandbox ({attestation}: {err}); {hint}") from err
    nonce = environ.get("MUTATION_SANDBOX_NONCE", "")
    if not isinstance(data, dict) or not nonce or data.get("nonce") != nonce:
        raise GateError(f"refusing to run: the sandbox attestation does not match this environment; {hint}")
    if environ.get("HOME") != data.get("home"):
        raise GateError(f"refusing to run: HOME is not the sandbox's empty home; {hint}")
    try:
        lines = (proc / "net" / "dev").read_text(encoding="utf-8").splitlines()[2:]
        init = (proc / "1" / "comm").read_text(encoding="utf-8").strip()
    except OSError as err:
        raise GateError(f"refusing to run: cannot read {proc}: {err}; {hint}") from err
    interfaces = sorted(line.split(":", 1)[0].strip() for line in lines if ":" in line)
    if interfaces != ["lo"]:
        raise GateError(f"refusing to run: network interfaces {interfaces}, the sandbox has only lo; {hint}")
    if init != "bwrap":
        raise GateError(f"refusing to run: PID 1 is {init!r}, not the sandbox's bwrap init; {hint}")


def go_list(root: Path) -> dict[str, int]:
    out = run(["go", "list", "-f", "{{.ImportPath}} {{.Module.Path}} {{len .TestGoFiles}} {{len .XTestGoFiles}}", "./..."], root)
    listing = {}
    for line in out.splitlines():
        import_path, module, tests, xtests = line.split()
        rel = import_path[len(module):].lstrip("/")
        listing["./" + rel if rel else "."] = int(tests) + int(xtests)
    return listing


def run(args: list[str], cwd: Path) -> str:
    try:
        proc = subprocess.run(args, cwd=cwd, capture_output=True, text=True, encoding="utf-8", errors="replace", check=False)
    except OSError as err:
        raise GateError(f"cannot run {args[0]}: {err}") from err
    if proc.returncode != 0:
        raise GateError(f"{' '.join(args)} exited {proc.returncode}: {proc.stderr.strip()[-2000:]}")
    return proc.stdout


def check_tool(binary: str, tool: dict, root: Path) -> str:
    path = shutil.which(binary)
    if path is None:
        raise GateError(f"{binary} not found; install it with: {tool['install']}")
    info = run(["go", "version", "-m", path], root)
    for match in GO_VERSION_MOD.finditer(info):
        if match["module"] == tool["module"]:
            if match["version"] != tool["version"]:
                raise GateError(f"{path} is {tool['module']} {match['version']}, want {tool['version']}: {tool['install']}")
            if match["sum"] != tool["sum"]:
                raise GateError(f"{path} was built from {tool['module']} {tool['version']} with module sum {match['sum']}, want {tool['sum']}")
            return path
    raise GateError(f"{path} is not a build of {tool['module']}; install it with: {tool['install']}")


def resolve_anchors(config: dict, root: Path) -> list[Anchor]:
    anchors = []
    for kill in config["required_kills"]:
        anchor = Anchor(kill["file"], kill["contains"], kill["reason"])
        try:
            lines = (root / anchor.file).read_text(encoding="utf-8").splitlines()
        except OSError as err:
            raise GateError(f"required kill {anchor.file}: {err}") from err
        matches = [n for n, text in enumerate(lines, 1) if anchor.contains in text]
        if len(matches) != 1:
            raise GateError(f"required kill {anchor.contains!r} matches {len(matches)} lines of {anchor.file}, want exactly 1; update scripts/mutation-gate.json")
        anchor.line = matches[0]
        anchors.append(anchor)
    return anchors


def parse_report(package: str, data: dict) -> tuple[dict[str, int], list[Mutant]]:
    counts = {status: 0 for status in STATUSES}
    mutants = []
    try:
        for entry in data["files"] or []:
            name = entry["file_name"].replace("\\", "/")
            # gremlins names files relative to the package it mutated.
            file = name if name.startswith(package.lstrip("./") + "/") else f"{package.lstrip('./')}/{name}"
            for m in entry["mutations"] or []:
                status = m["status"]
                if status not in counts:
                    raise GateError(f"{package}: unknown mutant status {status!r}")
                counts[status] += 1
                mutants.append(Mutant(package, file, int(m["line"]), int(m["column"]), m["type"], status))
    except (KeyError, TypeError, ValueError) as err:
        raise GateError(f"{package}: malformed gremlins report: {err!r}") from err
    return counts, mutants


def evaluate(config: dict, reports: dict[str, dict], anchors: list[Anchor]) -> Result:
    packages, mutants = {}, []
    for package, data in reports.items():
        counts, found = parse_report(package, data)
        if counts[KILLED] + counts[LIVED] == 0:
            raise GateError(f"{package}: no mutant was measured; its tests cover no mutated line")
        packages[package] = counts
        mutants.extend(found)
    result = Result(packages, mutants, anchors)
    for package, counts in sorted(packages.items()):
        share = counts[TIMED_OUT] / (counts[KILLED] + counts[LIVED] + counts[TIMED_OUT])
        if share > config["max_timed_out_share"]:
            result.failures.append(
                f"{package}: {counts[TIMED_OUT]} timed-out mutants ({100 * share:.1f}% of those run) exceed "
                f"{100 * config['max_timed_out_share']:.1f}%; efficacy without them is not trustworthy (slow host or hanging mutants)")
    total = efficacy(result.totals)
    if total is None or total < config["break_efficacy"]:
        shown = "none" if total is None else f"{total:.2f}%"
        result.failures.append(f"total test efficacy {shown} is below the break threshold {config['break_efficacy']:.2f}%")
    for anchor in anchors:
        if "./" + anchor.file.rsplit("/", 1)[0] not in reports:
            continue  # its package is not in this run
        at_line = [m for m in mutants if m.file == anchor.file and m.line == anchor.line]
        if not at_line:
            raise GateError(f"required kill {anchor.file}:{anchor.line} ({anchor.contains!r}) has no mutant; update scripts/mutation-gate.json")
        for m in at_line:
            if m.status not in (KILLED, NOT_VIABLE):
                result.failures.append(f"required kill {m.file}:{m.line}:{m.column} {m.type} is {m.status} ({anchor.reason})")
    return result


def pct(value: float | None) -> str:
    return "n/a" if value is None else f"{value:.2f}%"


def source_line(root: Path, mutant: Mutant, cache: dict[str, list[str]]) -> str:
    if mutant.file not in cache:
        try:
            cache[mutant.file] = (root / mutant.file).read_text(encoding="utf-8").splitlines()
        except OSError:
            cache[mutant.file] = []
    lines = cache[mutant.file]
    return lines[mutant.line - 1].strip() if 0 < mutant.line <= len(lines) else ""


def render(config: dict, result: Result, root: Path) -> tuple[str, str]:
    totals = result.totals
    verdict = "FAIL" if result.failures else "PASS"
    out = [
        f"## Mutation testing: {verdict}",
        "",
        f"Tool {config['tool']['module']} {config['tool']['version']}, {config['workers']} workers, timeout coefficient {config['timeout_coefficient']}, mutators {', '.join(config['mutators'])}.",
        f"Total test efficacy {pct(efficacy(totals))} (break {config['break_efficacy']:.2f}%, target {config['target_efficacy']:.2f}%), mutant coverage {pct(mutant_coverage(totals))}.",
        "Efficacy = killed / (killed + lived). Timed-out, not-viable and not-covered mutants are outside that denominator.",
        "",
        "| Package | Killed | Lived | Timed out | Not covered | Not viable | Efficacy | Mutant coverage |",
        "| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |",
    ]
    for package, counts in sorted(result.packages.items()):
        out.append(f"| `{package}` | {counts[KILLED]} | {counts[LIVED]} | {counts[TIMED_OUT]} | {counts[NOT_COVERED]} | {counts[NOT_VIABLE]} | {pct(efficacy(counts))} | {pct(mutant_coverage(counts))} |")
    out.append(f"| **Total** | {totals[KILLED]} | {totals[LIVED]} | {totals[TIMED_OUT]} | {totals[NOT_COVERED]} | {totals[NOT_VIABLE]} | {pct(efficacy(totals))} | {pct(mutant_coverage(totals))} |")
    if result.timings:
        out += ["", "| Package | gremlins run | First test run (sets each mutant's deadline) |", "| --- | ---: | ---: |"]
        for package, t in sorted(result.timings.items()):
            out.append(f"| `{package}` | {t['seconds']:.1f} s | {t['coverage_run'] or 'not logged'} |")
    if config["exclusions"]:
        out += ["", "Excluded packages:", ""]
        out += [f"- `{e['package']}`: {e['reason']}" for e in config["exclusions"]]
    if result.anchors:
        out += ["", "Required kills (any mutant there other than KILLED or NOT VIABLE fails the gate):", ""]
        out += [f"- `{a.file}:{a.line}` `{a.contains}`: {a.reason}" for a in result.anchors]
    if result.failures:
        out += ["", "Failures:", ""] + [f"- {f}" for f in result.failures]
    cache: dict[str, list[str]] = {}
    survivors = sorted((m for m in result.mutants if m.status == LIVED), key=lambda m: (m.file, m.line, m.column, m.type))
    timed_out = sorted((m for m in result.mutants if m.status == TIMED_OUT), key=lambda m: (m.file, m.line, m.column, m.type))
    lines = [f"## Surviving mutants ({len(survivors)})", ""]
    if not survivors:
        lines.append("None.")
    for m in survivors:
        lines.append(f"- `{m.file}:{m.line}:{m.column}` {m.type}: `{source_line(root, m, cache)}`")
    lines += ["", f"## Timed-out mutants ({len(timed_out)})", ""]
    if not timed_out:
        lines.append("None.")
    for m in timed_out:
        lines.append(f"- `{m.file}:{m.line}:{m.column}` {m.type}: `{source_line(root, m, cache)}`")
    return "\n".join(out) + "\n", "\n".join(lines) + "\n"


def gremlins_args(gremlins: str, package: str, config: dict, report: Path, settings: Path) -> list[str]:
    args = [gremlins, "unleash", "--config", str(settings),
            "--workers", str(config["workers"]), "--timeout-coefficient", str(config["timeout_coefficient"])]
    args += [f"--{m}={'true' if m in config['mutators'] else 'false'}" for m in ALL_MUTATORS]
    return args + ["--output", str(report), package]


def go_dirs(root: Path) -> dict[str, str]:
    """The Go directories as the caller's environment resolves them, before
    HOME is replaced, so builds keep their module and build caches."""
    names = ("GOPATH", "GOMODCACHE", "GOCACHE", "GOENV")
    values = run(["go", "env", *names], root).splitlines()
    if len(values) != len(names):
        raise GateError(f"go env printed {len(values)} lines for {len(names)} names")
    return dict(zip(names, values))


def gremlins_env(environ: dict[str, str], dirs: dict[str, str], home: Path) -> dict[str, str]:
    """The environment for gremlins and every test run it starts.

    Only an allowlist of the caller's variables passes. HOME and the per-user
    folders point at an empty throwaway folder: a mutant can switch off a test's
    fixture path and send the code to its default location, for example a
    `len(paths) == 0` negation in internal/agent falls back to the host's own T3
    database. gremlins also times each mutant against its first `go test
    -cover` run; a cached result makes that run take milliseconds and nearly
    every mutant time out, so test caching is off (-count=1)."""
    env = {k: v for k, v in environ.items() if k in ENV_KEEP}
    env.update(dirs)
    for name, sub in (("HOME", ""), ("USERPROFILE", ""), ("XDG_CONFIG_HOME", ".config"), ("XDG_CACHE_HOME", ".cache"),
                      ("XDG_DATA_HOME", ".local/share"), ("XDG_STATE_HOME", ".local/state"),
                      ("APPDATA", "AppData/Roaming"), ("LOCALAPPDATA", "AppData/Local")):
        env[name] = str(home / sub) if sub else str(home)
    env["GOFLAGS"] = (env.get("GOFLAGS", "") + " -count=1").strip()
    return env


def unleash(gremlins: str, package: str, config: dict, report: Path, log: Path, root: Path, env: dict[str, str]) -> dict:
    settings = report.with_name("gremlins-config.yaml")
    settings.write_text(GREMLINS_CONFIG, encoding="utf-8")
    args = gremlins_args(gremlins, package, config, report, settings)
    print(f"mutation: {' '.join(args[1:])}", flush=True)
    with log.open("w", encoding="utf-8") as sink:
        proc = subprocess.run(args, cwd=root, env=env, stdout=sink, stderr=subprocess.STDOUT, check=False)
    text = log.read_text(encoding="utf-8", errors="replace")
    if proc.returncode != 0:
        raise GateError(f"gremlins exited {proc.returncode} on {package}; see {log}")
    if TOOL_ERRORS.search(text):
        raise GateError(f"gremlins could not apply or restore a mutation in {package}; see {log}")
    try:
        return json.loads(report.read_text(encoding="utf-8"))
    except (OSError, ValueError) as err:
        raise GateError(f"{package}: cannot read gremlins report {report}: {err}") from err


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--config", type=Path, default=CONFIG_DEFAULT)
    parser.add_argument("--root", type=Path, default=REPO_ROOT, help="module root (default: the repository)")
    parser.add_argument("--packages", nargs="+", help="run only these in-scope packages, e.g. ./internal/usage")
    parser.add_argument("--report-dir", type=Path, default=Path("mutation-report"))
    parser.add_argument("--summary", type=Path, help="append the summary and survivors here (e.g. $GITHUB_STEP_SUMMARY)")
    parser.add_argument("--from-reports", type=Path, help="evaluate existing <package>.json reports in this directory")
    parser.add_argument("--gremlins", default=os.environ.get("GREMLINS", "gremlins"), help="gremlins binary")
    args = parser.parse_args(argv)
    try:
        config = load_config(args.config)
        root = args.root.resolve()
        packages = args.packages or config["packages"]
        unknown = [p for p in packages if p not in config["packages"]]
        if unknown:
            raise GateError(f"not in scope: {', '.join(unknown)}")
        anchors = resolve_anchors(config, root)
        args.report_dir.mkdir(parents=True, exist_ok=True)
        reports, timings = {}, {}
        if args.from_reports:
            for package in packages:
                path = args.from_reports / f"{package_name(package)}.json"
                try:
                    reports[package] = json.loads(path.read_text(encoding="utf-8"))
                except (OSError, ValueError) as err:
                    raise GateError(f"cannot read {path}: {err}") from err
        else:
            overrides = sorted(name for name in os.environ if name.upper().startswith("GREMLINS_"))
            if overrides:
                raise GateError(f"unset {', '.join(overrides)}: gremlins settings come from scripts/mutation-gate.json only")
            require_sandbox(dict(os.environ))
            check_scope(config, go_list(root))
            gremlins = check_tool(args.gremlins, config["tool"], root)
            dirs = go_dirs(root)
            with tempfile.TemporaryDirectory(prefix="mutation-home-") as home:
                env = gremlins_env(dict(os.environ), dirs, Path(home))
                for package in packages:
                    name = package_name(package)
                    log = (args.report_dir / f"{name}.log").resolve()
                    started = time.monotonic()
                    reports[package] = unleash(gremlins, package, config, (args.report_dir / f"{name}.json").resolve(), log, root, env)
                    coverage = COVERAGE_TIME.search(log.read_text(encoding="utf-8", errors="replace"))
                    timings[package] = {"seconds": round(time.monotonic() - started, 1), "coverage_run": coverage[1] if coverage else None}
        result = evaluate(config, reports, anchors)
        result.timings = timings
    except GateError as err:
        print(f"mutation: error: {err}", file=sys.stderr)
        return 2
    summary, survivors = render(config, result, root)
    (args.report_dir / "summary.md").write_text(summary, encoding="utf-8")
    (args.report_dir / "survivors.md").write_text(survivors, encoding="utf-8")
    (args.report_dir / "summary.json").write_text(json.dumps({
        "verdict": "fail" if result.failures else "pass",
        "break_efficacy": config["break_efficacy"],
        "target_efficacy": config["target_efficacy"],
        "efficacy": efficacy(result.totals),
        "mutant_coverage": mutant_coverage(result.totals),
        "packages": result.packages,
        "timings": result.timings,
        "failures": result.failures,
        "survivors": [m.__dict__ for m in result.mutants if m.status == LIVED],
        "timed_out": [m.__dict__ for m in result.mutants if m.status == TIMED_OUT],
    }, indent=2) + "\n", encoding="utf-8")
    if args.summary:
        with args.summary.open("a", encoding="utf-8") as sink:
            sink.write(summary + "\n" + survivors)
    sys.stdout.write(summary + "\n" + survivors)
    return 1 if result.failures else 0


if __name__ == "__main__":
    sys.exit(main())
