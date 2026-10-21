---
name: Agent Fleet Dashboard
description: One gold-on-black monospace board for every coding agent, quota window and balance in a private Tailscale fleet.
colors:
  gold: "#ffd700"
  gold-dim: "#b89b23"
  ember: "#ff7a4d"
  ember-dim: "#c2603f"
  bg: "#050505"
  group-head: "#080808"
  subrow: "#0b0b0a"
  card: "#0d0d0d"
  card-border: "#262626"
  track: "#1d1c18"
  text: "#e8e4d8"
  muted: "#8a8578"
  muted-light: "#aaa596"
  forecast-fill: "rgba(255, 215, 0, .14)"
  forecast-edge: "rgba(184, 155, 35, .7)"
  overrun-fill: "rgba(255, 122, 77, .32)"
  limit-line: "rgba(138, 133, 120, .55)"
  active-wash: "rgba(255, 215, 0, .07)"
  state-working: "#7ce3a3"
  state-error: "#ffab91"
  pulse-green: "#32ff7e"
  pulse-label: "#7ee8a6"
  pulse-screen: "#000603"
  pulse-border: "#123c24"
  pulse-beat-flash: "#b9ffd1"
  idle-dot: "#333"
  chip-working-bg: "#29230c"
  chip-working-text: "#ffe57b"
  chip-blocked-bg: "#3a1914"
  chip-blocked-text: "#ffb2a5"
  chip-done-bg: "#142d20"
  chip-done-text: "#a1dfb3"
  chip-idle-bg: "#1b2433"
  chip-idle-text: "#b9ccee"
  chip-neutral-bg: "#222222"
  chip-neutral-text: "#bbbbbb"
typography:
  display:
    fontFamily: '"Cascadia Code", "JetBrains Mono", ui-monospace, Consolas, monospace'
    fontSize: "34px"
    fontWeight: 700
    lineHeight: 1
  headline:
    fontFamily: '"Cascadia Code", "JetBrains Mono", ui-monospace, Consolas, monospace'
    fontSize: "20px"
    fontWeight: 600
    letterSpacing: "2px"
  title:
    fontFamily: '"Cascadia Code", "JetBrains Mono", ui-monospace, Consolas, monospace'
    fontSize: "14px"
    fontWeight: 600
    letterSpacing: "1px"
  figure-lead:
    fontFamily: '"Cascadia Code", "JetBrains Mono", ui-monospace, Consolas, monospace'
    fontSize: "14px"
    fontWeight: 600
    fontFeature: '"tnum"'
  verdict:
    fontFamily: '"Cascadia Code", "JetBrains Mono", ui-monospace, Consolas, monospace'
    fontSize: "13px"
    fontWeight: 600
    fontFeature: '"tnum"'
  group-label:
    fontFamily: '"Cascadia Code", "JetBrains Mono", ui-monospace, Consolas, monospace'
    fontSize: "12px"
    fontWeight: 600
    letterSpacing: ".04em"
  body:
    fontFamily: '"Cascadia Code", "JetBrains Mono", ui-monospace, Consolas, monospace'
    fontSize: "12px"
    fontWeight: 400
    fontFeature: '"tnum"'
  body-dense:
    fontFamily: '"Cascadia Code", "JetBrains Mono", ui-monospace, Consolas, monospace'
    fontSize: "11px"
    fontWeight: 400
    lineHeight: 1.6
  label:
    fontFamily: '"Cascadia Code", "JetBrains Mono", ui-monospace, Consolas, monospace'
    fontSize: "10px"
    fontWeight: 600
    letterSpacing: ".05em"
rounded:
  hairline: "1px"
  sm: "3px"
  md: "4px"
  dot: "50%"
spacing:
  hair: "4px"
  xs: "6px"
  sm: "8px"
  md: "10px"
  panel-x: "14px"
  gutter: "16px"
  column-gap: "18px"
  page: "24px"
  section: "28px"
components:
  section-toggle:
    textColor: "{colors.gold}"
    typography: "{typography.title}"
    padding: "10px 2px"
  board:
    backgroundColor: "{colors.card}"
    textColor: "{colors.text}"
    rounded: "{rounded.md}"
  group-head:
    backgroundColor: "{colors.group-head}"
    textColor: "{colors.gold}"
    typography: "{typography.group-label}"
    padding: "0 14px"
    height: "40px"
  column-head:
    textColor: "{colors.muted}"
    typography: "{typography.label}"
    padding: "9px 14px 7px 36px"
  runway-row:
    textColor: "{colors.text}"
    typography: "{typography.body}"
    padding: "0 14px 0 36px"
    height: "32px"
  runway-track:
    backgroundColor: "{colors.track}"
    rounded: "{rounded.hairline}"
    height: "8px"
  runway-track-lead:
    backgroundColor: "{colors.track}"
    rounded: "{rounded.hairline}"
    height: "10px"
  runway-used:
    backgroundColor: "{colors.gold-dim}"
  runway-used-tight:
    backgroundColor: "{colors.gold}"
  runway-used-out:
    backgroundColor: "{colors.ember}"
  runway-forecast:
    backgroundColor: "{colors.forecast-fill}"
  runway-overrun:
    backgroundColor: "{colors.overrun-fill}"
  balance-amount:
    textColor: "{colors.gold}"
    typography: "{typography.figure-lead}"
  pager-button:
    backgroundColor: "{colors.card}"
    textColor: "{colors.gold-dim}"
    rounded: "{rounded.sm}"
    padding: "2px 10px"
  pager-button-hover:
    backgroundColor: "{colors.card}"
    textColor: "{colors.gold}"
  select:
    backgroundColor: "{colors.card}"
    textColor: "{colors.text}"
    rounded: "{rounded.md}"
    padding: "6px 28px 6px 10px"
    height: "36px"
  machine-tile:
    backgroundColor: "{colors.card}"
    textColor: "{colors.text}"
    rounded: "{rounded.md}"
    padding: "10px 14px"
    height: "58px"
  state-chip-working:
    backgroundColor: "{colors.chip-working-bg}"
    textColor: "{colors.chip-working-text}"
    rounded: "{rounded.sm}"
    padding: "4px 7px"
  state-chip-blocked:
    backgroundColor: "{colors.chip-blocked-bg}"
    textColor: "{colors.chip-blocked-text}"
    rounded: "{rounded.sm}"
    padding: "4px 7px"
  state-chip-done:
    backgroundColor: "{colors.chip-done-bg}"
    textColor: "{colors.chip-done-text}"
    rounded: "{rounded.sm}"
    padding: "4px 7px"
  state-chip-idle:
    backgroundColor: "{colors.chip-idle-bg}"
    textColor: "{colors.chip-idle-text}"
    rounded: "{rounded.sm}"
    padding: "4px 7px"
  state-chip-unknown:
    backgroundColor: "{colors.chip-neutral-bg}"
    textColor: "{colors.chip-neutral-text}"
    rounded: "{rounded.sm}"
    padding: "4px 7px"
  pulse-panel:
    backgroundColor: "{colors.pulse-screen}"
    textColor: "{colors.pulse-green}"
    typography: "{typography.display}"
    rounded: "{rounded.md}"
    height: "184px"
---

# Design System: Agent Fleet Dashboard

## Overview

**Creative North Star: "The Night-Shift Console"**

The dashboard is an operator's console read between agent runs: near-black glass, one gold voice, one monospace face, and dense rows of tabular figures. Everything is a bordered panel on a black page, and every panel is a table at heart: uppercase column heads in muted ink, gold group header rows, then data rows at 11-13px. Density is high on purpose. Franz reads it on a desktop tab, in T3 Code's narrow panel and on a phone, so the layout reflows instead of hiding meaning, and the same row grammar holds at 390px.

Color carries state, never decoration. Gold marks identity and measured values; dim gold is the at-rest fill; translucent gold is a forecast. Warm ember appears only when something has gone past a plan limit or a collector has failed. The Pulse monitor is the one place the system leaves gold: it is a separate green phosphor instrument, and it stays green (a PRODUCT.md brand commitment).

Depth comes from tonal steps between near-blacks and 1px hairlines, not shadows. Motion is small and functional: chevrons rotate, live dots breathe, a changed row flashes gold once. All of it switches off under reduced motion.

**Key Characteristics:**
- Gold (#ffd700) on near-black (#050505), one monospace stack, uppercase section titles.
- Tables everywhere: bordered #0d0d0d panels, #080808 gold group header rows, 10px uppercase column heads.
- Tabular numerals for every figure that can change.
- Flat, hairline-bordered surfaces; only the Pulse instrument glows.
- State is always spelled out in words as well as color ("limit reached", "no reading", "refresh failed").

## Colors

A single-accent warm palette: gold on graded near-blacks with warm-grey ink, plus one ember warning tone and a separately owned green instrument.

### Primary
- **Signal Gold** (`gold`): page title, section titles, group header labels, row titles in the session table, balance amounts, the live dot, focus rings, and measured usage once a window is tight or at risk. It is the identity color and the "look here" color.
- **Burnished Gold** (`gold-dim`): the at-rest measured fill of a runway bar, pager button text, footer links, chevrons and the online badge border. Gold that is present but not asking for attention.
- **Forecast Gold** (`forecast-fill` with `forecast-edge` outline): translucent gold that extends a runway bar from measured use to the projected value at reset, up to the 100% limit.

### Secondary
- **Ember** (`ember`): the over-limit and failure tone of the Usage & Balances section only. It colors the projected value and phrase of a window forecast to pass its limit, the measured bar and value of a window already at 100%, the verdict in the section header when that verdict is a risk, the group summary of such a provider, the "refresh failed · cached" note, and the "Not reporting:" / "Refresh failing:" labels.
- **Ember Overrun** (`overrun-fill` with `ember-dim` outline): the translucent ember segment drawn from the 100% line to the projected end. Past 125% it fades out through a mask instead of clipping hard.

### Neutral
- **Page Black** (`bg`): the page.
- **Header Black** (`group-head`): group header rows in the runway board and machine rows in the session table, one step below the panel.
- **Output Black** (`subrow`): expanded session output sub-rows.
- **Panel Black** (`card`): every bordered panel, button and select.
- **Hairline** (`card-border`): panel borders, row dividers and the section-toggle underline.
- **Runway Track** (`track`): the warm empty track behind every runway bar.
- **Parchment** (`text`): primary text and measured percentages.
- **Ash** (`muted`): column heads, secondary metadata, reset times, freshness stamps and missing values. The `limit-line` hairline is Ash at 55%.
- **Light Ash** (`muted-light`): secondary text in the Herdr section (paths, notes, filter labels). It is a reused literal in herdr.css rather than a custom property.
- **Active Wash** (`active-wash`): the 7% gold row tint for an active session; a 22% version flashes and fades once when a row changes.

### State
- **Working Green** (`state-working`): working and monitoring states in the session table.
- **Error Coral** (`state-error`): error and unavailable states in the session table and source-collector failures.
- **Herdr state chips**: tinted pill pairs (working: `chip-working-text` on `chip-working-bg`; blocked: `chip-blocked-text` on `chip-blocked-bg`; done: `chip-done-text` on `chip-done-bg`; idle: `chip-idle-text` on `chip-idle-bg`; unknown: `chip-neutral-text` on `chip-neutral-bg`).

### Pulse instrument
- **Phosphor Green** (`pulse-green`) and **Phosphor Label** (`pulse-label`) on **Scope Black** (`pulse-screen`) inside a **Scope Rim** (`pulse-border`). Used only by the Pulse monitor.

### Contrast (WCAG 2.x, text on Panel Black #0d0d0d)

| Token | Ratio on `card` | Ratio on `group-head` | Result |
|---|---|---|---|
| `text` | 15.29:1 | 15.75:1 | AAA |
| `gold` | 13.86:1 | 14.28:1 | AAA |
| `state-working` | 12.38:1 | 12.75:1 | AAA |
| `state-error` | 10.63:1 | 10.95:1 | AAA |
| `muted-light` | 7.90:1 | 8.14:1 | AAA |
| `ember` | 7.54:1 | 7.77:1 | AAA |
| `gold-dim` | 7.17:1 | 7.39:1 | AAA |
| `muted` | 5.28:1 | 5.44:1 | AA (AAA needs 7:1) |
| `ember-dim` | 4.66:1 | 4.81:1 | AA; used only as a bar outline, not text |

Herdr chips (text on their own fill): working 12.50:1, blocked 9.15:1, done 9.62:1, idle 9.60:1, unknown 8.29:1. Pulse green on Scope Black is 14.57:1. Every text color on the panel clears AA at the 10-12px sizes in use; `muted` is the floor, so nothing dimmer may carry readable text.

### Named Rules
**The Ember Means Past-the-Line Rule.** Ember is reserved for Usage & Balances, and there only for a forecast or measurement past the plan limit and for collector-failure labels. It never decorates, never marks "tight" (that is gold), and other sections keep their own error coral.

**The Gold Ladder Rule.** Measured use climbs a fixed ladder: Burnished Gold at rest, Signal Gold when tight (85% or more measured or forecast) or at risk, Ember only at 100%. A forecast is always translucent, never solid.

**The Green Belongs to Pulse Rule.** Phosphor green is the Pulse instrument's own palette. Outside Pulse, green appears only as a state word color (working, done); it never becomes an accent.

## Typography

**Display Font:** Cascadia Code (with JetBrains Mono, ui-monospace, Consolas, monospace)
**Body Font:** the same stack
**Label/Mono Font:** the same stack

**Character:** One terminal face at every level. Hierarchy comes from size steps of 1-2px, weight 600 versus 400, gold versus parchment, and uppercase tracking, never from a second family. No web fonts are downloaded (a PRODUCT.md constraint); the stack uses whatever coding font the machine already has.

### Hierarchy
- **Display** (700, 34px, line-height 1): the Pulse active-session count only, in phosphor green with a soft glow.
- **Headline** (600, 20px, 2px tracking, uppercase): the page title.
- **Title** (600, 14px, 1px tracking, uppercase): collapsible section titles (Live Herdr agents, Usage & Balances, Pulse).
- **Figure Lead** (600, 14px, tabular): a lead runway row's used and forecast values, and balance amounts.
- **Verdict** (600, 13px, tabular, sentence case): the one-line answer beside a section title ("Codex weekly runs out in 1d 6h").
- **Group Label** (600, 12px, .04em tracking): provider names in gold group header rows. Session machine rows use 11px with 1px tracking.
- **Body** (400, 12px, tabular): runway rows, balances, failure lists. Herdr rows run at 13px; the session table at 11px.
- **Body Dense** (400, 11px, line-height 1.6): session metadata, notes, freshness stamps and footers.
- **Label** (600, 10px, .05em tracking, uppercase): column heads and in-panel row labels such as "Prepaid balances".

### Named Rules
**The One Face Rule.** Every glyph on the page is the monospace stack. Contrast comes from weight, size, case and color.

**The Tabular Figures Rule.** Any number that refreshes (percentages, durations, counts, money) is set with `font-variant-numeric: tabular-nums` so columns stay aligned across the 5-second poll.

## Layout

A full-width, single-column stack of collapsible sections on a 24px page margin, with no max-width container. Each section is a gold uppercase toggle with a hairline underline (14px below), then a bordered panel. Sections remember their collapsed state.

Inside panels the model is a grid table. Cells use 14px horizontal padding (10px in the session table, 6px on phones); grouped rows indent 36px so data aligns past the group chevron. The Usage runway board uses five columns: window `minmax(140px, 1.1fr)`, bar `minmax(180px, 2.4fr)`, used `56px`, at reset `minmax(168px, 1.2fr)`, resets in `84px`, with an 18px column gap. Row heights are 32px for data rows, 36px for disclosures, 40px for group headers and 58px for Herdr machine tiles.

Responsive behavior keeps meaning and drops chrome:
- **820px:** runway column heads hide; each row becomes a two-column card (window / resets-in, full-width bar, used / forecast), and the bar's sr-only labels become visible.
- **680px:** the session table keeps computer, title and model and hides the rest.
- **640px:** the Usage verdict wraps under the title and extra-risk counts move to their own line.
- **600px:** Herdr column heads hide and rows restack into a two-column grid; selects go full width at 42px tall.

Every section is expected to work from 390px wide.

## Elevation & Depth

The system is flat. Depth is tonal: Page Black (#050505) under Header Black group rows (#080808) under Panel Black (#0d0d0d), separated by 1px Hairline borders. No card, button or table casts a shadow. Hover states lift with color (border to Burnished Gold, a #18170f warm wash on machine tiles), not elevation.

### Shadow Vocabulary
- **Scope glow** (`box-shadow: inset 0 0 24px rgba(0, 255, 102, .06), 0 0 12px rgba(0, 255, 102, .04)`): the Pulse panel rim only.
- **Phosphor bloom** (`filter: drop-shadow(0 0 4px rgba(50, 255, 126, .72))`, `text-shadow: 0 0 8px rgba(50, 255, 126, .7)`): the Pulse trace and readout only, reduced to 2px under reduced motion.

### Named Rules
**The Hairline, Not Shadow Rule.** Separation is a 1px `card-border` line or a one-step tonal change. Glow belongs to the Pulse instrument and nowhere else.

## Shapes

Nearly square. Panels, selects and machine tiles use gently softened 4px corners; pills, pager buttons and badges use 3px; runway bars use a 1px hairline radius so they read as instrument tracks, not capsules. Circles appear only as 8px status dots. Chevrons are 12px stroked SVG paths that rotate -90 degrees when collapsed. The one recurring geometry is the vertical limit line: a 1px Ash hairline at the 100% mark that extends above and below each bar so it reads as one line down the whole column.

## Components

### Section Toggle
Quiet, full-width and typographic. A transparent button in Title type and Signal Gold, a Burnished Gold chevron, an optional Ash hint or verdict on the right, and a hairline underline. The chevron rotates over 150ms (none under reduced motion). Focus is a 2px gold outline.

### Boards / Panels
- **Corner Style:** 4px.
- **Background:** Panel Black, Header Black for group rows.
- **Shadow Strategy:** none (see Elevation & Depth).
- **Border:** 1px Hairline; rows divide with the same hairline, and phone layouts divide stacked runway rows with a fainter #1a1a17 line.
- **Internal Padding:** 14px horizontal; 6px x 10px cells in the session table.

### Runway Row (signature)
One quota window per row on a shared 0-125% scale, so the 100% limit sits at 80% of every bar's width and the limit line stacks into one vertical rule. The bar layers a measured segment (Burnished Gold, Signal Gold when tight or at risk, Ember at 100%), a translucent gold forecast to the limit, and a translucent ember overrun past it. Groups and rows sort by urgency; a group's first row, when it is tight or worse, becomes the lead row with a 10px bar and 14px figures. Each row states its condition in words ("limit reached", "runs out in 2h", "near limit", "no forecast", "no reading"); a missing reading shows an Ash em dash, never 0%.

### Failure Disclosure
A `details` row at the foot of the board: Ember labels ("Not reporting:", "Refresh failing:") followed by Ash provider names, expanding to one Parchment-named line per failure. It takes one line when closed, so failures stay visible but smaller than working data.

### Provider selection

An inline "Choose providers" disclosure sits beneath the Usage heading and
outside its collapsible data body. A native fieldset groups labeled checkboxes
with a gold accent, Parchment text, 42px label targets and the existing gold
focus outline. Options wrap naturally on phones. A quiet Ash-light note states
that choices are saved in this browser. The controls stay mounted across polls
and remain available in empty or offline states. Deselected providers contribute
neither readings nor warnings to the board or its collapsed verdict.

### Buttons
- **Shape:** 3px corners.
- **Pager:** Panel Black fill, Hairline border, Burnished Gold 12px text, 2px x 10px padding.
- **Hover / Focus:** text to Signal Gold and border to Burnished Gold; focus is a 2px gold outline with 3px offset.
- **Disabled:** 35% opacity, default cursor.

### Chips
- **Style:** tinted pill (3px corners, 4px x 7px, 12px text), light text on a dark tint of the same hue.
- **State:** working, blocked, done, idle and unknown pairs as listed in Colors. The session table's online/offline badge is the outline variant: 10px text, 1px border, Signal Gold on Burnished Gold, or Ash on Hairline.

### Inputs / Fields
- **Style:** native select on Panel Black, 1px Hairline border, 4px corners, 36px tall (42px on phones), inherits the monospace face.
- **Focus:** 2px gold outline, 3px offset.

### Navigation
Herdr machine tiles act as navigation: flexible 190px-basis tiles with a gold 14px machine name, Light Ash 11px detail, and an 18px gold icon on the right. Hover warms the fill to #18170f and the border to Burnished Gold. Unavailable machines drop the name to Light Ash.

### Pulse Monitor
A self-contained green oscilloscope on Scope Black with a Scope Rim border, a 34px phosphor readout top-left, a faint vertical cursor rail at 75% and a glowing cursor dot that breathes (1.8s) and beats on activity (0.52s). Its colors and glows are not reused anywhere else.

## Do's and Don'ts

### Do:
- **Do** put every new section in a Section Toggle plus a bordered Panel Black board with a Hairline border and 4px corners.
- **Do** use Signal Gold (#ffd700) for identity labels and urgent figures, Burnished Gold (#b89b23) for at-rest fills, and Ash (#8a8578) as the dimmest text color.
- **Do** set refreshing numbers in tabular figures and state every condition in words as well as color.
- **Do** keep Ember (#ff7a4d) to past-the-limit forecasts and measurements and to collector-failure labels inside Usage & Balances.
- **Do** keep missing data explicit: an Ash em dash plus "no reading" or "unknown", never a healthy-looking zero.
- **Do** give every control a 2px gold focus outline and turn off chevron, dot and glow animation under `prefers-reduced-motion`.

### Don't:
- **Don't** add a second typeface or download web fonts; the monospace stack is the identity.
- **Don't** use drop shadows or glows on boards, buttons or rows; glow belongs to the Pulse instrument.
- **Don't** use green as an accent or recolor the Pulse monitor gold.
- **Don't** use Ember for "near limit" or for decoration; near limit is gold.
- **Don't** set readable text dimmer than Ash (#8a8578, 5.28:1 on #0d0d0d).
- **Don't** render quota windows as one dial or card per provider; quota windows share one runway scale and one limit line.

## Projects

Projects uses the existing gold section toggle and hairline-bordered repository
rows. A summary keeps the repository, Clean/Attention state, PR/issue counts and
checkout count visible. Native keyboard-operable disclosures reveal per-machine
local health followed by one set of GitHub tables. Coral marks unavailable or
attention health; producer verdict words remain visible. Tables scroll within
the panel on phones, with focusable labeled scroll regions. Expansion and focus
survive polling; the section preference is browser-local.

## GitHub billing

The GitHub organization card sits inside Usage & Balances using its existing
gold figures, muted timestamps, tabular monospace values and hairline tables.
The lead row pairs measured credits and net dollars with their month-end
projections. Product spend and budget tables retain scope and stop-state words.
Ember identifies billing attention or collection errors. The paid-usage policy
limitation stays visible. Tables scroll within labeled keyboard-focusable regions
on phones; provider selection removes the card and its alerts together.
