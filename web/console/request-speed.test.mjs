import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import vm from 'node:vm';
import {HttpFerryAdapter} from './api.js';

const adapter=new HttpFerryAdapter();
const base={id:'speed',status:'success',duration_ms:2500,ttft_ms:500,usage:{input_tokens:1000,output_tokens:125,reasoning_tokens:25}};
// Request throughput includes waiting/recovery; reasoning tokens are not added twice.
for(const stream of [true,false])assert.equal(adapter.mapRequest({...base,stream}).tokensPerSecond,50);
assert.equal(adapter.mapRequest({...base,usage:{input_tokens:100,output_tokens:0}}).tokensPerSecond,0);
for(const patch of [
  {status:'streaming'},{status:'waiting'},{usage:null},{usage:{input_tokens:100}},
  {usage:{input_tokens:100,output_tokens:null}},{usage:{input_tokens:100,output_tokens:-1}},
  {usage:{input_tokens:100,output_tokens:Infinity}},
  ...[0,-1,null,undefined,NaN,Infinity,'2500'].map(duration_ms=>({duration_ms})),
])assert.equal(adapter.mapRequest({...base,...patch}).tokensPerSecond,null);

const source=await fs.readFile(new URL('./app.part11.jsfrag',import.meta.url),'utf8');
const context=vm.createContext({Number,Math,esc:String,nt:en=>en,requestPricingLabel:()=>'',requestMoney:String,
  state:{requestCache:new Map()},queueMicrotask(){},badge:String,requestCostDetails:()=>'',fmtCompact:String});
for(const name of ['capabilityNames','requestTimingTone','requestAccountingCells','requestKeySubtitle','pageRequestDetail']){
  const start=source.indexOf('function '+name+'('),end=source.indexOf('\nfunction ',start+1);
  vm.runInContext(source.slice(start,end<0?undefined:end),context);
}
for(const [request,label] of [[base,'50.0'],[{...base,status:'streaming'},'—'],[{...base,usage:null},'—']]){
  const row=adapter.mapRequest(request);
  context.requestDataSync=()=>row;
  assert.ok(context.requestAccountingCells(row).includes(`${label} tokens/s`));
  assert.ok(context.pageRequestDetail('speed').includes(`<strong>${label}</strong><small>tokens/s</small>`));
}
console.log('Request tokens/s mapping, list and details: PASS');
