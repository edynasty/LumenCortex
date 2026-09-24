import test from 'node:test';
import assert from 'node:assert/strict';

import { CognitiveGraph } from '../src/graph.js';
import {
  GraphGovernor,
  GraphGovernorAnalyzer,
  LLMGraphGovernorCurator,
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
  graph.addNode({
    id: 'e',
    kind: 'entity',
    title: 'Provider Registry',
    body: 'E',
    status: 'active',
    grade: 'static',
    trustZone: 'repo_trusted',
    tags: ['provider']
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

  assert.equal(result.metrics.nodeCount, 5);
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


test('model-backed Graph Governor Curator proposes a validated plan without mutating the graph', async () => {
  const graph = graphFixture();
  const before = graph.snapshot();
  let request;

  const provider = {
    model: 'governor-reasoner',
    async complete(input) {
      request = input;
      return {
        message: {
          role: 'assistant',
          content: JSON.stringify({
            archive: ['c'],
            canonicalize: [
              { canonical: 'a', aliases: ['b'], reason: 'same concept title' }
            ],
            branch: [
              { from: 'a', to: 'b', reason: 'preserve contradiction' }
            ],
            promote: [
              { title: 'Provider model architecture', childIds: ['a', 'b', 'e'], reason: 'shared provider cluster' }
            ],
            epoch: { proposed: false, reasons: [] },
            summary: 'Curate provider architecture cluster.'
          })
        },
        finishReason: 'stop'
      };
    }
  };

  const repository = {
    _graph: before,
    graph() {
      return new CognitiveGraph(this._graph);
    },
    writeGraph(next) {
      this._graph = structuredClone(next);
    },
    commit() {
      throw new Error('propose must not commit');
    }
  };

  const curator = new LLMGraphGovernorCurator({
    provider,
    reasoningEffort: 'high',
    maxTokens: 4000
  });
  const governor = new GraphGovernor({
    repository,
    analyzer: new GraphGovernorAnalyzer({ promotionMinGroup: 3, archiveThreshold: 0.5 }),
    curator
  });

  const result = await governor.propose();

  assert.equal(result.validation.valid, true);
  assert.equal(result.curator.model, 'governor-reasoner');
  assert.deepEqual(result.plan.archive, ['c']);
  assert.equal(result.plan.canonicalize[0].canonical, 'a');
  assert.deepEqual(result.plan.promote[0].childIds, ['a', 'b', 'e']);
  assert.equal(request.reasoningEffort, 'high');

  const payload = JSON.parse(request.messages[1].content);
  assert.equal(payload.objective, 'Propose long-horizon graph maintenance. Do not execute changes.');
  assert.equal(payload.nodes.a.title, 'Provider Architecture');
  assert.equal(payload.nodes.d, undefined);
  assert.deepEqual(repository._graph, before);
});
