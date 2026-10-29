import fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';

const parts = [];
for (let i = 1; i <= 14; i += 1) {
  parts.push(await fs.readFile(new URL(`./app.part${i}.jsfrag`, import.meta.url), 'utf8'));
}
let source = parts.join('');
source = source.replace(
  "import { createAdapter } from './api.js';",
  "const { createAdapter } = await import(new URL('./api.js', document.baseURI).href);"
);
source = source.replace(
  "import { scenarios } from './mock-data.js';",
  "const { scenarios } = await import(new URL('./mock-data.js', document.baseURI).href);"
);
const file = path.join(os.tmpdir(), `ferry-console-${process.pid}.mjs`);
await fs.writeFile(file, source);
const result = spawnSync(process.execPath, ['--check', file], { encoding: 'utf8' });
await fs.unlink(file).catch(() => {});
if (result.status !== 0) {
  process.stderr.write(result.stderr || result.stdout || 'assembled source syntax check failed\n');
  process.exit(result.status || 1);
}
console.log('Ferry Console v3 assembled source: PASS');
