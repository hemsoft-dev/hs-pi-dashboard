// Synthetic snapshots for issue #72 UI evidence. No credentials or provider I/O.
// Run from any directory: node scripts/codex-fixture.cjs [loopback-port]
const http = require('node:http');
const {readFileSync} = require('node:fs');
const path = require('node:path');
const root = path.join(__dirname, '../internal/web');
const states = new Set(['mismatch', 'matching', 'unknown']);
function snapshot(state, claudeState) {
  const at = new Date().toISOString();
  const weekly = {label:'Weekly',usedPercent:9,projectedEnd:63,windowSeconds:604800,
    resetAt:new Date(Date.now()+6*86400000).toISOString()};
  const paid = {key:'codex',label:'Codex',kind:'quota',fetchedAt:at,plan:'prolite',subscriptionPriceUsd:100,
    codex:{credentialSource:'cli',planSource:'usage-response',windows:['weekly']},gauges:[weekly]};
  const home = state === 'matching'
    ? {...paid,codex:{...paid.codex,credentialSource:'pi'}}
    : state === 'unknown'
      ? {...paid,plan:'unknown',codex:{credentialSource:'pi',planSource:'usage-response',windows:['30d']},gauges:[],error:'codex quota unavailable: verified paid plan required'}
      : {...paid,plan:'free',codex:{credentialSource:'pi',planSource:'usage-response',windows:['30d']},gauges:[],error:'codex quota unavailable: verified paid plan required'};
  const machine = (name, card) => ({name,online:true,generatedAt:at,sessions:[],activeSessions:0,usage:{machine:name,generatedAt:at,cards:[card]}});
  const fleet = {generatedAt:at,activeSessions:0,hermesSessions:[],machines:state === 'unknown'
    ? [machine('Home',home)] : [machine('Mini',paid),machine('Home',home)]};
  // Optional synthetic co-provider warning for summary-severity regressions.
  const claude = {out:[100,null],risk:[50,125],tight:[90,95]}[claudeState];
  if (claude) fleet.machines[0].usage.cards.push({key:'claude',label:'Claude',kind:'quota',fetchedAt:at,
    gauges:[{label:'Weekly',usedPercent:claude[0],projectedEnd:claude[1],windowSeconds:604800,
      resetAt:new Date(Date.now()+24*3600000).toISOString()}]});
  return fleet;
}
const assets = new Map(['herdr.css','herdr.js','projects.css','projects.js','billing.css','billing.js'].map(f=>['/assets/'+f,f]));
assets.set('/assets/smoothie-1.36.1.js','vendor/smoothie-1.36.1.js');
const server = http.createServer((req,res)=>{
  const url = new URL(req.url,'http://127.0.0.1');
  const referer = new URL(req.headers.referer || '/', 'http://127.0.0.1');
  const requested = (url.pathname === '/api/fleet' ? referer : url).searchParams.get('state');
  const state = states.has(requested) ? requested : 'mismatch';
  res.setHeader('Cache-Control','no-store');
  if(url.pathname === '/api/fleet') {
    res.setHeader('Content-Type','application/json');
    res.end(JSON.stringify(snapshot(state,referer.searchParams.get('claude'))));
  } else if(url.pathname === '/') {
    res.setHeader('Content-Type','text/html; charset=utf-8');
    // Only a provenance label is added. All rendering logic/styles are the
    // current embedded index.html, without mocks or DOM substitutions.
    const claude = ['out','risk','tight'].includes(url.searchParams.get('claude')) ? url.searchParams.get('claude') : '';
    res.end(readFileSync(path.join(root,'index.html'),'utf8').replace('</h1>',`</h1><span class="sub">Synthetic fixture · ${state}${claude ? ' · Claude '+claude : ''}</span>`));
  } else if(assets.has(url.pathname)) {
    res.setHeader('Content-Type',url.pathname.endsWith('.css') ? 'text/css' : 'text/javascript');
    res.end(readFileSync(path.join(root,assets.get(url.pathname))));
  } else if(url.pathname === '/favicon.ico') {res.statusCode=204;res.end();}
  else {res.statusCode=404;res.end();}
});
server.listen(Number(process.argv[2] || 49372),'127.0.0.1',()=>{
  console.log(`Synthetic Codex fixture: http://127.0.0.1:${server.address().port}/?state=mismatch`);
});
