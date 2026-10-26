# Muse Code allowance contract

Source: `muse usage --json`, pinned to Muse Code `1.4.13-R6996.1`
([changelog](https://dev.meta.ai/docs/muse-code/changelog)). The command is
non-inference. The reader in `../../muse.go` runs it with `META_API_KEY`
removed and `MUSE_LOGIN=0`, `MUSE_NO_AUTO_UPDATE=1`. It closes stdin, discards
stderr, caps stdout at 4 MiB and allows 15 s within the 20 s card budget. A
reading is reused for 5 minutes. Failures back off 5, 10, 20, then 30 minutes.

## Fixtures

`measured.json` is the real reading Chief took on mini at 2026-10-26 13:54 ET.
It contains no identity or credential. Every other file is synthetic and
derived from it:

| File | Proves |
| --- | --- |
| `measured.json` | both windows, tier, source, observation time |
| `null-allowance.json` | `allowance: null` is unavailable; synthetic `reason`/`hint` (with a fake email) never surface |
| `zero.json` | genuine 0% stays numeric 0 |
| `exhausted.json` | over-limit values cap at 100 for the bar; the forecast keeps the raw value |
| `partial.json` | a window missing `usedPercent` is dropped; its sibling survives |
| `malformed-values.json` | string percentages or resets are rejected, never coerced |
| `reset-beyond-window.json` | a weekly reset more than 7 days after `asOfMs` is rejected; the 5-hour window survives |
| `unsupported-schema.json` | `schemaVersion` other than 1 is unsupported |
| `extra-window.json` | an undocumented window is kept without duration or forecast; token totals and billing objects never become quota; markup in `tier` is dropped |
| `truncated.json` | malformed JSON |

Reset rollover, stale observation, a missing CLI, command failure, PAYG key
exclusion, caching and backoff are covered in `../../muse_test.go`. The UI
renders the same card shape in `scripts/usage.test.cjs`.

## Field mapping (agent `GET /usage` card)

| CLI field | Card field | Notes |
| --- | --- | --- |
| — | `key: "muse-code"`, `label: "Muse Code"`, `kind: "quota"` | stable provider identity |
| `allowance.window` | gauge `bucketId: "window"`, `label: "5h"`, `windowSeconds: 18000` | duration from the vendor's five-hour window; reset must be after `asOfMs` and at most 5 h (+60 s) later |
| `allowance.weekly` | gauge `bucketId: "weekly"`, `label: "Weekly"`, `windowSeconds: 604800` | reset at most 7 days (+60 s) after `asOfMs` |
| other `allowance.<key>` with `usedPercent` and `resetsAtMs` | gauge `bucketId`/`label: <key>`, no `windowSeconds`, no `projectedEnd` | key must match `^[A-Za-z0-9_-]{1,40}$` |
| `usedPercent` | `usedPercent` | a 0-100 percentage of the allowance used (observed 5 and 15); values above 100 cap at 100; negative or non-numeric drops the window |
| `resetsAtMs` | `resetAt` (RFC 3339 UTC) | must be after both `asOfMs` and the collection time |
| — | `projectedEnd` | `used / elapsed fraction`, anchored at `asOfMs`; omitted under 2 minutes elapsed |
| `asOfMs` | `observedAt` | provider observation time; `fetchedAt` is when the agent ran the CLI |
| `tier` | `plan` | kept only if it matches `^[A-Za-z0-9 ._()+-]{1,64}$` |
| `source` | `allowanceSource` | kept only if it matches `^[a-z0-9_-]{1,32}$` |
| `reason`, `hint` | — | never read or surfaced |
| — | `freshForSeconds: 420` | cache interval plus one agent poll |
| — | `subscriptionPriceUsd: null` | no price is assumed |

An error card has `error` set and no `gauges`. Possible errors:
`muse CLI not installed`, `muse usage command failed`, `muse usage timed out`,
`muse usage output malformed`, `muse usage schema unsupported`,
`allowance unavailable (CLI reported none)`, `allowance observation time missing`
and `allowance has no valid windows`. After a failure the agent keeps the
last good windows with their original `fetchedAt`, `observedAt` and `plan`,
and the error marks them as cached.

## Consumers

The hub passes cards through `/api/fleet` unchanged. The UI merges the same
`key` across agents and shows it once, preferring an error-free, fresher
reading. It ages a card from the older of `observedAt` and `fetchedAt`.
Chief's `scripts/usage.py` `gauge_row` accepts the `5h` and `Weekly` gauges
because their `windowSeconds` match the labels, and it marks undocumented
windows unknown. Muse Code has no SUS.

## Not known

The CLI reports no units beyond a percentage, no absolute caps, no billing
mode and no model scope. A Wonder Full listing in a model catalog does not
prove entitlement. Pay-as-you-go Meta API keys and OpenCode's Muse Spark are
separate pools and are not read here.
