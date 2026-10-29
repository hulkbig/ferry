import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import vm from 'node:vm';
import { MockFerryAdapter, HttpFerryAdapter } from './api.js';

const source=await fs.readFile(new URL('./app.part6.jsfrag',import.meta.url),'utf8');
const state={settings:{}};
const patches=[],toasts=[];
const context=vm.createContext({state,Number,esc:String,render(){},showToast(...args){toasts.push(args)},adapter:{async updateSettings(patch){patches.push(structuredClone(patch));return structuredClone(patch)}}});
vm.runInContext(source.slice(source.indexOf('function settingsAutomaticUsageReset()')),context);
assert.match(context.settingsAutomaticUsageReset(),/value="20"/);
assert.match(context.settingsAutomaticUsageReset(),/data-action="auto-reset-enabled" checked/);
await context.updateAutomaticUsageResetSetting({dataset:{action:'auto-reset-threshold'},value:'7'});
assert.deepEqual(patches.at(-1),{codex_usage_reset_auto:{enabled:true,threshold_minutes:7}});
await context.updateAutomaticUsageResetSetting({dataset:{action:'auto-reset-enabled'},checked:false});
assert.deepEqual(patches.at(-1),{codex_usage_reset_auto:{enabled:false,threshold_minutes:7}});
assert.doesNotMatch(context.settingsAutomaticUsageReset(),/data-action="auto-reset-enabled" checked/);
for(const value of ['0','-1','1441','1.5','NaN','']){
 await context.updateAutomaticUsageResetSetting({dataset:{action:'auto-reset-threshold'},value});
}
assert.equal(patches.length,2,'invalid values must never be submitted');
const mock=new MockFerryAdapter();
assert.deepEqual((await mock.getSettings()).codex_usage_reset_auto,{enabled:true,threshold_minutes:20});
await mock.updateSettings(patches[1]);
assert.deepEqual((await mock.getSettings()).codex_usage_reset_auto,patches[1].codex_usage_reset_auto);
const http=new HttpFerryAdapter();
http.request=async (path,options)=>{assert.equal(path,'/api/settings');assert.equal(options.method,'PATCH');assert.deepEqual(JSON.parse(options.body),patches[1]);return patches[1]};
await http.updateSettings(patches[1]);
console.log('Automatic usage-reset settings: PASS');
