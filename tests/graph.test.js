import test from 'node:test';
import assert from 'node:assert/strict';
import { CognitiveGraph, applyDiff, diffGraphs, emptyGraph, invertDiff } from '../src/index.js';

function sampleGraph() {
  const graph = new CognitiveGraph();
  const evidence = graph.addNode({
    id: 'e1', kind: 'evidence', title: 'ResourceService.java', body: 'submit validates only',
    trustZone: 'repo_trusted', grade: 'static'
  });
  const belief = graph.addNode({
    id: 'b1', kind: 'belief', title: 'Submission inventory semantics', body: 'submit does not deduct inventory',
    evidenceIds: [evidence.id], grade: 'static'
  });
  graph.addEdge({ id: 'x1', from: belief.id, to: evidence.id, type: 'derived_from' });
  return graph;
}

test('graph enforces references and removes attached edges with node', () => {
  const graph = sampleGraph();
  assert.equal(graph.neighbors('b1').length, 1);
  graph.removeNode('e1');
  assert.equal(graph.getEdge('x1'), undefined);
  assert.equal(graph.requireNode('b1').status, 'stale');
  assert.deepEqual(graph.requireNode('b1').evidenceIds, []);
  assert.equal(graph.validate(), true);
});

test('graph diff is reversible', () => {
  const before = emptyGraph();
  const graph = sampleGraph().snapshot();
  const diff = diffGraphs(before, graph);
  const applied = applyDiff(before, diff);
  assert.deepEqual(applied, graph);
  const reverted = applyDiff(applied, invertDiff(diff));
  assert.deepEqual(reverted, before);
});
