const {test}=require('node:test');const assert=require('node:assert/strict');const view=require('./billing.js');
const billing={organization:'example-org',month:'2026-10',credits:100,projectedCredits:200,creditGrossUsd:1,creditNetUsd:0,netUsd:0,grossUsd:10,projectedNetUsd:0,products:[{product:'Actions',netUsd:0,grossUsd:10,hasStopBudget:true,attention:false}],budgets:[{products:['actions'],amount:0,stopsUsage:true,scope:'organization',type:'ProductPricing'}],attention:false};
const card={label:'GitHub (example-org)',kind:'billing',billing,fetchedAt:new Date().toISOString()};
test('normal billing shows measured and projected values, money, scope and policy limits',()=>{const html=view.render(card);for(const text of ['100','200','Copilot AI credits','$10.00','$0.00','Yes','organization','not exposed by the API','No reported spend alert','including subscription SKUs reported by GitHub'])assert.ok(html.includes(text),text)});
test('attention remains visible when Copilot policy is unavailable',()=>{const html=view.render({...card,billing:{...billing,attention:true,products:[{product:'Copilot',netUsd:1,grossUsd:1,hasStopBudget:false,attention:true}]}});assert.ok(html.includes('Attention'));assert.ok(html.includes('None · attention'));assert.ok(html.includes('$1.00'));});
test('inline errors escape remote content and show attempt time',()=>{const html=view.render({label:'GitHub',error:'scope <script>secret</script>',fetchedAt:card.fetchedAt});assert.ok(html.includes('&lt;script&gt;'));assert.ok(!html.includes('<script>'));assert.ok(html.includes('Last attempt'));assert.ok(!html.includes('No reported spend alert'));});
test('empty month is a measured zero with no invented budget',()=>{const html=view.render({...card,billing:{...billing,credits:0,projectedCredits:0,products:[],budgets:[]}});assert.ok(html.includes('<b>0</b> Copilot'));assert.ok(html.includes('No configured budgets'));assert.ok(html.includes('No metered usage this month'));});

test('cached refresh failures and old readings stay explicit',()=>{
 const html=view.render({...card,error:'API unavailable'});assert.ok(html.includes('Refresh failed · cached'));assert.ok(html.includes('API unavailable'));
 const stale=view.render({...card,fetchedAt:'2020-01-01T00:00:00Z'});assert.ok(stale.includes('Stale reading'));
});

test('credit report ahead of the summary is shown without rewriting summary money',()=>{
 const b={...billing,creditNetUsd:1,creditGrossUsd:10,netUsd:0,grossUsd:2,attention:true};
 const html=view.render({...card,billing:b});
 assert.ok(html.includes('$0.00</b> net in usage summary'));
 assert.ok(html.includes('AI-credit report is ahead of the usage summary: Copilot $1.00 net / $10.00 gross'));
 assert.ok(html.includes('retain the summary API values'));
});
