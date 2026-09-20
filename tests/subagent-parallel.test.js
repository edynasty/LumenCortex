import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { CognitiveRepository } from '../src/repository.js';
import { LumenCortexRuntime } from '../src/runtime.js';
import { ToolRegistry } from '../src/tools.js';
import { SubagentPool } from '../src/subagent.js';
import { ParallelSessionRunner } from '../src/parallel.js';

test('subagents run as distinct durable sessions and parallel runner aggregates them', async () => {
  const root=fs.mkdtempSync(path.join(os.tmpdir(),'mw-subagents-'));
  const repo=new CognitiveRepository(root);
  repo.init();
  const runtime=new LumenCortexRuntime(repo);
  let active=0;
  let maxActive=0;
  const provider={
    model:'mock-subagent',
    async complete({messages}){
      active+=1;
      maxActive=Math.max(maxActive,active);
      await new Promise(resolve=>setTimeout(resolve,40));
      const user=[...messages].reverse().find(m=>m.role==='user');
      active-=1;
      return {message:{role:'assistant',content:'finding:'+user.content},finishReason:'stop',usage:{total_tokens:3}};
    }
  };
  const pool=new SubagentPool({
    provider,repository:repo,runtime,workspace:root,tools:new ToolRegistry(),concurrency:2
  });
  const runner=new ParallelSessionRunner({subagentPool:pool,concurrency:2});
  const result=await runner.runTasks([
    {goal:'inspect auth',role:'security'},
    {goal:'inspect inventory',role:'domain'},
    {goal:'inspect database',role:'data'}
  ]);
  assert.equal(result.length,3);
  assert.ok(result.every(x=>x.ok));
  assert.equal(new Set(result.map(x=>x.sessionId)).size,3);
  assert.ok(maxActive>=2, `expected real overlap, maxActive=${maxActive}`);
  assert.match(result[0].final,/inspect auth/);
  assert.equal(repo.graph().findNodes(n=>n.kind==='task'&&n.tags?.includes('agent-session')).length,0);
});
