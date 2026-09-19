import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { CognitiveRepository } from '../src/index.js';

function tempRepo() {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'modelweave-'));
  const repo = new CognitiveRepository(dir);
  repo.init();
  return repo;
}

test('repository commits, branches, merges and reverts', () => {
  const repo = tempRepo();
  let graph = repo.graph();
  graph.addNode({ id: 'root', kind: 'entity', title: 'Inventory' });
  repo.writeGraph(graph.snapshot());
  const base = repo.commit('add inventory');

  repo.createBranch('hypothesis');
  repo.checkout('hypothesis');
  graph = repo.graph();
  graph.addNode({ id: 'h1', kind: 'belief', title: 'Race condition', body: 'possible race' });
  repo.writeGraph(graph.snapshot());
  repo.commit('investigate race');

  repo.checkout('main');
  const merged = repo.merge('hypothesis');
  assert.equal(merged.conflicts.length, 0);
  assert.ok(repo.graph().getNode('h1'));

  const reverted = repo.revert(merged.commit.id);
  assert.equal(reverted.conflicts.length, 0);
  assert.equal(repo.graph().getNode('h1'), undefined);
  assert.ok(repo.getCommit(base.id));
});

test('three-way merge detects conflicting cognition edits', () => {
  const repo = tempRepo();
  let graph = repo.graph();
  graph.addNode({ id: 'n1', kind: 'belief', title: 'Deduct phase', body: 'unknown' });
  repo.writeGraph(graph.snapshot());
  repo.commit('seed belief');
  repo.createBranch('other');

  graph = repo.graph();
  graph.updateNode('n1', { body: 'ACCEPT' });
  repo.writeGraph(graph.snapshot());
  repo.commit('ours says accept');

  repo.checkout('other');
  graph = repo.graph();
  graph.updateNode('n1', { body: 'FULFILL' });
  repo.writeGraph(graph.snapshot());
  repo.commit('theirs says fulfill');

  repo.checkout('main');
  const result = repo.merge('other');
  assert.equal(result.conflicts.length, 1);
  assert.equal(result.conflicts[0].id, 'n1');
});
