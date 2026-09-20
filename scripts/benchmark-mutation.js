// LumenCortex 100k incremental graph mutation benchmark.
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { performance } from 'node:perf_hooks';
import { CognitiveRepository } from '../src/repository.js';

const N=Number(process.env.LUMENCORTEX_MUTATION_BENCH_NODES ?? 100000);
const root=fs.mkdtempSync(path.join(os.tmpdir(),'lcx-mutation-bench-'));
const repo=new CognitiveRepository(root);
repo.init();

let graph=repo.graph();
for(let i=0;i<N;i+=1){
  graph.addNode({
    id:`n_${i}`,
    kind:'evidence',
    title:`Node ${i}`,
    body:`function symbol${i}(){ return ${i}; }`,
    grade:'static',
    trustZone:'repo_trusted',
    metadata:{sourceKind:'file-chunk',path:`src/f${i}.js`}
  });
}
const t0=performance.now();
repo.writeGraph(graph.snapshot());
const initialMs=performance.now()-t0;

graph=repo.graph();
graph.updateNode(`n_${Math.floor(N/2)}`,{body:'function changedSingleNode(){ return 42; }'});
const t1=performance.now();
repo.writeGraph(graph.snapshot());
const singleMutationMs=performance.now()-t1;

const result={nodes:N,initialWriteMs:Number(initialMs.toFixed(2)),singleMutationMs:Number(singleMutationMs.toFixed(2))};
console.log(JSON.stringify(result,null,2));
if(singleMutationMs>750){
  console.error(`single-node mutation too slow: ${singleMutationMs.toFixed(2)}ms > 750ms`);
  process.exit(1);
}
