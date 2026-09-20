import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { spawnSync } from 'node:child_process';

const cli = fileURLToPath(new URL('../src/cli.js', import.meta.url));

test('doctor can inspect provider configuration outside a LumenCortex workspace', () => {
  const cwd = fs.mkdtempSync(path.join(os.tmpdir(), 'lcx-doctor-'));
  const result = spawnSync(process.execPath, [
    cli,
    'doctor',
    '--provider',
    'generic'
  ], {
    cwd,
    encoding: 'utf8',
    env: {
      ...process.env,
      LUMENCORTEX_BASE_URL: 'http://127.0.0.1:11434/v1',
      LUMENCORTEX_MODEL: 'qwen3:4b-instruct',
      LUMENCORTEX_REQUIRE_API_KEY: 'false'
    }
  });

  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /provider: generic/);
  assert.match(result.stdout, /qwen3:4b-instruct/);
  assert.equal(fs.existsSync(path.join(cwd, '.lumencortex')), false);
});
