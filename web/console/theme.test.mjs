import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
const source=fs.readFileSync(new URL('./app.part13.jsfrag',import.meta.url),'utf8').split('const nt=')[0];
function setup(saved,matches=false,blocked=false,legacy={}){
 const storage=new Map([...Object.entries(legacy),...(saved?[['ferry.theme',saved]]:[])]);let listener;
 const media={matches,addEventListener(type,fn){assert.equal(type,'change');listener=fn;}};
 const context=vm.createContext({navigator:{language:'zh-CN'},document:{documentElement:{dataset:{}}},matchMedia:()=>media,localStorage:{getItem(k){if(blocked)throw Error();return storage.get(k);},setItem(k,v){if(blocked)throw Error();storage.set(k,v);}},render(){}});
 vm.runInContext(source+'applyConsolePreferences();',context);
 return {context,storage,theme:()=>context.document.documentElement.dataset.theme,change(value){media.matches=value;listener();},cycle(){context.cycleConsoleTheme();}};
}
let x=setup();assert.equal(x.theme(),'light');x.change(true);assert.equal(x.theme(),'dark');assert.equal(x.storage.get('ferry.theme'),'system');
x.cycle();assert.equal(x.theme(),'light');x.change(false);x.change(true);assert.equal(x.theme(),'light');x.cycle();assert.equal(x.theme(),'dark');x.cycle();assert.equal(x.storage.get('ferry.theme'),'system');x.change(false);assert.equal(x.theme(),'light');
for(const mode of ['light','dark']){x=setup(mode);x.change(true);assert.equal(x.theme(),mode);assert.equal(x.storage.get('ferry.theme'),mode);}
x=setup('garbage',true);assert.equal(x.storage.get('ferry.theme'),'system');assert.equal(x.theme(),'dark');
x=setup(undefined,false,true);x.change(true);assert.equal(x.theme(),'dark');assert.match(x.context.consoleThemeControl(),/跟随系统/);
console.log('PASS system theme changes, manual overrides, three-mode cycle, persisted defaults and unavailable storage');

x=setup(undefined,false,false,{'smartapi.theme':'dark','smartapi.locale':'en-US'});
assert.equal(x.theme(),'dark');assert.equal(x.storage.get('ferry.theme'),'dark');assert.equal(x.storage.get('smartapi.theme'),'dark');assert.equal(x.context.document.documentElement.lang,'en-US');assert.equal(x.context.document.title,'Ferry Console');
x=setup('light',false,false,{'smartapi.theme':'dark','smartapi.locale':'invalid'});assert.equal(x.theme(),'light');assert.equal(x.context.document.documentElement.lang,'zh-CN');assert.equal(x.context.document.title,'Ferry 控制台');
x=setup(undefined,true,false,{'smartapi.theme':'invalid'});assert.equal(x.theme(),'dark');assert.equal(x.storage.get('ferry.theme'),'system');
console.log('PASS legacy preference migration, validation and Ferry titles');
