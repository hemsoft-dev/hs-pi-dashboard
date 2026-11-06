"""Tests for crap_gate.py. Run: python3 -m unittest discover -s scripts -p "test_*.py"."""

import json
import tempfile
import unittest
from pathlib import Path

import crap_gate as gate

MODULE = "github.com/HemSoft/hs-pi-dashboard"

COVER = r"""github.com/HemSoft/hs-pi-dashboard/internal/usage/usage.go:700:	credsFromMap		0.0%
github.com/HemSoft/hs-pi-dashboard/internal/t3/collector.go:75:	Poll			97.9%
github.com/HemSoft/hs-pi-dashboard/internal/t3/collector.go:300:	small			100.0%
total:								(statements)		83.5%
"""

GOCYCLO_LINUX = r"""49 t3 (*Collector).Poll internal/t3/collector.go:75:1
8 usage credsFromMap internal/usage/usage.go:700:1
2 t3 small internal/t3/collector.go:300:1
14 t3 verifyRuntimeProcess internal/t3/process_windows.go:20:1
"""

# gocyclo on Windows prints backslash paths.
GOCYCLO_WINDOWS = GOCYCLO_LINUX.replace("internal/t3/", "internal\\t3\\").replace("internal/usage/", "internal\\usage\\")


def config(**overrides):
    base = {
        "crap_threshold": 30,
        "platforms": {
            "linux": {"coverage_floor": 83.0, "worst_crap": 72.0},
            "windows": {"coverage_floor": 80.0, "worst_crap": 72.0},
        },
        "exceptions": [
            {"function": "internal/usage/usage.go:credsFromMap",
             "reason": "Untested credential parsing at the #88 baseline.",
             "max_crap": {"linux": 72.0, "windows": 72.0}},
            {"function": "internal/t3/collector.go:(*Collector).Poll",
             "reason": "Complexity 49 hotspot at the #88 baseline.",
             "max_crap": {"linux": 49.0}},
        ],
    }
    base.update(overrides)
    return base


def measure(cover=COVER, cyclo=GOCYCLO_LINUX):
    cover_map, total = gate.parse_cover_func(cover, MODULE)
    cyclo_map = gate.parse_gocyclo(cyclo, MODULE)
    functions, skipped = gate.join_functions(cover_map, cyclo_map)
    keys = {f"{file}:{name}" for (file, _), (name, _) in cyclo_map.items()}
    return functions, total, skipped, keys


class CrapScoreTest(unittest.TestCase):
    def test_formula_matches_known_values(self):
        self.assertEqual(gate.crap_score(8, 0.0), 72.0)
        self.assertEqual(gate.crap_score(49, 97.9), 49.0)
        self.assertEqual(gate.crap_score(40, 86.9), 43.6)
        self.assertEqual(gate.crap_score(5, 100.0), 5.0)


class ParseTest(unittest.TestCase):
    def test_join_skips_functions_not_built_on_platform(self):
        functions, total, skipped, _ = measure()
        self.assertEqual(total, 83.5)
        self.assertEqual(skipped, 1)
        self.assertEqual([f.key for f in functions], [
            "internal/usage/usage.go:credsFromMap",
            "internal/t3/collector.go:(*Collector).Poll",
            "internal/t3/collector.go:small",
        ])

    def test_windows_paths_normalize_to_the_same_keys(self):
        linux, *_ = measure()
        windows, *_ = measure(cyclo=GOCYCLO_WINDOWS)
        self.assertEqual([f.key for f in linux], [f.key for f in windows])

    def test_covered_function_without_complexity_is_an_error(self):
        cyclo = "\n".join(line for line in GOCYCLO_LINUX.splitlines() if "small" not in line)
        with self.assertRaisesRegex(gate.GateError, "no gocyclo entry"):
            measure(cyclo=cyclo)

    def test_unrecognized_lines_are_errors(self):
        with self.assertRaises(gate.GateError):
            gate.parse_cover_func("garbage\n", MODULE)
        with self.assertRaises(gate.GateError):
            gate.parse_gocyclo("garbage\n", MODULE)
        with self.assertRaisesRegex(gate.GateError, "no total"):
            gate.parse_cover_func(COVER.splitlines()[0], MODULE)

    def test_package_coverage_merges_duplicate_blocks(self):
        profile = "\n".join([
            "mode: atomic",
            f"{MODULE}/internal/t3/a.go:1.1,2.2 3 0",
            f"{MODULE}/internal/t3/a.go:1.1,2.2 3 4",
            f"{MODULE}/internal/t3/a.go:3.1,4.2 1 0",
            f"{MODULE}/main.go:1.1,2.2 2 0",
        ])
        self.assertEqual(gate.package_coverage(profile, MODULE), {".": (0, 2), "internal/t3": (3, 4)})


class EvaluateTest(unittest.TestCase):
    def run_gate(self, cfg, platform="linux", cover=COVER, cyclo=GOCYCLO_LINUX):
        functions, total, _, keys = measure(cover, cyclo)
        return gate.evaluate(cfg, platform, functions, total, keys)

    def test_baseline_passes(self):
        outcome = self.run_gate(config())
        self.assertEqual(outcome.violations, [])

    def test_failing_control_hotspot_coverage_drop_is_rejected(self):
        # A test change that lowers Poll from 97.9% to 90% raises CRAP 49.0 -> 51.4.
        outcome = self.run_gate(config(), cover=COVER.replace("97.9%", "90.0%"))
        self.assertEqual(len(outcome.violations), 1, outcome.violations)
        self.assertIn("Poll CRAP rose from its baseline 49.0 to 51.4", outcome.violations[0])

    def test_failing_control_new_worst_score_is_rejected(self):
        # At 70% Poll scores 113.8, above its ceiling and above the platform's worst baseline 72.0.
        outcome = self.run_gate(config(), cover=COVER.replace("97.9%", "70.0%"))
        self.assertEqual(len(outcome.violations), 2, outcome.violations)
        self.assertIn("Worst CRAP 113.8", outcome.violations[0])
        self.assertIn("Poll CRAP rose from its baseline 49.0 to 113.8", outcome.violations[1])

    def test_failing_control_new_function_above_threshold_is_rejected(self):
        # complexity 12 at 45%: 144 * 0.55^3 + 12 = 36.0.
        cover = COVER.replace("total:", f"{MODULE}/internal/t3/new.go:10:\tfresh\t\t45.0%\ntotal:")
        cyclo = GOCYCLO_LINUX + "12 t3 fresh internal/t3/new.go:10:1\n"
        outcome = self.run_gate(config(), cover=cover, cyclo=cyclo)
        self.assertEqual(len(outcome.violations), 1, outcome.violations)
        self.assertIn("internal/t3/new.go:fresh (line 10) has CRAP 36.0 above 30", outcome.violations[0])
        self.assertIn("no checked-in exception", outcome.violations[0])

    def test_new_function_at_threshold_passes(self):
        # complexity 12 at 50%: 144 * 0.125 + 12 = 30.0, not above 30.
        cover = COVER.replace("total:", f"{MODULE}/internal/t3/new.go:10:\tfresh\t\t50.0%\ntotal:")
        cyclo = GOCYCLO_LINUX + "12 t3 fresh internal/t3/new.go:10:1\n"
        cfg = config()
        functions, total, _, keys = measure(cover, cyclo)
        self.assertEqual([f.crap for f in functions if f.name == "fresh"], [30.0])
        self.assertEqual(gate.evaluate(cfg, "linux", functions, total, keys).violations, [])

    def test_changed_function_crossing_threshold_is_rejected(self):
        cyclo = GOCYCLO_LINUX.replace("2 t3 small", "40 t3 small")
        cover = COVER.replace("100.0%", "80.0%")
        outcome = self.run_gate(config(), cover=cover, cyclo=cyclo)
        self.assertTrue(any("small" in v and "no checked-in exception" in v for v in outcome.violations))

    def test_coverage_floor(self):
        outcome = self.run_gate(config(), cover=COVER.replace("83.5%", "82.9%"))
        self.assertEqual(outcome.violations, ["Total statement coverage 82.9% is below the declared floor 83.0%."])

    def test_worst_baseline_is_enforced_independently(self):
        cfg = config()
        cfg["platforms"]["linux"]["worst_crap"] = 60.0
        outcome = self.run_gate(cfg)
        self.assertEqual(len(outcome.violations), 1)
        self.assertIn("Worst CRAP 72.0", outcome.violations[0])

    def test_exception_without_platform_ceiling_fails(self):
        # Poll's exception has no windows ceiling.
        outcome = self.run_gate(config(), platform="windows", cyclo=GOCYCLO_WINDOWS)
        self.assertEqual(len(outcome.violations), 1)
        self.assertIn("no max_crap.windows ceiling", outcome.violations[0])

    def test_stale_exception_fails(self):
        cfg = config()
        cfg["exceptions"].append({"function": "internal/gone.go:old", "reason": "Removed long ago, kept by mistake.",
                                  "max_crap": {"linux": 40.0}})
        outcome = self.run_gate(cfg)
        self.assertEqual(outcome.violations, ["Exception internal/gone.go:old names a function that no longer exists; remove it."])

    def test_improvement_is_a_notice_not_a_failure(self):
        # credsFromMap at 50%: 64 * 0.125 + 8 = 16.0, under the threshold.
        cover = COVER.replace("credsFromMap\t\t0.0%", "credsFromMap\t\t50.0%")
        outcome = self.run_gate(config(), cover=cover)
        self.assertEqual(outcome.violations, [])
        self.assertEqual(outcome.notices, [
            "internal/usage/usage.go:credsFromMap is now at CRAP 16.0; its linux ceiling can be removed."])


class ConfigTest(unittest.TestCase):
    def write(self, data):
        handle = tempfile.NamedTemporaryFile("w", suffix=".json", delete=False, encoding="utf-8")
        with handle:
            json.dump(data, handle)
        self.addCleanup(Path(handle.name).unlink)
        return Path(handle.name)

    def test_exception_needs_reason(self):
        cfg = config()
        cfg["exceptions"][0]["reason"] = " "
        with self.assertRaisesRegex(gate.GateError, "reason"):
            gate.load_config(self.write(cfg), "linux")

    def test_missing_platform_fails(self):
        with self.assertRaisesRegex(gate.GateError, "platforms.macos"):
            gate.load_config(self.write(config()), "macos")

    def test_duplicate_exception_fails(self):
        cfg = config()
        cfg["exceptions"].append(dict(cfg["exceptions"][0]))
        with self.assertRaisesRegex(gate.GateError, "repeats"):
            gate.load_config(self.write(cfg), "linux")

    def test_checked_in_config_is_valid_for_both_platforms(self):
        for platform in ("linux", "windows"):
            cfg = gate.load_config(gate.CONFIG_DEFAULT, platform)
            floor = cfg["platforms"][platform]["coverage_floor"]
            self.assertGreater(floor, 0, f"{platform} needs a declared coverage floor")
            for exc in cfg["exceptions"]:
                for value in exc["max_crap"].values():
                    self.assertGreater(value, cfg["crap_threshold"], f"{exc['function']} ceiling is not above the threshold")


if __name__ == "__main__":
    unittest.main()
