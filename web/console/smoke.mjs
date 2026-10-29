import assert from 'node:assert/strict';
import { MockFerryAdapter } from './api.js';

const api = new MockFerryAdapter();

const models = await api.getModels();
assert(models.length >= 5, 'models should load');
const model = await api.getModel('gpt-5.6-sol');
assert.equal(model.routes[0].id, 'codex-personal');
const modelRequests = await api.getRequests({ model:'gpt-5.6-sol', outcome:'', limit:25, offset:0 });
assert.ok(modelRequests.length > 0 && modelRequests.every(request => request.model === 'gpt-5.6-sol'), 'request filters must constrain demo data by logical model');
assert.equal(modelRequests.total, modelRequests.length);
const conversationRequests = await api.getRequests({ conversation_id:'conv_01H8K0F2', limit:25, offset:0 });
assert.ok(conversationRequests.length > 1 && conversationRequests.every(request => request.conversation === 'conv_01H8K0F2'), 'conversation trails must use exact conversation matching');
await api.updateModel('gpt-5.6-sol', { enabled:false });
const disabledPreview = await api.simulateModel('gpt-5.6-sol');
assert.equal(disabledPreview.previewing_disabled_model, true, 'disabled models should remain simulatable');
await api.updateModel('gpt-5.6-sol', { enabled:true });

const review = await api.reviewRouting('gpt-5.6-sol', {
  routes: [model.routes[1], model.routes[0], model.routes[2]],
  conversationStrategy: 'migrate',
});
assert.equal(review.after[0].id, 'openrouter-main');

const applied = await api.applyRouting('gpt-5.6-sol', review);
assert.ok(applied.revision, 'apply should create revision');

const provider = await api.getProvider('codex-personal');
const before = provider.connections.length;
const addedConnection = await api.addConnection('codex-personal', { account: 'codex-smoke', region: 'US East', authType: 'OAuth' });
const after = (await api.getProvider('codex-personal')).connections.length;
assert.equal(after, before + 1, 'connection should be added');
await api.updateConnection('codex-personal', addedConnection.id, { status:'Unavailable' });
await api.deleteConnection('codex-personal', addedConnection.id);
assert.equal((await api.getProvider('codex-personal')).connections.length, before, 'disabled connection should be directly deletable');

const disposableProvider = await api.addProvider({ name:'Disposable Provider', type:'custom' });
await api.updateProvider(disposableProvider.id, { enabled:false });
await api.deleteProvider(disposableProvider.id);
assert.equal((await api.getProviders()).some(provider => provider.id === disposableProvider.id), false, 'disabled unreferenced provider should be directly deletable');

const created = await api.createApiKey({ name: 'smoke-key', scope: 'All models' });
assert.match(created.key, /^sk-ferry-/);
await api.revokeApiKey(created.item.id);
const keys = await api.getApiKeys();
assert.equal(keys.find(k => k.id === created.item.id)?.status, 'Revoked');

console.log('Ferry Console v3 smoke: PASS');
