import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import test from 'node:test';

const source = await fs.readFile(new URL('./app.part9.jsfrag', import.meta.url), 'utf8');
const start = source.indexOf('const providerProtocolMeta=');
const end = source.indexOf('const providerCapabilityMeta=');
assert.ok(start >= 0 && end > start);
const { presets, apply, conversion, settings, options } = new Function('esc', `${source.slice(start, end)};return {presets:providerChannelPresets,apply:applyProviderChannelPreset,conversion:providerChannelConversionProtocols,settings:providerChannelPresetSettings,options:providerChannelPresetOptions}`)(value => String(value).replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('"', '&quot;'));
const endpoints = {
  groq:'https://api.groq.com/openai/v1', together:'https://api.together.ai/v1',
  fireworks:'https://api.fireworks.ai/inference/v1', cerebras:'https://api.cerebras.ai/v1',
  minimax:'https://api.minimax.io/v1', 'minimax-cn':'https://api.minimax.cn/v1',
  baidu:'https://qianfan.baidubce.com/v2', tencent:'https://api.hunyuan.cloud.tencent.com/v1',
  nvidia:'https://integrate.api.nvidia.com/v1', huggingface:'https://router.huggingface.co/v1', cliproxyapi:'',
};
function populatedDraft() {
  return {
    type:'custom', channelPreset:'openai', name:'OpenAI', baseUrl:'https://old.invalid/v1',
    secret:'test-only-secret', credentialRef:'store:old', catalog:['old-model'], probeError:'old-error',
    modelSyncCatalogMode:'volcengine-plan', modelSyncCatalog:{type:'volcengine-plan',plan:'agent',credentialRef:'store:old-catalog'}, modelSyncCatalogReplace:true, modelSyncCatalogCredential:{accessKeyId:'old-ak',secretAccessKey:'old-sk',sessionToken:'old-token'}, modelSync:{status:'running'}, modelSyncRunning:true,
    probeGeneration:7, probing:true, probeCompleted:2, probeTotal:4,
    scheduledMessage:{enabled:true, upstreamModel:'old-model', message:'keep user text'},
    protocols:['openai-chat','openai-responses','anthropic','openai-images'].map(protocol => ({
      protocol, connectionId:'old', connectionIdentity:'old', connectionEnabled:false, connectionConcurrency:3,
      baseUrl:'https://old.invalid/v1', overrideUrl:true, urlMode:'full', fullUrl:'https://old.invalid/private', secret:'test-only-override', credentialRef:'store:old',
      overrideKey:true, models:['old-model'], manualModels:['manual'], modelSpecs:{old:{}}, autoConvert:true,
      status:'verified', error:'old-error', probeStartedAt:42, probeElapsedMs:10, modelInput:'old',
    })),
  };
}
test('preset inventory contains configuration only, never credentials or model capabilities', () => {
  assert.equal(new Set(presets.map(p => p.id)).size, presets.length);
  for (const preset of presets) {
    assert.deepEqual(Object.keys(preset).filter(key=>!['protocolBaseUrls','modelSyncCatalogPlan'].includes(key)).sort(), ['id','group','label','name','baseUrl','protocol','conversion','kind','description'].sort());
    assert.deepEqual(conversion(preset), ['openai-chat','openai-responses','anthropic']);
    if (preset.baseUrl) {
      const url = new URL(preset.baseUrl);
      assert.equal(url.protocol, 'https:');
      assert.equal(url.username + url.password + url.search + url.hash, '');
      assert.doesNotMatch(url.pathname, /\/(chat\/completions|responses|messages)$/);
    }
  }
});
for (const [id, baseUrl] of Object.entries(endpoints)) {
  test(`${id}: apply editable endpoint and discard previous credentials and supply`, () => {
    const preset = presets.find(p => p.id === id);
    assert.ok(preset);
    assert.equal(preset.baseUrl, baseUrl);
    const w = populatedDraft();
    apply(w, id);
    assert.equal(w.type, 'custom');
    assert.equal(w.channelPreset, id);
    assert.equal(w.name, preset.name);
    assert.equal(w.baseUrl, baseUrl);
    assert.equal(w.activeProtocol, 'openai-chat');
    assert.equal(w.secret + w.credentialRef + w.probeError, '');
    assert.deepEqual(w.catalog, []);
    assert.equal(w.probeGeneration, 8, 'in-flight old probe results must be invalidated');
    assert.equal(w.probing, false);
    assert.equal(w.probeCompleted + w.probeTotal, 0);
    assert.deepEqual(w.scheduledMessage, {enabled:false, upstreamModel:'', message:'keep user text'});
    for (const p of w.protocols) {
      assert.equal(p.urlMode, 'base');
      assert.equal(p.fullUrl, '');
      assert.equal(p.secret + p.credentialRef + p.baseUrl + p.connectionId + p.connectionIdentity + p.error + p.modelInput, '');
      assert.equal(p.overrideKey || p.overrideUrl, false);
      assert.equal(p.connectionConcurrency, null);
      assert.equal(p.connectionEnabled, true);
      assert.deepEqual(p.models, []);
      assert.deepEqual(p.manualModels, []);
      assert.deepEqual(p.modelSpecs, {});
      assert.equal(p.status, 'idle');
      assert.equal(p.probeStartedAt, 0);
      assert.equal(p.probeElapsedMs, null);
      assert.equal(p.autoConvert, p.protocol !== 'openai-images');
    }
    assert.match(settings(w), /Detect native support/);
    assert.match(options(id), new RegExp(`value="${id}" selected`));
  });
}
test('reselecting a channel and switching to Custom preserve manual configuration', () => {
  const w = populatedDraft();
  const before = structuredClone(w);
  apply(w, 'openai');
  assert.deepEqual(w, before);
  apply(w, 'custom');
  assert.deepEqual(w, {...before, channelPreset:'custom'});
  w.name = 'My custom name';
  apply(w, 'groq');
  assert.equal(w.name, 'My custom name');
});
test('bridge requires an operator address and distinguishes external OAuth and keys', () => {
  const bridge = presets.find(p => p.id === 'cliproxyapi');
  assert.equal(bridge.baseUrl, '');
  assert.match(bridge.description, /client API key, not its management key/);
  assert.match(bridge.description, /not native Ferry OAuth/);
  assert.match(settings({channelPreset:bridge.id}), /Enter your own compatible Base URL/);
});

const subscriptionEndpoints = {
  "glm-coding": {
    "openai-chat": "https://open.bigmodel.cn/api/coding/paas/v4",
    "anthropic": "https://open.bigmodel.cn/api/anthropic/v1",
    "openai-responses": "https://open.bigmodel.cn/api/v1"
  },
  "zai-coding": {
    "openai-chat": "https://api.z.ai/api/coding/paas/v4",
    "anthropic": "https://api.z.ai/api/anthropic/v1",
    "openai-responses": "https://api.z.ai/api/v1"
  },
  "alibaba-coding": {
    "openai-chat": "https://coding.dashscope.aliyuncs.com/v1",
    "anthropic": "https://coding.dashscope.aliyuncs.com/apps/anthropic/v1"
  },
  "alibaba-token": {
    "openai-chat": "https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1",
    "anthropic": "https://token-plan.cn-beijing.maas.aliyuncs.com/apps/anthropic/v1",
    "openai-responses": "https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1"
  },
  "volcengine-coding": {
    "openai-chat": "https://ark.cn-beijing.volces.com/api/coding/v3",
    "anthropic": "https://ark.cn-beijing.volces.com/api/coding/v1",
    "openai-responses": "https://ark.cn-beijing.volces.com/api/coding/v3"
  },
  "volcengine-agent": {
    "openai-chat": "https://ark.cn-beijing.volces.com/api/plan/v3",
    "anthropic": "https://ark.cn-beijing.volces.com/api/plan/v1",
    "openai-responses": "https://ark.cn-beijing.volces.com/api/plan/v3"
  },
  "kimi-coding": {
    "openai-chat": "https://api.kimi.com/coding/v1",
    "anthropic": "https://api.kimi.com/coding/v1",
    "openai-responses": "https://api.kimi.com/coding/v1"
  },
  "kimi-coding-global": {
    "openai-chat": "https://api.kimi.ai/coding/v1",
    "anthropic": "https://api.kimi.ai/coding/v1",
    "openai-responses": "https://api.kimi.ai/coding/v1"
  },
  "minimax-cn-coding": {
    "openai-chat": "https://api.minimax.cn/v1",
    "anthropic": "https://api.minimax.cn/anthropic/v1",
    "openai-responses": "https://api.minimax.cn/v1"
  },
  "minimax-coding": {
    "openai-chat": "https://api.minimax.io/v1",
    "anthropic": "https://api.minimax.io/anthropic/v1",
    "openai-responses": "https://api.minimax.io/v1"
  },
  "baidu-coding": {
    "openai-chat": "https://qianfan.baidubce.com/v2/coding",
    "anthropic": "https://qianfan.baidubce.com/anthropic/coding/v1"
  },
  "baidu-token": {
    "openai-chat": "https://qianfan.baidubce.com/v2/tokenplan/personal",
    "anthropic": "https://qianfan.baidubce.com/anthropic/tokenplan/personal/v1",
    "openai-responses": "https://qianfan.baidubce.com/v2/tokenplan/personal"
  }
};

for (const [id, urls] of Object.entries(subscriptionEndpoints)) {
  test(`${id}: independent subscription protocol URLs and clean catalog credentials`, () => {
    const w = populatedDraft();
    apply(w, id);
    assert.deepEqual(presets.find(p => p.id === id).protocolBaseUrls, urls);
    assert.equal(w.baseUrl, urls['openai-chat']);
    assert.equal(w.secret + w.credentialRef, '');
    assert.equal(w.modelSync, null);
    assert.equal(w.modelSyncRunning, false);
    assert.deepEqual(w.modelSyncCatalogCredential, {accessKeyId:'',secretAccessKey:'',sessionToken:''});
    assert.equal(w.modelSyncCatalogReplace, false);
    const plan = id === 'volcengine-coding' ? 'coding' : id === 'volcengine-agent' ? 'agent' : null;
    assert.equal(w.modelSyncCatalogMode, 'provider', 'AK/SK catalog is opt-in, including Ark plans');
    assert.deepEqual(w.modelSyncCatalog, plan ? {type:'volcengine-plan',plan,credentialRef:''} : null);
    for (const p of w.protocols) {
      assert.equal(p.baseUrl, urls[p.protocol] || '');
      assert.equal(p.overrideUrl, Boolean(urls[p.protocol]));
      assert.equal(p.fullUrl + p.secret + p.credentialRef, '');
      assert.equal(p.urlMode, 'base');
      assert.deepEqual(p.models, []);
      assert.deepEqual(p.modelResults, []);
      assert.deepEqual(p.modelSpecs, {});
    }
    assert.match(urls.anthropic + '/messages', /\/v1\/messages$/);
    assert.doesNotMatch(urls.anthropic, /\/v1\/v1/);
    apply(w, 'openai');
    assert.equal(w.modelSyncCatalogMode, 'provider');
    assert.equal(w.modelSyncCatalog, null);
    assert.deepEqual(w.modelSyncCatalogCredential, {accessKeyId:'',secretAccessKey:'',sessionToken:''});
  });
}
test('subscription roots stay separate from PAYG, except documented MiniMax shared roots', () => {
  for (const [subscription, payg] of [['glm-coding','zhipu'],['alibaba-coding','alibaba'],['alibaba-token','alibaba'],['volcengine-coding','volcengine'],['volcengine-agent','volcengine'],['kimi-coding','moonshot'],['baidu-token','baidu']]) {
    assert.notEqual(presets.find(p=>p.id===subscription).baseUrl, presets.find(p=>p.id===payg).baseUrl);
  }
  for (const [subscription,payg] of [['minimax-coding','minimax'],['minimax-cn-coding','minimax-cn']]) {
    assert.equal(presets.find(p=>p.id===subscription).baseUrl, presets.find(p=>p.id===payg).baseUrl);
    const w=populatedDraft();w.channelPreset=payg;apply(w,subscription);
    assert.equal(w.secret+w.credentialRef,'');
  }
});
