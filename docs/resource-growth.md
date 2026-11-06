# Resource growth checks

The agent and the fleet server run for days. A collector that keeps a
connection, a goroutine, a child process or a cache entry per cycle would only
show up after hours on the always-on machines. These tests run hundreds of
collection cycles against fakes and fail when a resource has not returned to
its warm-up baseline (issue #91).

| Test | Package | One cycle |
| --- | --- | --- |
| `TestResourceGrowthAgentCollectionCycles` | `internal/agent` | A usage refresh through `usage.Store` (Codex, Claude and Moonshot over HTTP to a fake provider, Antigravity by running a fake `agy`), a session refresh through `sessionCache` (Pi and Claude Code scanners, the T3 collector reading a fixture V2 SQLite database), then `GET /usage` and `GET /sessions` over a real listener |
| `TestResourceGrowthFleetPolls` | `internal/fleet` | `Server.poll` and `Server.pollUsage` against four fake agents: two healthy, one legacy (404 on `/herdr` and `/projects`), one answering 503 |
| `TestResourceGrowth*DetectsRetainedResources` | both | The same harness with one deliberate leak per measured cycle; the check must report it |
| `internal/resourcecheck` tests | `internal/resourcecheck` | The readers themselves: each must see a retained goroutine, file, heap block and child |

Every cycle runs against fakes only. HOME points at a temporary folder with
synthetic credentials, PATH is empty, provider settings point at a local
server, and the agent package's `TestMain` already replaces the Windows
Credential Manager reader. No test reaches a real provider, vault, CLI or
personal file. The fake `agy` is the test binary itself, switched by an
environment variable in `init`.

## Method

`resourcecheck.Plan.Run` does four things:

1. **Warm-up.** Blocks of warm-up cycles fill caches, keep-alive pools and
   the T3 collector's state. While a block still adds OS threads or handles,
   another block runs, up to a cap (agent: 30-cycle blocks, 300 cycles at
   most; fleet: 300-cycle blocks, 3000 at most). The Go runtime keeps every
   OS thread it creates, and Windows keeps per-process socket state, so both
   fill during the first bursts. A leak never stops growing, so it reaches
   the cap and still shows in the measured cycles.
2. **Baseline.** Five readings 50 ms apart; each resource keeps its lowest
   value, so a connection still closing from warm-up cannot raise it.
3. **Measured cycles.** 300 agent cycles or 1000 fleet cycles by default.
4. **Settle.** Readings every 50 ms for up to 3 s until every resource is
   within bounds. That covers teardown already under way (a closed
   connection's goroutines exiting, a reaped child disappearing). A retained
   resource never comes back and is reported at the deadline.

Each reading forces two garbage collections first.

| Resource | Linux | Windows | macOS |
| --- | --- | --- | --- |
| Goroutines | `runtime.NumGoroutine` | same | same |
| OS threads | `threadcreate` profile count | same | same |
| Live heap | `HeapAlloc` after GC | same | same |
| Handles | entries in `/proc/self/fd` | `GetProcessHandleCount` | entries in `/dev/fd` |
| Child processes | `/proc/*/stat` with this parent, zombies included | Toolhelp snapshot of running children | not measured |

The package uses the standard library only.

## Bounds

The full reasoning sits beside each bound in the test files. In short:

| Resource | Agent | Fleet | Why |
| --- | --- | --- | --- |
| Goroutines | 8 | 8 | A cycle leaves only keep-alive connections, 3 goroutines each in this process. Two the baseline missed are 6, plus 2 for runtime and testing timers |
| OS threads | GOMAXPROCS + 12 | GOMAXPROCS + 4 | The runtime cannot pass GOMAXPROCS threads running Go plus those blocked in system calls (agent: 12 at once; fleet: one per agent). A thread only leaks by stranding its goroutine, which the goroutine bound catches |
| Live heap | 1 MiB | 1 MiB | Both keep one snapshot per source, fixed-size for fixed fakes. A 4 KiB retention per cycle is 1.2 MiB over 300 cycles |
| Handles | 8 + 8 per new thread on Windows | same | Two connections are 4 descriptors on Linux. On Windows each Go thread holds about 6 handles, 8 once it has used a socket (measured; `TestHandlesPerThread` rechecks it), so the threads' own handles are credited |
| Children | 0 | 0 | `agy` is waited for inside the cycle; the server starts no processes |

A leak of one unit per cycle exceeds these bounds about 37 times over in the
agent test (300 goroutines against 8) and more in the fleet test. A leak
slower than one unit per 37 agent cycles can stay within bounds; raise
`HS_PI_DASHBOARD_GROWTH_CYCLES` to look for one.

## Evidence

Measured on 2026-11-06 at `origin/main` 3a4dff7 plus this change, Go 1.27.3.
Home is Windows 11 (32 logical CPUs), Mini is Linux amd64 (12).

| Run | Agent: growth over 300 cycles | Fleet: growth over 1000 cycles |
| --- | --- | --- |
| Mini, 5 fresh processes | goroutines 0, FDs 0, children 0, heap +101 to +150 KiB, threads +0 to +4 | goroutines 0, FDs 0, heap 0 to +57 KiB, threads 0 |
| Mini, 3000 cycles | heap +192 KiB, threads +4, goroutines, FDs and children 0 | heap +35 KiB, other resources 0 |
| Mini, CI form (`-race -p 1`, coverage) | passes; package run 24 s | passes; package run 14 s |
| Home, 5 fresh processes | goroutines 0, children 0, heap +131 to +245 KiB, threads +0 to +2, handles exactly 8 per new thread | goroutines 0, heap +67 to +115 KiB, threads +0 to +3, handles exactly 8 per new thread |
| Home, 3000 cycles | heap +357 KiB, threads +3, handles +24, goroutines and children 0 | heap +110 KiB, threads +3, handles +24, goroutines 0 |
| Home, CI form (coverage) | passes; package run 22 s | passes; package run 9 s |

Heap growth flattens instead of scaling with cycles: ten times the cycles
added under three times the heap (Mini +192 KiB at 3000 against +101 to +150 at
300; Home +357 KiB against +131 to +245). A heap profile diff between cycle 300 and 2300
on Home came to 137 KB, most of it runtime goroutine descriptors (which Go
never frees) and the profiler's own tables. No production function grew.

### Deliberate leaks

Each control adds one leak per measured cycle (12 cycles) under the same
bounds. All are reported on both platforms:

| Control | Reported as |
| --- | --- |
| A fake collector that never closes its provider or agent response bodies | goroutines and handles |
| A goroutine blocked forever | goroutines |
| 256 KiB kept per cycle | live heap |
| A child process never waited for | child processes and handles (Linux and Windows) |

### Leaks in production code

Each mutant edits one production file in a scratch copy and runs the clean
test unchanged:

| Mutant | Linux | Windows |
| --- | --- | --- |
| `codex.go`: no `resp.Body.Close()` | fails: +300 goroutines | fails: +300 goroutines |
| `antigravity_cli.go`: `cmd.Start()` without `Wait` | fails: +300 goroutines, +300 children | fails: +300 goroutines (Windows lists only running children, and Go's finalizers close the process handles) |
| `herdr.go`: no `resp.Body.Close()` | passes (equivalent, see below) | passes |
| `herdr.go`: no `Close` and no timeout context | fails: +6000 goroutines, +4000 FDs, +48 MB heap | fails: +6000 goroutines, +4008 handles, +49 MB heap |
| `server.go`: a goroutine per usage poll blocked on a send | fails: +4000 goroutines, +2.8 MB heap | fails: +4000 goroutines, +2.8 MB heap |

The surviving mutant is not a leak. `fetchHerdr` cancels its per-request
context on return, and the transport then closes the connection, so a
missing `Close` alone releases everything.

The Codex mutant is stricter than production would need. Its retained
goroutines are `http.Client` timeout timers, and the Fetcher's 20-second
client timeout releases them (measured: 113 goroutines at the last cycle, 13
after 22 seconds). The test runs cycles far faster than the agent's 60-second
loop, so they outlive the 3-second settle. A missing `Close` is a defect
either way.

## Running it

```sh
go test ./internal/resourcecheck ./internal/agent ./internal/fleet \
  -run 'ResourceGrowth|Settle|Measure|Violations|HandlesPerThread|PlanRun|PlanWarmup|ParentPID' -count=1 -v
```

The verbose output prints each test's warm-up length, baseline and final
readings. `HS_PI_DASHBOARD_GROWTH_CYCLES` raises the measured cycles; it
must be at least 1000 because the fleet test uses that by default.

## Where it runs

- **Every pull request and push to `main`:** CI's `go test ./...` runs these
  tests on Linux (with the race detector), Windows and macOS, so `verify`
  fails when one does.
- **[resource-growth.yml](../.github/workflows/resource-growth.yml):** the same tests on
  their own, verbose, with the readings in the job summary. It runs on pull
  requests that touch `internal/`, `go.mod` or `go.sum`, on pushes to
  `main`, weekly on Mondays at 09:17 UTC with 3000 cycles, and on demand
  with a cycle count.

## Limits

- Fakes, not providers: real provider latency, TLS and proxy paths are not
  exercised. The fake provider is plain HTTP on loopback.
- The agent test composes the session cache from the same sources as
  `sessionHandler`, because production's cache refreshes at most every 5 s
  of wall time. It does not run `agent.Run`, the Herdr collector, the
  project store or the pmd push.
- `muse` caches for 5 minutes and is not driven per cycle. GitHub billing
  (`gh`) and Windows PowerShell credential reads are off.
- The fleet test does not call `/api/fleet`: that handler also reads Hermes
  sessions through `hermes.Default()`, which binds to the home directory of
  its first caller in the process and could read a real `~/.hermes`.
- macOS does not count child processes, and the Windows count sees only
  running children: an exited child whose handle is still open shows as
  handle growth instead.
- Windows handle growth is credited at 8 per new OS thread, so a handle leak
  of fewer than 8 per new thread during the same run would be hidden. Home's
  fleet runs still added 1 to 3 threads during the measured cycles, so up to
  about 24 handles can go unseen there.
