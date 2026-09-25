import test from 'node:test';
import assert from 'node:assert/strict';
import { CognitiveGraph, propagateStaleDependents } from '../src/index.js';

test('transitive invalidation follows only explicit dependency semantics', () => {
  const graph = new CognitiveGraph();
  graph.addNode({ id: 'e', kind: 'evidence', title: 'changed evidence', grade: 'static', trustZone: 'repo_trusted' });
  graph.addNode({ id: 'b1', kind: 'belief', title: 'direct belief', evidenceIds: ['e'] });
  graph.addNode({ id: 'b2', kind: 'belief', title: 'dependent belief' });
  graph.addNode({ id: 'abs', kind: 'abstraction', title: 'derived abstraction', childIds: ['b2'] });
  graph.addNode({ id: 'neg', kind: 'negative', title: 'dependent negative' });
  graph.addNode({ id: 'related', kind: 'belief', title: 'mere relation' });
  graph.addNode({ id: 'archived', kind: 'abstraction', title: 'archived dependent', status: 'archived', childIds: ['e'] });
  graph.addNode({ id: 'invalid', kind: 'belief', title: 'invalid dependent', status: 'invalid', evidenceIds: ['e'] });
  graph.addEdge({ id: 'dep', from: 'b2', to: 'b1', type: 'depends_on' });
  graph.addEdge({ id: 'derive', from: 'neg', to: 'abs', type: 'derived_from' });
  graph.addEdge({ id: 'relation', from: 'related', to: 'e', type: 'relates_to' });

  const dirtied = propagateStaleDependents(graph, ['e'], { reason: 'test-source-changed' });

  assert.deepEqual(new Set(dirtied), new Set(['b1', 'b2', 'abs', 'neg']));
  for (const id of ['b1', 'b2', 'abs', 'neg']) {
    const node = graph.requireNode(id);
    assert.equal(node.status, 'stale');
    assert.equal(node.metadata.staleReason, 'test-source-changed');
    assert.deepEqual(node.metadata.staleSourceIds, ['e']);
    assert.equal(node.metadata.stalePropagation, true);
  }
  assert.equal(graph.requireNode('b1').metadata.staleDepth, 1);
  assert.equal(graph.requireNode('b2').metadata.staleDepth, 2);
  assert.equal(graph.requireNode('abs').metadata.staleDepth, 3);
  assert.equal(graph.requireNode('neg').metadata.staleDepth, 4);
  assert.equal(graph.requireNode('related').status, 'active');
  assert.equal(graph.requireNode('archived').status, 'archived');
  assert.equal(graph.requireNode('invalid').status, 'invalid');
});

test('transitive invalidation traverses stale intermediate nodes without rewriting them', () => {
  const graph = new CognitiveGraph();
  graph.addNode({ id: 'e', kind: 'evidence', title: 'source' });
  graph.addNode({ id: 'already-stale', kind: 'belief', title: 'old belief', status: 'stale', evidenceIds: ['e'], metadata: { staleReason: 'older-reason' } });
  graph.addNode({ id: 'downstream', kind: 'abstraction', title: 'downstream', childIds: ['already-stale'] });
  const versionBefore = graph.requireNode('already-stale').version;

  const dirtied = propagateStaleDependents(graph, ['e'], { reason: 'new-change' });

  assert.deepEqual(dirtied, ['downstream']);
  assert.equal(graph.requireNode('already-stale').version, versionBefore);
  assert.equal(graph.requireNode('already-stale').metadata.staleReason, 'older-reason');
  assert.equal(graph.requireNode('downstream').status, 'stale');
});
