export const scenarios = {
  healthy: {
    id: 'healthy', label: 'Healthy', system: 'Healthy', model: 'Healthy', provider: 'Healthy',
    description: 'All providers are serving normally. No recovery is required.',
    outcomes:{direct:1468,recovered:0,failed:0}, modelOutcomes:{direct:679,recovered:0,failed:0},
    failureTrend:[0,0,0,0,0,0,0,0], fallbacks:0, modelTraffic:[100,0,0], needs:0,
    providerRecovered:0, providerFailed:0, usage:{tokens:19_200_000,estimatedCost:168.78},
  },
  protected: {
    id: 'protected', label: 'Protected', system: 'Healthy', model: 'Protected', provider: 'Recovering',
    description: 'Primary capacity is reduced. Ferry is recovering traffic without client-visible errors.',
    outcomes:{direct:1415,recovered:53,failed:0}, modelOutcomes:{direct:650,recovered:29,failed:0},
    failureTrend:[0,0,0,0,0,0,0,0], fallbacks:7, modelTraffic:[61,39,0], needs:0,
    providerRecovered:31, providerFailed:0, usage:{tokens:19_200_000,estimatedCost:168.78},
  },
  exhausted: {
    id: 'exhausted', label: 'Provider Exhausted', system: 'Healthy', model: 'Protected', provider: 'Exhausted',
    description: 'The primary provider has no usable capacity. Traffic is fully served by fallback providers.',
    outcomes:{direct:1025,recovered:443,failed:0}, modelOutcomes:{direct:474,recovered:205,failed:0},
    failureTrend:[0,0,0,0,0,0,0,0], fallbacks:443, modelTraffic:[0,78,22], needs:0,
    providerRecovered:0, providerFailed:0, usage:{tokens:19_200_000,estimatedCost:168.78},
  },
  unavailable: {
    id: 'unavailable', label: 'Model Unavailable', system: 'Degraded', model: 'Unavailable', provider: 'Exhausted',
    description: 'All serving providers for gpt-5.6-sol are unavailable. Client failures are visible.',
    outcomes:{direct:1323,recovered:75,failed:70}, modelOutcomes:{direct:575,recovered:34,failed:70},
    failureTrend:[5,7,8,9,10,11,9,11], fallbacks:178, modelTraffic:[0,0,0], needs:1,
    providerRecovered:0, providerFailed:70, usage:{tokens:19_200_000,estimatedCost:168.78},
  }
};

export const initialData = {
  organizations: [
    { id: 'build-next', name: 'build.next', role: 'Owner', members: 8 },
    { id: 'team-alpha', name: 'Team Alpha', role: 'Admin', members: 12 },
    { id: 'acme-corp', name: 'Acme Corp', role: 'Member', members: 24 },
    { id: 'personal', name: 'Personal', role: 'Owner', members: 1 }
  ],
  overview: {
    requestRate: 97.87,
    tokenRate: '1.28M',
    activeConversations: 23,
    medianLatency: '1.42s',
    directSuccessPct: 96.39,
    recoveredPct: 3.61,
    failedPct: 0,
    recoveredCount: 53,
    totalRequests: '1.47K',
    totalTokens: '19.2M',
    estimatedCost: '$168.78',
    managedEvents: [
      { kind:'ok', title:'Codex account temporarily rate limited', detail:'31 requests recovered · no client errors' },
      { kind:'ok', title:'7 conversations migrated to OpenRouter', detail:'Rehydrated · +620ms median recovery latency' },
      { kind:'ok', title:'2 provider identities in cooldown', detail:'Auto recovering · no client impact' }
    ]
  },
  models: [
    { id:'gpt-5.6-sol', name:'gpt-5.6-sol', description:'Unified OpenAI-compatible model', enabled:true, status:'Protected', requestRate:45.27, firstToken:'218ms', completion:'1.42s', recovered:29 },
    { id:'claude-opus', name:'claude-opus', description:'Anthropic high-reasoning route', enabled:true, status:'Healthy', requestRate:23.8, firstToken:'205ms', completion:'1.06s', recovered:12 },
    { id:'smart', name:'smart', description:'Balanced automatic model route', enabled:true, status:'Healthy', requestRate:28.8, firstToken:'176ms', completion:'740ms', recovered:5 },
    { id:'gpt-4o', name:'gpt-4o', description:'General OpenAI route', enabled:true, status:'Healthy', requestRate:0, firstToken:'191ms', completion:'890ms', recovered:0 },
    { id:'gemini-1.5-pro', name:'gemini-1.5-pro', description:'Google large-context route', enabled:false, status:'Unavailable', requestRate:0, firstToken:'—', completion:'—', recovered:0 }
  ],
  routes: {
    'gpt-5.6-sol': [
      { id:'codex-personal', provider:'Codex Personal', model:'openai/gpt-5.6-sol', status:'Recovering', serving:61, mode:'First', recoverOn:['connect_failure','rate_limit','provider_5xx','quota_exhausted','ttft_timeout','stream_broken'] },
      { id:'openrouter-main', provider:'OpenRouter Main', model:'openai/gpt-5.6', status:'Healthy', serving:39, mode:'Next', recoverOn:['connect_failure','rate_limit','provider_5xx','quota_exhausted','ttft_timeout','stream_broken'] },
      { id:'openai-direct', provider:'OpenAI Direct', model:'gpt-5.6', status:'Standby', serving:0, mode:'Next', recoverOn:[] }
    ],
    'claude-opus': [
      { id:'anthropic-main', provider:'Anthropic Main', model:'claude-opus-4.1', status:'Healthy', serving:100, mode:'First', recoverOn:['connect_failure','rate_limit','provider_5xx','quota_exhausted','ttft_timeout','stream_broken'] },
      { id:'openrouter-main', provider:'OpenRouter Main', model:'anthropic/claude-opus', status:'Healthy', serving:0, mode:'Next', recoverOn:[] }
    ],
    'smart': [
      { id:'openrouter-main', provider:'OpenRouter Main', model:'smart', status:'Healthy', serving:100, mode:'First', recoverOn:[] }
    ]
  },
  overrides: [
    { model_id:'gpt-5.6-sol', id:'production-route', name:'Production prefers OpenRouter', enabled:true, callers:['production-key'], mode:'adjust_default', prefer:[{provider_id:'openrouter-main',upstream_model:'openai/gpt-5.6'}], avoid:[] },
    { model_id:'gpt-5.6-sol', id:'coding-route', name:'Coding key avoids direct OpenAI', enabled:true, callers:['coding-agent-key'], protocols:['openai-responses'], required_capabilities:{tools:true}, mode:'adjust_default', prefer:[], avoid:[{provider_id:'openai-direct',upstream_model:'gpt-5.6'}] },
    { model_id:'gpt-5.6-sol', id:'afternoon-route', name:'Afternoon custom route', enabled:true, weekdays:['mon','tue','wed','thu','fri'], timezone:'Asia/Shanghai', time_window:{start:'12:00',end:'18:00'}, mode:'custom_route', route:{steps:[{provider_id:'openrouter-main',upstream_model:'openai/gpt-5.6',recover_on:['connect_failure','rate_limit','provider_5xx']},{provider_id:'openai-direct',upstream_model:'gpt-5.6',recover_on:[]}]} }
  ],
  providers: [
    { id:'codex-personal', name:'Codex Personal', type:'OpenAI Compatible', baseUrl:'https://chatgpt.com/backend-api/codex', raw:{type:'cliproxy-codex'}, upstreamModels:['openai/gpt-5.6-sol','openai/gpt-4.1'], status:'Recovering', capacity:61, connections:4, available:3, requestRate:82, servingModels:['gpt-5.6-sol','gpt-4.1'], lastSuccessfulAt:'2026-08-28T14:23:17+08:00', logo:'codex' },
    { id:'openrouter-main', name:'OpenRouter Main', type:'OpenAI Compatible', baseUrl:'https://openrouter.ai/api/v1', upstreamModels:['openai/gpt-5.6','anthropic/claude-opus'], status:'Healthy', capacity:73, connections:2, available:2, requestRate:51, servingModels:['gpt-5.6-sol','gpt-4.1','claude-opus'], lastSuccessfulAt:'2026-08-28T14:23:16+08:00', logo:'router' },
    { id:'openai-direct', name:'OpenAI Direct', type:'OpenAI', baseUrl:'https://api.openai.com/v1', upstreamModels:['gpt-5.6','gpt-4.1'], status:'Healthy', capacity:88, connections:1, available:1, requestRate:18, servingModels:['gpt-5.6-sol','gpt-4.1'], lastSuccessfulAt:'2026-08-28T14:23:09+08:00', logo:'openai' },
    { id:'anthropic-main', name:'Anthropic Main', type:'Anthropic', baseUrl:'https://api.anthropic.com', upstreamModels:['claude-opus-4.1','claude-sonnet-4'], status:'Healthy', capacity:91, connections:2, available:2, requestRate:47, servingModels:['claude-opus','claude-sonnet'], lastSuccessfulAt:'2026-08-28T14:22:07+08:00', logo:'anthropic' },
    { id:'azure-openai', name:'Azure OpenAI', type:'Azure', baseUrl:'https://example.openai.azure.com', upstreamModels:['gpt-4.1'], status:'Healthy', capacity:66, connections:2, available:2, requestRate:12, servingModels:['gpt-4.1'], lastSuccessfulAt:null, logo:'azure' }
  ],
  connections: {
    'codex-personal': [
      { id:'account-a', name:'Account A', type:'OAuth', account:'codex-main-01', region:'US East', status:'Cooling', load:0, capacity:0, lastIssue:'HTTP 429 · Too Many Requests', lastUsed:'2 minutes ago', cooldownSeconds:42, conversations:3, requestsMoved:31 },
      { id:'account-b', name:'Account B', type:'OAuth', account:'codex-main-02', region:'US East', status:'Serving', load:42, capacity:71, lastIssue:'—', lastUsed:'now', cooldownSeconds:0, conversations:8, requestsMoved:0 },
      { id:'account-c', name:'Account C', type:'OAuth', account:'codex-main-03', region:'US West', status:'Serving', load:37, capacity:64, lastIssue:'—', lastUsed:'now', cooldownSeconds:0, conversations:7, requestsMoved:0 },
      { id:'account-d', name:'Account D', type:'OAuth', account:'codex-main-04', region:'US West', status:'Standby', load:21, capacity:92, lastIssue:'—', lastUsed:'8 minutes ago', cooldownSeconds:0, conversations:5, requestsMoved:0 }
    ],
    'openrouter-main': [
      { id:'router-a', name:'Router A', type:'API Key', account:'or-main-a', region:'Global', status:'Serving', load:39, capacity:78, lastIssue:'—', lastUsed:'now', cooldownSeconds:0, conversations:7, requestsMoved:0 },
      { id:'router-b', name:'Router B', type:'API Key', account:'or-main-b', region:'Global', status:'Standby', load:12, capacity:96, lastIssue:'—', lastUsed:'12 minutes ago', cooldownSeconds:0, conversations:0, requestsMoved:0 }
    ]
  },
  liveRecovery: [
    { time:'14:23:17', from:'Codex Personal', to:'Codex Personal', action:'Provider Retry', result:'Success', overhead:'+412ms' },
    { time:'14:23:11', from:'Codex Personal', to:'Codex Personal', action:'Provider Retry', result:'Success', overhead:'+386ms' },
    { time:'14:22:58', from:'Account A', to:'OpenRouter Main', action:'Provider Fallback', result:'Success', overhead:'+821ms' }
  ],
  requests: [
    { id:'req_01H8K721', time:'14:23:17', model:'gpt-5.6-sol', upstreamModel:'openai/gpt-5.6-sol', providerId:'Codex Personal', conversation:'conv_01H8K0F2', apiKey:'coding-agent-key', protocol:'openai-responses', route:'Codex Personal → Account B', result:'Success (Recovered)', attempts:2, stream:true, streamMode:'Streaming', ttft:'312ms', latency:'1.42s', totalDuration:'1.42s', tokens:'10.2K', recovered:true },
    { id:'req_01H8KS3Y', time:'14:23:16', model:'gpt-5.6-sol', upstreamModel:'openai/gpt-5.6-sol', providerId:'Codex Personal', conversation:'conv_01H8K0F2', apiKey:'coding-agent-key', protocol:'openai-responses', route:'Codex Personal', result:'Success (Direct)', attempts:1, stream:true, streamMode:'Streaming', ttft:'184ms', latency:'0.89s', totalDuration:'0.89s', tokens:'6.1K', recovered:false },
    { id:'req_01H8KS03', time:'14:23:14', model:'claude-opus', upstreamModel:'claude-opus-4.1', providerId:'Anthropic Main', conversation:'conv_01H8J5EA', apiKey:'production-key', route:'Anthropic Main', result:'Success (Direct)', attempts:1, stream:false, streamMode:'Non-streaming', ttft:'—', latency:'1.12s', totalDuration:'1.12s', tokens:'9.7K', recovered:false },
    { id:'req_01H8K300', time:'14:23:12', model:'gpt-5.6-sol', upstreamModel:'openai/gpt-5.6-sol', providerId:'Codex Personal', conversation:'conv_01H8K9H1', apiKey:'production-key', route:'Codex Personal', result:'Success (Direct)', attempts:1, stream:true, streamMode:'Streaming', ttft:'142ms', latency:'0.59s', totalDuration:'0.59s', tokens:'4.2K', recovered:false },
    { id:'req_01H8K0JV', time:'14:23:09', model:'gpt-5.6-sol', upstreamModel:'gpt-5.6', providerId:'OpenAI Direct', conversation:'conv_01H8K9H2', apiKey:'coding-agent-key', route:'Codex → OpenRouter → OpenAI', result:'Success (Recovered)', attempts:3, stream:true, streamMode:'Streaming', ttft:'1.18s', latency:'2.31s', totalDuration:'2.31s', tokens:'31.6K', recovered:true },
    { id:'req_01H8K22A', time:'14:22:06', model:'smart', upstreamModel:'smart', providerId:'OpenRouter Main', conversation:'conv_01H8K2B6', apiKey:'internal-test', route:'OpenRouter Main', result:'Success (Direct)', attempts:1, stream:false, streamMode:'Non-streaming', ttft:'—', latency:'0.56s', totalDuration:'0.56s', tokens:'2.1K', recovered:false },
    { id:'req_01H8K1TR', time:'14:22:07', model:'claude-opus', upstreamModel:'claude-opus-4.1', providerId:'Anthropic Main', conversation:'conv_01H8K1Y7', apiKey:'production-key', route:'Anthropic Main', result:'Success (Direct)', attempts:1, stream:true, streamMode:'Streaming', ttft:'205ms', latency:'1.06s', totalDuration:'1.06s', tokens:'8.8K', recovered:false }
  ],
  requestDetails: {
    'req_01H8K721': {
      id:'req_01H8K721', model:'gpt-5.6-sol', logicalModel:'gpt-5.6-sol', upstreamModel:'openai/gpt-5.6-sol', provider:'Codex Personal', providerId:'Codex Personal', conversation:'conv_01H8K0F2', apiKey:'coding-agent-key', time:'14:23:17', stream:true, streamMode:'Streaming', ttft:'312ms', latency:'1.42s', totalDuration:'1.42s', tokens:'10.2K', result:'Success (Recovered)', attempts:2, connectionRebinds:1, providerFallbacks:0, conversationRebind:'No', clientConnection:'Kept Open', recoveryOverhead:'+618ms',
      trace:[
        { time:'14:23:17', title:'Request Received', sub:'Client request accepted', latency:'0ms', kind:'normal' },
        { time:'14:23:17', title:'Candidate Excluded', sub:'OpenAI Responses · reasoning state cannot be represented losslessly', latency:'—', kind:'warning' },
        { time:'14:23:17', title:'Candidate Excluded', sub:'Backup route excluded by routing policy', latency:'—', kind:'excluded' },
        { time:'14:23:17', title:'Connection Selected', sub:'Codex Personal selected by default route', latency:'12ms', kind:'selected' },
        { time:'14:23:17', title:'Provider Attempt #1', sub:'Codex Personal · HTTP 429 Rate Limited', latency:'286ms', kind:'error' },
        { time:'14:23:18', title:'Provider Attempt #2', sub:'Codex Personal · Success', latency:'943ms', kind:'success' },
        { time:'14:23:18', title:'Response Completed', sub:'Response returned to client', latency:'1.42s', kind:'success' }
      ],
      routePath:[
        { provider:'Codex Personal', connection:'Account A', status:'Failed', failure:'HTTP 429 Rate Limited', model:'openai/gpt-5.6-sol' },
        { provider:'Codex Personal', connection:'Account B', status:'Success', failure:'', model:'openai/gpt-5.6-sol' }
      ]
    }
  },
  apiKeys: [
    { id:'production-key', name:'production-key', preview:'sk-…8f3a', status:'Active', scope:'All models', usage:42, created:'May 14, 2026', createdAt:'2026-05-14T00:00:00Z' },
    { id:'coding-agent-key', name:'coding-agent-key', preview:'sk-…7bd4', status:'Active', scope:'gpt-5.6-sol, claude-opus', usage:18, created:'May 11, 2026', createdAt:'2026-05-11T00:00:00Z' },
    { id:'internal-test', name:'internal-test', preview:'sk-…3d1e', status:'Active', scope:'All models', usage:6, created:'May 11, 2026', createdAt:'2026-05-11T00:00:00Z' },
    { id:'readonly-key', name:'readonly-key', preview:'sk-…7f64', status:'Active', scope:'Read Only (Analytics)', usage:1, created:'Apr 30, 2026', createdAt:'2026-04-30T00:00:00Z' },
    { id:'old-key', name:'old-key', preview:'sk-…48b0', status:'Revoked', scope:'gpt-4o', usage:0, created:'Apr 15, 2026', createdAt:'2026-04-15T00:00:00Z' }
  ],
  revisions: [
    { id:185, time:'Aug 26 · 14:29', actor:'Alice', scope:'gpt-5.6-sol', summary:'Routing priority changed · OpenRouter moved to primary', status:'Applied' },
    { id:184, time:'Aug 26 · 14:06', actor:'Alice', scope:'gpt-5.6-sol', summary:'Recovery policy updated · max attempts 3', status:'Superseded' },
    { id:183, time:'Aug 25 · 21:45', actor:'Codex', scope:'Codex Personal', summary:'Connection D added to standby capacity', status:'Applied' },
    { id:182, time:'Aug 25 · 18:22', actor:'Alice', scope:'Organization', summary:'Default recovery behavior updated', status:'Applied' }
  ],
  billing: {
    from:'2026-07-30T00:00:00.000Z', to:'2026-08-29T00:00:00.000Z',
    summary:{requests:3_710_000,succeeded:3_693_000,failed:14_000,canceled:3_000,input_tokens:200_000_000,output_tokens:48_530_000,tokens_observed:true,total_cost:2184.61,cost_observed:true},
    outcomes:{direct_success:3_500_000,recovered_success:193_000,failed:17_000},
    by_provider:{
      'OpenAI Direct':{requests:1_800_000,failed:3_000,canceled:0,input_tokens:105_000_000,output_tokens:25_000_000,tokens_observed:true,total_cost:1264.21,cost_observed:true},
      'Anthropic Main':{requests:800_000,failed:2_000,canceled:0,input_tokens:48_000_000,output_tokens:12_000_000,tokens_observed:true,total_cost:542.18,cost_observed:true},
      'OpenRouter Main':{requests:700_000,failed:1_000,canceled:0,input_tokens:40_000_000,output_tokens:10_000_000,tokens_observed:true,total_cost:363.47,cost_observed:true},
      'Azure OpenAI':{requests:200_000,failed:500,canceled:0,input_tokens:5_000_000,output_tokens:1_000_000,tokens_observed:true,total_cost:8.75,cost_observed:true},
      Groq:{requests:200_000,failed:500,canceled:0,input_tokens:2_000_000,output_tokens:530_000,tokens_observed:true,total_cost:6,cost_observed:true},
      '':{requests:10_000,failed:7_000,canceled:3_000,input_tokens:0,output_tokens:0,tokens_observed:false,cost_observed:false},
    },
    by_model:{
      'gpt-5.6-sol':{requests:1_500_000,failed:3_000,canceled:0,input_tokens:80_000_000,output_tokens:20_000_000,tokens_observed:true,total_cost:1124.11,cost_observed:true},
      'claude-opus':{requests:800_000,failed:2_000,canceled:0,input_tokens:48_000_000,output_tokens:12_000_000,tokens_observed:true,total_cost:532.27,cost_observed:true},
      'gpt-4o':{requests:700_000,failed:1_000,canceled:0,input_tokens:40_000_000,output_tokens:10_000_000,tokens_observed:true,total_cost:364.22,cost_observed:true},
      'gpt-3.5-turbo':{requests:400_000,failed:500,canceled:0,input_tokens:20_000_000,output_tokens:5_000_000,tokens_observed:true,total_cost:100,cost_observed:true},
      smart:{requests:300_000,failed:500,canceled:0,input_tokens:12_000_000,output_tokens:1_530_000,tokens_observed:true,total_cost:64.01,cost_observed:true},
      '':{requests:10_000,failed:7_000,canceled:3_000,input_tokens:0,output_tokens:0,tokens_observed:false,cost_observed:false},
    },
    trend:[
      {from:'2026-08-22T00:00:00.000Z',requests:830_000,total_cost:470.18},
      {from:'2026-08-23T00:00:00.000Z',requests:905_000,total_cost:512.42},
      {from:'2026-08-24T00:00:00.000Z',requests:962_000,total_cost:563.36},
      {from:'2026-08-25T00:00:00.000Z',requests:1_013_000,total_cost:638.65},
    ],
  },
};

const demoBucketStarts=Array.from({length:8},(_,index)=>new Date(Date.UTC(2026,7,28,14,index*2)).toISOString());
const demoTrafficSeries=(id,label,values,failures=[])=>{
  const points=values.map((requests,index)=>{
    const failed=Math.max(0,Math.min(requests,Number(failures[index]||0))),succeeded=requests-failed;
    const bucketDuration=index===values.length-1?60000:120000;
    return {from:demoBucketStarts[index],to:new Date(Date.parse(demoBucketStarts[index])+bucketDuration).toISOString(),requests,succeeded,failed};
  });
  const requests=points.reduce((sum,point)=>sum+point.requests,0),succeeded=points.reduce((sum,point)=>sum+point.succeeded,0);
  return {id,label,requests,succeeded,failed:requests-succeeded,successRate:requests?succeeded/requests*100:null,points};
};
const demoTrafficForFailures=failures=>{
  const all=[168,176,181,173,189,194,186,201],gpt=[76,81,79,84,88,91,86,94],smart=[48,51,55,46,57,58,56,61],claude=[44,44,47,43,44,45,44,46];
  const codex=[66,70,68,71,73,75,70,78],router=[60,64,68,63,72,75,72,78],anthropic=[42,42,45,39,44,44,44,45],codexPrimary=[62,66,64,67,69,71,66,73];
  const codexOther=codex.map((value,index)=>value-codexPrimary[index]),routerGpt=router.map((value,index)=>value-smart[index]);
  return {trend:demoTrafficSeries('all-requests','All requests',all,failures).points,trafficTrends:{
    logicalModels:[demoTrafficSeries('gpt-5.6-sol','gpt-5.6-sol',gpt,failures),demoTrafficSeries('smart','smart',smart),demoTrafficSeries('claude-opus','claude-opus',claude)],
    providers:[demoTrafficSeries('codex-personal','Codex Personal',codex,failures),demoTrafficSeries('openrouter-main','OpenRouter Main',router),demoTrafficSeries('anthropic-main','Anthropic Main',anthropic)],
    providerModels:[demoTrafficSeries('codex-personal\u0000openai/gpt-5.6-sol','Codex Personal / openai/gpt-5.6-sol',codexPrimary,failures),demoTrafficSeries('codex-personal\u0000openai/gpt-4.1','Codex Personal / openai/gpt-4.1',codexOther),demoTrafficSeries('openrouter-main\u0000smart','OpenRouter Main / smart',smart),demoTrafficSeries('anthropic-main\u0000claude-opus-4.1','Anthropic Main / claude-opus-4.1',anthropic),demoTrafficSeries('openrouter-main\u0000openai/gpt-5.6','OpenRouter Main / openai/gpt-5.6',routerGpt)],
  }};
};
for(const scenario of Object.values(scenarios))Object.assign(scenario,demoTrafficForFailures(scenario.failureTrend));
Object.assign(initialData.overview,demoTrafficForFailures(scenarios.protected.failureTrend));
const demoCacheHit=(id,eligibleInputTokens,cacheReadTokens,cacheWriteTokens,requests,observedRequests=requests)=>({
  id,requests,observedRequests,eligibleInputTokens,cacheReadTokens,cacheWriteTokens,
  hitRate:eligibleInputTokens?cacheReadTokens/eligibleInputTokens*100:null,
});
initialData.overview.cacheHit={
  overall:demoCacheHit('overall',18_000_000,15_610_000,243_000,1_468),
  apiKeys:[
    demoCacheHit('coding-agent-key',9_000_000,8_400_000,80_000,600),
    demoCacheHit('production-key',7_000_000,5_700_000,120_000,695),
    demoCacheHit('internal-test',1_500_000,1_200_000,40_000,146),
    demoCacheHit('old-key',500_000,310_000,3_000,27),
  ],
  logicalModels:[
    demoCacheHit('gpt-5.6-sol',8_000_000,7_200_000,90_000,679),
    demoCacheHit('claude-opus',4_500_000,3_800_000,80_000,357),
    demoCacheHit('smart',5_500_000,4_610_000,73_000,432),
    demoCacheHit('gpt-4o',0,0,0,0,0),
  ],
};
const demoRequestUsage=[
  [9360,840,8128,0,390],[5480,620,4864,0,214],[8910,790,8064,0,302],[3710,490,3072,0,166],[28600,3000,24576,0,1420],[1810,290,1536,0,104],[7980,820,7168,0,281],
];
initialData.requests.forEach((request,index)=>{
  const [inputTokens,outputTokens,cacheReadTokens,cacheWriteTokens,reasoningTokens]=demoRequestUsage[index]||[0,0,0,0,0];
  const totalCost=[0.0069,0.0042,0.0645,0.0031,0.0886,0,0.0595][index];
  request.raw={total_cost:totalCost,cost_observed:totalCost>0,estimated_cash_cost:totalCost,serving_cost:totalCost,allocated_sub_cost:0,recovery_overhead_cost:0,shadow_cost:totalCost,currency:'USD',ttft_ms:[312,184,0,142,1180,0,205][index],duration_ms:[1420,890,1120,590,2310,560,1060][index]};
  Object.assign(request,{usageObserved:true,inputTokens,outputTokens,cacheReadTokens,cacheWriteTokens,reasoningTokens,tokenSummary:`${inputTokens.toLocaleString()} in · ${outputTokens.toLocaleString()} out`,cacheSummary:`${cacheReadTokens.toLocaleString()} read · ${cacheWriteTokens.toLocaleString()} write`});
});
Object.assign(initialData.requestDetails.req_01H8K721,initialData.requests[0]);
initialData.requestDetails.req_01H8K721.routePath=initialData.requestDetails.req_01H8K721.routePath.map((step,index)=>({...step,protocol:'openai-responses',ttft:index?'312ms':'—',latency:index?'943ms':'286ms',tokenSummary:index?'9,360 in · 840 out':'—',cacheSummary:index?'8,128 read · 0 write':'—'}));

export function cloneInitialData() {
  return JSON.parse(JSON.stringify(initialData));
}
