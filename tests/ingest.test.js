import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { CognitiveGraph, ingestWorkspace } from '../src/index.js';

test('ingest creates stable file/chunk hierarchy and dependency edges', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'mw-ingest-'));
  fs.mkdirSync(path.join(dir, 'src'));
  fs.writeFileSync(path.join(dir, 'src', 'b.js'), 'export const value = 1\n');
  fs.writeFileSync(path.join(dir, 'src', 'a.js'), "import { value } from './b.js'\nexport const x = value + 1\n");

  const first = ingestWorkspace(new CognitiveGraph().snapshot(), dir, { chunkLines: 10 });
  assert.equal(first.stats.files, 2);
  const files = Object.values(first.graph.nodes).filter((n) => n.metadata?.sourceKind === 'file');
  assert.equal(files.length, 2);
  const deps = Object.values(first.graph.edges).filter((e) => e.type === 'depends_on');
  assert.equal(deps.length, 1);

  const second = ingestWorkspace(first.graph, dir, { chunkLines: 10 });
  assert.equal(second.stats.changedEvidence, 0);
  assert.equal(Object.keys(second.graph.nodes).length, Object.keys(first.graph.nodes).length);
  assert.deepEqual(second.graph, first.graph);
});

test('changed source dirties beliefs that cite changed evidence', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'mw-ingest-change-'));
  const file = path.join(dir, 'service.js');
  fs.writeFileSync(file, 'export const phase = "SUBMIT"\n');
  let result = ingestWorkspace(new CognitiveGraph().snapshot(), dir, { chunkLines: 10 });
  const evidence = Object.values(result.graph.nodes).find((n) => n.metadata?.sourceKind === 'file-chunk');
  const graph = new CognitiveGraph(result.graph);
  graph.addNode({
    id: 'belief', kind: 'belief', title: 'phase belief', body: 'phase is submit',
    grade: 'static', evidenceIds: [evidence.id]
  });
  fs.writeFileSync(file, 'export const phase = "ACCEPT"\n');
  result = ingestWorkspace(graph.snapshot(), dir, { chunkLines: 10 });
  assert.ok(result.changedEvidence.includes(evidence.id));
  assert.equal(result.graph.nodes.belief.status, 'stale');
});


test('changed source transitively dirties higher-level cognition', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'mw-ingest-transitive-'));
  const file = path.join(dir, 'service.js');
  fs.writeFileSync(file, 'export const phase = "SUBMIT"\n');

  let result = ingestWorkspace(new CognitiveGraph().snapshot(), dir, { chunkLines: 10 });
  const evidence = Object.values(result.graph.nodes)
    .find((node) => node.metadata?.sourceKind === 'file-chunk');
  const graph = new CognitiveGraph(result.graph);
  graph.addNode({
    id: 'direct-belief',
    kind: 'belief',
    title: 'Direct phase belief',
    evidenceIds: [evidence.id]
  });
  graph.addNode({
    id: 'phase-summary',
    kind: 'abstraction',
    title: 'Phase summary',
    childIds: ['direct-belief']
  });
  graph.addNode({
    id: 'downstream-belief',
    kind: 'belief',
    title: 'Downstream decision'
  });
  graph.addEdge({
    id: 'downstream-dependency',
    from: 'downstream-belief',
    to: 'phase-summary',
    type: 'depends_on'
  });

  fs.writeFileSync(file, 'export const phase = "ACCEPT"\n');
  result = ingestWorkspace(graph.snapshot(), dir, { chunkLines: 10 });

  assert.equal(result.graph.nodes['direct-belief'].status, 'stale');
  assert.equal(result.graph.nodes['phase-summary'].status, 'stale');
  assert.equal(result.graph.nodes['downstream-belief'].status, 'stale');
  assert.ok(result.dirtiedBeliefs.includes('downstream-belief'));
  assert.deepEqual(
    result.graph.nodes['downstream-belief'].metadata.staleSourceIds,
    [evidence.id]
  );
});
