import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import vm from 'node:vm';

// Dependency-free fragment DOM: exercise the actual renderers, text walker and
// attribute localization together, including repeat passes and locale reversal.
const decode=value=>value.replace(/&(?:amp|lt|gt|quot|#39);/g,entity=>({'&amp;':'&','&lt;':'<','&gt;':'>','&quot;':'"','&#39;':"'"}[entity]));
class Element {
 constructor(tag,attrs={},parent=null){this.tagName=tag.toUpperCase();this.attrs=attrs;this.parentElement=parent;this.children=[];}
 getAttribute(name){return this.attrs[name]??null;}
 hasAttribute(name){return Object.hasOwn(this.attrs,name);}
 setAttribute(name,value){this.attrs[name]=String(value);}
 matches(selector){return selector.split(',').some(raw=>{
  const s=raw.trim();
  if(s.startsWith('.'))return (this.attrs.class||'').split(/\s+/).includes(s.slice(1));
  if(s.startsWith('[')){const [,name,value]=s.match(/^\[([^=\]]+)(?:="([^"]*)")?\]$/)||[];return this.hasAttribute(name)&&(value===undefined||this.getAttribute(name)===value);}
  return this.tagName===s.toUpperCase();
 });}
 closest(selector){for(let el=this;el;el=el.parentElement)if(el.matches(selector))return el;return null;}
 querySelectorAll(selector){return this.children.flatMap(child=>child instanceof Element?[...(child.matches(selector)?[child]:[]),...child.querySelectorAll(selector)]:[]);}
 get textContent(){return this.children.map(child=>child instanceof Element?child.textContent:child.nodeValue).join('');}
}
function fragment(html){
 const root=new Element('root');let current=root;
 for(const token of html.match(/<[^>]+>|[^<]+/g)||[]){
  if(token.startsWith('</')){current=current.parentElement||root;continue;}
  if(token.startsWith('<')){
   const [,tag,tail]=token.match(/^<([\w-]+)([\s\S]*?)\/?\s*>$/)||[];
   if(!tag)continue;
   const attrs={};for(const match of tail.matchAll(/([^\s=]+)(?:="([^"]*)"|='([^']*)'|=([^\s]+))?/g))attrs[match[1]]=decode(match[2]??match[3]??match[4]??'');
   const el=new Element(tag,attrs,current);current.children.push(el);
   if(!['input','br','hr','img','meta','link'].includes(tag)&&!token.endsWith('/>'))current=el;
  }else current.children.push({nodeValue:decode(token),parentElement:current});
 }
 return root;
}
const textNodes=root=>root.children.flatMap(child=>child instanceof Element?textNodes(child):[child]);
const snapshot=root=>({text:textNodes(root).map(node=>node.nodeValue),attrs:root.querySelectorAll('[placeholder],[title],[aria-label],[data-label],[value],[data-id]').map(el=>Object.fromEntries(Object.entries(el.attrs).filter(([name])=>!name.startsWith('data-i18n-original-'))))});
const esc=value=>String(value??'').replaceAll('&','&amp;').replaceAll('<','&lt;').replaceAll('>','&gt;').replaceAll('"','&quot;').replaceAll("'",'&#39;');
const now='2026-09-28T04:05:06Z';
const state={
 auth:{username:'administrator'},models:[],providers:[],apiKeys:[],managementTokens:[],securityEvents:[],modelCache:new Map(),requestCache:new Map(),
 settings:{},settingsTab:'general',modelsSearch:'',modelStatus:'',modelType:'',keySearch:'',
 requests:[],requestTotal:0,requestLimit:25,requestOffset:0,requestWindow:'24h',requestFilter:'all',requestSearch:'',requestLastUpdated:new Date(now),
};
const context=vm.createContext({
 state,console,Date,Intl,Math,Number,URL,URLSearchParams,location:{href:"https://ferry.test/#/settings/security"},consoleLocale:'en-US',NodeFilter:{SHOW_TEXT:4},
 document:{querySelector:()=>null,createTreeWalker(root){const nodes=textNodes(root);let index=0;return {currentNode:null,nextNode(){this.currentNode=nodes[index++];return Boolean(this.currentNode);}};}},
 nt:(en,zh)=>context.consoleLocale==='zh-CN'?zh:en,nextLocale:()=>context.consoleLocale==='zh-CN'?'zh':'en',esc,icon:()=>'',badge:status=>'<span class="badge"><span class="status-dot"></span>'+esc(status)+'</span>',
 demoMode:false,scenarioView:()=>({}),scenarioBanner:()=>'',queueMicrotask(){},setInterval(){},
 app:{addEventListener(){}},currentRoute:()=>({section:'settings',id:'general'}),decorateRequestWindowOptions(){},requestLocalTime:value=>value||'',
});
const run=source=>vm.runInContext(source,context);
const read=part=>fs.readFile(new URL('./app.part'+part+'.jsfrag',import.meta.url),'utf8');
async function loadFunctions(part,names){
 const source=await read(part);
 for(const name of names){
  const start=source.indexOf('function '+name+'(');assert.notEqual(start,-1,name+' exists');
  const rest=source.slice(start),next=rest.slice(1).search(/\n(?:async )?function |\n(?:const|let) /);
  run(next<0?rest:rest.slice(0,next+1));
 }
}
run(await read(14));
await loadFunctions(3,['modelIsEnabled','modelLifecycle','pageModels']);
await loadFunctions(6,['pageSettings','settingsGeneral','settingsAutomaticUsageReset','settingsCodexUsageProtection','settingsPricingSync','settingsCodexVersion']);
await loadFunctions(7,['managementTokenStatus','ferrySkillInstallPrompt','ferrySkillInstallCard','settingsSecurity']);
await loadFunctions(11,['requestFilterSummary','requestKeySubtitle','requestMoney','requestPricingLabel','requestReferenceDetails','requestCostDetails','requestTimingTone','requestAccountingCells','pageRequests','pageApiKeys','apiKeySecretActivity','apiKeyDetailModal']);
const queueSource=await read(12);run(queueSource.slice(queueSource.indexOf('// Queue controls')));

function localized(expression,locale='zh-CN'){
 context.consoleLocale=locale;const root=fragment(run(expression));context.localizeDOM(root);return root;
}
function roundTrip(expression){
 const root=localized(expression,'en-US'),original=snapshot(root);
 context.consoleLocale='zh-CN';context.localizeDOM(root);const zh=snapshot(root);
 context.localizeDOM(root);assert.deepEqual(snapshot(root),zh,'a second locale pass must be idempotent');
 context.consoleLocale='en-US';context.localizeDOM(root);assert.deepEqual(snapshot(root),original,'same-node English restoration: '+expression);
 return localized(expression);
}
const contains=(root,...values)=>values.forEach(value=>assert.ok(root.textContent.includes(value),'missing rendered translation: '+value));

// Models: statuses translate, generated copy retains every byte of the ID,
// while even product-looking administrator descriptions are opaque.
const modelID='vendor/model.v2. Queue · Cache/$&';
state.models=[
 {id:modelID,name:'Model Queue',description:'Automatically created for upstream model '+modelID,status:'Unavailable',raw:{auto_generated:true},routes:[],requestRate:0},
 {id:'custom',name:'Provider Cache',description:'Search. Queue · Cache',status:'Degraded',routes:[],requestRate:0},
 {id:'custom-prefix',name:'Custom prefix',description:'Automatically created for upstream model User. Queue · Cache',status:'Healthy',raw:{auto_generated:false},routes:[],requestRate:0},
];
let root=roundTrip('pageModels()');
contains(root,'运行状态：不可用','运行状态：降级','为上游模型自动创建：'+modelID,'Search. Queue · Cache',state.models[2].description);
assert.ok(root.querySelectorAll('[data-id]').some(el=>el.getAttribute('data-id')===modelID));

// Queue wire enums only translate in badges and policy cells, never identifiers.
context.fixtureQueue={generated_at:now,waiting:1,running:2,open_requests:6,requests:['received','waiting','active','recovering','streaming','delivering'].map((phase,i)=>({request_id:'request-'+i,phase,logical_model_id:'active',provider_id:'provider',connection_id:'connection',started_at:now})),connections:['global','provider','connection'].map(policy=>({provider_id:'provider',connection_id:'connection',identity:'active',enabled:true,policy_source:policy,runtime:{in_flight:1,waiting:0,max_queue:256,max_wait_seconds:120,recovering:true}}))};
run('queueData=fixtureQueue;queueLoaded=true');
root=roundTrip('pageQueue()');
contains(root,'队列','渠道 / 连接','已接收','排队中','执行中','恢复中','流式传输中','交付中','全局 · 串行恢复','渠道 · 串行恢复','连接 · 串行恢复','provider / connection','active');
contains(root,new Date(now).toLocaleTimeString('zh-CN'));
for(const id of ['active','provider','connection','global','vendor/Queue','sha-Cache-abcdef'])assert.equal(context.translatedConsoleText(id),id);

// Requests: actual page controls, all placeholders, numeric cache accounting,
// the unknown-cost state, opaque filter values and an accessible mobile label.
state.providers=[{id:'provider.v2',name:'Provider name'}];
state.requestModel=modelID;state.requestProvider='provider.v2';state.requestApiKey='Key. Queue · Cache';
state.requests=[{id:'req_abcdef0123456789',apiKey:'Key. Queue · Cache',time:'12:05:06',model:modelID,conversation:'conversation-Cache',providerId:'provider.v2',upstreamModel:modelID,usageObserved:true,inputTokens:2000,outputTokens:100,cacheReadTokens:1000,raw:{cache_eligible_input_tokens:2000,cost_observed:false}}];state.requestTotal=1;
root=roundTrip('pageRequests()');
contains(root,'搜索','导出全部 JSON','导出全部 CSV','缓存 ↓ 1,000 · 50.0%','未记录费用','更新于 '+new Date(now).toLocaleTimeString('zh-CN'),'第 1 / 1 页','模型 '+modelID,'API 密钥 Key. Queue · Cache');
for(const placeholder of ['请求 / 会话 / API 密钥','任意失败类型','任意状态','任意版本'])assert.ok(root.querySelectorAll('[placeholder]').some(el=>el.getAttribute('placeholder')===placeholder),placeholder);
assert.ok(root.querySelectorAll('[aria-label]').some(el=>el.getAttribute('aria-label')==='搜索请求'));
state.requestExporting=true;state.requestLoading=true;contains(localized('pageRequests()'),'正在导出…','正在更新请求…');state.requestExporting=false;state.requestLoading=false;

// Keys: literal scopes, status badges, usage units, and data-label attributes.
const key={id:'key-1',name:'Key. Queue · Cache',preview:'sk-Cache…1234',status:'Active',scope:'All models',usage:12,created:'2026-09-28',revealable:true,identity:{},secrets:[{id:'secret-1',preview:'sk-Queue…5678',state:'ACTIVE',created_at:now},{id:'secret-2',preview:'sk-Cache…9012',state:'GRACE',created_at:now}],lastRequestAt:now};
state.apiKeys=[key,{...key,id:'key-2',name:'Revoked key',status:'Revoked',usage:'—'}];
root=roundTrip('pageApiKeys()');contains(root,key.name,key.preview,'全部模型','已撤销','12 次/分钟','暂不可用');
for(const label of ['名称','密钥','状态','权限','用量','创建时间','操作'])assert.ok(root.querySelectorAll('[data-label]').some(el=>el.getAttribute('data-label')===label),'responsive label '+label);
state.modal={key};root=roundTrip('apiKeyDetailModal(state.modal)');
contains(root,'近 30 天','有效','宽限期','模型：全部模型','协议：全部','到期：永不','预算 不限','创建于 '+new Date(now).toLocaleString('zh-CN'),'（密钥级活动）','从未记录');
for(const [usage,expected] of [
 [{requests:0,failed:0},['0','0','0','0']],
 [{requests:2,failed:1},['2','1','暂不可用','未记录']],
 [{requests:2,failed:0,tokens_observed:true,input_tokens:100,output_tokens:20,cost_observed:true,total_cost:0.25},['2','0','120','0.25']],
]){
 key.usageSummary=usage;root=roundTrip('apiKeyDetailModal(state.modal)');
 assert.deepEqual(root.querySelectorAll('.summary-chip').map(chip=>chip.querySelectorAll('strong')[0].textContent),expected,'key usage distinguishes zero, missing observations and recorded values');
}

// Settings: both toggle branches, current/stale/paused pricing, and native
// diagnostic text that happens to contain translatable product messages.
state.modal=null;const diagnostic='Unavailable. Queue · Cache/$&';
state.settings={debug_request_response:false,scheduled_model_sync_enabled:true,pricing_sync:{settings:{enabled:true,source:'litellm',interval_hours:24},source:'litellm',version:'sha-Queue.Cache',total:123,stale:true,last_success:now,next_update:now,error:diagnostic}};
root=roundTrip('pageSettings()');
contains(root,'常规','安全','队列与并发','刷新价格','123 条价格','使用缓存 / 等待更新','上次成功 '+new Date(now).toLocaleString('zh-CN'),'下次更新 '+new Date(now).toLocaleString('zh-CN'),'已关闭调试记录','需要详细请求与响应证据时可临时开启。','已开启定时同步','渠道按各自配置的周期同步，自动更新能力元数据。','litellm · sha-Queue.Cache',diagnostic+'. 仍可使用最近一次成功更新的缓存。');
state.settings.debug_request_response=true;state.settings.scheduled_model_sync_enabled=false;state.settings.pricing_sync.settings.enabled=false;state.settings.pricing_sync.stale=false;
root=roundTrip('settingsGeneral()');contains(root,'已开启调试记录','仅记录新请求，可在请求详情中查看内容。','已暂停定时同步','定时更新已暂停','最新','下次更新 已暂停');
state.settings.pricing_sync.version='';state.settings.pricing_sync.last_success='0001-01-01T00:00:00Z';root=roundTrip('settingsPricingSync()');contains(root,'暂无成功的快照','上次成功 —');
state.settings.codex_auto_update=true;state.settings.codex_client_version='0.161.0-alpha.3';state.settings.codex_effective_client_version='0.161.0-alpha.3';state.settings.codex_release_channel='prerelease';state.settings.codex_update_status={status:'manual'};
root=roundTrip('settingsCodexVersion()');contains(root,'自动跟随 GitHub 版本','版本通道','含预发布版','手动指定版本','正在使用手动版本','清空手动版本并发布后，才会开始自动更新。','0.161.0-alpha.3');
assert.ok(root.querySelectorAll('[data-action="codex-auto-update"]')[0].hasAttribute('checked'));
assert.equal(root.querySelectorAll('[data-action="codex-client-version"]')[0].getAttribute('value'),'0.161.0-alpha.3');

// User content / diagnostics / machine values are never interpreted as UI.
state.managementTokens=[{id:'token-id',name:'Token Queue',description:'Updated 12:05:06',preview:'sha-Queue.Cache',enabled:true}];
root=roundTrip('settingsSecurity()');contains(root,'Token Queue','Updated 12:05:06','sha-Queue.Cache');
context.protectedHTML='<div data-i18n-skip title="Queue" data-label="Name">Queue · Cache</div><pre>Runtime: Unavailable</pre><code>Search</code><div role="alert">Unavailable. Queue · Cache</div><div class="trace-detail">Reference · Cache</div><input value="Queue" placeholder="Any status">';
root=roundTrip('protectedHTML');contains(root,'Queue · Cache','Runtime: Unavailable','Search','Unavailable. Queue · Cache','Reference · Cache');
assert.equal(root.querySelectorAll('[value]')[0].getAttribute('value'),'Queue');
assert.equal(root.querySelectorAll('[data-label]')[0].getAttribute('data-label'),'Name');

// Composite captures must not become replacement-string operators or be split
// at punctuation embedded in resource IDs / catalog names.
for(const [english,chinese] of [
 ['Automatically created for upstream model '+modelID,'为上游模型自动创建：'+modelID],
 ['Reference · source. Queue · Cache/$& · Cached','参考 · source. Queue · Cache/$& · 已缓存'],
 ['Models: '+modelID+' · Protocols: openai.chat · Expires: Never · 12 RPM · Budget 3.5','模型：'+modelID+' · 协议：openai.chat · 到期：永不 · 12 RPM · 预算 3.5'],
 ['Updated — · Cached / awaiting refresh','更新于 — · 使用缓存 / 等待刷新'],
 ['Cache ↓ — · —','缓存 ↓ — · —'],
 ]){
 context.consoleLocale='zh-CN';assert.equal(context.translatedConsoleText(english),chinese);
 context.consoleLocale='en-US';assert.equal(context.translatedConsoleText(english),english);
}
assert.equal(context.consoleDateTime('invalid date'),'—');
console.log('PASS rendered locale coverage, scoped queue enums, opaque identifiers/descriptions/diagnostics, dates, attributes and reversible locale switching');

// Detail pages contain generated trace labels alongside opaque upstream evidence.
await loadFunctions(11,['capabilityNames','candidateExclusionDetail','pageRequestDetail']);
context.requestDataSync=()=>({id:'req-test',failed:true,streamMode:'Non-streaming',raw:{},trace:[{kind:'error',time:'12:00',title:'Provider Attempt #2 Failed',sub:'Reference · Cache',latency:'2ms'},{kind:'normal',title:'Caller Affinity Not Reused',sub:'upstream diagnostic'}],routePath:[{status:'Failed',failure:'Reference · Cache',responseModel:'vendor/Queue',latency:'2ms'}]});
context.fmtCompact=String;
root=roundTrip("pageRequestDetail('req-test')");
contains(root,'请求 req-test','非流式','渠道尝试 #2 失败','未复用调用方亲和性','响应模式','所需能力','推理 Token','恢复额外耗时','失败信息','Reference · Cache','vendor/Queue');
contains(localized("pageRequestDetail('req-test')"),'返回模型：','计价版本','完成耗时');
assert.ok(!localized("pageRequestDetail('req-test')",'en-US').textContent.match(/[\u4e00-\u9fff]/),'English detail contains no Chinese product copy');
contains(localized('pageRequests()'),'下一页');
console.log('PASS request detail labels, trace titles, pagination and preserved upstream evidence');
