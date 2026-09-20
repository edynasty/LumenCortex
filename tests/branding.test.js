import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { BRAND } from '../src/brand.js';

const root=path.resolve(path.dirname(fileURLToPath(import.meta.url)),'..');

test('LumenCortex package exposes short, full and legacy CLI commands', () => {
  const pkg=JSON.parse(fs.readFileSync(path.join(root,'package.json'),'utf8'));
  assert.equal(pkg.name,'lumencortex');
  assert.equal(pkg.version,'0.5.0');
  assert.equal(pkg.bin.lcx,'./bin/lcx.js');
  assert.equal(pkg.bin.lumencortex,'./bin/lumencortex.js');
  assert.equal(pkg.bin.modelweave,'./bin/modelweave.js');
  assert.equal(BRAND.name,'LumenCortex');
  assert.equal(BRAND.stateDir,'.lumencortex');
});

test('full and short LumenCortex wrappers default to the TUI', () => {
  const full=fs.readFileSync(path.join(root,'bin','lumencortex.js'),'utf8');
  const short=fs.readFileSync(path.join(root,'bin','lcx.js'),'utf8');
  assert.match(full,/process\.argv\.length === 2.*push\('tui'\)/s);
  assert.match(short,/process\.argv\.length === 2.*push\('tui'\)/s);
});
