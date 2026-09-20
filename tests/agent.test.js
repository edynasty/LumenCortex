import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { AgentLoop, AgentMaxStepsError } from '../src/agent.js';
import { ToolRegistry } from '../src/tools.js';
import { AgentSessionStore } from '../src/session.js';

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
    contextCalls: 0,
    context() {
      this.contextCalls += 1;
      return { selectedNodes: [], selectedEdges: [], usedTokens: 0, budgetTokens: 1000 };
    },
    promote() { throw new Error('promotion should not run with empty context'); }
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
  assert.equal(runtime.contextCalls, 2, 'attention should be recomputed for every reasoning step');
  assert.equal(result.session.metadata.recentObservationNodeIds.length, 1);
});

test('agent sends a bounded moving working set instead of replaying every message', async () => {
  const { root, repository, runtime } = fixture();
  const requests = [];
  let n = 0;
  const provider = {
    model: 'mock',
    complete: async ({ messages }) => {
      requests.push(messages);
      n += 1;
      if (n < 5) {
        return {
          message: {
            role: 'assistant',
            content: '',
            tool_calls: [{ id: `c${n}`, type: 'function', function: { name: 'echo', arguments: JSON.stringify({ text: `step-${n}` }) } }]
          },
          finishReason: 'tool_calls'
        };
      }
      return { message: { role: 'assistant', content: 'done' }, finishReason: 'stop' };
    }
  };
  const tools = new ToolRegistry().register({
    name: 'echo',
    permission: 'read',
    parameters: { type: 'object', properties: { text: { type: 'string' } } },
    execute: ({ text }) => `${text} ${'x'.repeat(3000)}`
  });
  const agent = new AgentLoop({ provider, repository, runtime, workspace: root, tools });
  const result = await agent.run('long loop', {
    autoIngest: false,
    autoPromote: false,
    recordTask: false,
    recentRounds: 2,
    workingChars: 7000
  });
  assert.equal(result.final, 'done');
  assert.equal(runtime.contextCalls, 5);
  assert.ok(result.session.messages.length > requests.at(-1).length, 'persisted history should be larger than model working set');
  assert.ok(requests.at(-1).filter((m) => m.role === 'assistant').length <= 2);
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


test('agent retries transient provider failures without losing the task', async () => {
  const { root, repository, runtime } = fixture();
  let attempts = 0;
  const provider = {
    model: 'mock-retry',
    complete: async () => {
      attempts += 1;
      if (attempts < 3) {
        const error = new Error('temporary upstream outage');
        error.status = 503;
        throw error;
      }
      return { message: { role: 'assistant', content: 'recovered' }, finishReason: 'stop' };
    }
  };
  const agent = new AgentLoop({
    provider,
    repository,
    runtime,
    workspace: root,
    tools: new ToolRegistry()
  });
  const result = await agent.run('retry task', {
    llmRetries: 2,
    retryBaseMs: 0,
    recordTask: false
  });
  assert.equal(result.final, 'recovered');
  assert.equal(attempts, 3);
  assert.equal(result.usage.requests, 3);
  assert.equal(result.session.status, 'completed');
});

test('agent persists an interrupted session after non-retryable provider failure', async () => {
  const { root, repository, runtime } = fixture();
  const provider = {
    model: 'mock-fail',
    complete: async () => {
      const error = new Error('bad request');
      error.status = 400;
      throw error;
    }
  };
  const agent = new AgentLoop({
    provider,
    repository,
    runtime,
    workspace: root,
    tools: new ToolRegistry()
  });

  let caught;
  try {
    await agent.run('interrupt task', { llmRetries: 0, recordTask: false });
  } catch (error) {
    caught = error;
  }
  assert.ok(caught);
  assert.ok(caught.sessionId);
  const saved = new AgentSessionStore(repository.dir).load(caught.sessionId);
  assert.equal(saved.status, 'interrupted');
  assert.equal(saved.error.status, 400);
  assert.equal(saved.error.step, 1);
});
