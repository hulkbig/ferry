import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import { HttpFerryAdapter, MockFerryAdapter } from './api.js';

const json = (body, status = 200) => new Response(JSON.stringify(body), {
  status,
  headers: { 'Content-Type':'application/json' },
});
const makeFetch = handler => async (rawUrl, options = {}) => {
  const url = new URL(rawUrl, 'https://console.test');
  const method = options.method || 'GET';
  const body = options.body ? JSON.parse(options.body) : null;
  return handler({ url, method, body });
};

// ---------- 1. Account operations render recovery affordances ----------
const part12 = await fs.readFile(new URL('./app.part12.jsfrag', import.meta.url), 'utf8');
const esc = value => String(value).replaceAll('&','&amp;').replaceAll('<','&lt;').replaceAll('>','&gt;').replaceAll('"','&quot;');
const renderer = part12.slice(part12.indexOf('function codexUsageWindowLabel('), part12.indexOf('function codexAccountOperations('));
const opsSource = part12.slice(part12.indexOf('function codexAccountNeedsRelogin('), part12.indexOf('async function loadCachedCodexUsage('));
let entry = { usage:null, loading:false, error:'', loaded:true };
const { ops, needsRelogin } = new Function('esc', 'state', 'providerCodexUsageEntry', 'icon',
  `${renderer}${opsSource};return { ops:codexAccountOperations, needsRelogin:codexAccountNeedsRelogin };`,
)(esc, { modal:{ providerId:'prov-1' } }, () => entry, () => '<svg/>');
const connection = { id:'conn-1', identity:'codex-example', status:'Active', enabled:true, oauthPending:false };

assert.equal(needsRelogin('account codex-example needs a new OAuth login: its refresh token was rejected (retry suppressed)'), true);
assert.equal(needsRelogin('read Codex usage: re-login may be required'), true);
assert.equal(needsRelogin('dial tcp: i/o timeout'), false);
assert.equal(needsRelogin(''), false);

entry = { ...entry, error:'account codex-example needs a new OAuth login: its refresh token was rejected (retry suppressed until 2026-09-08)' };
const rejected = ops(connection);
assert.match(rejected, /Usage refresh failed/);
assert.match(rejected, /data-action="provider-codex-relogin" data-mode="browser" data-provider="prov-1" data-connection="conn-1"/);
assert.match(rejected, /data-action="provider-codex-relogin" data-mode="device"/);
assert.match(rejected, />Sign in again</);
assert.match(rejected, />Sign in with Codex</);
assert.match(rejected, />Use device code</);
assert.match(rejected, /Account credential/);

entry = { ...entry, error:'read Codex usage: context deadline exceeded' };
const transient = ops(connection);
assert.doesNotMatch(transient, />Sign in again</);
assert.match(transient, /Usage refresh failed/);
assert.match(transient, /data-action="provider-codex-relogin" data-mode="browser"/);

const pending = ops({ ...connection, oauthPending:true });
assert.match(pending, /data-action="resume-oauth" data-provider="prov-1" data-id="conn-1"/);
assert.match(pending, />Continue pending sign-in</);

entry = { ...entry, error:'account x needs a new OAuth login: <script>alert(1)</script>' };
assert.doesNotMatch(ops(connection), /<script>alert/);
console.log('PASS account operations surface re-login for rejected credentials');

// ---------- 2. Provider workspace usage cards keep the recovery inline ----------
const part4 = await fs.readFile(new URL('./app.part4.jsfrag', import.meta.url), 'utf8');
const sectionHelpers = part4.slice(part4.indexOf('function providerIsEnabled('), part4.indexOf('function providerCodexUsageKey('));
const sectionSource = part12.slice(part12.indexOf('function providerCodexUsageSection('), part12.indexOf('function codexProviderFormModal('));
const { section } = new Function('esc', 'state', 'providerCodexUsageEntry', 'badge', 'icon',
  `${sectionHelpers}${renderer}${sectionSource};return { section:providerCodexUsageSection };`,
)(esc, { modal:null }, () => entry, label => `<span>${label}</span>`, () => '<svg/>');
const provider = { raw:{ type:'cliproxy-codex' } };
const accounts = [{ id:'conn-1', account:'codex-example', status:'Serving' }];

entry = { ...entry, error:'read Codex usage: account codex-example needs a new OAuth login: its refresh token was rejected' };
const workspaceRejected = section('prov-1', provider, {}, accounts);
assert.match(workspaceRejected, /Usage refresh failed/);
assert.match(workspaceRejected, /data-action="provider-codex-relogin" data-mode="browser" data-provider="prov-1" data-connection="conn-1"/);

entry = { ...entry, error:'read Codex usage: connection timeout' };
assert.doesNotMatch(section('prov-1', provider, {}, accounts), /provider-codex-relogin/);
entry = { ...entry, error:'' };
assert.doesNotMatch(section('prov-1', provider, {}, accounts), /Usage refresh failed/);
console.log('PASS provider workspace usage cards inline the sign-in action only for credential rejection');

// ---------- 2. HTTP adapter maps device login, status, cancel, completion ----------
const seen = [];
let loginActive = true;
const http = new HttpFerryAdapter('', makeFetch(({ url, method, body }) => {
  seen.push({ path:url.pathname, method, body });
  if (url.pathname.endsWith('/oauth/login') && method === 'POST') {
    return json(body.mode === 'device'
      ? { provider_id:'prov-1', connection_id:'conn-1', account:'codex-example', mode:'device', status:'polling', user_code:'WDJB-MJHT', verification_url:'https://auth.openai.com/codex/device', interval_seconds:7 }
      : { provider_id:'prov-1', connection_id:'conn-1', account:'codex-example', mode:'browser', status:'pending', authorize_url:'https://auth.test/authorize' });
  }
  if (url.pathname.endsWith('/oauth/status')) {
    return json(loginActive
      ? { provider_id:'prov-1', connection_id:'conn-1', account:'codex-example', status:'active', login:{ status:'polling', mode:'device', user_code:'WDJB-MJHT', verification_url:'https://auth.openai.com/codex/device' } }
      : { provider_id:'prov-1', connection_id:'conn-1', account:'codex-example', status:'active' });
  }
  if (url.pathname.endsWith('/oauth/login') && method === 'DELETE') return json({ status:'cancelled' });
  if (url.pathname.endsWith('/oauth/callback')) return json({ status:'active' });
  return json({ status:'ok' });
}));

const device = await http.startConnectionOAuth('prov-1', 'conn-1', { mode:'device' });
assert.deepEqual(seen.at(-1).body, { mode:'device' });
assert.equal(device.userCode, 'WDJB-MJHT');
assert.equal(device.verificationUrl, 'https://auth.openai.com/codex/device');
assert.equal(device.intervalSeconds, 7);

const browser = await http.startConnectionOAuth('prov-1', 'conn-1');
assert.deepEqual(seen.at(-1).body, { mode:'browser' });
assert.equal(browser.authorizeUrl, 'https://auth.test/authorize');

const waiting = await http.getConnectionOAuthStatus('prov-1', 'conn-1');
assert.equal(waiting.login.status, 'polling');
assert.equal(waiting.login.userCode, 'WDJB-MJHT');
loginActive = false;
const finished = await http.getConnectionOAuthStatus('prov-1', 'conn-1');
assert.equal(finished.login, null);

await http.cancelConnectionOAuth('prov-1', 'conn-1');
assert.equal(seen.at(-1).method, 'DELETE');

seen.length = 0;
await http.completeConnectionOAuth('prov-1', 'conn-1', 'https://localhost:1455/auth/callback?code=x&state=y', { autoEnable:false });
assert.deepEqual(seen.map(item => item.method), ['POST']);
assert.equal(seen[0].body.callback_url.includes('code=x'), true);
console.log('PASS http adapter: device mapping, status polling, cancel, re-login completion skips forced enable');

// ---------- 3. Mock adapter drives the device flow end to end ----------
const mock = new MockFerryAdapter();
const mockLogin = await mock.startConnectionOAuth('codex-personal', 'account-a', { mode:'device' });
assert.equal(mockLogin.mode, 'device');
assert.match(mockLogin.userCode, /^[A-Z]{4}-[A-Z]{4}$/);
assert.equal((await mock.getConnectionOAuthStatus('codex-personal', 'account-a')).login.status, 'polling');
await mock.getConnectionOAuthStatus('codex-personal', 'account-a');
assert.equal((await mock.getConnectionOAuthStatus('codex-personal', 'account-a')).login, null);

const disabled = mock.data.connections['codex-personal'].find(item => item.id === 'account-a');
disabled.status = 'Unavailable';
disabled.enabled = false;
await mock.completeConnectionOAuth('codex-personal', 'account-a', 'https://localhost:1455/auth/callback?code=x&state=y', { autoEnable:false });
assert.equal(disabled.enabled, false);
await mock.completeConnectionOAuth('codex-personal', 'account-a', 'https://localhost:1455/auth/callback?code=x&state=y');
assert.equal(disabled.enabled, true);
console.log('PASS mock adapter: device polling completes and first-time sign-in still enables');
