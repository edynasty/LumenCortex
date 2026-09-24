import test from 'node:test';
import assert from 'node:assert/strict';

import { CognitiveGraph } from '../src/graph.js';
import {
  GraphGovernor,
  GraphGovernorAnalyzer,
  validateGraphGovernorPlan
} from '../src/graph-governor.js';

function graphFixture() {
  const graph = new CognitiveGraph();
  graph.addNode({
    id: 'a',
    kind: 'belief',
    title: 'Provider Architecture',
    body: 'A',
    status: 'active',
    grade: 'tested',
    trustZone: 'repo_trusted',
    tags: ['provider']
  });
  graph.addNode({
    id: 'b',
    kind: 'belief',
    title: 'provider architecture',
    body: 'B',
    status: 'active',
    grade: 'static',
    trustZone: 'model_inferred',
    tags: ['provider']
  });
  graph.addNode({
    id: 'c',
    kind: 'evidence',
    title: 'Old hypothesis',
    body: 'C',
    status: 'stale',
    grade: 'hypothesis',
    trustZone: 'model_inferred',
    tags: ['provider']
  });
  graph.addNode({
    id: 'd',
    kind: 'evidence',
    title: 'Runtime proof',
    body: 'D',
    status: 'stale',
    grade: 'reproduced',
    trustZone: 'runtime_verified'
  });
  graph.addEdge({
    id: 'e1',
    from: 'a',
    to: 'b',
    type: 'contradicts',
    weight: 0.9
  });
  return graph;
}

test('Graph Governor analyzer produces governance candidates without mutating the graph', () => {
  const graph = graphFixture();
  const before = graph.snapshot();
  const analyzer = new GraphGovernorAnalyzer({ promotionMinGroup: 3, archiveThreshold: 0.5 });
  const result = analyzer.analyze(before);

  assert.equal(result.metrics.nodeCount, 4);
  assert.equal(result.metrics.contradictionCount, 1);
  assert.ok(result.candidates.canonicalize.some((group) => group.nodeIds.includes('a') && group.nodeIds.includes('b')));
  assert.ok(result.candidates.promote.some((group) => group.tag === 'provider'));
  assert.ok(result.candidates.archive.some((item) => item.nodeId === 'c'));
  assert.equal(result.candidates.archive.some((item) => item.nodeId === 'd'), false);
  assert.deepEqual(graph.snapshot(), before);
});

test('Graph Governor validator blocks automatic archive of reproduced evidence', () => {
  const graph = graphFixture().snapshot();
  const result = validateGraphGovernorPlan({ archive: ['d'] }, graph);
  assert.equal(result.valid, false);
  assert.match(result.errors.join(' '), /high-grade evidence/);
});

test('Graph Governor safe executor only applies tiers and archival actions', () => {
  const graph = graphFixture();
  const repository = {
    _graph: graph.snapshot(),
    graph() {
      return new CognitiveGraph(this._graph);
    },
    writeGraph(next) {
      this._graph = structuredClone(next);
    },
    commit(message, options) {
      return { id: 'gov-commit', message, options };
    }
  };
  const governor = new GraphGovernor({ repository });
  const result = governor.applySafe({
    archive: ['c'],
    tiers: {
      hot: ['a'],
      warm: ['b'],
      cold: ['c', 'd']
    }
  });

  assert.equal(result.applied, true);
  assert.equal(result.commit.id, 'gov-commit');
  assert.equal(repository._graph.nodes.c.status, 'archived');
  assert.equal(repository._graph.nodes.a.metadata.storageTier, 'hot');
  assert.equal(repository._graph.nodes.b.metadata.storageTier, 'warm');
});
