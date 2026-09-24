import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

import { CognitiveGraph } from '../src/graph.js';
import { CognitiveRepository } from '../src/repository.js';
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


test('semantic Governor apply canonicalizes without deleting provenance', () => {
  const graph = graphFixture();
  const repository = {
    _graph: graph.snapshot(),
    graph() {
      return new CognitiveGraph(this._graph);
    },
    writeGraph(next) {
      this._graph = structuredClone(next);
    },
    commit() {
      throw new Error('commit not expected');
    }
  };
  const governor = new GraphGovernor({ repository });

  const safeOnly = governor.applyPlan({
    canonicalize: [
      { canonical: 'a', aliases: ['b'], reason: 'same provider architecture concept' }
    ]
  }, { semantic: false, commit: false });

  assert.equal(safeOnly.deferred.canonicalize.length, 1);
  assert.equal(repository._graph.nodes.b.metadata.canonicalNodeId, undefined);

  const semantic = governor.applyPlan({
    canonicalize: [
      { canonical: 'a', aliases: ['b'], reason: 'same provider architecture concept' }
    ]
  }, { semantic: true, commit: false });

  assert.equal(semantic.applied, true);
  assert.equal(repository._graph.nodes.b.metadata.canonicalNodeId, 'a');
  assert.ok(repository._graph.nodes.a);
  assert.ok(repository._graph.nodes.b);

  const edge = Object.values(repository._graph.edges)
    .find((item) => item.type === 'canonicalizes' && item.from === 'a' && item.to === 'b');
  assert.ok(edge);
  assert.equal(edge.metadata.governor, true);
});

test('Cortex Epoch records governance metadata and is reversible through Cognitive Git', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'lcx-cortex-epoch-'));
  const repo = new CognitiveRepository(root);
  try {
    repo.init();
    repo.writeGraph(graphFixture().snapshot());
    const baseline = repo.commit('fixture baseline');

    const governor = new GraphGovernor({ repository: repo });
    const result = governor.applyPlan({
      canonicalize: [
        { canonical: 'a', aliases: ['b'], reason: 'same provider architecture concept' }
      ],
      epoch: {
        proposed: true,
        reasons: ['canonicalization-backlog']
      },
      summary: 'Canonicalize duplicate provider architecture beliefs.'
    }, {
      semantic: true,
      createEpoch: true
    });

    assert.ok(result.epoch?.id);
    assert.equal(result.epoch.rollbackTarget, baseline.id);
    assert.equal(result.commit.metadata.epoch.id, result.epoch.id);
    assert.equal(repo.graph().getNode('b').metadata.canonicalNodeId, 'a');
    assert.equal(repo.graph().getNode(result.epoch.id).metadata.cortexEpoch, true);
    assert.ok(Object.values(repo.graph().snapshot().edges).some((edge) =>
      edge.type === 'canonicalizes' && edge.from === 'a' && edge.to === 'b'
    ));

    const reverted = repo.revert(result.commit.id);
    assert.deepEqual(reverted.conflicts, []);
    assert.equal(repo.graph().getNode(result.epoch.id), undefined);
    assert.equal(repo.graph().getNode('b').metadata.canonicalNodeId, undefined);
    assert.equal(Object.values(repo.graph().snapshot().edges).some((edge) => edge.type === 'canonicalizes'), false);
  } finally {
    repo.close();
  }
});


test('semantic Governor apply preserves competing branches and promotes global abstractions', () => {
  const graph = graphFixture();
  const repository = {
    _graph: graph.snapshot(),
    graph() {
      return new CognitiveGraph(this._graph);
    },
    writeGraph(next) {
      this._graph = structuredClone(next);
    },
    commit() {
      throw new Error('commit not expected');
    }
  };
  const governor = new GraphGovernor({ repository });

  const result = governor.applyPlan({
    branch: [
      { from: 'a', to: 'b', reason: 'Both provider architecture hypotheses remain plausible.' }
    ],
    promote: [
      { title: 'Provider Architecture Overview', childIds: ['a', 'b', 'e'], reason: 'Shared provider cluster.' }
    ]
  }, {
    semantic: true,
    commit: false
  });

  assert.equal(result.deferred.branch.length, 0);
  assert.equal(result.deferred.promote.length, 0);

  const nodes = Object.values(repository._graph.nodes);
  const branch = nodes.find((node) => node.metadata?.governorBranch === true);
  const promotion = nodes.find((node) => node.metadata?.globalPromotion === true);

  assert.ok(branch);
  assert.deepEqual(branch.childIds, ['a', 'b']);
  assert.ok(promotion);
  assert.deepEqual(promotion.childIds, ['a', 'b', 'e']);

  assert.ok(repository._graph.nodes.a);
  assert.ok(repository._graph.nodes.b);
  assert.ok(repository._graph.nodes.e);

  const branchEdges = Object.values(repository._graph.edges)
    .filter((edge) => edge.metadata?.cognitiveBranch === true);
  const promotionEdges = Object.values(repository._graph.edges)
    .filter((edge) => edge.metadata?.globalPromotion === true);

  assert.equal(branchEdges.length, 2);
  assert.equal(promotionEdges.length, 3);
});


test('Governor tier value can use storage access telemetry without changing evidence quality', () => {
  const graph = new CognitiveGraph();
  graph.addNode({
    id: 'recent',
    kind: 'entity',
    title: 'Recent context',
    status: 'active',
    grade: 'static',
    trustZone: 'repo_trusted'
  });
  graph.addNode({
    id: 'quiet',
    kind: 'entity',
    title: 'Quiet context',
    status: 'active',
    grade: 'static',
    trustZone: 'repo_trusted'
  });

  const now = Date.parse('2026-09-25T00:00:00.000Z');
  const analysis = new GraphGovernorAnalyzer().analyze(graph.snapshot(), {
    now,
    storageByNode: {
      recent: {
        tier: 'warm',
        accessCount: 64,
        lastAccessAt: '2026-09-24T23:59:00.000Z'
      },
      quiet: {
        tier: 'warm',
        accessCount: 0,
        lastAccessAt: null
      }
    }
  });

  assert.ok(analysis.values.recent > analysis.values.quiet);
  assert.equal(graph.getNode('recent').grade, 'static');
  assert.equal(graph.getNode('quiet').grade, 'static');
});
