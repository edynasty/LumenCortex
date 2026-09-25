import test from 'node:test';
import assert from 'node:assert/strict';
import { AttentionEngine, CognitiveGraph } from '../src/index.js';

function graphFixture() {
  const graph = new CognitiveGraph();
  graph.addNode({ id: 'submit', kind: 'entity', title: 'C端提交 application', body: 'submit application validates inventory' });
  graph.addNode({ id: 'accept', kind: 'entity', title: 'B端受理 inventory acceptance', body: 'accept rechecks inventory' });
  graph.addNode({ id: 'lock', kind: 'belief', title: '库存并发锁', body: 'optimistic locking guards concurrent acceptance', grade: 'tested' });
  graph.addNode({ id: 'oauth', kind: 'negative', title: 'OAuth 已排除', body: 'authentication is unrelated to inventory mismatch', grade: 'tested' });
  graph.addNode({ id: 'report', kind: 'entity', title: '财务报表', body: 'monthly revenue report' });
  graph.addEdge({ id: 'e1', from: 'submit', to: 'accept', type: 'affects', weight: 1 });
  graph.addEdge({ id: 'e2', from: 'accept', to: 'lock', type: 'depends_on', weight: 1 });
  graph.addEdge({ id: 'e3', from: 'accept', to: 'oauth', type: 'relates_to', weight: 0.3 });
  return graph.snapshot();
}

test('attention light propagates over relevant graph and respects budget', () => {
  const engine = new AttentionEngine(graphFixture());
  const result = engine.illuminate('为什么C端提交后B端受理库存不一致，并发锁是否有问题', { budgetTokens: 500, maxHops: 3 });
  const ids = result.selectedNodes.map((n) => n.id);
  assert.ok(ids.includes('submit'));
  assert.ok(ids.includes('accept'));
  assert.ok(ids.includes('lock'));
  assert.equal(ids.includes('report'), false);
  assert.ok(result.usedTokens <= 500);
  assert.ok(result.trace.length >= 3);
});

test('multi-light exposes separate exploration policies', () => {
  const lights = new AttentionEngine(graphFixture()).illuminateMulti('库存受理异常', { budgetTokens: 400 });
  assert.ok(lights.exploit.selectedNodes.length > 0);
  assert.ok(lights.explore.selectedNodes.length > 0);
  assert.ok(Array.isArray(lights.contrarian.selectedNodes));
  assert.ok(Array.isArray(lights.anomaly.selectedNodes));
});


test('structural cut stops propagation without deleting either side', () => {
  const graph = new CognitiveGraph();
  graph.addNode({ id: 'seed', kind: 'entity', title: 'critical inventory seed', body: 'critical inventory seed' });
  graph.addNode({ id: 'remote', kind: 'entity', title: 'remote subsystem', body: 'unrelated downstream detail' });
  graph.addEdge({ id: 'link', from: 'seed', to: 'remote', type: 'causes', weight: 1 });

  const before = new AttentionEngine(graph.snapshot()).illuminate('critical inventory seed', {
    budgetTokens: 500,
    maxHops: 2,
    seedNodeIds: ['seed']
  });
  assert.ok(before.selectedNodes.some((node) => node.id === 'remote'));

  graph.cutEdge('link', { reason: 'temporary cognitive amputation' });
  const cut = new AttentionEngine(graph.snapshot()).illuminate('critical inventory seed', {
    budgetTokens: 500,
    maxHops: 2,
    seedNodeIds: ['seed']
  });
  assert.equal(cut.selectedNodes.some((node) => node.id === 'remote'), false);
  assert.ok(graph.getNode('remote'));
  assert.ok(graph.getEdge('link'));

  graph.restoreEdge('link');
  const restored = new AttentionEngine(graph.snapshot()).illuminate('critical inventory seed', {
    budgetTokens: 500,
    maxHops: 2,
    seedNodeIds: ['seed']
  });
  assert.ok(restored.selectedNodes.some((node) => node.id === 'remote'));
});


test('attention max-heap preserves stable insertion order for equal-score seeds', () => {
  const graph = new CognitiveGraph();
  graph.addNode({ id: 'first', kind: 'entity', title: 'first seed', body: 'same' });
  graph.addNode({ id: 'second', kind: 'entity', title: 'second seed', body: 'same' });

  const result = new AttentionEngine(graph.snapshot()).illuminate('same', {
    seedNodeIds: ['first', 'second'],
    budgetTokens: 500,
    maxHops: 0
  });

  assert.deepEqual(
    result.trace.slice(0, 2).map((entry) => entry.nodeId),
    ['first', 'second']
  );
});


test('associative Light is bounded and respects structural cuts', () => {
  const graph = new CognitiveGraph();
  graph.addNode({ id: 'seed', kind: 'entity', title: 'root context', body: 'root context' });
  graph.addNode({ id: 'a', kind: 'entity', title: 'adjacent context', body: 'adjacent context' });
  graph.addNode({ id: 'b', kind: 'entity', title: 'indirect association', body: 'indirect association' });
  graph.addNode({ id: 'c', kind: 'entity', title: 'beyond hop bound', body: 'beyond hop bound' });
  graph.addEdge({ id: 's-a', from: 'seed', to: 'a', type: 'relates_to', weight: 1 });
  graph.addEdge({ id: 'a-b', from: 'a', to: 'b', type: 'relates_to', weight: 1 });
  graph.addEdge({ id: 'b-c', from: 'b', to: 'c', type: 'relates_to', weight: 1 });

  const engine = new AttentionEngine(graph.snapshot());
  const bounded = engine.illuminateAssociative('root context', {
    seedNodeIds: ['seed'],
    candidateNodeIds: ['seed'],
    maxHops: 2,
    associativeNodeLimit: 3,
    budgetTokens: 500,
    pprIterations: 20
  });
  assert.equal(bounded.mode, 'associative');
  assert.equal(bounded.neighborhoodNodeCount, 3);
  assert.ok(bounded.selectedNodes.some((node) => node.id === 'b'));
  assert.equal(bounded.selectedNodes.some((node) => node.id === 'c'), false);
  assert.ok(bounded.usedTokens <= 500);

  graph.cutEdge('a-b', { reason: 'exclude remote branch' });
  const cut = new AttentionEngine(graph.snapshot()).illuminateAssociative('root context', {
    seedNodeIds: ['seed'],
    candidateNodeIds: ['seed'],
    maxHops: 3,
    associativeNodeLimit: 10,
    budgetTokens: 500,
    pprIterations: 20
  });
  assert.equal(cut.selectedNodes.some((node) => node.id === 'b'), false);
  assert.equal(cut.selectedNodes.some((node) => node.id === 'c'), false);
});
