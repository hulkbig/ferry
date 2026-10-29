#!/usr/bin/env node
'use strict';

const fs = require('fs');
const path = require('path');
const { spawnSync } = require('child_process');

const file = process.argv[2] || '/tmp/console.html';
if (!fs.existsSync(file)) {
  console.error('未找到 ' + file + ' — 先运行 CPA_CONSOLE_DUMP=1 导出');
  process.exit(1);
}
const root = path.resolve(__dirname, '..');
const handoff = path.join(root, 'web', 'console');
const html = fs.readFileSync(file, 'utf8');
const api = fs.readFileSync(path.join(handoff, 'api.js'), 'utf8');
const fragments = pattern => fs.readdirSync(handoff).filter(name => pattern.test(name))
  .sort((left, right) => left.localeCompare(right, 'en', {numeric:true}))
  .map(name => fs.readFileSync(path.join(handoff, name), 'utf8')).join('');
const app = fragments(/^app\.part\d+\.jsfrag$/);
const css = fragments(/^styles\.part\d+\.css$/);
const nextCss = fs.readFileSync(path.join(handoff,'styles.part8.css'),'utf8');
let failures = 0;
const check = (name, condition) => {
  console.log((condition ? 'PASS' : 'FAIL') + ' ' + name);
  if (!condition) failures++;
};

const syntax = spawnSync(process.execPath, [path.join(handoff, 'verify-source.mjs')], { encoding:'utf8' });
if (syntax.status !== 0) console.error(syntax.stderr || syntax.stdout);
check('javascript: assembled source parses successfully', syntax.status === 0);
check('production: reviewed next console is served', html.includes('Ferry Console') && html.includes('./app-loader.js') && app.includes('data-visual-version="next"'));
check('production: same-origin HTTP adapter is default', html.includes('data-default-adapter="http"'));
check('demo: standalone handoff remains mock-capable', api.includes("this.mode = 'mock'") && api.includes("params.get('demo') === '1'"));
check('brand: Ferry only', html.includes('Ferry') && !/>CPA</.test(html));
check('design: next theme supports light and dark', nextCss.includes('[data-theme=dark]') && nextCss.includes('--blue:') && nextCss.includes('--surface:'));
check('design: restrained geometry and linear icons', /--radius:\s*10px/.test(css) && /--radius-sm:\s*7px/.test(css) && app.includes('stroke-width="1.75"'));
check('design: next analytics and tabbed provider configuration are embedded', nextCss.includes('.next-metrics') && app.includes('pageOverviewNext') && app.includes("nextDetailTabs('providers',id)") && app.includes('Provider Configuration') && app.includes('data-action="edit-provider"'));
check('design: no grid-paper background', !css.includes('CHANNEL-CENTERED'));

for (const label of ['Overview', 'Models & Routing', 'Providers', 'Requests', 'API Keys', 'Settings']) {
  check('navigation: ' + label, app.includes(label));
}
check('navigation: no legacy top-level policy/channel/pool/target pages',
  !app.includes("['policies'") && !app.includes("['channels'") && !app.includes("['pools'") && !app.includes("['targets'"));

check('overview: direct/recovered/failed are distinct',
  ['Direct Success', 'Recovered', 'Failed'].every(label => app.includes(label)));
check('routing: edit-review-apply with CAS recovery',
  app.includes('Review routing changes') && app.includes('routing-apply') && app.includes('e.status===409'));
check('providers: single-layer endpoint/credential/models workspace',
  ['Each provider is one upstream channel', 'Edit Provider', 'Detected upstream models', 'Last successful request'].every(label => app.includes(label)) &&
  !app.includes('Recovery Policy') && !app.includes('Live Recovery'));
check('providers: OAuth/API key/existing credential setup',
  ['Codex OAuth', 'API Key / Token', 'Existing Credential', 'Complete Codex sign-in'].every(label => app.includes(label)));
check('requests: server-side filters and one request trace',
  app.includes('reloadRequests') && app.includes('Execution Trace') && app.includes('Attempt Path') &&
  app.includes('Candidate Exclusions') && app.includes('First Frame / TTFT') &&
  app.includes('Complete Response') && app.includes('Cache read tokens'));
check('api keys: complete masked lifecycle with explicit reveal and copy',
  ['key-create-submit', 'reveal-key', 'copy-stored-key', 'rotate-key', 'toggle-key', 'revoke-key', 'delete-key'].every(label => app.includes(label)) &&
  api.includes('secret_preview') && api.includes('/secret'));
check('organization: no fake tenant isolation in real mode',
  !app.includes('Single-runtime scope') && !app.includes('Local Runtime') && api.includes('organizationIsolation:false'));
check('errors: loading/partial/full/conflict states',
  app.includes('Loading Ferry Console') && app.includes('Runtime partially available') &&
  app.includes('Console failed to load') && app.includes('Revision conflict'));

for (const endpoint of ['/api/analytics', '/api/overview', '/api/models', '/api/providers', '/api/requests', '/api/api-keys', '/api/revisions', '/api/usage-billing']) {
  check('api: ' + endpoint, api.includes(endpoint));
}
check('responsive: mobile breakpoint', /@media\s*\(max-width:\s*780px\)/.test(css));
check('accessibility: main landmark and skip link', html.includes('Skip to content') && app.includes('<main'));

console.log(failures === 0 ? '\nALL PASS' : '\n' + failures + ' FAILURES');
process.exit(failures === 0 ? 0 : 1);
