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
