const assert = require('node:assert/strict');
const { test } = require('node:test');
const { readFileSync } = require('node:fs');
const vm = require('node:vm');
const path = require('node:path');

function renderer() {
  const html = readFileSync(path.join(__dirname, '../internal/web/index.html'), 'utf8');
  const storage = new Map();
  const body = { innerHTML: '', contains: () => false, querySelectorAll: () => [] };
  const r = vm.createContext({
    localStorage: { getItem: key => storage.get(key) ?? null, setItem: (key, value) => storage.set(key, value) },
    document: { activeElement: null, getElementById: () => body },
    esc: s => String(s ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c])),
    lastFleet: null,
  });
  vm.runInContext(html.slice(html.indexOf('function compactDur('), html.indexOf('let lastFleet = null;')), r);
  vm.runInContext(html.slice(html.indexOf('function cardCollapsed('), html.indexOf('const USAGE_TOGGLE_KEY')), r);
  return { r, body, storage };
}
function gauge(usedPercent, projectedEnd = null) {
  return { label: 'Weekly', usedPercent, projectedEnd, resetAt: new Date(Date.now() + 3600000).toISOString() };
}
function card(overrides = {}) {
  return { key: 'codex', label: 'Codex', kind: 'quota', fetchedAt: new Date().toISOString(), gauges: [gauge(50, 125)], ...overrides };
}

test('quota endpoints and midpoint have correct measured geometry and text', () => {
  const { r } = renderer();
  const zero = r.gaugeSvg(gauge(0));
  assert.match(zero, /0% used/);
  assert.doesNotMatch(zero, /class="measured-arc"/);
  assert.match(r.gaugeSvg(gauge(50)), /class="measured-arc" d="M 14.00 70.00 A 58 58 0 0 1 72.00 12.00"/);
  assert.match(r.gaugeSvg(gauge(100)), /class="measured-arc" d="M 14.00 70.00 A 58 58 0 0 1 130.00 70.00"/);
});

test('forecast has its own marker and uncapped over-limit text', () => {
  const { r } = renderer();
  const html = r.gaugeSvg(gauge(50, 125));
  assert.match(html, /50% used.*At reset 125% · over limit/);
  assert.match(html, /class="forecast-marker" cx="130.00" cy="70.00"/);
  assert.doesNotMatch(r.gaugeSvg(gauge(50)), /class="forecast-marker"/);
  assert.match(r.gaugeSvg(gauge(50)), /forecast unavailable/);
});

test('missing and invalid numbers never look like healthy zero or create invalid SVG', () => {
  const { r } = renderer();
  for (const value of [null, undefined, NaN, Infinity, '50']) {
    const html = r.gaugeSvg({ ...gauge(value, value), resetAt: 'invalid' });
    assert.match(html, /Weekly: unavailable; reset time unavailable; forecast unavailable/);
    assert.doesNotMatch(html, /NaN|Infinity|0% used|measured-arc|forecast-marker/);
  }
  assert.match(r.gaugeSvg(gauge(-5)), /0% used/);
  assert.match(r.gaugeSvg(gauge(105)), /100% used/);
  assert.match(r.gaugeSvg({ label: '<img onerror=x>', usedPercent: 50 }), /&lt;img onerror=x&gt;/);
});

test('errors, stale and missing timestamps stay explicit with cached quota and balance values', () => {
  const { r } = renderer();
  assert.match(r.usageCard(card({ error: 'fetch failed', fetchedAt: new Date(Date.now()-240000).toISOString() }), 'global'), /stale · 4m ago.*fetch failed · cached values/s);
  assert.match(r.usageCard(card({ fetchedAt: null }), 'global'), /update time unavailable/);
  assert.match(r.usageCard(card({ kind:'balance', balanceUsd: 37.88, error:'offline' }), 'global'), /offline · cached values.*\$37.88/s);
  assert.match(r.usageCard(card({ kind:'balance', balanceUsd: null }), 'global'), /balance unavailable/);
});

test('account deduplication prefers healthy then fresh data across refreshes and keeps collapse', () => {
  const { r, body, storage } = renderer();
  const fresh = card({ gauges:[gauge(0)] });
  const fleet = { machines:[{ usage:{ cards:[card({ error:'offline', gauges:[gauge(100)] })] } }, { usage:{ cards:[fresh, card({ key:'balance', label:'Moonshot', kind:'balance', balanceUsd:37.88 })] } }] };
  r.renderUsage(fleet);
  assert.equal((body.innerHTML.match(/class="ucard-label">Codex/g)||[]).length, 1);
  assert.match(body.innerHTML, /0% used/);
  assert.match(body.innerHTML, /\$37.88/);
  assert.doesNotMatch(body.innerHTML, /offline/);
  const newer = card({ gauges:[gauge(100)], fetchedAt:new Date(Date.now()+60000).toISOString() });
  fleet.machines.push({ usage:{ cards:[newer] } });
  storage.set('hs-pi-dashboard:card-collapsed:global:codex', '1');
  r.lastFleet = fleet;
  r.renderUsage(fleet);
  assert.match(body.innerHTML, /aria-expanded="false"/);
  assert.match(body.innerHTML, /100% used/);
  assert.doesNotMatch(body.innerHTML, /gauge-dial/);
  r.toggleCard('global', 'codex');
  assert.equal(storage.get('hs-pi-dashboard:card-collapsed:global:codex'), '0');
  assert.match(body.innerHTML, /aria-expanded="true"/);
  assert.match(body.innerHTML, /gauge-dial/);
});
