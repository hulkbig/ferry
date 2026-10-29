import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import vm from 'node:vm';
import { HttpFerryAdapter } from './api.js';

const source=await fs.readFile(new URL('./app.part13.jsfrag',import.meta.url),'utf8');
const messages=await fs.readFile(new URL('./app.part14.jsfrag',import.meta.url),'utf8');
const prefs=new Map(),pending=[],listeners={};
let renders=0,route='#/models/a',confirmation;
const state={analyticsWindow:'24h',analyticsTo:'2026-09-19T00:00:00Z',analyticsCache:new Map(),models:[],providers:[],analyticsDimension:'models'};
const context=vm.createContext({
 state,Intl,Date,URL,URLSearchParams,Blob,console,navigator:{language:'zh-CN'},
 localStorage:{getItem:key=>prefs.get(key),setItem:(key,value)=>prefs.set(key,value)},
 matchMedia:()=>({matches:false}),
 document:{documentElement:{dataset:{}},addEventListener:(name,callback)=>{listeners[name]=callback}},
 window:{confirm:value=>{confirmation=value;return true}},
 location:{hash:'#/models/a'},history:{replaceState(_a,_b,hash){context.location.hash=hash}},
 configureRequestAutoRefresh(){},closeOverlay(){},async reloadRequests(){},
 currentRoute:()=>({raw:route}),render:()=>{renders++},
 queueMicrotask:fn=>pending.push({run:fn}),demoMode:false,
 esc:value=>String(value).replaceAll('&','&amp;').replaceAll('<','&lt;').replaceAll('"','&quot;'),icon:()=>'',
 adapter:{getAnalytics:filters=>new Promise((resolve,reject)=>pending.push({filters,resolve,reject}))},
});
vm.runInContext(source,context);
vm.runInContext('Object.assign(globalThis,{nextCost,nextCount,nextSuccess,nextTheoreticalCoverage})',context);
vm.runInContext(messages,context);
assert.equal(prefs.get('ferry.locale'),'zh-CN');
assert.equal(prefs.get('ferry.theme'),'system');
assert.equal(context.document.documentElement.dataset.theme,'light');
assert.equal(context.nextCost({cost:0,priced_attempts:0}),'—','missing prices must not become free');
assert.notEqual(context.nextCost({cost:0,priced_attempts:1}),'—','known free is an observed zero');
assert.match(context.nextCost({cost:0.000004,priced_attempts:1}),/0\.000004/);
assert.equal(context.nextCount({basis:'attempts',attempts:5,requests:3}),5);
assert.equal(context.nextSuccess({basis:'attempts',attempt_succeeded:2,succeeded:3}),2);
assert.equal(context.nextResourceLink('models','deleted'),'<'+'span data-i18n-skip>deleted</span>');

// Details retain the overview cohort; intersections are tied to the page.
state.analyticsIntersection={route,filter:{provider:'p/x&?'}};
assert.equal(context.nextScope({model:'a'}).provider,'p/x&?');
const key=context.nextAnalyticsKey({model:'a'});
route='#/providers/p';
assert.equal(context.nextScope({provider:'p'}).model,undefined);
assert.equal(context.nextAnalyticsKey({model:'a'}),key);
const old=context.loadNextAnalytics({model:'a'},key);
assert.equal(pending[0].filters.to,undefined,'latest reports use server time, not the browser clock');
state.analyticsCache.clear();
const fresh=context.loadNextAnalytics({model:'a'},key);
pending[1].resolve({summary:{cost:5}});await fresh;
pending[0].resolve({summary:{cost:99}});await old;
assert.equal(state.analyticsCache.get(key).data.summary.cost,5,'stale requests cannot replace refreshed results');
assert.equal(renders,1,'stale response cannot redraw');
const failed=context.loadNextAnalytics({},'failed');
pending[2].reject(new Error('unavailable'));await failed;
assert.equal(state.analyticsCache.get('failed').data,null);
state.analyticsCache.set(context.nextAnalyticsKey({}),state.analyticsCache.get('failed'));
assert.match(context.nextAnalytics({},false),/统计暂不可用/);
assert.doesNotMatch(context.nextAnalytics({},false),/演示|Demo|production ledger/);
assert.match(context.nextAnalytics({},false),/unavailable/);
state.analyticsCache.get('failed').error={status:500,message:'analytics query unavailable <script>'};
assert.match(context.nextAnalytics({},false),/HTTP 500.*analytics query unavailable &lt;script>/);
context.demoMode=true;
assert.match(context.nextAnalytics({},false),/演示模式没有生产账本/);
assert.doesNotMatch(context.nextAnalytics({},false),/HTTP 500/);
context.demoMode=false;

// Reversible product translations preserve technical identifiers and free text.
assert.equal(context.translatedConsoleText('Overview'),'运行概览');
assert.equal(context.translatedConsoleText('a/provider.v2'),'a/provider.v2');
assert.equal(context.translatedConsoleText('2 configured · 3 converted'),'2 个已配置 · 3 个可转换');
assert.equal(context.translatedConsoleText(' upstream · '),' 上游模型 · ');
assert.equal(context.translatedConsoleText('Recovering'),'恢复中');
assert.equal(context.translatedConsoleText('4 legacy credentials'),'4 组旧版凭据');
assert.equal(context.translatedConsoleText('Disable logical model model.v2?\n\nNew requests for this model will stop routing.'),'停用逻辑模型 model.v2？\n\n此模型的新请求将停止路由。');
assert.equal(context.translatedConsoleText('Logical model routes affected: model.v2. New requests for this model will stop routing.'),'受影响的逻辑模型路由：model.v2。 此模型的新请求将停止路由。');
context.confirmConsole('Restore revision #42? The server will validate, review, and apply it as a new immutable revision.');
assert.match(confirmation,/恢复版本 #42/);
await listeners.click({target:{closest:()=>({dataset:{next:'locale'}})},preventDefault(){},stopImmediatePropagation(){}});
assert.equal(prefs.get('ferry.locale'),'en-US');
assert.equal(context.translatedConsoleText('Overview'),'Overview');
assert.equal(context.document.documentElement.lang,'en-US');
assert.match(context.nextAnalytics({},false),/The analytics API request failed/);
assert.doesNotMatch(context.nextAnalytics({},false),/Demo mode/);
await listeners.click({target:{closest:()=>({dataset:{next:'theme'}})},preventDefault(){},stopImmediatePropagation(){}});
assert.equal(prefs.get('ferry.theme'),'light');

// Small datasets have distinct whole-count chart ticks and attempt semantics.
const chart=context.nextChart([{requests:1,succeeded:1,cost:0,from:state.analyticsTo,to:state.analyticsTo}],'attempts');
for(const tick of [0,1,2,3,4])assert.match(chart,new RegExp('text-anchor="end">'+tick+'</text>'));
assert.match(chart,/1 attempts \/ 1 successful/);
assert.match(chart,/Recorded attempt and success counts over time/);
assert.doesNotMatch(chart,/NaN/);

// Filters survive reserved characters without altering API parameters.
let received;
const adapter=new HttpFerryAdapter('',async raw=>{received=new URL(raw,'https://console.test');return new Response(JSON.stringify({summary:{cost:0}}),{headers:{'Content-Type':'application/json'}})});
await adapter.getAnalytics({window:'168h',to:state.analyticsTo,model:'model /?&=',provider:'provider#+'});
assert.equal(received.pathname,'/api/analytics');
assert.equal(received.searchParams.get('model'),'model /?&=');
assert.equal(received.searchParams.get('provider'),'provider#+');
assert.equal(received.searchParams.get('to'),state.analyticsTo);
assert.equal(received.searchParams.get('window'),'168h');
for(const window of ['168h','720h']){
 await adapter.getAnalytics({window,to:state.analyticsTo});
 assert.equal(received.searchParams.get('window'),window);
 assert.equal(received.searchParams.get('to'),state.analyticsTo);
}
const failingAdapter=new HttpFerryAdapter('',async()=>new Response(JSON.stringify({error:'analytics query unavailable'}),{status:500}));
await assert.rejects(()=>failingAdapter.getAnalytics({window:'720h'}),error=>error.status===500&&error.message==='analytics query unavailable');
console.log('PASS analytics accounting display, cohort filters, refresh races, failures, preferences and translations');

// Provider collection navigation must render without a detail-page ID.
const providerSource=await fs.readFile(new URL('./app.part4.jsfrag',import.meta.url),'utf8');
const listSource=providerSource.slice(providerSource.indexOf('function pageProviders()'),providerSource.indexOf('\nfunction providerListCard'));
const providerState={providers:[],providerSearch:''};
const providerContext=vm.createContext({
 state:providerState,refreshQuotaOnEntry(){},providerIsCodex:()=>false,scenarioView:()=>({}),scenarioBanner:()=>'',icon:()=>'',esc:String,demoMode:false,
 providerModels:()=>[],providerUpstreamModels:()=>[],providerListCard:p=>'<article>'+p.name+'</article>',
});
vm.runInContext(listSource,providerContext);
assert.match(providerContext.pageProviders(),/No providers/);
providerState.providers=[{id:'p1',name:'Provider One',type:'custom',status:'Healthy'}];
assert.match(providerContext.pageProviders(),/<article>Provider One<\/article>/);
providerState.providerSearch='missing';
assert.match(providerContext.pageProviders(),/No providers/);
console.log('PASS provider collection navigation and search');

// Exact, shareable request cohorts clear stale filters and restore report context.
Object.assign(state,{analyticsWindow:'168h',analyticsTo:'2026-09-19T12:34:56.789Z',requestSearch:'stale',requestConversation:'stale',requestProvider:'stale',requestFilter:'failed',requestAutoRefresh:true,detailTabs:{'models:a':'overview'}});
context.location.hash='#/models/a';
const window=context.nextRequestWindow();
await context.openReportRequests({...window,model:'a/?&',provider:'p/#',attempt_failure_type:'failed'});
assert.equal(state.requestFrom,window.from);assert.equal(state.requestTo,window.to);
assert.equal(state.requestLogicalModel,'a/?&');assert.equal(state.requestParticipatingProvider,'p/#');
for(const field of ['requestSearch','requestConversation','requestProvider'])assert.equal(state[field],'');
assert.equal(state.requestFilter,'all');assert.equal(state.requestAutoRefresh,false);
const savedHash=context.location.hash;
state.appliedReportLocation='';state.requestLogicalModel='stale';
assert.equal(context.applyReportLocation(),true);
assert.equal(state.requestLogicalModel,'a/?&');assert.equal(state.requestTo,'2026-09-19T12:34:56.789Z');
state.analyticsWindow='24h';context.returnToReport();
assert.equal(context.location.hash,'#/models/a');assert.equal(state.analyticsWindow,'168h');
context.location.hash=savedHash;context.forgetReportLocation();assert.equal(context.location.hash,'#/requests');
context.location.hash='#/requests?report='+encodeURIComponent(JSON.stringify({from:window.from,to:window.to,model:{unexpected:true},origin:{hash:'javascript:alert(1)',window:'168h',to:window.to}}));
assert.equal(context.applyReportLocation(),true);assert.equal(state.requestLogicalModel,'');assert.equal(state.reportOrigin,null);
assert.match(context.nextCostLabel({cost:1,priced_attempts:1,unpriced_attempts:1}),/partial/);
assert.equal(context.nextCostLabel({cost:0,priced_attempts:0,unpriced_attempts:1}),'—');
assert.match(context.nextDetailTabs('providers','p'),/Health & capacity.*Usage & quota.*Configuration/);
assert.match(context.nextDetailTabs('models','a'),/Overview.*Routing/);
console.log('PASS exact report drillthrough, reload, return context, unknown and partial costs');

// Current API-equivalent values are independent of plan and recorded bills.
assert.equal(context.nextTheoreticalCost({cost:9,priced_attempts:1,theoretical_priced_attempts:0}),'—');
assert.equal(context.nextTheoreticalCost({cost:0,priced_attempts:0,theoretical_cost:2.5,theoretical_priced_attempts:1}),'≈ $2.50');
assert.equal(context.nextTheoreticalCost({theoretical_cost:0,theoretical_priced_attempts:1}),'≈ $0.00');
assert.equal(context.nextTheoreticalCost({theoretical_cost:2.5,theoretical_priced_attempts:1,theoretical_unpriced_attempts:1}),'≈ $2.50 · partial');
assert.equal(context.nextTheoreticalCoverage({attempts:2,theoretical_priced_attempts:1,priced_attempts:0}),'50.00%');
const estimateRows=Array.from({length:8},(_,i)=>({id:'provider-'+i,basis:'attempts',attempts:1,requests:1,attempt_succeeded:1,theoretical_cost:i===7?0:8-i,theoretical_priced_attempts:i===7?0:1,theoretical_unpriced_attempts:i===7?1:0,cost:0,priced_attempts:0,unpriced_attempts:1}));
const estimateSummary={...estimateRows[0],attempts:8,theoretical_cost:35,theoretical_priced_attempts:7,theoretical_unpriced_attempts:1};
const estimateData={summary:estimateSummary,providers:estimateRows,reference_pricing:{source:'fixture <catalog>',version:'v1',updated_at:state.analyticsTo,stale:true}};
const billing=context.nextBillingPanels(estimateData,{});
assert.ok(billing.includes('Other providers</span><strong>≈ $2.00 · partial'));
assert.match(billing,/fixture &lt;catalog>.*Cached prices/);
assert.match(billing,/Recorded costs &amp; diagnostics|Recorded costs & diagnostics/);
const table=context.nextAttribution(estimateRows,'providers',estimateSummary,{},estimateData);
assert.ok(table.includes('provider-7</span>'),'unestimated providers remain visible');
assert.ok(table.includes('≈ $8.00<small>Recorded —</small>'),'subscription reference value survives missing recorded costs');
assert.doesNotMatch(billing+table,/NaN|undefined/);
console.log('PASS all-provider theoretical cost, unknown/free/partial pricing, attribution and export provenance');

// A browser clock ahead of the API must not turn latest windows into HTTP 400.
// Exercise real toolbar clicks, deferred loading, HTTP adapter and report render.
const serverTo='2026-10-20T12:00:00.000Z',calls=[];
context.adapter=new HttpFerryAdapter('',async raw=>{
 const url=new URL(raw,'https://console.test');calls.push(url);
 if(url.searchParams.has('to')&&Date.parse(url.searchParams.get('to'))>Date.parse(serverTo))return new Response(JSON.stringify({error:'to must not be in the future'}),{status:400});
 const hours=Number.parseInt(url.searchParams.get('window'));
 return new Response(JSON.stringify({...estimateData,summary:{...estimateSummary,basis:'requests',requests:431,succeeded:431,errors:[]},models:[],timeline:[],to:serverTo,from:new Date(Date.parse(serverTo)-hours*3600000).toISOString()}));
});
async function clickWindow(value){await listeners.click({target:{closest:()=>({dataset:{next:'window',value}})},preventDefault(){},stopImmediatePropagation(){}})}
for(const window of ['168h','720h']){
 await clickWindow(window);
 // Simulate boot's client timestamp, five minutes ahead of the server.
 state.analyticsTo='2026-10-20T12:05:00.000Z';
 context.nextAnalytics({},false,true);
 const scheduled=pending.pop();await scheduled.run();
 assert.equal(calls.at(-1).searchParams.get('window'),window);
 assert.equal(calls.at(-1).searchParams.has('to'),false);
 assert.equal(state.analyticsTo,serverTo);
 assert.match(context.nextAnalytics({},false,true),/431/);
 assert.doesNotMatch(context.nextAnalytics({},false,true),/Analytics unavailable/);
 context.nextAnalytics({provider:'filtered'},false);
 await pending.pop().run();
 assert.equal(calls.at(-1).searchParams.get('to'),serverTo,'filters retain the resolved cohort');
}
// Refresh invalidates queued work before it can issue a mismatched request.
await clickWindow('168h');context.nextAnalytics({},false);
const staleLoad=pending.pop();
await clickWindow('720h');context.nextAnalytics({},false);
const currentLoad=pending.pop(),beforeCalls=calls.length;
await staleLoad.run();assert.equal(calls.length,beforeCalls);
await currentLoad.run();assert.equal(calls.length,beforeCalls+1);
assert.equal(calls.at(-1).searchParams.get('window'),'720h');
assert.equal(state.analyticsCache.size,1,'invalidated queued work cannot resurrect old keys');
console.log('PASS server-time latest windows, pinned filter cohorts and invalidated deferred loads');

const bucket=(hour,value)=>({from:`2026-10-24T0${hour}:00:00Z`,to:`2026-10-24T0${hour+1}:00:00Z`,requests:3,succeeded:2,direct:2,failed:1,ttft_p95_ms:value,ttft_samples:value==null?0:2});
const series=[bucket(0,12000),bucket(1,6000),bucket(2,null),bucket(3,8000)];
assert.doesNotMatch(context.nextChart(series),/<circle/,'failures use the stack, not floating dots');
const latency=context.nextLatencyChart(series);
assert.equal((latency.match(/class="next-latency-point"/g)||[]).length,3);
assert.equal((latency.match(/class="next-latency-line"/g)||[]).length,1,'never connect across missing samples');
assert.equal((latency.match(/tabindex="0"/g)||[]).length,3);
assert.match(latency,/text-anchor="end">0 ms/);
assert.doesNotMatch(latency,/NaN|Infinity/);
assert.match(context.nextLatencyChart([bucket(0,null)]),/No successful timing samples/);
assert.match(context.nextLatencyChart([bucket(0,0)]),/next-latency-point/);
console.log('PASS failure stacks, latency axes, sample gaps and keyboard targets');
const partialPoints=[{from:'2026-10-24T00:45:00Z',to:'2026-10-24T01:00:00Z'},{from:'2026-10-24T01:00:00Z',to:'2026-10-24T02:00:00Z'}];
const geometry=context.nextChartGeometry(partialPoints);
const firstWidth=geometry.edge(partialPoints[0],0,true)-geometry.edge(partialPoints[0],0);
const secondWidth=geometry.edge(partialPoints[1],1,true)-geometry.edge(partialPoints[1],1);
assert.ok(Math.abs(secondWidth/firstWidth-4)<1e-10,'a partial 15-minute bucket must not occupy a full hour');
assert.equal(geometry.edge(partialPoints[0],0,true),geometry.edge(partialPoints[1],1),'both charts align adjacent bucket boundaries');
