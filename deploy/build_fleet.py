"""Cross-compile and validate fleet binaries before deployment."""
import argparse
import os
import re
from pathlib import Path
import struct
import subprocess

TARGETS = {'windows-amd64': ('windows', 'amd64'),
           'linux-amd64': ('linux', 'amd64'),
           'darwin-arm64': ('darwin', 'arm64')}


ROOT = Path(__file__).resolve().parent.parent


def minimum_go_version():
    """The go.mod directive is the single CI and fleet toolchain policy."""
    directive = re.search(r'^go[ \t]+(\d+)\.(\d+)\.(\d+)[ \t]*(?://.*)?$',
                          (ROOT / 'go.mod').read_text(), re.MULTILINE)
    if not directive:
        raise ValueError('go.mod must declare a patch-level Go version')
    return tuple(map(int, directive.groups()))


def require_toolchain(env, startup=None, flags=0):
    minimum = minimum_go_version()
    required = '.'.join(map(str, minimum))
    try:
        result = subprocess.run(['go', 'version'], cwd=ROOT, env=env,
                                startupinfo=startup, creationflags=flags,
                                capture_output=True, text=True, check=True,
                                timeout=15)
    except (OSError, subprocess.SubprocessError) as error:
        raise ValueError(f'Could not verify local Go; requires Go {required} or newer') from error
    version = re.match(r'^go version go(\d+)\.(\d+)(?:\.(\d+))? ', result.stdout)
    if not version:
        raise ValueError(f'Unrecognized stable Go version; requires Go {required} or newer')
    actual = tuple(int(part or 0) for part in version.groups())
    if actual < minimum:
        found = '.'.join(map(str, actual))
        raise ValueError(f'Go {found} is older than required Go {required}; refusing fleet build')
    print(result.stdout.strip())


def validate_binary(path, target):
    with open(path, 'rb') as binary:
        header = binary.read(64)
        valid = False
        if target == 'linux-amd64':
            valid = (len(header) == 64 and header[:6] == b'\x7fELF\x02\x01'
                     and struct.unpack_from('<H', header, 18)[0] == 62)
        elif target == 'darwin-arm64':
            valid = (len(header) == 64 and header[:4] == b'\xcf\xfa\xed\xfe'
                     and struct.unpack_from('<I', header, 4)[0] == 0x0100000C)
        elif target == 'windows-amd64':
            if len(header) == 64 and header[:2] == b'MZ':
                binary.seek(struct.unpack_from('<I', header, 60)[0])
                pe = binary.read(6)
                valid = pe == b'PE\0\0\x64\x86'
        if not valid:
            raise ValueError(f'{path}: binary does not match {target}; refusing deployment')


def build(target, output_dir):
    goos, goarch = TARGETS[target]
    destination = output_dir / f'hs-pi-dashboard-{target}{".exe" if goos == "windows" else ""}'
    temporary = destination.with_suffix(destination.suffix + '.building')
    env = dict(os.environ, GOOS=goos, GOARCH=goarch, CGO_ENABLED='0', GOTOOLCHAIN='local')
    startup = None
    flags = 0
    if os.name == 'nt':
        startup = subprocess.STARTUPINFO()
        startup.dwFlags |= subprocess.STARTF_USESHOWWINDOW
        startup.wShowWindow = 0
        flags = subprocess.CREATE_NO_WINDOW
    require_toolchain(env, startup, flags)
    try:
        subprocess.run(['go', 'build', '-o', str(temporary), '.'],
                       cwd=ROOT, env=env,
                       startupinfo=startup, creationflags=flags,
                       check=True, timeout=180)
        validate_binary(temporary, target)
        temporary.replace(destination)
    finally:
        temporary.unlink(missing_ok=True)
    print(f'Validated {target}: {destination}')
    return destination


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--target', choices=['all', *TARGETS], default='all')
    args = parser.parse_args()
    output = Path(__file__).resolve().parent.parent / 'dist'
    output.mkdir(exist_ok=True)
    try:
        for selected in TARGETS if args.target == 'all' else [args.target]:
            build(selected, output)
    except (ValueError, OSError, subprocess.SubprocessError) as error:
        parser.exit(1, f'build_fleet: {error}\n')
