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


test('cognitive blame reports commits that changed a node', () => {
  const repo = tempRepo();
  let graph = repo.graph();
  graph.addNode({ id: 'n1', kind: 'belief', title: 'Inventory phase', body: 'unknown' });
  repo.writeGraph(graph.snapshot());
  const first = repo.commit('seed node');

  graph = repo.graph();
  graph.updateNode('n1', { body: 'ACCEPT' });
  repo.writeGraph(graph.snapshot());
  const second = repo.commit('refine node');

  const blame = repo.blameNode('n1');
  assert.equal(blame.length, 2);
  assert.equal(blame[0].commitId, second.id);
  assert.equal(blame[1].commitId, first.id);
});

test('cognitive cherry-pick grafts a compatible cognition diff', () => {
  const repo = tempRepo();
  let graph = repo.graph();
  graph.addNode({ id: 'base', kind: 'entity', title: 'Base' });
  repo.writeGraph(graph.snapshot());
  repo.commit('base');

  repo.createBranch('finding');
  repo.checkout('finding');
  graph = repo.graph();
  graph.addNode({ id: 'finding1', kind: 'belief', title: 'Finding', body: 'verified path' });
  repo.writeGraph(graph.snapshot());
  const findingCommit = repo.commit('add finding');

  repo.checkout('main');
  const picked = repo.cherryPick(findingCommit.id);
  assert.equal(picked.conflicts.length, 0);
  assert.ok(repo.graph().getNode('finding1'));
  assert.equal(picked.commit.metadata.cherryPick, findingCommit.id);
});


test('legacy .modelweave state is migrated to .lumencortex without losing cognitive history', () => {
  const root=fs.mkdtempSync(path.join(os.tmpdir(),'lumencortex-migrate-'));
  const original=new CognitiveRepository(root);
  original.init();
  let graph=original.graph();
  graph.addNode({ id:'legacy-node', kind:'belief', title:'Legacy cognition', body:'must survive rename' });
  original.writeGraph(graph.snapshot());
  const commit=original.commit('legacy cognition');

  fs.renameSync(path.join(root,'.lumencortex'),path.join(root,'.modelweave'));
  const migrated=new CognitiveRepository(root);

  assert.equal(migrated.dir,path.join(root,'.lumencortex'));
  assert.equal(fs.existsSync(path.join(root,'.modelweave')),false);
  assert.equal(migrated.graph().getNode('legacy-node').body,'must survive rename');
  assert.equal(migrated.headCommitId(),commit.id);
});
