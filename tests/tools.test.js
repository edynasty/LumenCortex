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
