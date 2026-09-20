import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { CognitiveRepository, ModelWeaveRuntime, PromotionController } from '../src/index.js';

test('active promotion creates a reusable parent without deleting child detail', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'mw-promotion-controller-'));
  const repo = new CognitiveRepository(dir);
  repo.init();

  const graph = repo.graph();
  const selectedNodes = [];
  for (let i = 0; i < 12; i += 1) {
    const node = graph.addNode({
      id: `detail_${i}`,
      kind: 'belief',
      title: `Inventory detail ${i}`,
      body: `detail body ${i}`,
      unresolved: i < 4 ? [`question-${i}`] : [],
      grade: 'hypothesis',
      trustZone: 'model_inferred'
    });
    selectedNodes.push({ ...node, activation: 1 - i * 0.03 });
  }
  repo.writeGraph(graph.snapshot());

  const runtime = new ModelWeaveRuntime(repo);
  const controller = new PromotionController(runtime, {
    pressureThreshold: 0.7,
    nodeThreshold: 8,
    unresolvedThreshold: 4,
    minChildren: 3,
    maxChildren: 6,
    cooldownSteps: 0
  });

  const context = {
    selectedNodes,
    selectedEdges: [],
    usedTokens: 850,
    budgetTokens: 1000
  };

  const first = controller.maybePromote('investigate inventory consistency', context, { step: 1 });
  assert.equal(first.promoted, true);
  assert.equal(first.abstraction.kind, 'abstraction');
  assert.equal(first.abstraction.childIds.length, 6);
  assert.equal(first.abstraction.metadata.automaticPromotion, true);

  const after = repo.graph();
  for (const id of first.abstraction.childIds) {
    assert.ok(after.getNode(id), `child ${id} must remain available for drill-down`);
  }

  const second = controller.maybePromote('investigate inventory consistency', context, { step: 2 });
  assert.equal(second.promoted, false);
  assert.equal(second.deduplicated, true);
  assert.equal(second.existing.id, first.abstraction.id);
});


test('active promotion can trigger from repeated activation before token pressure is high', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'mw-promotion-reuse-'));
  const repo = new CognitiveRepository(dir);
  repo.init();

  const graph = repo.graph();
  const selectedNodes = [];
  const activationCounts = {};
  for (let i = 0; i < 5; i += 1) {
    const node = graph.addNode({
      id: `reuse_${i}`,
      kind: 'evidence',
      title: `Repeated evidence ${i}`,
      body: `evidence ${i}`,
      grade: 'static',
      trustZone: 'repo_trusted'
    });
    selectedNodes.push({ ...node, activation: 0.8 - i * 0.02 });
    activationCounts[node.id] = 4;
  }
  repo.writeGraph(graph.snapshot());

  const runtime = new ModelWeaveRuntime(repo);
  const controller = new PromotionController(runtime, {
    pressureThreshold: 0.95,
    nodeThreshold: 99,
    unresolvedThreshold: 99,
    reuseThreshold: 3,
    reuseNodeThreshold: 4,
    minChildren: 3,
    cooldownSteps: 0
  });

  const result = controller.maybePromote('reused inventory evidence', {
    selectedNodes,
    selectedEdges: [],
    usedTokens: 200,
    budgetTokens: 1000
  }, { step: 4, activationCounts });

  assert.equal(result.promoted, true);
  assert.ok(result.assessment.reasons.includes('repeated-activation'));
  assert.equal(result.abstraction.childIds.length, 5);
});
