# Mutation testing

Mutation testing changes one comparison, operator or sign in production code
and runs the tests against that change. If the tests still pass, the mutant
*lived*: no test notices that change. The job exists because the checks that
decide whether a peer's reading is trusted, when the agent refreshes and when T3
data counts as stale can be flipped without failing any coverage or CRAP gate
(#92).

## Tool and command

The job runs [gremlins](https://github.com/go-gremlins/gremlins) v0.6.0,
pinned with its module sum in
[scripts/mutation-gate.json](../scripts/mutation-gate.json), through
[scripts/mutation.py](../scripts/mutation.py). The runner refuses any other
gremlins build: it reads the binary's module version and sum with
`go version -m`.

Mutants run only inside the bubblewrap sandbox of
[scripts/mutation_sandbox.py](../scripts/mutation_sandbox.py) (see
[Sandbox](#sandbox)). It needs Linux, bubblewrap 0.8 or newer and unprivileged
user namespaces. The launcher copies the toolchain, the gremlins binary, the
go.sum modules and the repository into a new work folder; nothing of the
repository is built or tested on the host.

```bash
go install github.com/go-gremlins/gremlins/cmd/gremlins@v0.6.0
go mod download
mutation_work="$(mktemp -d)"
python3 scripts/mutation_sandbox.py --go-root "$(go env GOROOT)" \
  --gremlins "$(go env GOPATH)/bin/gremlins" --module-cache "$(go env GOMODCACHE)" \
  --work "$mutation_work/run" --report-dir mutation-report
# Only some in-scope packages:
python3 scripts/mutation_sandbox.py ... --packages ./internal/usage ./internal/agent
# Prove the sandbox and stop before any mutant:
python3 scripts/mutation_sandbox.py ... --probe-only
# Re-evaluate saved gremlins reports (<package>.json); runs no Go, no sandbox needed:
python3 scripts/mutation.py --from-reports mutation-report --report-dir /tmp/re-eval
# Test the gate and the launcher (no sandbox, no Go):
python3 -m unittest discover -s scripts -p "test_mutation*.py"
```

`--go-root` must be the Go version in `go.mod` (its `VERSION` file is checked),
and `--module-cache` must already hold the selected module graph from
`go mod download`. Historical unused go.sum entries need not be cached;
the sandbox verifies and downloads the selected graph from the staged local
proxy, with no route to the host or internet. On a shared host,
keep the run's memory bounded, for example under
`systemd-run --user --scope -p MemoryMax=4G -p MemorySwapMax=0 -p OOMPolicy=continue`.

Started directly, `scripts/mutation.py` refuses before any Go command (exit 2):
it looks for the sandbox's attestation file and nonce, its empty HOME, a network
with only `lo` and bwrap's init as PID 1. `--from-reports` alone works anywhere.

For each package in scope the runner calls `gremlins unleash --workers 2
--timeout-coefficient 10 --output <report> <package>`. gremlins first runs the
package's tests once with coverage, then applies one mutant at a time and runs
`go test -failfast <package>`: the package's real tests, with no `-run` filter,
so a mutant can't pass because no test ran. A failing run kills the mutant.

The runner pins everything that changes the result:

- the mutator set: gremlins v0.6.0's defaults (arithmetic base, conditionals
  boundary and negation, increment-decrement, invert negatives), passed as one
  explicit flag per mutator type, with an empty gremlins config file, so no
  `.gremlins.yaml`, `~/.config/gremlins` or `/etc/gremlins` applies;
- no `GREMLINS_*` environment overrides: the run stops if one is set;
- the environment: inside the sandbox, gremlins and every test run it starts
  get only an allowlist of variables, the sandbox's private module and build
  caches, and `HOME`, `XDG_*`, `APPDATA` and `LOCALAPPDATA` in an empty
  throwaway folder;
- no test cache: gremlins times each mutant against its coverage run, so the
  runner adds `-count=1` to `GOFLAGS`. A cached coverage run takes milliseconds,
  and in one trial it made 205 of 610 `internal/usage` mutants time out.

The runner stops with exit 2 when gremlins logs `failed to apply mutation` or
`failed to restore mutation` (for example a full temporary folder).

## Sandbox

A mutant can switch off a test's fixture and send code to its default
location. Negating `len(paths) == 0` in `internal/agent/agent.go` makes the
session handler use the default T3 database and the installed `t3` CLI, which on
a developer machine are the real ones. A throwaway HOME alone does not stop a
binary found on `PATH` or a service on the host's loopback, so every Go
command of a mutation run, including `go list` and the probe, runs in
bubblewrap with:

- new user, PID, network, IPC, UTS and cgroup namespaces; nested user
  namespaces disabled; no capabilities, `no_new_privs`, a new session, and the
  sandbox killed with its launcher;
- a read-only root with an explicit mount allowlist: the host's `/usr`
  read-only, `/usr/local` hidden, and host tools that can reach an account or
  another machine (`t3`, `gh`, `tailscale`, `ssh` and others) replaced by a stub
  that refuses to run; a copied Go toolchain and gremlins binary; a copy of the
  repository, read-only; a private module cache filled from a file proxy of the
  go.sum modules (checked against go.sum) and read-only during mutation; a
  private build cache, temp folder and empty HOME; synthetic `/etc/passwd`,
  `/etc/group` and `/etc/hosts`. The host root, the real HOME, `/run`, `/etc`
  and the host's `/proc` are never mounted;
- only the sandbox's own loopback: tests can serve fake HTTP fixtures, but no
  host listener, tailnet or internet address is reachable;
- an environment built from scratch. bwrap itself also starts with only
  `PATH`, because its init process stays visible as PID 1 inside the sandbox.

Before each step the launcher plants synthetic canaries on the host: a file
outside every mount and an open descriptor to it, a loopback TCP listener, a
Unix and an abstract socket, a process with a marker argument, a fake `t3`
pairing executable on the launcher's `PATH`, and a dummy credential in the
launcher's environment. It shows each one is live from the host, then runs
[scripts/mutation-sandbox-probe](../scripts/mutation-sandbox-probe) with
`go test` in the sandbox of the next step. The probe checks from inside that no
canary is reachable and that mounts, environment, processes, descriptors,
network, privileges and HOME are exactly as declared, and runs positive
controls: Go builds and runs a program, an in-sandbox HTTP fixture, a fake CLI
and temp files work, and gremlins is the pinned build. The launcher refuses the
step with exit 3 unless all 21 checks pass and the host saw no connection and no
run of the fake executable. It probes before `go mod download`, before the
mutation run and again after it, and keeps the canaries up throughout.
`mutation-report/sandbox/` holds every probe result and the launcher's evidence.

Exit codes: 0, 1 and 2 are the runner's; 3 means the sandbox could not be set
up or proven and no mutant ran (or, after the run, that a canary tripped).

Only committed HEAD is staged. Commit intended changes before running the
launcher; dirty, ignored and untracked files are excluded. The source commit
is recorded in `sandbox/evidence.json`. Source archive links and paths outside
the private staging directory are refused. A final proof failure marks the
summary UNPROVEN even when the mutation score itself passes.

## Scope

The scope covers every Go package of the module. Each package is either listed
in `packages` or listed in `exclusions` with a reason; a package in neither,
or in both, stops the run with exit 2, so a new package can't fall out of scope
unnoticed. A package in scope must have tests on the runner's platform.

| Package | In scope | Reason when excluded |
| --- | --- | --- |
| `internal/agent`, `fleet`, `githubcli`, `herdr`, `hermes`, `pmd`, `projects`, `resourcecheck`, `sessions`, `usage` | Yes | |
| `.` (`main.go`) | No | No tests; every mutant would be not covered. The CRAP gate tracks `main`. |
| `internal/backgroundcmd` | No | Its only test file is Windows-only, so on Linux every mutant would be not covered. |
| `internal/t3` | No | Runtime cost: 27 to 61 seconds per test run and 369 runnable mutants, an estimated 40 minutes to 3 hours more with 2 workers, against 12 minutes for the rest of the scope. |
| `internal/web` | No | Embedded assets with no Go logic; the Node renderer tests cover the scripts. |

The reasons are kept in the config and printed in every report.

`internal/agent` and `internal/fleet` include the steady-state checks of
[resource-growth.md](resource-growth.md) (300 agent and 1000 fleet cycles by
default), and every mutant of those packages runs them, since gremlins runs the
package's whole suite. They stay in at their default cycle counts: lowering the
counts would weaken the tests the mutants are measured against. Their cost
shows in the report's timing table (see [Measured results](#measured-results)).
`internal/resourcecheck`, which those checks are built on, is in scope with its
own tests.

## Statuses and thresholds

| Status | Meaning | In the efficacy denominator |
| --- | --- | --- |
| KILLED | A test failed with the mutant applied. | Yes |
| LIVED | Every test passed with the mutant applied. | Yes |
| TIMED OUT | The run took longer than 10 times the package's coverage run. | No |
| NOT VIABLE | The mutant does not compile. | No |
| NOT COVERED | No test of the package runs that line. | No |

Test efficacy is killed / (killed + lived), per package and in total. Mutant
coverage, (killed + lived) / (killed + lived + not covered), is reported beside
it. Mutants are counted only against their own package's tests, as gremlins
measures coverage per package.

The config declares a **break threshold of 83.0%** and a **target of 90%**
total efficacy. The job fails below the break threshold; a total at the break
value passes. The threshold applies to the total of whatever packages ran, so a
`--packages` run of weaker packages can fail it although the full scope passes;
required kills and timeouts are judged the same way in both. The break value sits under the 83.63% that the full scope
measured when the gate was added (see below), leaving room for mutants that
alternate between killed and timed out on a busy runner. The target is the level
to work toward; it doesn't fail the job. Raise the break threshold as sustained test improvements establish a higher
floor; do not lower it to make a failing run pass.

Timed-out mutants leave the denominator, so many of them could hide survivors.
The job also fails when more than 10% (`max_timed_out_share`) of a package's
killed, lived and timed-out mutants timed out. That points at a slow or
overloaded host, or at mutants that make tests hang; rerun on a quieter runner
or find the hanging tests before trusting the numbers.

### Required kills

`required_kills` lists lines where every mutant must be KILLED (NOT VIABLE is
also accepted). A survivor, a timeout or a not-covered mutant on one of those
lines fails the job, whatever the total. Each entry names the file and a text
the line contains, so the guard follows the line when code above it moves. The
run stops with exit 2 if that text matches no line or several lines, or if
gremlins produced no mutant on the line.

| Line | Check |
| --- | --- |
| `internal/usage/claude_peer.go` `len(body) > claudePeerMaxBody` | Peer response size limit |
| `internal/usage/claude_peer.go` `len(g.Label) > 128` | Peer gauge label length |
| `internal/usage/claude_peer.go` `*g.UsedPercent < 0 \|\| *g.UsedPercent > 100` | Peer gauge percent range |
| `internal/usage/claude_peer.go` `*g.ProjectedEnd < 0` | Peer gauge projection sign |
| `internal/usage/claude_peer.go` `g.WindowSeconds >= 0 && g.WindowSeconds <= claudePeerMaxWindow` | Peer gauge window range |
| `internal/agent/session_cache.go` `now.Sub(c.completed) >= sessionInterval` | Session refresh interval |
| `internal/agent/session_cache.go` `now.Sub(source.LastSuccessAt) > 5*time.Minute` | Five-minute T3 source expiry |
| `internal/sessions/claude.go` lines 69 to 71 | Claude transcript entry filters (meta, role, UUID or message id) |

The tests that kill them pin each limit on both sides of its edge: a peer body
of exactly the limit is accepted and one byte more is refused, a 128-byte label
passes and 129 bytes (also as 43 three-byte runes) fail, and so on for the
percent, projection and window limits; a refresh starts exactly at the interval
and not a nanosecond before; T3 data is fresh at exactly five minutes and stale
one nanosecond later; and every excluded transcript record would change the
prompt, the activity span, the message count or the output if it were accepted.

## CI

[.github/workflows/mutation.yml](../.github/workflows/mutation.yml) runs on
pull requests that change Go code, `go.mod`, `go.sum` or the mutation setup,
every Monday at 09:17 UTC against `main`, and on demand. It is not part of the
required `verify` check. On the ephemeral `ubuntu-22.04` runner it builds Bubblewrap 0.11.1 from
[upstream commit `124c4cdf`](https://github.com/containers/bubblewrap/commit/124c4cdf4321f63ef17a1cb0ce8f9dd45bd7adbe)
with a checked archive SHA256 and no setuid installation. It installs pinned
gremlins and the selected public Go modules, runs
the gate and launcher unit tests, then the launcher, which proves the sandbox
before any repository code runs. If the runner cannot create the sandbox (for
example namespace creation denied by runner policy), the job fails with
exit 3 and runs no mutant; it never falls back to running unconfined or changes host security policy.
The first Ubuntu 24.04 qualification failed before any mutant at isolated
loopback setup (`RTM_NEWADDR: Operation not permitted`); Ubuntu 22.04 is
qualified by the same probes rather than by assuming what that error means.
It writes
the summary table, failures, the full survivor list and the sandbox verdict to
the job summary and uploads the `mutation-report` artifact for 30 days.

## Reading the report

`mutation-report/` holds:

- `summary.md`: verdict, per-package counts, efficacy and mutant coverage,
  exclusions, required kills and any failures;
- `survivors.md`: every surviving mutant and every timed-out mutant as
  `file:line:column`, the mutator and the source line;
- `summary.json`: the same data for scripts, with each package's gremlins wall
  time and first test run;
- `<package>.json` and `<package>.log`: gremlins' own report and log;
- `sandbox/`: each probe's checks (`probe-*.json`), the bwrap arguments and
  logs of every step, and `evidence.json` with the host-side canary controls
  and post-checks.

A surviving mutant names a comparison or operator that the tests do not pin.
Add a test that fails when that change is made (check it by making the change by
hand), or, if the mutant behaves exactly like the original, say why in the pull
request. Mutator names follow gremlins: CONDITIONALS_BOUNDARY turns `>` into
`>=`, CONDITIONALS_NEGATION turns `>` into `<=` and `==` into `!=`,
ARITHMETIC_BASE swaps `+`/`-` and `*`/`/`, INCREMENT_DECREMENT swaps `++`/`--`
and INVERT_NEGATIVES drops a unary minus.

## Measured results

### Initial full-scope measurement

Before the additional projection and subscription-price boundary tests,
measured on 2026-11-06 inside the sandbox on Linux amd64 (kernel 7.0,
bubblewrap 0.11.1, 12 cores, shared host), with Go 1.27.3, gremlins v0.6.0 and
2 workers, against `main` at `82e3b72` plus the #92 tests. All three probes
passed and the host canaries saw no connection and no `t3` run. The mutation
step took 700 seconds; staging and the probes add under a minute.

| Package | Killed | Lived | Timed out | Not covered | Efficacy | gremlins run | First test run |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| `internal/agent` | 11 | 7 | 0 | 5 | 61.11% | 106 s | 18.8 s |
| `internal/fleet` | 52 | 6 | 0 | 3 | 89.66% | 48 s | 5.6 s |
| `internal/githubcli` | 1 | 0 | 0 | 3 | 100.00% | 0.4 s | 0.2 s |
| `internal/herdr` | 13 | 2 | 0 | 7 | 86.67% | 2 s | 0.2 s |
| `internal/hermes` | 42 | 15 | 0 | 9 | 73.68% | 30 s | 1.1 s |
| `internal/pmd` | 44 | 6 | 0 | 13 | 88.00% | 12 s | 0.4 s |
| `internal/projects` | 65 | 12 | 2 | 4 | 84.42% | 39 s | 1.5 s |
| `internal/resourcecheck` | 46 | 14 | 2 | 9 | 76.67% | 67 s | 1.8 s |
| `internal/sessions` | 129 | 16 | 1 | 7 | 88.97% | 21 s | 0.3 s |
| `internal/usage` | 501 | 104 | 5 | 89 | 82.81% | 376 s | 1.6 s |
| **Total** | 904 | 182 | 10 | 149 | 83.24% | 700 s | |

Every required-kill mutant was KILLED. No package came near the 10% timeout
limit (the highest, `internal/resourcecheck`, was 3.2%). The nine packages
measured before `internal/resourcecheck` existed give exactly the counts of the
earlier throwaway-HOME run (858 killed, 168 lived, 8 timed out, 83.63%);
`internal/resourcecheck` brings the total to 83.24%, still above the 83.0% break
threshold, which stays where it was set. The initial `internal/usage` plus `internal/agent` run was 82.18%, below
the threshold despite passing required kills. That did not satisfy #92's
focused validation criterion, so projection and subscription-price tests were
added; the new focused result is below.

The steady-state checks make `internal/agent` the slowest package per mutant
(an 18.8-second first run, so a 188-second deadline per mutant), but it has only
23 runnable mutants. `internal/usage` dominates the run by mutant count. The
first-run time varies between runs on a shared host (`internal/usage` measured
10.9 seconds in an earlier run of the same code), which moves the deadlines but
did not change any status here.

`internal/agent/agent.go:115` (`if len(paths) == 0`, negated) lives in the
sandbox: with no T3 database, `t3` binary or host network to fall back to, no
test notices the change. That is the honest result for a run that cannot reach
the host; a test that pins the fallback without the host would kill it.

Before the #92 tests, `internal/usage`, `internal/agent` and `internal/sessions`
measured 626 killed and 141 lived (81.62%), with 15 survivors on the
required-kill lines. The new tests kill all of them.

The audit sample in #92 reported `internal/hermes` at 33 killed, 0 lived and 24
timed out (100%). Its code is unchanged since that revision; with test caching
off it measures 42 killed, 15 lived and none timed out, so 15 of those 24
timeouts were survivors.

### Focused validation after boundary-test improvements

A fresh confined run at source `fee834a8` (the same Go code and tests as the
subsequent launcher/workflow repairs) measured:

| Package | Killed | Lived | Timed out | Not covered | Efficacy |
| --- | ---: | ---: | ---: | ---: | ---: |
| `internal/usage` | 529 | 81 | 5 | 84 | 86.72% |
| `internal/agent` | 11 | 7 | 0 | 5 | 61.11% |
| **Total** | **540** | **88** | **5** | **89** | **85.99%** |

The focused command exits 0 above the unchanged 83.0% break threshold.
Every required kill in those packages was KILLED. All three 21-check probes
passed, with zero host-canary connections or fake `t3` executions. The mutation
step took 720 seconds. The added tests pin Monday reset tolerance, UTC
conversion, the two-minute projection minimum, and finite positive current and
next-period subscription prices. Hosted CI publishes the current full-scope
measurement; these local results do not substitute for that qualification.
