# hs-pi-dashboard

[![CI](https://github.com/hemsoft-dev/hs-pi-dashboard/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/hemsoft-dev/hs-pi-dashboard/actions/workflows/ci.yml)

A gold-on-black, fleet-wide dashboard for [pi](https://github.com/badlogic/pi-mono)
coding-agent sessions across the Tailscale network, including Claude Code and
[T3 Code](https://github.com/pingdotgg/t3code).
Its project icon is `assets/icon.svg`, registered for T3 Code in `t3.json` and
inlined as the dashboard's browser favicon.
One Go binary, two modes:

```text
┌─ laptop ─┐   ┌─ home ──┐   ┌─ air / grokbot ────┐
│ agent    │   │ agent   │   │ agent (later)      │  watches ~/.pi/agent/sessions
│ :8787    │   │ :8787   │   │ :8787              │  GET /sessions → JSON summaries
└────┬─────┘   └────┬────┘   └─────────┬──────────┘
     └──────────────┼──────────────────┘
        polled every 5s over Tailscale/MagicDNS
          ┌──────────▼──────────────────┐
          │ hs-pi-dashboard serve :8788 │  on mini (always-on)
          └──────────▼──────────────────┘
        http://mini:8788 — machine → project → session cards
```

## What it shows

The live Herdr monitor shows agent states across machines and named Herdr
sessions, with machine and state filters. It starts collapsed; select **Live Herdr
agents** to show or hide all machine terminals, filters, and workspace states.
Pulse starts expanded unless you previously saved it collapsed. Each section
opens independently, and collection continues while Herdr is collapsed.
Collection is read-only and does not
send input, change focus, start a Herdr server, or read terminal transcripts.
Unavailable or stale collectors do not count as live. Agents running only in
tmux are not included in this monitor; session history and Pulse remain separate.

Each session title includes its originating application: **Pi**, **T3 Code**,
**Claude Code**, or **Hermes**. This label stays visible on mobile and is separate
from the computer running it and the provider/model powering it. Collectors
report application identity explicitly. For older agents, the server recognizes
T3/Claude ID namespaces and unnamespaced Pi sessions from the reported Pi
collector directory. Missing metadata or an unknown source shows **Unknown app**.

The session table shows each machine, project, provider, model, thinking level,
message count, cost, start time, and duration. Active sessions remain visible
before they produce assistant text and flash when their state changes;
completed transcript sessions without output stay hidden. T3 threads stay
visible even without output, with their thread title, repository and state. Rebooted machines keep their
last known sessions grayed out.

Above the session grid sits an expandable/collapsible **Usage & Balances**
section built as a runway board. Every quota window (Codex weekly, OpenCode Go
5-hour / weekly / monthly, Antigravity Gemini and third-party 5-hour + weekly)
is one row on a shared 0-125% scale with the plan limit marked at 100%. The
solid bar is measured usage; the outlined extension is the forecast at reset,
turning ember where it passes the limit, so over-limit forecasts line up down
the column. Each row shows percent used, the uncapped forecast, how long until
the window runs out at the current pace, and the reset countdown. Providers and
their windows sort by urgency, and the section header names the window that
runs out first, so the answer stays visible with the section collapsed.

OpenCode Zen and Moonshot AI prepaid balances sit in one line below the board.
Providers with no data collapse into a single "Not reporting" disclosure that
holds the raw collector errors. The agent refreshes usage every 60s
(`GET /usage`), and the server folds it into `/api/fleet`. The header shows one
update time; a provider is labeled only when its data is older than three
minutes or when a failed refresh left its last successful reading in place.
Missing values read "no reading" rather than zero. Section and provider
collapse preferences persist, and the 5-second refresh keeps keyboard focus.

Use **Choose providers** below the section heading to show or hide each quota
or balance provider. Changes apply immediately to rows, warnings and the
collapsed summary. They are saved in this browser for this dashboard address;
other browsers and devices have their own choices. All five existing providers
start enabled and Claude starts disabled. To use Claude instead of Antigravity,
uncheck Antigravity and check Claude. The selector remains available when the
section is collapsed, agents are offline, or every provider is disabled.
These are display preferences; agents keep refreshing their cached readings.
A provider you switch on that no agent reports yet, such as Claude with older
agents, is listed under **Not reporting** until the agents are updated.

Claude reports subscription quotas from the read-only
`GET https://api.anthropic.com/api/oauth/usage` endpoint with the
`anthropic-beta: oauth-2025-04-20` header and a Claude Code `User-Agent` (some
valid sessions are refused otherwise), using an existing subscription session.
Credential precedence on each agent is `CLAUDE_CODE_OAUTH_TOKEN`, Claude Code's
`~/.claude/.credentials.json` (`claudeAiOauth.accessToken`), then Pi's
`~/.pi/agent/auth.json` (`anthropic.access`). `CLAUDE_CONFIG_DIR` replaces the
Claude Code directory and disables fallback to Pi, so a custom profile cannot
silently select another account. Tokens need usage/profile access; API keys and
inference-only setup tokens do not supply subscription usage. Sign in through
Claude Code or Pi as usual. The dashboard never starts login, reads the macOS
Keychain, renews tokens or writes credentials. An expired or inaccessible session
shows a setup/error message; renew it in its owning app. Service processes need
the same home directory or environment override as the signed-in account.

The collector supports five-hour, weekly, model-specific weekly and OAuth-app
windows, Routines weekly usage (including the `seven_day_cowork` alias), and
model-scoped `limits` entries. Unscoped `limits` session and weekly entries are
used only when the matching `five_hour` or `seven_day` window is absent. Missing or
invalid values remain unavailable, while valid sibling windows still report.
After a rate limit (HTTP 429) each agent waits for `Retry-After`, or five
minutes when absent, capped at one hour, before asking again. Structured
model-scoped limits replace the legacy Sonnet/Opus weekly windows for the same
model. Forecasts require a valid reset within the known window duration and at least
two minutes elapsed. Extra-usage spend and session token totals are excluded.
The schema and authentication headers were checked against
[CodexBar's OAuth fetcher](https://github.com/steipete/CodexBar/blob/59a7d46ac61338786e5d32d8324a3e414525e601/Sources/CodexBarCore/Providers/Claude/ClaudeOAuth/ClaudeOAuthUsageFetcher.swift)
and its
[scoped weekly parser](https://github.com/steipete/CodexBar/blob/59a7d46ac61338786e5d32d8324a3e414525e601/Sources/CodexBarCore/Providers/Claude/ClaudeWeb/ClaudeWebExtraRateWindowParser.swift).

The board is plain HTML and CSS embedded in the Go binary, with no chart
library. `PRODUCT.md` and `DESIGN.md` record the product context and the
gold-on-black design system for later UI work.

The expandable **Pulse** monitor counts recently active Pi, Claude Code, T3 and Hermes sessions
across the fleet. Pulse and the table use one fleet snapshot, and every counted
active session remains in the row payload even beyond the inactive-history cap.
Its green trace moves continuously to a live cursor at 75% of the monitor
width, resting at the current active-session level while zero stays on the
bottom baseline. The dot tracks the trace's rendered value and scale. Each
horizontal line is an integer session level from zero through five by default;
the grid adds denser integer levels when a count or beat needs more headroom.
When an active session row receives a meaningful update, the row flash and
cursor fire together with a short beat above that resting level.
Cached sessions from offline machines and stale Hermes rows without an end
timestamp do not count. The browser pauses the sweep
when Pulse is collapsed or the tab is hidden, and reduced-motion mode renders a
static trace.

The agent reads Pi and Claude Code session files without talking to either
running process. Claude Code sessions appear in the same table and Pulse count,
with `claude-code` in the provider column. No Claude hooks or plugin are needed.

Claude transcripts default to `~/.claude/projects`, or the `projects` directory
under `CLAUDE_CONFIG_DIR` when set. Override the path with `agent -claude-dir`.
The scanner keeps recent assistant text, model, effort when recorded, message
counts, and tokens including cache reads and writes. Repeated assistant content
blocks count as one API message, not separate responses. Subagent directories,
sidechain records, thinking, and tool payloads are excluded from displayed output.

Activity uses the same recent-write window as Pi, not process liveness. A quiet
session stops counting after two minutes by default. Claude transcript records
do not provide dollar costs, so their price cells stay blank. No subscription
balance is inferred. Future Pi streaming support would use an extension.

## T3 Code collection

Each agent reads `~/.t3/userdata/state.sqlite` in SQLite read-only mode and verifies
live states through T3's local `/api/orchestration/shell` endpoint. It collects
all non-deleted threads across every project in that environment, including closed
tabs and archived history. T3 rows remain in the payload beyond the transcript
history cap; the table paginates them. No T3 plugin, agent tool call or open tab is
required. The collector reads metadata, turn timing and provider counters without
reading prompt text or displaying tool payloads.

The collector supports both V1 `state.sqlite` and Orchestrator V2
`statev2.sqlite`. When the default environment upgrades, it switches to V2 and
keeps imported thread identities stable. V2 reads its current thread, run,
provider-turn and tool-node projections; the frozen legacy tables are never used
as a live fallback. All non-deleted threads are included, including delegated
child threads, archived history and threads with no assistant output. Independent
environments retain separate identities.

V2 live snapshots use the same local authenticated shell endpoint with
`x-t3-orchestration-protocol: 2`. Working, connecting, waiting and background
monitoring states come from that runtime, so quiet work stays active without
recent transcript writes. Persisted running metadata alone never counts as live.
Unknown schema or runtime states produce an unavailable source and recover on a
later poll. V2 shell responses allow up to 64 MiB of decompressed JSON, including
active and archived history; V1 keeps its 8 MiB limit. Larger responses are
unavailable rather than partially counted. A V2 run waiting for checkpoint
capture appears as working; only a pending approval or input request appears as
waiting for the user. Explicitly configure `state.sqlite` to monitor a separate
V1 instance;
an upgraded runtime cannot validate its frozen V1 copy as live.

V2 statistics use distinct tool nodes and the latest provider-turn counter
snapshot, with current provider-thread context usage. Unreported totals remain
`n/a`; zero remains zero. Only metadata and selected counter fields are read
from V2 JSON projections, excluding prompt text and tool inputs/outputs.
T3 V2 rows show the sidebar status separately from activity: **Working**, **Goal**,
**Waiting**, **Approval**, **Input**, **Limited**, **Failed**, **Woke**, or **Done**.
A ready, acknowledged thread has no badge. Working stays green. Approval and
input, including requests from live app-owned subagents, take precedence over
runtime work. As in upstream `threadShell.ts`, a live app-owned child's request
propagates through its ancestry even when an intermediate parent has settled;
ending the requesting child clears that propagated request. Usage-limit failures
show Limited until recovery. Done and Woke
use the server's visit and snooze metadata. Missing visited metadata never
invents an unread completion, and unavailable/offline sources override cached
labels. V1 and older dashboard agents retain their existing normalized labels.

The mapping follows T3 Code's current sidebar at
[`1451e7cb2303`](https://github.com/pingdotgg/t3code/blob/1451e7cb23034d29774338608368a15d8845df36/apps/web/src/components/Sidebar.logic.ts#L922)
and its shell presentation rules, rather than the older status-pill helper.
The fleet's local Waiting patch also treats background command tasks as waiting;
unpatched upstream only lets subagent, monitor and background-task kinds hold
completion. A checkpoint-only run remains Working. The agent exposes this
source-backed label in optional `t3Status`; an empty string means no badge.
The separate `status` and `active` fields continue to describe verified activity.
Label changes alone do not alter Pulse, so a Waiting badge on an idle runtime
need not count as live work.

Legacy activity states include connecting, working, monitoring, waiting, idle,
stopped and error. Pulse counts verified connecting, working, monitoring and
waiting threads. Quiet tools and background monitoring stay active regardless of
`-active-window`; that flag still controls native transcript activity. Open,
snoozed or unsettled threads are not active merely because they exist. The table
and Pulse use the same agent snapshot on each five-second dashboard refresh.
Duration uses the latest turn, not the lifetime of the thread.

The agent discovers each environment's loopback origin from the adjacent
`server-runtime.json`. It verifies the recorded process creation time and that
the process owns the listening port before sending any credential. Under the
same OS account, it runs the installed T3 CLI's
`auth pairing create --base-dir <environment> --ttl 1m --json`, then exchanges
that one-time credential at `/oauth/token`, requesting only `orchestration:read`.
When the CLI advertises `--scope`, the pairing grant itself also requests
`orchestration:read`. Older V1-compatible CLIs, including 0.0.45, have no scope
flag: their grant carries standard client scopes, expires after one minute,
is single-use, and is immediately narrowed at the verified local server.
The resulting bearer always has exactly `orchestration:read`.
It refuses broader bearer tokens, redirects and non-loopback origins. This supported
auth flow creates T3-managed access records; it does not modify thread projections
or restart T3. The session token stays in agent memory and is reused until expiry
or runtime restart. Renewal uses a new local pairing grant; an HTTP 401/403 clears
the token and retries on the next poll. No token belongs in service arguments,
environment variables, files, logs or dashboard payloads.

V2 requires a CLI compatible with the current server: an old V1 CLI can create
a pairing grant in the legacy database that the V2 server cannot exchange.
Install a matching T3 CLI and expose it on the agent's
PATH, or pass `-t3-bin /absolute/path/to/t3`. On Windows, use the native `t3.exe`,
not an npm `.cmd` or PowerShell wrapper. The agent also discovers the native
binary beside the standard npm launcher. That installation stores it
under `%APPDATA%\npm\node_modules\t3\node_modules\@t3code\t3-win32-x64\t3.exe`.
The environment base directory comes from the database path: for
`~/.t3-server/userdata/state.sqlite`, pairing uses `~/.t3-server`. Separate
configured environments get separate credentials and stable thread identities.
The supplied Air LaunchAgent sets an absolute `-t3-bin` path because launchd does
not inherit an interactive shell's PATH. Adjust that path for other installations.

Database and live requests share a 2.5-second polling deadline. Missing discovery,
a stopped runtime, failed authentication, timeouts or incompatible responses leave
T3 metadata visible as unavailable and clear its active count. Fresh or stale
persisted running metadata cannot keep a dead thread active. Collection retries
on each poll and recovers without duplicate rows. A compact T3 collection
disclosure keeps paths and failure details hidden until expanded. Actual
unavailable/offline sources retain a highlighted status in that summary.

T3 rows start collapsed. Click their title or use Tab followed by Enter/Space to
expand statistics and any stored assistant output, even when no output exists.
Expansion and diagnostic disclosure survive polling; row expansion also survives
pagination. Collapse the row to return to the compact list.

The expanded statistics line shows distinct tool calls and the latest reported input,
output, total, cache, reasoning and context counters. Repeated cumulative snapshots
replace previous counters rather than adding to them. `n/a` means the provider did
not report that metric; zero remains zero. Context usage is separate from total
processed tokens. Claude and Codex report different subsets, and these values are
provider telemetry, not billing totals. T3 costs are unavailable. A native Claude
transcript associated with a T3 provider session/thread is excluded so the session
and Pulse count once. V2 retains every linked Claude native identity across
provider switches, including switches to Codex. The newest linked native output
remains expandable on the app thread.
If T3 metadata is inactive or unavailable while the native transcript has fresh
writes, the row explicitly says "Claude transcript active" and Pulse counts that
independent activity once. Its T3 state remains visible, statistics are marked
cached, and unavailable turn timing is not inferred from transcript lifetime.

Configure each environment with a repeated flag, or disable collection explicitly:

```bash
./hs-pi-dashboard agent -addr <tailscale-ip>:8787 -t3-db /path/to/userdata/statev2.sqlite
./hs-pi-dashboard agent -addr <tailscale-ip>:8787 -t3-db /first/state.sqlite -t3-db /second/state.sqlite
./hs-pi-dashboard agent -addr <tailscale-ip>:8787 -t3-db ~/.t3-server/userdata/state.sqlite -t3-bin /path/to/t3
./hs-pi-dashboard agent -addr <tailscale-ip>:8787 -t3-db off
```

Missing, locked or incompatible databases produce a visible T3 collection warning
without breaking Pi collection. Cached metadata remains visible but never counts
as live. Collection retries on each poll and recovers when the database is readable
again. It never creates a missing database. Thread IDs include the environment path
so independent environments cannot collide. T3's SQLite projections are an internal
interface; schema changes may require a dashboard update.

For fleet rollout, build all OS targets from the same reviewed revision with
`python deploy/build_fleet.py`, then replace and restart **every dashboard agent**
and Mini's dashboard server with that build. Transfer macOS executables in a tar
archive, extract into a staging directory with a command-line tool, and verify
the executable SHA-256 before launch. Direct Taildrop executable downloads can
acquire quarantine metadata and stall before the program starts.
Configure database paths on the machine
that owns each T3 environment, under the user account that can read its userdata.
Check each agent's `/sessions` source health and the dashboard's `/api/fleet` payload.
The dashboard can be opened in T3's browser panel as an ordinary page. Merge and
fleet installation are separate from implementing this collector.

## Build

```powershell
go build ./...
go test ./...
```

Cross-compile for the fleet with explicit OS and architecture targets. The
builder checks each binary's format and architecture before replacing an
existing artifact. It also keeps Windows build processes hidden.

```powershell
python deploy/build_fleet.py
# Build one target:
python deploy/build_fleet.py --target linux-amd64
# Test the format checks, including a Windows binary mislabeled as Linux:
python -m unittest discover -s deploy -p test_build_fleet.py
```

## Run

### Agent (every machine with pi)

```powershell
# Windows (home, laptop) — bind the Tailscale IP so the agent stays off the LAN
.\hs-pi-dashboard.exe agent -addr 100.101.122.39:8787

# macOS/Linux
./hs-pi-dashboard agent -addr <tailscale-ip>:8787
```

Endpoints: `GET /sessions` (snapshot JSON), `GET /usage` (provider plan and
balance cards), `GET /herdr` (cached live agent states), `GET /health`.
Herdr is discovered from installed CLI locations, or can be specified with
`-herdr-bin`. Machines without Herdr report unavailable telemetry rather than
an empty healthy monitor.

### Server (mini)

```bash
./hs-pi-dashboard serve -addr 100.97.164.73:8788
```

Endpoints: `/` (dashboard), `/api/fleet` (aggregated JSON), `/healthz`.

Agents are configured with `-fleet name|url` pairs; the default covers all
five computers from the fleet map. An optional third field,
`name|url|https-terminal-url`, adds a link to that machine's browser terminal.
Terminal links require HTTPS and cannot embed credentials. Machines that don't
answer are shown offline with their last known sessions grayed out.

## Install

### home / laptop — Windows scheduled task (agent)

Run as the regular user (no admin needed for the task itself):

```powershell
schtasks /create /tn "hs-pi-dashboard-agent" /sc onlogon /rl limited /f `
  /tr '"C:\path\to\hs-pi-dashboard.exe" agent -addr 100.101.122.39:8787'
schtasks /run /tn "hs-pi-dashboard-agent"
```

If other machines can't reach port 8787, allow it inbound once (admin):

```powershell
netsh advfirewall firewall add rule name="hs-pi-dashboard agent" dir=in action=allow protocol=TCP localport=8787
```

### air — macOS LaunchAgent (agent)

Ship the arm64 binary via Taildrop from any tailnet machine, then set it up on
the Mac (the file arrives in ~/Downloads):

```powershell
tailscale file cp dist/hs-pi-dashboard-darwin-arm64 franzs-macbook-air:
```

```bash
mkdir -p ~/bin ~/logs
mv ~/Downloads/hs-pi-dashboard-darwin-arm64 ~/bin/hs-pi-dashboard
chmod +x ~/bin/hs-pi-dashboard
mkdir -p ~/Library/LaunchAgents
cp deploy/com.hemsoft.hs-pi-dashboard.agent.plist ~/Library/LaunchAgents/
launchctl load ~/Library/LaunchAgents/com.hemsoft.hs-pi-dashboard.agent.plist
```

RunAtLoad + KeepAlive start the agent at login and restart it after crashes or
a failed Tailscale bind, so reboots no longer leave the machine dark.

### mini — always-on server

Ship the Linux binary via Taildrop, then run it at boot:

```bash
mkdir -p ~/bin
mv ~/Downloads/from-home--hs-pi-dashboard-linux-amd64 ~/bin/hs-pi-dashboard
chmod +x ~/bin/hs-pi-dashboard
(crontab -l 2>/dev/null; echo '@reboot /home/franz/bin/hs-pi-dashboard serve -addr 100.97.164.73:8788 >> /home/franz/logs/hs-pi-dashboard.log 2>&1') | crontab -
# start it now, no reboot needed:
mkdir -p ~/logs && nohup ~/bin/hs-pi-dashboard serve -addr 100.97.164.73:8788 >> ~/logs/hs-pi-dashboard.log 2>&1 &
```

## Security model (v1)

No authentication by design: agents and the server bind to Tailscale
interface addresses, so only tailnet devices (including iphone/ipad) can
reach them. Add a shared token in v2 before any broader exposure.

## Roadmap

- [ ] v2: pi extension reporting live turn/tool events from running sessions
- [x] v1: usage runway board (Codex, OpenCode Go, Antigravity, Zen, Moonshot)
- [ ] v2: xAI usage once a source exists (card removed for now — Grok
      exposes no usage API)
- [ ] v2: optional bearer token between server and agents
- [ ] v2: systemd user unit for mini (with lingering) instead of cron
- [ ] v3: click-through session transcript, cost rollups per project
