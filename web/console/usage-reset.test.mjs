import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import vm from 'node:vm';
import { HttpFerryAdapter, MockFerryAdapter } from './api.js';

// Closed network boundary: adapters are stubs and fetch always rejects.
const previousFetch=globalThis.fetch;
globalThis.fetch=()=>{throw new Error('Network disabled in usage-reset checks')};
try {
 const part4=await fs.readFile(new URL('./app.part4.jsfrag',import.meta.url),'utf8');
 const source=await fs.readFile(new URL('./app.part12.jsfrag',import.meta.url),'utf8');
 const percent=part4.slice(part4.indexOf('function codexUsagePercent('),part4.indexOf('function providerCodexUsagePills('));
 const code=source.slice(source.indexOf('function codexUsageWindowLabel('),source.indexOf('function codexAccountNeedsRelogin('));
 const r={id:'request/a',provider_id:'provider/a',connection_id:'connection/a',account:'local-a',account_id:'upstream-a',credit:{id:'credit/a',reset_type:'codex_rate_limits',status:'available',granted_at:'2026-09-01T00:00:00Z',expires_at:'2099-10-01T00:00:00Z'},created_by:'agent',created_at:new Date().toISOString(),expires_at:new Date(Date.now()+600000).toISOString(),status:'pending'};
 const state={auth:{authenticated:true},usageResetRequests:[],usageResetDismissed:new Set(),usageResetEpoch:0};
 let confirms=0,cancels=0,prepares=0,journal=[r],release,received,refreshes=0;
 const adapter={
  getUsageResetRequests:async()=>structuredClone(journal),
  prepareUsageReset:async()=>{prepares++;return structuredClone(r)},
  cancelUsageReset:async request=>{cancels++;return {...request,status:'canceled'}},
  confirmUsageReset:request=>{confirms++;received=request;return new Promise(resolve=>{release=resolve})},
 };
 const context=vm.createContext({state,adapter,structuredClone,Date,Set,URLSearchParams,location:{hash:'#/providers/provider%2Fa'},document:{hidden:false},render(){},renderPortal(){},showToast(){},invalidateProviderCodexUsage(){},loadSharedCodexUsage(){refreshes++},esc:value=>String(value).replaceAll('&','&amp;').replaceAll('<','&lt;').replaceAll('>','&gt;').replaceAll('"','&quot;')});
 vm.runInContext(percent+code,context);
 const usage={account_id:'upstream-a',rate_limit:{primary_window:{limit_window_seconds:604800,used_percent:95}},rate_limit_reset_credits:{available_count:3,total_earned_count:4,credits:[r.credit,{...r.credit,id:'credit/b',expires_at:null}]}};
 let html=context.codexUsageCreditsView(usage,{providerId:r.provider_id,connectionId:r.connection_id});
 assert.doesNotMatch(html,/<details|<table/);assert.match(html,/data-credit="credit\/a"/);assert.match(html,/data-credit="credit\/b"/);assert.match(html,/2 of 3/);assert.match(html,/Expiration not reported/);assert.match(html,/data-account="upstream-a"/);
 const visibleText=html.replace(/<[^>]*>/g,'');
 assert.doesNotMatch(visibleText,/credit\/a|credit\/b|codex_rate_limits|Granted|Earned:|You can request a reset/);
 assert.doesNotMatch(html,/data-action="reset-cooldown"|set-quota|reset-conversation/);
 // Usage is informational; even 0% or 50% must allow an explicit request.
 const target={providerId:r.provider_id,connectionId:r.connection_id};
 for(const used_percent of [0,50,89,100]){
  const displayed=context.codexUsageCreditsView({...usage,rate_limit:{primary_window:{used_percent}}},target);
  const buttons=[...displayed.matchAll(/<button[^>]*data-action="codex-usage-reset-prepare"[^>]*>/g)];
  assert.equal(buttons.length,2);
  for(const [button] of buttons)assert.doesNotMatch(button,/\bdisabled\b/,'usage percent cannot disable a valid credit');
 }
 assert.equal(prepares,0);assert.equal(confirms,0,'rendering never submits a reset');
 state.usageResetHistoryLoaded=true;
 // History separates request provenance, result, and upstream card lifecycle.
 const historyUsage={...usage,rate_limit_reset_credits:{credits:[{...r.credit,status:'redeemed',redeemed_at:'2026-10-23T04:00:00Z'},{...r.credit,id:'expired',expires_at:'2020-01-01T00:00:00Z'}]}};
 state.usageResetRequests=[{...r,status:'nothing_to_reset',windows_reset:0,created_by:'system:expiring-codex-credit'}, {...r,id:'manual',credit:{...r.credit,id:'manual-card'},status:'reset',windows_reset:1}, {...r,id:'foreign',account_id:'another-account',status:'reset'}];
 let history=context.codexResetHistory(historyUsage,target);
 assert.equal(history.length,4);assert.ok(history.some(x=>x.source==='Automatic trigger'&&x.result==='No eligible window to reset'&&x.windows===0));
 assert.ok(history.some(x=>x.source==='Manual request'&&x.windows===1));
 assert.ok(history.some(x=>x.source==='Other channel / source unconfirmed'));assert.ok(history.some(x=>x.source==='Credit expiration'));
 assert.ok(!history.some(x=>x.id==='foreign'),'a reused connection must not mix upstream accounts');
 state.usageResetRequests=[{...r,status:'reset'}];
 assert.equal(context.codexResetHistory(historyUsage,target).filter(x=>x.result==='Credit already redeemed').length,0,'local success explains upstream redemption');
 state.usageResetRequests=[{...r,status:'uncertain'}];
 assert.ok(context.codexResetHistory(historyUsage,target).some(x=>x.source==='Trigger unknown'),'uncertain local consumption is not attributed to another client');
 state.usageResetRequests=[{...r,expires_at:'2020-01-01T00:00:00Z'}];
 assert.ok(context.codexResetHistory(usage,target).some(x=>x.result==='Confirmation expired'));
 assert.match(context.codexUsageCreditsView({account_id:r.account_id},target),/Confirmation expired/,'journal survives missing upstream details');
 assert.match(context.codexResetHistoryView(null,target),/Confirmation expired/,'journal is visible before usage loads');
 state.usageResetHistoryError=true;assert.match(context.codexResetHistoryView(usage,target),/history unavailable/);state.usageResetHistoryError=false;
 state.usageResetRequests=[{...r,connection_id:'<script>bad</script>',status:'reset'}];
 assert.doesNotMatch(context.codexResetHistoryView(usage,target),/<script>/);
 const combined=context.codexUsageCreditsView(usage,target);
 assert.equal((combined.match(/<ul class="codex-reset-list">/g)||[]).length,1,'available credits and history share one list');
 assert.ok(combined.indexOf('codex-usage-reset-prepare')<combined.indexOf('codex-reset-record'),'history follows available credits');
 assert.match(combined,/codex-reset-record used/);
 assert.match(combined,/Manual request/);
 assert.doesNotMatch(combined,/<h4>Reset history|Windows reset|data-i18n-skip/,'history hides implementation IDs');
 state.usageResetRequests=[];

 const now=Date.parse('2026-09-21T00:00:00Z'),day=86400000;
 for(const [value,tone] of [[null,'unknown'],['invalid','unknown'],[now-1,'expired'],[now,'expired'],[now+1,'danger'],[now+day,'danger'],[now+day+1,'warning'],[now+7*day,'warning'],[now+7*day+1,'healthy']]){
  assert.equal(context.codexResetExpiryTone(typeof value==='number'?new Date(value).toISOString():value,now),tone);
 }
 for(const [changedUsage,credit,binding,requests,reason] of [
  [{...usage,account_id:''},r.credit,target,[],'Account identity unavailable'],
  [usage,r.credit,{providerId:r.provider_id},[],'Account identity unavailable'],
  [usage,r.credit,target,[r],'Review the pending request first'],
  [usage,{...r.credit,id:''},target,[],'Selected credit unavailable'],
  [usage,{...r.credit,status:'redeemed'},target,[],'Selected credit unavailable'],
  [{...usage,rate_limit_reset_credits:{available_count:0}},r.credit,target,[],'Selected credit unavailable'],
  [{...usage,rate_limit_reset_credits:{}},r.credit,target,[],'Selected credit unavailable'],
  [usage,{...r.credit,reset_type:'other_reset'},target,[],'Unsupported reset scope'],
  [usage,{...r.credit,expires_at:'invalid'},target,[],'Invalid expiration'],
  [usage,{...r.credit,expires_at:'2000-01-01T00:00:00Z'},target,[],'Expired'],
 ])assert.equal(context.codexResetDisabledReason(changedUsage,credit,binding,requests),reason);
 state.usageResetRequests=[r];
 const blocked=context.codexUsageCreditsView(usage,target);
 assert.match(blocked,/Review the pending request first/);
 assert.match(blocked,/data-action="codex-usage-reset-prepare"[^>]*disabled/);
 // A different upstream account remains independent of this pending request.
 const independent=context.codexUsageCreditsView({...usage,account_id:'upstream-b'},{providerId:'provider/b',connectionId:'connection/b'});
 assert.doesNotMatch(independent,/data-action="codex-usage-reset-prepare"[^>]*disabled/);
 state.usageResetRequests=[];
 const bad=context.codexUsageCreditsView({...usage,rate_limit_reset_credits:{available_count:3,credits:null}},null);assert.match(bad,/details unavailable/);assert.doesNotMatch(bad,/Use reset/);
 const injected=context.codexUsageCreditsView({...usage,rate_limit_reset_credits:{available_count:1,credits:[{...r.credit,id:'"><script>bad</script>'}]}},{providerId:'<bad>',connectionId:'b'});assert.doesNotMatch(injected,/<script>/);
 await context.pollCodexUsageResets();assert.equal(confirms,0);assert.equal(state.modal.request.id,r.id,'external preparation opens confirmation only');
 assert.match(context.codexUsageResetModal(state.modal),/class="modal-backdrop"/);
 assert.match(context.codexUsageResetModal(state.modal),/consume 1 credit/);
 await context.handleCodexUsageResetAction('codex-usage-reset-cancel',{dataset:{request:r.id}});assert.equal(cancels,1);assert.equal(confirms,0);
 context.dismissCodexUsageReset();
 await context.handleCodexUsageResetAction('codex-usage-reset-prepare',{dataset:{provider:r.provider_id,connection:r.connection_id,account:r.account_id,credit:r.credit.id}});
 assert.equal(prepares,1);assert.equal(confirms,0);
 const promise=context.handleCodexUsageResetAction('codex-usage-reset-confirm',{dataset:{request:r.id}});
 await context.handleCodexUsageResetAction('codex-usage-reset-confirm',{dataset:{request:r.id}});
 assert.equal(confirms,1,'double click sends only one confirmation');
 // Changing the displayed row/modal cannot redirect the captured request.
 state.modal.request.credit.id='other-credit';state.modal.request.account_id='other-account';
 assert.equal(received.credit.id,'credit/a');assert.equal(received.account_id,'upstream-a');
 release({...received,status:'reset',windows_reset:2});await promise;
 assert.equal(refreshes,1);assert.equal(confirms,1);
 context.openCodexUsageReset(r);
 adapter.confirmUsageReset=async()=>{confirms++;throw new Error('simulated response loss')};adapter.getUsageResetRequests=async()=>{throw new Error('journal unavailable')};
 await context.handleCodexUsageResetAction('codex-usage-reset-confirm',{dataset:{request:r.id}});
 assert.equal(state.modal.request.id,r.id);assert.equal(state.modal.request.credit.id,r.credit.id);assert.equal(state.modal.request.status,'uncertain');assert.equal(prepares,1,'response loss cannot create a new intent');
 assert.match(context.codexUsageResetModal(state.modal),/Retry this exact request/);
 context.clearCodexUsageResetState();assert.equal(state.modal,null);assert.equal(state.usageResetRequests.length,0);
 let finishPrepare;adapter.prepareUsageReset=()=>new Promise(resolve=>{finishPrepare=resolve});
 const pending=context.handleCodexUsageResetAction('codex-usage-reset-prepare',{dataset:{provider:r.provider_id,connection:r.connection_id,account:r.account_id,credit:r.credit.id}});
 context.clearCodexUsageResetState();state.auth.authenticated=false;finishPrepare(r);await pending;assert.equal(state.modal,null,'late preparation cannot reopen a signed-out dialog');

 // HTTP adapter contract is captured directly; it cannot access real HTTP.
 const http=new HttpFerryAdapter(),calls=[];
 http.request=async(path,options={})=>{calls.push({path,...options});return {request:r,requests:[r]}};
 await http.getUsageResetRequests();await http.prepareUsageReset(r.provider_id,r.connection_id,r.credit.id,r.account_id);await http.confirmUsageReset(r);await http.cancelUsageReset(r);
 assert.deepEqual(calls.map(c=>c.method||'GET'),['GET','POST','POST','POST']);
 assert.equal(calls[2].path,'/api/providers/provider%2Fa/connections/connection%2Fa/oauth/usage-reset-requests/request%2Fa/confirm');
 assert.deepEqual(JSON.parse(calls[2].body),{credit_id:r.credit.id,account_id:r.account_id,confirmation:'consume_one_codex_usage_reset'});
 assert.match(calls[3].path,/\/cancel$/);
 // Exercise the real demo path as well as the isolated UI handler stubs.
 const demo=new MockFerryAdapter(),demoUsage=await demo.getConnectionOAuthUsage('codex-personal','account-a');
 const demoCredit=demoUsage.rate_limit_reset_credits.credits[0];
 const prepared=await demo.prepareUsageReset('codex-personal','account-a',demoCredit.id,demoUsage.account_id);
 assert.equal(prepared.status,'pending');assert.equal(prepared.credit.id,demoCredit.id);
 assert.equal((await demo.getConnectionOAuthUsage('codex-personal','account-a')).rate_limit_reset_credits.available_count,3,'preparation does not consume a demo credit');
 await demo.confirmUsageReset(prepared);await demo.confirmUsageReset(prepared);
 assert.equal((await demo.getConnectionOAuthUsage('codex-personal','account-a')).rate_limit_reset_credits.available_count,2,'confirmation consumes the selected demo credit only once');
 console.log('PASS offline credit details, confirmation boundary, immutable target, duplicate clicks, unknown outcomes and session changes');
} finally { globalThis.fetch=previousFetch; }
