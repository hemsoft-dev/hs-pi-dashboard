# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Users

Franz, the single operator of a private Tailscale fleet of coding-agent machines (home, laptop, air, mini, grokbot). He checks the dashboard between agent runs to see what every agent is doing and how much plan capacity is left.

## Product Purpose

hs-pi-dashboard is one Go binary that collects Pi, Claude Code, T3 Code, Herdr and Hermes session state from every machine and serves a single fleet view from mini. Success means one glance answers "what is running, what needs me, and will any plan run out before it resets".

## Operating Context

- Viewed in a desktop browser tab, in T3 Code's narrow browser panel, and on a phone over Tailscale. All three are regular, so every section must work from about 390px wide up.
- The page polls `/api/fleet` every 5 seconds; agents refresh provider usage every 60 seconds. Sections are collapsible and remember their state.
- The main question for Usage & Balances is whether a quota window will run out before it resets. Prepaid balances and collector health are secondary.

## Capabilities and Constraints

- Plain HTML, CSS and JavaScript embedded in the Go binary with `embed`; no build step, framework or web font downloads.
- Read-only: the dashboard never sends input to agents or providers.
- Weekly subscription usage includes per-window SUS and a provider-price-weighted Fleet SUS. Scores share the displayed forecast; fresh or unavailable windows show n/a. Grok reports observed seven-day activity without a quota estimate.
- Usage data comes from the `/api/fleet` contract: quota cards with windows (`usedPercent`, `resetAt`, `windowSeconds`, `projectedEnd`), balance cards (`balanceUsd`), observed activity cards (`activity` with hashed turn IDs/timestamps and source coverage), optional subscription prices (`subscriptionPriceUsd`), live-source limitations (`warning`), per-card `error` and `fetchedAt`.
- Provider collectors can fail independently; the UI must keep failures, stale data and missing values explicit rather than showing a healthy zero.

## Brand Commitments

- Gold on near-black (`#ffd700` on `#050505`), monospace type, uppercase section titles. Franz chose to keep this identity for the Usage & Balances redesign (2026-10-10).
- The Pulse monitor's green trace is its own instrument and stays green.

## Evidence on Hand

- Live data exists only for Franz's own accounts. The repository is public, so screenshots and fixtures use synthetic values.

## Product Principles

1. Answer the operator's question at a glance; details are one click away, never in the way.
2. Failures stay visible and honest but take less room than working data.
3. One visual language across sections: a new section looks like it was always part of the dashboard.
4. Nothing on the page changes provider or agent state.

## Accessibility & Inclusion

Keyboard operation of every control, focus kept across the 5-second refresh, values readable without color, and reduced-motion support.
