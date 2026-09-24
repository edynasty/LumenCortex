import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

import {
  AlgorithmDecisionProvider,
  CategoryResolver,
  CognitiveController,
  CognitiveRouter,
  DecisionLayer,
  ProgressMonitor,
  SystemOneDecisionProvider,
  loadCognitiveProfile
} from '../src/cognitive-control.js';
import { AgentLoop } from '../src/agent.js';
import { ToolRegistry } from '../src/tools.js';

test('algorithmic decision layer classifies visual work without executing it', async () => {
  const provider = new AlgorithmDecisionProvider();
  const result = await provider.decide({
    state: {
      goal: 'Implement the responsive frontend layout and CSS for the settings screen',
      progress: {}
    }
  });
  assert.equal(result.answers.category.choice, 'visual-engineering');
  assert.ok(result.answers.need_think.noul >= 0 && result.answers.need_think.noul <= 1);
});

test('framework router raises Think effort for repeated equivalent failures', async () => {
  const decisionProvider = new AlgorithmDecisionProvider();
  const state = {
    goal: 'Debug a cross-module transaction deadlock',
    progress: {
      maxRepeatedFailure: 4,
      noProgressSteps: 3
    },
    context: { selectedNodeCount: 3 }
  };
  const algorithm = await decisionProvider.decide({ state });
  const router = new CognitiveRouter();
  const route = router.route({
    state,
    decision: {
      algorithm,
      models: [],
      errors: [],
      signals: algorithm.answers
    }
  });

  assert.equal(route.think, true);
  assert.equal(route.effort, 'max');
  assert.ok(['deep', 'ultrabrain'].includes(route.category));
});

test('progress monitor groups repeated tool failures by normalized signature', () => {
  const monitor = new ProgressMonitor();
  monitor.observeTool({
    name: 'shell',
    args: { command: 'npm test' },
    result: { ok: false, content: 'Error at /tmp/a.js line 12345' },
    step: 1
  });
  monitor.observeTool({
    name: 'shell',
    args: { command: 'npm test' },
    result: { ok: false, content: 'Error at /tmp/b.js line 67890' },
    step: 2
  });

  const snapshot = monitor.snapshot();
  assert.equal(snapshot.distinctFailureCount, 1);
  assert.equal(snapshot.maxRepeatedFailure, 2);
});

test('System One decision provider uses the Jev/Laya compatible wire endpoint', async () => {
  let request;
  const provider = new SystemOneDecisionProvider({
    name: 'mock-system-one',
    baseURL: 'http://decision.test',
    model: 'decision-model',
    fetchImpl: async (url, init) => {
      request = { url, body: JSON.parse(init.body) };
      return new Response(JSON.stringify({
        model: 'decision-model',
        answers: {
          category: {
            type: 'choice',
            choice: 'deep',
            confidence: 0.91,
            probabilities: { deep: 0.91, general: 0.09 }
          },
          need_think: { type: 'noul', noul: 0.83 }
        }
      }), { status: 200, headers: { 'content-type': 'application/json' } });
    }
  });

  const result = await provider.decide({
    state: { goal: 'debug transaction' },
    questions: {
      category: { type: 'choice', instructions: 'Category?', criteria: { general: 'normal', deep: 'hard' } }
    }
  });

  assert.equal(request.url, 'http://decision.test/v1/systemone');
  assert.equal(request.body.model, 'decision-model');
  assert.equal(result.answers.category.choice, 'deep');
});

test('category resolver follows explicit ordered model chains', () => {
  const profile = {
    categories: {
      general: { default: true, models: [] },
      deep: {
        models: [
          { provider: 'missing', model: 'model-a' },
          { provider: 'mock', model: 'model-b' }
        ]
      }
    }
  };
  const fallback = { model: 'fallback', complete: async () => ({}) };
  const resolver = new CategoryResolver({
    profile,
    fallbackProvider: fallback,
    fallbackProviderName: 'fallback-provider',
    providerFactory: (name, options) => {
      if (name === 'missing') throw new Error('not configured');
      return { model: options.model, complete: async () => ({}) };
    }
  });

  const chain = resolver.resolveChain('deep');
  assert.deepEqual(chain.entries.map((entry) => entry.provider.model), ['model-b']);
});

test('profile accepts user-defined categories without numeric suitability configuration', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'lcx-cognition-'));
  const dir = path.join(root, '.lumencortex');
  fs.mkdirSync(dir, { recursive: true });
  fs.writeFileSync(path.join(dir, 'cognition.json'), JSON.stringify({
    categories: {
      'rust-hard': {
        description: 'Hard Rust implementation and debugging',
        models: [
          { provider: 'generic', model: 'local-rust-model' },
          { provider: 'openrouter', model: 'backup-model' }
        ]
      }
    }
  }));

  const profile = loadCognitiveProfile(root);
  assert.deepEqual(
    profile.categories['rust-hard'].models.map((entry) => entry.model),
    ['local-rust-model', 'backup-model']
  );
  assert.equal(profile.categories.general.default, true);
});

test('agent uses the selected category model chain and switches only after provider failure', async () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'lcx-cognitive-agent-'));
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
    context() {
      return { selectedNodes: [], selectedEdges: [], usedTokens: 0, budgetTokens: 1000 };
    },
    promote() { throw new Error('promotion not expected'); }
  };

  let firstCalls = 0;
  let secondCalls = 0;
  const first = {
    model: 'deep-primary',
    async complete() {
      firstCalls += 1;
      const error = new Error('primary unavailable');
      error.status = 503;
      throw error;
    }
  };
  const second = {
    model: 'deep-secondary',
    async complete({ messages }) {
      secondCalls += 1;
      assert.ok(messages.some((message) => message.role === 'system' && /Think mode is active/.test(message.content)));
      return { message: { role: 'assistant', content: 'done' }, finishReason: 'stop' };
    }
  };

  const profile = {
    categories: {
      general: { default: true, description: 'general', models: [] },
      deep: {
        description: 'deep',
        models: [
          { provider: 'mock', model: 'deep-primary' },
          { provider: 'mock', model: 'deep-secondary' }
        ]
      }
    }
  };
  const resolver = new CategoryResolver({
    profile,
    fallbackProvider: second,
    fallbackProviderName: 'fallback',
    providerFactory: (_name, options) => options.model === 'deep-primary' ? first : second
  });
  const controller = new CognitiveController({
    profile,
    decisionLayer: new DecisionLayer(),
    router: new CognitiveRouter(),
    categoryResolver: resolver
  });
  const events = [];
  const agent = new AgentLoop({
    provider: second,
    repository,
    runtime,
    workspace: root,
    tools: new ToolRegistry(),
    cognitiveController: controller,
    onEvent: (event) => events.push(event)
  });

  const result = await agent.run('Debug the architecture transaction deadlock', {
    llmRetries: 0,
    autoPromote: false,
    autoIngest: false,
    recordTask: false
  });

  assert.equal(result.final, 'done');
  assert.equal(firstCalls, 1);
  assert.equal(secondCalls, 1);
  assert.equal(result.session.steps[0].cognition.category, 'deep');
  assert.equal(result.session.steps[0].cognition.think, true);
  assert.ok(events.some((event) => event.type === 'cognition.model_chain'));
});
