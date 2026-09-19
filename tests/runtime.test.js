import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import {
  CognitiveRepository,
  ModelWeaveRuntime,
  auditEvidence,
  promoteNodes
} from '../src/index.js';

function runtimeRepo() {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'modelweave-runtime-'));
  const repo = new CognitiveRepository(dir);
  repo.init();
  return repo;
}

test('promotion keeps child detail and creates drill-down edges', () => {
  const repo = runtimeRepo();
  let graph = repo.graph();
  graph.addNode({ id: 'a', kind: 'entity', title: 'Inventory validation', body: 'detailed A' });
  graph.addNode({ id: 'b', kind: 'entity', title: 'Inventory locking', body: 'detailed B' });
  const result = promoteNodes(graph.snapshot(), ['a', 'b'], { id: 'abs', title: 'Inventory consistency' });
  assert.equal(result.graph.nodes.a.body, 'detailed A');
  assert.equal(result.graph.nodes.b.body, 'detailed B');
  assert.deepEqual(result.graph.nodes.abs.childIds.sort(), ['a', 'b']);
  assert.equal(Object.values(result.graph.edges).filter((e) => e.type === 'abstracts').length, 2);
});

test('evidence expiry dirties dependent belief', () => {
  const repo = runtimeRepo();
  const graph = repo.graph();
  graph.addNode({
    id: 'e', kind: 'evidence', title: 'runtime trace', body: 'ok', grade: 'runtime',
    trustZone: 'runtime_verified', observedAt: '2020-01-01T00:00:00.000Z', ttlMs: 1000
  });
  graph.addNode({ id: 'b', kind: 'belief', title: 'auth is healthy', grade: 'runtime', evidenceIds: ['e'] });
  const audit = auditEvidence(graph.snapshot(), Date.now());
  assert.equal(audit.graph.nodes.e.status, 'stale');
  assert.equal(audit.graph.nodes.b.status, 'stale');
});

test('runtime commits structured worker output atomically', async () => {
  const repo = runtimeRepo();
  let graph = repo.graph();
  graph.addNode({
    id: 'e1', kind: 'evidence', title: 'Service code', body: 'submit validates only', grade: 'static', trustZone: 'repo_trusted'
  });
  repo.writeGraph(graph.snapshot());
  repo.commit('seed source');

  const runtime = new ModelWeaveRuntime(repo);
  const result = await runtime.execute('determine submit inventory behavior', {
    async reason() {
      return {
        summary: 'submission validates but does not deduct',
        operations: [
          {
            type: 'add_node',
            node: {
              id: 'belief1', kind: 'belief', title: 'Submit does not deduct', body: 'No inventory deduction on submit',
              grade: 'static', evidenceIds: ['e1']
            }
          },
          { type: 'add_edge', edge: { id: 'edge1', from: 'belief1', to: 'e1', type: 'derived_from' } }
        ]
      };
    }
  });
  assert.ok(result.commit?.id);
  assert.equal(repo.graph().getNode('belief1').body, 'No inventory deduction on submit');
});

test('runtime rolls back invalid model-inferred evidence', async () => {
  const repo = runtimeRepo();
  const before = repo.graph().snapshot();
  const runtime = new ModelWeaveRuntime(repo);
  await assert.rejects(
    runtime.execute('bad evidence', {
      async reason() {
        return {
          operations: [
            { type: 'add_node', node: { id: 'fake', kind: 'evidence', title: 'fake', trustZone: 'model_inferred' } }
          ]
        };
      }
    }),
    /cannot use model_inferred/
  );
  assert.deepEqual(repo.graph().snapshot(), before);
});
