# Rate-limited producer fixture

gh-x-v0.23.2-rate-limited.json was captured on October 23, 2026 at 12:41 AM EDT
from the installed gh-x v0.23.2 executable, SHA256 below. It is producer output,
not a hand-built schema. No live credentials or requests were used.

A temporary Git checkout with an empty fixture commit used origin
https://github.com/hemsoft-dev/hs-pi-dashboard.git. HOME, XDG_CACHE_HOME and
XDG_CONFIG_HOME pointed into the temporary fixture directory. PATH placed a
stub gh before /usr/bin:/bin. The stub exited 1 with a GitHub rate-limit
diagnostic. Invoking the installed gh-x executable with status --json returned
exit 0 and empty stderr, with failures in all four section reasons.

The retained stdout is unmodified, including temporary paths and producer
timestamps. Tests inject generatedAt as their clock; the captured reset
deadline is thereby reproducible. Existing gh-x-v0.22.0.json remains the
successful real-producer contract fixture.
8cd9f4100a12534f5a10cef1620f7265de6d4510bc350ab1e242257a80832ceb  /Users/home/.local/share/gh/extensions/gh-x/gh-x
