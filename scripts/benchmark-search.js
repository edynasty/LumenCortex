import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { performance } from 'node:perf_hooks';
import { PersistentSearchIndex } from '../src/search-index.js';

const count=Number(process.env.MODELWEAVE_SEARCH_BENCH_NODES ?? 100000);
const queries=Number(process.env.MODELWEAVE_SEARCH_BENCH_QUERIES ?? 50);
const dir=fs.mkdtempSync(path.join(os.tmpdir(),'mw-search-bench-'));
const graph={nodes:{},edges:{}};

for(let i=0;i<count;i+=1){
  const id='chunk_'+String(i).padStart(6,'0');
  graph.nodes[id]={
    id,
    kind:'evidence',
    title:`src/service/InventoryService${i}.java:L1-L20`,
    body:`public class InventoryService${i} { public void reserveItem${i}(String sku) { validateStock${i}(sku); } }`,
    grade:'static',
    trustZone:'repo_trusted',
    status:'active',
    metadata:{sourceKind:'file-chunk',path:`src/service/InventoryService${i}.java`,startLine:1,endLine:20}
  };
}

const index=new PersistentSearchIndex(dir);
const buildStart=performance.now();
const stats=index.build(graph);
const buildMs=performance.now()-buildStart;
const samples=[];
for(let q=0;q<queries;q+=1){
  const n=(q*7919)%count;
  const start=performance.now();
  const result=index.search(`reserveItem${n}`,{limit:10});
  samples.push(performance.now()-start);
  if(result[0]?.nodeId!==('chunk_'+String(n).padStart(6,'0'))){
    throw new Error(`wrong top hit for reserveItem${n}: ${result[0]?.nodeId}`);
  }
}
samples.sort((a,b)=>a-b);
const p50=samples[Math.floor(samples.length*0.50)] ?? 0;
const p95=samples[Math.min(samples.length-1,Math.floor(samples.length*0.95))] ?? 0;
const sizeBytes=fs.statSync(index.file).size;
const result={
  nodes:count,
  queries,
  buildMs:Number(buildMs.toFixed(2)),
  queryP50Ms:Number(p50.toFixed(3)),
  queryP95Ms:Number(p95.toFixed(3)),
  indexSizeMB:Number((sizeBytes/1024/1024).toFixed(2)),
  terms:stats.termCount,
  symbols:stats.symbolCount
};
console.log(JSON.stringify(result,null,2));

if(buildMs>60000) throw new Error(`index build too slow: ${buildMs.toFixed(0)}ms`);
if(p95>100) throw new Error(`query p95 too slow: ${p95.toFixed(2)}ms`);
