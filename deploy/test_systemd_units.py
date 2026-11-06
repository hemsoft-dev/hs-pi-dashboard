"""Checks the versioned systemd user units Mini runs (issue #89).

Run with: python3 -m unittest discover -s deploy -p 'test_*.py'
"""

import re
import shlex
import unittest
from pathlib import Path

DEPLOY = Path(__file__).resolve().parent
MAIN_GO = DEPLOY.parent / "main.go"
BINARY = "%h/bin/hs-pi-dashboard"

UNITS = {
    "hs-pi-dashboard.service": {"command": "serve", "restart": "on-failure"},
    "hs-pi-dashboard-agent.service": {"command": "agent", "restart": "always"},
}

# Environment names that would carry a credential. Units name settings only;
# credentials come from gh's stored login and the pmd token file.
SECRET_NAME = re.compile(r"TOKEN|SECRET|PASSWORD|PASSWD|KEY|CREDENTIAL|COOKIE|WEBHOOK", re.I)


def parse(path):
    """Return [(section, key, value)] in file order, as systemd reads it."""
    entries = []
    section = None
    for number, raw in enumerate(path.read_text(encoding="utf-8").splitlines(), 1):
        line = raw.strip()
        if not line or line[0] in "#;":
            continue
        if line.startswith("[") and line.endswith("]"):
            section = line[1:-1]
            continue
        if raw.rstrip().endswith("\\"):
            raise AssertionError(f"{path.name}:{number}: line continuations are not used here")
        key, sep, value = line.partition("=")
        if not sep or section is None:
            raise AssertionError(f"{path.name}:{number}: not a key=value line in a section")
        entries.append((section, key.strip(), value.strip()))
    return entries


def drop_ins(unit):
    folder = DEPLOY / f"{unit}.d"
    return sorted(folder.glob("*.conf")) if folder.is_dir() else []


def effective(unit):
    """Merge the unit and its drop-ins: an empty value resets list keys."""
    merged = {}
    for path in [DEPLOY / unit, *drop_ins(unit)]:
        for section, key, value in parse(path):
            values = merged.setdefault((section, key), [])
            if value == "":
                values.clear()
            elif key in ("ExecStart", "Environment", "After", "WantedBy"):
                values.append(value)
            else:
                values[:] = [value]
    return merged


def defined_flags(command):
    source = MAIN_GO.read_text(encoding="utf-8")
    name = {"agent": "runAgent", "serve": "runServe"}[command]
    body = re.search(rf"^func {name}\(.*?^}}", source, re.S | re.M)
    if body is None:
        raise AssertionError(f"main.go has no {name}")
    return set(re.findall(r'fs\.\w+\("([\w-]+)"', body.group(0)))


class SystemdUnitTests(unittest.TestCase):
    def test_restart_policy_matches_mini(self):
        for unit, expected in UNITS.items():
            with self.subTest(unit=unit):
                config = effective(unit)
                self.assertEqual(config[("Service", "Restart")], [expected["restart"]])
                self.assertEqual(config[("Service", "RestartSec")], ["5"])
                self.assertEqual(config[("Install", "WantedBy")], ["default.target"])

    def test_one_command_with_known_flags(self):
        for unit, expected in UNITS.items():
            with self.subTest(unit=unit):
                commands = effective(unit)[("Service", "ExecStart")]
                self.assertEqual(len(commands), 1, "a drop-in must reset ExecStart= first")
                argv = shlex.split(commands[0])
                self.assertEqual(argv[:2], [BINARY, expected["command"]])
                flags = {arg[1:] for arg in argv[2:] if arg.startswith("-")}
                unknown = flags - defined_flags(expected["command"])
                self.assertFalse(unknown, f"flags not defined in main.go: {sorted(unknown)}")
                self.assertIn("addr", flags, "bind the Tailscale address, not every interface")

    def test_drop_ins_reset_exec_start_before_replacing_it(self):
        for unit in UNITS:
            for path in drop_ins(unit):
                keys = [(key, value) for _, key, value in parse(path) if key == "ExecStart"]
                if keys:
                    with self.subTest(drop_in=path.name):
                        self.assertEqual(keys[0], ("ExecStart", ""))
                        self.assertEqual(len(keys), 2)

    def test_no_secrets_or_environment_files(self):
        for unit in UNITS:
            for path in [DEPLOY / unit, *drop_ins(unit)]:
                for _, key, value in parse(path):
                    with self.subTest(file=path.name, key=key):
                        self.assertNotEqual(key, "EnvironmentFile", "keep secret files out of the repo")
                        if key == "Environment":
                            name = value.strip('"').partition("=")[0]
                            self.assertNotRegex(name, SECRET_NAME)

    def test_parser_rejects_an_unknown_flag(self):
        # Positive control for the flag check above.
        self.assertNotIn("not-a-flag", defined_flags("agent"))
        self.assertIn("project-path", defined_flags("agent"))
        self.assertIn("fleet", defined_flags("serve"))


if __name__ == "__main__":
    unittest.main()
