import os
import pathlib
import subprocess
import struct
import tempfile
import unittest
from unittest import mock
import build_fleet
from build_fleet import validate_binary


def elf(machine=62):
    header = bytearray(64)
    header[:6] = b'\x7fELF\x02\x01'
    struct.pack_into('<H', header, 18, machine)
    return header


def pe():
    header = bytearray(134)
    header[:2] = b'MZ'
    struct.pack_into('<I', header, 60, 128)
    header[128:134] = b'PE\0\0\x64\x86'
    return header


def macho(cpu=0x0100000C):
    header = bytearray(64)
    header[:4] = b'\xcf\xfa\xed\xfe'
    struct.pack_into('<I', header, 4, cpu)
    return header


class BinaryTargetTests(unittest.TestCase):
    def check_header(self, payload, target, valid):
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / 'hs-pi-dashboard-linux-amd64'
            path.write_bytes(payload)
            if valid:
                validate_binary(path, target)
            else:
                with self.assertRaises(ValueError):
                    validate_binary(path, target)

    def test_filename_cannot_disguise_windows_binary_as_linux(self):
        self.check_header(pe(), 'linux-amd64', False)

    def test_linux_requires_x86_64(self):
        self.check_header(elf(183), 'linux-amd64', False)

    def test_macos_requires_arm64(self):
        self.check_header(macho(0x01000007), 'darwin-arm64', False)

    def test_rejects_truncated_and_unknown_headers(self):
        for payload in [b'', b'MZ', b'\x7fELF', bytes(64)]:
            for target in ['linux-amd64', 'windows-amd64', 'darwin-arm64']:
                with self.subTest(target=target, payload=payload[:4]):
                    self.check_header(payload, target, False)

    def test_accepts_matching_formats(self):
        for payload, target in [(elf(), 'linux-amd64'), (pe(), 'windows-amd64'), (macho(), 'darwin-arm64')]:
            with self.subTest(target=target):
                self.check_header(payload, target, True)


class ToolchainTests(unittest.TestCase):
    def result(self, version):
        return subprocess.CompletedProcess(['go', 'version'], 0,
                                           stdout=f'go version {version} darwin/arm64\n')

    def test_minimum_comes_from_go_mod(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            (root / 'go.mod').write_text('module fixture\n\ngo 1.99.8 // minimum\n')
            with mock.patch.object(build_fleet, 'ROOT', root):
                self.assertEqual(build_fleet.minimum_go_version(), (1, 99, 8))
            (root / 'go.mod').write_text('module fixture\n\ngo 1.99\n')
            with mock.patch.object(build_fleet, 'ROOT', root):
                with self.assertRaisesRegex(ValueError, 'patch-level'):
                    build_fleet.minimum_go_version()

    @mock.patch.object(build_fleet, 'minimum_go_version', return_value=(1, 27, 3))
    def test_accepts_minimum_and_newer_stable(self, _minimum):
        for version in ['go1.27.3', 'go1.27.4', 'go1.28.0']:
            with self.subTest(version=version):
                with mock.patch.object(build_fleet.subprocess, 'run', return_value=self.result(version)):
                    build_fleet.require_toolchain({'GOTOOLCHAIN': 'local'})

    @mock.patch.object(build_fleet, 'minimum_go_version', return_value=(1, 27, 3))
    def test_rejects_old_unstable_or_unrecognized_versions(self, _minimum):
        for version in ['go1.26.10', 'go1.27.2', 'go1.27', 'go1.28rc1',
                        'go1.27.3rc1', 'devel go1.28-abc', 'unknown']:
            with self.subTest(version=version):
                with mock.patch.object(build_fleet.subprocess, 'run', return_value=self.result(version)):
                    with self.assertRaisesRegex(ValueError, 'requires Go 1.27.3|required Go 1.27.3'):
                        build_fleet.require_toolchain({'GOTOOLCHAIN': 'local'})

    @mock.patch.object(build_fleet, 'minimum_go_version', return_value=(1, 27, 3))
    def test_failed_or_hung_version_query_is_refused(self, _minimum):
        for error in [FileNotFoundError(), subprocess.TimeoutExpired('go', 15),
                      subprocess.CalledProcessError(1, ['go', 'version'])]:
            with self.subTest(error=error):
                with mock.patch.object(build_fleet.subprocess, 'run', side_effect=error):
                    with self.assertRaisesRegex(ValueError, 'requires Go 1.27.3'):
                        build_fleet.require_toolchain({'GOTOOLCHAIN': 'local'})

    @mock.patch.object(build_fleet, 'minimum_go_version', return_value=(1, 27, 3))
    def test_old_toolchain_never_builds_or_replaces_existing_artifact(self, _minimum):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            artifact = root / 'hs-pi-dashboard-linux-amd64'
            artifact.write_bytes(b'existing artifact')
            with mock.patch.dict(os.environ, {'GOTOOLCHAIN': 'auto'}):
                with mock.patch.object(build_fleet.subprocess, 'run', return_value=self.result('go1.27.2')) as run:
                    with self.assertRaisesRegex(ValueError, 'older than required Go 1.27.3'):
                        build_fleet.build('linux-amd64', root)
            self.assertEqual(run.call_count, 1)
            self.assertEqual(run.call_args.args[0], ['go', 'version'])
            self.assertEqual(run.call_args.kwargs['env']['GOTOOLCHAIN'], 'local')
            self.assertEqual(artifact.read_bytes(), b'existing artifact')
            self.assertFalse(artifact.with_suffix('.building').exists())

    @mock.patch.object(build_fleet, 'minimum_go_version', return_value=(1, 27, 3))
    def test_patched_toolchain_builds_validates_and_replaces(self, _minimum):
        def command(args, **kwargs):
            self.assertEqual(kwargs['env']['GOTOOLCHAIN'], 'local')
            if args[1] == 'version':
                return self.result('go1.27.3')
            self.assertEqual(args[:3], ['go', 'build', '-o'])
            pathlib.Path(args[3]).write_bytes(elf())
            return subprocess.CompletedProcess(args, 0)
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            with mock.patch.object(build_fleet.subprocess, 'run', side_effect=command) as run:
                artifact = build_fleet.build('linux-amd64', root)
            self.assertEqual(run.call_count, 2)
            self.assertEqual(artifact.read_bytes(), elf())
            self.assertFalse(artifact.with_suffix('.building').exists())


if __name__ == '__main__':
    unittest.main()
