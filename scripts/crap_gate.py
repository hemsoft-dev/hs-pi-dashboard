#!/usr/bin/env python3
"""Coverage and per-function CRAP report with a non-regression gate.

CI runs this after the Go tests on Linux and Windows, for example:

    python3 scripts/crap_gate.py --platform linux --profile cover.out
        --report-dir crap-report --summary "$GITHUB_STEP_SUMMARY"

It joins `go tool cover -func` (statement coverage per function) with pinned
gocyclo output (cyclomatic complexity per function) for production code, then
computes CRAP = complexity^2 * (1 - coverage)^3 + complexity.

Go reports statement coverage only, so CRAP here uses statement coverage, not
branch coverage. The gate reads scripts/crap-gate.json and fails when:

- total statement coverage drops below the platform's declared floor;
- any production function scores above the CRAP threshold without a
  checked-in exception that names a reason and a ceiling for this platform;
- a function with an exception scores above its ceiling (the ceilings are the
  measured baseline, so a test change that lowers a hotspot's coverage fails);
- the worst CRAP score on the platform rises above the declared baseline;
- an exception names a function that no longer exists.

Functions in files that are not built on the platform (for example
`*_windows.go` on Linux) have no coverage entry there and are skipped; the
other platform's run measures them.

`--suggest` prints measured values for refreshing the config after a deliberate
change. It never edits the config.
"""

from __future__ import annotations

import argparse
import json
import math
import os
import re
import shutil
import subprocess
import sys
from dataclasses import dataclass
from pathlib import Path

CONFIG_DEFAULT = Path(__file__).with_name("crap-gate.json")
GOCYCLO_IGNORE = r"_test\.go$"
WORST_ROWS = 20

COVER_FUNC_RE = re.compile(r"^(?P<file>.+?):(?P<line>\d+):\s+(?P<name>\S+)\s+(?P<pct>\d+(?:\.\d+)?)%$")
COVER_TOTAL_RE = re.compile(r"^total:\s+\(statements\)\s+(?P<pct>\d+(?:\.\d+)?)%$")
GOCYCLO_RE = re.compile(r"^(?P<comp>\d+)\s+(?P<pkg>\S+)\s+(?P<name>.+?)\s+(?P<file>.+):(?P<line>\d+):(?P<col>\d+)$")
PROFILE_RE = re.compile(r"^(?P<file>.+):(?P<range>\d+\.\d+,\d+\.\d+)\s+(?P<stmts>\d+)\s+(?P<count>\d+)$")


class GateError(Exception):
    """Bad input, config or tool output. Exit code 2, distinct from a gate failure."""


@dataclass(frozen=True)
class Function:
    file: str
    line: int
    name: str
    complexity: int
    coverage: float  # percent, as go tool cover prints it (0.1 resolution)

    @property
    def key(self) -> str:
        return f"{self.file}:{self.name}"

    @property
    def crap(self) -> float:
        return crap_score(self.complexity, self.coverage)


def crap_score(complexity: int, coverage_pct: float) -> float:
    uncovered = 1.0 - coverage_pct / 100.0
    return round(complexity * complexity * uncovered ** 3 + complexity, 1)


def normalize_path(path: str, module: str) -> str:
    path = path.replace("\\", "/")
    if path.startswith(module + "/"):
        path = path[len(module) + 1:]
    while path.startswith("./"):
        path = path[2:]
    return path


def read_module(go_mod: Path) -> str:
    for raw in go_mod.read_text(encoding="utf-8").splitlines():
        parts = raw.split()
        if len(parts) == 2 and parts[0] == "module":
            return parts[1]
    raise GateError(f"no module line in {go_mod}")


def parse_cover_func(text: str, module: str) -> tuple[dict[tuple[str, int], tuple[str, float]], float]:
    functions: dict[tuple[str, int], tuple[str, float]] = {}
    total = None
    for raw in text.splitlines():
        line = raw.strip()
        if not line:
            continue
        match = COVER_TOTAL_RE.match(line)
        if match:
            total = float(match["pct"])
            continue
        match = COVER_FUNC_RE.match(line)
        if not match:
            raise GateError(f"unrecognized go tool cover line: {line!r}")
        key = (normalize_path(match["file"], module), int(match["line"]))
        functions[key] = (match["name"], float(match["pct"]))
    if total is None:
        raise GateError("go tool cover output has no total line")
    return functions, total


def parse_gocyclo(text: str, module: str) -> dict[tuple[str, int], tuple[str, int]]:
    result: dict[tuple[str, int], tuple[str, int]] = {}
    for raw in text.splitlines():
        line = raw.strip()
        if not line:
            continue
        match = GOCYCLO_RE.match(line)
        if not match:
            raise GateError(f"unrecognized gocyclo line: {line!r}")
        key = (normalize_path(match["file"], module), int(match["line"]))
        result[key] = (match["name"], int(match["comp"]))
    if not result:
        raise GateError("gocyclo reported no functions")
    return result


def join_functions(cover: dict, cyclo: dict) -> tuple[list[Function], int]:
    """Return measured production functions and the count not built on this platform."""
    missing = sorted(set(cover) - set(cyclo))
    if missing:
        file, line = missing[0]
        raise GateError(
            f"{len(missing)} covered function(s) have no gocyclo entry, first {file}:{line}; "
            "the two tools disagree about function positions"
        )
    functions = []
    for key, (name, complexity) in cyclo.items():
        if key not in cover:
            continue
        functions.append(Function(key[0], key[1], name, complexity, cover[key][1]))
    functions.sort(key=lambda f: (-f.crap, -f.complexity, f.key))
    return functions, len(cyclo) - len(functions)


def package_coverage(profile_text: str, module: str) -> dict[str, tuple[int, int]]:
    """Statement totals per package directory: {pkg: (covered, total)}.

    A block can appear more than once when profiles are merged; it counts as
    covered if any copy ran.
    """
    blocks: dict[tuple[str, str], tuple[int, bool]] = {}
    lines = profile_text.splitlines()
    if not lines or not lines[0].startswith("mode:"):
        raise GateError("coverage profile has no mode line")
    for raw in lines[1:]:
        line = raw.strip()
        if not line:
            continue
        match = PROFILE_RE.match(line)
        if not match:
            raise GateError(f"unrecognized coverage profile line: {line!r}")
        key = (normalize_path(match["file"], module), match["range"])
        stmts = int(match["stmts"])
        hit = int(match["count"]) > 0
        previous = blocks.get(key)
        blocks[key] = (stmts, hit or (previous is not None and previous[1]))
    packages: dict[str, list[int]] = {}
    for (file, _), (stmts, hit) in blocks.items():
        pkg = file.rsplit("/", 1)[0] if "/" in file else "."
        entry = packages.setdefault(pkg, [0, 0])
        entry[1] += stmts
        if hit:
            entry[0] += stmts
    return {pkg: (c, t) for pkg, (c, t) in sorted(packages.items())}


def load_config(path: Path, platform: str) -> dict:
    try:
        config = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError) as exc:
        raise GateError(f"cannot read {path}: {exc}") from exc
    threshold = config.get("crap_threshold")
    if not isinstance(threshold, (int, float)) or threshold <= 0:
        raise GateError("crap_threshold must be a positive number")
    platforms = config.get("platforms")
    if not isinstance(platforms, dict) or platform not in platforms:
        raise GateError(f"config has no platforms.{platform} entry")
    for name in ("coverage_floor", "worst_crap"):
        value = platforms[platform].get(name)
        if not isinstance(value, (int, float)) or value < 0:
            raise GateError(f"platforms.{platform}.{name} must be a non-negative number")
    seen = set()
    for index, exc in enumerate(config.get("exceptions", [])):
        where = f"exceptions[{index}]"
        function = exc.get("function")
        if not isinstance(function, str) or ":" not in function:
            raise GateError(f"{where}.function must be 'path/file.go:Name'")
        if function in seen:
            raise GateError(f"{where} repeats {function}")
        seen.add(function)
        reason = exc.get("reason")
        if not isinstance(reason, str) or len(reason.strip()) < 10:
            raise GateError(f"{where} ({function}) needs a reason of at least 10 characters")
        ceilings = exc.get("max_crap")
        if not isinstance(ceilings, dict) or not ceilings:
            raise GateError(f"{where} ({function}) needs max_crap per platform")
        for plat, value in ceilings.items():
            if plat not in platforms or not isinstance(value, (int, float)) or value <= 0:
                raise GateError(f"{where} ({function}) has a bad max_crap.{plat}")
    return config


@dataclass
class Outcome:
    violations: list[str]
    notices: list[str]


def evaluate(config: dict, platform: str, functions: list[Function], total: float,
             all_keys: set[str]) -> Outcome:
    threshold = float(config["crap_threshold"])
    plat = config["platforms"][platform]
    exceptions = {exc["function"]: exc for exc in config.get("exceptions", [])}
    measured = {f.key: f for f in functions}
    violations: list[str] = []
    notices: list[str] = []

    if total < plat["coverage_floor"]:
        violations.append(
            f"Total statement coverage {total:.1f}% is below the declared floor {plat['coverage_floor']:.1f}%."
        )

    worst = functions[0].crap if functions else 0.0
    if worst > plat["worst_crap"]:
        violations.append(
            f"Worst CRAP {worst:.1f} ({functions[0].key}) is above the declared baseline {plat['worst_crap']:.1f}."
        )

    for function in functions:
        if function.crap <= threshold:
            continue
        exc = exceptions.get(function.key)
        if exc is None:
            violations.append(
                f"{function.key} (line {function.line}) has CRAP {function.crap:.1f} above {threshold:g} "
                f"(complexity {function.complexity}, coverage {function.coverage:.1f}%) and no checked-in exception."
            )
            continue
        ceiling = exc["max_crap"].get(platform)
        if ceiling is None:
            violations.append(
                f"{function.key} has CRAP {function.crap:.1f} above {threshold:g}; its exception has no "
                f"max_crap.{platform} ceiling."
            )
        elif function.crap > ceiling:
            violations.append(
                f"{function.key} CRAP rose from its baseline {ceiling:.1f} to {function.crap:.1f} "
                f"(complexity {function.complexity}, coverage {function.coverage:.1f}%)."
            )
        elif function.crap < ceiling:
            notices.append(f"{function.key} improved to {function.crap:.1f}; its {platform} ceiling {ceiling:.1f} can be lowered.")

    for key, exc in sorted(exceptions.items()):
        if key not in all_keys:
            violations.append(f"Exception {key} names a function that no longer exists; remove it.")
            continue
        function = measured.get(key)
        if function is not None and function.crap <= threshold and platform in exc["max_crap"]:
            notices.append(f"{key} is now at CRAP {function.crap:.1f}; its {platform} ceiling can be removed.")
    return Outcome(violations, notices)


def suggest(config: dict, platform: str, functions: list[Function], total: float) -> dict:
    threshold = float(config["crap_threshold"])
    return {
        "platform": platform,
        "total_coverage": total,
        "coverage_floor_at_most": math.floor(total * 10) / 10,
        "worst_crap": functions[0].crap if functions else 0.0,
        "above_threshold": {f.key: f.crap for f in functions if f.crap > threshold},
    }


def render_summary(platform: str, config: dict, functions: list[Function], total: float,
                   packages: dict[str, tuple[int, int]], skipped: int, outcome: Outcome) -> str:
    threshold = float(config["crap_threshold"])
    plat = config["platforms"][platform]
    above = sum(1 for f in functions if f.crap > threshold)
    status = "failed" if outcome.violations else "passed"
    out = [
        f"## Coverage and CRAP ({platform}): {status}",
        "",
        f"Statement coverage {total:.1f}% (floor {plat['coverage_floor']:.1f}%). "
        f"{len(functions)} production functions measured, {skipped} not built on {platform}. "
        f"{above} above CRAP {threshold:g}; worst {functions[0].crap if functions else 0:.1f} "
        f"(baseline {plat['worst_crap']:.1f}). Coverage basis: statements.",
        "",
    ]
    if outcome.violations:
        out += ["### Violations", ""] + [f"- {v}" for v in outcome.violations] + [""]
    if outcome.notices:
        out += ["### Notices", ""] + [f"- {n}" for n in outcome.notices] + [""]
    out += ["### Packages", "", "| Coverage | Statements | Package |", "| ---: | ---: | --- |"]
    for pkg, (covered, stmts) in packages.items():
        pct = 100.0 * covered / stmts if stmts else 0.0
        out.append(f"| {pct:.1f}% | {stmts} | `{pkg}` |")
    out += ["", f"### Worst {min(WORST_ROWS, len(functions))} functions", "",
            "| CRAP | Complexity | Coverage | Function |", "| ---: | ---: | ---: | --- |"]
    for f in functions[:WORST_ROWS]:
        out.append(f"| {f.crap:.1f} | {f.complexity} | {f.coverage:.1f}% | `{f.name}`, `{f.file}:{f.line}` |")
    out.append("")
    return "\n".join(out)


def render_tsv(functions: list[Function]) -> str:
    rows = ["crap\tcomplexity\tcoverage\tfunction\tfile\tline"]
    rows += [f"{f.crap:.1f}\t{f.complexity}\t{f.coverage:.1f}\t{f.name}\t{f.file}\t{f.line}" for f in functions]
    return "\n".join(rows) + "\n"


def run_tool(args: list[str], cwd: Path) -> str:
    try:
        result = subprocess.run(args, cwd=cwd, capture_output=True, text=True, encoding="utf-8", check=False)
    except OSError as exc:
        raise GateError(f"cannot run {args[0]}: {exc}") from exc
    if result.returncode != 0:
        raise GateError(f"{' '.join(args)} exited {result.returncode}: {result.stderr.strip()}")
    return result.stdout


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--platform", required=True, help="config section to use, e.g. linux or windows")
    parser.add_argument("--profile", required=True, type=Path, help="go test -coverprofile output")
    parser.add_argument("--config", type=Path, default=CONFIG_DEFAULT)
    parser.add_argument("--repo", type=Path, default=Path(__file__).resolve().parent.parent)
    parser.add_argument("--gocyclo", default="gocyclo", help="pinned gocyclo binary")
    parser.add_argument("--report-dir", type=Path, help="write summary.md, functions.tsv and the profile here")
    parser.add_argument("--summary", type=Path, help="append the Markdown summary to this file")
    parser.add_argument("--suggest", action="store_true", help="print measured baseline values and exit 0")
    args = parser.parse_args(argv)

    if hasattr(sys.stdout, "reconfigure"):
        sys.stdout.reconfigure(encoding="utf-8", errors="replace")
    try:
        repo = args.repo.resolve()
        profile = args.profile.resolve()
        module = read_module(repo / "go.mod")
        config = load_config(args.config, args.platform)
        cover, total = parse_cover_func(run_tool(["go", "tool", "cover", f"-func={profile}"], repo), module)
        cyclo = parse_gocyclo(run_tool([args.gocyclo, "-ignore", GOCYCLO_IGNORE, "."], repo), module)
        functions, skipped = join_functions(cover, cyclo)
        packages = package_coverage(profile.read_text(encoding="utf-8"), module)
    except GateError as exc:
        print(f"crap_gate: {exc}", file=sys.stderr)
        return 2

    if args.suggest:
        print(json.dumps(suggest(config, args.platform, functions, total), indent=2))
        return 0

    all_keys = {f"{file}:{name}" for (file, _), (name, _) in cyclo.items()}
    outcome = evaluate(config, args.platform, functions, total, all_keys)
    summary = render_summary(args.platform, config, functions, total, packages, skipped, outcome)
    print(summary)
    if args.summary:
        with args.summary.open("a", encoding="utf-8") as handle:
            handle.write(summary + "\n")
    if args.report_dir:
        args.report_dir.mkdir(parents=True, exist_ok=True)
        (args.report_dir / "summary.md").write_text(summary + "\n", encoding="utf-8")
        (args.report_dir / "functions.tsv").write_text(render_tsv(functions), encoding="utf-8")
        shutil.copyfile(profile, args.report_dir / "cover.out")
    return 1 if outcome.violations else 0


if __name__ == "__main__":
    sys.exit(main())
