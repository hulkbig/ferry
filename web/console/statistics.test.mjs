import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import vm from 'node:vm';
import { HttpFerryAdapter, MockFerryAdapter } from './api.js';
import { scenarios } from './mock-data.js';

const response = {
  from:'2026-09-04T00:00:00Z', to:'2026-09-05T00:00:00Z',
  summary:{requests:12345,succeeded:12340,failed:3,canceled:2,input_tokens:1234567,output_tokens:2000,tokens_observed:true,total_cost:0.00012345,cost_observed:true},
  outcomes:{direct_success:12338,recovered_success:2,failed:5},
  by_provider:{'':{requests:3,failed:3},'provider-a':{requests:12342,canceled:2,input_tokens:1234567,output_tokens:2000,tokens_observed:true,total_cost:0.00012345,cost_observed:true}},
  by_model:{'':{requests:3,failed:3}},
  trend:[{requests:12345,total_cost:0.00012345}],
};
const adapter = new HttpFerryAdapter('', async () => new Response(JSON.stringify(response),{headers:{'Content-Type':'application/json'}}));
const billing = await adapter.getBilling('24h');
const exported = JSON.parse(JSON.stringify(billing.reconciliation));
assert.equal(exported.summary.requests,12345,'export must retain exact request counts');
assert.equal(exported.summary.total_cost,0.00012345,'export must retain monetary precision');
assert.equal(exported.from,response.from,'export must identify the exact UTC interval');
assert.equal(exported.window,'24h');
assert.equal(billing.providers[1].name,'No provider selected');
assert.equal(billing.providers[1].amount,'—','missing prices must remain unknown');
assert.equal(billing.providers.reduce((total,row)=>total+row.requests,0),12345);
assert.equal(billing.providers[0].tokens,1236567);
assert.equal(billing.canceledRequests,2);

// The demo fixture uses the production billing contract and every displayed
// dimension reconciles with the exact exported summary.
const mock = new MockFerryAdapter();
const mockProviderID=mock.data.providers[0].id;
const mockProviderBefore=await mock.getProvider(mockProviderID);
assert.equal(mockProviderBefore.provider.raw.model_sync_interval,'1h','legacy mock Providers default to hourly model sync');
assert.equal(mockProviderBefore.provider.raw.model_sync.status,'idle');
await mock.updateProvider(mockProviderID,{model_sync_interval:'30m'});
const mockSync=await mock.syncProviderModels(mockProviderID);
assert.equal(mockSync.status,'succeeded');
assert.equal(mockSync.discovered_models,mockProviderBefore.provider.upstreamModels.length);
assert.equal(mockSync.added_models,0);
assert.equal(mockSync.created_logical_models,0);
assert.deepEqual(mockSync.added_model_ids,[]);
assert.equal((await mock.getProvider(mockProviderID)).provider.raw.model_sync_interval,'30m');
const mockCatalogProvider=await mock.addProvider({
  name:'Volcengine Plan Demo',type:'custom',baseUrl:'https://ark.cn-beijing.volces.com/api/plan/v3',credentialRef:'store:plan-data',
  modelSyncInterval:'1h',modelSyncCatalog:{type:'volcengine-plan',plan:'agent',credentialRef:''},
  modelSyncCatalogCredential:{access_key_id:'AK_DEMO_ONLY',secret_access_key:'SK_DEMO_ONLY',session_token:'STS_DEMO_ONLY'},
  protocols:[{protocol:'openai-responses',models:['ark-code-latest'],autoConvert:true,credentialRef:'store:plan-data'}],
});
const mockCatalogDetail=await mock.getProvider(mockCatalogProvider.id);
assert.deepEqual(mockCatalogDetail.provider.raw.model_sync_catalog,{type:'volcengine-plan',plan:'agent',credential_ref:'store:volcengine-plan-demo-model-catalog-demo'});
assert.doesNotMatch(JSON.stringify(mockCatalogDetail),/AK_DEMO_ONLY|SK_DEMO_ONLY|STS_DEMO_ONLY/,'mock Provider reads must never expose control-plane credential material');
mock.data.requests.push({id:'req-canceled-demo',model:'gpt-5.6-sol',providerId:'Codex Personal',apiKey:'production-key',result:'Canceled',canceled:true,status:'canceled'});
const mockFailedOutcome=await mock.getRequests({outcome:'FAILED'});
assert.ok(mockFailedOutcome.some(request=>request.id==='req-canceled-demo'),'mock FAILED outcome must include canceled requests like the HTTP contract');
const mockCanceledStatus=await mock.getRequests({status:'canceled'});
assert.deepEqual(mockCanceledStatus.map(request=>request.id),['req-canceled-demo'],'Canceled remains available as the more specific UI status filter');
const mockBilling = await mock.getBilling('720h');
assert.equal(mockBilling.costObserved,true);
assert.equal(mockBilling.totalCost,'$2,184.61');
assert.equal(mockBilling.totalRequests,'3.71M');
assert.equal(mockBilling.providers.reduce((sum,row)=>sum+row.requests,0),mockBilling.reconciliation.summary.requests);
assert.equal(mockBilling.providers.reduce((sum,row)=>sum+row.tokens,0),mockBilling.reconciliation.summary.input_tokens+mockBilling.reconciliation.summary.output_tokens);
assert.equal(mockBilling.models.reduce((sum,row)=>sum+row.requests,0),mockBilling.reconciliation.summary.requests);
assert.equal(mockBilling.models.reduce((sum,row)=>sum+row.tokens,0),mockBilling.reconciliation.summary.input_tokens+mockBilling.reconciliation.summary.output_tokens);
assert.equal(mockBilling.providers.at(-1).name,'No provider selected');
assert.equal(mockBilling.providers.at(-1).amount,'—');
assert.equal(Date.parse(mockBilling.reconciliation.to)-Date.parse(mockBilling.reconciliation.from),720*60*60*1000);
const cents=value=>Math.round(Number(value||0)*100);
for(const window of ['24h','168h','720h']){
  const view=await mock.getBilling(window),raw=view.reconciliation,expectedHours=Number.parseInt(window,10);
  assert.equal(Date.parse(raw.to)-Date.parse(raw.from),expectedHours*60*60*1000);
  assert.equal(raw.trend.reduce((sum,point)=>sum+point.requests,0),raw.summary.requests,`${window} trend requests must reconcile`);
  assert.equal(raw.trend.reduce((sum,point)=>sum+cents(point.total_cost),0),cents(raw.summary.total_cost),`${window} trend cost must reconcile`);
  assert.equal(raw.outcomes.direct_success+raw.outcomes.recovered_success,raw.summary.succeeded);
  assert.equal(raw.outcomes.failed,raw.summary.failed+raw.summary.canceled);
  assert.equal(raw.outcomes.direct_success+raw.outcomes.recovered_success+raw.outcomes.failed,raw.summary.requests);
  for(const dimension of ['by_provider','by_model']){
    const rows=Object.values(raw[dimension]);
    for(const field of ['requests','failed','canceled','input_tokens','output_tokens'])assert.equal(rows.reduce((sum,row)=>sum+Number(row[field]||0),0),Number(raw.summary[field]||0),`${window} ${dimension} ${field} must reconcile`);
    assert.equal(rows.reduce((sum,row)=>sum+(row.cost_observed?cents(row.total_cost):0),0),cents(raw.summary.total_cost),`${window} ${dimension} cost must reconcile`);
  }
  assert.ok(raw.trend.every(point=>Date.parse(point.from)>=Date.parse(raw.from)&&Date.parse(point.to)<=Date.parse(raw.to)));
}

// Each scenario is one deterministic ledger: headline outcomes, the all-call
// trend, logical models, providers, and provider/model series agree per bucket.
for(const scenario of Object.values(scenarios)){
  const expectedTotal=Object.values(scenario.outcomes).reduce((sum,value)=>sum+value,0);
  const trendTotal=scenario.trend.reduce((sum,point)=>sum+point.requests,0);
  const trendFailed=scenario.trend.reduce((sum,point)=>sum+point.failed,0);
  assert.equal(trendTotal,expectedTotal,`${scenario.id} headline and trend request totals differ`);
  assert.equal(trendFailed,scenario.outcomes.failed,`${scenario.id} headline and trend failures differ`);
  const logicalPrimary=scenario.trafficTrends.logicalModels.find(series=>series.id==='gpt-5.6-sol');
  assert.equal(logicalPrimary.requests,Object.values(scenario.modelOutcomes).reduce((sum,value)=>sum+value,0));
  assert.equal(logicalPrimary.failed,scenario.modelOutcomes.failed);
  for(const dimension of ['logicalModels','providers','providerModels']){
    for(let index=0;index<scenario.trend.length;index++){
      const points=scenario.trafficTrends[dimension].map(series=>series.points[index]);
      assert.equal(points.reduce((sum,point)=>sum+point.requests,0),scenario.trend[index].requests,`${scenario.id} ${dimension} bucket ${index} requests differ`);
      assert.equal(points.reduce((sum,point)=>sum+point.failed,0),scenario.trend[index].failed,`${scenario.id} ${dimension} bucket ${index} failures differ`);
    }
  }
}

const scenarioSource=await fs.readFile(new URL('./app.part2.jsfrag',import.meta.url),'utf8');
const scenarioController=scenarioSource.slice(scenarioSource.indexOf('const demoOverviewWindows='),scenarioSource.indexOf('function render()'));
const scenarioState={scenario:'protected',overviewWindow:'15m'};
const scenarioContext=vm.createContext({state:scenarioState,scenarios,demoMode:true,emptyOverview:()=>({})});
vm.runInContext(scenarioController,scenarioContext);
for(const [window,minutes] of Object.entries({'15m':15,'1h':60,'24h':1440,'168h':10080})){
  scenarioState.overviewWindow=window;const view=scenarioContext.scenarioView(),factor=minutes/15;
  assert.equal(view.totalRequests,1468*factor);
  assert.equal(view.recoveredCount,53*factor);
  assert.equal(view.requestRate,97.87);
  assert.equal(Date.parse(view.trend.at(-1).to)-Date.parse(view.trend[0].from),minutes*60*1000);
  assert.equal(view.trend.reduce((sum,point)=>sum+point.requests,0),view.totalRequests);
  assert.equal(view.trafficTrends.logicalModels.reduce((sum,series)=>sum+series.requests,0),view.totalRequests);
  assert.equal(view.totalTokens,19_200_000*factor);
}

const canceledAdapter = new HttpFerryAdapter('', async () => new Response(JSON.stringify({total:1,requests:[{
  id:'req-canceled',timestamp:'2026-09-05T00:00:00Z',logical_model_id:'gpt-5.6-sol',status:'canceled',outcome:'',usage:{},
}]}),{headers:{'Content-Type':'application/json'}}));
const canceledRequests=await canceledAdapter.getRequests({status:'canceled'});
assert.equal(canceledRequests[0].result,'Canceled');
assert.equal(canceledRequests[0].canceled,true);
assert.equal(canceledRequests[0].failed,false);

// Execute the actual window controller with deferred requests: failed or
// overlapping fetches must never relabel previously loaded statistics.
const source = await fs.readFile(new URL('./app.part1.jsfrag',import.meta.url),'utf8');
const controller = source.slice(source.indexOf('async function changeUsageWindow('),source.indexOf('async function ensureModel('));
const state = {usageLoading:{},billingWindow:'24h',billing:{old:true},overviewWindow:'15m',overview:{old:true},models:[{old:true}]};
let resolveBilling;
const messages = [];
const context = vm.createContext({state,adapter:{getBilling:()=>new Promise(resolve=>{resolveBilling=resolve;})},render:()=>{},showToast:message=>messages.push(message)});
vm.runInContext(controller,context);
const pending = context.changeUsageWindow('billing','168h');
assert.equal(state.usageLoading.billing,true);
assert.equal(state.billingWindow,'24h','old data keeps its original window until the request succeeds');
await context.changeUsageWindow('billing','720h');
resolveBilling({window:'168h'});
await pending;
assert.equal(state.billingWindow,'168h');
assert.equal(state.billing.window,'168h');
assert.equal(state.usageLoading.billing,false);
context.adapter.getBilling=async()=>{throw new Error('offline');};
await context.changeUsageWindow('billing','720h');
assert.equal(state.billingWindow,'168h');
assert.equal(state.billing.window,'168h');
assert.match(messages[0],/offline/);
context.adapter.getOverview=async()=>({new:true});
context.adapter.getModels=async()=>{throw new Error('models unavailable');};
await context.changeUsageWindow('overview','24h');
assert.equal(state.overviewWindow,'15m');
assert.equal(state.overview.old,true,'partial overview update must be atomic');
assert.equal(state.models[0].old,true);

// The newest request-list response owns the view even when an older request
// resolves later, and canceled is sent as a status rather than a success outcome.
const requestSource=await fs.readFile(new URL('./app.part5.jsfrag',import.meta.url),'utf8');
const requestController=requestSource.slice(requestSource.indexOf('function requestFilters('),requestSource.indexOf('function requestDataSync('));
const requestState={requestLimit:25,requestOffset:0,requestWindow:'15m',requestSearch:'old',requestConversation:'',requestModel:'',requestProvider:'',requestApiKey:'',requestConnection:'',requestFailureType:'',requestStatus:'',requestRevision:'',requestFilter:'all',requestFrom:'',requestTo:'',requests:[],requestTotal:0,requestLoading:false};
const deferred=[],requestMessages=[];
const requestContext=vm.createContext({state:requestState,adapter:{getRequests:filters=>new Promise(resolve=>deferred.push({filters,resolve}))},render:()=>{},showToast:message=>requestMessages.push(message)});
vm.runInContext(requestController,requestContext);
const older=requestContext.reloadRequests();
requestState.requestSearch='new';
const newer=requestContext.reloadRequests();
const newestPage=Object.assign([{id:'newest'}],{total:1});
deferred[1].resolve(newestPage);
await newer;
const oldestPage=Object.assign([{id:'stale'}],{total:1});
deferred[0].resolve(oldestPage);
await older;
assert.equal(requestState.requests[0].id,'newest');
assert.equal(requestState.requestLoading,false);
assert.equal(requestMessages.length,0);
requestState.requestFilter='canceled';
const canceledFilters=requestContext.requestFilters();
assert.equal(canceledFilters.status,'canceled');
assert.equal(canceledFilters.outcome,'');

// Request export freezes one interval, walks all result pages, preserves raw
// ledger objects in JSON, and neutralizes spreadsheet formulas in CSV cells.
const exportSource=await fs.readFile(new URL('./app.part10.jsfrag',import.meta.url),'utf8');
const exportController=exportSource.slice(exportSource.indexOf('function csvCell('),exportSource.indexOf('function swap('));
let exportedBlob,downloadClicks=0;
const exportCalls=[],exportState={requestExporting:false};
const exportMessages=[];
const exportContext=vm.createContext({state:exportState,render:()=>{},showToast:(message,type)=>exportMessages.push({message,type}),Blob,URL:{createObjectURL:blob=>{exportedBlob=blob;return 'blob:test'},revokeObjectURL:()=>{}},document:{createElement:()=>({click:()=>{downloadClicks++}})},setTimeout:fn=>fn(),requestFilters:()=>({window:'24h',q:'needle'}),adapter:{getRequests:async filters=>{exportCalls.push(filters);const raw={id:`raw-${filters.offset}`,caller:'=2+3',usage:{input_tokens:123}};return Object.assign([{id:`mapped-${filters.offset}`,raw}],{total:2})}}});
vm.runInContext(exportController,exportContext);
assert.equal(exportContext.csvCell('\t=SUM(A1:A2)'),`"'\t=SUM(A1:A2)"`);
assert.equal(exportContext.csvCell('\uFEFF+SUM(A1:A2)'),`"'\uFEFF+SUM(A1:A2)"`);
assert.equal(exportContext.csvCell(-17),'"-17"');
await exportContext.exportRequests('json');
assert.equal(exportCalls.length,2);
assert.deepEqual(exportCalls.map(call=>call.offset),[0,1]);
assert.equal(exportCalls[0].to,exportCalls[1].to,'all export pages must share one UTC boundary');
assert.equal(downloadClicks,1);
const exportEnvelope=JSON.parse(await exportedBlob.text());
assert.equal(exportEnvelope.reported_total,2);
assert.equal(exportEnvelope.exported_count,2);
assert.equal(exportEnvelope.requests[0].id,'raw-0');
assert.equal(exportEnvelope.requests[0].usage.input_tokens,123);

const returnedMetadata={upstream_model:'configured-alias',response_model:'observed-model',service_tier:'fast',response_service_tier:'priority'};
exportContext.exportRows([{id:'metadata-request',raw:returnedMetadata}],'csv','metadata');
const metadataCSV=(await exportedBlob.text()).split('\n');
const metadataHeaders=metadataCSV[0].split(',').map(value=>value.replaceAll('"',''));
const metadataValues=metadataCSV[1].split(',').map(value=>value.replaceAll('"',''));
for(const [key,value] of Object.entries(returnedMetadata))assert.equal(metadataValues[metadataHeaders.indexOf(key)],value,'CSV must preserve '+key);
const mappedMetadata=exportContext.requestExportRow({upstreamModel:'configured-alias',responseModel:'observed-model',serviceTier:'fast',responseServiceTier:'priority'});
for(const [key,value] of Object.entries(returnedMetadata))assert.equal(mappedMetadata[key],value,'mapped export must preserve '+key);
const successfulClicks=downloadClicks;
exportContext.adapter.getRequests=async filters=>Object.assign([{id:'repeated',raw:{id:'repeated'}}],{total:2});
await exportContext.exportRequests('json');
assert.equal(downloadClicks,successfulClicks,'a changing ledger must not download a partial file');
assert.equal(exportMessages.at(-1).type,'error');
assert.match(exportMessages.at(-1).message,/changed during export.*Retry/);

console.log('PASS exact statistics, self-consistent demo ledgers, atomic windows, and request exports');
