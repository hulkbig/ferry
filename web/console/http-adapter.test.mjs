import assert from 'node:assert/strict';
import { HttpFerryAdapter, MockFerryAdapter, FerryError } from './api.js';

const json = (body, status = 200) => new Response(JSON.stringify(body), {
  status,
  headers: { 'Content-Type':'application/json' },
});

function makeFetch(handler) {
  return async (rawUrl, options = {}) => {
    const url = new URL(rawUrl, 'https://console.test');
    const method = options.method || 'GET';
    const body = options.body ? JSON.parse(options.body) : null;
    return handler({ url, method, body });
  };
}

const originalFetch = globalThis.fetch;
let defaultFetchReceiver;
try {
  globalThis.fetch = function () {
    defaultFetchReceiver = this;
    return json({ status:'ok' });
  };
  const defaultAdapter = new HttpFerryAdapter();
  assert.deepEqual(await defaultAdapter.request('/api/fetch-binding'), { status:'ok' });
  assert.equal(defaultFetchReceiver, globalThis, 'default fetch must retain its global receiver');
} finally {
  globalThis.fetch = originalFetch;
}

const provider = {
  id:'provider-a', name:'Provider A', type:'custom', state:'recovering', enabled:true, base_url:'https://provider.test/v1',
  model_sync_interval:'30m',model_sync:{status:'succeeded',last_started_at:'2026-08-26T00:10:00Z',last_completed_at:'2026-08-26T00:10:02Z',last_success_at:'2026-08-26T00:10:02Z',next_run_at:'2026-08-26T00:40:02Z',last_error_code:'',last_error:'',discovered_models:3,added_models:1,created_logical_models:1,revision:12},
  model_sync_catalog:{type:'volcengine-plan',plan:'agent',credential_ref:'store:ark-control'},
  scheduled_message:{enabled:true,upstream_model:'upstream',message:'provider heartbeat'},
  available_connections:1, used_by_models:['smart'], traffic:{ requests:30, last_success_at:'2026-08-26T00:14:00Z' },
  connections:[{
    id:'connection-a', identity:'primary', enabled:true, credential_ref:'store:redacted',
    models:[{ upstream_model:'upstream', protocols:['openai-chat'] }],
    runtime:{ auth:'ready', reachability:'online', in_flight:1, quota_windows:[] },
    affinity:{ conversation:2 },
  }],
};

const requestsSeen = [];
const readAdapter = new HttpFerryAdapter('', makeFetch(({ url, method, body }) => {
  requestsSeen.push({ url, method, body });
  if (url.pathname === '/api/overview') return json({
    client_outcomes:{ direct_success:7, recovered_success:2, failed:1 },
    rates:{ requests_per_minute:4.25, tokens_per_minute:1200 }, active_conversations:3,
    usage_summary:{ requests:10, input_tokens:100, output_tokens:50, tokens_observed:true, total_cost:0.125, cost_observed:true, median_latency_ms:225, avg_recovery_latency_ms:31 },
    managed_automatically:['req-recovered'], needs_attention:['req-failed'],
    cache_hit_summary:{
      overall:{ requests:10,observed_requests:9,cache_eligible_input_tokens:2000,cache_read_tokens:1500,cache_write_tokens:100,hit_rate_pct:75 },
      api_keys:[{ id:'key-a',requests:7,observed_requests:7,cache_eligible_input_tokens:1600,cache_read_tokens:1280,cache_write_tokens:80,hit_rate_pct:80 }],
      logical_models:[{ id:'smart',requests:10,observed_requests:9,cache_eligible_input_tokens:2000,cache_read_tokens:1500,cache_write_tokens:100,hit_rate_pct:75 }],
    },
    trend:[
      { from:'2026-08-26T00:00:00Z',to:'2026-08-26T00:01:00Z',requests:2,succeeded:2,failed:0 },
      { from:'2026-08-26T00:01:00Z',to:'2026-08-26T00:02:00Z',requests:1,succeeded:0,failed:1 },
    ],
    traffic_trend:[
      { logical_model_id:'smart',provider_id:'provider-a',upstream_model:'upstream',points:[
        { from:'2026-08-26T00:00:00Z',to:'2026-08-26T00:01:00Z',requests:2,succeeded:2,failed:0 },
        { from:'2026-08-26T00:01:00Z',to:'2026-08-26T00:02:00Z',requests:0,succeeded:0,failed:0 },
      ] },
      { logical_model_id:'smart',provider_id:'provider-b',upstream_model:'backup',points:[
        { from:'2026-08-26T00:00:00Z',to:'2026-08-26T00:01:00Z',requests:0,succeeded:0,failed:0 },
        { from:'2026-08-26T00:01:00Z',to:'2026-08-26T00:02:00Z',requests:1,succeeded:0,failed:1 },
      ] },
    ],
  });
  if (url.pathname === '/api/providers') return json({
    from:'2026-08-26T00:00:00Z', to:'2026-08-26T00:15:00Z', providers:[provider],
  });
  if (url.pathname === '/api/providers/provider-a') return json({
    ...provider,
    connections:provider.connections.map(item => ({
      ...item, runtime:{ ...item.runtime, in_flight:0, cooling_until:'2999-01-01T00:00:00Z', recovery_state:'backing_off', recovery_reason:'upstream EOF', retry_at:'2999-01-01T00:00:05Z' },
    })),
    recent_recovery:[{ request_id:'req-recovered', at:'2026-08-26T00:14:00Z', from:'connection-old', to:'connection-a', action:'connection_rebind', result:'recovered_success', overhead_ms:50 }],
  });
  if (method === 'POST' && url.pathname === '/api/providers/provider-a/models/sync') return json({
    status:'succeeded',discovered_models:4,added_models:1,created_logical_models:1,added_model_ids:['upstream-new'],created_logical_model_ids:['upstream-new'],
  });
  if (url.pathname === '/api/models') return json({
    from:'2026-08-26T00:00:00Z', to:'2026-08-26T00:15:00Z',
    models:[{
      id:'smart', display_name:'Smart', enabled:true, state:'recovering', existing_conversation_strategy:'migrate',
      traffic:{ requests:30, direct_traffic:20, recovery_traffic:8, failed:2, avg_ttft_ms:135, avg_duration_ms:420 },
      default_route:[{ provider_id:'provider-a', upstream_model:'upstream', runtime_state:'available', eligible_connections:1, total_connections:1 }],
    }],
  });
  if (url.pathname === '/api/requests') return json({ requests:[{
    id:'req-recovered', timestamp:'2026-08-26T00:14:00Z', caller:'key-a', logical_model_id:'smart',
    conversation_id:'conv-a', provider_id:'provider-a', connection_id:'connection-a', upstream_model:'upstream',
    response_model:'upstream-observed', service_tier:'fast', response_service_tier:'priority',
    outcome:'RECOVERED_SUCCESS', status:'success', attempts:2, stream:true, ttft_ms:135, duration_ms:420,
    usage:{ input_tokens:10, output_tokens:5, cache_read_tokens:8, cache_write_tokens:1, reasoning_tokens:2 }, connection_rebind_count:1,
  }] });
  if (url.pathname === '/api/requests/req-recovered') return json({
    client_request:{ id:'req-recovered', timestamp:'2026-08-26T00:14:00Z', caller:'key-a', logical_model_id:'smart', conversation_id:'conv-a', provider_id:'provider-a', upstream_model:'upstream', response_model:'upstream-observed', service_tier:'fast', response_service_tier:'priority', outcome:'RECOVERED_SUCCESS', status:'success', stream:true, ttft_ms:135, duration_ms:420, usage:{ input_tokens:10, output_tokens:5, cache_read_tokens:8, cache_write_tokens:1, reasoning_tokens:2 } },
    attempts:[
      { sequence:1, timestamp:'2026-08-26T00:14:00Z', provider_id:'provider-a', connection_id:'connection-old', upstream_model:'upstream', status:'failed', latency_ms:40, failure:{ message:'rate limited' } },
      { sequence:2, timestamp:'2026-08-26T00:14:01Z', provider_id:'provider-a', connection_id:'connection-a', upstream_model:'upstream', response_model:'upstream-observed', service_tier:'fast', response_service_tier:'priority', protocol:'openai-responses', status:'success', latency_ms:200, ttft_ms:90, rate_card_version:'rate-v3', plan_kind:'subscription', estimated_cash_cost:0.0042, allocated_sub_cost:0.0001, usage:{ input_tokens:10, output_tokens:5, cache_read_tokens:8, cache_write_tokens:1 } },
    ],
    execution_trace_events:[
      { at:'2026-08-26T00:14:00.000Z', type:'MODEL_RESOLVED', message:'smart' },
      { at:'2026-08-26T00:14:00.100Z', type:'ROUTE_RESOLVED', message:'route-fingerprint' },
      { at:'2026-08-26T00:14:00.200Z', type:'AFFINITY_FOUND', provider_id:'provider-a', connection_id:'connection-a', upstream_model:'upstream', protocol:'openai-chat', affinity_scope:'CALLER', affinity_source:'api_key_identity' },
      { at:'2026-08-26T00:14:00.300Z', type:'AFFINITY_REUSED', provider_id:'provider-a', connection_id:'connection-a', upstream_model:'upstream', protocol:'openai-chat', affinity_scope:'CALLER', affinity_source:'api_key_identity' },
      { at:'2026-08-26T00:14:00.400Z', type:'TARGET_SKIPPED', route_step:2, provider_id:'provider-a', connection_id:'connection-responses', upstream_model:'upstream', protocol:'openai-responses', message:'reasoning state is not lossless' },
      { at:'2026-08-26T00:14:00.450Z', type:'TARGET_SKIPPED', route_step:3, provider_id:'provider-b', connection_id:'connection-b', upstream_model:'upstream', protocol:'openai-chat', message:'excluded by routing policy' },
      { at:'2026-08-26T00:14:00.500Z', type:'CONNECTION_SELECTED', route_step:1, provider_id:'provider-a', connection_id:'connection-a', upstream_model:'upstream', protocol:'openai-chat' },
      { at:'2026-08-26T00:14:00.600Z', type:'ATTEMPT_FAILED', attempt:1, provider_id:'provider-a', connection_id:'connection-old', failure_type:'RATE_LIMIT' },
      { at:'2026-08-26T00:14:01Z', type:'CONNECTION_REBOUND', attempt:2, provider_id:'provider-a', connection_id:'connection-a' },
    ],
    resource_impact:{ connection_recovery_count:1, recovery_fallback_count:0 }, continuity:{ conversation_migration_count:0 },
    client_impact:{ recovery_added_latency_ms:50 }, runtime_revision:12,
  });
  if (method === 'GET' && url.pathname === '/api/api-keys') return json({ api_keys:[{
    identity:{ id:'key-a', name:'Frontend', enabled:true, created_at:'2026-08-20T00:00:00Z' },
    secrets:[{ id:'secret-a', preview:'sk-ferry-…beef', state:'ACTIVE', revealable:true }], usage:{ requests:30 },
  }] });
  if (method === 'GET' && url.pathname === '/api/api-keys/key-a') return json({
    identity:{ id:'key-a', name:'Frontend', enabled:true, created_at:'2026-08-20T00:00:00Z' },
    secrets:[{ id:'secret-a', preview:'sk-ferry-…beef', state:'ACTIVE', revealable:true, created_at:'2026-08-20T00:00:00Z' }],
    usage:{ requests:30 }, recent_requests:[{ id:'req-recovered', timestamp:'2026-08-26T00:14:00Z' }], last_request_at:'2026-08-26T00:14:00Z',
  });
  if (method === 'POST' && url.pathname === '/api/api-keys') return json({
    secret:'sk-ferry-one-time-secret', secret_preview:'sk-ferry-…once',
    identity:{ id:'key-new', name:body.name, description:body.description, enabled:true, created_at:'2026-08-26T00:00:00Z' },
  }, 201);
  if (method === 'GET' && url.pathname === '/api/api-keys/key-a/secret') return json({
    api_key_id:'key-a', name:'Frontend', secret:'sk-ferry-persisted-secret',
  });
  if (url.pathname === '/api/organizations') return json({ organization_scope_supported:false, organizations:[{ id:'local', name:'Local Ferry', role:'Owner', members:null }] });
  if (url.pathname === '/api/revisions') return json({ revisions:[{ revision:12, created_at:'2026-08-26T00:00:00Z', active:true }] });
  if (url.pathname === '/api/usage-billing') return json({
    summary:{ requests:10, input_tokens:100, output_tokens:50, tokens_observed:true, total_cost:0.125, cost_observed:true }, outcomes:{ recovered_success:2 },
    by_provider:{ 'provider-a':{ est_cash_cost:0.125, cost_observed:true } }, by_model:{ smart:{ est_cash_cost:0.125, cost_observed:true } },
  });
  if (url.pathname.endsWith('/routing/review')) return json({
    draft_id:'routing-draft', expected_revision:12, expected_next_revision:13,
    before:[{ provider_id:'provider-a', upstream_model:'upstream' }], after:body.routes,
    expected_traffic_per_minute:2, active_conversations:3, conversation_strategy:body.conversation_strategy,
  });
  if (url.pathname.endsWith('/simulate')) return json({
    logical_model_id:'smart', matched_override:'afternoon-route', simulated_at:body.at,
    default_route:{ steps:[{ provider_id:'provider-a', upstream_model:'upstream' }] },
    effective_route:{ steps:[{ provider_id:'provider-a', upstream_model:'upstream' }] },
    connection_candidates:[{ provider_id:'provider-a', connection_id:'connection-a', upstream_model:'upstream', eligible:true, rank:1 }],
    expected_selection:{ provider_id:'provider-a', connection_id:'connection-a', upstream_model:'upstream' },
  });
  if (url.pathname.endsWith('/routing/apply')) return json({ error:{ code:'REVISION_CONFLICT', message:'Runtime revision changed.', current_revision:13, retryable:true } }, 409);
  return json({ error:`Unhandled ${method} ${url.pathname}` }, 500);
}));

const overview = await readAdapter.getOverview();
assert.equal(overview.systemStatus, 'Available');
assert.equal(overview.runtimeStatus, 'Available');
assert.equal(overview.clientStatus, 'Degraded');
assert.equal(overview.configurationStatus, 'Needs Attention');
assert.equal(overview.overallStatus, 'Available');
assert.equal(overview.medianLatency, '225ms');
assert.equal(overview.recoveredPct, 20);
assert.equal(overview.totalTokens, '150');
assert.equal(overview.estimatedCost, '$0.125');
assert.equal(overview.cacheHit.overall.hitRate, 75);
assert.equal(overview.cacheHit.overall.observedRequests, 9);
assert.equal(overview.cacheHit.apiKeys[0].id, 'key-a');
assert.equal(overview.cacheHit.apiKeys[0].eligibleInputTokens, 1600);
assert.equal(overview.cacheHit.logicalModels[0].hitRate, 75);
assert.equal(overview.trafficTrends.logicalModels.length, 1);
assert.equal(overview.trafficTrends.logicalModels[0].requests, 3);
assert.equal(overview.trafficTrends.logicalModels[0].successRate, 2 / 3 * 100);
assert.equal(overview.trafficTrends.providers.length, 2);
assert.equal(overview.trafficTrends.providerModels[1].label, 'provider-b / backup');

const models = await readAdapter.getModels();
assert.equal(models[0].routes[0].provider, 'Provider A');
assert.equal(models[0].routes[0].status, 'Route available');
assert.equal(models[0].conversationStrategy, 'migrate');
assert.equal(models[0].clientSuccess, '93.33%');
assert.equal(models[0].firstToken, '135ms');
assert.equal(models[0].completion, '420ms');
assert.equal(models[0].enabled, true);

const filtered = await readAdapter.getRequests({ q:'conv-a', conversation_id:'conv-a', model:'smart', provider:'provider-a', outcome:'RECOVERED_SUCCESS' });
assert.equal(filtered[0].result, 'Success (Recovered)');
assert.equal(filtered[0].streamMode, 'Streaming');
assert.equal(filtered[0].ttft, '135ms');
assert.equal(filtered[0].totalDuration, '420ms');
assert.equal(filtered[0].upstreamModel, 'upstream');
assert.equal(filtered[0].responseModel, 'upstream-observed');
assert.equal(filtered[0].serviceTier, 'fast');
assert.equal(filtered[0].responseServiceTier, 'priority');
assert.equal(filtered[0].tokenSummary, '10 in · 5 out');
assert.equal(filtered[0].cacheSummary, '8 read · 1 write');
assert.equal(filtered[0].reasoningTokens, 2);
assert.equal(filtered[0].usageObserved, true);
assert.equal(readAdapter.mapRequest({ id:'usage-unobserved' }).usageObserved, false);
const requestUrl = requestsSeen.find(call => call.url.pathname === '/api/requests').url;
assert.equal(requestUrl.searchParams.get('q'), 'conv-a');
assert.equal(requestUrl.searchParams.get('conversation_id'), 'conv-a');
assert.equal(requestUrl.searchParams.get('provider'), 'provider-a');

const detail = await readAdapter.getRequest('req-recovered');
assert.equal(detail.upstreamModel, 'upstream');
assert.equal(detail.responseModel, 'upstream-observed');
assert.equal(detail.serviceTier, 'fast');
assert.equal(detail.responseServiceTier, 'priority');
assert.equal(detail.routePath.length, 2);
assert.equal(detail.routePath[0].failure, 'rate limited');
assert.equal(detail.routePath[0].responseModel, '');
assert.equal(detail.routePath[0].responseServiceTier, '');
assert.equal(detail.routePath[1].model, 'upstream');
assert.equal(detail.routePath[1].responseModel, 'upstream-observed');
assert.equal(detail.routePath[1].serviceTier, 'fast');
assert.equal(detail.routePath[1].responseServiceTier, 'priority');
assert.equal(detail.runtimeRevision, 12);
assert.equal(detail.routePath[1].ttft, '90ms');
assert.equal(detail.routePath[1].rateCardVersion, 'rate-v3');
assert.equal(detail.routePath[1].planKind, 'subscription');
assert.equal(detail.routePath[1].estimatedCashCost, 0.0042);
assert.equal(detail.routePath[1].allocatedSubCost, 0.0001);
assert.equal(detail.routePath[1].latency, '200ms');
assert.equal(detail.routePath[1].cacheSummary, '8 read · 1 write');
assert.equal(detail.connectionRebinds, 1);
assert.equal(detail.streamMode, 'Streaming');
assert.equal(detail.ttft, '135ms');
assert.equal(detail.totalDuration, '420ms');
assert.deepEqual(detail.trace.slice(0,7).map(event => event.title), [
  'Logical Model Resolved', 'Route Plan Resolved', 'Caller Affinity Found',
  'Caller Affinity Reused', 'Candidate Excluded', 'Candidate Excluded', 'Connection Selected',
]);
assert.equal(detail.trace[2].sub, 'provider-a · upstream · connection-a · openai-chat · Caller affinity (API Key Identity)');
assert.equal(detail.trace[4].sub, 'provider-a · upstream · connection-responses · openai-responses · Route 2 · reasoning state is not lossless');
assert.equal(detail.trace[6].sub, 'provider-a · upstream · connection-a · openai-chat · Route 1');
assert.deepEqual(detail.trace.slice(2,9).map(event => event.kind), [
  'normal', 'success', 'warning', 'excluded', 'selected', 'error', 'warning',
]);
const fallbackTraceAdapter = new HttpFerryAdapter('', makeFetch(({ url }) => {
  if (url.pathname === '/api/requests/req-fallback') return json({
    client_request:{ id:'req-fallback', timestamp:'2026-08-26T00:14:00Z', caller:'key-a', logical_model_id:'smart', outcome:'DIRECT_SUCCESS', status:'success', duration_ms:2400 },
    attempts:[{ sequence:1, timestamp:'2026-08-26T00:14:01Z', provider_id:'provider-a', connection_id:'connection-a', upstream_model:'upstream', status:'success', latency_ms:1400 }],
    execution_trace_events:[], resource_impact:{}, continuity:{}, client_impact:{}, runtime_revision:12,
  });
  return json({ error:'unexpected path' }, 500);
}));
const fallbackDetail = await fallbackTraceAdapter.getRequest('req-fallback');
assert.notEqual(fallbackDetail.trace.at(-1).time, fallbackDetail.trace[0].time, 'fallback completion time must advance by request duration');
assert.equal(fallbackDetail.trace.at(-1).title, 'Response Completed');

// Live traces have no persisted attempts. Each stage must display its own
// elapsed time, and retries must not pair with an earlier attempt's start.
const phaseEvents = [
  [0,'BODY_RECEIVE_STARTED'], [10,'BODY_RECEIVE_COMPLETED'],
  [10,'BODY_PARSE_STARTED'], [30,'BODY_PARSE_COMPLETED'],
  [30,'PLANNING_STARTED'], [35,'COMPATIBILITY_STARTED'],
  [65,'COMPATIBILITY_COMPLETED'], [70,'PLANNING_COMPLETED'],
  [70,'ATTEMPT_STARTED',1], [170,'ADMISSION_COMPLETED',1],
  [200,'ATTEMPT_STARTED',2], [250,'ADMISSION_COMPLETED',2], [320,'UPSTREAM_FIRST_EVENT',2],
].map(([ms,type,attempt]) => ({at:new Date(Date.UTC(2026,8,1)+ms).toISOString(),type,attempt}));
const phaseBody = {
  client_request:{id:'req-pending-phases',timestamp:phaseEvents[0].at,status:'received',live:true,duration_ms:9000},
  attempts:[],execution_trace_events:phaseEvents,
};
const phaseAdapter = new HttpFerryAdapter('', makeFetch(() => json(phaseBody)));
let phaseDetail = await phaseAdapter.getRequest('req-pending-phases');
assert.deepEqual(phaseDetail.trace.map(event => event.latency), [
  '—','10ms','—','20ms','—','—','30ms','40ms','—','100ms','—','50ms','70ms',
]);
assert.equal(phaseDetail.trace[1].title,'Request Body Received');
// Completed attempts must not override phase durations either. COMPLETE alone
// uses the total request duration, even when associated with an attempt.
phaseBody.attempts = [{sequence:2,latency_ms:1234}];
phaseBody.client_request.status = 'success';
phaseBody.execution_trace_events = [...phaseEvents,
  {at:new Date(Date.UTC(2026,8,1)+400).toISOString(),type:'COMPLETE',attempt:2},
  {at:new Date(Date.UTC(2026,8,1)+500).toISOString(),type:'UPSTREAM_FIRST_EVENT',attempt:3},
];
phaseDetail = await phaseAdapter.getRequest('req-pending-phases');
assert.equal(phaseDetail.trace[11].latency,'50ms');
assert.equal(phaseDetail.trace[12].latency,'70ms');
assert.equal(phaseDetail.trace[13].latency,'9.00s');
assert.equal(phaseDetail.trace[14].latency,'—','missing phase start must not invent a duration');

const providerDetail = await readAdapter.getProvider('provider-a');
assert.equal(providerDetail.connections[0].status, 'Cooling');
assert.equal(providerDetail.connections[0].recoveryState, 'backing_off');
assert.equal(providerDetail.connections[0].recoveryReason, 'upstream EOF');
assert.equal(providerDetail.connections[0].retryAt, '2999-01-01T00:00:05Z');
assert.equal(providerDetail.connections[0].enabled, true);
assert.equal(providerDetail.liveRecovery[0].action, 'connection_rebind');
assert.equal(providerDetail.provider.raw.scheduled_message.upstream_model, 'upstream');
assert.equal(providerDetail.provider.raw.model_sync_interval, '30m');
assert.equal(providerDetail.provider.raw.model_sync.created_logical_models, 1);
assert.deepEqual(providerDetail.provider.raw.model_sync_catalog,{type:'volcengine-plan',plan:'agent',credential_ref:'store:ark-control'});
const syncResult=await readAdapter.syncProviderModels('provider-a');
assert.equal(syncResult.discovered_models,4);
assert.equal(syncResult.added_models,1);
assert.equal(syncResult.created_logical_models,1);
assert.deepEqual(syncResult.created_logical_model_ids,['upstream-new']);
const syncRequest=requestsSeen.find(call=>call.url.pathname==='/api/providers/provider-a/models/sync');
assert.equal(syncRequest.method,'POST');
assert.equal(syncRequest.body,null,'manual model sync has no request body');
const providerList = await readAdapter.getProviders();
assert.equal(providerList[0].enabled, true);
assert.equal(providerList[0].connectionItems[0].id, 'connection-a');
assert.equal(providerList[0].lastSuccessfulAt, '2026-08-26T00:14:00Z');

const keys = await readAdapter.getApiKeys();
assert.equal(keys[0].preview, 'sk-ferry-…beef');
assert.equal(keys[0].revealable, true);
assert.equal(JSON.stringify(keys).includes('secret-a'), true, 'secret metadata is allowed');
assert.equal(JSON.stringify(keys).includes('raw-secret'), false, 'raw secrets must never be synthesized');
const oneTimeKey = await readAdapter.createApiKey({ name:'New frontend', description:'test' });
assert.equal(oneTimeKey.key, 'sk-ferry-one-time-secret');
assert.equal(oneTimeKey.item.preview, 'sk-ferry-…once');
assert.equal(JSON.stringify(await readAdapter.getApiKeys()).includes(oneTimeKey.key), false, 'one-time secret must not reappear in lists');
const persistedKey = await readAdapter.revealApiKey('key-a');
assert.equal(persistedKey.key, 'sk-ferry-persisted-secret');
const keyDetail = await readAdapter.getApiKey('key-a');
assert.equal(keyDetail.lastRequestAt, '2026-08-26T00:14:00Z');
const organizations = await readAdapter.getOrganizations();
assert.equal(organizations[0].tenantIsolation, false);
const billing = await readAdapter.getBilling();
assert.equal(billing.recoveredRequests, 2);

const unknownUsageAdapter = new HttpFerryAdapter('', makeFetch(({ url }) => {
  if (url.pathname === '/api/overview') return json({ client_outcomes:{}, usage_summary:{ requests:1, input_tokens:0, output_tokens:0, total_cost:0 } });
  if (url.pathname === '/api/usage-billing') return json({ summary:{ requests:1, input_tokens:0, output_tokens:0, total_cost:0 }, outcomes:{}, by_provider:{ 'provider-a':{ total_cost:0 } }, by_model:{} });
  return json({ error:'unexpected path' }, 500);
}));
const unknownOverview = await unknownUsageAdapter.getOverview();
assert.equal(unknownOverview.totalTokens, '—');
assert.equal(unknownOverview.estimatedCost, '—');
assert.equal(unknownOverview.tokenRate, '—');
assert.equal(unknownOverview.hasTraffic, false);
const unknownBilling = await unknownUsageAdapter.getBilling();
assert.equal(unknownBilling.totalTokens, '—');
assert.equal(unknownBilling.totalCost, '—');
assert.equal(unknownBilling.providers[0].amount, '—');

const review = await readAdapter.reviewRouting('smart', {
  routes:models[0].routes, conversationStrategy:'migrate',
});
assert.equal(review.draftId, 'routing-draft');
assert.equal(review.expectedRevision, 12);
const routingReviewRequest = requestsSeen.find(request => request.url.pathname.endsWith('/routing/review'));
assert.deepEqual(routingReviewRequest.body, {
  routes:[{ provider_id:'provider-a', upstream_model:'upstream', recover_on:[] }],
  conversation_strategy:'migrate',
});
const simulationInput = {
  caller_id:'key-a', protocol:'openai-responses', stream:true,
  at:'2026-08-30T04:00:00.000Z',
  required_capabilities:{ tools:true, min_context:32768, min_output:8192 },
};
const simulation = await readAdapter.simulateModel('smart', simulationInput);
assert.equal(simulation.matched_override, 'afternoon-route');
assert.equal(simulation.expected_selection.connection_id, 'connection-a');
const simulationRequest = requestsSeen.find(request => request.url.pathname.endsWith('/simulate'));
assert.equal(simulationRequest.method, 'POST');
assert.deepEqual(simulationRequest.body, simulationInput);
await assert.rejects(
  readAdapter.applyRouting('smart', review),
  error => error instanceof FerryError && error.status === 409 && error.code === 'REVISION_CONFLICT' && error.currentRevision === 13,
);

const failedAdapter = new HttpFerryAdapter('', async () => json({
  error:{ code:'CAPABILITY_UNAVAILABLE', message:'Ledger is unavailable.', retryable:true },
}, 503));
await assert.rejects(
  failedAdapter.getOverview(),
  error => error instanceof FerryError && error.status === 503 && error.code === 'CAPABILITY_UNAVAILABLE' && error.message === 'Ledger is unavailable.',
);

let managementToken = { id:'mtok_skill', name:'Ferry Skill', preview:'smt_…beef', enabled:true, allow_high_risk:false, created_at:'2026-09-02T00:00:00Z', updated_at:'2026-09-02T00:00:00Z' };
const managementCalls = [];
const managementAdapter = new HttpFerryAdapter('', makeFetch(({ url, method, body }) => {
  managementCalls.push({path:url.pathname,method,body});
  if(method==='GET'&&url.pathname==='/api/security-events')return json({events:[{event_type:'management_token_used'}]});
  if(method==='GET'&&url.pathname==='/api/management-tokens')return json({management_tokens:[managementToken]});
  if(method==='POST'&&url.pathname==='/api/management-tokens')return json({management_token:{...managementToken,name:body.name,allow_high_risk:body.allow_high_risk},secret:'smt_one_time'},201);
  if(method==='PATCH'&&url.pathname==='/api/management-tokens/mtok_skill'){managementToken={...managementToken,...body};return json({management_token:managementToken});}
  if(method==='POST'&&url.pathname==='/api/management-tokens/mtok_skill/rotate')return json({management_token:{...managementToken,preview:'smt_…cafe'},secret:'smt_rotated'});
  if(method==='POST'&&url.pathname==='/api/management-tokens/mtok_skill/revoke'){managementToken={...managementToken,enabled:false,revoked_at:'2026-09-02T01:00:00Z'};return json({management_token:managementToken});}
  if(method==='DELETE'&&url.pathname==='/api/management-tokens/mtok_skill')return new Response(null,{status:204});
  return json({error:`Unhandled ${method} ${url.pathname}`},500);
}));
assert.equal((await managementAdapter.getSecurityEvents())[0].event_type,'management_token_used');
assert.equal((await managementAdapter.getManagementTokens())[0].id,'mtok_skill');
const createdManagementToken=await managementAdapter.createManagementToken({name:'automation',allow_high_risk:false});
assert.equal(createdManagementToken.secret,'smt_one_time');
assert.equal(createdManagementToken.token.name,'automation');
assert.equal((await managementAdapter.updateManagementToken('mtok_skill',{allow_high_risk:true})).allow_high_risk,true);
assert.equal((await managementAdapter.rotateManagementToken('mtok_skill')).secret,'smt_rotated');
assert.ok((await managementAdapter.revokeManagementToken('mtok_skill')).revoked_at);
await managementAdapter.deleteManagementToken('mtok_skill');
assert.ok(managementCalls.every(call=>call.path.startsWith('/api/')), 'management token and security-event calls must use only product management routes');

let draftCounter = 0;
let providerCreated = false;
let connectionCreated = false;
let connectionCreatedID = '';
let secretValueSeen = '';
const mutationAdapter = new HttpFerryAdapter('', makeFetch(({ url, method, body }) => {
  if (method === 'POST' && url.pathname === '/api/providers') {
    providerCreated = true;
    assert.equal(body.type, 'custom');
    assert.equal(body.base_url, 'https://new-provider.test/v1');
    assert.equal(body.model_sync_interval, '30m');
    assert.deepEqual(body.scheduled_message, {enabled:true,upstream_model:'upstream',message:'scheduled heartbeat'});
    if (body.connection) {
      connectionCreated = true;
      connectionCreatedID = body.connection.id;
      assert.match(body.connection.credential.secret_ref, /^store:/);
      assert.equal(body.connection.models[0].upstream_model, 'upstream');
    }
    return json({ draft_id:`draft-${++draftCounter}` }, 202);
  }
  if (method === 'POST' && url.pathname.startsWith('/api/secrets/')) {
    secretValueSeen = body.value;
    return json({ stored:true });
  }
  if (method === 'POST' && url.pathname === '/api/providers/new-provider/connections') {
    connectionCreated = true;
    connectionCreatedID = body.id;
    assert.match(body.credential.secret_ref, /^store:/);
    assert.equal(body.models[0].upstream_model, 'upstream');
    return json({ draft_id:`draft-${++draftCounter}` }, 202);
  }
  if (method === 'GET' && url.pathname.startsWith('/api/drafts/')) return json({ base_revision:1 });
  if (method === 'POST' && url.pathname.endsWith('/validate')) return json({ valid:true, errors:[] });
  if (method === 'POST' && url.pathname.endsWith('/apply')) return json({ revision:2 });
  if (method === 'GET' && url.pathname === '/api/providers') return json({
    from:'2026-08-26T00:00:00Z', to:'2026-08-26T00:15:00Z', providers:providerCreated ? [{ ...provider, id:'new-provider', name:'New Provider', connections:connectionCreated ? provider.connections.map(item => ({ ...item, id:connectionCreatedID })) : [] }] : [],
  });
  if (method === 'GET' && url.pathname === '/api/providers/new-provider') return json({
    ...provider, id:'new-provider', name:'New Provider', connections:connectionCreated ? provider.connections.map(item => ({ ...item, id:connectionCreatedID })) : [],
  });
  return json({ error:`Unhandled ${method} ${url.pathname}` }, 500);
}));

const createdProvider = await mutationAdapter.addProvider({
  name:'New Provider', type:'custom', baseUrl:'https://new-provider.test/v1', authType:'API Key',
  account:'primary', models:'upstream', protocol:'openai-chat', secret:'top-secret-value',
  modelSyncInterval:'30m',
  scheduledMessage:{enabled:true,upstream_model:'upstream',message:'scheduled heartbeat'},
});
assert.equal(createdProvider.id, 'new-provider');
assert.equal(connectionCreatedID, 'new-provider');
assert.equal(secretValueSeen, 'top-secret-value');

let catalogCreateBody,catalogCreateSecret='';
const catalogCreateAdapter=new HttpFerryAdapter('',makeFetch(({url,method,body})=>{
  if(method==='POST'&&url.pathname.startsWith('/api/secrets/')){catalogCreateSecret=body.value;return json({stored:true})}
  if(method==='POST'&&url.pathname==='/api/providers'){catalogCreateBody=body;return json({draft_id:'catalog-create-draft'},202)}
  if(method==='GET'&&url.pathname==='/api/drafts/catalog-create-draft')return json({base_revision:1});
  if(method==='POST'&&url.pathname.endsWith('/validate'))return json({valid:true,errors:[]});
  if(method==='POST'&&url.pathname.endsWith('/apply'))return json({revision:2});
  if(method==='GET'&&url.pathname==='/api/providers')return json({from:'2026-08-26T00:00:00Z',to:'2026-08-26T00:15:00Z',providers:[{...provider,id:'volcengine-plan',name:'Volcengine Plan',model_sync_catalog:catalogCreateBody.model_sync_catalog}]});
  return json({error:`Unhandled ${method} ${url.pathname}`},500);
}));
await catalogCreateAdapter.addProvider({
  name:'Volcengine Plan',type:'custom',baseUrl:'https://ark.cn-beijing.volces.com/api/plan/v3',credentialRef:'store:data-plane',enabled:true,modelSyncInterval:'1h',
  modelSyncCatalog:{type:'volcengine-plan',plan:'agent',credentialRef:''},modelSyncCatalogCredential:{access_key_id:'AK_CREATE',secret_access_key:'SK_CREATE'},
  protocols:[{protocol:'openai-responses',models:['ark-code-latest'],autoConvert:true,credentialRef:'store:data-plane'}],
});
assert.deepEqual(JSON.parse(catalogCreateSecret),{access_key_id:'AK_CREATE',secret_access_key:'SK_CREATE'});
assert.equal(catalogCreateBody.model_sync_catalog.type,'volcengine-plan');
assert.equal(catalogCreateBody.model_sync_catalog.plan,'agent');
assert.notEqual(catalogCreateBody.model_sync_catalog.credential_ref,catalogCreateBody.connections[0].credential.secret_ref,'control-plane and inference credentials must remain separate');
assert.doesNotMatch(JSON.stringify(catalogCreateBody),/AK_CREATE|SK_CREATE/);

let discoveryInput;
const discoveryAdapter = new HttpFerryAdapter('', makeFetch(({ url, method, body }) => {
  if (method === 'POST' && url.pathname === '/api/providers/discover-models') {
    discoveryInput = body;
    return json({ models:['upstream-a','upstream-b'], source:'upstream' });
  }
  return json({ error:`Unhandled ${method} ${url.pathname}` }, 500);
}));
assert.deepEqual(await discoveryAdapter.discoverProviderModels({
  type:'custom', baseUrl:'https://provider.test/v1', protocol:'openai-chat', secret:'temporary-secret',
}), ['upstream-a','upstream-b']);
assert.deepEqual(discoveryInput, {
  type:'custom', base_url:'https://provider.test/v1', protocol:'openai-chat', secret:'temporary-secret', credential_ref:'',
});

let protocolDiscoveryInput;
const protocolDiscoveryAdapter = new HttpFerryAdapter('', makeFetch(({ url, method, body }) => {
  if (method === 'POST' && url.pathname === '/api/providers/discover-protocols') {
    protocolDiscoveryInput = body;
    return json({
      models:['chat-native','responses-native'],
      protocols:[
        {protocol:'openai-chat',status:'verified',models:['chat-native'],model_results:[{
          model:'chat-native',status:'verified',cache_hit:true,probe_requests:0,
          capabilities:{text:true,stream:true,structured_output:true},
          capability_evidence:[{capability:'structured_output',status:'supported',source:'cache'}],
        }]},
        {protocol:'openai-responses',status:'verified',models:['responses-native']},
        {protocol:'anthropic',status:'unconfigured',models:[]},
      ],
    });
  }
  return json({ error:`Unhandled ${method} ${url.pathname}` }, 500);
}));
const protocolDiscovery = await protocolDiscoveryAdapter.discoverProviderProtocols({
  providerId:'provider-a',type:'custom',baseUrl:'https://provider.test/v1',secret:'temporary-secret',refresh:false,
  protocols:[
    {protocol:'openai-chat'},
    {protocol:'openai-responses',urlMode:'full',fullUrl:'https://responses.test/custom/invoke',credentialRef:'store:responses'},
    {protocol:'anthropic'},
  ],
});
assert.equal(protocolDiscovery.protocols[2].status,'unconfigured');
assert.equal(protocolDiscovery.protocols[0].model_results[0].capability_evidence[0].status,'supported');
assert.equal(protocolDiscovery.protocols[0].model_results[0].cache_hit,true);
assert.equal(protocolDiscoveryInput.provider_id,'provider-a');
assert.equal(protocolDiscoveryInput.refresh,false);
assert.deepEqual(protocolDiscoveryInput.protocols[1],{
  protocol:'openai-responses',base_url:'',full_url:'https://responses.test/custom/invoke',secret:'',credential_ref:'store:responses',models:[],
});

let protocolConfigBody,protocolCatalogSecretValue='',protocolCatalogSecretPath='';
const protocolConfigAdapter = new HttpFerryAdapter('', makeFetch(({ url, method, body }) => {
  if(method==='POST'&&url.pathname.startsWith('/api/secrets/')){
    protocolCatalogSecretPath=url.pathname;protocolCatalogSecretValue=body.value;return json({stored:true});
  }
  if (method === 'PUT' && url.pathname === '/api/providers/provider-a/protocol-config') {
    protocolConfigBody = body;
    return json({draft_id:'protocol-config-draft'},202);
  }
  if (method === 'GET' && url.pathname === '/api/drafts/protocol-config-draft') return json({base_revision:4});
  if (method === 'POST' && url.pathname.endsWith('/validate')) return json({valid:true,errors:[]});
  if (method === 'POST' && url.pathname.endsWith('/apply')) return json({revision:5});
  if (method === 'GET' && url.pathname === '/api/providers/provider-a') return json(provider);
  return json({ error:`Unhandled ${method} ${url.pathname}` }, 500);
}));
await protocolConfigAdapter.saveProviderProtocolConfig('provider-a',{
  name:'Provider A',baseUrl:'https://provider.test/v1',credentialRef:'store:shared',enabled:true,
  modelSyncInterval:'off',
  modelSyncCatalog:{type:'volcengine-plan',plan:'coding',credentialRef:'store:old-ark-control'},
  modelSyncCatalogCredential:{access_key_id:'AK_CONTROL_TEST',secret_access_key:'SK_CONTROL_TEST',session_token:'STS_CONTROL_TEST'},
  scheduledMessage:{enabled:true,upstream_model:'chat-native',message:'protocol heartbeat'},
  concurrency:{max_concurrent:4,max_queue:12,max_wait_seconds:30,queue_when_full:true,queue_on_throttle:false},
  protocols:[
    {protocol:'openai-chat',connectionId:'connection-a',connectionIdentity:'preserved identity',connectionEnabled:false,
      connectionConcurrency:{max_concurrent:2},autoConvert:true,overrideUrl:false,models:['chat-native','glm-5.3-flash'],modelSpecs:{
        'chat-native':{plan_id:'plan-a',capabilities:{text:true,stream:true,tools:true,parallel_tools:true,reasoning:true,structured_output:false,vision:false,context_window:131072,max_output:8192}},
        'glm-5.3-flash':{capabilities:{text:true,stream:true,tools:true,reasoning:false}},
      }},
    {protocol:'openai-responses',autoConvert:false,overrideUrl:true,urlMode:'full',fullUrl:'https://responses.test/custom/invoke',credentialRef:'store:responses',models:['responses-native']},
    {protocol:'anthropic',autoConvert:true,models:[]},
    {protocol:'openai-images',autoConvert:true,models:[]},
  ],
});
assert.deepEqual(protocolConfigBody.protocol_conversion,{'openai-chat':true,'openai-responses':false,anthropic:true,'openai-images':false});
assert.equal(protocolConfigBody.model_sync_interval,'off');
assert.equal(protocolConfigBody.model_sync_catalog.type,'volcengine-plan');
assert.equal(protocolConfigBody.model_sync_catalog.plan,'coding');
assert.match(protocolConfigBody.model_sync_catalog.credential_ref,/^store:p-provider-a-model-catalog-/);
assert.equal(protocolConfigBody.model_sync_catalog.credential_ref.slice('store:'.length),decodeURIComponent(protocolCatalogSecretPath.split('/').at(-1)));
assert.deepEqual(JSON.parse(protocolCatalogSecretValue),{access_key_id:'AK_CONTROL_TEST',secret_access_key:'SK_CONTROL_TEST',session_token:'STS_CONTROL_TEST'});
assert.doesNotMatch(JSON.stringify(protocolConfigBody),/AK_CONTROL_TEST|SK_CONTROL_TEST|STS_CONTROL_TEST/,'Provider drafts must receive only a secret reference');
assert.deepEqual(protocolConfigBody.scheduled_message,{enabled:true,upstream_model:'chat-native',message:'protocol heartbeat'});
assert.equal(protocolConfigBody.concurrency.max_concurrent,4);
assert.equal(protocolConfigBody.connections.length,2);
assert.equal(protocolConfigBody.connections[0].identity,'preserved identity');
assert.equal(protocolConfigBody.connections[0].enabled,false);
assert.equal(protocolConfigBody.connections[0].concurrency.max_concurrent,2);
assert.equal(protocolConfigBody.connections[0].models[0].capabilities.parallel_tools,true);
assert.equal(protocolConfigBody.connections[0].models[0].capabilities.context_window,131072);
assert.equal(protocolConfigBody.connections[0].models[0].plan_id,'plan-a');
assert.equal(protocolConfigBody.connections[0].models[1].upstream_model,'glm-5.3-flash');
assert.equal(protocolConfigBody.connections[0].models[1].capabilities.reasoning,false);
assert.equal(protocolConfigBody.connections[1].base_url,'');
assert.equal(protocolConfigBody.connections[1].full_url,'https://responses.test/custom/invoke');
assert.equal(protocolConfigBody.connections[1].credential.secret_ref,'store:responses');

let failedCatalogSecretPath='',deletedFailedCatalogSecretPath='';
const rejectedCatalogAdapter=new HttpFerryAdapter('',makeFetch(({url,method,body})=>{
  if(method==='POST'&&url.pathname.startsWith('/api/secrets/')){failedCatalogSecretPath=url.pathname;assert.deepEqual(JSON.parse(body.value),{access_key_id:'AK_REJECTED',secret_access_key:'SK_REJECTED'});return json({stored:true})}
  if(method==='PUT'&&url.pathname==='/api/providers/provider-a/protocol-config'){
    assert.doesNotMatch(JSON.stringify(body),/AK_REJECTED|SK_REJECTED/);return json({draft_id:'rejected-catalog-draft'},202)
  }
  if(method==='GET'&&url.pathname==='/api/drafts/rejected-catalog-draft')return json({base_revision:7});
  if(method==='POST'&&url.pathname.endsWith('/validate'))return json({valid:false,errors:[{message:'rejected'}]});
  if(method==='DELETE'&&url.pathname.startsWith('/api/secrets/')){deletedFailedCatalogSecretPath=url.pathname;return json({deleted:true})}
  return json({error:`Unhandled ${method} ${url.pathname}`},500);
}));
await assert.rejects(rejectedCatalogAdapter.saveProviderProtocolConfig('provider-a',{
  name:'Provider A',baseUrl:'https://provider.test/v1',credentialRef:'store:shared',enabled:true,modelSyncInterval:'1h',
  modelSyncCatalog:{type:'volcengine-plan',plan:'agent',credentialRef:''},modelSyncCatalogCredential:{access_key_id:'AK_REJECTED',secret_access_key:'SK_REJECTED'},
  protocols:[{protocol:'openai-chat',connectionId:'connection-a',connectionIdentity:'primary',connectionEnabled:true,autoConvert:true,overrideUrl:false,models:['upstream']}],
}),error=>error instanceof FerryError&&error.code==='VALIDATION_FAILURE');
assert.equal(deletedFailedCatalogSecretPath,failedCatalogSecretPath,'a newly stored control-plane credential must be removed when its Provider draft is rejected');

const longNumericProviderID=`9${'very-long-provider-id-'.repeat(5)}`,encodedLongProviderID=encodeURIComponent(longNumericProviderID);
let uncertainCatalogSecretPath='',uncertainCatalogDeleted=false,applyWasSent=false;
const uncertainApplyAdapter=new HttpFerryAdapter('',makeFetch(({url,method})=>{
  if(method==='POST'&&url.pathname.startsWith('/api/secrets/')){uncertainCatalogSecretPath=url.pathname;return json({stored:true})}
  if(method==='PUT'&&url.pathname===`/api/providers/${encodedLongProviderID}/protocol-config`)return json({draft_id:'uncertain-catalog-draft'},202);
  if(method==='GET'&&url.pathname==='/api/drafts/uncertain-catalog-draft')return json({base_revision:9});
  if(method==='POST'&&url.pathname.endsWith('/validate'))return json({valid:true,errors:[]});
  if(method==='POST'&&url.pathname.endsWith('/apply')){applyWasSent=true;throw new TypeError('network response lost')}
  if(method==='DELETE'&&url.pathname.startsWith('/api/secrets/')){uncertainCatalogDeleted=true;return json({deleted:true})}
  return json({error:`Unhandled ${method} ${url.pathname}`},500);
}));
await assert.rejects(uncertainApplyAdapter.saveProviderProtocolConfig(longNumericProviderID,{
  name:'Long Provider',baseUrl:'https://provider.test/v1',credentialRef:'store:shared',enabled:true,modelSyncInterval:'1h',
  modelSyncCatalog:{type:'volcengine-plan',plan:'coding',credentialRef:''},modelSyncCatalogCredential:{access_key_id:'AK_UNCERTAIN',secret_access_key:'SK_UNCERTAIN'},
  protocols:[{protocol:'openai-chat',connectionId:'connection-a',connectionIdentity:'primary',connectionEnabled:true,autoConvert:true,overrideUrl:false,models:['upstream']}],
}),error=>error instanceof FerryError&&error.code==='APPLY_RESULT_UNCERTAIN'&&/credential was retained; reload/.test(error.message));
const uncertainSecretName=decodeURIComponent(uncertainCatalogSecretPath.split('/').at(-1));
assert.equal(applyWasSent,true);
assert.equal(uncertainCatalogDeleted,false,'a catalog credential must be retained once apply has been sent because the server result may be committed');
assert.ok(uncertainSecretName.length<=64,'generated secret names must fit the server limit');
assert.match(uncertainSecretName,/^[a-z]/,'generated secret names must start with a letter even when the Provider ID does not');

let deletedAppliedSecret = false;
const delayedVisibilityAdapter = new HttpFerryAdapter('', makeFetch(({ url, method }) => {
  if (method === 'POST' && url.pathname.startsWith('/api/secrets/')) return json({ stored:true });
  if (method === 'POST' && url.pathname === '/api/providers/provider-a/connections') return json({ draft_id:'delayed-draft' }, 202);
  if (method === 'GET' && url.pathname === '/api/drafts/delayed-draft') return json({ base_revision:2 });
  if (method === 'POST' && url.pathname.endsWith('/validate')) return json({ valid:true, errors:[] });
  if (method === 'POST' && url.pathname.endsWith('/apply')) return json({ revision:3 });
  if (method === 'GET' && url.pathname === '/api/providers/provider-a') return json({ ...provider, connections:[] });
  if (method === 'DELETE' && url.pathname.startsWith('/api/secrets/')) {
    deletedAppliedSecret = true;
    return json({ deleted:true });
  }
  return json({ error:`Unhandled ${method} ${url.pathname}` }, 500);
}));
await assert.rejects(
  delayedVisibilityAdapter.addConnection('provider-a', {
    authType:'API Key', account:'eventual', models:'upstream', protocol:'openai-chat', secret:'must-remain',
  }),
  error => error instanceof FerryError && error.code === 'CAPABILITY_UNAVAILABLE',
);
assert.equal(deletedAppliedSecret, false, 'an applied connection must retain its stored credential when visibility lags');

let oauthEnabled = false;
const oauthAdapter = new HttpFerryAdapter('', makeFetch(({ url, method, body }) => {
  if (method === 'POST' && url.pathname === '/api/providers/provider-a/connections') {
    assert.equal(body.enabled, false, 'OAuth connection must remain disabled before callback completion');
    assert.match(body.credential.secret_ref, /^oauth:codex:/);
    return json({ draft_id:'oauth-create-draft' }, 202);
  }
  if (method === 'PATCH' && url.pathname === '/api/providers/provider-a/connections/provider-a-oauth-user') {
    assert.equal(body.enabled, true);
    oauthEnabled = true;
    return json({ draft_id:'oauth-enable-draft' }, 202);
  }
  if (method === 'GET' && url.pathname.startsWith('/api/drafts/')) return json({ base_revision:oauthEnabled ? 2 : 1 });
  if (method === 'POST' && url.pathname.endsWith('/validate')) return json({ valid:true, errors:[] });
  if (method === 'POST' && url.pathname.endsWith('/apply')) return json({ revision:oauthEnabled ? 3 : 2 });
  if (method === 'GET' && url.pathname === '/api/providers/provider-a') return json({
    ...provider,
    connections:[{
      ...provider.connections[0], id:'provider-a-oauth-user', identity:'oauth-user', enabled:oauthEnabled,
      credential_ref:'oauth:codex:oauth-user',
    }],
  });
  if (method === 'POST' && url.pathname.endsWith('/oauth/login')) return json({ authorize_url:'https://oauth.test/authorize', account:'oauth-user' });
  if (method === 'POST' && url.pathname.endsWith('/oauth/callback')) {
    assert.equal(body.callback_url, 'https://console.test/oauth/callback?code=ok&state=state');
    return json({ status:'active' });
  }
  return json({ error:`Unhandled ${method} ${url.pathname}` }, 500);
}));
const pendingOAuth = await oauthAdapter.addConnection('provider-a', {
  authType:'OAuth', account:'oauth-user', models:'upstream', protocol:'openai-responses',
});
assert.equal(pendingOAuth.oauthPending, true);
assert.equal(oauthEnabled, false);
await oauthAdapter.completeConnectionOAuth('provider-a', pendingOAuth.id, 'https://console.test/oauth/callback?code=ok&state=state');
assert.equal(oauthEnabled, true, 'OAuth callback completion must apply an explicit enable revision');

const importedAuthJSON = {
  auth_mode:'chatgpt',
  tokens:{ id_token:'id-token', access_token:'access-token', refresh_token:'refresh-token', account_id:'account-123' },
  last_refresh:'2026-08-30T00:00:00Z',
};
let importRequest;
const importedUsage = {
  plan_type:'pro',
  rate_limit:{ allowed:true, limit_reached:false, primary_window:{ used_percent:25, reset_after_seconds:900 } },
  credits:{ has_credits:true, unlimited:false, balance:'42.5' },
  observed_at:'2026-08-30T01:02:03Z',
};
const oauthOperationsAdapter = new HttpFerryAdapter('', makeFetch(({ url, method, body }) => {
  if (method === 'POST' && url.pathname === '/api/providers/provider-a/connections/connection-a/oauth/import') {
    importRequest = body;
    return json({ status:'active', account:{ name:'codex-user', account_id:'account-123', expired:false } });
  }
  if (method === 'GET' && url.pathname === '/api/providers/provider-a/connections/connection-a/oauth/usage') {
    return json({ usage:importedUsage });
  }
  return json({ error:`Unhandled ${method} ${url.pathname}` }, 500);
}));
const authFile = { name:'auth.json', text:async () => JSON.stringify(importedAuthJSON) };
const importedAccount = await oauthOperationsAdapter.importConnectionOAuth('provider-a', 'connection-a', authFile);
assert.deepEqual(importRequest, { auth_json:importedAuthJSON }, 'auth.json import must use the dedicated account-scoped endpoint');
assert.equal(importedAccount.account.account_id, 'account-123');
assert.deepEqual(
  await oauthOperationsAdapter.getConnectionOAuthUsage('provider-a', 'connection-a'),
  importedUsage,
  'usage reads must unwrap the account-scoped snapshot',
);
await assert.rejects(
  oauthOperationsAdapter.importConnectionOAuth('provider-a', 'connection-a', { text:async () => '{invalid' }),
  /not valid JSON/i,
);
await assert.rejects(
  oauthOperationsAdapter.importConnectionOAuth('provider-a', 'connection-a', { text:async () => JSON.stringify({ padding:'x'.repeat(129 * 1024) }) }),
  /128 KiB or smaller/i,
);

console.log('Ferry Console v3 HTTP adapter contract: PASS');

const resetRequests=[];
const resetAdapter=new HttpFerryAdapter('',makeFetch(request=>{resetRequests.push(request);return json({cleared_models:2,persisted:true})}));
for(const scope of [{},{provider_id:'p'},{provider_id:'p',connection_id:'c',upstream_model:'a/b'},{model_id:'a/b'}])await resetAdapter.resetCooldowns(scope);
assert.deepEqual(resetRequests.map(({url,method,body})=>({path:url.pathname,method,body})),[
 {path:'/api/runtime/clear-cooldown',method:'POST',body:{}},
 {path:'/api/runtime/clear-cooldown',method:'POST',body:{provider_id:'p'}},
 {path:'/api/runtime/clear-cooldown',method:'POST',body:{provider_id:'p',connection_id:'c',upstream_model:'a/b'}},
 {path:'/api/models/a%2Fb/clear-cooldown',method:'POST',body:{}}
]);

const codexPublishCalls=[];
const versionAdapter=new HttpFerryAdapter('',makeFetch(({url,method,body})=>{
 codexPublishCalls.push(url.pathname);
 if(url.pathname==='/api/drafts'){assert.equal(body.base_revision,12);assert.deepEqual(body.intent,{codex_client_version:'0.154.0'});return json({id:'version-draft'},201)}
 if(url.pathname==='/api/drafts/version-draft')return json({base_revision:12});
 if(url.pathname.endsWith('/validate'))return json({valid:true});
 if(url.pathname.endsWith('/apply')){assert.equal(body.expected_revision,12);return json({revision:13})}
 if(url.pathname==='/api/settings')return json({revision:13,codex_effective_client_version:'0.154.0'});
 throw new Error('Unexpected request '+url.pathname);
}));
assert.equal((await versionAdapter.publishCodexClientVersion('0.154.0',12)).revision,13);
assert.deepEqual(codexPublishCalls,['/api/drafts','/api/drafts/version-draft','/api/drafts/version-draft/validate','/api/drafts/version-draft/apply','/api/settings']);
await assert.rejects(()=>versionAdapter.publishCodexClientVersion('0.154.0',0),error=>error.status===409);

const automaticCalls=[];
const automaticAdapter=new HttpFerryAdapter('',makeFetch(({url,body})=>{
 automaticCalls.push(url.pathname);
 if(url.pathname==='/api/drafts'){
   assert.equal(body.base_revision,20);
   assert.deepEqual(body.intent,{codex_client_version:'',codex_auto_update:true,codex_release_channel:'prerelease'});
   return json({id:'auto-version-draft'},201);
 }
 if(url.pathname==='/api/drafts/auto-version-draft')return json({base_revision:20});
 if(url.pathname.endsWith('/validate'))return json({valid:true});
 if(url.pathname.endsWith('/apply')){assert.equal(body.expected_revision,20);return json({revision:21})}
 if(url.pathname==='/api/settings')return json({revision:21,codex_auto_update:true,codex_release_channel:'prerelease',codex_client_version:'',codex_update_status:{status:'pending'}});
 throw new Error('Unexpected automatic version request '+url.pathname);
}));
const autoSettings=await automaticAdapter.publishCodexClientVersion('',20,{codex_auto_update:true,codex_release_channel:'prerelease'});
assert.equal(autoSettings.codex_update_status.status,'pending');
assert.deepEqual(automaticCalls,['/api/drafts','/api/drafts/auto-version-draft','/api/drafts/auto-version-draft/validate','/api/drafts/auto-version-draft/apply','/api/settings']);

const demoVersionAdapter=new MockFerryAdapter();
let demoVersionSettings=await demoVersionAdapter.getSettings();
demoVersionSettings=await demoVersionAdapter.publishCodexClientVersion('0.161.0-alpha.10',demoVersionSettings.revision,{codex_auto_update:true,codex_release_channel:'prerelease'});
assert.equal(demoVersionSettings.codex_update_status.status,'manual');
demoVersionSettings=await demoVersionAdapter.publishCodexClientVersion('',demoVersionSettings.revision,{codex_auto_update:true,codex_release_channel:'prerelease'});
assert.equal(demoVersionSettings.codex_effective_client_version,'0.161.0-alpha.10','switching to auto must keep the working version');
demoVersionSettings=await demoVersionAdapter.publishCodexClientVersion('0.161.0-alpha.9',demoVersionSettings.revision);
demoVersionSettings=await demoVersionAdapter.publishCodexClientVersion('',demoVersionSettings.revision);
assert.equal(demoVersionSettings.codex_effective_client_version,'0.161.0-alpha.10','a lower manual pin must not discard the higher saved automatic version');
demoVersionSettings=await demoVersionAdapter.publishCodexClientVersion('',demoVersionSettings.revision,{codex_auto_update:false,codex_release_channel:'stable'});
assert.equal(demoVersionSettings.codex_effective_client_version,'0.161.0-alpha.10');
assert.equal(demoVersionSettings.codex_update_status.status,'disabled');
