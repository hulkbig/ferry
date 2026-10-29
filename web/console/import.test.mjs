import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import test from 'node:test';
import vm from 'node:vm';

const settingsSource=await fs.readFile(new URL('./app.part6.jsfrag',import.meta.url),'utf8');
const source=settingsSource.split('// End of Settings import flow.')[0];
const file=(text,name='accounts.json')=>({name,size:text.length,text:async()=>text});
const ready={items:[{id:'one',name:'<img src=x onerror=alert(1)>',kind:'api_key',status:'ready'},{id:'two',name:'Existing',status:'exists'},{id:'three',name:'Invalid',status:'unsupported'}],warnings:['Review first']};
function harness(reply=async()=>ready){
 const listeners={},calls=[],nodes=[];let route={section:'settings',id:'import'},renders=0;
 const context=vm.createContext({
  state:{auth:{authenticated:true},providers:[],models:[],apiKeys:[],providerCache:new Map(),modelCache:new Map()},
  app:{addEventListener:(type,fn)=>(listeners[type]||=[]).push(fn),querySelectorAll:()=>nodes,querySelector:()=>({remove(){}})},
  window:{addEventListener:(type,fn)=>listeners[type]=[fn]},
  currentRoute:()=>route,render:()=>{renders++;},demoMode:false,nt:en=>en,
  esc:value=>String(value??'').replace(/[&<>'"]/g,ch=>({'&':'&amp;','<':'&lt;','>':'&gt;',"'":'&#39;','"':'&quot;'}[ch])),icon:()=>'<svg></svg>',showToast:()=>{},
  adapter:{rawProvidersPromise:Promise.resolve({providers:[]}),request:async(path,options)=>{calls.push({path,body:JSON.parse(options.body)});return reply(path,options);},getProviders:async function(){assert.equal(this.rawProvidersPromise,null,'import must invalidate the cached provider catalog');return [{id:'imported'}];},getModels:async()=>[],getApiKeys:async()=>[{id:'key'}],getRevisions:async()=>[],getSettings:async()=>({revision:8})},
 });
 vm.runInContext(source,context);
 const run=code=>vm.runInContext(code,context);
 return {context,calls,nodes,run,route(value){route=value;run('syncImportRoute()');},file(key,value){context.upload=value;run(`importState.files.${key}=upload`);},event(type,target){for(const fn of listeners[type]||[])fn({target,preventDefault(){}});},renders:()=>renders};
}
const snapshot=h=>JSON.parse(h.run('JSON.stringify(importState)'));

test('Settings background pricing refresh waits for administrator login',async()=>{
 let calls=0;
 const context=vm.createContext({state:{auth:{authenticated:false},settingsTab:'import'},
  adapter:{getPricingStatus:async()=>{calls++;return {total:10};}},
  currentRoute:()=>({section:'settings'}),document:{activeElement:null},render(){},showToast:assert.fail});
 const start=settingsSource.indexOf('async function refreshPricingStatus()');
 vm.runInContext(settingsSource.slice(start,settingsSource.indexOf('\nfunction settingsBackup()',start)),context);
 await vm.runInContext('refreshPricingStatus()',context);assert.equal(calls,0);
 context.state.auth.authenticated=true;
 await vm.runInContext('refreshPricingStatus()',context);assert.equal(calls,1);assert.equal(context.state.settings.pricing_sync.total,10);
 context.state.auth.authenticated=false;
 await vm.runInContext('refreshPricingStatus()',context);assert.equal(calls,1);
});

test('Settings import keeps preview read-only, applies only selected items, clears credentials and refreshes lists',async()=>{
 const h=harness(async path=>path.endsWith('preview')?ready:{items:[{id:'one',name:'Saved',kind:'api_key',status:'imported'}]});
 h.file('document',[file('{}')]);h.file('keys',[file('[{"name":"Client","key":"PRIVATE-KEY"}]')]);
 await h.run('previewImport()');assert.equal(h.calls.length,1);assert.equal(h.calls[0].path,'/api/imports/preview');
 let html=h.run('importReview()');assert.match(html,/&lt;img/);assert.doesNotMatch(html,/<img/);assert.match(html,/1 ready · 1 existing · 1 unsupported/);assert.doesNotMatch(html,/PRIVATE-KEY/);
 await h.run('applyImport()');assert.deepEqual(h.calls[1].body.selected,['one']);assert.equal(h.calls[1].body.client_keys[0].key,'PRIVATE-KEY');
 assert.equal(snapshot(h).request,null);assert.equal(snapshot(h).files.keys.length,0);assert.equal(snapshot(h).completed,true);
 assert.equal(h.context.state.providers[0].id,'imported');assert.match(h.run('importReview()'),/href="#\/providers"/);assert.match(h.run('importReview()'),/href="#\/api-keys"/);
 await h.run('applyImport()');assert.equal(h.calls.length,2);
});

test('file selections survive rerenders and invalidation prevents importing an older preview',async()=>{
 const h=harness();h.file('document',[file('{}','<sample>.json')]);await h.run('previewImport()');
 const node={dataset:{importFile:'document'},files:[file('{"new":true}','replacement.json')],addEventListener(type,fn){this.change=fn;}};h.nodes.push(node);h.run('bindImportFileInputs()');node.change();
 assert.equal(snapshot(h).request,null);assert.equal(snapshot(h).result,null);assert.equal(snapshot(h).files.document[0].name,'replacement.json');
 assert.match(h.run('settingsImport()'),/replacement.json · 12 B/);await h.run('applyImport()');assert.equal(h.calls.length,1);
 h.run('invalidateImport()');assert.equal(snapshot(h).files.document[0].name,'replacement.json');
});

test('malformed JSON, file count, size and namespace fail before sending credential data',async()=>{
 for(const setup of [h=>h.file('keys',[file('PRIVATE BROKEN JSON')]),h=>h.file('auth',Array.from({length:501},()=>file('{}'))),h=>{h.context.upload=[{name:'large.json',size:4*1024*1024+1}];h.run('importState.files.document=upload');},h=>{h.file('document',[file('{}')]);h.run("importState.namespace='BAD NAME'");}]){
  const h=harness();setup(h);await h.run('previewImport()');assert.equal(h.calls.length,0);assert.ok(snapshot(h).error);assert.doesNotMatch(h.run('settingsImport()'),/PRIVATE BROKEN JSON/);
 }
});

test('unknown and partial apply outcomes keep per-item results and require a fresh preview before retrying',async()=>{
 const h=harness(async path=>{if(path.endsWith('apply'))throw new Error('Network unavailable');return ready;});h.file('document',[file('{}')]);await h.run('previewImport()');await h.run('applyImport()');await h.run('applyImport()');
 assert.equal(h.calls.length,2);assert.equal(snapshot(h).request,null);assert.match(snapshot(h).error,/Preview again/);
 const partial=harness(async path=>path.endsWith('preview')?ready:{items:[{id:'one',status:'failed',name:'Failed item',credential_ref:'store:retained',message:'Retained unbound credential'}]});partial.file('document',[file('{}')]);await partial.run('previewImport()');await partial.run('applyImport()');
 assert.match(partial.run('importReview()'),/0 imported · 0 existing · 1 failed/);assert.match(partial.run('importReview()'),/store:retained/);
});

test('leaving Settings, signing out and late responses cannot restore a credential draft',async()=>{
 let resolve;const h=harness(()=>new Promise(done=>{resolve=done;}));h.file('document',[file('PRIVATE')]);const pending=h.run('previewImport()');
 while(!resolve)await new Promise(done=>setImmediate(done));
 h.route({section:'settings',id:'general'});h.route({section:'settings',id:'import'});resolve(ready);await pending;
 assert.equal(snapshot(h).request,null);assert.equal(snapshot(h).result,null);assert.equal(snapshot(h).files.document.length,0);
 h.file('keys',[file('PRIVATE')]);h.context.state.auth.authenticated=false;h.run('syncImportRoute()');assert.equal(snapshot(h).files.keys.length,0);
 h.context.state.auth.authenticated=true;h.file('document',[file('PRIVATE')]);h.event('pagehide',{});assert.equal(snapshot(h).files.document.length,0);
});

test('expired login clears uploaded credentials and returns to the console sign-in boundary',async()=>{
 const h=harness(async()=>{throw Object.assign(new Error('Unauthorized'),{status:401});});h.file('document',[file('PRIVATE')]);await h.run('previewImport()');
 assert.equal(h.context.state.auth.authenticated,false);assert.equal(snapshot(h).files.document.length,0);assert.equal(snapshot(h).request,null);
});

test('source switch and clear discard files while keeping identifier for safe retries; demo never uploads',async()=>{
 const h=harness();h.file('auth',[file('{}')]);h.event('change',{dataset:{},value:'cliproxyapi',matches:selector=>selector==='[data-import-source]'});assert.equal(snapshot(h).files.auth.length,0);assert.match(h.run('settingsImport()'),/OAuth credential files/);
 h.run("importState.namespace='office'");h.file('document',[file('{}')]);h.event('click',{closest:()=>({dataset:{importAction:'clear'}})});assert.equal(snapshot(h).namespace,'office');assert.equal(snapshot(h).source,'cliproxyapi');assert.equal(snapshot(h).files.document.length,0);
 h.context.demoMode=true;h.file('document',[file('{}')]);await h.run('previewImport()');assert.equal(h.calls.length,0);assert.match(h.run('settingsImport()'),/Demo mode never uploads files/);
});

test('Settings route, provider shortcut and old bookmarks all open the integrated console',async()=>{
 const settings=await fs.readFile(new URL('./app.part6.jsfrag',import.meta.url),'utf8');assert.match(settings,/\['import',nt\('Import & Migration'/);assert.match(settings,/settingsImport\(\)/);
 const providers=await fs.readFile(new URL('./app.part4.jsfrag',import.meta.url),'utf8');assert.match(providers,/href="#\/settings\/import"/);assert.doesNotMatch(providers,/href="\/import.html"/);
 const legacy=await fs.readFile(new URL('./import.html',import.meta.url),'utf8');assert.match(legacy,/url=.\/#\/settings\/import/);assert.doesNotMatch(legacy,/<form/);
 const h=harness();assert.match(h.run("importFileSummary([{name:'<sample>.json',size:2}], '')"),/<sample>.json · 2 B/);assert.match(h.run('settingsImport()'),/No file selected/);
});
