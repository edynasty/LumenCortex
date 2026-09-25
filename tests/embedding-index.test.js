import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import {
  CognitiveRepository,
  LumenCortexRuntime,
  OpenAICompatibleEmbeddingProvider,
  PersistentEmbeddingIndex,
  embeddingRuntimeConfig,
  reciprocalRankFusion
} from '../src/index.js';

function tempRepo(prefix = 'lcx-embedding-') {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), prefix));
  const repo = new CognitiveRepository(root);
  repo.init();
  return { root, repo };
}

class FakeEmbeddingProvider {
  constructor(model = 'fake-embedding-v1') {
    this.model = model;
    this.calls = [];
  }

  async embed(input) {
    const values = Array.isArray(input) ? input : [input];
    this.calls.push([...values]);
    return values.map((value) => vectorFor(value));
  }
}

function vectorFor(value) {
  const text = String(value).toLowerCase();
  if (
    text.includes('inventory') ||
    text.includes('reserve available') ||
    text.includes('warehouse contention') ||
    text.includes('stock contention')
  ) return [1, 0, 0];
  if (text.includes('mail') || text.includes('welcome')) return [0, 1, 0];
  if (text.includes('auth') || text.includes('token')) return [0, 0, 1];
  return [0.2, 0.2, 0.2];
}

test('OpenAI-compatible embedding provider uses /embeddings and restores response order', async () => {
  let request;
  const provider = new OpenAICompatibleEmbeddingProvider({
    baseURL: 'https://example.test/v1',
    apiKey: 'secret',
    model: 'embed-model',
    fetchImpl: async (url, init) => {
      request = {
        url,
        headers: init.headers,
        body: JSON.parse(init.body)
      };
      return new Response(JSON.stringify({
        model: 'embed-model',
        data: [
          { index: 1, embedding: [0, 1] },
          { index: 0, embedding: [1, 0] }
        ]
      }), { status: 200 });
    }
  });

  const vectors = await provider.embed(['first', 'second']);
  assert.equal(request.url, 'https://example.test/v1/embeddings');
  assert.equal(request.headers.Authorization, 'Bearer secret');
  assert.deepEqual(request.body, {
    model: 'embed-model',
    input: ['first', 'second']
  });
  assert.deepEqual(vectors, [[1, 0], [0, 1]]);
});

test('persistent embedding index incrementally reuses unchanged node vectors', async () => {
  const { repo } = tempRepo();
  let graph = repo.graph();
  graph.addNode({
    id: 'inventory',
    kind: 'evidence',
    title: 'Inventory coordinator',
    body: 'reserve available units before acceptance',
    grade: 'static',
    trustZone: 'repo_trusted'
  });
  graph.addNode({
    id: 'mailer',
    kind: 'evidence',
    title: 'Mailer',
    body: 'send welcome mail',
    grade: 'static',
    trustZone: 'repo_trusted'
  });
  repo.writeGraph(graph.snapshot());

  const provider = new FakeEmbeddingProvider();
  const index = new PersistentEmbeddingIndex(repo.dir, {
    provider,
    model: provider.model,
    batchSize: 8
  });

  const first = await index.sync(repo.graph().snapshot(), {
    graphRevision: repo.graphRevision()
  });
  assert.equal(first.embedded, 2);
  assert.equal(first.reused, 0);
  assert.equal(first.count, 2);
  assert.equal(provider.calls.length, 1);
  assert.equal(provider.calls[0].length, 2);

  const second = await index.sync(repo.graph().snapshot(), {
    graphRevision: repo.graphRevision()
  });
  assert.equal(second.embedded, 0);
  assert.equal(second.reused, 2);
  assert.equal(provider.calls.length, 1);

  graph = repo.graph();
  graph.updateNode('inventory', {
    body: 'inventory capacity changed but still reserve available units'
  });
  repo.writeGraph(graph.snapshot());

  const third = await index.sync(repo.graph().snapshot(), {
    graphRevision: repo.graphRevision()
  });
  assert.equal(third.embedded, 1);
  assert.equal(third.reused, 1);
  assert.equal(provider.calls.length, 2);
  assert.equal(provider.calls[1].length, 1);

  const hits = await index.search('warehouse contention', { limit: 2 });
  assert.equal(hits[0].nodeId, 'inventory');
  assert.ok(hits[0].score > hits[1].score);
  assert.ok(hits[0].reasons.includes('embedding-cosine'));
  assert.equal(index.stats().graphRevision, repo.graphRevision());

  index.close();
  repo.close();
});

test('reciprocal rank fusion combines lexical and semantic ranks deterministically', () => {
  const fused = reciprocalRankFusion([
    {
      name: 'lexical',
      weight: 1,
      hits: [
        { nodeId: 'lexical-only', score: 10, reasons: ['fts5'] },
        { nodeId: 'shared', score: 5, reasons: ['fts5'] }
      ]
    },
    {
      name: 'semantic',
      weight: 1,
      hits: [
        { nodeId: 'shared', score: 0.95, reasons: ['embedding-cosine'] },
        { nodeId: 'semantic-only', score: 0.9, reasons: ['embedding-cosine'] }
      ]
    }
  ], { k: 60, limit: 3 });

  assert.equal(fused[0].nodeId, 'shared');
  assert.deepEqual(fused[0].channelRanks, { lexical: 2, semantic: 1 });
  assert.ok(fused[0].reasons.includes('rrf:lexical'));
  assert.ok(fused[0].reasons.includes('rrf:semantic'));
  assert.ok(fused.some((hit) => hit.nodeId === 'lexical-only'));
  assert.ok(fused.some((hit) => hit.nodeId === 'semantic-only'));
});

test('hybrid Runtime retrieval can seed Attention from a pure semantic hit', async () => {
  const { repo } = tempRepo('lcx-hybrid-');
  let graph = repo.graph();
  graph.addNode({
    id: 'inventory',
    kind: 'evidence',
    title: 'Inventory capacity coordinator',
    body: 'reserve available units before acceptance',
    grade: 'static',
    trustZone: 'repo_trusted'
  });
  graph.addNode({
    id: 'mailer',
    kind: 'evidence',
    title: 'Welcome notification sender',
    body: 'send welcome mail after registration',
    grade: 'static',
    trustZone: 'repo_trusted'
  });
  repo.writeGraph(graph.snapshot());

  const provider = new FakeEmbeddingProvider();
  const runtime = new LumenCortexRuntime(repo, {
    embeddingProvider: provider,
    embeddingModel: provider.model
  });

  assert.deepEqual(runtime.search('warehouse contention'), []);

  const context = await runtime.contextHybrid('warehouse contention', {
    candidateLimit: 4,
    maxHops: 0,
    budgetTokens: 1000
  });
  assert.equal(context.mode, 'hybrid');
  assert.equal(context.hybrid.model, provider.model);
  assert.equal(context.hybrid.hits[0].nodeId, 'inventory');
  assert.ok(context.selectedNodes.some((node) => node.id === 'inventory'));
  assert.ok(context.seeds.some((seed) =>
    seed.nodeId === 'inventory' && seed.reason === 'retrieval-seed'
  ));
  assert.equal(runtime.embeddingIndex.stats().graphRevision, repo.graphRevision());

  runtime.close();
  repo.close();
});

test('hybrid Runtime retrieval falls back to weighted Attention when embeddings are unconfigured', async () => {
  const { repo } = tempRepo('lcx-hybrid-fallback-');
  const graph = repo.graph();
  graph.addNode({
    id: 'seed',
    kind: 'entity',
    title: 'fallback context',
    body: 'fallback context'
  });
  repo.writeGraph(graph.snapshot());

  const runtime = new LumenCortexRuntime(repo);
  const result = await runtime.contextHybrid('fallback context', {
    seedNodeIds: ['seed'],
    budgetTokens: 1000
  });
  assert.equal(result.mode, 'weighted');
  assert.equal(result.requestedMode, 'hybrid');
  assert.equal(result.fallbackReason, 'embedding-provider-unavailable');
  assert.ok(result.selectedNodes.some((node) => node.id === 'seed'));

  runtime.close();
  repo.close();
});


test('embedding Runtime config resolves explicit profile and environment credentials', () => {
  const profile = {
    retrieval: {
      embeddings: {
        enabled: true,
        provider: 'generic',
        model: 'embed-configured',
        baseURL: 'http://embedding.test/v1',
        apiKeyEnv: 'CUSTOM_EMBED_KEY',
        headers: { 'X-Embedding': 'yes' },
        timeoutMs: 1234,
        batchSize: 9,
        candidateLimit: 70,
        lexicalLimit: 90,
        semanticLimit: 110,
        semanticMinScore: 0.25,
        rrfK: 41,
        lexicalWeight: 0.7,
        semanticWeight: 1.4
      }
    }
  };
  const configured = embeddingRuntimeConfig(profile, {
    env: {
      CUSTOM_EMBED_KEY: 'secret'
    },
    fetchImpl: async () => {
      throw new Error('network not expected');
    }
  });

  assert.equal(configured.provider.model, 'embed-configured');
  assert.equal(configured.provider.baseURL, 'http://embedding.test/v1');
  assert.equal(configured.provider.apiKey, 'secret');
  assert.equal(configured.provider.headers['X-Embedding'], 'yes');
  assert.equal(configured.provider.timeoutMs, 1234);
  assert.equal(configured.batchSize, 9);
  assert.deepEqual(configured.hybrid, {
    candidateLimit: 70,
    lexicalLimit: 90,
    semanticLimit: 110,
    semanticMinScore: 0.25,
    rrfK: 41,
    lexicalWeight: 0.7,
    semanticWeight: 1.4
  });
});

test('embedding Runtime config is opt-in and requires an explicit model', () => {
  assert.equal(
    embeddingRuntimeConfig({ retrieval: { embeddings: { enabled: false } } }),
    null
  );
  assert.throws(
    () => embeddingRuntimeConfig({
      retrieval: {
        embeddings: {
          enabled: true,
          provider: 'generic',
          baseURL: 'http://embedding.test/v1'
        }
      }
    }),
    /explicit model/
  );
});


test('Runtime rebuilds a model embedding cache once when query dimension drifts', async () => {
  const { repo } = tempRepo('lcx-embedding-drift-');
  const graph = repo.graph();
  graph.addNode({
    id: 'inventory',
    kind: 'evidence',
    title: 'Inventory coordinator',
    body: 'reserve available units',
    grade: 'static',
    trustZone: 'repo_trusted'
  });
  repo.writeGraph(graph.snapshot());

  const provider = {
    model: 'mutable-embedding-model',
    dimension: 2,
    calls: [],
    async embed(input) {
      const values = Array.isArray(input) ? input : [input];
      this.calls.push({ dimension: this.dimension, size: values.length });
      return values.map(() =>
        this.dimension === 2 ? [1, 0] : [1, 0, 0]
      );
    }
  };
  const runtime = new LumenCortexRuntime(repo, {
    embeddingProvider: provider,
    embeddingModel: provider.model
  });

  await runtime.refreshEmbeddingIndex();
  assert.equal(runtime.embeddingIndex.stats().minDimension, 2);
  assert.equal(runtime.embeddingIndex.stats().maxDimension, 2);

  provider.dimension = 3;
  const hits = await runtime.semanticSearch('warehouse contention', { limit: 3 });

  assert.equal(hits[0].nodeId, 'inventory');
  assert.equal(runtime.embeddingIndex.stats().minDimension, 3);
  assert.equal(runtime.embeddingIndex.stats().maxDimension, 3);
  assert.deepEqual(
    provider.calls.map((call) => [call.dimension, call.size]),
    [[2, 1], [3, 1], [3, 1], [3, 1]]
  );

  runtime.close();
  repo.close();
});
