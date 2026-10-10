const assert = require('node:assert/strict');
const { test } = require('node:test');
const { readFileSync } = require('node:fs');
const vm = require('node:vm');
const path = require('node:path');

const H = 3600000;

function renderer() {
  const html = readFileSync(path.join(__dirname, '../internal/web/index.html'), 'utf8');
  const storage = new Map();
  const nodes = {
    'usage-body': { innerHTML: '', contains: () => false, querySelectorAll: () => [], querySelector: () => null },
    'usage-summary': { innerHTML: '', textContent: '' },
  };
  const r = vm.createContext({
    localStorage: { getItem: key => storage.get(key) ?? null, setItem: (key, value) => storage.set(key, value) },
    document: { activeElement: null, getElementById: id => nodes[id] },
    esc: s => String(s ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c])),
    lastFleet: null,
  });
  vm.runInContext(html.slice(html.indexOf('function rel('), html.indexOf('let lastFleet = null;')), r);
  vm.runInContext(html.slice(html.indexOf('function cardCollapsed('), html.indexOf('const USAGE_TOGGLE_KEY')), r);
  return { r, body: nodes['usage-body'], summary: nodes['usage-summary'], storage };
}
function gauge(usedPercent, projectedEnd = null, extra = {}) {
  return { label: 'Weekly', usedPercent, projectedEnd, resetAt: new Date(Date.now() + 10 * H).toISOString(), ...extra };
}
function card(overrides = {}) {
  return { key: 'codex', label: 'Codex', kind: 'quota', fetchedAt: new Date().toISOString(), gauges: [gauge(50, 125)], ...overrides };
}
const widthOf = (html, cls) => Number((html.match(new RegExp(`class="${cls}[^"]*" style="left:[\\d.]+%;width:([\\d.]+)%`)) || [])[1] ?? NaN);

test('window labels become readable words', () => {
  const { r } = renderer();
  assert.equal(r.windowLabel('Weekly'), 'Weekly');
  assert.equal(r.windowLabel('5h'), '5-hour');
  assert.equal(r.windowLabel('Monthly'), 'Monthly');
  assert.equal(r.windowLabel('Gemini W'), 'Gemini · weekly');
  assert.equal(r.windowLabel('3P 5h'), 'Third-party · 5-hour');
  assert.equal(r.windowLabel('Window'), 'Window');
  assert.equal(r.windowLabel(''), 'Window');
});

test('quota boundaries draw on the shared 0-125% scale with the limit at 100%', () => {
  const { r } = renderer();
  const zero = r.quotaRow(r.windowState(gauge(0), 'Codex'));
  assert.match(zero, /0%<span class="ulbl"> used/);
  assert.doesNotMatch(zero, /ubar-used/);
  assert.equal(widthOf(r.quotaRow(r.windowState(gauge(50), 'Codex')), 'ubar-used'), 40);
  const full = r.quotaRow(r.windowState(gauge(100), 'Codex'));
  assert.equal(widthOf(full, 'ubar-used'), 80);
  assert.match(full, /urow s-out/);
  assert.match(full, /limit reached/);
  assert.match(full, /class="ubar-limit"/);
});

test('forecasts keep their uncapped value and say when the window runs out', () => {
  const { r } = renderer();
  const w = r.windowState(gauge(50, 125), 'Codex');
  assert.equal(w.status, 'risk');
  // 50% used, 125% at reset in 10h: 50 more points at 7.5/h runs out in 6h 40m.
  assert.equal(Math.round(w.runsOutMs / 60000), 400);
  const html = r.quotaRow(w);
  assert.match(html, /<b>125%<\/b><span class="ulbl"> at reset<\/span> <span class="uphrase">runs out in 6h 40m/);
  assert.equal(widthOf(html, 'ubar-fc'), 40);
  assert.equal(widthOf(html, 'ubar-over'), 20);
  assert.doesNotMatch(html, /ubar-over beyond/);
  assert.match(r.quotaRow(r.windowState(gauge(50, 190), 'Codex')), /ubar-over beyond.*<b>190%<\/b>/s);
  const onPace = r.quotaRow(r.windowState(gauge(30, 60), 'Codex'));
  assert.match(onPace, /urow s-ok.*<b>60%<\/b><span class="ulbl"> at reset<\/span><\/span>/s);
  assert.doesNotMatch(onPace, /ubar-over/);
  assert.match(r.quotaRow(r.windowState(gauge(80, 92), 'Codex')), /s-tight.*near limit/s);
  assert.match(r.quotaRow(r.windowState(gauge(40), 'Codex')), /no forecast/);
});

test('missing and invalid numbers never look like a healthy zero', () => {
  const { r } = renderer();
  for (const value of [null, undefined, NaN, Infinity, '50']) {
    const w = r.windowState({ ...gauge(value, value), resetAt: 'invalid' }, 'Codex');
    const html = r.quotaRow(w);
    assert.equal(w.status, 'unknown');
    assert.match(html, /no reading/);
    assert.match(html, /reset time <\/span>unknown/);
    assert.doesNotMatch(html, /NaN|Infinity|0%|ubar-used|ubar-fc/);
  }
  assert.equal(r.windowState(gauge(-5), 'Codex').used, 0);
  assert.equal(r.windowState(gauge(105), 'Codex').used, 100);
  assert.equal(r.remainingMs('0001-01-01T00:00:00Z'), null);
  assert.match(r.quotaRow(r.windowState({ label: '<img onerror=x>', usedPercent: 50 }, 'Codex')), /&lt;img onerror=x&gt;/);
});

test('urgency puts the window that runs out first ahead of everything else', () => {
  const { r } = renderer();
  const order = [gauge(20, 40), gauge(10, 130, { label: 'Late' }), gauge(90, 95), gauge(60, 150, { label: 'Soon' }), gauge(null), gauge(100)]
    .map(g => r.windowState(g, 'X')).sort(r.compareUrgency).map(w => `${w.status}:${w.raw}`);
  assert.deepEqual(order, ['out:Weekly', 'risk:Soon', 'risk:Late', 'tight:Weekly', 'ok:Weekly', 'unknown:Weekly']);
});

test('the board leads with the most urgent provider and states it in the section header', () => {
  const { r, body, summary } = renderer();
  const fleet = { machines: [{ usage: { cards: [
    card({ key: 'antigravity', label: 'Antigravity', gauges: [gauge(0, null, { label: 'Gemini 5h' }), gauge(18, 112, { label: '3P W' })] }),
    card({ gauges: [gauge(82, 131)] }),
    card({ key: 'moonshot', label: 'Moonshot AI', kind: 'balance', gauges: undefined, balanceUsd: 42.5 }),
  ] } }] };
  r.renderUsage(fleet);
  assert.ok(body.innerHTML.indexOf('>Codex<') < body.innerHTML.indexOf('>Antigravity<'));
  const ag = body.innerHTML.slice(body.innerHTML.indexOf('>Antigravity<'));
  assert.ok(ag.indexOf('Third-party · weekly') < ag.indexOf('Gemini · 5-hour'));
  assert.match(summary.innerHTML, /<span class="uverdict s-risk">Codex weekly runs out in [^<]+ <span class="umore"><span class="usep">· <\/span>1 more at risk<\/span><\/span><span class="usr">, <\/span><span class="ufresh">updated now<\/span>/);
  assert.match(body.innerHTML, /class="ucols"/);
  assert.match(body.innerHTML, /<li class="urow s-risk ulead"><span class="uwin">Weekly/);
  assert.equal((body.innerHTML.match(/ulead/g) || []).length, 1);
  assert.match(body.innerHTML, /Prepaid balances.*Moonshot AI<\/span><span class="uamount">\$42.50/s);
  assert.doesNotMatch(body.innerHTML, /ufail|unote/);

  r.renderUsage({ machines: [{ usage: { cards: [card({ gauges: [gauge(20, 40)] })] } }] });
  assert.match(summary.innerHTML, /uverdict s-ok">all plans on pace/);
  assert.doesNotMatch(body.innerHTML, /ulead/);
});

test('failures collapse into one disclosure while cached and stale data stay labeled', () => {
  const { r, body } = renderer();
  const old = new Date(Date.now() - 12 * 60000).toISOString();
  r.renderUsage({ machines: [{ usage: { cards: [
    card({ key: 'opencode-go', label: 'OpenCode Go', gauges: undefined, error: 'parse failed' }),
    card({ key: 'opencode-zen', label: 'OpenCode Zen', kind: 'balance', gauges: undefined, error: 'parse failed' }),
    card({ key: 'antigravity', label: 'Antigravity', error: 'timeout', fetchedAt: old }),
    card({ fetchedAt: old }),
    card({ key: 'moonshot', label: 'Moonshot AI', kind: 'balance', gauges: undefined, balanceUsd: 37.88, error: 'offline' }),
    card({ key: 'xai', label: 'xAI', kind: 'unknown', gauges: undefined }),
  ] } }] });
  const html = body.innerHTML;
  assert.doesNotMatch(html, />OpenCode Go<\/span>|>OpenCode Zen<\/span>/);
  assert.match(html, /<summary>.*Not reporting:<\/b> OpenCode Go, OpenCode Zen, xAI.*Refresh failing:<\/b> Antigravity, Moonshot AI<\/span><\/summary>/s);
  assert.match(html, /<b>OpenCode Go<\/b> couldn't read usage <code>\(parse failed\)<\/code>/);
  assert.match(html, /<b>OpenCode Zen<\/b> couldn't read balance/);
  assert.match(html, /<b>xAI<\/b> no usage source yet/);
  assert.match(html, /<b>Antigravity<\/b> refresh failed, showing cached values <code>\(timeout\)/);
  assert.match(html, /unote cached">refresh failed · cached 12m ago/);
  assert.match(html, />Codex<\/span><span class="unote">stale · 12m ago/);
  assert.match(html, /\$37.88<\/span><span class="unote cached">refresh failed · cached just now/);
  assert.doesNotMatch(html, /<details class="ufail" open/);
  vm.runInContext('usageFailuresOpen = true', r);
  r.renderUsage({ machines: [{ usage: { cards: [card({ gauges: undefined, error: 'x' })] } }] });
  assert.match(body.innerHTML, /<details class="ufail" open>/);
  assert.match(body.innerHTML, /no quota windows reported/);
});

test('account deduplication prefers healthy then fresh data and keeps collapse across refreshes', () => {
  const { r, body, storage } = renderer();
  const fleet = { machines: [
    { usage: { cards: [card({ error: 'offline', gauges: [gauge(100)] })] } },
    { usage: { cards: [card({ gauges: [gauge(0)] })] } },
  ] };
  r.renderUsage(fleet);
  assert.equal((body.innerHTML.match(/class="ucard-label">Codex/g) || []).length, 1);
  assert.match(body.innerHTML, /0%<span class="ulbl"> used/);
  assert.doesNotMatch(body.innerHTML, /offline/);
  fleet.machines.push({ usage: { cards: [card({ gauges: [gauge(100)], fetchedAt: new Date(Date.now() + 60000).toISOString() })] } });
  storage.set('hs-pi-dashboard:card-collapsed:global:codex', '1');
  r.lastFleet = fleet;
  r.renderUsage(fleet);
  assert.match(body.innerHTML, /aria-expanded="false" aria-controls="usage-card-global%3Acodex"/);
  assert.match(body.innerHTML, /ugroup-sum s-out">Weekly <b>100% used<\/b> · <span class="uphrase">limit reached/);
  assert.doesNotMatch(body.innerHTML, /class="ucols"/);
  assert.match(body.innerHTML, /id="usage-card-global%3Acodex" hidden><\/ul>/);
  r.toggleCard('global', 'codex');
  assert.equal(storage.get('hs-pi-dashboard:card-collapsed:global:codex'), '0');
  assert.match(body.innerHTML, /aria-expanded="true"/);
  assert.match(body.innerHTML, /class="urow s-out ulead"/);
  assert.match(body.innerHTML, /class="ucols"/);
});

test('no reporters keeps an explicit empty state', () => {
  const { r, body, summary } = renderer();
  r.renderUsage({ machines: [{ usage: null }] });
  assert.match(body.innerHTML, /no usage reporters online yet/);
  assert.equal(summary.textContent, 'waiting for agents');
});

test('cached forecasts count down from their fetch time instead of drifting', () => {
  const { r } = renderer();
  const fetched = new Date(Date.now() - H).toISOString();
  // Fetched an hour ago with 10h left: 50% -> 125% runs out 6h40m after fetch, so 5h40m from now.
  const cached = r.windowState({ label: 'Weekly', usedPercent: 50, projectedEnd: 125, resetAt: new Date(Date.now() + 9 * H).toISOString() }, 'Codex', fetched);
  assert.equal(Math.round(cached.runsOutMs / 60000), 340);
  const late = r.windowState({ label: 'Weekly', usedPercent: 50, projectedEnd: 125, resetAt: new Date(Date.now() + H).toISOString() }, 'Codex', new Date(Date.now() - 9 * H).toISOString());
  assert.equal(late.runsOutMs, 0);
  assert.equal(r.statusPhrase(late), 'likely at limit now');
  const future = r.windowState(gauge(50, 125), 'Codex', new Date(Date.now() + 5 * H).toISOString());
  assert.equal(Math.round(future.runsOutMs / 60000), 400);
});

test('a window without a reading is never reported as on pace', () => {
  const { r, summary } = renderer();
  r.renderUsage({ machines: [{ usage: { cards: [card({ gauges: [gauge(20, 40), gauge(null, null, { label: '5h' })] }), card({ key: 'x', label: 'Other', gauges: [gauge(null)] })] } }] });
  assert.match(summary.innerHTML, /uverdict s-unknown">Codex 5-hour has no reading <span class="umore"><span class="usep">· <\/span>1 more without a reading<\/span>/);
  assert.doesNotMatch(summary.innerHTML, /on pace/);
  r.renderUsage({ machines: [{ usage: { cards: [card({ gauges: [gauge(50, 125), gauge(null)] })] } }] });
  assert.match(summary.innerHTML, /s-risk">Codex weekly runs out in/);
});
