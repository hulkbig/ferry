import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import vm from 'node:vm';
import { HttpFerryAdapter, MockFerryAdapter } from './api.js';

const seen=[];
const adapter=new HttpFerryAdapter('',async(path,options={})=>{
 seen.push({path,body:options.body?JSON.parse(options.body):null});
 return new Response(JSON.stringify({waiting:2,running:1,connections:[],requests:[]}),{status:200});
});
assert.equal((await adapter.getQueue()).waiting,2);
assert.equal(seen[0].path,'/api/queue');
for(const status of ['received','waiting','active','recovering','streaming','delivering']){
 const r=adapter.mapRequest({id:'r',status,timestamp:new Date().toISOString(),duration_ms:12500,ttft_ms:1250});
 assert.equal(r.ttft,'1.25s');assert.equal(r.totalDuration,'—');assert.equal(r.elapsedDuration,'12.5s');
 assert.equal(r.live,true);assert.doesNotMatch(r.result,/Success|Failed/);
}
assert.equal(adapter.mapRequest({status:'success',outcome:'DIRECT_SUCCESS'}).live,false);
adapter.applyDraftMutation=async()=>{};adapter.getProvider=async()=>({connections:[{id:'c'}]});
await adapter.updateConnection('p','c',{concurrency:{max_concurrent:3}});
assert.deepEqual(seen.at(-1).body,{concurrency:{max_concurrent:3}});
await adapter.updateConnection('p','c',{concurrency:null});
assert.deepEqual(seen.at(-1).body,{concurrency:null});
await adapter.updateSettings({expected_revision:4,concurrency:{max_concurrent:2}});
assert.equal(seen.at(-1).body.expected_revision,4);

const mock=new MockFerryAdapter(),settings=await mock.getSettings();
await mock.updateSettings({expected_revision:settings.revision,concurrency:{max_concurrent:4,max_queue:8}});
assert.equal((await mock.getQueue()).defaults.max_concurrent,4);
await assert.rejects(()=>mock.updateSettings({expected_revision:settings.revision,concurrency:{max_concurrent:2}}));

const source=await fs.readFile(new URL('./app.part12.jsfrag',import.meta.url),'utf8');
const listeners={},writes=[],toasts=[],node={innerHTML:''};let renders=0;
const phases=['received','waiting','active','recovering','streaming','delivering'];
const q={generated_at:new Date().toISOString(),open_requests:phases.length,waiting:1,running:2,defaults:{max_concurrent:3,max_queue:8,max_wait_seconds:120},requests:phases.map((phase,i)=>({request_id:i?phase:'<request>',phase,started_at:new Date().toISOString(),logical_model_id:'m',provider_id:'p',connection_id:'c'})),connections:[{connection_id:'c',provider_id:'p',identity:'<img onerror=alert(1)>',enabled:true,policy_source:'global',runtime:{in_flight:2,waiting:1,max_concurrent:3,max_queue:8,max_wait_seconds:120}}]};
const provider={id:'p',name:'Provider P',raw:{concurrency:{max_concurrent:4,max_queue:9,max_wait_seconds:100}}};
let route={section:'queue'};
const context=vm.createContext({
 state:{auth:{authenticated:true},settings:{},providers:[provider],providerCache:new Map()},Date,Math,Number,FormData,encodeURIComponent,URLSearchParams,consoleLocale:'en-US',location:{hash:'#/queue'},
 app:{addEventListener:(name,fn)=>listeners[name]=fn,querySelector:()=>node},document:{hidden:false},
 currentRoute:()=>route,render:()=>renders++,localizeDOM:()=>{},setInterval:()=>{},queueMicrotask,
 nt:en=>en,esc:value=>String(value??'').replaceAll('&','&amp;').replaceAll('<','&lt;').replaceAll('"','&quot;'),badge:value=>String(value),
 showToast:(message,type)=>toasts.push({message,type}),
 adapter:{getQueue:async()=>q,getSettings:async()=>({revision:7,concurrency:{max_concurrent:2}}),updateSettings:async patch=>{writes.push(patch);return {revision:8,...patch};},getProvider:async()=>({provider,connections:[{id:'c',raw:{}}]}),updateConnection:async(...args)=>writes.push(args),updateProvider:async(...args)=>writes.push(args)},
});
const run=code=>vm.runInContext(code,context);
const requestSource=await fs.readFile(new URL('./app.part5.jsfrag',import.meta.url),'utf8');
run(requestSource.slice(requestSource.indexOf('function requestFilters('),requestSource.indexOf('function requestDataSync(')));
const requestQueries=[];context.adapter.getRequests=async filters=>{requestQueries.push(filters);return [];};
for(const phase of phases){
 context.state.requestFilter=phase;await run('reloadRequests()');
 assert.equal(requestQueries.at(-1).status,phase,'request list sends the selected live phase');
}
const appSource=await fs.readFile(new URL('./app.part11.jsfrag',import.meta.url),'utf8');
run(appSource.slice(appSource.indexOf('function requestKeySubtitle('),appSource.indexOf('\nfunction ',appSource.indexOf('function requestKeySubtitle(')+1)));
run(source.slice(source.indexOf('// Queue controls')));
await run('refreshQueue()');
let html=run('pageQueue()');
assert.match(html,/Open client requests<\/div><div class="metric-value">6<\/div>/);
for(const phase of phases)assert.ok(html.includes(phase[0].toUpperCase()+phase.slice(1)),phase+' is visible');
assert.match(html,/Upstream in flight/);assert.match(html,/href="#\/providers\/p">p<\/a><span class="queue-connection-separator" aria-hidden="true"> \/ <\/span><small>/);assert.match(html,/&lt;img/);assert.doesNotMatch(html,/<img/);assert.match(html,/%3Crequest%3E/);
assert.doesNotMatch(html,/data-queue-form|data-queue-action="configure"/,'monitoring must link to Settings instead of editing policy');
assert.match(html,/href="#\/settings\/concurrency\?scope=global"/);
assert.match(html,/href="#\/settings\/concurrency\?provider=p&amp;connection=c"/);
assert.equal(run("queueSettingsLink('provider/a&b','connection?x')"),'#/settings/concurrency?provider=provider%2Fa%26b&connection=connection%3Fx');
for(const label of ['Running / limit','Waiting / limit','Wait timeout','Policy / recovery','Configure'])assert.ok(html.includes('data-label="'+label+'"'),label);
const click=(action,provider='',connection='')=>listeners.click({target:{closest:()=>({dataset:{queueAction:action,provider,connection}})}});
route={section:'settings',id:'concurrency'};context.location.hash='#/settings/concurrency';
html=run('settingsConcurrency()');
assert.match(html,/Global defaults/);assert.match(html,/Provider P/);assert.match(html,/data-provider="p" data-connection="c"/);
await click('configure');
assert.match(run('queuePolicyForm()'),/Global concurrency defaults/);
listeners.input({target:{closest:()=>true,name:'max_concurrent',type:'number',value:'5'}});
await run('refreshQueue()');assert.equal(run('queueEditing.policy.max_concurrent'),5,'refresh preserves edits');
await run('saveQueuePolicy({max_concurrent:5,max_queue:10,max_wait_seconds:30,queue_when_full:true,queue_on_throttle:false})');
assert.equal(writes.at(-1).expected_revision,7);assert.equal(writes.at(-1).concurrency.max_concurrent,5);
await click('configure','p');assert.equal(run('queueEditing.policy.max_concurrent'),4,'provider override is loaded');
await click('inherit');assert.deepEqual(JSON.parse(JSON.stringify(writes.at(-1))),['p',{concurrency:null}]);
await click('configure','p','c');assert.equal(run('queueEditing.policy.max_concurrent'),4,'connection inherits provider override');
await click('inherit');assert.deepEqual(JSON.parse(JSON.stringify(writes.at(-1))),['p','c',{concurrency:null}]);
await click('configure');context.adapter.updateSettings=async()=>{throw new Error('revision conflict')};
listeners.input({target:{closest:()=>true,name:'max_concurrent',type:'number',value:'6'}});
await run('saveQueuePolicy(queueEditing.policy)');
assert.equal(toasts.at(-1).type,'error');assert.match(run('queuePolicyForm()'),/Global concurrency defaults/);assert.equal(run('queueEditing.policy.max_concurrent'),6,'CAS errors retain the draft');
await click('cancel');
context.location.hash='#/settings/concurrency?provider=p&connection=c';run('settingsConcurrency()');await new Promise(resolve=>setImmediate(resolve));
assert.equal(run('queueEditing.connection'),'c','deep links open the exact connection editor');
assert.equal(run('queueEditing.provider'),'p');
// Re-entering the same link must reopen the editor after it was canceled.
context.window={addEventListener:(name,handler)=>listeners[name]=handler};
context.applyReportLocation=()=>false;context.providerQuotaView='';
run(appSource.split('\n').find(line=>line.startsWith("window.addEventListener('hashchange'")));
await click('cancel');context.location.hash='#/queue';route={section:'queue'};listeners.hashchange();
context.location.hash='#/settings/concurrency?provider=p&connection=c';route={section:'settings',id:'concurrency'};listeners.hashchange();
run('settingsConcurrency()');await new Promise(resolve=>setImmediate(resolve));
assert.equal(run('queueEditing.connection'),'c','returning to the same Settings link opens its editor again');
// A policy fetch completing after navigation must not replace the next draft.
await click('cancel');let resolveProvider;
context.adapter.getProvider=()=>new Promise(resolve=>{resolveProvider=resolve;});
const pendingEditor=run("openQueueEditor('p','c')");
context.location.hash='#/queue';route={section:'queue'};listeners.hashchange();
resolveProvider({provider,connections:[{id:'c',raw:{}}]});await pendingEditor;
assert.equal(run('queueEditing'),null,'navigation discards an obsolete policy response');
console.log('PASS Queue API, all live phases, timings, Settings links, policy inheritance, revisions, draft persistence and escaping');
