# Antigravity synthetic fixtures

The inline fixtures in `usage_test.go`, `antigravity_cli_test.go`,
`antigravity_gemini_test.go` and `scripts/usage.test.cjs` are synthetic schema
examples, not captured provider responses. Values, group names and reset times
are chosen to distinguish Gemini Models from Argon and third-party pools.

The source contract is the existing parser fixtures at
[main 21cb01a](https://github.com/hemsoft-dev/hs-pi-dashboard/tree/21cb01a0a9b9f2b9713b14baee6f8f8927899e29/internal/usage)
and Chief's read-only `scripts/usage.py` `AG_BUCKETS` / `ag_gauges` evidence
recorded in [issue #65](https://github.com/hemsoft-dev/hs-pi-dashboard/issues/65).
That evidence identifies `gemini-5h` and `gemini-weekly` as the Gemini Models
buckets, distinct from `gemini-argon-wtu-weekly`, `3p-5h` and `3p-weekly`.
CLI printmode uses `id`, `remaining_fraction` and `reset_time`; the endpoint
uses `bucketId`, `remainingFraction` and `resetTime`. Both carry `window`.
Group display text is not an identity contract.

No independently saved raw `command.data.groups` response was available.
The metadata-only `antigravity.result.json` audit cache was not quota evidence
and was not used. No provider command, account invocation or live capture was
performed for this change. Tests inject command output or an HTTP transport;
rendered evidence uses an isolated loopback fixture and synthetic fleet data.
