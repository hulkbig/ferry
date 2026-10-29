import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import vm from 'node:vm';
import { HttpFerryAdapter, MockFerryAdapter } from './api.js';

const read = part => fs.readFile(new URL(`./app.part${part}.jsfrag`, import.meta.url), 'utf8');
const [formSource, clickSource, inputSource] = await Promise.all([read(9), read(10), read(11)]);
const state = { wizard:{}, modelCache:new Map(), overviewWindow:'15m' };
const adapter = new MockFerryAdapter();
const actions = {}, inputs = {}, toasts = [];
let locale = 'en-US';
const context = vm.createContext({
  state, adapter, location:{hash:''},
  app:{addEventListener:(name, handler) => { actions[name] = handler; }},
  portal:{addEventListener:(name, handler) => { inputs[name] = handler; }},
  ensureModel:id => adapter.getModel(id),
  renderPortal(){}, render(){}, closeOverlay(){},
  showToast:(message, tone) => toasts.push({message, tone}),
  resourceHash:(section, id) => `#/${section}/${encodeURIComponent(id)}`,
  nt:(en, zh) => locale === 'zh-CN' ? zh : en,
  esc:value => String(value??'').replaceAll('&', '&amp;').replaceAll('"', '&quot;').replaceAll('<', '&lt;'),
});
function loadListener(source, marker) {
  const start = source.indexOf(marker), end = source.indexOf('\n});', start);
  assert.ok(start >= 0 && end > start, 'the actual event handler must be executable');
  vm.runInContext(source.slice(start, end + 4), context);
}
loadListener(clickSource, "app.addEventListener('click'");
loadListener(inputSource, "portal.addEventListener('input'");
const formStart = formSource.indexOf('function modelFormModal(');
const formEnd = formSource.indexOf('\nconst overrideProtocolOptions', formStart);
vm.runInContext(formSource.slice(formStart, formEnd), context);
const click = dataset => actions.click({target:{closest:() => ({dataset})}});
const input = (wizard, value) => inputs.input({target:{dataset:{wizard}, value}});

// Existing configuration fills the real form and survives edits to other data.
await adapter.createModel({id:'timeout-test', display_name:'Test', enabled:false,
  description:'Keep description', exact_only:true, first_output_timeout_seconds:180, default_route:{steps:[]}});
await click({action:'edit-model', id:'timeout-test'});
assert.equal(state.wizard.model.firstOutputTimeoutSeconds, 180);
for (const language of ['en-US', 'zh-CN']) {
  locale = language;
  const html = context.modelFormModal({mode:'edit'});
  assert.match(html, /type="number" min="0" max="7200" step="1"/);
  assert.match(html, /data-wizard="model-first-output-timeout" value="180"/);
  assert.ok(html.includes(language === 'en-US' ? 'First output timeout (seconds)' : '首输出等待上限（秒）'));
  assert.ok(html.includes(language === 'en-US' ? '0 disables this limit for this model' : '0 关闭此模型的限制'));
  assert.ok(html.includes(language === 'en-US' ? 'text, reasoning, or tool output' : '文本、推理或工具输出'));
}
for (const value of ['', ' ', '-1', '7201', '1.5', 'NaN', 'Infinity']) {
  input('model-first-output-timeout', value);
  await click({action:'model-save', mode:'edit'});
  assert.equal(toasts.at(-1).tone, 'error');
  assert.match(toasts.at(-1).message, /0 至 7200 秒的整数（0 关闭）/);
  assert.equal((await adapter.getModel('timeout-test')).model.raw.first_output_timeout_seconds, 180);
}
for (const value of ['0', '1', '7200']) {
  input('model-first-output-timeout', value);
  await click({action:'model-save', mode:'edit'});
  const saved = (await adapter.getModel('timeout-test')).model.raw;
  assert.equal(saved.first_output_timeout_seconds, Number(value));
  assert.equal(saved.exact_only, true, 'timeout edits must preserve compatibility policy');
  assert.equal(saved.description, 'Keep description');
  await click({action:'edit-model', id:'timeout-test'});
  assert.equal(state.wizard.model.firstOutputTimeoutSeconds, Number(value));
}
await click({action:'add-model'});
assert.equal(state.wizard.model.firstOutputTimeoutSeconds, 0, 'new models start with the timeout disabled');
state.wizard.model.enabled = true;
input('model-id', 'new-timeout-test');
await click({action:'model-save', mode:'create'});
assert.equal((await adapter.getModel('new-timeout-test')).model.raw.first_output_timeout_seconds, 0);
assert.equal((await adapter.getModel('new-timeout-test')).model.raw.enabled, false, 'route-less model creation must remain disabled');
await click({action:'edit-model', id:'gpt-5.6-sol'});
assert.equal(state.wizard.model.firstOutputTimeoutSeconds, 0, 'legacy missing field means disabled');

// The HTTP adapter carries the numeric field, including explicit zero, through
// its existing draft write/apply flow; omitted patches must remain omitted.
const requests = [], applied = [];
const http = new HttpFerryAdapter();
http.request = async (path, options) => {
  requests.push({path, method:options.method, body:JSON.parse(options.body)});
  return {draft_id:'test-draft'};
};
http.applyDraftMutation = async mutation => { applied.push(mutation.draft_id); };
http.getModel = async () => ({});
for (const seconds of [180, 0]) {
  await http.createModel({id:'model/with-slash', first_output_timeout_seconds:seconds});
  assert.equal(requests.at(-1).body.first_output_timeout_seconds, seconds);
  await http.updateModel('model/with-slash', {first_output_timeout_seconds:seconds});
  assert.equal(requests.at(-1).body.first_output_timeout_seconds, seconds);
  assert.equal(requests.at(-1).path, '/api/models/model%2Fwith-slash');
}
await http.updateModel('model/with-slash', {display_name:'Rename'});
assert.ok(!Object.hasOwn(requests.at(-1).body, 'first_output_timeout_seconds'));
assert.equal(applied.length, 5);
const mapped = http.mapModel({id:'model/with-slash', first_output_timeout_seconds:0}, 15, {});
assert.equal(mapped.raw.first_output_timeout_seconds, 0);

console.log('Ferry first output timeout model UI and adapter: PASS');
