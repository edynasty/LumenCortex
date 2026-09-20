import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { createCodingTools, resolveInside } from '../src/tools.js';

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
