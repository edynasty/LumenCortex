import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

import {
  WorkUnitManager,
  createWorkUnit,
  validateWorkUnitPlan
} from '../src/work-unit.js';
import {
  CognitiveController,
  CognitiveRouter,
  DecisionLayer
} from '../src/cognitive-control.js';
import { AgentLoop } from '../src/agent.js';
import { ToolRegistry } from '../src/tools.js';

test('Work Units reject model routing fields', () => {
  assert.throws(
    () => createWorkUnit({ goal: 'fix bug', model: 'some-model' }),
    /cannot select model/
  );
  assert.throws(
    () => createWorkUnit({ goal: 'fix bug', provider: 'openrouter' }),
    /cannot select provider/
  );
  assert.throws(
    () => createWorkUnit({ goal: 'fix bug', category: 'deep' }),
    /cannot select category/
  );
});

test('Work Unit plans validate dependency cycles', () => {
  assert.throws(
    () => validateWorkUnitPlan([
      { id: 'a', goal: 'A', dependsOn: ['b'] },
      { id: 'b', goal: 'B', dependsOn: ['a'] }
    ]),
    /dependency cycle/
  );
});

test('Work Unit completion requires declared evidence and passed verification', () => {
  const session = { metadata: {} };
  const manager = new WorkUnitManager(session);
  manager.seed([{
    id: 'repair',
    goal: 'Repair transaction bug',
    requiredEvidence: ['production-call-path'],
    verification: ['focused-test']
  }]);

  assert.equal(manager.current().id, 'repair');
  assert.throws(
    () => manager.complete('repair'),
    /missing required evidence/
  );

  manager.update('repair', {
    evidence: [{
      requirement: 'production-call-path',
      ref: 'src/service.js:42',
      summary: 'production caller verified'
    }],
    verification_results: [{
      check: 'focused-test',
      status: 'passed',
      detail: '2/2 passed'
    }]
  });
  const completed = manager.complete('repair', { summary: 'fixed' });
  assert.equal(completed.status, 'completed');
  assert.equal(manager.hasIncomplete(), false);
});

test('Work Unit dependencies activate in order', () => {
  const session = { metadata: {} };
  const manager = new WorkUnitManager(session);
  manager.seed([
    { id: 'inspect', goal: 'Inspect root cause' },
    { id: 'repair', goal: 'Apply repair', dependsOn: ['inspect'] }
  ]);

  assert.equal(manager.current().id, 'inspect');
  manager.complete('inspect');
  assert.equal(manager.current().id, 'repair');
});

test('Agent blocks premature final answer until persistent Work Unit is completed', async () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'lcx-work-unit-agent-'));
  const repoDir = path.join(root, '.lumencortex');
  fs.mkdirSync(repoDir, { recursive: true });

  const graphState = { version: 1, nodes: {}, edges: {}, metadata: {} };
  const repository = {
    dir: repoDir,
    _graph: graphState,
    graph() {
      return {
        snapshot: () => structuredClone(this._graph),
        getNode: (id) => this._graph.nodes[id],
        addNode: (node) => {
          this._graph.nodes[node.id] = structuredClone(node);
          return node;
        },
        putNode: (node) => {
          this._graph.nodes[node.id] = structuredClone(node);
          return node;
        }
      };
    },
    writeGraph(next) { this._graph = structuredClone(next); },
    commit() { return { id: 'commit-work-unit' }; }
  };

  const runtime = {
    context() {
      return { selectedNodes: [], selectedEdges: [], usedTokens: 0, budgetTokens: 1000 };
    },
    promote() { throw new Error('promotion not expected'); }
  };

  let calls = 0;
  const provider = {
    model: 'mock-model',
    async complete() {
      calls += 1;
      if (calls === 1) {
        return {
          message: { role: 'assistant', content: 'premature final' },
          finishReason: 'stop'
        };
      }
      if (calls === 2) {
        return {
          message: {
            role: 'assistant',
            content: '',
            tool_calls: [{
              id: 'wu-call',
              type: 'function',
              function: {
                name: 'work_unit_update',
                arguments: JSON.stringify({ id: 'wu1', status: 'completed', summary: 'done' })
              }
            }]
          },
          finishReason: 'tool_calls'
        };
      }
      return {
        message: { role: 'assistant', content: 'all work units complete' },
        finishReason: 'stop'
      };
    }
  };

  const controller = new CognitiveController({
    profile: {
      categories: {
        general: { default: true, description: 'general', models: [] }
      }
    },
    decisionLayer: new DecisionLayer(),
    router: new CognitiveRouter(),
    categoryResolver: null
  });
  const events = [];
  const agent = new AgentLoop({
    provider,
    repository,
    runtime,
    workspace: root,
    tools: new ToolRegistry(),
    cognitiveController: controller,
    onEvent: (event) => events.push(event)
  });

  const result = await agent.run('Implement the planned change', {
    maxSteps: 5,
    llmRetries: 0,
    autoPromote: false,
    autoIngest: false,
    recordTask: false,
    workUnits: [{ id: 'wu1', goal: 'Implement and verify the change' }]
  });

  assert.equal(result.final, 'all work units complete');
  assert.equal(calls, 3);
  assert.equal(result.session.metadata.workUnits.items.wu1.status, 'completed');
  assert.ok(events.some((event) => event.type === 'work_unit.blocked_final'));
  assert.ok(result.session.messages.some((message) =>
    message.role === 'user' && /Work Unit completion gate rejected/.test(message.content)
  ));
});
