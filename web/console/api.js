import { cloneInitialData } from './mock-data.js?visual=v3-provider-relogin';

const sleep = (ms = 90) => new Promise(resolve => setTimeout(resolve, ms));
const clone = value => JSON.parse(JSON.stringify(value));
const defaultFetch = (...args) => globalThis.fetch(...args);

const readCodexAuthJSON = async source => {
  if (!source) throw new Error('Choose an auth.json file first.');
  const text = typeof source === 'string' ? source : await source.text();
  if (!text.trim()) throw new Error('auth.json is empty.');
  if (new TextEncoder().encode(text).length > 128 * 1024) throw new Error('auth.json must be 128 KiB or smaller.');
  try { return JSON.parse(text); }
  catch { throw new Error('auth.json is not valid JSON.'); }
};

const providerModelCapabilityDefaults = protocol => ({
  text:protocol !== 'openai-images', images:protocol === 'openai-images', stream:false, tools:false,
  parallel_tools:false, reasoning:false, structured_output:false,
  vision:false,priority:false, context_window:0, max_output:0,
});

const normalizedProviderModelCapabilities = (protocol, model, declared = {}) => ({
  ...providerModelCapabilityDefaults(protocol), ...declared,
});

const codexProviderModelProtocol = model => String(model||'').trim().toLowerCase() === 'gpt-image-2'
  ? 'openai-images' : 'openai-responses';

const providerConnectionModel = (protocolItem, upstreamModel) => {
  const spec = protocolItem.modelSpecs?.[upstreamModel] || {};
  return {
    upstream_model:upstreamModel,
    protocols:[protocolItem.protocol],
    capabilities:normalizedProviderModelCapabilities(protocolItem.protocol,upstreamModel,spec.capabilities),
    ...(spec.plan_id ? {plan_id:spec.plan_id} : {}),
  };
};

const mockRouteStepKey = step => `${step.provider_id || step.id}\u0000${step.upstream_model || step.model}`;
const mockOverrideMatches = (override, input = {}) => {
  if (!override.enabled) return false;
  if ((override.callers || []).length && !(override.callers || []).includes(input.caller_id || '')) return false;
  const protocol=input.protocol || 'openai-chat';
  if ((override.protocols || []).length && !override.protocols.includes(protocol)) return false;
  const at=new Date(input.at || Date.now()),timezone=override.timezone || 'UTC';let parts;
  try { parts=Object.fromEntries(new Intl.DateTimeFormat('en-US',{timeZone:timezone,weekday:'short',hour:'2-digit',minute:'2-digit',hourCycle:'h23'}).formatToParts(at).filter(part=>part.type!=='literal').map(part=>[part.type,part.value])); } catch { return false; }
  if ((override.weekdays || []).length && !override.weekdays.includes(String(parts.weekday || '').slice(0,3).toLowerCase())) return false;
  if (override.time_window) { const current=Number(parts.hour)*60+Number(parts.minute),minutes=value=>Number(value.slice(0,2))*60+Number(value.slice(3,5)),start=minutes(override.time_window.start),end=minutes(override.time_window.end),inside=start<=end?current>=start&&current<end:current>=start||current<end;if(!inside)return false; }
  const want=override.required_capabilities || {},required={...(input.required_capabilities||{}),text:protocol!=='openai-images'||Boolean(input.required_capabilities?.text),stream:Boolean(input.stream)||Boolean(input.required_capabilities?.stream)};
  for (const key of ['text','images','stream','tools','parallel_tools','reasoning','structured_output','vision','priority']) if(want[key]&&!required[key])return false;
  return Number(required.min_context||0)>=Number(want.min_context||0)&&Number(required.min_output||0)>=Number(want.min_output||0);
};
const mockEffectiveRoute = (routes, override) => {
  if (!override) return clone(routes);
  if (override.mode === 'custom_route') return (override.route?.steps || []).map(step=>({id:step.provider_id,provider:step.provider_id,model:step.upstream_model,status:'Healthy',serving:0,recoverOn:clone(step.recover_on || [])}));
  const preferred=new Set((override.prefer||[]).map(mockRouteStepKey)),avoided=new Set((override.avoid||[]).map(mockRouteStepKey));
  return [...routes.filter(route=>preferred.has(mockRouteStepKey(route))&&!avoided.has(mockRouteStepKey(route))),...routes.filter(route=>!preferred.has(mockRouteStepKey(route))&&!avoided.has(mockRouteStepKey(route)))].map(route=>clone(route));
};

// Native OAuth responses are public views, never credential/token objects.
const nativeAccountPattern = /^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,95}$/;
export function parseNativeCredential(ref) {
  const match = /^native-oauth:([^:]+):([^:]+)$/.exec(String(ref || ''));
  return match && nativeAccountPattern.test(match[1]) && nativeAccountPattern.test(match[2]) ? {driver:match[1], account:match[2]} : null;
}
function nativeAuthError() { return new FerryError('Native sign-in is unavailable. Check the account status and try again.', 503, {code:'NATIVE_AUTH_UNAVAILABLE'}); }
function nativeAccountPath(driver, account) {
  if (!nativeAccountPattern.test(driver) || !nativeAccountPattern.test(account)) throw nativeAuthError();
  return `/api/oauth/native/${encodeURIComponent(driver)}/accounts/${encodeURIComponent(account)}`;
}
function nativeSignInURL(value) {
  try {
    const u = new URL(value);
    if (u.protocol !== 'https:' || u.username || u.password || u.hash || String(value).length > 16384) return '';
    if ([...u.searchParams.keys()].some(key => /token|password|secret|verifier/i.test(key))) return '';
    return u.href;
  } catch { return ''; }
}
function nativePublicView(body, driver, account) {
  const ref = `native-oauth:${driver}:${account}`;
  if (!body || body.driver !== driver || body.account !== account || body.credential_ref !== ref || !['connected','pending','key_pending','relogin_required','expired','disconnected'].includes(body.status)) throw nativeAuthError();
  const binding = {};
  if (body.protocol && !['openai-chat','openai-responses','anthropic','openai-images'].includes(body.protocol)) throw nativeAuthError();
  if (body.base_url) {
    const u = new URL(body.base_url);
    if (u.protocol!=='https:' || u.username || u.password || u.search || u.hash || !['openai-chat','openai-responses','anthropic','openai-images'].includes(body.protocol)) throw nativeAuthError();
    binding.base_url=body.base_url;binding.protocol=body.protocol;
  }
  return {driver, account, credential_ref:ref, status:body.status, ...binding,
    url:body.status==='pending' ? nativeSignInURL(body.url) : '',
    user_code:body.status==='pending' && typeof body.user_code==='string' ? body.user_code.slice(0,256) : '',
    expires_at:typeof body.expires_at==='string' ? body.expires_at : '',
    interval_seconds:Number.isFinite(body.interval_seconds) ? Math.max(0,body.interval_seconds) : 0};
}

export class FerryAdapter {
  async getNativeOAuthDrivers() { return []; }
  async beginNativeOAuth() { throw nativeAuthError(); }
  async completeNativeOAuth() { throw nativeAuthError(); }
  async getNativeOAuthStatus() { throw nativeAuthError(); }
  async disconnectNativeOAuth() { throw nativeAuthError(); }
  async getAuthStatus() { return { initialized:true, authenticated:true, username:'demo-admin' }; }
  async initializeAdmin(_input) { throw new Error('Not implemented'); }
  async login(_input) { throw new Error('Not implemented'); }
  async logout() { throw new Error('Not implemented'); }
  async changePassword(_input) { throw new Error('Not implemented'); }
  async getSecurityEvents() { return []; }
  async getManagementTokens() { return []; }
  async createManagementToken(_input) { throw new Error('Not implemented'); }
  async updateManagementToken(_id, _patch) { throw new Error('Not implemented'); }
  async rotateManagementToken(_id) { throw new Error('Not implemented'); }
  async revokeManagementToken(_id) { throw new Error('Not implemented'); }
  async deleteManagementToken(_id) { throw new Error('Not implemented'); }
  async getAnalytics(_filters) { return null; }
  async getOverview(_window) { throw new Error('Not implemented'); }
  async getModels() { throw new Error('Not implemented'); }
  async getModel(_id) { throw new Error('Not implemented'); }
  async createModel(_input) { throw new Error('Not implemented'); }
  async updateModel(_id, _patch) { throw new Error('Not implemented'); }
  async deleteModel(_id) { throw new Error('Not implemented'); }
  async createOverride(_modelId, _input) { throw new Error('Not implemented'); }
  async updateOverride(_modelId, _id, _input) { throw new Error('Not implemented'); }
  async deleteOverride(_modelId, _id) { throw new Error('Not implemented'); }
  async simulateModel(_modelId, _input) { throw new Error('Not implemented'); }
  async reviewRouting(_modelId, _draft) { throw new Error('Not implemented'); }
  async applyRouting(_modelId, _review) { throw new Error('Not implemented'); }
  async getProviders() { throw new Error('Not implemented'); }
  async getProvider(_id) { throw new Error('Not implemented'); }
  async discoverProviderModels(_provider) { throw new Error('Not implemented'); }
  async discoverProviderProtocols(_provider) { throw new Error('Not implemented'); }
  async syncProviderModels(_id) { throw new Error('Not implemented'); }
  async addProvider(_provider) { throw new Error('Not implemented'); }
  async updateProvider(_id, _patch) { throw new Error('Not implemented'); }
  async saveProviderProtocolConfig(_id, _config) { throw new Error('Not implemented'); }
  async deleteProvider(_id) { throw new Error('Not implemented'); }
  async addConnection(_providerId, _connection) { throw new Error('Not implemented'); }
  async updateConnection(_providerId, _connectionId, _patch) { throw new Error('Not implemented'); }
  async deleteConnection(_providerId, _connectionId) { throw new Error('Not implemented'); }
  async testConnection(_providerId, _connectionId) { throw new Error('Not implemented'); }
  async resetCooldowns(_scope = {}) { throw new Error('Not implemented'); }
  async rotateConnectionSecret(_providerId, _connectionId, _secret) { throw new Error('Not implemented'); }
  async getRequests(_filters) { throw new Error('Not implemented'); }
  async getRequest(_id) { throw new Error('Not implemented'); }
  async getQueue() { throw new Error('Queue is unavailable'); }
  async getSettings() { return { debug_request_response:false, scheduled_model_sync_enabled:true, codex_usage_reset_auto:{enabled:true,threshold_minutes:20}, codex_usage_protection:{max_used_percent:98,allow_credits:false}, auto_logical_model_limit:10, backup:{enabled:true,interval_hours:24,include_history:false,s3_prefix:"ferry/scheduled",local_copies:2,remote_copies:30} }; }
  async getPricingStatus() { return {}; }
  async refreshPricing() { throw new Error('Not implemented'); }
  async updateSettings(_patch) { throw new Error('Not implemented'); }
  async publishCodexClientVersion(_version, _revision, _options) { throw new Error('Not implemented'); }
  async getApiKeys() { throw new Error('Not implemented'); }
  async getApiKey(_id) { throw new Error('Not implemented'); }
  async createApiKey(_input) { throw new Error('Not implemented'); }
  async revealApiKey(_id) { throw new Error('Not implemented'); }
  async revokeApiKey(_id) { throw new Error('Not implemented'); }
  async updateApiKey(_id, _patch) { throw new Error('Not implemented'); }
  async rotateApiKey(_id) { throw new Error('Not implemented'); }
  async retireApiKeySecret(_id, _secretId) { throw new Error('Not implemented'); }
  async deleteApiKey(_id) { throw new Error('Not implemented'); }
  async startConnectionOAuth(_providerId, _connectionId, _options) { throw new Error('Not implemented'); }
  async completeConnectionOAuth(_providerId, _connectionId, _callbackUrl, _options) { throw new Error('Not implemented'); }
  async cancelConnectionOAuth(_providerId, _connectionId) { throw new Error('Not implemented'); }
  async getConnectionOAuthStatus(_providerId, _connectionId) { throw new Error('Not implemented'); }
  async importConnectionOAuth(_providerId, _connectionId, _authFile) { throw new Error('Not implemented'); }
  async getConnectionOAuthUsage(_providerId, _connectionId, _options) { throw new Error('Not implemented'); }
  async getUsageResetRequests() { return []; }
  async prepareUsageReset(_providerId, _connectionId, _creditId, _accountId) { throw new Error('Not implemented'); }
  async confirmUsageReset(_request) { throw new Error('Not implemented'); }
  async cancelUsageReset(_request) { throw new Error('Not implemented'); }
  async getOrganizations() { throw new Error('Not implemented'); }
  async getRevisions() { throw new Error('Not implemented'); }
  async getRevision(_id) { throw new Error('Not implemented'); }
  async restoreRevision(_id) { throw new Error('Not implemented'); }
  async getAlerts() { return { alerts:[], unread:0 }; }
  async acknowledgeAlert(_id) { throw new Error('Not implemented'); }
  async getBilling() { throw new Error('Not implemented'); }
}

// Keep the demo's manual-to-automatic handover consistent with the server.
function compareCodexClientVersions(a,b){
  const split=value=>{const at=value.indexOf('-');return at<0?[value,null]:[value.slice(0,at),value.slice(at+1)];};
  const [ac,ap]=split(a),[bc,bp]=split(b),aa=ac.split('.'),bb=bc.split('.');
  const numberCompare=(x,y)=>BigInt(x)<BigInt(y)?-1:BigInt(x)>BigInt(y)?1:0;
  for(let i=0;i<3;i++){const order=numberCompare(aa[i],bb[i]);if(order)return order;}
  if(ap===null||bp===null)return ap===bp?0:ap===null?1:-1;
  const ax=ap.split('.'),bx=bp.split('.');
  for(let i=0;i<Math.min(ax.length,bx.length);i++){
    const an=/^[0-9]+$/.test(ax[i]),bn=/^[0-9]+$/.test(bx[i]);
    const order=an&&bn?numberCompare(ax[i],bx[i]):an!==bn?(an?-1:1):ax[i]<bx[i]?-1:ax[i]>bx[i]?1:0;
    if(order)return order;
  }
  return Math.sign(ax.length-bx.length);
}

export class MockFerryAdapter extends FerryAdapter {
  async resetCooldowns(_scope = {}) { return { cleared_models: 1, persisted: true }; }
  constructor() {
    super();
    this.mode = 'mock';
    this.data = cloneInitialData();
    this.revision = 185;
    this.settings = { debug_request_response:false, debug_capture_limit_bytes:262144, debug_retention:'same as request history', scheduled_model_sync_enabled:true, codex_usage_reset_auto:{enabled:true,threshold_minutes:20}, codex_usage_protection:{max_used_percent:98,allow_credits:false}, auto_logical_model_limit:10, backup:{enabled:true,interval_hours:24,include_history:false,s3_prefix:"ferry/scheduled",local_copies:2,remote_copies:30} };
    this.apiKeySecrets = new Map(this.data.apiKeys.filter(item => item.status !== 'Revoked').map(item => [item.id, `sk-ferry-demo-${item.id}`]));
    this.managementTokens = [];
    this.oauthDevicePolls = new Map();
  }

  async getOverview() { await sleep(); return clone(this.data.overview); }
  async getAuthStatus() { return { initialized:true, authenticated:true, username:'Alice' }; }
  async getSecurityEvents() { return []; }
  async getManagementTokens() { await sleep(); return clone(this.managementTokens); }
  async createManagementToken(input) {
    await sleep();
    if(this.managementTokens.some(token=>token.name===input.name))throw new Error(`Management token ${input.name} already exists`);
    const secret=`smt_demo_${Math.random().toString(36).slice(2)}${Date.now().toString(36)}`;
    const now=new Date().toISOString(),token={id:`mtok_${Math.random().toString(36).slice(2)}`,name:input.name,description:input.description||'',preview:`${secret.slice(0,10)}…${secret.slice(-4)}`,enabled:true,allow_high_risk:Boolean(input.allow_high_risk),created_at:now,updated_at:now,...(input.expires_at?{expires_at:input.expires_at}:{})};
    this.managementTokens.push(token);return {token:clone(token),secret};
  }
  async updateManagementToken(id,patch) { await sleep();const token=this.managementTokens.find(item=>item.id===id);if(!token)throw new Error(`Unknown management token ${id}`);const next=clone(patch);if(next.clear_expiry){delete token.expires_at;delete next.clear_expiry}Object.assign(token,next,{updated_at:new Date().toISOString()});return clone(token); }
  async rotateManagementToken(id) { await sleep();const token=this.managementTokens.find(item=>item.id===id);if(!token||token.revoked_at)throw new Error(`Unknown or revoked management token ${id}`);const secret=`smt_demo_${Math.random().toString(36).slice(2)}${Date.now().toString(36)}`;token.preview=`${secret.slice(0,10)}…${secret.slice(-4)}`;token.updated_at=new Date().toISOString();return {token:clone(token),secret}; }
  async revokeManagementToken(id) { await sleep();const token=this.managementTokens.find(item=>item.id===id);if(!token)throw new Error(`Unknown management token ${id}`);token.enabled=false;token.revoked_at=new Date().toISOString();token.updated_at=token.revoked_at;return clone(token); }
  async deleteManagementToken(id) { await sleep();const index=this.managementTokens.findIndex(item=>item.id===id&&item.revoked_at);if(index<0)throw new Error('Revoke the management token before deleting it');this.managementTokens.splice(index,1); }
  async getModels() {
    await sleep();
    return clone(this.data.models.map(model => ({
      ...model, enabled:model.enabled !== false, routes:clone(this.data.routes[model.id] || []),
      raw:{ id:model.id, display_name:model.name, description:model.description || '', ...(model.raw || {}), enabled:model.enabled !== false },
    })));
  }
  async getModel(id) {
    await sleep();
    const model = this.data.models.find(x => x.id === id);
    if (!model) throw new Error(`Unknown model ${id}`);
    const enabled = model.enabled !== false;
    const rawOverrides=(this.data.overrides||[]).filter(item=>item.model_id===id);
    return { model: clone({ ...model, enabled, conversationStrategy:model.raw?.existing_conversation_strategy||'keep-existing', raw:{ id:model.id, display_name:model.name, description:model.description || '', ...(model.raw || {}), enabled } }), routes: clone(this.data.routes[id] || []), overrides:rawOverrides.map(raw=>({id:raw.id,type:'Request match',matcher:(raw.callers||[]).join(', ')||raw.name,route:raw.mode==='custom_route'?'Custom route':'Adjust default',status:raw.enabled?'Active':'Disabled',raw:clone(raw)})) };
  }
  async createModel(input) {
    await sleep();
    if (this.data.models.some(model => model.id === input.id)) throw new Error(`Model ${input.id} already exists`);
    const routes = (input.default_route?.steps || []).map((step, index) => ({
      id:step.provider_id,
      provider:this.data.providers.find(provider => provider.id === step.provider_id)?.name || step.provider_id,
      model:step.upstream_model,
      status:'Healthy', serving:0, mode:index === 0 ? 'First' : 'Next',
      recoverOn:clone(step.recover_on || []),
    }));
    const model = {
      id:input.id, name:input.display_name || input.id, description:input.description || '',
      enabled:Boolean(input.enabled), status:input.enabled && routes.length ? 'Healthy' : 'Unavailable',
      routes:clone(routes), requestRate:0, firstToken:'—', completion:'—', recovered:0, failed:0,
      raw:clone(input),
    };
    this.data.models.push(model);
    this.data.routes[input.id] = routes;
    return clone(model);
  }
  async updateModel(id, patch) {
    await sleep();
    const model = this.data.models.find(item => item.id === id);
    if (!model) throw new Error(`Unknown model ${id}`);
    model.raw = { ...(model.raw || { id }), ...clone(patch) };
    if (patch.display_name !== undefined) model.name = patch.display_name;
    if (patch.description !== undefined) model.description = patch.description;
    if (patch.overrides !== undefined) this.data.overrides=[...(this.data.overrides||[]).filter(item=>item.model_id!==id),...clone(patch.overrides).map(item=>({...item,model_id:id}))];
    if (patch.enabled !== undefined) {
      model.enabled = Boolean(patch.enabled);
      model.status = model.enabled && (this.data.routes[id] || []).length ? 'Healthy' : 'Unavailable';
    }
    return clone(model);
  }
  async deleteModel(id) {
    await sleep();
    const index = this.data.models.findIndex(model => model.id === id);
    if (index < 0) throw new Error(`Unknown model ${id}`);
    this.data.models.splice(index, 1);
    delete this.data.routes[id];
    this.data.overrides=(this.data.overrides||[]).filter(item=>item.model_id!==id);
  }
  async createOverride(modelId,input) {
    await sleep();if((this.data.overrides||[]).some(item=>item.model_id===modelId&&item.id===input.id))throw new Error(`Override ${input.id} already exists`);this.data.overrides.push({...clone(input),model_id:modelId});return clone(input);
  }
  async updateOverride(modelId,id,input) {
    await sleep();const index=(this.data.overrides||[]).findIndex(item=>item.model_id===modelId&&item.id===id);if(index<0)throw new Error(`Unknown override ${id}`);this.data.overrides[index]={...clone(input),id,model_id:modelId};return clone(this.data.overrides[index]);
  }
  async deleteOverride(modelId,id) {
    await sleep();const index=(this.data.overrides||[]).findIndex(item=>item.model_id===modelId&&item.id===id);if(index<0)throw new Error(`Unknown override ${id}`);this.data.overrides.splice(index,1);
  }
  async simulateModel(modelId,input={}) {
    await sleep();
    const model = this.data.models.find(item => item.id === modelId);
    if (!model) throw new Error(`Unknown model ${modelId}`);
    const defaultRoutes = this.data.routes[modelId] || [],override=(this.data.overrides||[]).filter(item=>item.model_id===modelId).find(item=>mockOverrideMatches(item,input)),routes=mockEffectiveRoute(defaultRoutes,override);
    if (!routes.length) throw new Error('Route cannot be simulated until at least one Provider is configured.');
    const candidates = routes.map((route, index) => ({
      provider_id:route.id, connection_id:`${route.id}-primary`, upstream_model:route.model,
      eligible:route.status !== 'Unavailable', rank:index + 1,
      exclusion_reason:route.status === 'Unavailable' ? 'Provider route is unavailable' : '',
    }));
    const expected = candidates.find(candidate => candidate.eligible);
    return {
      logical_model_id:modelId, matched_override:override?.id||'', default_route:{steps:defaultRoutes.map(route=>({provider_id:route.id,upstream_model:route.model,recover_on:clone(route.recoverOn||[])}))}, effective_route:{steps:routes.map(route=>({provider_id:route.id,upstream_model:route.model,recover_on:clone(route.recoverOn||[])}))}, connection_candidates:candidates, simulated_at:new Date(input.at||Date.now()).toISOString(),
      expected_selection:expected ? { provider_id:expected.provider_id, connection_id:expected.connection_id, upstream_model:expected.upstream_model } : {},
      previewing_disabled_model:model.enabled === false,
      ...(expected ? {} : { planning_failure:{ message:'No configured Provider is currently eligible.' } }),
    };
  }
  async reviewRouting(modelId, draft) {
    await sleep(120);
    const before = clone(this.data.routes[modelId] || []);
    return {
      modelId,
      revision: this.revision + 1,
      before,
      after: clone(draft.routes),
      expectedTraffic: this.data.models.find(x => x.id === modelId)?.requestRate || 0,
      activeConversations: this.data.overview.activeConversations,
      conversationStrategy: draft.conversationStrategy || 'keep-existing'
    };
  }
  async applyRouting(modelId, review) {
    await sleep(150);
    this.revision += 1;
    this.data.routes[modelId] = clone(review.after).map((r, i) => ({ ...r, mode: i === 0 ? 'First' : 'Next' }));
    const model=this.data.models.find(item=>item.id===modelId);if(model)model.raw={...(model.raw||{}),existing_conversation_strategy:review.conversationStrategy||'keep-existing'};
    this.data.revisions.unshift({
      id: this.revision,
      time: 'Aug 26 · 14:29', actor:'Alice', scope:modelId,
      summary:`Routing priority changed · ${review.after[0]?.provider || 'Primary route'} moved to primary`, status:'Applied'
    });
    return {
      revision: this.revision,
      message: `${modelId} routing revision applied`,
      existingConversations: review.conversationStrategy === 'keep-existing' ? this.data.overview.activeConversations : 0
    };
  }
  async getProviders() {
    await sleep();
    return clone(this.data.providers.map(provider => ({
      ...provider,
      enabled:provider.enabled !== false,
      raw:{...(provider.raw || {}), base_url:provider.raw?.base_url || provider.baseUrl || '',model_sync_interval:provider.raw?.model_sync_interval||'1h',model_sync:provider.raw?.model_sync||{status:provider.enabled===false?'disabled':'idle',last_started_at:null,last_completed_at:null,last_success_at:null,next_run_at:null,last_error_code:'',last_error:'',discovered_models:0,added_models:0,created_logical_models:0,revision:this.revision}},
      connectionItems:this.data.connections[provider.id] || [],
    })));
  }
  async getProvider(id) {
    await sleep();
    const provider = this.data.providers.find(x => x.id === id);
    if (!provider) throw new Error(`Unknown provider ${id}`);
    return {
      provider:clone({ ...provider, enabled:provider.enabled !== false, raw:{...(provider.raw || {}), base_url:provider.raw?.base_url || provider.baseUrl || '',model_sync_interval:provider.raw?.model_sync_interval||'1h',model_sync:provider.raw?.model_sync||{status:provider.enabled===false?'disabled':'idle',last_started_at:null,last_completed_at:null,last_success_at:null,next_run_at:null,last_error_code:'',last_error:'',discovered_models:0,added_models:0,created_logical_models:0,revision:this.revision}} }),
      connections:clone(this.data.connections[id] || []),
      liveRecovery:clone(this.data.liveRecovery),
      recoveryPolicy:{ providerRetry:'Enabled', providerFallback:'Configured per model route', preserveClientStream:'Enabled', conversationHandling:'Revision strategy', maxRecoveryTime:'—' },
    };
  }
  async discoverProviderModels(provider) {
    await sleep(120);
    if (provider.type === 'cliproxy-codex') {
      return ['gpt-5.4','gpt-5.4-mini','gpt-5.5','gpt-5.6-sol','gpt-5.6-terra','gpt-5.6-luna','codex-auto-review','gpt-5.3-codex-spark','gpt-image-2'];
    }
    const baseUrl = String(provider.baseUrl || '').replace(/\/$/, '');
    const existing = this.data.providers.find(item => String(item.baseUrl || item.raw?.base_url || '').replace(/\/$/, '') === baseUrl);
    if (existing) return clone([...(existing.upstreamModels || [])]);
    if (provider.protocol === 'anthropic') return ['claude-opus-4.1','claude-sonnet-4','claude-haiku-3.5'];
    return ['deepseek-v4-flash','deepseek-v4-pro','glm-5.3','gpt-5.6-sol'];
  }
  async discoverProviderProtocols(provider) {
    const requested=(provider.protocols||[]).map(item=>item.protocol).filter(Boolean);
    const protocols=requested.length?requested:['openai-chat','openai-responses','anthropic','openai-images'];
    const delayByProtocol={'openai-chat':180,'openai-responses':360,anthropic:620,'openai-images':840};
    await sleep(protocols.length===1?(delayByProtocol[protocols[0]]||240):180);
    const models = await this.discoverProviderModels({...provider, protocol:'openai-chat'});
    if (!models.some(model=>/image/i.test(model))) models.push('gpt-image-2');
    return {
      models,
      source:'real-probe',
      protocols:protocols.map(protocol=>{const input=(provider.protocols||[]).find(item=>item.protocol===protocol)||{},configured=[...new Set(input.models||[])],verified=protocol==='openai-images'?configured.filter(model=>/image/i.test(model)):configured;return {
        protocol,base_url:input.baseUrl||provider.baseUrl,full_url:input.fullUrl||'',
        endpoint:input.fullUrl||`${String(input.baseUrl||provider.baseUrl||'').replace(/\/$/,'')}/${protocol==='openai-chat'?'chat/completions':protocol==='openai-responses'?'responses':protocol==='anthropic'?'messages':'images/generations'}`,
        status:configured.length?'verified':'unconfigured',models:verified,
        model_results:configured.map(model=>{const supported=verified.includes(model),capabilityEvidence=protocol==='openai-images'?[{capability:'images',status:supported?'supported':'unsupported',source:'cache'}]:[{capability:'text',status:supported?'supported':'unsupported',source:'cache'},{capability:'stream',status:supported?'supported':'unsupported',source:'cache'},...(protocol==='anthropic'?[{capability:'structured_output',status:'unsupported',source:'protocol-contract'}]:[{capability:'structured_output',status:supported?'supported':'unsupported',source:'cache'}])];return {model,status:supported?'verified':'failed',capabilities:{...normalizedProviderModelCapabilities(protocol,model,{}),...(protocol==='openai-images'?{images:supported}:{text:supported,stream:supported,structured_output:protocol==='anthropic'?false:supported})},capability_evidence:capabilityEvidence,cache_hit:true,probe_requests:0,...(supported?{}:{error:'model is not available on this protocol'})}}),
        ...(configured.length?{}:{error:'select at least one model to verify this protocol endpoint'}),
      }}),
    };
  }
  async syncProviderModels(id) {
    await sleep(180);
    const provider=this.data.providers.find(item=>item.id===id);
    if(!provider)throw new Error(`Unknown provider ${id}`);
    provider.raw||={};
    const now=new Date(),interval=provider.raw.model_sync_interval||'1h',unsupported=provider.raw.model_sync?.status==='unsupported',disabled=provider.enabled===false;
    const status=unsupported?'unsupported':disabled?'disabled':'succeeded',next=status==='succeeded'&&interval!=='off'?new Date(now.getTime()+(interval==='30m'?1800000:3600000)).toISOString():null;
    provider.raw.model_sync={status,last_started_at:now.toISOString(),last_completed_at:now.toISOString(),last_success_at:status==='succeeded'?now.toISOString():provider.raw.model_sync?.last_success_at||null,next_run_at:next,last_error_code:'',last_error:'',discovered_models:(provider.upstreamModels||[]).length,added_models:0,created_logical_models:0,revision:this.revision};
    return clone({...provider.raw.model_sync,added_model_ids:[],created_logical_model_ids:[]});
  }
  async addProvider(provider) {
    await sleep(140);
    const protocolModels = Array.isArray(provider.protocols)
      ? [...new Set(provider.protocols.flatMap(item=>item.models||[]))]
      : null;
    const normalized = {
      id: provider.id || provider.name.toLowerCase().replace(/[^a-z0-9]+/g,'-'),
      name: provider.name,
      type: provider.type,
      status: 'Healthy', enabled:true, capacity: 100, connections: 1, available: 1, requestRate: 0,
      servingModels: protocolModels || provider.models || [], logo: provider.logo || 'router',
      raw:{base_url:provider.baseUrl,protocol_conversion:Object.fromEntries((provider.protocols||[]).map(item=>[item.protocol,Boolean(item.autoConvert)])),scheduled_message:provider.scheduledMessage||null,concurrency:provider.concurrency||null,model_sync_interval:provider.modelSyncInterval||'1h',model_sync_catalog:provider.modelSyncCatalog?{type:'volcengine-plan',plan:provider.modelSyncCatalog.plan==='coding'?'coding':'agent',credential_ref:provider.modelSyncCatalog.credentialRef||`store:${provider.id||provider.name.toLowerCase().replace(/[^a-z0-9]+/g,'-')}-model-catalog-demo`}:null,model_sync:{status:'idle',last_started_at:null,last_completed_at:null,last_success_at:null,next_run_at:null,last_error_code:'',last_error:'',discovered_models:0,added_models:0,created_logical_models:0,revision:this.revision}}
    };
    this.data.providers.push(normalized);
    const models = Array.isArray(provider.models) ? provider.models : String(provider.models || '').split(',').map(value=>value.trim()).filter(Boolean);
    const importedAuth=Boolean(provider.authFile),oauthSignIn=/oauth/i.test(provider.authType||'')&&!importedAuth;
    this.data.connections[normalized.id] = protocolModels ? provider.protocols.filter(item=>(item.models||[]).length).map(item=>({
      id:item.connectionId||`${normalized.id}-${item.protocol}`, name:item.connectionIdentity||`${provider.name} · ${item.protocol}`, type:'API Key / Token',
      account:item.connectionIdentity||`${provider.name} · ${item.protocol}`, status:'Standby', enabled:item.connectionEnabled!==false, load:0, capacity:100,
      lastIssue:'—', lastUsed:'never', cooldownSeconds:0, conversations:0, requestsMoved:0,
      baseUrl:item.overrideUrl&&item.urlMode!=='full'?item.baseUrl:'',fullUrl:item.overrideUrl&&item.urlMode==='full'?item.fullUrl:'', raw:{credential_ref:item.credentialRef||provider.credentialRef||'',base_url:item.overrideUrl&&item.urlMode!=='full'?item.baseUrl:'',full_url:item.overrideUrl&&item.urlMode==='full'?item.fullUrl:'',concurrency:item.connectionConcurrency||null},
      models:item.models.map(upstreamModel=>providerConnectionModel(item,upstreamModel)),
    })) : [{
      id:normalized.id, name:provider.account||normalized.name, type:provider.authType||'API Key / Token',
      account:provider.account||normalized.name, status:oauthSignIn?'Unavailable':'Standby',
      enabled:!oauthSignIn, load:0, capacity:100, lastIssue:'—', lastUsed:'never',
      cooldownSeconds:0, conversations:0, requestsMoved:0,
      models:models.map(upstream_model=>{const protocol=normalized.type==='cliproxy-codex'?codexProviderModelProtocol(upstream_model):(provider.protocol||'openai-chat');return {upstream_model,protocols:[protocol],capabilities:normalizedProviderModelCapabilities(protocol,upstream_model)}}),
    }];
    if(importedAuth)await this.importConnectionOAuth(normalized.id,normalized.id,provider.authFile);
    return clone(normalized);
  }
  async updateProvider(id, patch) {
    await sleep();
    const provider = this.data.providers.find(x => x.id === id);
    if (!provider) throw new Error(`Unknown provider ${id}`);
    if (patch.name !== undefined) provider.name = patch.name;
    if (patch.base_url !== undefined) provider.baseUrl = patch.base_url;
    if (patch.enabled !== undefined) {
      provider.enabled = Boolean(patch.enabled);
      provider.status = provider.enabled ? 'Healthy' : 'Unavailable';
    }
    if (patch.concurrency !== undefined) {
      provider.raw = provider.raw || {};
      provider.raw.concurrency = patch.concurrency;
    }
    if (patch.scheduled_message !== undefined) {
      provider.raw = provider.raw || {};
      provider.raw.scheduled_message = patch.scheduled_message;
    }
    if (patch.model_sync_interval !== undefined) {
      provider.raw = provider.raw || {};
      provider.raw.model_sync_interval = ['off','30m','1h'].includes(patch.model_sync_interval)?patch.model_sync_interval:'1h';
    }
    if (patch.model_sync_catalog !== undefined) {
      provider.raw = provider.raw || {};
      provider.raw.model_sync_catalog = patch.model_sync_catalog ? clone(patch.model_sync_catalog) : null;
    }
    if (patch.connections !== undefined) {
      const connections = this.data.connections[id] || [];
      for (const connectionPatch of patch.connections) {
        const connection = connections.find(item => item.id === connectionPatch.id);
        if (!connection) throw new Error(`Unknown connection ${connectionPatch.id}`);
        connection.models = clone(connectionPatch.models || []);
      }
    }
    return this.getProvider(id);
  }
  async saveProviderProtocolConfig(id, config) {
    const provider = this.data.providers.find(x => x.id === id);
    if (!provider) throw new Error(`Unknown provider ${id}`);
    provider.name = config.name;
    provider.baseUrl = config.baseUrl;
    provider.enabled = Boolean(config.enabled);
    const catalog=config.modelSyncCatalog?{type:'volcengine-plan',plan:config.modelSyncCatalog.plan==='coding'?'coding':'agent',credential_ref:config.modelSyncCatalog.credentialRef||provider.raw?.model_sync_catalog?.credential_ref||`store:${id}-model-catalog-demo`}:null;
    provider.raw = {...(provider.raw||{}), name:config.name, base_url:config.baseUrl, protocol_conversion:Object.fromEntries(config.protocols.map(item=>[item.protocol,Boolean(item.autoConvert)])),scheduled_message:config.scheduledMessage||null,concurrency:config.concurrency||null,model_sync_interval:config.modelSyncInterval||'1h',model_sync_catalog:catalog};
    this.data.connections[id] = config.protocols.filter(item=>(item.models||[]).length).map(item=>({
      id:item.connectionId||`${id}-${item.protocol}`, name:item.connectionIdentity||`${config.name} · ${item.protocol}`,
      account:item.connectionIdentity||`${config.name} · ${item.protocol}`, type:'API Key / Token', enabled:item.connectionEnabled!==false,
      baseUrl:item.overrideUrl&&item.urlMode!=='full'?item.baseUrl:'',fullUrl:item.overrideUrl&&item.urlMode==='full'?item.fullUrl:'', status:'Standby', load:0, capacity:100,
      lastIssue:'—', lastUsed:'never', cooldownSeconds:0, conversations:0, requestsMoved:0,
      raw:{credential_ref:item.credentialRef||config.credentialRef||'',base_url:item.overrideUrl&&item.urlMode!=='full'?item.baseUrl:'',full_url:item.overrideUrl&&item.urlMode==='full'?item.fullUrl:'',concurrency:item.connectionConcurrency||null},
      models:item.models.map(upstreamModel=>providerConnectionModel(item,upstreamModel)),
    }));
    return this.getProvider(id);
  }
  async deleteProvider(id) {
    await sleep();
    const index = this.data.providers.findIndex(x => x.id === id);
    if (index < 0) throw new Error(`Unknown provider ${id}`);
    this.data.providers.splice(index, 1);
    delete this.data.connections[id];
    for (const model of this.data.models) {
      const before = this.data.routes[model.id] || [];
      const after = before.filter(route => route.id !== id);
      if (after.length === before.length) continue;
      this.data.routes[model.id] = after;
      model.routes = clone(after);
      if (!after.length) {
        model.enabled = false;
        model.status = 'Unavailable';
        if (model.raw) model.raw.enabled = false;
      }
    }
  }
  async addConnection(providerId, connection) {
    await sleep(150);
    const list = this.data.connections[providerId] || (this.data.connections[providerId] = []);
    const item = {
      id: connection.id || `connection-${list.length + 1}`,
      name: connection.name || `Account ${String.fromCharCode(65 + list.length)}`,
      type: connection.authType || connection.type || 'OAuth', account: connection.account || 'new-account', region: connection.region || 'US East',
      status:'Standby', enabled:true, load:0, capacity:100, lastIssue:'—', lastUsed:'never', cooldownSeconds:0, conversations:0, requestsMoved:0,
      models:String(connection.models||'').split(',').map(value=>value.trim()).filter(Boolean).map(upstream_model=>{const protocol=codexProviderModelProtocol(upstream_model);return {upstream_model,protocols:[protocol],capabilities:normalizedProviderModelCapabilities(protocol,upstream_model)}}),
      raw:{credential_ref:`oauth:codex:${connection.account||'new-account'}`},
    };
    list.push(item);
    const provider = this.data.providers.find(x => x.id === providerId);
    if (provider) { provider.connections = list.length; provider.available = list.filter(x => x.status !== 'Cooling').length; }
    if(connection.authFile)await this.importConnectionOAuth(providerId,item.id,connection.authFile);
    return clone(item);
  }
  async updateConnection(providerId, connectionId, patch) {
    await sleep();
    const list = this.data.connections[providerId] || [];
    const item = list.find(x => x.id === connectionId);
    if (!item) throw new Error(`Unknown connection ${connectionId}`);
    Object.assign(item, clone(patch));
    if(patch.concurrency!==undefined)item.raw={...item.raw,concurrency:clone(patch.concurrency)};
    if (patch.identity !== undefined) item.name = item.account = patch.identity;
    return clone(item);
  }
  async rotateConnectionSecret(providerId, connectionId, secret) {
    await sleep();
    const item = (this.data.connections[providerId] || []).find(connection => connection.id === connectionId);
    if (!item) throw new Error(`Unknown connection ${connectionId}`);
    if (!secret) throw new Error('Replacement credential is required');
    item.credentialUpdatedAt = new Date().toISOString();
    return { rotated:true };
  }
  async deleteConnection(providerId, connectionId) {
    await sleep();
    const list = this.data.connections[providerId] || [];
    const index = list.findIndex(x => x.id === connectionId);
    if (index < 0) throw new Error(`Unknown connection ${connectionId}`);
    list.splice(index, 1);
    const provider = this.data.providers.find(x => x.id === providerId);
    if (provider) {
      provider.connections = list.length;
      provider.available = list.filter(x => x.status !== 'Unavailable' && x.status !== 'Cooling').length;
    }
  }
  async getRequests(filters = {}) {
    await sleep();
    const query = String(filters.q || '').toLowerCase();
    const provider=this.data.providers.find(item=>item.id===filters.provider),providerNames=new Set([filters.provider,provider?.name].filter(Boolean));
    const filtered=this.data.requests.filter(item => {
      if (filters.model && item.model !== filters.model) return false;
      if (filters.provider && !providerNames.has(item.providerId) && !providerNames.has(item.provider)) return false;
      if (filters.api_key && item.apiKey !== filters.api_key) return false;
      if (filters.conversation_id && item.conversation !== filters.conversation_id) return false;
      if (filters.outcome === 'RECOVERED_SUCCESS' && !item.recovered) return false;
      if (filters.outcome === 'DIRECT_SUCCESS' && (item.recovered || item.failed || item.canceled)) return false;
      if (filters.outcome === 'FAILED' && !item.failed && !item.canceled && !String(item.result||'').toLowerCase().includes('fail')) return false;
      if (filters.status && String(item.status||(item.canceled?'canceled':item.failed?'failed':'success')).toLowerCase() !== String(filters.status).toLowerCase()) return false;
      return !query || [item.id, item.model, item.conversation, item.apiKey, item.providerId, item.upstreamModel, item.route]
        .some(value => String(value || '').toLowerCase().includes(query));
    });
    const offset=Number(filters.offset||0),limit=Number(filters.limit||filtered.length||1),items=clone(filtered.slice(offset,offset+limit));
    items.total=filtered.length;return items;
  }
  async getRequest(id) {
    await sleep();
    const detail = this.data.requestDetails[id];
    if (detail) return clone(detail);
    const req = this.data.requests.find(x => x.id === id);
    if (!req) throw new Error(`Unknown request ${id}`);
    return {
      ...clone(req), connectionRebinds:0, providerFallbacks:0, conversationRebind:'No', clientConnection:'Kept Open', recoveryOverhead:'0ms',
      trace:[
        { time:req.time, title:'Request Received', sub:'Client request accepted', latency:'0ms', kind:'normal' },
        { time:req.time, title:'Connection Selected', sub:`${req.route} selected`, latency:'8ms', kind:'selected' },
        { time:req.time, title:'Response Completed', sub:'Response returned to client', latency:req.latency, kind:'success' }
      ]
    };
  }
  async getQueue() {
    await sleep();
    const normalize=p=>({max_concurrent:p?.max_concurrent||0,max_queue:p?.max_queue||256,max_wait_seconds:p?.max_wait_seconds||120,queue_when_full:p?.queue_when_full!==false,queue_on_throttle:p?.queue_on_throttle!==false});
    const defaults=normalize(this.settings.concurrency),connections=[];
    for(const provider of this.data.providers)for(const c of this.data.connections[provider.id]||[]){const own=c.concurrency??c.raw?.concurrency,pp=provider.raw?.concurrency;connections.push({connection_id:c.id,provider_id:provider.id,identity:c.name||c.id,enabled:c.enabled!==false,concurrency:own||null,policy_source:own?'connection':pp?'provider':'global',runtime:{...normalize(own||pp||defaults),connection_id:c.id,in_flight:0,waiting:0,recovering:false}});}
    return {generated_at:new Date().toISOString(),waiting:0,running:0,defaults,connections,requests:[]};
  }
  async getSettings() { await sleep(); return {...clone(this.settings),revision:this.revision}; }
  async getPricingStatus() { return clone(this.settings.pricing_sync||{settings:{enabled:true,source:'litellm',interval_hours:24},total:0,stale:true}); }
  async refreshPricing() { const status=await this.getPricingStatus();status.last_success=new Date().toISOString();this.settings.pricing_sync=status;return clone(status); }
  async publishCodexClientVersion(version, revision, options) {
    await sleep();
    if(revision!==this.revision)throw new FerryError('Revision conflict. Reload settings and review again.',409);
    const previousManual=this.settings.codex_client_version||'';
    this.settings.codex_client_version=version;
    if(options)Object.assign(this.settings,options);
    if(previousManual&&!version&&this.settings.codex_auto_update&&compareCodexClientVersions(previousManual,this.settings.codex_auto_version||this.settings.codex_default_client_version||'0.153.0')>0)this.settings.codex_auto_version=previousManual;
    this.settings.codex_effective_client_version=version||this.settings.codex_auto_version||this.settings.codex_default_client_version||'0.153.0';
    this.settings.codex_update_status={status:version?'manual':this.settings.codex_auto_update?'pending':'disabled'};
    this.settings.revision=++this.revision;
    return clone(this.settings);
  }
  async updateSettings(patch) { await sleep(); const {pricing_sync,expected_revision,...rest}=clone(patch);if(rest.concurrency){if(expected_revision!==this.revision)throw Object.assign(new Error("Reload settings before saving concurrency"),{status:409});this.revision++;}if(rest.backup){const b=rest.backup;if(b.s3_credentials){b.s3={...b.s3,credentials_ref:'backup:s3:00000000-0000-0000-0000-000000000000'};delete b.s3_credentials;}else if(b.s3?.endpoint&&b.s3.endpoint===this.settings.backup?.s3?.endpoint){b.s3.credentials_ref=this.settings.backup.s3.credentials_ref;}}Object.assign(this.settings,rest);if(pricing_sync)this.settings.pricing_sync={...await this.getPricingStatus(),settings:pricing_sync};return clone(this.settings); }
  async getApiKeys() { await sleep(); return clone(this.data.apiKeys.map(item => ({ ...item, revealable:this.apiKeySecrets.has(item.id) }))); }
  async getApiKey(id) {
    await sleep();
    const item = this.data.apiKeys.find(x => x.id === id);
    if (!item) throw new Error(`Unknown API key ${id}`);
    const requests = this.data.requests.filter(x => x.apiKey === item.name);
    const failed = requests.filter(x => x.failed).length;
    const tokenCount = requests.reduce((total, request) => {
      const match = String(request.tokens || '').trim().match(/^([\d.]+)\s*([KMG])?$/i);
      if (!match) return total;
      const scale = {K:1e3,M:1e6,G:1e9}[String(match[2] || '').toUpperCase()] || 1;
      return total + Number(match[1]) * scale;
    }, 0);
    return clone({
      ...item,
      identity:{
        id:item.id, name:item.name, description:item.description || '', enabled:item.status === 'Active',
        revoked_at:item.status === 'Revoked' ? new Date().toISOString() : null,
        created_at:new Date(item.created).toISOString(), scope_models:item.scope === 'All models' ? [] : item.scope.split(',').map(x=>x.trim()),
        scope_protocols:item.scopeProtocols || [], expires_at:item.expiresAt || null,
        rate_limit_rpm:Number(item.rateLimitRPM || 0), monthly_budget_usd:Number(item.monthlyBudgetUSD || 0),
		exact_only:Boolean(item.exactOnly),
      },
      secrets:[{id:`${item.id}-active`,preview:item.preview,state:item.status === 'Revoked' ? 'RETIRED' : 'ACTIVE',revealable:this.apiKeySecrets.has(item.id),created_at:new Date(item.created).toISOString(),last_used_at:requests.length?new Date().toISOString():null}],
      usageSummary:{requests:requests.length,failed,input_tokens:tokenCount,output_tokens:0,total_cost:0,tokens_observed:requests.length>0,cost_observed:false},
      recentRequests:requests.map(x=>({id:x.id,logical_model_id:x.model,outcome:x.failed||x.canceled?'FAILED':x.recovered?'RECOVERED_SUCCESS':'DIRECT_SUCCESS',status:x.canceled?'canceled':x.failed?'failed':'success'})),
      revealable:this.apiKeySecrets.has(item.id),
    });
  }
  async createApiKey(input) {
    await sleep(100);
    const suffix = Math.random().toString(16).slice(2,6);
    const fullKey = `sk-ferry-${Math.random().toString(36).slice(2)}${Date.now().toString(36)}`;
    const item = { id:input.name, name:input.name, preview:`sk-…${suffix}`, status:'Active', scope:input.scope || 'All models', usage:0, created:'Aug 26, 2026', exactOnly:Boolean(input.exact_only) };
    this.data.apiKeys.unshift(item);
    this.apiKeySecrets.set(item.id, fullKey);
    return { key: fullKey, item: clone({ ...item, revealable:true }) };
  }
  async revealApiKey(id) {
    await sleep(40);
    const key = this.apiKeySecrets.get(id);
    if (!key) throw new Error('This API key cannot be revealed. Change the key once to enable reveal and copy.');
    return { key, id };
  }
  async revokeApiKey(id) {
    await sleep();
    const item = this.data.apiKeys.find(x => x.id === id);
    if (!item) throw new Error(`Unknown API key ${id}`);
    item.status = 'Revoked';
    this.apiKeySecrets.delete(id);
    return clone(item);
  }
  async updateApiKey(id, patch) {
    await sleep();
    const item = this.data.apiKeys.find(x => x.id === id);
    if (!item) throw new Error(`Unknown API key ${id}`);
    if (patch.enabled !== undefined) item.status = patch.enabled ? 'Active' : 'Disabled';
    if (patch.description !== undefined) item.description = patch.description;
    if (patch.scope_models !== undefined) item.scope = patch.scope_models.length ? patch.scope_models.join(', ') : 'All models';
    if (patch.scope_protocols !== undefined) item.scopeProtocols = clone(patch.scope_protocols);
    if (patch.expires_at !== undefined || patch.clear_expiry) item.expiresAt = patch.clear_expiry ? null : patch.expires_at;
    if (patch.rate_limit_rpm !== undefined) item.rateLimitRPM = patch.rate_limit_rpm;
    if (patch.monthly_budget_usd !== undefined) item.monthlyBudgetUSD = patch.monthly_budget_usd;
    if (patch.exact_only !== undefined) item.exactOnly = Boolean(patch.exact_only);
    return clone(item);
  }
  async rotateApiKey(id) {
    await sleep();
    const item = this.data.apiKeys.find(x => x.id === id);
    if (!item) throw new Error(`Unknown API key ${id}`);
    const key=`sk-ferry-${Math.random().toString(36).slice(2)}${Date.now().toString(36)}`;
    this.apiKeySecrets.set(id,key);
    return { key, item:clone(item) };
  }

  async retireApiKeySecret(id, secretId) {
    await sleep();
    return { id, secretId, retired:true };
  }
  async deleteApiKey(id) {
    await sleep();
    const index = this.data.apiKeys.findIndex(x => x.id === id);
    if (index < 0) throw new Error(`Unknown API key ${id}`);
    this.data.apiKeys.splice(index, 1);
  }
  async startConnectionOAuth(providerId, connectionId, options = {}) {
    await sleep();
    if (options.mode === 'device') {
      this.oauthDevicePolls.set(connectionId, 2);
      return { providerId, connectionId, mode:'device', status:'polling', userCode:'WDJB-MJHT', verificationUrl:'https://auth.openai.com/codex/device', intervalSeconds:5 };
    }
    return { providerId, connectionId, mode:'browser', status:'pending', authorizeUrl:'https://example.test/oauth/demo' };
  }
  async getConnectionOAuthStatus(providerId, connectionId) {
    await sleep(60);
    const item=(this.data.connections[providerId]||[]).find(connection=>connection.id===connectionId);
    if(!item)throw new Error(`Unknown connection ${connectionId}`);
    const remaining=this.oauthDevicePolls.get(connectionId);
    if(remaining>0){
      this.oauthDevicePolls.set(connectionId,remaining-1);
      return { status:'active', login:{ status:'polling', mode:'device', userCode:'WDJB-MJHT', verificationUrl:'https://auth.openai.com/codex/device' } };
    }
    return { status:'active', login:null };
  }
  async cancelConnectionOAuth(_providerId, connectionId) {
    await sleep();
    this.oauthDevicePolls.delete(connectionId);
    return { status:'cancelled' };
  }
  async completeConnectionOAuth(providerId, connectionId, _callbackUrl, options = {}) {
    await sleep();
    if (options.autoEnable !== false) {
      const item=(this.data.connections[providerId]||[]).find(connection=>connection.id===connectionId);
      if(item){item.status='Standby';item.enabled=true;item.auth='ready';}
    }
    return { providerId, connectionId, status:'active' };
  }
  async importConnectionOAuth(providerId, connectionId, authFile) {
    await sleep(80);
    const parsed=await readCodexAuthJSON(authFile),tokens=parsed?.tokens||parsed;
    if(!tokens?.access_token||!tokens?.refresh_token)throw new Error('auth.json must contain ChatGPT access_token and refresh_token values.');
    const item=(this.data.connections[providerId]||[]).find(connection=>connection.id===connectionId);
    if(!item)throw new Error(`Unknown connection ${connectionId}`);
    item.status='Standby';item.enabled=true;item.auth='ready';item.accountId=tokens.account_id||parsed.account_id||'demo-account';
    return {providerId,connectionId,status:'active',account:{name:item.account,account_id:item.accountId,plan_type:'pro',expired:false}};
  }
  async getConnectionOAuthUsage(providerId, connectionId, _options = {}) {
    await sleep(100);
    const item=(this.data.connections[providerId]||[]).find(connection=>connection.id===connectionId);
    if(!item)throw new Error(`Unknown connection ${connectionId}`);
    const now=Math.floor(Date.now()/1000);
    return {account_id:'demo-'+connectionId,plan_type:'pro',rate_limit:{allowed:true,limit_reached:false,primary_window:{used_percent:93,limit_window_seconds:18000,reset_after_seconds:2700,reset_at:now+2700},secondary_window:{used_percent:12,limit_window_seconds:604800,reset_after_seconds:172800,reset_at:now+172800}},credits:{has_credits:true,unlimited:false,balance:'42.5'},additional_rate_limits:[{limit_name:'GPT-5.3-Codex-Spark',metered_feature:'spark',rate_limit:{allowed:true,limit_reached:false,primary_window:{used_percent:8,limit_window_seconds:18000,reset_after_seconds:2700,reset_at:now+2700}}}],rate_limit_reset_credits:{available_count:3-(this.demoUsageResetRequests||[]).filter(r=>r.account_id==='demo-'+connectionId&&r.status==='reset').length,total_earned_count:3,credits:[0,1,2].map(i=>({id:'demo-credit-'+connectionId+'-'+i,reset_type:'codex_rate_limits',status:(this.demoUsageResetRequests||[]).some(r=>r.credit.id==='demo-credit-'+connectionId+'-'+i&&r.status==='reset')?'redeemed':'available',granted_at:new Date((now-86400)*1000).toISOString(),expires_at:i===2?null:new Date((now+(i+1)*86400)*1000).toISOString(),title:'Full reset (Weekly + 5 hr)',description:'Demo data'}))},observed_at:new Date().toISOString()};
  }
  async getUsageResetRequests() { return clone(this.demoUsageResetRequests||[]); }
  async prepareUsageReset(providerId,connectionId,creditId,accountId) {
    const usage=await this.getConnectionOAuthUsage(providerId,connectionId);
    const credit=usage.rate_limit_reset_credits.credits.find(c=>c.id===creditId&&c.status==='available');
    if(usage.account_id!==accountId||!credit)throw new Error('Demo reset target mismatch');
    this.demoUsageResetRequests||=[];
    const existing=this.demoUsageResetRequests.find(r=>r.account_id===accountId&&r.status==='pending');
    if(existing){if(existing.credit.id!==creditId)throw new Error('Review the pending request first');return clone(existing)}
    const request={id:'demo-reset-'+crypto.randomUUID(),provider_id:providerId,connection_id:connectionId,account:connectionId,account_id:accountId,credit,rate_limit:usage.rate_limit,created_by:'demo-admin',created_at:new Date().toISOString(),expires_at:new Date(Date.now()+600000).toISOString(),status:'pending',windows_reset:0};
    this.demoUsageResetRequests.unshift(request);return clone(request);
  }
  async confirmUsageReset(request) {
    const record=(this.demoUsageResetRequests||[]).find(r=>r.id===request.id&&r.provider_id===request.provider_id&&r.connection_id===request.connection_id&&r.account_id===request.account_id&&r.credit.id===request.credit.id);
    if(!record||!['pending','reset'].includes(record.status))throw new Error('Demo reset target mismatch');
    record.status='reset';record.windows_reset=2;return clone(record);
  }
  async cancelUsageReset(request) {
    const record=(this.demoUsageResetRequests||[]).find(r=>r.id===request.id&&r.provider_id===request.provider_id&&r.connection_id===request.connection_id);
    if(!record||record.status!=='pending')throw new Error('Demo request cannot be canceled');
    record.status='canceled';return clone(record);
  }
  async getOrganizations() { await sleep(); return clone(this.data.organizations); }
  async getRevisions() { await sleep(); return clone(this.data.revisions); }
  async getBilling(window = '720h') {
    await sleep();
    const body=demoBillingForWindow(this.data.billing,window);
    return billingView(body,window);
  }
}

export class FerryError extends Error {
  constructor(message, status, details = {}) {
    super(message);
    this.name = 'FerryError';
    this.status = status;
    Object.assign(this, details);
  }
}

const words = value => String(value || '').toLowerCase().split(/[_\s-]+/).filter(Boolean)
  .map(word => word[0].toUpperCase() + word.slice(1)).join(' ');
const slug = value => String(value || '').toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-|-$/g, '').slice(0, 60);
const providerCatalogSecretName = providerID => {
  const suffix=`-${Date.now().toString(36)}-${Math.random().toString(36).slice(2,8)}`;
  const base=`p-${slug(providerID)||'provider'}-model-catalog`;
  return `${base.slice(0,64-suffix.length).replace(/-+$/,'')}${suffix}`;
};
const number = value => Number.isFinite(Number(value)) ? Number(value) : null;
const compact = value => value === null || value === undefined ? '—' : new Intl.NumberFormat('en', { notation:'compact', maximumFractionDigits:2 }).format(value);
const currency = value => value === null || value === undefined ? '—' : new Intl.NumberFormat('en-US', { style:'currency', currency:'USD', maximumFractionDigits:4 }).format(value);
const duration = value => value === null || value === undefined ? '—' : value >= 1000 ? `${(value / 1000).toFixed(value >= 10000 ? 1 : 2)}s` : `${Math.round(value)}ms`;
const percentage = (part, total) => total > 0 ? `${(part / total * 100).toFixed(2)}%` : '—';
const dateLabel = value => value ? new Date(value).toLocaleDateString('en-US', { year:'numeric', month:'short', day:'numeric' }) : '—';
const timeLabel = value => value ? new Date(value).toLocaleTimeString('en-US', { hour12:false }) : '—';
const usageDetails = usage => {
  const input=Number(usage?.input_tokens||0),output=Number(usage?.output_tokens||0);
  const cacheRead=Number(usage?.cache_read_tokens||0),cacheWrite=Number(usage?.cache_write_tokens||0);
  const reasoning=Number(usage?.reasoning_tokens||0),total=input+output;
  const observed=total>0||cacheRead>0||cacheWrite>0||reasoning>0;
  return {
    usageObserved:observed,
    inputTokens:input,outputTokens:output,cacheReadTokens:cacheRead,cacheWriteTokens:cacheWrite,reasoningTokens:reasoning,
    tokens:observed?compact(total):'—',
    tokenSummary:observed?`${compact(input)} in · ${compact(output)} out`:'—',
    cacheSummary:observed?`${compact(cacheRead)} read · ${compact(cacheWrite)} write`:'—',
    reasoningSummary:observed?compact(reasoning):'—',
  };
};
const allocateWhole = (total, weights) => {
  const target=Math.max(0,Math.round(Number(total)||0)),safe=(weights||[]).map(value=>Math.max(0,Number(value)||0)),weightTotal=safe.reduce((sum,value)=>sum+value,0);
  if(!safe.length)return[];if(!weightTotal)return safe.map((_,index)=>index===0?target:0);
  const exact=safe.map(value=>value/weightTotal*target),allocated=exact.map(Math.floor),order=exact.map((value,index)=>({index,remainder:value-allocated[index]})).sort((left,right)=>right.remainder-left.remainder||left.index-right.index);
  for(let remaining=target-allocated.reduce((sum,value)=>sum+value,0),index=0;remaining>0;remaining--,index++)allocated[order[index%order.length].index]++;
  return allocated;
};
const demoBillingForWindow = (fixture, window) => {
  const base=clone(fixture),baseSummary=base.summary||{},hours=Math.max(1,Number.parseInt(window,10)||720),factor=hours/720,to=new Date(base.to||Date.now()),from=new Date(to.getTime()-hours*60*60*1000);
  const requests=Math.max(0,Math.round(Number(baseSummary.requests||0)*factor));
  const [succeeded,failed,canceled]=allocateWhole(requests,[baseSummary.succeeded,baseSummary.failed,baseSummary.canceled]);
  const [direct,recovered]=allocateWhole(succeeded,[base.outcomes?.direct_success,base.outcomes?.recovered_success]);
  const inputTokens=Math.max(0,Math.round(Number(baseSummary.input_tokens||0)*factor)),outputTokens=Math.max(0,Math.round(Number(baseSummary.output_tokens||0)*factor));
  const totalCostCents=Math.max(0,Math.round(Number(baseSummary.total_cost||0)*factor*100));
  const distributeDimension=source=>{
    const entries=Object.entries(source||{}),ids=entries.map(([id])=>id),usage=entries.map(([,value])=>value||{});
    const requestValues=allocateWhole(requests,usage.map(value=>value.requests)),failedValues=allocateWhole(failed,usage.map(value=>value.failed)),canceledValues=allocateWhole(canceled,usage.map(value=>value.canceled));
    const inputValues=allocateWhole(inputTokens,usage.map(value=>value.input_tokens)),outputValues=allocateWhole(outputTokens,usage.map(value=>value.output_tokens));
    const costValues=allocateWhole(totalCostCents,usage.map(value=>value.cost_observed?value.total_cost:0));
    return Object.fromEntries(ids.map((id,index)=>[id,{requests:requestValues[index],failed:failedValues[index],canceled:canceledValues[index],input_tokens:inputValues[index],output_tokens:outputValues[index],tokens_observed:Boolean(usage[index].tokens_observed),...(usage[index].cost_observed?{total_cost:costValues[index]/100,cost_observed:true}:{cost_observed:false})}]));
  };
  const sourceTrend=base.trend||[],trendRequests=allocateWhole(requests,sourceTrend.map(point=>point.requests)),trendCosts=allocateWhole(totalCostCents,sourceTrend.map(point=>point.total_cost));
  const duration=to.getTime()-from.getTime(),trend=sourceTrend.map((_,index)=>({from:new Date(from.getTime()+duration*index/sourceTrend.length).toISOString(),to:new Date(from.getTime()+duration*(index+1)/sourceTrend.length).toISOString(),requests:trendRequests[index],total_cost:trendCosts[index]/100}));
  return {from:from.toISOString(),to:to.toISOString(),summary:{requests,succeeded,failed,canceled,input_tokens:inputTokens,output_tokens:outputTokens,tokens_observed:Boolean(baseSummary.tokens_observed),total_cost:totalCostCents/100,cost_observed:Boolean(baseSummary.cost_observed)},outcomes:{direct_success:direct,recovered_success:recovered,failed:failed+canceled},by_provider:distributeDimension(base.by_provider),by_model:distributeDimension(base.by_model),trend};
};
const billingView = (body, window) => {
  const summary=body.summary||{},totalTokens=Number(summary.input_tokens||0)+Number(summary.output_tokens||0);
  const totalCost=number(summary.total_cost??summary.est_cash_cost),costObserved=Boolean(summary.cost_observed);
  const bars=(source,unassigned)=>Object.entries(source||{}).map(([id,usage])=>({
    id,name:id||unassigned,
    requests:Number(usage.requests||0),failed:Number(usage.failed||0),canceled:Number(usage.canceled||0),
    tokens:usage.tokens_observed?Number(usage.input_tokens||0)+Number(usage.output_tokens||0):null,
    amount:usage.cost_observed?currency(number(usage.total_cost??usage.est_cash_cost)):'—',
    pct:costObserved&&totalCost>0?Number(usage.total_cost??usage.est_cash_cost??0)/totalCost*100:0,
  })).sort((left,right)=>right.requests-left.requests||left.name.localeCompare(right.name));
  return {
    totalCost:costObserved?currency(totalCost):'—',totalRequests:compact(summary.requests),totalTokens:summary.tokens_observed?compact(totalTokens):'—',
    avgCost:costObserved&&summary.tokens_observed&&totalTokens>0&&totalCost!==null?currency(totalCost/totalTokens*1000):'—',
    recoveredRequests:Number(body.outcomes?.recovered_success||0),failedRequests:Number(summary.failed||0),canceledRequests:Number(summary.canceled||0),costObserved,
    providers:bars(body.by_provider,'No provider selected'),models:bars(body.by_model,'No model selected'),trend:body.trend||[],window,
    reconciliation:{schema_version:1,window,from:body.from,to:body.to,summary,outcomes:body.outcomes||{},by_provider:body.by_provider||{},by_model:body.by_model||{},trend:body.trend||[]},
  };
};
const cacheHitMetric = (raw = {}) => ({
  id:String(raw.id||''),requests:Number(raw.requests||0),observedRequests:Number(raw.observed_requests||0),
  eligibleInputTokens:Number(raw.cache_eligible_input_tokens||0),cacheReadTokens:Number(raw.cache_read_tokens||0),
  cacheWriteTokens:Number(raw.cache_write_tokens||0),
  hitRate:raw.hit_rate_pct===null||raw.hit_rate_pct===undefined?null:number(raw.hit_rate_pct),
});
const cacheHitOverview = raw => ({
  overall:cacheHitMetric(raw?.overall),
  apiKeys:(raw?.api_keys||[]).map(cacheHitMetric),
  logicalModels:(raw?.logical_models||[]).map(cacheHitMetric),
});
const aggregateTrafficTrend = (routes, dimension) => {
  const bucketMeta=new Map(),groups=new Map();
  const identity=route=>{
    if(dimension==='logical_model')return route.logical_model_id?{id:route.logical_model_id,label:route.logical_model_id}:null;
    if(dimension==='provider')return route.provider_id?{id:route.provider_id,label:route.provider_id}:null;
    if(!route.provider_id||!route.upstream_model)return null;
    return {id:`${route.provider_id}\u0000${route.upstream_model}`,label:`${route.provider_id} / ${route.upstream_model}`};
  };
  for(const route of routes||[]){
    const selected=identity(route);if(!selected)continue;
    let group=groups.get(selected.id);
    if(!group){group={...selected,buckets:new Map()};groups.set(selected.id,group)}
    (route.points||[]).forEach((point,index)=>{
      const key=point.from||`bucket-${index}`;
      if(!bucketMeta.has(key))bucketMeta.set(key,{from:point.from||null,to:point.to||null,index});
      const bucket=group.buckets.get(key)||{requests:0,succeeded:0,failed:0};
      bucket.requests+=Number(point.requests||0);bucket.succeeded+=Number(point.succeeded||0);bucket.failed+=Number(point.failed||0);
      group.buckets.set(key,bucket);
    });
  }
  const bucketKeys=[...bucketMeta.entries()].sort((left,right)=>{
    const leftTime=Date.parse(left[1].from||''),rightTime=Date.parse(right[1].from||'');
    return Number.isFinite(leftTime)&&Number.isFinite(rightTime)?leftTime-rightTime:left[1].index-right[1].index;
  }).map(([key])=>key);
  return [...groups.values()].map(group=>{
    const points=bucketKeys.map(key=>({...bucketMeta.get(key),...(group.buckets.get(key)||{requests:0,succeeded:0,failed:0})}));
    const requests=points.reduce((sum,point)=>sum+point.requests,0),succeeded=points.reduce((sum,point)=>sum+point.succeeded,0);
    const failed=points.reduce((sum,point)=>sum+point.failed,0);
    return {id:group.id,label:group.label,requests,succeeded,failed,successRate:requests?succeeded/requests*100:null,points};
  }).sort((left,right)=>right.requests-left.requests||left.label.localeCompare(right.label));
};
const minutesIn = body => {
  const from = Date.parse(body?.from || ''), to = Date.parse(body?.to || '');
  return Number.isFinite(from) && Number.isFinite(to) && to > from ? (to - from) / 60000 : 15;
};

function connectionRole(connection) {
  const runtime = connection.runtime || {};
  if (!connection.enabled || runtime.auth === 'auth_error' || runtime.recovery_state === 'needs_attention') return 'Unavailable';
  if (['backing_off','cooling','quota_exhausted'].includes(runtime.recovery_state)) return 'Cooling';
  if (['ready_to_probe','probing'].includes(runtime.recovery_state)) return 'Recovering';
  if (runtime.reachability === 'offline') return 'Unavailable';
  if (runtime.cooling_until && Date.parse(runtime.cooling_until) > Date.now()) return 'Cooling';
  if ((runtime.quota_windows || []).some(window => window.availability === 'exhausted')) return 'Cooling';
  return Number(runtime.in_flight || 0) > 0 ? 'Serving' : 'Standby';
}

function remainingCapacity(connection) {
  const observed = (connection.runtime?.quota_windows || [])
    .map(window => number(window.remaining_fraction)).filter(value => value !== null);
  return observed.length ? Math.max(0, Math.min(100, Math.min(...observed) * 100)) : null;
}

export class HttpFerryAdapter extends FerryAdapter {
  async getNativeOAuthDrivers() {
    try {
      const body = await this.request('/api/oauth/native/drivers');
      return (Array.isArray(body?.drivers) ? body.drivers : []).filter(info => {
        try {
          const u = info.base_url ? new URL(info.base_url) : null;
          return nativeAccountPattern.test(info.id) && ['browser','device','import'].includes(info.mode) &&
            ['openai-chat','openai-responses','anthropic','openai-images'].includes(info.protocol) &&
            (!u ? info.mode==='import' : u.protocol==='https:' && !u.username && !u.password && !u.search && !u.hash);
        } catch { return false; }
      }).map(info => ({id:info.id, name:String(info.name || info.id).slice(0,128), mode:info.mode, base_url:info.base_url, protocol:info.protocol}));
    } catch { throw nativeAuthError(); }
  }
  async nativeOAuthRequest(driver, account, suffix, options) {
    try { return nativePublicView(await this.request(nativeAccountPath(driver,account)+suffix,options),driver,account); }
    catch { throw nativeAuthError(); }
  }
  async beginNativeOAuth(driver, account, fields = {}) {
    return this.nativeOAuthRequest(driver,account,'/login',{method:'POST',body:JSON.stringify({fields})});
  }
  async completeNativeOAuth(driver, account, callback = '') {
    return this.nativeOAuthRequest(driver,account,'/complete',{method:'POST',body:JSON.stringify({callback})});
  }
  async getNativeOAuthStatus(driver, account) { return this.nativeOAuthRequest(driver,account,'/status'); }
  async disconnectNativeOAuth(driver, account) { return this.nativeOAuthRequest(driver,account,'/login',{method:'DELETE'}); }

  // Validate before any secret write/config mutation or inference probe. Converted
  // ingress tabs have no native models and retain their separate conversion policy.
  async validateNativeProvider(provider, probing = false) {
    const items = (provider.protocols || []).filter(item => probing || item.models?.length);
    const usesNative = [provider.credentialRef,...items.map(item=>item.credentialRef)].some(ref=>String(ref||'').startsWith('native-oauth:'));
    if (!usesNative) return;
    const drivers = await this.getNativeOAuthDrivers();
    for (const item of items) {
      const ref = item.credentialRef || provider.credentialRef || '';
      if (!ref.startsWith('native-oauth:')) continue;
      let parsed = parseNativeCredential(ref), info = drivers.find(info=>info.id===parsed?.driver);
      const endpoint = item.baseUrl || provider.baseUrl;
      if (info && (endpoint!==info.base_url || item.protocol!==info.protocol)) {
        const view=await this.getNativeOAuthStatus(parsed.driver,parsed.account);
        if(view.status==='connected' && view.base_url && view.protocol)info={...info,base_url:view.base_url,protocol:view.protocol};
      }
      if (!info || item.protocol!==info.protocol || endpoint!==info.base_url || item.fullUrl || provider.secret || item.secret) throw nativeAuthError();
    }
  }

  async resetCooldowns(scope = {}) {
    const { model_id, ...filter } = scope;
    const path = model_id ? `/api/models/${encodeURIComponent(model_id)}/clear-cooldown` : '/api/runtime/clear-cooldown';
    return this.request(path, { method: 'POST', body: JSON.stringify(filter) });
  }
  constructor(baseUrl = '', fetchImpl = defaultFetch) {
    super();
    this.mode = 'http';
    this.baseUrl = baseUrl.replace(/\/$/, '');
    this.fetchImpl = fetchImpl;
    this.rawProvidersPromise = null;
    this.capabilities = { organizationIsolation:false };
    this.csrfToken = this.cookieValue('smartapi_admin_csrf');
  }

  cookieValue(name) {
    const prefix = `${encodeURIComponent(name)}=`;
    const item = String(globalThis.document?.cookie || '').split(';').map(value => value.trim()).find(value => value.startsWith(prefix));
    return item ? decodeURIComponent(item.slice(prefix.length)) : '';
  }

  async request(path, options = {}) {
    const method = String(options.method || 'GET').toUpperCase();
    const csrf = this.csrfToken || this.cookieValue('smartapi_admin_csrf');
    const { headers: optionHeaders = {}, ...requestOptions } = options;
    const response = await this.fetchImpl(`${this.baseUrl}${path}`, {
      ...requestOptions,
      headers: { 'Content-Type':'application/json', ...(!['GET','HEAD','OPTIONS'].includes(method) && csrf ? {'X-CSRF-Token':csrf} : {}), ...optionHeaders },
    });
    const text = response.status === 204 ? '' : await response.text();
    let body = null;
    if (text) {
      try { body = JSON.parse(text); } catch { body = text; }
    }
    if (!response.ok) {
      const detail = body?.error;
      const structured = detail && typeof detail === 'object' ? detail : {};
      const message = structured.message || (typeof detail === 'string' ? detail : '') ||
        (typeof body === 'string' ? body : '') || response.statusText || 'Request failed';
      throw new FerryError(message, response.status, {
        code: structured.code || `HTTP_${response.status}`,
        currentRevision: structured.current_revision ?? body?.current_revision,
        retryable: structured.retryable ?? response.status >= 500,
        details: structured.details || body?.errors || null,
      });
    }
    return body;
  }

  async getAuthStatus() {
    const body = await this.request('/api/auth/status');
    this.csrfToken = this.cookieValue('smartapi_admin_csrf');
    return body;
  }

  async initializeAdmin(input) {
    const body = await this.request('/api/auth/initialize', { method:'POST', body:JSON.stringify(input) });
    this.csrfToken = body.csrf_token || this.cookieValue('smartapi_admin_csrf');
    return body;
  }

  async login(input) {
    const body = await this.request('/api/auth/login', { method:'POST', body:JSON.stringify(input) });
    this.csrfToken = body.csrf_token || this.cookieValue('smartapi_admin_csrf');
    return body;
  }

  async logout() {
    await this.request('/api/auth/logout', { method:'POST', body:'{}' });
    this.csrfToken = '';
  }

  async changePassword(input) {
    const body = await this.request('/api/auth/password', { method:'POST', body:JSON.stringify(input) });
    this.csrfToken = '';
    return body;
  }

  async getSecurityEvents() {
    const body = await this.request('/api/security-events');
    return body.events || [];
  }

  async getManagementTokens() {
    const body = await this.request('/api/management-tokens');
    return body.management_tokens || [];
  }

  async createManagementToken(input) {
    const body = await this.request('/api/management-tokens', { method:'POST', body:JSON.stringify(input) });
    return { token:body.management_token, secret:body.secret };
  }

  async updateManagementToken(id, patch) {
    const body = await this.request(`/api/management-tokens/${encodeURIComponent(id)}`, { method:'PATCH', body:JSON.stringify(patch) });
    return body.management_token;
  }

  async rotateManagementToken(id) {
    const body = await this.request(`/api/management-tokens/${encodeURIComponent(id)}/rotate`, { method:'POST', body:'{}' });
    return { token:body.management_token, secret:body.secret };
  }

  async revokeManagementToken(id) {
    const body = await this.request(`/api/management-tokens/${encodeURIComponent(id)}/revoke`, { method:'POST', body:'{}' });
    return body.management_token;
  }

  async deleteManagementToken(id) {
    return this.request(`/api/management-tokens/${encodeURIComponent(id)}`, { method:'DELETE' });
  }

  loadProvidersRaw(force = false) {
    if (force) this.rawProvidersPromise = null;
    if (!this.rawProvidersPromise) {
      this.rawProvidersPromise = this.request('/api/providers?window=15m')
        .catch(error => { this.rawProvidersPromise = null; throw error; });
    }
    return this.rawProvidersPromise;
  }

  providerNames(body) {
    return new Map((body?.providers || []).map(provider => [provider.id, provider.name || provider.id]));
  }

  mapRoute(step, index, providerNames) {
    return {
      id:step.provider_id,
      provider:providerNames.get(step.provider_id) || step.provider_id,
      model:step.upstream_model,
      status:step.runtime_state === 'available' ? 'Route available' : 'Route unavailable',
      serving:Math.round(Number(step.live_traffic_share || 0) * 10000) / 100,
      mode:index === 0 ? 'First' : 'Next',
      recoverOn:[...(step.recover_on || [])],
      eligibleConnections:Number(step.eligible_connections || 0),
      totalConnections:Number(step.total_connections || 0),
    };
  }

  mapModel(raw, windowMinutes, providerNames) {
    const traffic = raw.traffic || {};
    const requests = Number(traffic.requests || 0);
    const successful = Number(traffic.direct_traffic || 0) + Number(traffic.recovery_traffic || 0);
    const routes = (raw.default_route || []).map((step, index) => this.mapRoute(step, index, providerNames));
    return {
      id:raw.id,
      name:raw.display_name || raw.id,
      description:raw.description || raw.runtime_sentence || '',
      enabled:Boolean(raw.enabled),
      status:words(raw.state),
      requestRate:Math.round(requests / Math.max(1, windowMinutes) * 100) / 100,
      clientSuccess:percentage(successful, requests),
      firstToken:duration(traffic.avg_ttft_ms),
      completion:duration(traffic.avg_duration_ms),
      direct:Number(traffic.direct_traffic || 0),
      recovered:Number(traffic.recovery_traffic || 0),
      failed:Number(traffic.failed || 0),
      trend:[], routes,
      runtimeSentence:raw.runtime_sentence,
      conversationStrategy:raw.existing_conversation_strategy || 'keep-existing',
	  exactOnly:Boolean(raw.exact_only),
      raw,
    };
  }

  async getAnalytics({window='24h',to='',model='',provider=''}={}) {
    return this.request('/api/analytics?'+new URLSearchParams({window,...(to?{to}:{}),model,provider}));
  }

  async getOverview(window = '15m') {
    const body = await this.request(`/api/overview?window=${encodeURIComponent(window)}`);
    const outcomes = body.client_outcomes || {};
    const usage = body.usage_summary || {};
    const direct = Number(outcomes.direct_success || 0), recovered = Number(outcomes.recovered_success || 0), failed = Number(outcomes.failed || 0);
    const total = direct + recovered + failed;
    const managed = body.managed_automatically || [];
    const unavailableModels = body.needs_attention || [];
    const needsAttention = unavailableModels.length + (failed > 0 ? failed : 0);
    const status = body.status || {};
    const clientStatus = words(status.client_traffic || (total === 0 ? 'no_traffic' : failed > 0 ? 'degraded' : 'healthy'));
    const runtimeStatus = words(status.runtime_api || 'available');
    const configurationStatus = words(status.configuration || (unavailableModels.length ? 'needs_attention' : 'healthy'));
    const overallStatus = words(status.overall || status.runtime_api || 'available');
    const routeTrend=body.traffic_trend||[];
    return {
      systemStatus:runtimeStatus, runtimeStatus, clientStatus, configurationStatus, overallStatus,
      requestRate:Math.round(Number(body.rates?.requests_per_minute || 0) * 100) / 100,
      tokenRate:usage.tokens_observed ? compact(body.rates?.tokens_per_minute) : '—',
      hasTraffic:total > 0,
      activeConversations:Number(body.active_conversations || 0),
      medianLatency:duration(usage.median_latency_ms),
      recoveryOverhead:duration(usage.avg_recovery_latency_ms),
      directSuccessPct:total ? direct / total * 100 : 0,
      recoveredPct:total ? recovered / total * 100 : 0,
      failedPct:total ? failed / total * 100 : 0,
      recoveredCount:recovered,
      providerFallbacks:null,
      needsAttention, configurationIssues:unavailableModels.length,
      attentionDetail:[
        failed ? `${failed} client request${failed === 1 ? '' : 's'} failed` : '',
        unavailableModels.length ? `${unavailableModels.length} model${unavailableModels.length === 1 ? '' : 's'} degraded or unavailable` : '',
      ].filter(Boolean).join('; '),
      totalRequests:compact(usage.requests),
      totalTokens:usage.tokens_observed ? compact(Number(usage.input_tokens || 0) + Number(usage.output_tokens || 0)) : '—',
      estimatedCost:usage.cost_observed ? currency(number(usage.total_cost ?? usage.est_cash_cost)) : '—',
      cacheHit:cacheHitOverview(body.cache_hit_summary),
      managedEvents:managed.map(id => ({ title:`${id} managed automatically`, detail:'Protection is active or recovered traffic was observed in the selected window.' })),
      modelProtection:body.model_protection || [],
      trend:body.trend || [],
      trafficTrends:{
        logicalModels:aggregateTrafficTrend(routeTrend,'logical_model'),
        providers:aggregateTrafficTrend(routeTrend,'provider'),
        providerModels:aggregateTrafficTrend(routeTrend,'provider_model'),
      },
      window,
    };
  }

  async getModels(window = '15m') {
    const [body, providers] = await Promise.all([
      this.request(`/api/models?window=${encodeURIComponent(window)}`), this.loadProvidersRaw(),
    ]);
    const names = this.providerNames(providers), windowMinutes = minutesIn(body);
    return (body.models || []).map(model => this.mapModel(model, windowMinutes, names));
  }

  async getModel(id) {
    const [raw, providers] = await Promise.all([
      this.request(`/api/models/${encodeURIComponent(id)}?window=15m`), this.loadProvidersRaw(),
    ]);
    const model = this.mapModel(raw, 15, this.providerNames(providers));
    const overrides = (raw.overrides || []).map(override => ({
      id:override.id, type:'API Key / Capability', matcher:(override.callers || []).join(', ') || override.name,
      route:override.mode === 'custom_route' ? 'Custom route' : 'Adjust default', status:override.enabled ? 'Active' : 'Disabled',
      raw:override,
    }));
    return { model, routes:model.routes, overrides };
  }

  async createModel(input) {
    const mutation = await this.request('/api/models', { method:'POST', body:JSON.stringify(input) });
    await this.applyDraftMutation(mutation); this.rawProvidersPromise=null; return this.getModel(input.id);
  }
  async updateModel(id, patch) {
    const mutation = await this.request(`/api/models/${encodeURIComponent(id)}`, { method:'PATCH', body:JSON.stringify(patch) });
    await this.applyDraftMutation(mutation); return this.getModel(id);
  }
  async deleteModel(id) {
    const mutation = await this.request(`/api/models/${encodeURIComponent(id)}?cascade=true`, { method:'DELETE' });
    return this.applyDraftMutation(mutation);
  }
  async createOverride(modelId, input) {
    const mutation = await this.request(`/api/models/${encodeURIComponent(modelId)}/overrides`, { method:'POST', body:JSON.stringify(input) });
    return this.applyDraftMutation(mutation);
  }
  async updateOverride(modelId, id, input) {
    const mutation = await this.request(`/api/models/${encodeURIComponent(modelId)}/overrides/${encodeURIComponent(id)}`, { method:'PATCH', body:JSON.stringify(input) });
    return this.applyDraftMutation(mutation);
  }
  async deleteOverride(modelId, id) {
    const mutation = await this.request(`/api/models/${encodeURIComponent(modelId)}/overrides/${encodeURIComponent(id)}`, { method:'DELETE' });
    return this.applyDraftMutation(mutation);
  }
  async simulateModel(modelId, input={}) {
    return this.request(`/api/models/${encodeURIComponent(modelId)}/simulate`, { method:'POST', body:JSON.stringify(input) });
  }

  async reviewRouting(id, draft) {
    const providers = await this.loadProvidersRaw();
    const names = this.providerNames(providers);
    const body = await this.request(`/api/models/${encodeURIComponent(id)}/routing/review`, {
      method:'POST',
      body:JSON.stringify({
        routes:draft.routes.map(route => ({ provider_id:route.id, upstream_model:route.model, recover_on:[...(route.recoverOn || [])] })),
        conversation_strategy:draft.conversationStrategy || 'keep-existing',
      }),
    });
    return {
      modelId:id, revision:body.expected_next_revision,
      before:(body.before || []).map((route, index) => this.mapRoute(route, index, names)),
      after:(body.after || []).map((route, index) => this.mapRoute(route, index, names)),
      expectedTraffic:Math.round(Number(body.expected_traffic_per_minute || 0) * 100) / 100,
      activeConversations:Number(body.active_conversations || 0),
      conversationStrategy:body.conversation_strategy,
      warnings:body.warnings || [], draftId:body.draft_id, expectedRevision:body.expected_revision,
    };
  }

  async applyRouting(id, review) {
    const body = await this.request(`/api/models/${encodeURIComponent(id)}/routing/apply`, {
      method:'POST', body:JSON.stringify({ draft_id:review.draftId, expected_revision:review.expectedRevision }),
    });
    return {
      revision:body.revision,
      message:`${id} routing revision applied`,
      existingConversations:review.conversationStrategy === 'keep-existing' ? review.activeConversations : 0,
    };
  }

  mapProvider(raw, windowMinutes) {
    const connections = raw.connections || [];
    const observedCapacity = connections.map(remainingCapacity).filter(value => value !== null);
    return {
      id:raw.id, name:raw.name || raw.id, type:words(raw.type), status:words(raw.state),
      enabled:Boolean(raw.enabled), connectionItems:connections.map(connection => this.mapConnection(connection)),
      capacity:observedCapacity.length ? observedCapacity.reduce((sum, value) => sum + value, 0) / observedCapacity.length : null,
      connections:connections.length, available:Number(raw.available_connections || 0),
      requestRate:Math.round(Number(raw.traffic?.requests || 0) / Math.max(1, windowMinutes) * 100) / 100,
      servingModels:[...(raw.used_by_models || [])], logo:raw.type === 'cliproxy-codex' ? 'codex' : 'router',
      upstreamModels:(raw.model_catalog || []).map(model=>model.upstream_model).filter(Boolean),
      recovered:Number(raw.recovery?.recovery_traffic || 0), failed:Number(raw.recovery?.failed || 0),
      lastSuccessfulAt:raw.traffic?.last_success_at || null, raw,
    };
  }

  mapConnection(raw) {
    const runtime = raw.runtime || {};
    const cooldown = runtime.cooling_until ? Math.max(0, Math.ceil((Date.parse(runtime.cooling_until) - Date.now()) / 1000)) : 0;
    const retryAt = runtime.retry_at && Date.parse(runtime.retry_at) > 0 ? runtime.retry_at : runtime.cooling_until || null;
    const retrySeconds = retryAt ? Math.max(0, Math.ceil((Date.parse(retryAt) - Date.now()) / 1000)) : 0;
    const oauth = String(raw.credential_ref || '').startsWith('oauth:codex:');
    return {
      id:raw.id, name:raw.identity || raw.id, type:oauth ? 'OAuth' : 'API Key / Token',
      enabled:Boolean(raw.enabled),
      account:raw.identity || (oauth ? raw.credential_ref.slice('oauth:codex:'.length) : raw.credential_ref || '—'),
      region:'—', status:connectionRole(raw), load:null, capacity:remainingCapacity(raw),
      lastIssue:runtime.recovery_reason || runtime.last_failure?.message || '—', lastUsed:'—', cooldownSeconds:cooldown,
      recoveryState:runtime.recovery_state || '', recoveryReason:runtime.recovery_reason || '', retryAt, retrySeconds,
      conversations:Number(raw.affinity?.conversation || 0), requestsMoved:null,
      baseUrl:raw.base_url || '', fullUrl:raw.full_url || '', models:raw.models || [], auth:runtime.auth || 'unknown', reachability:runtime.reachability || 'unknown', raw,
    };
  }

  async getProviders() {
    const body = await this.loadProvidersRaw();
    const windowMinutes = minutesIn(body);
    return (body.providers || []).map(provider => this.mapProvider(provider, windowMinutes));
  }

  async getProvider(id) {
    const raw = await this.request(`/api/providers/${encodeURIComponent(id)}?window=15m`);
    return {
      provider:this.mapProvider(raw, 15), connections:(raw.connections || []).map(connection => this.mapConnection(connection)),
      liveRecovery:(raw.recent_recovery || []).map(event => ({
        requestId:event.request_id, time:timeLabel(event.at), from:event.from, to:event.to,
        action:event.action, result:words(event.result), overhead:duration(event.overhead_ms),
      })),
      recoveryPolicy:{ providerRetry:'Enabled', providerFallback:'Configured per model route', preserveClientStream:'Enabled', conversationHandling:'Revision strategy', maxRecoveryTime:'—' },
    };
  }

  async discoverProviderModels(provider) {
    const body = await this.request('/api/providers/discover-models', {
      method:'POST', body:JSON.stringify({
        type:provider.type, base_url:provider.baseUrl, protocol:provider.protocol,
        secret:provider.secret || '', credential_ref:provider.credentialRef || '',
      }),
    });
    return [...(body.models || [])];
  }

  async discoverProviderProtocols(provider) {
    await this.validateNativeProvider(provider, true);
    return this.request('/api/providers/discover-protocols', {
      method:'POST', body:JSON.stringify({
        provider_id:provider.providerId || provider.id || '', type:provider.type || 'custom', base_url:provider.baseUrl,
        secret:provider.secret || '', credential_ref:provider.credentialRef || '',
        refresh:Boolean(provider.refresh),
        protocols:(provider.protocols || []).map(item => ({
          protocol:item.protocol, base_url:item.urlMode==='full'?'':item.baseUrl || '', full_url:item.urlMode==='full'?item.fullUrl || '':'',
          secret:item.secret || '', credential_ref:item.credentialRef || '',
          models:item.models || [],
        })),
      }),
    });
  }

  async syncProviderModels(id) {
    return this.request(`/api/providers/${encodeURIComponent(id)}/models/sync`, { method:'POST' });
  }

  async applyDraftMutation(result, options = {}) {
    if (!result?.draft_id) return result;
    const draft = await this.request(`/api/drafts/${encodeURIComponent(result.draft_id)}`);
    const validation = await this.request(`/api/drafts/${encodeURIComponent(result.draft_id)}/validate`, { method:'POST', body:'{}' });
    if (!validation.valid) {
      throw new FerryError('Draft validation failed.', 422, { code:'VALIDATION_FAILURE', details:validation.errors || [] });
    }
    if (typeof options.onApplyStart === 'function') options.onApplyStart();
    return this.request(`/api/drafts/${encodeURIComponent(result.draft_id)}/apply`, {
      method:'POST', body:JSON.stringify({ expected_revision:draft.base_revision }),
    });
  }

  async prepareProviderModelSyncCatalog(id, provider) {
    const input = provider.modelSyncCatalog;
    if (!input) return { catalog:null, secretName:'' };
    if (input.type && input.type !== 'volcengine-plan') {
      throw new FerryError('Unsupported model catalog source.', 400, { code:'INVALID_REQUEST' });
    }
    const plan = input.plan === 'coding' ? 'coding' : 'agent';
    let credentialRef = String(input.credentialRef || '').trim();
    const credential = provider.modelSyncCatalogCredential;
    let secretName = '';
    if (credential) {
      const accessKeyID = String(credential.access_key_id || '').trim();
      const secretAccessKey = String(credential.secret_access_key || '').trim();
      const sessionToken = String(credential.session_token || '').trim();
      if (!accessKeyID || !secretAccessKey) {
        throw new FerryError('Volcengine Access Key ID and Secret Access Key are required for the Plan catalog.', 400, { code:'INVALID_REQUEST' });
      }
      secretName = providerCatalogSecretName(id);
      const value = JSON.stringify({ access_key_id:accessKeyID, secret_access_key:secretAccessKey, ...(sessionToken ? { session_token:sessionToken } : {}) });
      await this.request(`/api/secrets/${encodeURIComponent(secretName)}`, { method:'POST', body:JSON.stringify({ value }) });
      credentialRef = `store:${secretName}`;
    }
    if (!credentialRef.startsWith('store:')) {
      throw new FerryError('The Volcengine Plan catalog requires a separately stored control-plane credential.', 400, { code:'INVALID_REQUEST' });
    }
    return { catalog:{ type:'volcengine-plan', plan, credential_ref:credentialRef }, secretName };
  }

  async discardPreparedProviderModelSyncCatalog(secretName) {
    if (!secretName) return;
    try { await this.request(`/api/secrets/${encodeURIComponent(secretName)}`, { method:'DELETE' }); } catch {}
  }

  async addProvider(provider) {
    const id = provider.id || slug(provider.name);
    const type = provider.type === 'cliproxy-codex' || /codex/i.test(provider.type) ? 'cliproxy-codex' : 'custom';
    if (!provider.baseUrl) throw new FerryError('Provider endpoint is required.', 400, { code:'INVALID_REQUEST' });
    if (provider.protocols && type === 'custom') {
      const built = await this.buildProviderProtocolConnections(id, provider);
      const preparedCatalog = await this.prepareProviderModelSyncCatalog(id, provider);
      let applyStarted = false, applyCompleted = false;
      try {
          const mutation = await this.request('/api/providers', {
          method:'POST', body:JSON.stringify({
            id, name:provider.name, type, base_url:provider.baseUrl, enabled:Boolean(provider.enabled),
            protocol_conversion:built.protocolConversion, scheduled_message:provider.scheduledMessage||null, concurrency:provider.concurrency||null, model_sync_interval:provider.modelSyncInterval||'1h', model_sync_catalog:preparedCatalog.catalog, connections:built.connections,
          }),
        });
        await this.applyDraftMutation(mutation,{onApplyStart:()=>{applyStarted=true}});
        applyCompleted = true;
        this.rawProvidersPromise = null;
        const providers = await this.loadProvidersRaw(true);
        const created = (providers.providers || []).find(item => item.id === id);
        if (!created) throw new FerryError('Provider applied but is not visible in the active runtime.', 503, { code:'CAPABILITY_UNAVAILABLE', retryable:true });
        return this.mapProvider(created, minutesIn(providers));
      } catch (error) {
        if (!applyStarted) await this.discardPreparedProviderModelSyncCatalog(preparedCatalog.secretName);
        if (applyStarted && !applyCompleted && preparedCatalog.secretName) throw new FerryError(`Provider apply result is uncertain. The new catalog credential was retained; reload the Provider before retrying. ${error.message||''}`.trim(),error.status||503,{code:error.code||'APPLY_RESULT_UNCERTAIN',retryable:true});
        throw error;
      }
    }
    const upstreamModels = Array.isArray(provider.models) ? provider.models : String(provider.models || '').split(',').map(item => item.trim()).filter(Boolean);
    // Compatibility for older API clients. The reviewed console always sends
    // models and a credential, creating the complete single-layer Provider.
    if (!upstreamModels.length) {
      const mutation = await this.request('/api/providers', {
        method:'POST', body:JSON.stringify({ id, name:provider.name, type, base_url:provider.baseUrl, enabled:true, scheduled_message:provider.scheduledMessage||null, concurrency:provider.concurrency||null, model_sync_interval:provider.modelSyncInterval||'1h' }),
      });
      await this.applyDraftMutation(mutation);
      const providers = await this.loadProvidersRaw(true);
      const created = (providers.providers || []).find(item => item.id === id);
      if (!created) throw new FerryError('Provider applied but is not visible in the active runtime.', 503, { code:'CAPABILITY_UNAVAILABLE', retryable:true });
      return this.mapProvider(created, minutesIn(providers));
    }
    const authType = provider.authType || 'API Key';
    const importedAuth = /auth\.json|auth file/i.test(authType);
    const oauthAuth = /oauth/i.test(authType) || importedAuth;
    let credentialRef = provider.credentialRef || '';
    let secretName = '';
    if (oauthAuth) credentialRef = `oauth:codex:${slug(provider.account || id)}`;
    else if (provider.secret) {
      secretName = slug(`${id}-credential`);
      await this.request(`/api/secrets/${encodeURIComponent(secretName)}`, { method:'POST', body:JSON.stringify({ value:provider.secret }) });
      credentialRef = `store:${secretName}`;
    } else if (!credentialRef) throw new FerryError('API key/token or credential reference is required.', 400, { code:'INVALID_REQUEST' });
    let applyStarted = false, applyCompleted = false;
    try {
      const protocol = provider.protocol || (oauthAuth ? 'openai-responses' : 'openai-chat');
      const mutation = await this.request('/api/providers', {
        method:'POST', body:JSON.stringify({
          id, name:provider.name, type, base_url:provider.baseUrl, enabled:true, scheduled_message:provider.scheduledMessage||null, concurrency:provider.concurrency||null, model_sync_interval:provider.modelSyncInterval||'1h',
          connection:{ id, identity:provider.account || provider.name || id, enabled:!oauthAuth,
            credential:{secret_ref:credentialRef}, models:upstreamModels.map(upstream_model=>{const nativeProtocol=oauthAuth?codexProviderModelProtocol(upstream_model):protocol;return {
              upstream_model, protocols:[nativeProtocol], capabilities:normalizedProviderModelCapabilities(nativeProtocol,upstream_model),
            }}) },
        }),
      });
      await this.applyDraftMutation(mutation,{onApplyStart:()=>{applyStarted=true}});
      applyCompleted = true;
      const providers = await this.loadProvidersRaw(true);
      const created = (providers.providers || []).find(item => item.id === id);
      if (!created) throw new FerryError('Provider applied but is not visible in the active runtime.', 503, { code:'CAPABILITY_UNAVAILABLE', retryable:true });
      const mapped = this.mapProvider(created, minutesIn(providers));
      if (importedAuth) {
        await this.importConnectionOAuth(id, id, provider.authFile);
        await this.updateConnection(id, id, { enabled:true });
        return (await this.getProvider(id)).provider;
      }
      if (oauthAuth) {
        const login = await this.startConnectionOAuth(id, id);
        return {...mapped, oauthPending:true, authorizeUrl:login.authorizeUrl, connectionId:id};
      }
      return mapped;
    } catch (error) {
      if (secretName && !applyStarted) { try { await this.request(`/api/secrets/${encodeURIComponent(secretName)}`, {method:'DELETE'}); } catch {} }
      if (secretName && applyStarted && !applyCompleted) throw new FerryError(`Provider apply result is uncertain. The new connection credential was retained; reload the Provider before retrying. ${error.message||''}`.trim(),error.status||503,{code:error.code||'APPLY_RESULT_UNCERTAIN',retryable:true});
      throw error;
    }
  }

  async updateProvider(id, patch) {
    const mutation = await this.request(`/api/providers/${encodeURIComponent(id)}`, { method:'PATCH', body:JSON.stringify(patch) });
    await this.applyDraftMutation(mutation); this.rawProvidersPromise=null; return this.getProvider(id);
  }

  async buildProviderProtocolConnections(id, provider) {
    await this.validateNativeProvider(provider);
    const protocolItems = Array.isArray(provider.protocols) ? provider.protocols : [];
    const usedIDs = new Set();
    const stored = new Map();
    const saveSecret = async (name, value) => {
      if (!value) return '';
      if (!stored.has(name)) {
        await this.request(`/api/secrets/${encodeURIComponent(name)}`, { method:'POST', body:JSON.stringify({ value }) });
        stored.set(name, true);
      }
      return `store:${name}`;
    };
    let defaultCredentialRef = provider.credentialRef || '';
    if (provider.secret) defaultCredentialRef = await saveSecret(slug(`${id}-credential`), provider.secret);
    const connections = [];
    for (const item of protocolItems) {
      const models = [...new Set((item.models || []).map(model=>String(model).trim()).filter(Boolean))];
      if (!models.length) continue;
      let credentialRef = item.credentialRef || defaultCredentialRef;
      if (item.secret) credentialRef = await saveSecret(slug(`${id}-${item.protocol}-credential`), item.secret);
      if (!credentialRef) throw new FerryError(`A key is required for ${item.protocol}.`, 400, { code:'INVALID_REQUEST' });
      let connectionID = item.connectionId || `${id}-${item.protocol}`;
      if (usedIDs.has(connectionID)) connectionID = `${id}-${item.protocol}`;
      let suffix = 2;
      while (usedIDs.has(connectionID)) connectionID = `${id}-${item.protocol}-${suffix++}`;
      usedIDs.add(connectionID);
      connections.push({
        id:connectionID, identity:item.connectionIdentity || `${provider.name || id} · ${item.protocol}`,
        base_url:item.overrideUrl&&item.urlMode!=='full' ? item.baseUrl : '',
        full_url:item.overrideUrl&&item.urlMode==='full' ? item.fullUrl : '', enabled:item.connectionEnabled !== false,
        credential:{secret_ref:credentialRef},
        concurrency:item.connectionConcurrency || null,
        models:models.map(upstreamModel=>providerConnectionModel(item,upstreamModel)),
      });
    }
    if (!connections.length) throw new FerryError('Configure at least one native upstream model.', 400, { code:'INVALID_REQUEST' });
    const protocolConversion = {};
    for (const item of protocolItems) protocolConversion[item.protocol] = item.protocol==='openai-images'?false:Boolean(item.autoConvert);
    return {connections, protocolConversion};
  }

  async saveProviderProtocolConfig(id, provider) {
    const built = await this.buildProviderProtocolConnections(id, provider);
    const preparedCatalog = await this.prepareProviderModelSyncCatalog(id, provider);
    let applyStarted = false, applyCompleted = false;
    try {
      const mutation = await this.request(`/api/providers/${encodeURIComponent(id)}/protocol-config`, {
        method:'PUT', body:JSON.stringify({
          name:provider.name, base_url:provider.baseUrl, enabled:Boolean(provider.enabled),
          protocol_conversion:built.protocolConversion, scheduled_message:provider.scheduledMessage||null, concurrency:provider.concurrency||null, model_sync_interval:provider.modelSyncInterval||'1h', model_sync_catalog:preparedCatalog.catalog, connections:built.connections,
        }),
      });
      await this.applyDraftMutation(mutation,{onApplyStart:()=>{applyStarted=true}});
      applyCompleted = true;
      this.rawProvidersPromise = null;
      return this.getProvider(id);
    } catch (error) {
      if (!applyStarted) await this.discardPreparedProviderModelSyncCatalog(preparedCatalog.secretName);
      if (applyStarted && !applyCompleted && preparedCatalog.secretName) throw new FerryError(`Provider apply result is uncertain. The new catalog credential was retained; reload the Provider before retrying. ${error.message||''}`.trim(),error.status||503,{code:error.code||'APPLY_RESULT_UNCERTAIN',retryable:true});
      throw error;
    }
  }
  async deleteProvider(id) {
    const mutation = await this.request(`/api/providers/${encodeURIComponent(id)}?cascade=true`, { method:'DELETE' });
    await this.applyDraftMutation(mutation); this.rawProvidersPromise=null;
  }

  async addConnection(providerId, connection) {
    const id = connection.id || slug(`${providerId}-${connection.account || 'connection'}`);
    const authType = connection.authType || connection.type || 'Existing Credential';
    const importedAuth = /auth\.json|auth file/i.test(authType);
    const oauthAuth = /oauth/i.test(authType) || importedAuth;
    let credentialRef = connection.credentialRef || '';
    let secretName = '';
    if (oauthAuth) {
      credentialRef = `oauth:codex:${slug(connection.account || id)}`;
    } else if (/api key|token/i.test(authType)) {
      if (!connection.secret) throw new FerryError('API key or token is required.', 400, { code:'INVALID_REQUEST' });
      secretName = slug(`${providerId}-${id}-credential`);
      await this.request(`/api/secrets/${encodeURIComponent(secretName)}`, {
        method:'POST', body:JSON.stringify({ value:connection.secret }),
      });
      credentialRef = `store:${secretName}`;
    } else if (!credentialRef) {
      throw new FerryError('Existing credential reference is required.', 400, { code:'INVALID_REQUEST' });
    }
    const upstreamModels = Array.isArray(connection.models) ? connection.models : String(connection.models || '').split(',').map(item => item.trim()).filter(Boolean);
    if (!upstreamModels.length) throw new FerryError('At least one upstream model is required.', 400, { code:'INVALID_REQUEST' });
    const protocol = connection.protocol || (oauthAuth ? 'openai-responses' : 'openai-chat');
    let applied = false;
    try {
      const mutation = await this.request(`/api/providers/${encodeURIComponent(providerId)}/connections`, {
        method:'POST', body:JSON.stringify({
          id, identity:connection.account || id, enabled:!oauthAuth,
          credential:{ secret_ref:credentialRef },
          models:upstreamModels.map(upstream_model => {const nativeProtocol=oauthAuth?codexProviderModelProtocol(upstream_model):protocol;return {
            upstream_model, protocols:[nativeProtocol], capabilities:normalizedProviderModelCapabilities(nativeProtocol,upstream_model),
          }}),
        }),
      });
      await this.applyDraftMutation(mutation);
      applied = true;
      this.rawProvidersPromise = null;
      const data = await this.getProvider(providerId);
      const item = data.connections.find(candidate => candidate.id === id);
      if (!item) throw new FerryError('Connection applied but is not visible in the active runtime.', 503, { code:'CAPABILITY_UNAVAILABLE', retryable:true });
      if (importedAuth) {
        await this.importConnectionOAuth(providerId, id, connection.authFile);
        return await this.updateConnection(providerId, id, { enabled:true });
      }
      if (oauthAuth) {
        const login = await this.startConnectionOAuth(providerId, id);
        return { ...item, authorizeUrl:login.authorizeUrl, oauthPending:true };
      }
      return item;
    } catch (error) {
      if (secretName && !applied) {
        try { await this.request(`/api/secrets/${encodeURIComponent(secretName)}`, { method:'DELETE' }); } catch {}
      }
      throw error;
    }
  }

  async updateConnection(providerId, connectionId, patch) {
    const body = {};
    if (patch.status !== undefined) body.enabled = patch.status !== 'Unavailable';
    if (patch.enabled !== undefined) body.enabled = Boolean(patch.enabled);
    if (patch.identity !== undefined) body.identity = patch.identity;
    if (patch.credential !== undefined) body.credential = patch.credential;
    if (patch.models !== undefined) body.models = patch.models;
    if (patch.concurrency !== undefined) body.concurrency = patch.concurrency;
    const mutation = await this.request(`/api/providers/${encodeURIComponent(providerId)}/connections/${encodeURIComponent(connectionId)}`, {
      method:'PATCH', body:JSON.stringify(body),
    });
    await this.applyDraftMutation(mutation);
    this.rawProvidersPromise = null;
    return (await this.getProvider(providerId)).connections.find(item => item.id === connectionId);
  }

  async deleteConnection(providerId, connectionId) {
    const mutation = await this.request(`/api/providers/${encodeURIComponent(providerId)}/connections/${encodeURIComponent(connectionId)}?cascade=true`, { method:'DELETE' });
    await this.applyDraftMutation(mutation); this.rawProvidersPromise=null;
  }
  async testConnection(providerId, connectionId) {
    return this.request(`/api/providers/${encodeURIComponent(providerId)}/connections/${encodeURIComponent(connectionId)}/test`, { method:'POST', body:'{}' });
  }
  async rotateConnectionSecret(providerId, connectionId, secret) {
    return this.request(`/api/providers/${encodeURIComponent(providerId)}/connections/${encodeURIComponent(connectionId)}/rotate-secret`, { method:'POST', body:JSON.stringify({secret}) });
  }

  async startConnectionOAuth(providerId, connectionId, options = {}) {
    const body = await this.request(`/api/providers/${encodeURIComponent(providerId)}/connections/${encodeURIComponent(connectionId)}/oauth/login`, {
      method:'POST', body:JSON.stringify({ mode:options.mode === 'device' ? 'device' : 'browser' }),
    });
    const interval=Number(body.interval_seconds);
    return {
      providerId, connectionId, account:body.account,
      mode:body.mode || 'browser', status:body.status,
      authorizeUrl:body.authorize_url,
      userCode:body.user_code, verificationUrl:body.verification_url,
      intervalSeconds:Number.isFinite(interval) && interval > 0 ? interval : 5,
    };
  }

  async getConnectionOAuthStatus(providerId, connectionId) {
    const body = await this.request(`/api/providers/${encodeURIComponent(providerId)}/connections/${encodeURIComponent(connectionId)}/oauth/status`);
    return {
      status:body.status,
      login:body.login ? {
        status:body.login.status, mode:body.login.mode, error:body.login.error || '',
        userCode:body.login.user_code, verificationUrl:body.login.verification_url,
        authorizeUrl:body.login.authorize_url,
      } : null,
    };
  }

  async cancelConnectionOAuth(providerId, connectionId) {
    return this.request(`/api/providers/${encodeURIComponent(providerId)}/connections/${encodeURIComponent(connectionId)}/oauth/login`, { method:'DELETE' });
  }

  async completeConnectionOAuth(providerId, connectionId, callbackUrl, options = {}) {
    const body = await this.request(`/api/providers/${encodeURIComponent(providerId)}/connections/${encodeURIComponent(connectionId)}/oauth/callback`, {
      method:'POST', body:JSON.stringify({ callback_url:callbackUrl }),
    });
    // Re-login keeps the operator's routing gate; only first-time sign-in enables.
    if (options.autoEnable !== false) await this.updateConnection(providerId, connectionId, { enabled:true });
    return { providerId, connectionId, status:body.status, warning:body.warning };
  }

  async importConnectionOAuth(providerId, connectionId, authFile) {
    const authJSON = await readCodexAuthJSON(authFile);
    return this.request(`/api/providers/${encodeURIComponent(providerId)}/connections/${encodeURIComponent(connectionId)}/oauth/import`, {
      method:'POST', body:JSON.stringify({ auth_json:authJSON }),
    });
  }

  async getConnectionOAuthUsage(providerId, connectionId, options = {}) {
    const refresh=options.refresh!==false;
    const body = await this.request(`/api/providers/${encodeURIComponent(providerId)}/connections/${encodeURIComponent(connectionId)}/oauth/usage?refresh=${refresh}`);
    return body.usage;
  }

  async getUsageResetRequests() {
    return (await this.request('/api/oauth/usage-reset-requests')).requests || [];
  }
  usageResetPath(providerId,connectionId) {
    return `/api/providers/${encodeURIComponent(providerId)}/connections/${encodeURIComponent(connectionId)}/oauth/usage-reset-requests`;
  }
  async prepareUsageReset(providerId,connectionId,creditId,accountId) {
    return (await this.request(this.usageResetPath(providerId,connectionId),{method:'POST',body:JSON.stringify({credit_id:creditId,account_id:accountId})})).request;
  }
  async confirmUsageReset(request) {
    return (await this.request(this.usageResetPath(request.provider_id,request.connection_id)+'/'+encodeURIComponent(request.id)+'/confirm',{method:'POST',body:JSON.stringify({credit_id:request.credit.id,account_id:request.account_id,confirmation:'consume_one_codex_usage_reset'})})).request;
  }
  async cancelUsageReset(request) {
    return (await this.request(this.usageResetPath(request.provider_id,request.connection_id)+'/'+encodeURIComponent(request.id)+'/cancel',{method:'POST',body:'{}'})).request;
  }

  requestQuery(filters = {}) {
    const query = new URLSearchParams({ window:filters.window || '15m' });
    for (const [key, value] of Object.entries(filters)) {
      if (key !== 'window' && value !== '' && value !== null && value !== undefined) query.set(key, value);
    }
    return query.toString();
  }

  mapRequest(raw) {
    const outcome = String(raw.outcome || '').toUpperCase();
    const status = String(raw.status || '').toUpperCase();
    const live = ['RECEIVED','WAITING','ACTIVE','RECOVERING','STREAMING','DELIVERING'].includes(status);
    const recovered = outcome === 'RECOVERED_SUCCESS';
    const canceled = outcome === 'CANCELED' || outcome === 'CANCELLED' || status === 'CANCELED' || status === 'CANCELLED';
    const failed = outcome === 'FAILED' || status === 'FAILED';
    const usage = usageDetails(raw.usage);
    return {
      id:raw.id, time:timeLabel(raw.timestamp), timestamp:raw.timestamp,
      model:raw.logical_model_id || raw.upstream_model || '—', logicalModel:raw.logical_model_id || '—',
      upstreamModel:raw.upstream_model || '—', responseModel:raw.response_model || '', serviceTier:raw.service_tier || '', responseServiceTier:raw.response_service_tier || '', conversation:raw.conversation_id || '—',
      apiKey:raw.caller || '—', route:raw.provider_id || '—',
      live, result:live ? words(status) : canceled ? 'Canceled' : failed ? 'Failed' : recovered ? 'Success (Recovered)' : 'Success (Direct)',
      attempts:Number(raw.attempts || 0), latency:live?'—':duration(raw.duration_ms), totalDuration:live?'—':duration(raw.duration_ms), elapsedDuration:live?duration(raw.duration_ms):null,
      stream:Boolean(raw.stream), streamMode:raw.stream ? 'Streaming' : 'Non-streaming',
      ttft:Number(raw.ttft_ms || 0) > 0 ? duration(raw.ttft_ms) : '—',
      ...usage,
      tokensPerSecond:!live && usage.usageObserved && typeof raw.usage?.output_tokens==='number' && Number.isFinite(raw.usage.output_tokens) && raw.usage.output_tokens>=0 && typeof raw.duration_ms==='number' && Number.isFinite(raw.duration_ms) && raw.duration_ms>0 ? raw.usage.output_tokens/(raw.duration_ms/1000) : null,
      recovered, failed, canceled, failureMessage:raw.failure_message || '', providerId:raw.provider_id, connectionId:raw.connection_id,
      connectionRebinds:Number(raw.connection_rebind_count || 0),
      providerFallbacks:Number(raw.provider_fallback_count || 0),
      conversationRebind:Number(raw.conversation_migration_count || 0) ? 'Yes' : 'No',
      recoveryOverhead:duration(raw.recovery_added_latency_ms), raw,
    };
  }

  async getRequests(filters = {}) {
    const body = await this.request(`/api/requests?${this.requestQuery(filters)}`);
    const items=(body.requests || []).map(request => this.mapRequest(request));
    items.total=Number(body.total||0); items.offset=Number(filters.offset||0); items.limit=Number(filters.limit||100); return items;
  }

  traceTitle(event) {
    const affinityScope=String(event.affinity_scope||'').toUpperCase();
    const affinityLabel={CONVERSATION:'Conversation Route',CACHE:'Cache Affinity',CALLER:'Caller Affinity'}[affinityScope]||'Affinity Route';
    const titles = {
      REQUEST_RECEIVED:'Request Received', MODEL_RESOLVED:'Logical Model Resolved', ROUTE_RESOLVED:'Route Plan Resolved',
      BODY_RECEIVE_STARTED:'Receiving Request Body', BODY_RECEIVE_COMPLETED:'Request Body Received', BODY_RECEIVE_FAILED:'Request Body Receive Failed',
      BODY_PARSE_STARTED:'Parsing Request Body', BODY_PARSE_COMPLETED:'Request Body Parsed', BODY_PARSE_FAILED:'Request Body Parse Failed',
      PLANNING_STARTED:'Planning Route', PLANNING_COMPLETED:'Route Planning Completed', PLANNING_FAILED:'Route Planning Failed', PLANNING_CANCELED:'Route Planning Canceled',
      COMPATIBILITY_STARTED:'Checking Request Compatibility', COMPATIBILITY_COMPLETED:'Request Compatibility Checked',
      ADMISSION_COMPLETED:'Connection Admission Completed', UPSTREAM_FIRST_EVENT:'First Upstream Event Received',
      OVERRIDE_MATCHED:'Routing Override Matched', AFFINITY_FOUND:`${affinityLabel} Found`,
      AFFINITY_REUSED:`${affinityLabel} Reused`,
      AFFINITY_BROKEN:affinityScope==='CONVERSATION'?'Conversation Route Migration':`${affinityLabel} Not Reused`,
      TARGET_SKIPPED:'Candidate Excluded', CONNECTION_SELECTED:'Connection Selected', ATTEMPT_STARTED:`Provider Attempt #${event.attempt || ''}`,
      ATTEMPT_FAILED:`Provider Attempt #${event.attempt || ''} Failed`, ATTEMPT_SUCCEEDED:`Provider Attempt #${event.attempt || ''} Succeeded`,
      CONNECTION_REBOUND:'Provider Retry', PROVIDER_EXHAUSTED:'Provider Exhausted', ROUTE_FALLBACK:'Provider Fallback',
      CONVERSATION_NATIVE:'Conversation Continuation Reused', CONVERSATION_REHYDRATING:'Conversation Rehydrate', CONVERSATION_REHYDRATED:'Conversation Rehydrated',
      COMMIT:'Response Committed', COMPLETE:'Response Completed', RECOVERY_STOPPED:'Recovery Stopped',
    };
    return titles[event.type] || words(event.type);
  }

  traceKind(event) {
    const type=String(event.type||'').toUpperCase();
    const evidence=[event.failure_type,event.failure_scope,event.message].filter(Boolean).join(' ').toLowerCase();
    if(type==='CONNECTION_SELECTED')return 'selected';
    if(type==='TARGET_SKIPPED'){
      const mismatch=/compatib|incompatib|lossless|unsupported|not supported|mismatch|missing|required capabilit|cannot be represented|degraded/.test(evidence);
      return mismatch?'warning':'excluded';
    }
    if(['AFFINITY_BROKEN','CONVERSATION_REHYDRATING','CONNECTION_REBOUND','PROVIDER_EXHAUSTED','ROUTE_FALLBACK'].includes(type))return 'warning';
    if(['ATTEMPT_FAILED','STREAM_BROKEN','RECOVERY_STOPPED'].includes(type)||/FAILED|STOPPED/.test(type))return 'error';
    if(['AFFINITY_REUSED','ATTEMPT_SUCCEEDED','CONVERSATION_NATIVE','CONVERSATION_REHYDRATED','COMMIT','COMPLETE'].includes(type))return 'success';
    return 'normal';
  }

  async getRequest(id) {
    const body = await this.request(`/api/requests/${encodeURIComponent(id)}`);
    const request = this.mapRequest(body.client_request || {});
    const attempts = body.attempts || [];
    const events = [...(body.execution_trace_events || [])].sort((left, right) => {
      const delta = Date.parse(left.at || 0) - Date.parse(right.at || 0);
      return Number.isFinite(delta) ? delta : 0;
    });
    const phaseStarts = {
      BODY_RECEIVE_COMPLETED:'BODY_RECEIVE_STARTED', BODY_RECEIVE_FAILED:'BODY_RECEIVE_STARTED',
      BODY_PARSE_COMPLETED:'BODY_PARSE_STARTED', BODY_PARSE_FAILED:'BODY_PARSE_STARTED',
      COMPATIBILITY_COMPLETED:'COMPATIBILITY_STARTED',
      PLANNING_COMPLETED:'PLANNING_STARTED', PLANNING_FAILED:'PLANNING_STARTED', PLANNING_CANCELED:'PLANNING_STARTED',
      ADMISSION_COMPLETED:'ATTEMPT_STARTED', UPSTREAM_FIRST_EVENT:'ADMISSION_COMPLETED',
    };
    const rawTrace = events.map((event, index) => {
      const attempt = attempts.find(item => Number(item.sequence) === Number(event.attempt));
      const resource = [event.provider_id, event.upstream_model].filter(Boolean).join(' · ');
      const affinitySource=words(String(event.affinity_source||'').replace(/\./g,' ')).replace(/^Api\b/,'API');
      const affinity = event.affinity_scope ? `${words(event.affinity_scope)} affinity${affinitySource ? ` (${affinitySource})` : ''}` : '';
      const failure = [event.failure_type, event.failure_scope].filter(Boolean).join(' / ');
      let latency = event.type === 'COMPLETE' ? request.latency : attempt ? duration(attempt.latency_ms) : '—';
      if (phaseStarts[event.type]) {
        // Pending requests have no completed attempt yet. Pair real phase
        // boundaries, keeping queue/upstream waits scoped to their attempt.
        const start = events.slice(0,index).findLast(item => item.type === phaseStarts[event.type] && Number(item.attempt || 0) === Number(event.attempt || 0));
        const elapsed = start ? Date.parse(event.at) - Date.parse(start.at) : NaN;
        latency = Number.isFinite(elapsed) && elapsed >= 0 ? duration(elapsed) : '—';
      }
      return {
        time:timeLabel(event.at), title:this.traceTitle(event),
        sub:[resource, event.connection_id, event.protocol, event.route_step ? `Route ${event.route_step}` : '', affinity, failure, event.message].filter(Boolean).join(' · ') || 'Runtime event',
        latency,
        kind:this.traceKind(event),
      };
    });
    const trace = rawTrace.reduce((grouped, event) => {
      const previous = grouped[grouped.length - 1];
      if (previous && previous.baseTitle === event.title && previous.sub === event.sub) {
        previous.repeat = Number(previous.repeat || 1) + 1;
        previous.title = `${event.title} ×${previous.repeat}`;
      } else grouped.push({...event, baseTitle:event.title});
      return grouped;
    }, []);
    const completionTime = raw => {
      const started = Date.parse(raw.timestamp || '');
      const elapsed = Number(raw.duration_ms || 0);
      return Number.isFinite(started) && elapsed >= 0 ? timeLabel(new Date(started + elapsed).toISOString()) : request.time;
    };
    const fallbackTrace = [
      { time:request.time, title:'Request Received', sub:'Client request accepted', latency:'0ms', kind:'normal' },
      ...attempts.map(attempt => ({
        time:timeLabel(attempt.timestamp), title:`Provider Attempt #${attempt.sequence}`,
        sub:[attempt.provider_id, attempt.upstream_model, attempt.failure?.message || words(attempt.status)].filter(Boolean).join(' · '),
        latency:duration(attempt.latency_ms), kind:attempt.status === 'success' ? 'success' : 'error',
      })),
      { time:completionTime(body.client_request || {}), title:request.live ? 'Request In Progress' : 'Response Completed', sub:request.result, latency:request.latency, kind:request.live ? 'normal' : request.failed ? 'error' : 'success' },
    ];
    return {
      ...request, trace:trace.length ? trace : fallbackTrace, attempts:attempts.length,
      routePath:attempts.map(attempt => ({
        provider:attempt.provider_id, status:words(attempt.status),
        rateCardVersion:attempt.rate_card_version,planKind:attempt.plan_kind,estimatedCashCost:attempt.estimated_cash_cost,allocatedSubCost:attempt.allocated_sub_cost,
        failure:attempt.failure?.message || '', model:attempt.upstream_model,protocol:attempt.protocol,
        responseModel:attempt.response_model || '',serviceTier:attempt.service_tier || '',responseServiceTier:attempt.response_service_tier || '',
        latency:duration(attempt.latency_ms),ttft:Number(attempt.ttft_ms||0)>0?duration(attempt.ttft_ms):'—',
        ...usageDetails(attempt.usage),
      })),
      connectionRebinds:Number(body.resource_impact?.connection_recovery_count || 0),
      providerFallbacks:Number(body.resource_impact?.recovery_fallback_count || 0),
      conversationRebind:Number(body.continuity?.conversation_migration_count || 0) ? 'Yes' : 'No',
      clientConnection:request.recovered && !request.failed ? 'Kept Open' : '—',
      recoveryOverhead:duration(body.client_impact?.recovery_added_latency_ms),
      runtimeRevision:body.runtime_revision,
      logicalModel:request.raw?.logical_model_id || '—', upstreamModel:request.raw?.upstream_model || '—',
      provider:request.raw?.provider_id || '—', debug:body.debug || null,
      requiredCapabilities:body.required_capabilities || null,
      raw:{...(request.raw||{}),exclusions:body.candidate_exclusions||[]},
    };
  }

  async getQueue() { return this.request('/api/queue'); }
  async getSettings() { return this.request('/api/settings'); }
  async getPricingStatus() { return this.request('/api/usage-billing/pricing'); }
  async refreshPricing() { return this.request('/api/usage-billing/pricing/refresh', {method:'POST'}); }
  async updateSettings(patch) { return this.request('/api/settings', {method:'PATCH', body:JSON.stringify(patch)}); }
  async publishCodexClientVersion(version, revision, options) {
    if(!Number.isSafeInteger(revision)||revision<=0)throw new FerryError('Reload settings before publishing.',409);
    const draft=await this.request('/api/drafts',{method:'POST',body:JSON.stringify({base_revision:revision,intent:{codex_client_version:version,...(options?{codex_auto_update:options.codex_auto_update,codex_release_channel:options.codex_release_channel}:{})}})});
    await this.applyDraftMutation({draft_id:draft.id});
    return this.getSettings();
  }

  mapApiKey(item, windowMinutes = 15) {
    const identity = item.identity || {};
    const active = (item.secrets || []).find(secret => secret.state === 'ACTIVE') || (item.secrets || [])[0];
    return {
      id:identity.id, name:identity.name, preview:active?.preview || '—',
      status:identity.revoked_at ? 'Revoked' : identity.enabled ? 'Active' : 'Disabled',
      scope:(identity.scope_models||[]).length ? (identity.scope_models||[]).join(', ') : 'All models',
      usage:item.usage ? Math.round(Number(item.usage.requests || 0) / Math.max(1, windowMinutes) * 100) / 100 : '—',
      created:dateLabel(identity.created_at), createdAt:identity.created_at, description:identity.description || '', secrets:item.secrets || [], identity, usageSummary:item.usage||null,
      recentRequests:item.recent_requests || [], lastRequestAt:item.last_request_at || null,
      revealable:Boolean(active?.revealable),
    };
  }

  async getApiKeys() {
    const body = await this.request('/api/api-keys?window=15m');
    const minutes=(Date.parse(body.to)-Date.parse(body.from))/60000;
    return (body.api_keys || []).map(item => this.mapApiKey(item, Number.isFinite(minutes)&&minutes>0?minutes:15));
  }

  async getApiKey(id) { const body=await this.request(`/api/api-keys/${encodeURIComponent(id)}?window=720h`); return this.mapApiKey(body, 43200); }

  async createApiKey(input) {
    const body = await this.request('/api/api-keys', {
      method:'POST', body:JSON.stringify(input),
    });
    return { key:body.secret, item:this.mapApiKey({ identity:body.identity, secrets:[{ preview:body.secret_preview, state:'ACTIVE', revealable:true }] }) };
  }

  async revealApiKey(id) {
    const body = await this.request(`/api/api-keys/${encodeURIComponent(id)}/secret`);
    return { key:body.secret, id:body.api_key_id, name:body.name };
  }

  async revokeApiKey(id) {
    await this.request(`/api/api-keys/${encodeURIComponent(id)}/revoke`, { method:'POST', body:'{}' });
    return { id, status:'Revoked' };
  }

  async updateApiKey(id, patch) {
    await this.request(`/api/api-keys/${encodeURIComponent(id)}`, { method:'PATCH', body:JSON.stringify(patch) });
    return { id, status:patch.enabled === false ? 'Disabled' : 'Active' };
  }

  async rotateApiKey(id) {
    const body = await this.request(`/api/api-keys/${encodeURIComponent(id)}/rotate`, { method:'POST', body:'{}' });
    return { key:body.secret, item:{ id, preview:body.secret_preview } };
  }

  async deleteApiKey(id) {
    return this.request(`/api/api-keys/${encodeURIComponent(id)}`, { method:'DELETE' });
  }

  async getOrganizations() {
    const body = await this.request('/api/organizations');
    this.capabilities.organizationIsolation = Boolean(body.organization_scope_supported);
    return (body.organizations || []).map(organization => ({ ...organization, tenantIsolation:this.capabilities.organizationIsolation }));
  }

  async getRevisions() {
    const body = await this.request('/api/revisions');
    return (body.revisions || []).map(item => ({
      id:item.revision, time:new Date(item.created_at).toLocaleString('en-US'), actor:item.actor||'system', scope:(item.resources||[]).join(', ')||'Runtime',
      summary:item.summary||(item.restored_from_revision ? `Restored from revision #${item.restored_from_revision}` : 'Runtime configuration applied'), source:item.source,
      status:item.active ? 'Applied' : 'Superseded',
    }));
  }

  async getRevision(id) { return this.request(`/api/revisions/${encodeURIComponent(id)}`); }
  async restoreRevision(id) {
    const draft=await this.request(`/api/revisions/${encodeURIComponent(id)}/restore-draft`, {method:'POST',body:'{}'});
    const validation=await this.request(`/api/drafts/${encodeURIComponent(draft.id)}/validate`, {method:'POST',body:'{}'});
    if(!validation.valid) throw new FerryError('Restore draft validation failed.',422,{details:validation.errors||[]});
    return this.request(`/api/drafts/${encodeURIComponent(draft.id)}/apply`, {method:'POST',body:JSON.stringify({expected_revision:draft.base_revision})});
  }
  async getAlerts() { return this.request('/api/alerts'); }
  async acknowledgeAlert(id) { return this.request(`/api/alerts/${encodeURIComponent(id)}/acknowledge`,{method:'POST',body:'{}'}); }

  async getBilling(window = '720h') {
    const body = await this.request(`/api/usage-billing?window=${encodeURIComponent(window)}`);
    return billingView(body,window);
  }
}

export function createAdapter() {
  const params = new URLSearchParams(location.search);
  const requested = params.get('adapter');
  const defaultAdapter = document.documentElement.dataset.defaultAdapter || 'mock';
  const mode = requested || (params.get('demo') === '1' ? 'mock' : defaultAdapter);
  return mode === 'http' ? new HttpFerryAdapter(params.get('base') || '') : new MockFerryAdapter();
}
