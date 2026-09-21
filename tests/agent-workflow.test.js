import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { AgentLoop } from '../src/agent.js';
import { ToolRegistry } from '../src/tools.js';
import { AgentSessionStore } from '../src/session.js';
import { WorkflowRuntime } from '../src/workflow.js';

function fixture() {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'lcx-workflow-agent-'));
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
    context() { return { selectedNodes: [], selectedEdges: [], usedTokens: 0, budgetTokens: 1000 }; },
    promote() { throw new Error('promotion should not run with empty context'); }
  };
  return { root, repository, runtime };
}

test('agent rejects an early final answer until workflow evidence is satisfied', async () => {
  const { root, repository, runtime } = fixture();
  const requests = [];
  const events = [];
  let turn = 0;
  const provider = {
    model: 'mock-workflow',
    async complete({ messages, tools }) {
      requests.push({ messages, tools: tools.map((item) => item.function.name) });
      turn += 1;
      if (turn === 1) return { message: { role: 'assistant', content: 'done too early' }, finishReason: 'stop' };
      if (turn === 2) return {
        message: {
          role: 'assistant',
          content: '',
          tool_calls: [{ id: 'check-1', type: 'function', function: { name: 'check', arguments: '{}' } }]
        },
        finishReason: 'tool_calls'
      };
      return { message: { role: 'assistant', content: 'verified done' }, finishReason: 'stop' };
    }
  };
  const tools = new ToolRegistry()
    .register({ name: 'check', permission: 'read', execute: () => ({ exitCode: 0 }) })
    .register({ name: 'dangerous_edit', permission: 'write', execute: () => 'should not be exposed' });
  const workflow = {
    version: 1,
    id: 'proof',
    entry: 'verify',
    facts: { verified: false },
    actions: {
      verify: {
        terminal: true,
        allowedTools: ['check'],
        outcomes: [{ when: { tool: 'check', ok: true }, set: { verified: true } }],
        completeWhen: { fact: 'verified', equals: true }
      }
    }
  };

  const agent = new AgentLoop({ provider, repository, runtime, workspace: root, tools, onEvent: (event) => events.push(event) });
  const result = await agent.run('prove completion', { workflow, autoIngest: false, autoPromote: false, recordTask: false });

  assert.equal(result.final, 'verified done');
  assert.equal(result.session.metadata.workflow.facts.verified, true);
  assert.deepEqual(requests.map((request) => request.tools), [['check'], ['check'], ['check']]);
  assert.ok(events.some((event) => event.type === 'workflow.blocked_final'));
  assert.ok(requests[1].messages.some((message) => String(message.content).includes('Workflow contract rejected completion')));
});

test('workflow tool boundary is rechecked after a transition in the same assistant turn', async () => {
  const { root, repository, runtime } = fixture();
  let forbiddenExecuted = false;
  let turn = 0;
  const provider = {
    model: 'mock-transition-boundary',
    async complete() {
      turn += 1;
      if (turn === 1) return {
        message: {
          role: 'assistant',
          content: '',
          tool_calls: [
            { id: 'a', type: 'function', function: { name: 'first', arguments: '{}' } },
            { id: 'b', type: 'function', function: { name: 'second', arguments: '{}' } }
          ]
        },
        finishReason: 'tool_calls'
      };
      return { message: { role: 'assistant', content: 'done' }, finishReason: 'stop' };
    }
  };
  const tools = new ToolRegistry()
    .register({ name: 'first', permission: 'read', execute: () => 'ok' })
    .register({ name: 'second', permission: 'write', execute: () => { forbiddenExecuted = true; return 'bad'; } });
  const workflow = {
    version: 1,
    id: 'boundary',
    entry: 'one',
    facts: { advanced: false },
    actions: {
      one: {
        allowedTools: ['first', 'second'],
        outcomes: [{ when: { tool: 'first', ok: true }, set: { advanced: true } }],
        routes: [{ to: 'two', when: { fact: 'advanced', equals: true } }]
      },
      two: { terminal: true, allowedTools: ['first'] }
    }
  };
  const agent = new AgentLoop({ provider, repository, runtime, workspace: root, tools });
  const result = await agent.run('respect transition', { workflow, autoIngest: false, autoPromote: false, recordTask: false });
  assert.equal(result.final, 'done');
  assert.equal(forbiddenExecuted, false);
  const denied = result.session.steps[0].toolCalls.find((call) => call.name === 'second');
  assert.equal(denied.denied, true);
});

test('agent pauses at a human gate and resumes after persisted approval', async () => {
  const { root, repository, runtime } = fixture();
  const tools = new ToolRegistry().register({ name: 'prepare', permission: 'read', execute: () => 'prepared' });
  const workflow = {
    version: 1,
    id: 'human-gate',
    entry: 'review',
    facts: { prepared: false },
    actions: {
      review: {
        terminal: true,
        allowedTools: ['prepare'],
        outcomes: [{ when: { tool: 'prepare', ok: true }, set: { prepared: true } }],
        completeWhen: { fact: 'prepared', equals: true },
        gates: [{ id: 'approve', type: 'human', title: 'Human approval' }]
      }
    }
  };
  const firstProvider = {
    model: 'mock-gate',
    async complete() {
      return {
        message: { role: 'assistant', content: '', tool_calls: [{ id: 'prepare-1', type: 'function', function: { name: 'prepare', arguments: '{}' } }] },
        finishReason: 'tool_calls'
      };
    }
  };
  const firstAgent = new AgentLoop({ provider: firstProvider, repository, runtime, workspace: root, tools });
  const paused = await firstAgent.run('prepare and wait', { workflow, autoIngest: false, autoPromote: false, recordTask: false });
  assert.equal(paused.session.status, 'waiting_gate');
  assert.equal(paused.waitingGate.gates[0].id, 'approve');

  const store = new AgentSessionStore(repository.dir);
  const saved = store.load(paused.session.id);
  const wf = WorkflowRuntime.fromSession(saved);
  wf.approve('approve', { actor: 'unit-test' });
  saved.metadata.workflow = wf.snapshot();
  store.save(saved);
  store.close();

  const secondProvider = {
    model: 'mock-gate-resume',
    async complete() { return { message: { role: 'assistant', content: 'approved done' }, finishReason: 'stop' }; }
  };
  const secondAgent = new AgentLoop({ provider: secondProvider, repository, runtime, workspace: root, tools });
  const completed = await secondAgent.run('', { sessionId: paused.session.id, autoIngest: false, autoPromote: false, recordTask: false });
  assert.equal(completed.final, 'approved done');
  assert.equal(completed.session.status, 'completed');
  assert.equal(completed.session.metadata.workflow.status, 'ready_to_finish');
});
