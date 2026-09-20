import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { CognitiveRepository } from '../src/index.js';

function tempRepo() {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'mw-cognitive-git-'));
  const repo = new CognitiveRepository(dir);
  repo.init();
  return repo;
}

test('cognitive blame traces the commits that changed an object', () => {
  const repo = tempRepo();
  let graph = repo.graph();
  graph.addNode({ id: 'belief1', kind: 'belief', title: 'Inventory phase', body: 'unknown' });
  repo.writeGraph(graph.snapshot());
  const created = repo.commit('create belief');

  graph = repo.graph();
  graph.updateNode('belief1', { body: 'ACCEPT' });
  repo.writeGraph(graph.snapshot());
  const updated = repo.commit('resolve belief');

  const history = repo.blame('belief1');
  assert.equal(history[0].commitId, updated.id);
  assert.equal(history[1].commitId, created.id);
});

test('cognitive cherry-pick grafts a finding from another branch', () => {
  const repo = tempRepo();
  repo.createBranch('finding');
  repo.checkout('finding');

  let graph = repo.graph();
  graph.addNode({ id: 'finding1', kind: 'belief', title: 'Retry causes duplicate acceptance', body: 'candidate cause' });
  repo.writeGraph(graph.snapshot());
  const findingCommit = repo.commit('record retry finding');

  repo.checkout('main');
  const result = repo.cherryPick(findingCommit.id);
  assert.equal(result.conflicts.length, 0);
  assert.ok(repo.graph().getNode('finding1'));
  assert.equal(result.commit.metadata.cherryPickOf, findingCommit.id);
});

test('cognitive rebase replays branch cognition onto a newer base', () => {
  const repo = tempRepo();

  let graph = repo.graph();
  graph.addNode({ id: 'root', kind: 'entity', title: 'Root' });
  repo.writeGraph(graph.snapshot());
  repo.commit('baseline');
  repo.createBranch('feature');

  graph = repo.graph();
  graph.addNode({ id: 'upstream', kind: 'belief', title: 'Upstream fact', body: 'new main cognition' });
  repo.writeGraph(graph.snapshot());
  repo.commit('advance main');

  repo.checkout('feature');
  graph = repo.graph();
  graph.addNode({ id: 'feature-fact', kind: 'belief', title: 'Feature fact', body: 'branch cognition' });
  repo.writeGraph(graph.snapshot());
  repo.commit('feature cognition');

  const result = repo.rebase('main');
  assert.equal(result.conflicts.length, 0);
  assert.ok(repo.graph().getNode('upstream'));
  assert.ok(repo.graph().getNode('feature-fact'));
  assert.equal(repo.currentBranch(), 'feature');
  assert.ok(result.commits.length >= 1);
});


test('cognitive blame follows merged-parent provenance', () => {
  const repo = tempRepo();
  repo.createBranch('finding');
  repo.checkout('finding');

  let graph = repo.graph();
  graph.addNode({ id: 'merged-finding', kind: 'belief', title: 'Merged provenance', body: 'came from finding branch' });
  repo.writeGraph(graph.snapshot());
  const source = repo.commit('source branch finding');

  repo.checkout('main');
  const merged = repo.merge('finding');
  assert.equal(merged.conflicts.length, 0);

  const history = repo.blame('merged-finding');
  assert.ok(history.some((entry) => entry.commitId === source.id));
});
