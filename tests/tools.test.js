import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { createCodingTools, resolveInside, ToolRegistry } from '../src/tools.js';
import { CognitiveRepository } from '../src/repository.js';
import { LumenCortexRuntime } from '../src/runtime.js';

test('resolveInside blocks traversal', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'mw-tools-'));
  assert.throws(() => resolveInside(root, '../escape.txt'));
});

test('coding tools read, replace and search', async () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'mw-tools-'));
  fs.writeFileSync(path.join(root, 'a.txt'), 'hello\nworld\n');
  const tools = createCodingTools({ workspace: root });
  const read = await tools.execute('read_file', { path: 'a.txt' }, { authorize: async () => true });
  assert.match(read.content, /hello/);
  const replace = await tools.execute('replace_in_file', { path: 'a.txt', old_text: 'world', new_text: 'agent' }, { authorize: async () => true });
  assert.equal(replace.ok, true);
  const search = await tools.execute('search_text', { query: 'agent' }, { authorize: async () => true });
  assert.match(search.content, /a.txt/);
});


test('apply_patch performs validated multi-hunk and multi-file edits', async () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'lcx-patch-'));
  fs.writeFileSync(path.join(root, 'a.txt'), 'alpha\nbeta\ngamma\n');
  const tools = createCodingTools({ workspace: root });

  const result = await tools.execute('apply_patch', {
    patches: [
      {
        path: 'a.txt',
        operation: 'update',
        edits: [
          { old_text: 'alpha', new_text: 'ALPHA' },
          { old_text: 'gamma', new_text: 'GAMMA' }
        ]
      },
      {
        path: 'nested/new.txt',
        operation: 'create',
        content: 'created\n'
      }
    ]
  }, { authorize: async () => true });

  assert.equal(result.ok, true);
  assert.equal(fs.readFileSync(path.join(root, 'a.txt'), 'utf8'), 'ALPHA\nbeta\nGAMMA\n');
  assert.equal(fs.readFileSync(path.join(root, 'nested/new.txt'), 'utf8'), 'created\n');
});


test('apply_patch validates the whole batch before mutating files', async () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'lcx-patch-validate-'));
  fs.writeFileSync(path.join(root, 'a.txt'), 'keep me\n');
  fs.writeFileSync(path.join(root, 'b.txt'), 'duplicate duplicate\n');
  const tools = createCodingTools({ workspace: root });

  const result = await tools.execute('apply_patch', {
    patches: [
      {
        path: 'a.txt',
        operation: 'update',
        edits: [{ old_text: 'keep me', new_text: 'changed' }]
      },
      {
        path: 'b.txt',
        operation: 'update',
        edits: [{ old_text: 'duplicate', new_text: 'x' }]
      }
    ]
  }, { authorize: async () => true });

  assert.equal(result.ok, false);
  assert.equal(fs.readFileSync(path.join(root, 'a.txt'), 'utf8'), 'keep me\n');
  assert.equal(fs.readFileSync(path.join(root, 'b.txt'), 'utf8'), 'duplicate duplicate\n');
  assert.match(result.content, /ambiguous/);
});


test('apply_patch blocks workspace traversal and duplicate targets', async () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'lcx-patch-safe-'));
  fs.writeFileSync(path.join(root, 'a.txt'), 'one\n');
  const tools = createCodingTools({ workspace: root });

  const escape = await tools.execute('apply_patch', {
    patches: [{ path: '../escape.txt', operation: 'create', content: 'nope' }]
  }, { authorize: async () => true });
  assert.equal(escape.ok, false);
  assert.match(escape.content, /escapes workspace/);

  const duplicate = await tools.execute('apply_patch', {
    patches: [
      { path: 'a.txt', operation: 'update', edits: [{ old_text: 'one', new_text: 'two' }] },
      { path: 'a.txt', operation: 'delete' }
    ]
  }, { authorize: async () => true });
  assert.equal(duplicate.ok, false);
  assert.equal(fs.readFileSync(path.join(root, 'a.txt'), 'utf8'), 'one\n');
});



test('shell tool keeps the Node event loop responsive during long commands', async () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'lcx-shell-async-'));
  const tools = createCodingTools({ workspace: root });
  const node = JSON.stringify(process.execPath);
  let timerFired = false;
  setTimeout(() => { timerFired = true; }, 25);

  const pending = tools.execute('shell', {
    command: `${node} -e "setTimeout(() => console.log('ASYNC_SHELL_OK'), 120)"`,
    timeout_ms: 2000
  }, { authorize: async () => true });

  await new Promise((resolve) => setTimeout(resolve, 50));
  assert.equal(timerFired, true);

  const result = await pending;
  assert.equal(result.ok, true);
  const payload = JSON.parse(result.content);
  assert.equal(payload.exitCode, 0);
  assert.equal(payload.timedOut, false);
  assert.match(payload.stdout, /ASYNC_SHELL_OK/);
});


test('shell tool streams stdout before process completion', async () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'lcx-shell-stream-'));
  const tools = createCodingTools({ workspace: root });
  const node = JSON.stringify(process.execPath);
  const chunks = [];
  let finished = false;

  const pending = tools.execute('shell', {
    command: `${node} -e "console.log('FIRST'); setTimeout(() => console.error('SECOND'), 120)"`,
    timeout_ms: 2000
  }, {
    authorize: async () => true,
    onOutput: (event) => chunks.push({ ...event, finished })
  }).then((result) => {
    finished = true;
    return result;
  });

  await new Promise((resolve) => setTimeout(resolve, 70));
  assert.ok(chunks.some((event) => event.stream === 'stdout' && /FIRST/.test(event.chunk)));
  assert.ok(chunks.some((event) => event.finished === false), 'at least one output chunk should arrive before completion');

  const result = await pending;
  assert.equal(result.ok, true);
  assert.ok(chunks.some((event) => event.stream === 'stderr' && /SECOND/.test(event.chunk)));
});


test('shell tool abort signal terminates the active command', async () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'lcx-shell-cancel-'));
  const tools = createCodingTools({ workspace: root });
  const controller = new AbortController();
  const node = JSON.stringify(process.execPath);

  const pending = tools.execute('shell', {
    command: `${node} -e "setTimeout(() => console.log('SHOULD_NOT_FINISH'), 5000)"`,
    timeout_ms: 10000
  }, {
    authorize: async () => true,
    signal: controller.signal
  });

  setTimeout(() => controller.abort(), 40);
  await assert.rejects(
    pending,
    (error) => error?.name === 'AbortError'
  );
});


test('shell tool reports timeout without blocking the runtime', async () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'lcx-shell-timeout-'));
  const tools = createCodingTools({ workspace: root });
  const node = JSON.stringify(process.execPath);
  const result = await tools.execute('shell', {
    command: `${node} -e "setTimeout(() => {}, 5000)"`,
    timeout_ms: 80
  }, { authorize: async () => true });

  assert.equal(result.ok, true);
  const payload = JSON.parse(result.content);
  assert.equal(payload.timedOut, true);
  assert.notEqual(payload.signal, null);
});



test('tool registry accepts camelCase aliases for snake_case schemas', async () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'mw-tools-alias-'));
  fs.writeFileSync(path.join(dir, 'a.txt'), 'one\ntwo\nthree\n');
  const tools = createCodingTools({ workspace: dir });
  const result = await tools.execute('read_file', { path: 'a.txt', startLine: 2, endLine: 2 });
  assert.equal(result.ok, true);
  const parsed = JSON.parse(result.content);
  assert.equal(parsed.startLine, 2);
  assert.equal(parsed.endLine, 2);
  assert.match(parsed.content, /2: two/);
});


test('tool registry can expose a bounded schema working set', () => {
  const registry = new ToolRegistry()
    .register({ name: 'alpha', execute: () => 'a' })
    .register({ name: 'beta', execute: () => 'b' })
    .register({ name: 'gamma', execute: () => 'g' });

  assert.deepEqual(
    registry.schemas(['gamma', 'alpha']).map(schema => schema.function.name),
    ['gamma', 'alpha']
  );
  assert.throws(() => registry.schemas(['missing']), /Unknown tool/);
});


test('LumenCortex tools expose only LumenCortex cognitive tool names', () => {
  const root=fs.mkdtempSync(path.join(os.tmpdir(),'lumencortex-tools-'));
  const repo=new CognitiveRepository(root);
  repo.init();
  const runtime=new LumenCortexRuntime(repo);
  const registry=createCodingTools({workspace:root,repository:repo,runtime});
  const names=registry.schemas().map(schema=>schema.function.name);
  assert.ok(names.includes('lumencortex_context'));
  assert.ok(names.includes('lumencortex_ingest'));
  assert.deepEqual(
    names.filter(name => name.includes('cortex') || name.includes('weave')).sort(),
    ['lumencortex_context','lumencortex_ingest']
  );
});
