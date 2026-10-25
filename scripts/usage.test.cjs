const assert = require('node:assert/strict');
const { test } = require('node:test');
const { readFileSync } = require('node:fs');
const vm = require('node:vm');
const path = require('node:path');

const H = 3600000;

function renderer(storage = new Map()) {
  const html = readFileSync(path.join(__dirname, '../internal/web/index.html'), 'utf8');
  const nodes = {
    'usage-body': { innerHTML: '', contains: () => false, querySelectorAll: () => [], querySelector: () => null },
    'usage-summary': { innerHTML: '', textContent: '' },
    'usage-provider-options': { innerHTML: '', dataset: {}, querySelectorAll: () => [] },
    'usage-provider-note': { textContent: '' },
  };
  const r = vm.createContext({
    BillingDashboard: require("../internal/web/billing.js"),
    localStorage: { getItem: key => storage.get(key) ?? null, setItem: (key, value) => storage.set(key, value) },
    document: { activeElement: null, getElementById: id => nodes[id] },
    esc: s => String(s ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c])),
    lastFleet: null,
  });
  vm.runInContext(html.slice(html.indexOf('function rel('), html.indexOf('let lastFleet = null;')), r);
  vm.runInContext(html.slice(html.indexOf('const USAGE_PROVIDERS_KEY'), html.indexOf('const USAGE_TOGGLE_KEY')), r);
  return { r, body: nodes['usage-body'], summary: nodes['usage-summary'], options: nodes['usage-provider-options'], storage };
}
function gauge(usedPercent, projectedEnd = null, extra = {}) {
  return { label: 'Weekly', usedPercent, projectedEnd, resetAt: new Date(Date.now() + 10 * H).toISOString(), ...extra };
}
function card(overrides = {}) {
  const result = { key: 'codex', label: 'Codex', kind: 'quota', fetchedAt: new Date().toISOString(), gauges: [gauge(50, 125)], ...overrides };
  // Existing generic card tests now model the canonical collector contract.
  if (result.key === 'antigravity') result.gauges = result.gauges?.map(g => {
    const id = g.bucketId ?? ({Weekly:'gemini-weekly','Gemini W':'gemini-weekly','Gemini 5h':'gemini-5h'}[g.label]);
    return {...g,bucketId:id,windowSeconds:g.windowSeconds ?? (id === 'gemini-5h' ? 18000 : 604800)};
  });
  return result;
}
const widthOf = (html, cls) => Number((html.match(new RegExp(`class="${cls}[^"]*" style="left:[\\d.]+%;width:([\\d.]+)%`)) || [])[1] ?? NaN);

test('window labels become readable words', () => {
  const { r } = renderer();
  assert.equal(r.windowLabel('Weekly'), 'Weekly');
  assert.equal(r.windowLabel('5h'), '5-hour');
  assert.equal(r.windowLabel('Monthly'), 'Monthly');
  assert.equal(r.windowLabel('Gemini W'), 'Gemini · weekly');
  assert.equal(r.windowLabel('3P 5h'), 'Third-party · 5-hour');
  // Claude collector labels carry no separator of their own.
  assert.equal(r.windowLabel('Sonnet weekly'), 'Sonnet · weekly');
  assert.equal(r.windowLabel('OAuth apps weekly'), 'OAuth apps · weekly');
  assert.equal(r.windowLabel('Claude Sonnet 4.5 weekly'), 'Claude Sonnet 4.5 · weekly');
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
  // 30s of slack keeps the countdown above 6h 40m even when the runner is
  // slow between building the fixture and rendering it.
  const w = r.windowState(gauge(50, 125, { resetAt: new Date(Date.now() + 10 * H + 30000).toISOString() }), 'Codex');
  assert.equal(w.status, 'risk');
  // 50% used, 125% at reset in ~10h: 50 more points at 7.5/h runs out in ~6h 40m.
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
    card({ key: 'antigravity', label: 'Antigravity', gauges: [gauge(0, null, { label: 'Gemini 5h' }), gauge(18, 112, { label: 'Gemini W' })] }),
    card({ gauges: [gauge(82, 131)] }),
    card({ key: 'moonshot', label: 'Moonshot AI', kind: 'balance', gauges: undefined, balanceUsd: 42.5 }),
  ] } }] };
  r.renderUsage(fleet);
  assert.ok(body.innerHTML.indexOf('>Codex<') < body.innerHTML.indexOf('>Antigravity<'));
  const ag = body.innerHTML.slice(body.innerHTML.indexOf('>Antigravity<'));
  assert.ok(ag.indexOf('Gemini · weekly') < ag.indexOf('Gemini · 5-hour'));
  assert.match(summary.innerHTML, /<span class="uverdict s-risk">Codex weekly runs out in [^<]+ <span class="umore"><span class="usep">· <\/span>1 more at risk<\/span><\/span>(?:<span class="usus[^"]*"[^>]*>[^<]*<\/span>)?<span class="usr">, <\/span><span class="ufresh">updated now<\/span>/);
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
  assert.match(body.innerHTML, /ugroup-sum s-out"><span>Weekly<\/span> <b>100% used ·<\/b> (?:<span class="usus[^"]*"[^>]*>[^<]*<\/span> )?<span class="uphrase">limit reached<\/span><\/span>/);
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

test('the header stamp describes the reading behind the verdict', () => {
  const { r, summary } = renderer();
  const old = new Date(Date.now() - 20 * 60000).toISOString();
  const fleet = (codex) => ({ machines: [{ usage: { cards: [codex, card({ key: 'moonshot', label: 'Moonshot AI', kind: 'balance', gauges: undefined, balanceUsd: 5 })] } }] });
  r.renderUsage(fleet(card({ gauges: [gauge(50, 125)], fetchedAt: old })));
  assert.match(summary.innerHTML, /<span class="ufresh stale">stale · 20m ago<\/span>/);
  assert.doesNotMatch(summary.innerHTML, /updated now/);
  r.renderUsage(fleet(card({ gauges: [gauge(50, 125)], fetchedAt: old, error: 'timeout' })));
  assert.match(summary.innerHTML, /<span class="ufresh cached">refresh failed · cached 20m ago<\/span>/);
  r.renderUsage(fleet(card({ gauges: [gauge(20, 40)] })));
  assert.match(summary.innerHTML, /all plans on pace<\/span>(?:<span class="usus[^"]*"[^>]*>[^<]*<\/span>)?<span class="usr">, <\/span><span class="ufresh">updated now<\/span>/);
});

test('collapsed summaries show the forecast that drives their status', () => {
  const { r, body, storage } = renderer();
  storage.set('hs-pi-dashboard:card-collapsed:global:codex', '1');
  r.renderUsage({ machines: [{ usage: { cards: [card({ gauges: [gauge(20, 90)] })] } }] });
  assert.match(body.innerHTML, /ugroup-sum s-tight"><span>Weekly<\/span> <b>20% used ·<\/b> <span>90% at reset ·<\/span> (?:<span class="usus[^"]*"[^>]*>[^<]*<\/span> )?<span class="uphrase">near limit<\/span><\/span>/);
});

test('verdicts without pace data keep their source and never claim on pace', () => {
  const { r, summary } = renderer();
  const old = new Date(Date.now() - 20 * 60000).toISOString();
  const balance = card({ key: 'moonshot', label: 'Moonshot AI', kind: 'balance', gauges: undefined, balanceUsd: 5 });
  r.renderUsage({ machines: [{ usage: { cards: [card({ gauges: [gauge(null)], fetchedAt: old, error: 'timeout' }), balance] } }] });
  assert.match(summary.innerHTML, /no quota readings<\/span>(?:<span class="usus[^"]*"[^>]*>[^<]*<\/span>)?<span class="usr">, <\/span><span class="ufresh cached">refresh failed · cached 20m ago/);
  r.renderUsage({ machines: [{ usage: { cards: [card({ gauges: [{ label: 'Weekly', usedPercent: 10 }] }), balance] } }] });
  assert.match(summary.innerHTML, /uverdict s-unknown">no forecasts yet<\/span>/);
  assert.doesNotMatch(summary.innerHTML, /on pace/);
  r.renderUsage({ machines: [{ usage: { cards: [card({ gauges: [gauge(0), gauge(20, 40)] })] } }] });
  assert.match(summary.innerHTML, /uverdict s-unknown">forecasts on pace <span class="umore"><span class="usep">· <\/span>1 without a forecast/);
  r.renderUsage({ machines: [{ usage: { cards: [card({ gauges: [gauge(20, 40)] })] } }] });
  assert.match(summary.innerHTML, /all plans on pace/);
});

test('an empty quota verdict is stamped from the quota cards, not a balance', () => {
  const { r, summary } = renderer();
  const old = new Date(Date.now() - 20 * 60000).toISOString();
  const balance = card({ key: 'moonshot', label: 'Moonshot AI', kind: 'balance', gauges: undefined, balanceUsd: 5 });
  r.renderUsage({ machines: [{ usage: { cards: [card({ gauges: undefined, error: 'parse failed', fetchedAt: old }), balance] } }] });
  assert.match(summary.innerHTML, /no quota readings<\/span>(?:<span class="usus[^"]*"[^>]*>[^<]*<\/span>)?<span class="usr">, <\/span><span class="ufresh cached">refresh failed · 20m ago<\/span>/);
  r.renderUsage({ machines: [{ usage: { cards: [card({ gauges: [], fetchedAt: old }), balance] } }] });
  assert.match(summary.innerHTML, /<span class="ufresh stale">stale · 20m ago<\/span>/);
  r.renderUsage({ machines: [{ usage: { cards: [balance] } }] });
  assert.match(summary.innerHTML, /no quota readings<\/span>(?:<span class="usus[^"]*"[^>]*>[^<]*<\/span>)?<span class="usr">, <\/span><span class="ufresh">updated now<\/span>/);
});

test('a refresh failure is stamped with the failed reading, not the oldest healthy one', () => {
  const { r, summary } = renderer();
  const hourOld = new Date(Date.now() - 60 * 60000).toISOString();
  const minuteOld = new Date(Date.now() - 60000).toISOString();
  r.renderUsage({ machines: [{ usage: { cards: [
    card({ gauges: [gauge(20, 40)], fetchedAt: hourOld }),
    card({ key: 'antigravity', label: 'Antigravity', gauges: [gauge(10, 30),gauge(5,10,{label:'Gemini 5h'})], fetchedAt: minuteOld, error: 'timeout' }),
  ] } }] });
  assert.match(summary.innerHTML, /<span class="ufresh cached">refresh failed · cached 60s ago<\/span>/);
});

test('provider choices remove disabled readings, failures and freshness from all summaries', () => {
  const { r, body, summary, options, storage } = renderer();
  const fleet = { machines: [{ usage: { cards: [
    card({ key: 'antigravity', label: 'Antigravity', error: 'timeout', fetchedAt: new Date(Date.now() - 60 * 60000).toISOString(), gauges: [gauge(100)] }),
    card({ key: 'claude', label: 'Claude', gauges: [gauge(20, 40)] }),
    card({ gauges: [gauge(30, 50)] }),
    card({ key: 'moonshot', label: 'Moonshot AI', kind: 'balance', gauges: undefined, balanceUsd: 5 }),
  ] } }, { usage: { cards: [card({ key: 'claude', label: 'Claude', gauges: [gauge(20, 40)] })] } }] };
  r.lastFleet = fleet;
  r.renderUsage(fleet);
  assert.match(body.innerHTML, /Antigravity/);
  assert.doesNotMatch(body.innerHTML, />Claude</);
  assert.match(options.innerHTML, /data-usage-provider="claude"><span>Claude/);
  r.setUsageProvider('antigravity', false);
  r.setUsageProvider('claude', true);
  assert.doesNotMatch(body.innerHTML, /Antigravity|timeout|ufail/);
  assert.equal((body.innerHTML.match(/class="ucard-label">Claude/g) || []).length, 1);
  assert.match(summary.innerHTML, /all plans on pace.*updated now/);
  assert.doesNotMatch(summary.innerHTML, /cached|Antigravity|limit reached/);
  assert.match(body.innerHTML, /Moonshot AI.*\$5.00/);
  const reloaded = renderer(storage);
  reloaded.r.renderUsage(fleet);
  assert.doesNotMatch(reloaded.body.innerHTML, /Antigravity/);
  assert.match(reloaded.body.innerHTML, />Claude</);
  assert.match(reloaded.options.innerHTML, /data-usage-provider="claude" checked/);
  r.setUsageProvider('antigravity', true);
  assert.match(body.innerHTML, /Antigravity/);
  r.setUsageProvider('moonshot', false);
  assert.doesNotMatch(body.innerHTML, /Moonshot AI|Prepaid balances/);
});

test('a switched-on provider that no agent reports is listed as not reporting', () => {
  const { r, body, summary } = renderer();
  const fleet = { machines: [{ usage: { cards: [card({ gauges: [gauge(30, 50)] })] } }] };
  r.lastFleet = fleet;
  r.renderUsage(fleet);
  assert.doesNotMatch(body.innerHTML, /ufail|Claude/);
  r.setUsageProvider('claude', true);
  assert.match(body.innerHTML, /Not reporting:<\/b> Claude.*<b>Claude<\/b> no agent reports it yet; update agents to enable it/);
  assert.match(body.innerHTML, /class="ucard-label">Codex/);
  assert.match(summary.innerHTML, /all plans on pace.*updated now/);
  r.setUsageProvider('claude', false);
  assert.doesNotMatch(body.innerHTML, /ufail|Claude/);
});

test('all-disabled state survives reload and keeps every choice available without reporters', () => {
  const { r, storage, body, options, summary } = renderer();
  for (const provider of r.usageProviderCatalog({})) r.setUsageProvider(provider.key, false);
  r.renderUsage({ machines: [] });
  assert.match(body.innerHTML, /No providers selected.*Choose providers/);
  assert.equal(summary.textContent, 'no providers selected');
  assert.equal((options.innerHTML.match(/type="checkbox"/g) || []).length, 7);
  assert.doesNotMatch(options.innerHTML, / checked/);
  const reload = renderer(storage);
  reload.r.renderUsage({});
  assert.match(reload.body.innerHTML, /No providers selected/);
  reload.r.setUsageProvider('claude', true);
  reload.r.renderUsage({ machines: [{ usage: { cards: [card()] } }] });
  assert.match(reload.body.innerHTML, /Selected providers have not reported/);
});

test('malformed preferences use defaults and storage failure still permits changes for this page', () => {
  for (const value of ['not-json', 'null', 'false', '[]', '2', '{"codex":"false","claude":1}']) {
    const { r } = renderer(new Map([['hs-pi-dashboard:usage-providers:v1', value]]));
    assert.equal(r.usageProviderEnabled('codex'), true);
    assert.equal(r.usageProviderEnabled('claude'), false);
  }
  const { r, body, options } = renderer();
  r.localStorage.setItem = () => { throw new Error('quota exceeded'); };
  r.setUsageProvider('claude', true);
  r.renderUsage({ machines: [{ usage: { cards: [card({ key: 'claude', label: 'Claude' })] } }] });
  assert.match(body.innerHTML, />Claude</);
  assert.match(options.innerHTML, /data-usage-provider="claude" checked/);
});


test('billing provider selection filters alerts before the shared header and preserves quota data', () => {
 const {r,body,summary}=renderer();
 const billing={key:'github-billing:example-org',label:'GitHub (example-org)',kind:'billing',error:'admin:org required',fetchedAt:new Date().toISOString()};
 const fleet={machines:[{name:'mini',online:true,usage:{cards:[card(),billing]}}]};
 r.lastFleet=fleet;r.renderUsage(fleet);
 assert.match(body.innerHTML,/ubilling/);assert.match(summary.innerHTML,/GitHub.*unavailable/);assert.match(summary.innerHTML,/uverdict s-risk/);
 r.setUsageProvider(billing.key,false);
 assert.doesNotMatch(body.innerHTML,/ubilling/);assert.doesNotMatch(summary.innerHTML,/GitHub/);assert.match(body.innerHTML,/Codex/);
 r.setUsageProvider(billing.key,true);
 assert.match(body.innerHTML,/admin:org required/);
});


test('healthy billing cannot hide failed or empty selected quota cards', () => {
 for (const error of ['quota API unavailable', undefined]) {
  const {r,summary}=renderer();
  const quota=card({gauges:[],error});
  const billing={key:'github-billing:example-org',label:'GitHub',kind:'billing',fetchedAt:new Date().toISOString(),billing:{month:'2026-10',netUsd:0,products:[],budgets:[]}};
  r.renderUsage({machines:[{usage:{cards:[quota,billing]}}]});
  assert.match(summary.innerHTML,/uverdict s-risk/);assert.match(summary.innerHTML,/Quota not reporting: Codex/);assert.match(summary.innerHTML,/refresh failed/);
 }
});

test('billing header uses its cadence while quota freshness stays unchanged',()=>{
 const {r}=renderer();
 const at=new Date(Date.now()-14*60000).toISOString();
 r.readings=[{fetchedAt:at,freshForSeconds:1080}];
 assert.doesNotMatch(vm.runInContext('headerStamp(readings, readings)',r),/stale/);
 r.readings=[{fetchedAt:at}];
 assert.match(vm.runInContext('headerStamp(readings, readings)',r),/stale/);
});

test('fallback freshness consistently uses the newest balance reading',()=>{
 const {r}=renderer();
 const cards=[{fetchedAt:new Date(Date.now()-20*60000).toISOString()},{fetchedAt:new Date().toISOString()}];
 assert.doesNotMatch(r.headerStamp([],cards), /stale/);
 assert.match(r.headerStamp([],cards), /updated now/);
});

test('previous-month fleet billing cannot override a current-month failure',()=>{
 const {r,body,summary}=renderer();
 const oldMonth=new Date(Date.UTC(new Date().getUTCFullYear(),new Date().getUTCMonth()-1,1)).toISOString().slice(0,7);
 const old={key:'github-billing:example-org',kind:'billing',label:'GitHub',fetchedAt:new Date(Date.now()-H).toISOString(),billing:{month:oldMonth,netUsd:999,products:[],budgets:[]}};
 const current={...old,fetchedAt:new Date().toISOString(),billing:undefined,error:'awaiting current UTC month'};
 for(const cards of [[old,current],[current,old],[old]]) {
  r.renderUsage({machines:cards.map(c=>({usage:{cards:[c]}}))});
  assert.doesNotMatch(body.innerHTML,/999/);
  assert.match(body.innerHTML,/current UTC month/);
  assert.match(summary.innerHTML,/unavailable/);
 }
 assert.equal(old.billing.netUsd,999);
});

test('billing alerts carry their own freshness alongside fresh quota',()=>{
 const {r,summary}=renderer();
 const billing={key:'github-billing:example-org',kind:'billing',label:'GitHub',freshForSeconds:1080,fetchedAt:new Date(Date.now()-20*60000).toISOString(),billing:{month:new Date().toISOString().slice(0,7),attention:true,products:[],budgets:[]}};
 const quota=card({gauges:[gauge(20,40)]});
 r.renderUsage({machines:[{usage:{cards:[quota,billing]}}]});
 assert.match(summary.innerHTML,/stale · 20m ago/);
 r.renderUsage({machines:[{usage:{cards:[quota,{...billing,error:'access unavailable'}]}}]});
 assert.match(summary.innerHTML,/refresh failed · cached 20m ago/);
});

function scoredGauge(projected,ageHours=96,used=40,label='Weekly') {
 return {label,usedPercent:used,projectedEnd:projected,windowSeconds:168*3600,resetAt:new Date(Date.now()+(168-ageHours)*H).toISOString()};
}
test('SUS follows shared projections, weekly age floor and band boundaries',()=>{
 const {r}=renderer();
 const at=new Date().toISOString();
 for(const [projected,want] of [[0,0],[74,74],[75,75],[89,89],[90,90],[100,100]]) {
  const w=r.windowState(scoredGauge(projected), 'Codex',at,'','codex');
  assert.equal(w.sus,want);
 }
 const over=r.windowState(scoredGauge(132),'Codex',at,'','codex');
 const expected=Math.round(100-100*(over.remaining-over.runsOutMs)/(168*H));
 assert.equal(over.sus,expected);
 assert.equal(r.windowState(scoredGauge(0,11),'Codex',at,'','codex').sus,null);
 assert.equal(r.windowState({...scoredGauge(0,12),resetAt:new Date(Date.parse(at)+156*H).toISOString()},'Codex',at,'','codex').sus,0);
 assert.equal(r.windowState(scoredGauge(90,96,40,'5h'),'Codex',at,'','codex').susEligible,false);
 assert.equal(r.windowState(scoredGauge(90),'Codex',at,'failed','codex').sus,null);
 assert.match(r.susMarkup(90),/usus green/);assert.match(r.susMarkup(75),/usus yellow/);assert.match(r.susMarkup(74),/usus red/);
});
test('Fleet SUS weights each subscription once and excludes unavailable scores',()=>{
 const {r}=renderer();
 const groups=[
  {card:{key:'claude',subscriptionPriceUsd:200},windows:[{sus:100}]},
  {card:{key:'codex',subscriptionPriceUsd:200},windows:[{sus:50}]},
  {card:{key:'antigravity',subscriptionPriceUsd:19.99},windows:[{sus:90,bucketId:'gemini-weekly'},{sus:100,bucketId:'3p-weekly'}]},
 ];
 assert.equal(r.fleetSubscriptionScore(groups),76);
 groups[0].card.subscriptionPriceUsd=null;
 assert.equal(r.fleetSubscriptionScore(groups),80);
 groups[0].windows[0].sus=null;
 assert.equal(r.fleetSubscriptionScore(groups),54);
 assert.equal(r.fleetSubscriptionScore([]),null);
});
test('weekly SUS is shown next to bars, collapsed summaries and filtered fleet figure',()=>{
 const {r,body,summary}=renderer();
 r.setUsageProvider('claude',true);
 const fleet={machines:[{usage:{cards:[card({gauges:[scoredGauge(90)]}),card({key:'claude',label:'Claude',gauges:[scoredGauge(50)]})]}}]};
 r.renderUsage(fleet);
 assert.match(body.innerHTML,/SUS 90/);assert.match(summary.innerHTML,/Fleet SUS 70/);
 r.setUsageProvider('claude',false);r.renderUsage(fleet);
 assert.match(summary.innerHTML,/Fleet SUS 90/);
 r.lastFleet=fleet;r.toggleCard('global','codex');r.renderUsage(fleet);
 assert.match(body.innerHTML,/ugroup-sum[^]*SUS 90/);
});
test('Grok aggregates observed seven-day turn IDs without inventing quota or SUS',()=>{
 const {r,body}=renderer();
 const at=new Date().toISOString();
 const activity={sourcesAvailable:1,scope:'T3 + Grok CLI',events:[{id:'same',at}]};
 const grok={key:'grok',label:'Grok / X',kind:'activity',fetchedAt:at,activity};
 r.renderUsage({machines:[{usage:{cards:[grok]}},{usage:{cards:[{...grok,activity:{...activity,events:[{id:'same',at},{id:'old',at:'2020-01-01'}]}}]}}]});
 assert.match(body.innerHTML,/1 turn\/7d/);
 assert.match(body.innerHTML,/SUS n\/a/);
 assert.doesNotMatch(body.innerHTML,/%|ubar/);
 r.setUsageProvider('grok',false);r.renderUsage({machines:[{usage:{cards:[grok]}}]});
 assert.doesNotMatch(body.innerHTML,/turn\/7d/);
});

test('cached SUS preserves lockout already elapsed after projected run-out',()=>{
 const {r}=renderer();
 const now=Date.now();
 const at=new Date(now-96*H).toISOString();
 const g={label:'Weekly',usedPercent:60,projectedEnd:300,windowSeconds:168*3600,resetAt:new Date(now+24*H).toISOString()};
 const first=r.windowState(g,'Codex',at,'','codex');
 assert.equal(first.runsOutMs,0);
 assert.equal(first.sus,40);
 r.fixedNow=now+H;
 vm.runInContext('Date.now=()=>fixedNow',r);
 assert.equal(r.windowState(g,'Codex',at,'','codex').sus,40);
});

test('exhausted readings infer the earlier crossing from the uncapped projection',()=>{
 const {r}=renderer();
 const now=Date.now();
 const at=new Date(now).toISOString();
 // Raw 120% at day 5 projects to 168%; the crossing was at day 4 1/6.
 const g={label:'Weekly',usedPercent:100,projectedEnd:168,windowSeconds:168*3600,resetAt:new Date(now+48*H).toISOString()};
 const w=r.windowState(g,'Codex',at,'','codex');
 assert.equal(w.sus,60);
 assert.ok(Math.abs(w.runsOutAt-(now-20*H))<5);
 assert.equal(w.runsOutMs,0);
});
test('fleet weighting retains fractional provider means until the final score',()=>{
 const {r}=renderer();
 const groups=[{card:{key:'antigravity',subscriptionPriceUsd:null},windows:[{sus:74.5,bucketId:'gemini-weekly'},{sus:100,bucketId:'3p-weekly'}]},
  {card:{key:'codex',subscriptionPriceUsd:200},windows:[{sus:74}]}];
 assert.equal(r.fleetSubscriptionScore(groups),74);
 assert.match(r.susMarkup(r.fleetSubscriptionScore(groups),'Fleet SUS'),/usus red/);
});
test('live legacy quota warnings do not claim cached or failed refresh',()=>{
 const {r,body}=renderer();
 r.renderUsage({machines:[{usage:{cards:[card({key:'antigravity',label:'Antigravity',warning:'CLI weekly unavailable; live legacy pools',gauges:[scoredGauge(90,96,40,'Gemini W')]})]}}]});
 assert.match(body.innerHTML,/live legacy pools/);
 assert.match(body.innerHTML,/SUS n\/a/);
 assert.doesNotMatch(body.innerHTML,/cached|Refresh failing|refresh failed/);
});
test('current partial or unavailable Grok activity never claims cached values',()=>{
 const {r,body}=renderer();
 for(const sourcesAvailable of [0,1]) {
  r.renderUsage({machines:[{usage:{cards:[{key:'grok',label:'Grok / X',kind:'activity',fetchedAt:new Date().toISOString(),error:'T3 activity unavailable',activity:{sourcesAvailable,scope:'Observed',events:[]}}]}}]});
  assert.match(body.innerHTML,/T3 activity unavailable/);
  assert.doesNotMatch(body.innerHTML,/cached|refresh failed/);
 }
});
test('fleet activity keeps the latest valid duplicate timestamp in either target order',()=>{
 const {r,body}=renderer();
 const recent=new Date(Date.now()-H).toISOString();
 const old=new Date(Date.now()-169*H).toISOString();
 const mk=at=>({usage:{cards:[{key:'grok',label:'Grok / X',kind:'activity',fetchedAt:recent,activity:{sourcesAvailable:1,scope:'Observed',events:[{id:'same',at}]}}]}});
 for(const times of [[recent,old],[old,recent],[recent,'invalid'],['invalid',recent]]) {
  r.renderUsage({machines:times.map(mk)});
  assert.match(body.innerHTML,/1 turn\/7d/);
 }
});

test('activity unions retain oldest contributor freshness and offline coverage',()=>{
 const {r,body}=renderer();
 const now=Date.now(),at=new Date(now-H).toISOString(),fresh=new Date(now).toISOString();
 const mk=(fetchedAt,sourcesAvailable,events,online=true)=>({online,usage:{cards:[{key:'grok',label:'Grok / X',kind:'activity',fetchedAt,activity:{sourcesAvailable,scope:'Observed',events}}]}});
 const old=mk(at,1,[{id:'old',at}],false);
 for(const current of [mk(fresh,0,[]),mk(fresh,1,[{id:'fresh',at:fresh}])]) {
  for(const machines of [[old,current],[current,old]]) {
   r.renderUsage({machines});
   assert.match(body.innerHTML,/stale · 60m ago/);
   assert.match(body.innerHTML,/offline agent/);
   assert.doesNotMatch(body.innerHTML,/cached/);
  }
 }
});

// Synthetic mixed/cached fleet contract. Provenance: internal/usage/testdata/antigravity/README.md.
test('Antigravity mixed pools cannot drive the board, warning or Fleet SUS',()=>{
 const {r,body,summary}=renderer();
 const at=new Date().toISOString();
 const gemini={...scoredGauge(47,72,20,'Untrusted display text'),bucketId:'gemini-weekly'};
 const third={...scoredGauge(214,120,92,'3P W'),bucketId:'3p-weekly'};
 const argon={...scoredGauge(163,96,70,'Argon W'),bucketId:'gemini-argon-wtu-weekly'};
 const short={...gauge(10,17,{label:'Gemini 5h',windowSeconds:18000,resetAt:new Date(Date.now()+2*H).toISOString()}),bucketId:'gemini-5h'};
 r.renderUsage({machines:[{usage:{cards:[card({key:'antigravity',label:'Antigravity',fetchedAt:at,gauges:[third,argon,gemini,short]})]}}]});
 assert.match(body.innerHTML,/Gemini · weekly/);
 assert.match(body.innerHTML,/20%/);assert.match(body.innerHTML,/47%/);
 assert.match(body.innerHTML,/SUS 47/);assert.match(summary.innerHTML,/Fleet SUS 47/);
 assert.match(summary.innerHTML,/all plans on pace/);
 assert.doesNotMatch(body.innerHTML,/Third-party|Argon|92%|70%|214%|163%|Untrusted display text/);
});

test('missing or malformed Gemini stays unavailable despite healthy unrelated pools',()=>{
 const valid={...scoredGauge(47,72,20,'Gemini W'),bucketId:'gemini-weekly'};
 const unrelated={...scoredGauge(99,96,90,'Gemini W'),bucketId:'3p-weekly'};
 const cases=[[],[unrelated],[{...valid,bucketId:undefined}], // old agent, label alone
  [{...valid,disabled:true}],[valid,{...valid}],
  [{...valid,usedPercent:null}],[{...valid,usedPercent:'20'}],
  [{...valid,usedPercent:-1}],[{...valid,usedPercent:101}],
  [{...valid,resetAt:'invalid'}],[{...valid,resetAt:new Date(Date.now()-H).toISOString()}],
  [{...valid,windowSeconds:18000}]];
 for(const gauges of cases){
  const {r,body,summary}=renderer();
  const ag={key:'antigravity',label:'Antigravity',kind:'quota',fetchedAt:new Date().toISOString(),gauges:[...gauges,unrelated]};
  const fleet={machines:[{usage:{cards:[ag]}}]};
  r.renderUsage(fleet);
  assert.match(body.innerHTML,/Gemini · weekly/);assert.match(body.innerHTML,/unavailable/);
  assert.match(body.innerHTML,/SUS n\/a/);assert.match(summary.innerHTML,/Fleet SUS n\/a/);
  assert.doesNotMatch(body.innerHTML,/90%|99%|20%|0%<span class="ulbl"> used|refresh failed · cached/);
  assert.doesNotMatch(summary.innerHTML,/all plans on pace|near limit|runs out|limit reached/);
  r.lastFleet=fleet;r.toggleCard('global','antigravity');r.renderUsage(fleet);
  assert.match(body.innerHTML,/ugroup-sum[^]*no reading/);
  assert.doesNotMatch(body.innerHTML,/90%|99%|20%/);
 }
});

test('partial Gemini windows keep valid metrics and only the eligible weekly SUS',()=>{
 const at=new Date().toISOString();
 const weekly={...scoredGauge(47,72,20,'Unrelated label'),bucketId:'gemini-weekly'};
 const short={...gauge(10,17,{label:'Unrelated label',windowSeconds:18000,resetAt:new Date(Date.now()+2*H).toISOString()}),bucketId:'gemini-5h'};
 for(const gauges of [[weekly],[weekly,{...short,usedPercent:'invalid'}],[short],[short,{...weekly,disabled:true}]]){
  const {r,body,summary}=renderer();
  r.renderUsage({machines:[{usage:{cards:[{key:'antigravity',label:'Antigravity',kind:'quota',fetchedAt:at,gauges}]}}]});
  assert.match(body.innerHTML,/unavailable/);
  if(gauges[0]===weekly){
   assert.match(body.innerHTML,/20%/);assert.match(body.innerHTML,/47%/);
   assert.match(summary.innerHTML,/Fleet SUS 47/);
  }else{
   assert.match(body.innerHTML,/10%/);assert.match(summary.innerHTML,/Fleet SUS n\/a/);
  }
  assert.doesNotMatch(body.innerHTML,/Unrelated label/);
 }
});

test('fleet selection cannot prefer a fresh old-agent or 3P-only card over Gemini evidence',()=>{
 const {r,body,summary}=renderer();
 const at=Date.now();
 const good={key:'antigravity',label:'Antigravity',kind:'quota',fetchedAt:new Date(at-60000).toISOString(),gauges:[{...scoredGauge(47,72,20,'Gemini W'),bucketId:'gemini-weekly'}]};
 for(const gauges of [[scoredGauge(200,96,90,'Gemini W')],[{...scoredGauge(200,96,90,'3P W'),bucketId:'3p-weekly'}]]){
  const bad={...good,fetchedAt:new Date(at).toISOString(),gauges};
  for(const cards of [[good,bad],[bad,good]]){
   r.renderUsage({machines:cards.map(c=>({usage:{cards:[c]}}))});
   assert.match(body.innerHTML,/20%/);assert.match(summary.innerHTML,/Fleet SUS 47/);
   assert.doesNotMatch(body.innerHTML,/90%|200%|Third-party/);
  }
 }
 // A cached genuine Gemini reading may remain visible after refresh failure,
 // but no SUS can be asserted for that failed reading.
 r.renderUsage({machines:[{usage:{cards:[{...good,error:'timeout',gauges:[...good.gauges,{...scoredGauge(200,96,90,'3P W'),bucketId:'3p-weekly'}]}]}}]});
 assert.match(body.innerHTML,/20%/);assert.match(body.innerHTML,/refresh failed · cached/);
 assert.match(summary.innerHTML,/Fleet SUS n\/a/);assert.doesNotMatch(body.innerHTML,/90%|200%|Third-party/);
});

test('Antigravity blended balances and unrelated weekly scores cannot enter Fleet SUS',()=>{
 const {r,body,summary}=renderer();
 r.renderUsage({machines:[{usage:{cards:[{key:'antigravity',label:'Antigravity',kind:'balance',balanceUsd:900,fetchedAt:new Date().toISOString()}]}}]});
 assert.match(body.innerHTML,/unavailable/);assert.doesNotMatch(body.innerHTML,/\$900/);
 assert.match(summary.innerHTML,/Fleet SUS n\/a/);
 const third=r.windowState({...scoredGauge(90,96,80,'Gemini W'),bucketId:'3p-weekly'},'Antigravity',new Date().toISOString(),'','antigravity');
 assert.equal(third.sus,null);assert.equal(third.susEligible,false);
 assert.equal(r.fleetSubscriptionScore([{card:{key:'antigravity'},windows:[{sus:100,bucketId:'3p-weekly'},{sus:80,bucketId:'gemini-argon-wtu-weekly'}]}]),null);
});
