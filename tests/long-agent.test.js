import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { AgentLoop } from '../src/agent.js';
import { ToolRegistry } from '../src/tools.js';

test('long agent task refreshes attention every step while bounding model working history', async () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'mw-long-agent-'));
  const repoDir = path.join(root, '.lumencortex');
  fs.mkdirSync(repoDir, { recursive: true });

  const repository = {
    dir: repoDir,
    _graph: { version: 1, nodes: {}, edges: {}, metadata: {} },
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

  let contextCalls = 0;
  const runtime = {
    context(focus, { budgetTokens } = {}) {
      contextCalls += 1;
      return {
        goal: focus,
        selectedNodes: [],
        selectedEdges: [],
        usedTokens: 0,
        budgetTokens: budgetTokens ?? 1000,
        trace: [],
        seeds: []
      };
    }
  };

  let modelStep = 0;
  const requestSizes = [];
  const provider = {
    model: 'mock-long',
    async complete({ messages }) {
      requestSizes.push(messages.length);
      modelStep += 1;
      if (modelStep <= 20) {
        return {
          message: {
            role: 'assistant',
            content: '',
            tool_calls: [{
              id: `call-${modelStep}`,
              type: 'function',
              function: {
                name: 'echo',
                arguments: JSON.stringify({ text: `step-${modelStep}` })
              }
            }]
          },
          finishReason: 'tool_calls'
        };
      }
      return {
        message: { role: 'assistant', content: 'long-task-done' },
        finishReason: 'stop'
      };
    }
  };

  const tools = new ToolRegistry().register({
    name: 'echo',
    permission: 'read',
    parameters: {
      type: 'object',
      properties: { text: { type: 'string' } }
    },
    execute: ({ text }) => text
  });

  const agent = new AgentLoop({
    provider,
    repository,
    runtime,
    workspace: root,
    tools
  });

  const result = await agent.run('perform a long multi-step task', {
    maxSteps: 24,
    recentRounds: 3,
    maxWorkingChars: 50000,
    autoIngest: false,
    recordTask: false
  });

  assert.equal(result.final, 'long-task-done');
  assert.equal(result.usage.requests, 21);
  assert.equal(contextCalls, 21, 'attention must be recomputed for every reasoning step');
  assert.equal(result.session.steps.length, 21);
  assert.equal(result.session.messages.filter((m) => m.role === 'tool').length, 20);
  assert.equal(result.session.metadata.contextHistory.length, 21);

  // Full history is durable in the session, but the model only receives the current
  // user turn plus the most recent complete tool rounds and current Active Subgraph.
  assert.ok(result.session.messages.length > 40);
  assert.ok(Math.max(...requestSizes.slice(4)) <= 10, `working message window grew unexpectedly: ${requestSizes}`);
});
