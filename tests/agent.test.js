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
  const repoDir = path.join(root, '.lumencortex');
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

test('user cancellation aborts the provider without retrying and persists resumable session state', async () => {
  const { root, repository, runtime } = fixture();
  const controller = new AbortController();
  let attempts = 0;
  const provider = {
    model: 'mock-cancel',
    complete: ({ signal }) => new Promise((resolve, reject) => {
      attempts += 1;
      if (!signal) return reject(new Error('missing abort signal'));
      const abort = () => {
        const error = new Error('cancelled by test');
        error.name = 'AbortError';
        reject(error);
      };
      if (signal.aborted) abort();
      else signal.addEventListener('abort', abort, { once: true });
    })
  };
  const agent = new AgentLoop({
    provider,
    repository,
    runtime,
    workspace: root,
    tools: new ToolRegistry()
  });

  setTimeout(() => controller.abort(), 20);
  let caught;
  try {
    await agent.run('cancel task', {
      signal: controller.signal,
      llmRetries: 3,
      retryBaseMs: 1,
      recordTask: false
    });
  } catch (error) {
    caught = error;
  }

  assert.ok(caught);
  assert.equal(caught.name, 'AbortError');
  assert.equal(attempts, 1, 'explicit cancellation must not be retried');
  assert.ok(caught.sessionId);
  const saved = new AgentSessionStore(repository.dir).load(caught.sessionId);
  assert.equal(saved.status, 'interrupted');
  assert.equal(saved.error.name, 'AbortError');
  assert.equal(saved.error.step, 1);
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


test('resumed session preserves global step numbering, context history and cumulative usage', async () => {
  const { root, repository, runtime } = fixture();
  const tools = new ToolRegistry().register({
    name: 'echo',
    permission: 'read',
    execute: () => 'checkpoint'
  });

  const firstProvider = {
    model: 'mock-first',
    complete: async () => ({
      message: {
        role: 'assistant',
        content: '',
        tool_calls: [{ id: 'first-call', type: 'function', function: { name: 'echo', arguments: '{}' } }]
      },
      finishReason: 'tool_calls',
      usage: { prompt_tokens: 10, completion_tokens: 2, total_tokens: 12 }
    })
  };
  const firstAgent = new AgentLoop({ provider: firstProvider, repository, runtime, workspace: root, tools });

  let sessionId;
  try {
    await firstAgent.run('long task', {
      maxSteps: 1,
      autoIngest: false,
      autoPromote: false,
      recordTask: false
    });
  } catch (error) {
    sessionId = error.sessionId;
  }
  assert.ok(sessionId);

  const store = new AgentSessionStore(repository.dir);
  const interrupted = store.load(sessionId);
  interrupted.error = { message: 'old transient failure', step: 1 };
  interrupted.status = 'interrupted';
  store.save(interrupted);

  const secondProvider = {
    model: 'mock-second',
    complete: async () => ({
      message: { role: 'assistant', content: 'continued-done' },
      finishReason: 'stop',
      usage: { prompt_tokens: 20, completion_tokens: 3, total_tokens: 23 }
    })
  };
  const secondAgent = new AgentLoop({ provider: secondProvider, repository, runtime, workspace: root, tools });
  const result = await secondAgent.run('continue', {
    sessionId,
    maxSteps: 1,
    autoIngest: false,
    autoPromote: false,
    recordTask: false
  });

  assert.equal(result.final, 'continued-done');
  assert.deepEqual(result.session.steps.map((step) => step.step), [1, 2]);
  assert.deepEqual(result.session.metadata.contextHistory.map((item) => item.step), [1, 2]);
  assert.equal(result.usage.requests, 2);
  assert.equal(result.usage.totalTokens, 35);
  assert.equal(result.session.error, undefined);
});


test('agent promotion dedupe key uses the stable session goal, not moving step focus', async () => {
  const { root, repository, runtime } = fixture();
  const promotionGoals = [];
  const promotionController = {
    maybePromote(goal) {
      promotionGoals.push(goal);
      return { promoted: false };
    }
  };
  let calls = 0;
  const provider = {
    model: 'mock-promotion-goal',
    complete: async () => {
      calls += 1;
      if (calls < 3) {
        return {
          message: {
            role: 'assistant',
            content: '',
            tool_calls: [{ id: `p${calls}`, type: 'function', function: { name: 'echo', arguments: '{}' } }]
          },
          finishReason: 'tool_calls'
        };
      }
      return { message: { role: 'assistant', content: 'done' }, finishReason: 'stop' };
    }
  };
  const tools = new ToolRegistry().register({ name: 'echo', permission: 'read', execute: () => 'ok' });
  const agent = new AgentLoop({ provider, repository, runtime, workspace: root, tools, promotionController });
  await agent.run('stable task goal', { autoIngest: false, recordTask: false });
  assert.deepEqual(promotionGoals, ['stable task goal', 'stable task goal', 'stable task goal']);
});


test('agent recovers once from an empty assistant turn', async () => {
  const { root, repository, runtime } = fixture();
  let calls = 0;
  const provider = {
    model: 'mock-empty-turn',
    complete: async () => {
      calls += 1;
      if (calls === 1) {
        return {
          message: { role: 'assistant', content: '', reasoning: 'still thinking' },
          finishReason: 'stop',
          usage: { prompt_tokens: 2, completion_tokens: 1, total_tokens: 3 }
        };
      }
      return {
        message: { role: 'assistant', content: 'recovered-final' },
        finishReason: 'recovered-stop',
        usage: { prompt_tokens: 3, completion_tokens: 1, total_tokens: 4 }
      };
    }
  };
  const agent = new AgentLoop({
    provider,
    repository,
    runtime,
    workspace: root,
    tools: new ToolRegistry()
  });
  const result = await agent.run('recover empty turn', {
    emptyTurnRetries: 1,
    recordTask: false
  });
  assert.equal(result.final, 'recovered-final');
  assert.equal(calls, 2);
  assert.equal(result.usage.requests, 2);
  assert.equal(result.usage.totalTokens, 7);
  assert.equal(result.session.steps[0].finishReason, 'recovered-stop');
});


test('agent can bound model tool-call fanout to one call per reasoning step', async () => {
  const { root, repository, runtime } = fixture();
  const executed = [];
  const events = [];
  let request = 0;
  const provider = {
    model: 'mock-fanout',
    complete: async () => {
      request += 1;
      if (request === 1) {
        return {
          message: {
            role: 'assistant',
            content: '',
            tool_calls: [
              { id: 'f1', type: 'function', function: { name: 'echo', arguments: '{"text":"one"}' } },
              { id: 'f2', type: 'function', function: { name: 'echo', arguments: '{"text":"two"}' } },
              { id: 'f3', type: 'function', function: { name: 'echo', arguments: '{"text":"three"}' } }
            ]
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
    execute: ({ text }) => { executed.push(text); return text; }
  });
  const agent = new AgentLoop({
    provider,
    repository,
    runtime,
    workspace: root,
    tools,
    onEvent: event => events.push(event)
  });
  const result = await agent.run('fanout task', {
    maxToolCallsPerStep: 1,
    autoIngest: false,
    autoPromote: false,
    recordTask: false
  });
  assert.deepEqual(executed, ['one']);
  assert.equal(result.session.messages.find(m => m.role === 'assistant' && m.tool_calls)?.tool_calls.length, 1);
  const deferred = events.find(event => event.type === 'tools.deferred');
  assert.deepEqual(
    { requested: deferred.requested, executing: deferred.executing, deferred: deferred.deferred },
    { requested: 3, executing: 1, deferred: 2 }
  );
});


test('agent sends only allowlisted tools to the provider', async () => {
  const { root, repository, runtime } = fixture();
  let requestTools;
  const provider = {
    model: 'mock-tool-working-set',
    complete: async ({ tools }) => {
      requestTools = tools;
      return { message: { role: 'assistant', content: 'done' }, finishReason: 'stop' };
    }
  };
  const tools = new ToolRegistry()
    .register({ name: 'read_one', permission: 'read', execute: () => 'one' })
    .register({ name: 'hidden_write', permission: 'write', execute: () => 'hidden' });

  const agent = new AgentLoop({ provider, repository, runtime, workspace: root, tools });
  await agent.run('bounded tools', {
    toolAllowlist: ['read_one'],
    recordTask: false
  });

  assert.deepEqual(requestTools.map(schema => schema.function.name), ['read_one']);
});

test('agent refuses a hallucinated tool outside the current working set', async () => {
  const { root, repository, runtime } = fixture();
  let hiddenExecuted = false;
  let turn = 0;
  const provider = {
    model: 'mock-hidden-tool',
    complete: async () => {
      turn += 1;
      if (turn === 1) {
        return {
          message: {
            role: 'assistant',
            content: '',
            tool_calls: [{
              id: 'hidden-call',
              type: 'function',
              function: { name: 'hidden_write', arguments: '{}' }
            }]
          },
          finishReason: 'tool_calls'
        };
      }
      return { message: { role: 'assistant', content: 'done' }, finishReason: 'stop' };
    }
  };
  const tools = new ToolRegistry()
    .register({ name: 'read_one', permission: 'read', execute: () => 'one' })
    .register({ name: 'hidden_write', permission: 'write', execute: () => { hiddenExecuted = true; return 'hidden'; } });

  const agent = new AgentLoop({ provider, repository, runtime, workspace: root, tools });
  const result = await agent.run('do not expose hidden tool', {
    toolAllowlist: ['read_one'],
    autoIngest: false,
    recordTask: false
  });

  assert.equal(hiddenExecuted, false);
  const toolMessage = result.session.messages.find(message => message.role === 'tool');
  assert.match(toolMessage.content, /not available in the current tool working set/);
});
