import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { CognitiveRepository } from '../src/repository.js';
import { LumenCortexRuntime } from '../src/runtime.js';

test('persistent search index ranks exact code symbols and seeds Attention Light', () => {
  const root=fs.mkdtempSync(path.join(os.tmpdir(),'mw-search-'));
  const repo=new CognitiveRepository(root);
  repo.init();
  const graph=repo.graph();
  graph.addNode({
    id:'inventory_chunk',
    kind:'evidence',
    title:'src/InventoryService.java:L1-L20',
    body:'public class InventoryService { public void reserveInventory(String sku) { validateStock(sku); } }',
    grade:'static',trustZone:'repo_trusted',
    metadata:{sourceKind:'file-chunk',path:'src/InventoryService.java',startLine:1,endLine:20}
  });
  graph.addNode({
    id:'gateway_chunk',
    kind:'evidence',
    title:'src/Gateway.java:L1-L20',
    body:'public class Gateway { public void routeRequest() {} }',
    grade:'static',trustZone:'repo_trusted',
    metadata:{sourceKind:'file-chunk',path:'src/Gateway.java',startLine:1,endLine:20}
  });
  repo.writeGraph(graph.snapshot());

  const runtime=new LumenCortexRuntime(repo);
  const stats=runtime.refreshSearchIndex();
  assert.equal(stats.documentCount >= 2,true);

  const hits=runtime.search('reserveInventory');
  assert.equal(hits[0].nodeId,'inventory_chunk');
  assert.ok(hits[0].reasons.some(x=>x.startsWith('symbol:')));

  const ctx=runtime.context('where is reserveInventory implemented',{budgetTokens:2000});
  assert.ok(ctx.selectedNodes.some(n=>n.id==='inventory_chunk'));
  assert.ok(runtime.searchIndex.stats().termCount>0);
});


test('runtime automatically refreshes a stale search index after graph mutation', () => {
  const root=fs.mkdtempSync(path.join(os.tmpdir(),'mw-search-revision-'));
  const repo=new CognitiveRepository(root);
  repo.init();
  const runtime=new LumenCortexRuntime(repo);
  runtime.refreshSearchIndex();
  const before=runtime.searchIndex.stats().graphRevision;

  const graph=repo.graph();
  graph.addNode({
    id:'late_symbol',
    kind:'evidence',
    title:'src/Late.java:L1-L10',
    body:'public class Late { public void newlyAddedSymbol() {} }',
    grade:'static',
    trustZone:'repo_trusted',
    metadata:{sourceKind:'file-chunk',path:'src/Late.java',startLine:1,endLine:10}
  });
  repo.writeGraph(graph.snapshot());

  const hits=runtime.search('newlyAddedSymbol');
  assert.equal(hits[0].nodeId,'late_symbol');
  assert.ok(runtime.searchIndex.stats().graphRevision>before);
  assert.equal(runtime.searchIndex.stats().graphRevision,repo.graphRevision());
});
