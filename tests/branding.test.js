import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { spawnSync } from 'node:child_process';
import { BRAND } from '../src/brand.js';

const root=path.resolve(path.dirname(fileURLToPath(import.meta.url)),'..');

test('LumenCortex package exposes short and full CLI commands', () => {
  const pkg=JSON.parse(fs.readFileSync(path.join(root,'package.json'),'utf8'));
  assert.equal(pkg.name,'lumencortex');
  assert.equal(pkg.version,'0.6.0');
  assert.equal(pkg.bin.lcx,'./bin/lcx.js');
  assert.equal(pkg.bin.lumencortex,'./bin/lumencortex.js');
  assert.equal(BRAND.name,'LumenCortex');
  assert.equal(BRAND.stateDir,'.lumencortex');
});

test('full and short LumenCortex wrappers default to the TUI', () => {
  const full=fs.readFileSync(path.join(root,'bin','lumencortex.js'),'utf8');
  const short=fs.readFileSync(path.join(root,'bin','lcx.js'),'utf8');
  assert.match(full,/process\.argv\.length === 2.*push\('tui'\)/s);
  assert.match(short,/process\.argv\.length === 2.*push\('tui'\)/s);
});


test('lumencortex with no arguments opens the TUI and can exit without provider credentials', () => {
  const workspace=fs.mkdtempSync(path.join(process.env.RUNNER_TEMP ?? '/tmp','lumencortex-tui-'));
  const result=spawnSync(process.execPath,[path.join(root,'bin','lumencortex.js')],{
    cwd:workspace,
    input:':quit\n',
    encoding:'utf8',
    timeout:5000,
    env:{
      ...process.env,
      LUMENCORTEX_PROVIDER:'openrouter',
      OPENROUTER_API_KEY:''
    }
  });
  assert.equal(result.status,0,`stderr=${result.stderr}\nstdout=${result.stdout}`);
  assert.equal(fs.existsSync(path.join(workspace,'.lumencortex')),true);
});
