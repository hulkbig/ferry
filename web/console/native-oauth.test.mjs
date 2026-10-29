import assert from 'node:assert/strict';
import test from 'node:test';
import fs from 'node:fs/promises';
import vm from 'node:vm';
import {HttpFerryAdapter, MockFerryAdapter, parseNativeCredential} from './api.js';
const drivers=[
 {id:'claude',name:'Claude',mode:'browser',base_url:'https://api.anthropic.com/v1',protocol:'anthropic'},
 {id:'kimi',name:'Kimi',mode:'device',base_url:'https://api.kimi.com/coding/v1',protocol:'openai-chat'},
 {id:'vertex',name:'Vertex',mode:'import',base_url:'',protocol:'openai-chat'},
 {id:'antigravity',name:'Antigravity',mode:'browser',base_url:'https://cloudcode-pa.googleapis.com',protocol:'openai-chat'},
 {id:'devin',name:'Devin',mode:'device',base_url:'https://api.devin.ai',protocol:'openai-chat'},
];
const view=(driver='claude',account='personal',status='connected',extra={})=>({driver,account,status,base_url:drivers.find(d=>d.id===driver)?.base_url||'',protocol:drivers.find(d=>d.id===driver)?.protocol||'',credential_ref:`native-oauth:${driver}:${account}`,...extra});
const plain=value=>JSON.parse(JSON.stringify(value));
const deferred=()=>{let resolve;const promise=new Promise(r=>resolve=r);return {promise,resolve}};
const p9=await fs.readFile(new URL('./app.part9.jsfrag',import.meta.url),'utf8');
const p12=await fs.readFile(new URL('./app.part12.jsfrag',import.meta.url),'utf8');
const p14=await fs.readFile(new URL('./app.part14.jsfrag',import.meta.url),'utf8');
const escape=value=>String(value??'').replaceAll('&','&amp;').replaceAll('<','&lt;').replaceAll('"','&quot;');
function harness(){
 const listeners=[],keyListeners=[],requests=[],state={auth:{authenticated:true},modal:{type:'add-provider'},wizard:{},settings:{},models:[]};
 const adapter={getNativeOAuthDrivers:async()=>drivers,getNativeOAuthStatus:async(d,a)=>view(d,a,'disconnected'),beginNativeOAuth:async(d,a,f)=>{requests.push({action:'begin',fields:f});return view(d,a,'pending',{url:'https://signin.example/authorize?state=public',user_code:'USER'})},completeNativeOAuth:async(d,a,c)=>{requests.push({action:'complete',callback:c});return view(d,a)},disconnectNativeOAuth:async(d,a)=>view(d,a,'disconnected')};
 const c=vm.createContext({state,adapter,console,URL,TextEncoder,Date,Map,Set,setInterval,clearInterval,queueMicrotask(){},demoMode:false,esc:escape,renderPortalPreservingView(){},renderPortal(){},providerUnlistedSection:()=>'',providerModelSyncDraftFingerprint:()=>'',portal:{querySelector:()=>null},document:{addEventListener:(_type,fn)=>keyListeners.push(fn)},app:{addEventListener:(type,fn)=>listeners.push(fn)}});
 vm.runInContext(p9.slice(p9.indexOf('const providerProtocolMeta='),p9.indexOf('function addProviderModal(')),c);
 vm.runInContext(p12.slice(0,p12.indexOf('function activeCodexProviderConnection(')),c);
 vm.runInContext(p12.slice(p12.indexOf('function providerEditorCanProbe('),p12.indexOf('function providerModelSyncDraftValue(')),c);
 vm.runInContext(p12.slice(p12.indexOf('function validateProviderEditor('),p12.indexOf('function providerFormModal(')),c);
 vm.runInContext(p12.slice(p12.indexOf('// Escape closes overlays'),p12.indexOf("app.addEventListener('click'")),c);
 vm.runInContext(p12.slice(p12.indexOf("app.addEventListener('click'"),p12.indexOf("portal.addEventListener('input'")),c);
 const w=c.newProviderEditor();w.name='Account';w.nativeOAuth=c.newNativeProviderState();w.nativeOAuth.drivers=drivers;w.nativeOAuth.driver='claude';w.nativeOAuth.initialized=true;w.baseUrl=drivers[0].base_url;state.wizard.provider=w;
 return {c,w,state,adapter,requests,escape:()=>keyListeners.forEach(fn=>fn({key:'Escape'})),click:action=>listeners[0]({target:{closest:()=>({dataset:{action}})},stopImmediatePropagation(){}})};
}
function httpHarness(handler){
 const calls=[];
 const adapter=new HttpFerryAdapter('',async(path,options)=>{
  calls.push({path,options});const result=await handler(path,options);
  const status=typeof result.status==='number'?result.status:200;return {ok:status===200,status,text:async()=>JSON.stringify(result.body??result)};
 });adapter.csrfToken='test-csrf';return {adapter,calls};
}

test('authenticated adapter uses direct views, CSRF and explicit POST complete; strips secrets',async()=>{
 const {adapter,calls}=httpHarness((path)=>path.endsWith('/drivers')?{drivers} : view('claude','local','pending',{url:'https://signin.example/?state=s',user_code:'USER',access_token:'RAW-TOKEN',refresh_token:'RAW-REFRESH',verifier:'RAW-VERIFIER',extra:{private_key:'RAW-KEY'}}));
 assert.deepEqual(await adapter.getNativeOAuthDrivers(),drivers);
 await adapter.beginNativeOAuth('claude','local',{});
 const result=await adapter.completeNativeOAuth('claude','local','callback-secret');
 await adapter.getNativeOAuthStatus('claude','local');await adapter.disconnectNativeOAuth('claude','local');
 assert.doesNotMatch(JSON.stringify(result),/RAW-/);
 assert.deepEqual(calls.map(c=>c.options.method||'GET'),['GET','POST','POST','GET','DELETE']);
 assert.deepEqual(JSON.parse(calls[1].options.body),{fields:{}});
 assert.deepEqual(JSON.parse(calls[2].options.body),{callback:'callback-secret'});
 for(const call of [calls[1],calls[2],calls[4]])assert.equal(call.options.headers['X-CSRF-Token'],'test-csrf');
 assert.equal(calls[3].options.body,undefined);
 assert.ok(calls.every(c=>c.path.startsWith('/api/oauth/native/')),'no config publish');
 assert.equal(parseNativeCredential('native-oauth:claude:local').driver,'claude');
 assert.equal(parseNativeCredential('native-oauth:claude:../bad'),null);
});

test('adapter safe errors, unavailable demo and malicious URL/view rejection',async()=>{
 const {adapter,calls}=httpHarness(()=>({status:500,body:{error:'RAW-SECRET'}}));
 await assert.rejects(adapter.completeNativeOAuth('claude','local','x'),e=>!e.message.includes('RAW'));
 await assert.rejects(adapter.getNativeOAuthStatus('claude','../unsafe'));
 assert.equal(calls.length,1);
 const demo=new MockFerryAdapter();assert.deepEqual(await demo.getNativeOAuthDrivers(),[]);await assert.rejects(demo.beginNativeOAuth());
 for(const url of ['javascript:alert(1)','https://u:password@example.com','https://example.com/?access_token=RAW-SECRET']){
  const {adapter:a}=httpHarness(()=>view('claude','personal','pending',{url}));assert.equal((await a.getNativeOAuthStatus('claude','personal')).url,'');
 }
 const {adapter:a}=httpHarness(()=>view('kimi','other'));await assert.rejects(a.getNativeOAuthStatus('claude','personal'));
});

test('native browser sign-in stores reference only, never callback or tokens in rendered panel',async()=>{
 const {c,w,requests}=harness();
 await c.nativeProviderAction(w,'begin');w.nativeOAuth.callback='PRIVATE-CALLBACK';
 await c.nativeProviderAction(w,'complete');
 assert.equal(requests[1].callback,'PRIVATE-CALLBACK');assert.equal(w.nativeOAuth.callback,'');
 assert.equal(w.credentialRef,'native-oauth:claude:personal');assert.equal(w.baseUrl,drivers[0].base_url);
 assert.ok(w.protocols.every(p=>p.models.length===0),'no invented models');
 c.nativeProviderApplyView(w,view('claude','personal','connected',{access_token:'RAW-TOKEN',refresh_token:'RAW-REFRESH',extra:{password:'RAW-PASSWORD'}}));
 const html=c.providerEditorModal(w,{type:'add-provider'});
 assert.doesNotMatch(html,/RAW-|PRIVATE-CALLBACK|data-wizard="provider-editor-secret"|data-wizard="provider-protocol-secret"/);
 assert.doesNotMatch(JSON.stringify(w.nativeOAuth),/RAW-/);
 assert.match(html,/Native provider sign-in/);
});

test('switch clears credentials, supply and probe evidence but preserves conversion policy',()=>{
 const {c,w}=harness();w.secret='old-key';w.credentialRef='native-oauth:claude:old';w.catalog=['old-model'];
 w.protocols[0].models=['old-model'];w.protocols[0].modelSpecs={'old-model':{capabilities:{tools:true}}};w.protocols[1].autoConvert=false;w.nativeOAuth.callback='old-callback';
 c.selectNativeProvider(w,'kimi','other');
 assert.equal(w.credentialRef+w.secret+w.nativeOAuth.callback,'');assert.deepEqual(plain(w.catalog),[]);
 assert.ok(w.protocols.every(p=>p.models.length===0&&Object.keys(p.modelSpecs).length===0));
 assert.equal(w.protocols[1].autoConvert,false);assert.equal(w.baseUrl,drivers[1].base_url);
});

for(const boundary of ['close','switch','logout','disconnect'])test(`stale completion ignored after ${boundary}`,async()=>{
 const {c,w,state,adapter,click}=harness();const wait=deferred();adapter.completeNativeOAuth=()=>wait.promise;
 w.nativeOAuth.callback='PRIVATE';const work=c.nativeProviderAction(w,'complete');assert.equal(w.nativeOAuth.callback,'');
 if(boundary==='close')state.modal=null;
 if(boundary==='switch')c.selectNativeProvider(w,'kimi','other');
 if(boundary==='logout')await click('logout');
 if(boundary==='disconnect')await c.nativeProviderAction(w,'disconnect');
 wait.resolve(view());await work;assert.equal(w.credentialRef,'');assert.notEqual(w.nativeOAuth.view?.status,'connected');
});

test('reopen existing ref recovers live status without erasing declarations or changing conversion',async()=>{
 const {c,state,adapter}=harness();adapter.getNativeOAuthStatus=async(d,a)=>view(d,a);
 const w=c.providerEditorFromData({provider:{id:'existing',name:'Existing',raw:{type:'custom',base_url:drivers[0].base_url,protocol_conversion:{anthropic:true,'openai-chat':false}}},connections:[{id:'connection',raw:{credential_ref:'native-oauth:claude:work'},models:[{upstream_model:'explicit-model',protocols:['anthropic'],capabilities:{tools:true,stream:false}}]}]});
 state.wizard.providerEdit=w;state.modal={type:'provider-form'};
 await c.loadNativeProvider(w);
 assert.equal(w.nativeOAuth.account,'work');assert.equal(w.nativeOAuth.view.status,'connected');
 assert.equal(w.protocols.find(p=>p.protocol==='anthropic').modelSpecs['explicit-model'].capabilities.tools,true);
 assert.equal(w.protocols[0].autoConvert,false);
});

test('stale driver/status loads cannot recover after close, switch or logout',async()=>{
 for(const which of ['drivers','status'])for(const boundary of ['close','switch','logout']){
  const {c,w,state,adapter,click}=harness(),wait=deferred();
  if(which==='drivers')adapter.getNativeOAuthDrivers=()=>wait.promise;else adapter.getNativeOAuthStatus=()=>wait.promise;
  const work=which==='drivers'?c.loadNativeProvider(w):c.nativeProviderAction(w,'status');
  if(boundary==='close')state.modal=null;
  if(boundary==='switch')c.selectNativeProvider(w,'kimi','other');
  if(boundary==='logout')await click('logout');
  wait.resolve(which==='drivers'?drivers:view());await work;
  assert.equal(w.credentialRef,'');assert.notEqual(w.nativeOAuth.view?.status,'connected');
 }
});

test('device completion is explicit; key_pending retries and relogin_required permits restart',async()=>{
 const {c,w,requests}=harness();c.selectNativeProvider(w,'kimi','personal');
 c.nativeProviderApplyView(w,view('kimi','personal','key_pending'));
 assert.match(c.nativeProviderPanel(w),/Finish account setup/);assert.equal(requests.length,0);
 await c.nativeProviderAction(w,'complete');assert.deepEqual(requests[0],{action:'complete',callback:''});
 c.nativeProviderApplyView(w,view('kimi','personal','relogin_required'));
 assert.equal(w.credentialRef,'');assert.match(c.nativeProviderPanel(w),/Start sign-in/);
});

test('Vertex and Antigravity fields are explicit, sent once and cleared; account-bound endpoint is used',async()=>{
 const {c,w,requests}=harness();c.selectNativeProvider(w,'vertex','project');
 assert.equal(w.modelSyncInterval,'off');assert.equal(w.baseUrl,'');
 w.nativeOAuth.fields={service_account:'PRIVATE-JSON',location:'us-central1'};
 assert.doesNotMatch(c.nativeProviderPanel(w),/PRIVATE-JSON/);
 await c.nativeProviderAction(w,'begin');assert.deepEqual(plain(requests[0].fields),{service_account:'PRIVATE-JSON',location:'us-central1'});
 assert.equal(w.nativeOAuth.fields.service_account,undefined);assert.match(c.nativeProviderPanel(w),/Complete import/);
 const base='https://us-central1-aiplatform.googleapis.com/v1/projects/p/locations/us-central1/endpoints/openapi';
 c.nativeProviderApplyView(w,view('vertex','project','connected',{base_url:base,protocol:'openai-chat'}));
 assert.equal(w.baseUrl,base);assert.equal(c.nativeProviderInfo(w).base_url,base);
 c.selectNativeProvider(w,'antigravity','work');w.nativeOAuth.fields={client_id:'authorized-client',client_secret:'PRIVATE-CLIENT-SECRET'};
 await c.nativeProviderAction(w,'begin');assert.equal(requests[1].fields.client_secret,'PRIVATE-CLIENT-SECRET');
 assert.equal(w.nativeOAuth.fields.client_secret,undefined);assert.doesNotMatch(c.nativeProviderPanel(w),/PRIVATE-CLIENT-SECRET/);
 assert.match(c.nativeProviderPanel(w),/type="password" data-native-field="client_secret"/);
 for(const id of ['vertex','antigravity','devin']){c.selectNativeProvider(w,id,'work');assert.equal(w.modelSyncInterval,'off');assert.match(c.nativeProviderPanel(w),/Enter explicit model IDs/)}
});

test('probe sends only native protocol, existing capability probe results remain authoritative',async()=>{
 const {c,w,adapter}=harness();c.nativeProviderApplyView(w,view());const p=w.protocols.find(p=>p.protocol==='anthropic');p.models=['chosen'];p.manualModels=['chosen'];
 const calls=[];adapter.discoverProviderProtocols=async payload=>{calls.push(plain(payload));return {models:['chosen'],protocols:[{protocol:'anthropic',status:'verified',models:['chosen'],model_results:[{model:'chosen',status:'verified',capability_evidence:[{capability:'tools',status:'supported'}]}]}]}};
 await c.probeProviderEditor(w);assert.equal(calls.length,1);assert.deepEqual(calls[0].protocols.map(p=>p.protocol),['anthropic']);
 assert.equal(p.modelSpecs.chosen.capabilities.tools,true);assert.equal(p.modelSpecs.chosen.capabilities.stream,false);
 assert.equal(w.protocols[0].autoConvert,true);
});

test('stale probe ignored after native switch',async()=>{
 const {c,w,adapter}=harness();c.nativeProviderApplyView(w,view());const wait=deferred();adapter.discoverProviderProtocols=()=>wait.promise;
 const work=c.probeProviderEditor(w);c.selectNativeProvider(w,'kimi','other');wait.resolve({models:['stale'],protocols:[{protocol:'anthropic',status:'verified',models:['stale']}]});await work;
 assert.deepEqual(plain(w.catalog),[]);assert.ok(w.protocols.every(p=>p.models.length===0));
});

test('adapter rejects wrong native endpoint/protocol before probe or secret/config write',async()=>{
 const {adapter,calls}=httpHarness(path=>{assert.equal(path,'/api/oauth/native/drivers');return {drivers}});
 const base={name:'Native',baseUrl:drivers[0].base_url,credentialRef:'native-oauth:claude:personal',protocols:[{protocol:'anthropic',models:['chosen'],autoConvert:true,modelSpecs:{chosen:{capabilities:{text:true,tools:false}}}}]};
 const built=await adapter.buildProviderProtocolConnections('native',base);assert.equal(built.connections[0].credential.secret_ref,base.credentialRef);
 for(const bad of [{...base,secret:'PRIVATE'},{...base,protocols:[{...base.protocols[0],fullUrl:'https://evil.test/messages'}]}])await assert.rejects(adapter.buildProviderProtocolConnections('native',bad));
 // Mismatched endpoint/protocol may only be accepted after authoritative account binding.
 const h=httpHarness(path=>path.endsWith('/drivers')?{drivers}:view());
 await assert.rejects(h.adapter.discoverProviderProtocols({...base,baseUrl:'https://evil.test'}));
 await assert.rejects(h.adapter.discoverProviderProtocols({...base,protocols:[{protocol:'openai-chat',models:['chosen']}]}));
 assert.ok([...calls,...h.calls].every(c=>(c.options.method||'GET')==='GET'));
});

test('dynamic imported binding passes exact endpoint/protocol validation and preserves conversion',async()=>{
 const base_url='https://us-central1-aiplatform.googleapis.com/v1/projects/p/locations/us-central1/endpoints/openapi';
 const h=httpHarness(path=>path.endsWith('/drivers')?{drivers}:view('vertex','work','connected',{base_url,protocol:'openai-chat'}));
 const built=await h.adapter.buildProviderProtocolConnections('vertex',{name:'Vertex',baseUrl:base_url,credentialRef:'native-oauth:vertex:work',protocols:[{protocol:'openai-chat',models:['explicit-id'],autoConvert:true},{protocol:'anthropic',models:[],autoConvert:true}]});
 assert.equal(built.connections.length,1);assert.equal(built.protocolConversion.anthropic,true);
 assert.ok(h.calls.every(c=>(c.options.method||'GET')==='GET'));
});

test('native copy has Chinese translations',()=>{
 for(const text of ['Native provider sign-in','Start sign-in','Complete sign-in','Check sign-in','Finish account setup','Service account JSON','OAuth client secret','Import account','Complete import','Native verification failed. Check sign-in status.'])assert.ok(p14.includes(text+'|'),text);
});

test('submission failures clear operator secrets and callback without echoing upstream errors',async()=>{
 const {c,w,adapter}=harness();c.selectNativeProvider(w,'antigravity','work');
 w.nativeOAuth.fields={client_id:'client',client_secret:'RAW-SECRET'};
 adapter.beginNativeOAuth=async()=>{throw new Error('RAW-SECRET upstream body')};
 await c.nativeProviderAction(w,'begin');
 assert.equal(w.nativeOAuth.fields.client_secret,undefined);assert.doesNotMatch(c.nativeProviderPanel(w),/RAW-SECRET/);
 w.nativeOAuth.callback='RAW-CALLBACK';adapter.completeNativeOAuth=async()=>{throw new Error('RAW-CALLBACK transport')};
 await c.nativeProviderAction(w,'complete');
 assert.equal(w.nativeOAuth.callback,'');assert.doesNotMatch(c.nativeProviderPanel(w),/RAW-CALLBACK/);
});

test('Escape invalidates pending completion and clears sensitive draft fields',async()=>{
 const {c,w,adapter,escape}=harness(),wait=deferred();
 adapter.completeNativeOAuth=()=>wait.promise;w.nativeOAuth.callback='PRIVATE-CALLBACK';
 const work=c.nativeProviderAction(w,'complete');w.nativeOAuth.fields={client_secret:'PRIVATE-SECRET'};
 escape();wait.resolve(view());await work;
 assert.equal(w.credentialRef,'');assert.equal(w.nativeOAuth.callback,'');assert.equal(w.nativeOAuth.fields.client_secret,undefined);
});
