import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { AgentLoop, AgentMaxStepsError } from '../src/agent.js';
import { ToolRegistry } from '../src/tools.js';

function fixture() {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'mw-agent-'));
  const repoDir = path.join(root, '.modelweave');
  fs.mkdirSync(repoDir, { recursive: true });
  const graphState = { version: 1, nodes: {}, edges: {}, metadata: {} };
  const repository = {
    dir: repoDir,
    _graph: graphState,
    graph() {
      return {
        snapshot: () => structuredClone(this._graph),
        getNode: () => undefined,
        addNode: (node) => { this._graph.nodes[node.id] = node; return node; },
        putNode: (node) => { this._graph.nodes[node.id] = node; return node; }
      };
    },
    writeGraph(graph) { this._graph = structuredClone(graph); },
    commit() { return { id: 'commit1' }; }
  };
  const runtime = {
    context: () => ({ selectedNodes: [], selectedEdges: [], usedTokens: 0, budgetTokens: 1000 })
  };
  return { root, repository, runtime };
}

test('agent loops through tool call and final answer', async () => {
  const { root, repository, runtime } = fixture();
  const responses = [
    { message: { role: 'assistant', content: '', tool_calls: [{ id: 'c1', type: 'function', function: { name: 'echo', arguments: '{"text":"hello"}' } }] }, finishReason: 'tool_calls', usage: { prompt_tokens: 10, completion_tokens: 5, total_tokens: 15 } },
    { message: { role: 'assistant', content: 'done' }, finishReason: 'stop', usage: { prompt_tokens: 12, completion_tokens: 2, total_tokens: 14 } }
  ];
  const provider = { model: 'mock', complete: async () => responses.shift() };
  const tools = new ToolRegistry().register({
    name: 'echo', permission: 'read', parameters: { type: 'object', properties: { text: { type: 'string' } } },
    execute: ({ text }) => text
  });
  const agent = new AgentLoop({ provider, repository, runtime, workspace: root, tools });
  const result = await agent.run('test', { autoIngest: false, recordTask: false });
  assert.equal(result.final, 'done');
  assert.equal(result.usage.requests, 2);
  assert.equal(result.usage.totalTokens, 29);
  assert.equal(result.session.steps.length, 2);
  assert.equal(result.session.messages.at(-2).role, 'tool');
});

test('agent stops at max steps', async () => {
  const { root, repository, runtime } = fixture();
  let n = 0;
  const provider = {
    model: 'mock',
    complete: async () => ({ message: { role: 'assistant', content: '', tool_calls: [{ id: `c${++n}`, type: 'function', function: { name: 'echo', arguments: '{}' } }] }, finishReason: 'tool_calls' })
  };
  const tools = new ToolRegistry().register({ name: 'echo', permission: 'read', execute: () => 'x' });
  const agent = new AgentLoop({ provider, repository, runtime, workspace: root, tools });
  await assert.rejects(() => agent.run('loop', { maxSteps: 2, autoIngest: false, recordTask: false }), AgentMaxStepsError);
});
