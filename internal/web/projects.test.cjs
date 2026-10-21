const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const view=require('./projects.js');
const document=JSON.parse(fs.readFileSync('internal/projects/testdata/gh-x-v0.22.0.json','utf8'));
test('renders exact producer verdicts, all tables and per-machine local health',()=>{
 const doc=structuredClone(document); doc.pullRequests.merged[0].aiReview='future verdict';
 const html=view.repository({name:'hemsoft-dev/hs-pi-dashboard',github:doc,locals:[{machine:'mini',path:'/mini',online:true,document:doc},{machine:'other',path:'/other',online:false,error:'timeout',document:doc}]});
 for(const text of ['Open pull requests','Open issues','Recently merged pull requests','Recent workflow runs','future verdict','mini','other','offline · cached','timeout','5 total'.replace('5 total','1 total')]) assert.ok(html.includes(text),text);
 assert.equal((html.match(/<h4>Open issues/g)||[]).length,1);
 assert.ok(html.includes('Attention'));
});
test('unavailable section does not look like empty success and invalid links are inert',()=>{
 const doc=structuredClone(document); doc.sections[0].status='unavailable';doc.sections[0].reason='<script>oops</script>';
 const html=view.repository({name:'test',github:doc,locals:[]});
 assert.ok(html.includes('unavailable issues'));assert.ok(html.includes('&lt;script&gt;'));assert.ok(!html.includes('<script>'));
 assert.equal(view.link('javascript:alert(1)','bad'),'bad');assert.equal(view.link('https://github.com.evil.example/test','bad'),'bad');
});
test('uses gh-x clean boolean, never infers cleanliness from counts',()=>{
 const doc=structuredClone(document); doc.health.branches.clean=false;doc.health.branches.local=1;
 assert.equal(view.localClean({online:true,document:doc}),false);
 assert.equal(view.localClean({online:true,document}),true);
});
