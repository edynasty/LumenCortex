import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { spawn, spawnSync } from 'node:child_process';
import http from 'node:http';
import { CognitiveRepository } from '../src/repository.js';

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


test('light CLI exposes deterministic retrieval profiles', () => {
  const cwd = fs.mkdtempSync(path.join(os.tmpdir(), 'lcx-light-mode-'));
  const repo = new CognitiveRepository(cwd);
  repo.init();
  const graph = repo.graph();
  graph.addNode({ id: 'seed', kind: 'entity', title: 'root failure', body: 'root failure' });
  graph.addNode({ id: 'cause', kind: 'belief', title: 'causal branch', body: 'causal branch' });
  graph.addNode({ id: 'related', kind: 'belief', title: 'related branch', body: 'related branch' });
  graph.addEdge({ id: 'cause-edge', from: 'seed', to: 'cause', type: 'causes', weight: 1 });
  graph.addEdge({ id: 'related-edge', from: 'seed', to: 'related', type: 'relates_to', weight: 1 });
  repo.writeGraph(graph.snapshot());
  repo.close();

  const result = spawnSync(process.execPath, [
    cli,
    'light',
    'root failure',
    '--mode',
    'causal',
    '--json'
  ], {
    cwd,
    encoding: 'utf8',
    env: { ...process.env }
  });

  assert.equal(result.status, 0, result.stderr);
  const parsed = JSON.parse(result.stdout);
  assert.equal(parsed.mode, 'causal');
  const activation = Object.fromEntries(
    parsed.selectedNodes.map((node) => [node.id, node.activation])
  );
  assert.ok(Number.isFinite(activation.cause));
  assert.ok(
    activation.related === undefined ||
    activation.cause > activation.related
  );
});

test('search --hybrid loads embedding config and reaches semantic-only candidates', async () => {
  const cwd = fs.mkdtempSync(path.join(os.tmpdir(), 'lcx-cli-hybrid-'));
  const repo = new CognitiveRepository(cwd);
  repo.init();
  const graph = repo.graph();
  graph.addNode({
    id: 'inventory',
    kind: 'evidence',
    title: 'Inventory capacity coordinator',
    body: 'reserve available units before acceptance',
    grade: 'static',
    trustZone: 'repo_trusted'
  });
  graph.addNode({
    id: 'mailer',
    kind: 'evidence',
    title: 'Welcome notification sender',
    body: 'send welcome mail after registration',
    grade: 'static',
    trustZone: 'repo_trusted'
  });
  repo.writeGraph(graph.snapshot());
  repo.close();

  const server = http.createServer(async (req, res) => {
    if (req.method !== 'POST' || req.url !== '/v1/embeddings') {
      res.writeHead(404).end();
      return;
    }
    let body = '';
    for await (const chunk of req) body += chunk;
    const payload = JSON.parse(body);
    const values = Array.isArray(payload.input) ? payload.input : [payload.input];
    const data = values.map((value, index) => {
      const text = String(value).toLowerCase();
      const embedding = text.includes('warehouse contention') ||
        text.includes('inventory') ||
        text.includes('reserve available')
        ? [1, 0]
        : [0, 1];
      return { index, embedding };
    });
    res.writeHead(200, { 'content-type': 'application/json' });
    res.end(JSON.stringify({ model: payload.model, data }));
  });
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  const address = server.address();

  const configDir = path.join(cwd, '.lumencortex');
  fs.writeFileSync(path.join(configDir, 'cognition.json'), JSON.stringify({
    retrieval: {
      embeddings: {
        enabled: true,
        provider: 'generic',
        model: 'fake-embed',
        baseURL: `http://127.0.0.1:${address.port}/v1`,
        batchSize: 16
      }
    }
  }));

  try {
    const result = await runCli([
      'search',
      'warehouse contention',
      '--hybrid',
      '--limit',
      '5'
    ], { cwd });

    assert.equal(result.code, 0, result.stderr);
    const hits = JSON.parse(result.stdout);
    assert.equal(hits[0].nodeId, 'inventory');
    assert.ok(hits[0].reasons.includes('rrf:semantic'));
  } finally {
    await new Promise((resolve) => server.close(resolve));
  }
});

function runCli(argv, { cwd, env = {} } = {}) {
  return new Promise((resolve, reject) => {
    const child = spawn(process.execPath, [cli, ...argv], {
      cwd,
      env: { ...process.env, ...env },
      stdio: ['ignore', 'pipe', 'pipe']
    });
    let stdout = '';
    let stderr = '';
    child.stdout.on('data', (chunk) => { stdout += chunk; });
    child.stderr.on('data', (chunk) => { stderr += chunk; });
    child.on('error', reject);
    child.on('exit', (code) => resolve({ code, stdout, stderr }));
  });
}


test('Node skills CLI manages project Skills without touching the user global root', () => {
  const cwd = fs.mkdtempSync(path.join(os.tmpdir(), 'lcx-cli-skills-'));
  const globalRoot = path.join(cwd, 'isolated-global-skills');
  const repo = new CognitiveRepository(cwd);
  repo.init();
  repo.close();

  const source = path.join(cwd, 'skill-source.md');
  fs.writeFileSync(source, [
    '---',
    'name: CLI Test Skill',
    'description: managed through CLI',
    '---',
    '',
    'CLI_SKILL_SENTINEL'
  ].join('\n'));

  const env = {
    ...process.env,
    LUMENCORTEX_SKILLS_GLOBAL_ROOT: globalRoot
  };

  let result = spawnSync(process.execPath, [
    cli, 'skills', 'save', 'project', 'cli-test', source
  ], { cwd, encoding: 'utf8', env });
  assert.equal(result.status, 0, result.stderr);
  assert.equal(JSON.parse(result.stdout).id, 'cli-test');

  result = spawnSync(process.execPath, [
    cli, 'skills', 'list', 'effective'
  ], { cwd, encoding: 'utf8', env });
  assert.equal(result.status, 0, result.stderr);
  let items = JSON.parse(result.stdout);
  assert.equal(items.length, 1);
  assert.equal(items[0].id, 'cli-test');
  assert.equal(items[0].enabled, true);

  result = spawnSync(process.execPath, [
    cli, 'skills', 'show', 'project', 'cli-test'
  ], { cwd, encoding: 'utf8', env });
  assert.equal(result.status, 0, result.stderr);
  assert.match(JSON.parse(result.stdout).content, /CLI_SKILL_SENTINEL/);

  result = spawnSync(process.execPath, [
    cli, 'skills', 'disable', 'project', 'cli-test'
  ], { cwd, encoding: 'utf8', env });
  assert.equal(result.status, 0, result.stderr);

  result = spawnSync(process.execPath, [
    cli, 'skills', 'list', 'effective'
  ], { cwd, encoding: 'utf8', env });
  items = JSON.parse(result.stdout);
  assert.equal(items[0].enabled, false);

  result = spawnSync(process.execPath, [
    cli, 'skills', 'enable', 'project', 'cli-test'
  ], { cwd, encoding: 'utf8', env });
  assert.equal(result.status, 0, result.stderr);

  result = spawnSync(process.execPath, [
    cli, 'skills', 'delete', 'project', 'cli-test'
  ], { cwd, encoding: 'utf8', env });
  assert.equal(result.status, 0, result.stderr);

  result = spawnSync(process.execPath, [
    cli, 'skills', 'list', 'effective'
  ], { cwd, encoding: 'utf8', env });
  assert.deepEqual(JSON.parse(result.stdout), []);
  assert.equal(fs.existsSync(globalRoot), false);
});


test('Governor scheduler CLI persists pending plans and requires explicit apply/clear intent', () => {
  const cwd = fs.mkdtempSync(path.join(os.tmpdir(), 'lcx-cli-governor-scheduler-'));
  const repo = new CognitiveRepository(cwd);
  repo.init();
  const graph = repo.graph();
  graph.addNode({
    id: 'stale',
    kind: 'evidence',
    title: 'Old hypothesis',
    body: 'obsolete',
    status: 'stale',
    grade: 'hypothesis',
    trustZone: 'model_inferred'
  });
  repo.writeGraph(graph.snapshot());
  repo.close();

  const configDir = path.join(cwd, '.lumencortex');
  fs.writeFileSync(path.join(configDir, 'cognition.json'), JSON.stringify({
    governor: {
      enabled: false,
      scheduler: {
        enabled: true,
        useCurator: false,
        checkRevisionDelta: 1,
        cooldownMs: 0,
        archiveCandidateThreshold: 1,
        canonicalizeGroupThreshold: 999,
        branchCandidateThreshold: 999,
        promotionGroupThreshold: 999,
        tierChangeThreshold: 999
      }
    }
  }));

  let result = spawnSync(process.execPath, [
    cli, 'governor', 'scheduler', 'run', '--force'
  ], { cwd, encoding: 'utf8', env: { ...process.env } });
  assert.equal(result.status, 0, result.stderr);
  const planned = JSON.parse(result.stdout);
  assert.equal(planned.scheduled, true);
  assert.ok(planned.pending.id.startsWith('govplan_'));

  result = spawnSync(process.execPath, [
    cli, 'governor', 'scheduler', 'status'
  ], { cwd, encoding: 'utf8', env: { ...process.env } });
  assert.equal(result.status, 0, result.stderr);
  const status = JSON.parse(result.stdout);
  assert.equal(status.state.pending.id, planned.pending.id);
  assert.equal(status.state.pending.stale, false);

  result = spawnSync(process.execPath, [
    cli, 'governor', 'scheduler', 'apply', '--dry-run'
  ], { cwd, encoding: 'utf8', env: { ...process.env } });
  assert.equal(result.status, 0, result.stderr);
  const dryRun = JSON.parse(result.stdout);
  assert.equal(dryRun.applied, false);
  assert.equal(dryRun.pending, true);

  result = spawnSync(process.execPath, [
    cli, 'governor', 'scheduler', 'apply'
  ], { cwd, encoding: 'utf8', env: { ...process.env } });
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /requires --yes or --dry-run/);

  result = spawnSync(process.execPath, [
    cli, 'governor', 'scheduler', 'clear', '--yes'
  ], { cwd, encoding: 'utf8', env: { ...process.env } });
  assert.equal(result.status, 0, result.stderr);
  assert.equal(JSON.parse(result.stdout).cleared, true);

  result = spawnSync(process.execPath, [
    cli, 'governor', 'scheduler', 'status'
  ], { cwd, encoding: 'utf8', env: { ...process.env } });
  assert.equal(JSON.parse(result.stdout).state.pending, null);
});
