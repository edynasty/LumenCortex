import test from 'node:test';
import assert from 'node:assert/strict';
import { CognitiveGraph, GRAPH_MUTATION_HINTS, applyDiff, diffGraphs, emptyGraph, invertDiff } from '../src/index.js';

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


test('structural cut preserves edge and can be restored or versioned as a diff', () => {
  const graph = sampleGraph();
  const before = graph.snapshot();

  const cut = graph.cutEdge('x1', { reason: 'isolate disproven causal path' });
  assert.equal(cut.metadata.attentionCut, true);
  assert.equal(graph.getEdge('x1').metadata.cutReason, 'isolate disproven causal path');
  assert.equal(graph.neighbors('b1').length, 1, 'cut is non-destructive');

  const cutState = graph.snapshot();
  const diff = diffGraphs(before, cutState);
  assert.equal(diff.operations.length, 1);
  assert.equal(diff.operations[0].type, 'put_edge');
  assert.deepEqual(applyDiff(cutState, invertDiff(diff)), before);

  const restored = graph.restoreEdge('x1');
  assert.equal(restored.metadata.attentionCut, undefined);
});

test('explicit graft creates a marked structural edge without copying nodes', () => {
  const graph = sampleGraph();
  graph.addNode({ id: 'task2', kind: 'task', title: 'Acceptance investigation' });
  const graft = graph.graftEdge(
    { id: 'g1', from: 'task2', to: 'b1', type: 'relates_to', weight: 0.8 },
    { reason: 'reuse inventory finding' }
  );
  assert.equal(graft.metadata.grafted, true);
  assert.equal(graft.metadata.graftReason, 'reuse inventory finding');
  assert.ok(graph.getNode('b1'));
});


test('graph snapshots carry non-enumerable mutation hints for incremental persistence', () => {
  const graph=new CognitiveGraph();
  graph.addNode({id:'a',kind:'belief',title:'A',body:'one'});
  graph.addNode({id:'b',kind:'belief',title:'B',body:'two'});
  graph.addEdge({id:'e',from:'a',to:'b',type:'relates_to'});
  graph.updateNode('a',{body:'changed'});
  graph.removeEdge('e');
  graph.removeNode('b');

  const snapshot=graph.snapshot();
  const hints=snapshot[GRAPH_MUTATION_HINTS];
  assert.deepEqual(hints.changedNodeIds,['a']);
  assert.deepEqual(hints.removedNodeIds,['b']);
  assert.deepEqual(hints.changedEdgeIds,[]);
  assert.deepEqual(hints.removedEdgeIds,['e']);
  assert.equal(Object.getOwnPropertyDescriptor(snapshot,GRAPH_MUTATION_HINTS).enumerable,false);
  assert.equal(JSON.stringify(snapshot).includes('changedNodeIds'),false);
});


test('structurally shared snapshot keeps old node values after later graph replacement updates', () => {
  const graph=new CognitiveGraph();
  graph.addNode({id:'stable',kind:'belief',title:'Stable',body:'before',metadata:{nested:'before'}});
  const before=graph.snapshot();

  graph.updateNode('stable',{body:'after',metadata:{nested:'after'}});
  const after=graph.snapshot();

  assert.equal(before.nodes.stable.body,'before');
  assert.equal(before.nodes.stable.metadata.nested,'before');
  assert.equal(after.nodes.stable.body,'after');
  assert.equal(after.nodes.stable.metadata.nested,'after');
  assert.notEqual(before.nodes,after.nodes);
  assert.notEqual(before.nodes.stable,after.nodes.stable);
});
