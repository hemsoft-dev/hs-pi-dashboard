const assert = require('node:assert/strict');
const { test } = require('node:test');
const { readFileSync } = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

function renderer() {
  const html = readFileSync(path.join(__dirname, '../internal/web/index.html'), 'utf8');
  const start = html.indexOf('// Row signature tracks');
  const end = html.indexOf('let lastHermes =', start);
  assert.ok(start >= 0 && end > start, 'session renderer boundaries');
  const sandbox = vm.createContext({
    esc: (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c])),
    compactDur: () => '1s', durationOf: () => '1s',
  });
  vm.runInContext(html.slice(start, end), sandbox);
  return sandbox;
}

function claude(overrides = {}) {
  return { id: 'claude-code:shared', source: 'claude-code', project: 'dashboard', provider: 'claude-code', model: 'claude-opus-4-6', active: true, startedAt: new Date().toISOString(), lastActivity: new Date().toISOString(), ...overrides };
}

test('Claude and Pi rows coexist and active Claude is visible before output', () => {
  const r = renderer();
  const html = r.sessionsTable([{ name: 'home', online: true, sessions: [{ ...claude(), id: 'shared', source:'pi', provider: 'openai-codex' }, claude()] }], []);
  assert.match(html, /claude-code/);
  assert.match(html, /openai-codex/);
  assert.equal((html.match(/<tr class="active"/g) || []).length, 2);
});

test('Claude output can expand under its namespaced row and is escaped', () => {
  const r = renderer();
  vm.runInContext('expandedOutputs.add("home/claude-code:shared")', r);
  const html = r.sessionsTable([{ name: 'home', online: true, sessions: [claude({ outputs: [{ text: '<script>unsafe</script>', at: new Date().toISOString() }] })] }], []);
  assert.match(html, /data-skey="home\/claude-code:shared"/);
  assert.match(html, /&lt;script&gt;unsafe&lt;\/script&gt;/);
  assert.doesNotMatch(html, /<script>/);
});

test('offline Claude with no output does not appear as live work', () => {
  const r = renderer();
  const html = r.sessionsTable([{ name: 'home', online: false, sessions: [claude()] }], []);
  assert.doesNotMatch(html, /<tr class="active"/);
  assert.match(html, /no active sessions or sessions with output yet/);
});

function t3(overrides = {}) {
  return { ...claude(), id: 't3:env:thread', source: 't3', name: 'Fix <unsafe> title', provider: 'codex', status: 'idle', active: false, project: 'fleet-repo', toolCallCount: 7, ...overrides };
}

test('quiet T3 threads start compact and can expand statistics without output', () => {
  const r = renderer();
  const machines = [{ name: 'mini', online: true, sessions: [t3()] }];
  let html = r.sessionsTable(machines, []);
  assert.doesNotMatch(html, /t3-metrics|t3-details/);
  assert.match(html, /<button class="session-toggle"[^>]+aria-expanded="false"/);
  assert.match(html, /data-skey="mini\/t3:env:thread"/);
  vm.runInContext('expandedOutputs.add("mini/t3:env:thread")', r);
  html = r.sessionsTable(machines, []);
  assert.match(html, /aria-expanded="true" aria-controls="session-details-mini%2Ft3%3Aenv%3Athread"/);
  assert.match(html, /id="session-details-mini%2Ft3%3Aenv%3Athread"/);
  assert.match(html, /Fix &lt;unsafe&gt; title/);
  assert.match(html, /T3 Code<\/span> · <span class="session-state idle">idle/);
  assert.match(html, /fleet-repo/);
  assert.match(html, /<b>tools<\/b> 7/);
  assert.match(html, /<b>in<\/b> n\/a/);
  assert.match(html, /<b>context<\/b> n\/a \/ n\/a/);
  assert.doesNotMatch(html, /class="offline"/);
});

test('T3 reported zero is distinct from missing, and context is not total tokens', () => {
  const r = renderer();
  vm.runInContext('expandedOutputs.add("mini/t3:env:thread")', r);
  const html = r.sessionsTable([{ name: 'mini', online: true, sessions: [t3({t3Usage: {inputTokens: 1200, outputTokens: 0, totalTokens: null, contextTokens: 45, contextLimit: 200}})] }], []);
  assert.match(html, /<b>in<\/b> 1,200/);
  assert.match(html, /<b>out<\/b> 0/);
  assert.match(html, /<b>total<\/b> n\/a/);
  assert.match(html, /<b>context<\/b> 45 \/ 200/);
});

test('source failure and offline cached T3 metadata are visible without live rows', () => {
  const r = renderer();
  const machine = { name: 'mini', online: false, sources: [{source: 't3', state: 'healthy'}], sessions: [t3({active: true, status: 'working'})] };
  let html = r.sessionsTable([machine], []);
  assert.match(html, /T3 Code<\/span> · <span class="session-state unavailable">offline/);
  assert.match(html, /Cached sessions; machine unreachable/);
  assert.doesNotMatch(html, /class="active"/);
  html = r.sessionsTable([{name: 'air', online: true, sources: [{source: 't3', state: 'unavailable', error: '<database missing>'}], sessions: []}], []);
  assert.match(html, /T3 unavailable/);
  assert.match(html, /&lt;database missing&gt;/);
});

test('T3 state and usage updates flash and trigger Pulse only while live', () => {
  const r = renderer();
  const m = { name: 'mini', online: true, sessions: [t3({ active: true, status: 'working', t3Usage: {inputTokens: 1}})] };
  r.sessionsTable([m], []);
  vm.runInContext('sessionBaselineReady = true', r);
  m.sessions[0].t3Usage.inputTokens = 2;
  assert.match(r.sessionsTable([m], []), /class="active flash toggleable"/);
  assert.equal(vm.runInContext('activeFlashDetected', r), true);
  vm.runInContext('activeFlashDetected = false', r);
  m.sessions[0].active = false;
  m.sessions[0].status = 'stopped';
  r.sessionsTable([m], []);
  assert.equal(vm.runInContext('activeFlashDetected', r), false);
});

test('T3 duration uses the current or latest turn and stops growing offline', () => {
  const r = renderer();
  r.compactDur = ms => String(ms) + 'ms';
  const start = new Date(Date.now() - 10000).toISOString();
  const end = new Date(Date.now() - 5000).toISOString();
  const s = t3({turnStartedAt: start, turnCompletedAt: end});
  assert.equal(r.t3TurnDuration(s, true), '5s');
  assert.equal(r.t3TurnDuration(t3(), true), 'n/a');
  assert.equal(r.t3TurnDuration({...s, turnCompletedAt: null, lastActivity: end, active: true}, false), '5s');
});

test('Pulse counts live working/monitoring/connecting/waiting and excludes unavailable or offline rows', () => {
  const html = readFileSync(path.join(__dirname, '../internal/web/index.html'), 'utf8');
  const start = html.indexOf('function snapshotActiveSessionCount(');
  const end = html.indexOf('function cardCollapsed(', start);
  const r = vm.createContext({});
  vm.runInContext(html.slice(start,end),r);
  assert.equal(r.snapshotActiveSessionCount({machines: [
    {online:true,sessions:[t3({active:true,status:'working'}),t3({active:true,status:'waiting'}),t3({active:true,status:'connecting'}),t3({active:true,status:'monitoring'}),t3({status:'unavailable'})]},
    {online:false,sessions:[t3({active:true,status:'working'})]}],hermesSessions:[]}),4);
});

test('T3 notices identify and escape each configured environment path', () => {
  const r=renderer();
  const html=r.sessionsTable([{name:'mini',online:true,sessions:[],sources:[
    {source:'t3',state:'unavailable',error:'unreadable',path:'/first/<unsafe>/state.sqlite'},
    {source:'t3',state:'unavailable',error:'unreadable',path:'/second/state.sqlite'}]}],[]);
  assert.match(html,/\/first\/&lt;unsafe&gt;\/state.sqlite/);
  assert.match(html,/\/second\/state.sqlite/);
});

test('independent live Claude fallback identifies cached T3 statistics and unknown turn time', () => {
  const r=renderer();
  vm.runInContext('expandedOutputs.add("mini/t3:env:thread")', r);
  const html=r.sessionsTable([{name:'mini',online:true,sessions:[t3({active:true,status:'unavailable',activitySource:'claude-code'})]}],[]);
  assert.match(html,/session-state unavailable">unavailable/);
  assert.match(html,/Claude transcript active/);
  assert.match(html,/<b>statistics<\/b> cached T3/);
  assert.match(html,/<b>turn<\/b> n\/a/);
});

function appLabels(html) {
  return [...html.matchAll(/class="session-app">([^<]+)<\/span>/g)].map(m => m[1]);
}

test('application identity is independent of provider, model and generic machine grouping', () => {
  const r = renderer();
  const common = {project:'Shared engine',provider:'openai-codex',model:'gpt-6.1-sol',active:true};
  const html = r.sessionsTable([{name:'mini',online:true,sessions:[
    {...common,id:'pi-run',source:'pi'},
    {...common,id:'t3:env:thread',source:'t3',status:'working'},
    {...common,id:'claude-code:native',source:'claude-code'},
    {...common,id:'unknown',source:'future'},
    {...common,id:'legacy-unknown'},
    {...common,id:'prototype-key',source:'constructor'},
  ]}], [{id:'hermes-run',source:'cli',profile:'developer',model:common.model,active:true}]);
  assert.deepEqual(appLabels(html).sort(),['Claude Code','Hermes','Pi','T3 Code','Unknown app','Unknown app','Unknown app'].sort());
  assert.equal((html.match(/<tr class="active[ "]/g)||[]).length,7);
  assert.match(html,/openai-codex/);
  assert.match(html,/gpt-6.1-sol/);
});

test('app labels survive pagination, expanded output, polling and offline cache', () => {
  const r=renderer();
  const sessions=Array.from({length:21},(_,i)=>({id:'pi-'+i,source:'pi',project:'Pi '+i,active:true,lastActivity:new Date(Date.now()-i*1000).toISOString(),outputs:[{text:'Result',at:new Date().toISOString()}]}));
  sessions.push(t3({lastActivity:'2000-01-01T00:00:00Z'}));
  const machines=[{name:'mini',online:true,sessions}];
  assert.equal(appLabels(r.sessionsTable(machines,[])).length,20);
  vm.runInContext('currentPage=2; expandedOutputs.add("mini/pi-20")',r);
  for(let poll=0;poll<2;poll++) {
    const html=r.sessionsTable(machines,[]);
    assert.deepEqual(appLabels(html),['Pi','T3 Code']);
    assert.match(html,/page 2 \/ 2/);
    assert.match(html,/out-text/);
  }
  machines[0].online=false;
  assert.deepEqual(appLabels(r.sessionsTable(machines,[])),['Pi','T3 Code']);
  assert.equal(vm.runInContext('currentPage',r),2);
});

test('a merged T3 row retains its app and independent Claude activity notice', () => {
  const r=renderer();
  const html=r.sessionsTable([{name:'mini',online:true,sessions:[t3({active:true,activitySource:'claude-code',status:'unavailable',outputs:[{text:'Native Claude result',at:new Date().toISOString()}]})]}],[]);
  assert.deepEqual(appLabels(html),['T3 Code']);
  assert.match(html,/Claude transcript active/);
  assert.equal((html.match(/<tr class="active/g)||[]).length,1);
});


test('expanded T3 output and statistics update through polling and pagination, then collapse', () => {
  const r = renderer();
  const target = t3({ active: true, status: 'working', t3Usage: {inputTokens: 1}, outputs: [{text:'Retained output',at:new Date().toISOString()}] });
  const history = Array.from({length:21}, (_,i) => t3({id:'t3:history:'+i,lastActivity:'2000-01-01T00:00:00Z'}));
  const machines = [{name:'mini',online:true,sessions:[target,...history]}];
  vm.runInContext('expandedOutputs.add("mini/t3:env:thread")',r);
  let html = r.sessionsTable(machines,[]);
  assert.match(html, /<b>in<\/b> 1/);
  assert.match(html, /Retained output/);
  vm.runInContext('currentPage=2',r);
  assert.doesNotMatch(r.sessionsTable(machines,[]), /t3-metrics|Retained output/);
  target.t3Usage.inputTokens = 42;
  vm.runInContext('currentPage=1',r);
  for (let poll=0;poll<2;poll++) {
    html = r.sessionsTable(machines,[]);
    assert.match(html, /<b>in<\/b> 42/);
    assert.match(html, /Retained output/);
    assert.equal((html.match(/t3-metrics/g)||[]).length,1);
  }
  vm.runInContext('expandedOutputs.delete("mini/t3:env:thread")',r);
  html = r.sessionsTable(machines,[]);
  assert.doesNotMatch(html, /t3-metrics|Retained output/);
  assert.match(html, /aria-expanded="false"/);
});

test('quiet staleness is subdued and diagnostic details start closed', () => {
  const r=renderer();
  const html=r.sessionsTable([{name:'mini',online:true,sessions:[t3({status:'stale'})],sources:[{source:'t3',state:'stale',error:'1 quiet running thread; liveness unavailable',path:'/demo/state.sqlite'}]}],[]);
  const summary=html.match(/<summary[^>]*>(.*?)<\/summary>/s)[1];
  assert.match(summary,/1 stale/);
  assert.doesNotMatch(summary,/source-failure|quiet running|state.sqlite/);
  assert.match(html,/<details class="source-status">/);
  assert.match(html,/quiet running thread; liveness unavailable/);
  assert.match(html,/\/demo\/state.sqlite/);
  const document=readFileSync(path.join(__dirname,'../internal/web/index.html'),'utf8');
  assert.doesNotMatch(document,/\.session-state\.stale[^}]*#ffab91/);
});

test('source failure and offline summaries keep highlighted status, preserve details and open state', () => {
  const r=renderer();
  vm.runInContext('sourceDiagnosticsOpen=true',r);
  const html=r.sessionsTable([
    {name:'air',online:true,sessions:[],sources:[{source:'t3',state:'unavailable',error:'<database missing>',path:'/demo/<unsafe>/state.sqlite'}]},
    {name:'cached',online:false,sessions:[],sources:[{source:'t3',state:'healthy',path:'/demo/cached.sqlite'}]},
  ],[]);
  assert.match(html,/<details class="source-status" open>/);
  assert.match(html,/<span class="source-failure">1 unavailable<\/span>/);
  assert.match(html,/<span class="source-failure">1 offline<\/span>/);
  const summary=html.match(/<summary[^>]*>(.*?)<\/summary>/s)[1];
  assert.doesNotMatch(summary,/missing|state.sqlite|cached.sqlite/);
  assert.match(html,/&lt;database missing&gt;/);
  assert.match(html,/\/demo\/&lt;unsafe&gt;\/state.sqlite/);
  assert.match(html,/Cached sessions; machine unreachable/);
});

 test('monitoring remains compact and counts through a quiet interval, stop and disconnect', () => {
  const r = renderer();
  const rows = [0,1,2].map(i => t3({id:'t3:env:work-'+i,active:true,status:'working',lastActivity:'2020-01-01T00:00:00Z'}));
  rows.push(t3({id:'t3:env:monitor',active:true,status:'monitoring',lastActivity:'2020-01-01T00:00:00Z'}));
  const machine = {name:'mini',online:true,sessions:rows};
  let html = r.sessionsTable([machine], []);
  assert.equal((html.match(/<tr class="active[ "]/g)||[]).length,4);
  assert.match(html,/session-state monitoring">monitoring/);
  assert.doesNotMatch(html,/t3-metrics/);
  rows[0].active=false; rows[0].status='stopped';
  html = r.sessionsTable([machine], []);
  assert.equal((html.match(/<tr class="active[ "]/g)||[]).length,3);
  rows.forEach(row => {row.active=false;row.status='unavailable';});
  html = r.sessionsTable([machine], []);
  assert.doesNotMatch(html,/<tr class="active[ "]/);
  machine.online=false; rows.forEach(row => {row.active=true;});
  assert.doesNotMatch(r.sessionsTable([machine], []),/<tr class="active[ "]/);
});
