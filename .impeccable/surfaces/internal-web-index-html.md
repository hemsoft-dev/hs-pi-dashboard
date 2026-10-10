---
version: 1
slug: "internal-web-index-html"
primary_target: "internal/web/index.html"
related_targets: []
---

Scope: the Usage & Balances section of the fleet dashboard (internal/web/index.html). Mode: Operate. Extension of the established gold-on-black world; no new identity.

Audience and job: Franz, on desktop, T3's narrow panel and phone, checking whether any quota window will run out before it resets. Balances and collector health are secondary.

Constraints: /api/fleet contract unchanged; keyboard toggles keep focus across the 5 s refresh; saved section and per-provider collapse; synthetic data only in evidence (public repo).

## Direction contract

THESIS: A runway board, not a wall of dials. Every quota window is one row on a shared 0-125% scale with a common 100% limit line, so a forecast that crosses the line is visible down the whole column. Refuses the category default of one speedometer card per provider.

OWN-WORLD: The session table's language: bordered #0d0d0d panel, #080808 gold group header rows, 11-12px monospace, tabular numerals, uppercase column heads. Gold for measured use, translucent gold for the forecast, ember (#ff7a4d) only where a forecast passes the limit.

STORY: Franz sees which window runs out first and when, then confirms the rest are on pace, then notices balances and any provider that is not reporting.

FIRST VIEWPORT: Section header carries the verdict ("Codex weekly runs out in 1d 6h") and one freshness stamp. Below it, provider groups ordered by urgency, rows ordered by urgency, the limit line aligned across rows. Balances and a one-line "not reporting" disclosure close the panel.

FORM: Instrument table (position 1 of 3 structures considered: runway table, ranked list, per-provider cards); no seed key, extension path.

FINISH: unreviewed and undocumented is unfinished; this build ends with the finish review, the verdict, DESIGN.md, and every shipping raster carrying its provenance
