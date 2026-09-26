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
  GenerativeDecisionProvider,
  ProgressMonitor,
  ProviderHealthRegistry,
  SystemOneDecisionProvider,
  createDecisionProvider,
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
    async complete({ messages, reasoningEffort }) {
      secondCalls += 1;
      assert.ok(messages.some((message) => message.role === 'system' && /Think mode is active/.test(message.content)));
      assert.ok(['medium', 'high', 'max'].includes(reasoningEffort));
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
  assert.equal(result.session.metadata.cognition.modelTelemetry['deep-primary'].failures, 1);
  assert.equal(result.session.metadata.cognition.modelTelemetry['deep-secondary'].calls, 1);
  assert.ok(result.session.metadata.cognition.modelTelemetry['deep-secondary'].ewmaLatencyMs >= 0);
});


test('Decision Layer opens a circuit only for operational failures and probes after cooldown', async () => {
  let now = 1000;
  let calls = 0;
  const health = new ProviderHealthRegistry({
    failureThreshold: 2,
    cooldownMs: 5000,
    now: () => now
  });
  const provider = {
    name: 'jev-test',
    model: 'jev',
    async decide() {
      calls += 1;
      const error = new Error('provider unavailable');
      error.status = 503;
      throw error;
    }
  };
  const layer = new DecisionLayer({
    providers: [provider],
    healthRegistry: health
  });

  await layer.decide({ state: { goal: 'x' } });
  await layer.decide({ state: { goal: 'x' } });
  const skipped = await layer.decide({ state: { goal: 'x' } });

  assert.equal(calls, 2);
  assert.equal(skipped.errors[0].skipped, true);
  assert.equal(health.snapshot()['decision:jev-test:jev'].available, false);

  now += 5001;
  await layer.decide({ state: { goal: 'x' } });
  assert.equal(calls, 3);
});

test('Category resolver skips a model whose provider circuit is open and preserves chain order', () => {
  const health = new ProviderHealthRegistry({ failureThreshold: 1, cooldownMs: 10000 });
  health.recordFailure('model:mock:model-a', Object.assign(new Error('down'), { status: 503 }));
  const resolver = new CategoryResolver({
    profile: {
      categories: {
        general: { default: true, models: [] },
        deep: {
          models: [
            { provider: 'mock', model: 'model-a' },
            { provider: 'mock', model: 'model-b' }
          ]
        }
      }
    },
    healthRegistry: health,
    providerFactory: (_name, options) => ({
      model: options.model,
      complete: async () => ({ message: { role: 'assistant', content: 'ok' } })
    })
  });

  const chain = resolver.resolveChain('deep');
  assert.deepEqual(chain.entries.map((entry) => entry.descriptor.model), ['model-b']);
  assert.equal(chain.skipped[0].descriptor.model, 'model-a');
  assert.equal(chain.skipped[0].reason, 'circuit-open');
});


test('cognition profile normalizes explicitly enabled embedding retrieval', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'lcx-cognition-embedding-'));
  const dir = path.join(root, '.lumencortex');
  fs.mkdirSync(dir, { recursive: true });
  fs.writeFileSync(path.join(dir, 'cognition.json'), JSON.stringify({
    retrieval: {
      embeddings: {
        enabled: true,
        provider: 'generic',
        model: 'embed-local',
        baseURL: 'http://127.0.0.1:11434/v1',
        apiKeyEnv: 'LOCAL_EMBED_KEY',
        timeoutMs: 5000,
        batchSize: 7,
        candidateLimit: 80,
        rrfK: 42,
        lexicalWeight: 0.8,
        semanticWeight: 1.3,
        semanticMinScore: 0.2
      }
    }
  }));

  const profile = loadCognitiveProfile(root);
  assert.deepEqual(profile.retrieval.embeddings, {
    enabled: true,
    provider: 'generic',
    model: 'embed-local',
    baseURL: 'http://127.0.0.1:11434/v1',
    apiKey: undefined,
    apiKeyEnv: 'LOCAL_EMBED_KEY',
    headers: {},
    timeoutMs: 5000,
    batchSize: 7,
    candidateLimit: 80,
    lexicalLimit: undefined,
    semanticLimit: undefined,
    semanticMinScore: 0.2,
    rrfK: 42,
    lexicalWeight: 0.8,
    semanticWeight: 1.3
  });
});

test('embedding retrieval remains disabled unless explicitly enabled', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'lcx-cognition-embedding-disabled-'));
  const dir = path.join(root, '.lumencortex');
  fs.mkdirSync(dir, { recursive: true });
  fs.writeFileSync(path.join(dir, 'cognition.json'), JSON.stringify({
    retrieval: {
      embeddings: {
        model: 'would-be-model',
        baseURL: 'http://127.0.0.1:11434/v1'
      }
    }
  }));

  const profile = loadCognitiveProfile(root);
  assert.equal(profile.retrieval.embeddings.enabled, false);
});


test('cognition profile keeps Governor scheduler independent and disabled by default', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'lcx-governor-scheduler-profile-'));
  const dir = path.join(root, '.lumencortex');
  fs.mkdirSync(dir, { recursive: true });
  fs.writeFileSync(path.join(dir, 'cognition.json'), JSON.stringify({
    governor: {
      enabled: false,
      scheduler: {
        enabled: true,
        useCurator: false,
        autoApplySafe: true,
        checkRevisionDelta: 7,
        cooldownMs: 1234,
        archiveCandidateThreshold: 4,
        canonicalizeGroupThreshold: 2,
        branchCandidateThreshold: 5,
        promotionGroupThreshold: 6,
        tierChangeThreshold: 9
      }
    }
  }));

  const profile = loadCognitiveProfile(root);
  assert.equal(profile.governor.enabled, false);
  assert.deepEqual(profile.governor.scheduler, {
    enabled: true,
    useCurator: false,
    autoApplySafe: true,
    checkRevisionDelta: 7,
    cooldownMs: 1234,
    archiveCandidateThreshold: 4,
    canonicalizeGroupThreshold: 2,
    branchCandidateThreshold: 5,
    promotionGroupThreshold: 6,
    tierChangeThreshold: 9
  });

  const emptyRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'lcx-governor-scheduler-default-'));
  const emptyDir = path.join(emptyRoot, '.lumencortex');
  fs.mkdirSync(emptyDir, { recursive: true });
  fs.writeFileSync(path.join(emptyDir, 'cognition.json'), JSON.stringify({
    governor: { enabled: false }
  }));
  const defaults = loadCognitiveProfile(emptyRoot);
  assert.equal(defaults.governor.scheduler.enabled, false);
  assert.equal(defaults.governor.scheduler.useCurator, false);
  assert.equal(defaults.governor.scheduler.autoApplySafe, false);
  assert.equal(defaults.governor.scheduler.checkRevisionDelta, 25);
});


test('Cognitive Router keeps explicit deterministic retrieval cues framework-owned', () => {
  const router = new CognitiveRouter();
  const state = {
    goal: 'Why did this regression start after the previous version?',
    progress: {}
  };
  const algorithm = {
    category: { type: 'choice', choice: 'deep', confidence: 0.9 },
    need_think: { type: 'noul', noul: 0.8 },
    evidence_sufficient: { type: 'noul', noul: 0.8 },
    stuck: { type: 'noul', noul: 0.1 },
    retrieval: { type: 'choice', choice: 'historical', confidence: 0.65 }
  };
  const route = router.route({
    state,
    decision: {
      algorithm: { answers: algorithm },
      signals: {
        ...algorithm,
        retrieval: { type: 'choice', choice: 'causal', confidence: 0.99 }
      }
    }
  });

  assert.equal(route.retrieval, 'historical');
  assert.equal(route.retrievalSource, 'algorithm');
  assert.ok(route.reasons.includes('retrieval-model-constrained'));
});

test('Cognitive Router requires high confidence before escalating lexical retrieval', () => {
  const router = new CognitiveRouter({
    retrievalConfidenceThreshold: 0.68,
    expensiveRetrievalConfidenceThreshold: 0.82
  });
  const state = { goal: 'Debug the root cause of this behavior', progress: {} };
  const algorithm = {
    category: { type: 'choice', choice: 'general', confidence: 0.7 },
    need_think: { type: 'noul', noul: 0.2 },
    evidence_sufficient: { type: 'noul', noul: 0.8 },
    stuck: { type: 'noul', noul: 0.1 },
    retrieval: { type: 'choice', choice: 'lexical', confidence: 0.65 }
  };

  const lowCausal = router.route({
    state,
    decision: {
      algorithm: { answers: algorithm },
      signals: {
        ...algorithm,
        retrieval: { type: 'choice', choice: 'causal', confidence: 0.67 }
      }
    }
  });
  assert.equal(lowCausal.retrieval, 'lexical');
  assert.equal(lowCausal.retrievalSource, 'algorithm');

  const highCausal = router.route({
    state,
    decision: {
      algorithm: { answers: algorithm },
      signals: {
        ...algorithm,
        retrieval: { type: 'choice', choice: 'causal', confidence: 0.91 }
      }
    }
  });
  assert.equal(highCausal.retrieval, 'causal');
  assert.equal(highCausal.retrievalSource, 'decision');

  const mediumHybrid = router.route({
    state,
    decision: {
      algorithm: { answers: algorithm },
      signals: {
        ...algorithm,
        retrieval: { type: 'choice', choice: 'hybrid', confidence: 0.8 }
      }
    }
  });
  assert.equal(mediumHybrid.retrieval, 'lexical');

  const highHybrid = router.route({
    state,
    decision: {
      algorithm: { answers: algorithm },
      signals: {
        ...algorithm,
        retrieval: { type: 'choice', choice: 'hybrid', confidence: 0.9 }
      }
    }
  });
  assert.equal(highHybrid.retrieval, 'hybrid');
  assert.equal(highHybrid.retrievalSource, 'decision');
});


test('retrieval relation applicability blocks model-only dependency and causal overreach', () => {
  const router = new CognitiveRouter({
    retrievalConfidenceThreshold: 0.68,
    expensiveRetrievalConfidenceThreshold: 0.82
  });

  const lexicalAlgorithm = {
    category: { type: 'choice', choice: 'deep', confidence: 0.8 },
    need_think: { type: 'noul', noul: 0.8 },
    evidence_sufficient: { type: 'noul', noul: 0.7 },
    stuck: { type: 'noul', noul: 0.1 },
    retrieval: { type: 'choice', choice: 'lexical', confidence: 0.65 }
  };

  const migration = router.route({
    state: {
      goal: 'Run the database schema migration safely in production',
      progress: {},
      context: {}
    },
    decision: {
      algorithm: { answers: lexicalAlgorithm },
      signals: {
        ...lexicalAlgorithm,
        retrieval: { type: 'choice', choice: 'dependency', confidence: 0.99 }
      }
    }
  });
  assert.equal(migration.retrieval, 'lexical');
  assert.equal(migration.retrievalSource, 'algorithm');
  assert.ok(migration.reasons.includes('retrieval-model-constrained'));

  const performance = router.route({
    state: {
      goal: 'Investigate performance of distributed transaction processing',
      progress: {},
      context: {}
    },
    decision: {
      algorithm: { answers: lexicalAlgorithm },
      signals: {
        ...lexicalAlgorithm,
        retrieval: { type: 'choice', choice: 'causal', confidence: 0.99 }
      }
    }
  });
  assert.equal(performance.retrieval, 'lexical');
  assert.equal(performance.retrievalSource, 'algorithm');

  const explicitDependency = router.route({
    state: {
      goal: 'Find where this symbol is imported and called',
      progress: {},
      context: {}
    },
    decision: {
      algorithm: { answers: lexicalAlgorithm },
      signals: {
        ...lexicalAlgorithm,
        retrieval: { type: 'choice', choice: 'dependency', confidence: 0.99 }
      }
    }
  });
  assert.equal(explicitDependency.retrieval, 'dependency');
  assert.equal(explicitDependency.retrievalSource, 'decision');

  const explicitCausal = router.route({
    state: {
      goal: 'Debug the root cause of a concurrency race',
      progress: {},
      context: {}
    },
    decision: {
      algorithm: { answers: lexicalAlgorithm },
      signals: {
        ...lexicalAlgorithm,
        retrieval: { type: 'choice', choice: 'causal', confidence: 0.99 }
      }
    }
  });
  assert.equal(explicitCausal.retrieval, 'causal');
  assert.equal(explicitCausal.retrievalSource, 'decision');
});

test('causal retrieval can become applicable from repeated failures or contradictions', () => {
  const router = new CognitiveRouter();
  const algorithm = {
    category: { type: 'choice', choice: 'ultrabrain', confidence: 0.9 },
    need_think: { type: 'noul', noul: 0.9 },
    evidence_sufficient: { type: 'noul', noul: 0.3 },
    stuck: { type: 'noul', noul: 0.9 },
    retrieval: { type: 'choice', choice: 'lexical', confidence: 0.65 }
  };

  const repeatedFailure = router.route({
    state: {
      goal: 'Try again',
      progress: { maxRepeatedFailure: 3 },
      context: {}
    },
    decision: {
      algorithm: { answers: algorithm },
      signals: {
        ...algorithm,
        retrieval: { type: 'choice', choice: 'causal', confidence: 0.95 }
      }
    }
  });
  assert.equal(repeatedFailure.retrieval, 'causal');

  const contradiction = router.route({
    state: {
      goal: 'Resolve this inconsistent behavior',
      progress: {},
      context: { contradictionCount: 2 }
    },
    decision: {
      algorithm: { answers: algorithm },
      signals: {
        ...algorithm,
        retrieval: { type: 'choice', choice: 'causal', confidence: 0.95 }
      }
    }
  });
  assert.equal(contradiction.retrieval, 'causal');
});


test('Generative DecisionProvider returns bounded typed answers and discounts self-reported confidence', async () => {
  const requests = [];
  const provider = {
    model: 'decision-fallback-model',
    async complete(request) {
      requests.push(request);
      return {
        model: 'decision-fallback-model',
        usage: { total_tokens: 42 },
        message: {
          role: 'assistant',
          content: [
            '```json',
            JSON.stringify({
              answers: {
                category: { type: 'choice', choice: 'deep', confidence: 0.9 },
                need_think: { type: 'noul', noul: 0.8 },
                evidence_sufficient: { type: 'noul', noul: 0.3 },
                stuck: { type: 'noul', noul: 0.2 },
                retrieval: { type: 'choice', choice: 'causal', confidence: 0.95 }
              }
            }),
            '```'
          ].join('\n')
        }
      };
    }
  };
  const decision = new GenerativeDecisionProvider({
    provider,
    confidenceScale: 0.85,
    confidenceCap: 0.9,
    scoreScale: 0.85
  });
  const questions = {
    category: {
      type: 'choice',
      criteria: { general: 'normal', deep: 'hard' }
    },
    need_think: { type: 'noul' },
    evidence_sufficient: { type: 'noul' },
    stuck: { type: 'noul' },
    retrieval: {
      type: 'choice',
      criteria: { lexical: 'default', causal: 'root cause' }
    }
  };

  const result = await decision.decide({
    state: { goal: 'Debug the root cause of a race' },
    questions
  });

  assert.equal(requests.length, 1);
  assert.equal(requests[0].tools.length, 0);
  assert.equal(requests[0].toolChoice, 'none');
  assert.equal(requests[0].reasoningEffort, 'low');
  assert.match(requests[0].messages[0].content, /bounded Decision Layer/);
  assert.match(requests[0].messages[0].content, /Do not execute the task/);
  assert.equal(result.answers.category.choice, 'deep');
  assert.equal(result.answers.category.confidence, 0.765);
  assert.equal(result.answers.retrieval.choice, 'causal');
  assert.ok(Math.abs(result.answers.retrieval.confidence - 0.8075) < 1e-9);
  assert.ok(Math.abs(result.answers.need_think.noul - 0.755) < 1e-9);
  assert.ok(Math.abs(result.answers.evidence_sufficient.noul - 0.33) < 1e-9);
  assert.equal(result.usage.total_tokens, 42);
});

test('Decision Layer can fall through System One failure to a generative decision model', async () => {
  const first = {
    name: 'laya',
    model: 'local-laya',
    async decide() {
      const error = new Error('laya unavailable');
      error.status = 503;
      throw error;
    }
  };
  const second = new GenerativeDecisionProvider({
    name: 'generative:deepseek',
    provider: {
      model: 'deepseek-flash',
      async complete() {
        return {
          message: {
            role: 'assistant',
            content: JSON.stringify({
              answers: {
                category: { type: 'choice', choice: 'deep', confidence: 0.95 },
                need_think: { type: 'noul', noul: 0.9 },
                retrieval: { type: 'choice', choice: 'causal', confidence: 0.95 }
              }
            })
          }
        };
      }
    }
  });
  const layer = new DecisionLayer({
    providers: [first, second],
    policy: 'first'
  });
  const summary = await layer.decide({
    state: { goal: 'Debug the root cause' },
    questions: {
      category: { type: 'choice', criteria: { general: 'normal', deep: 'hard' } },
      need_think: { type: 'noul' },
      retrieval: { type: 'choice', criteria: { lexical: 'default', causal: 'root cause' } }
    }
  });

  assert.equal(summary.models.length, 1);
  assert.equal(summary.models[0].source, 'generative:deepseek');
  assert.equal(summary.errors.length, 1);
  assert.equal(summary.errors[0].provider, 'laya');
  assert.equal(summary.signals.category.choice, 'deep');
});

test('malformed generative decision output fails closed and participates in circuit fallback', async () => {
  let badCalls = 0;
  const bad = new GenerativeDecisionProvider({
    name: 'bad-generative',
    provider: {
      model: 'bad-model',
      async complete() {
        badCalls += 1;
        return {
          message: { role: 'assistant', content: 'not-json' }
        };
      }
    }
  });
  const good = {
    name: 'good-system-one',
    model: 'good-model',
    async decide() {
      return {
        source: 'good-system-one',
        answers: {
          category: { type: 'choice', choice: 'general', confidence: 0.8 }
        }
      };
    }
  };
  const health = new ProviderHealthRegistry({
    failureThreshold: 1,
    cooldownMs: 10000
  });
  const layer = new DecisionLayer({
    providers: [bad, good],
    healthRegistry: health
  });

  const first = await layer.decide({
    state: { goal: 'normal task' },
    questions: {
      category: { type: 'choice', criteria: { general: 'normal' } }
    }
  });
  const second = await layer.decide({
    state: { goal: 'normal task' },
    questions: {
      category: { type: 'choice', criteria: { general: 'normal' } }
    }
  });

  assert.equal(badCalls, 1);
  assert.equal(first.models[0].source, 'good-system-one');
  assert.equal(second.errors[0].skipped, true);
  assert.equal(health.snapshot()['decision:bad-generative:bad-model'].available, false);
});

test('createDecisionProvider builds a generative fallback from an ordinary execution provider', async () => {
  const factoryCalls = [];
  const provider = createDecisionProvider({
    type: 'generative',
    name: 'fallback-router',
    provider: 'mock-provider',
    model: 'router-model',
    reasoningEffort: 'low',
    maxTokens: 321
  }, {
    providerFactory(name, options) {
      factoryCalls.push({ name, options });
      return {
        model: options.model,
        async complete() {
          return {
            message: {
              role: 'assistant',
              content: JSON.stringify({
                answers: {
                  category: { type: 'choice', choice: 'general', confidence: 0.9 }
                }
              })
            }
          };
        }
      };
    }
  });

  assert.equal(provider.name, 'fallback-router');
  assert.equal(factoryCalls[0].name, 'mock-provider');
  assert.equal(factoryCalls[0].options.model, 'router-model');

  const result = await provider.decide({
    state: { goal: 'small task' },
    questions: {
      category: { type: 'choice', criteria: { general: 'normal' } }
    }
  });
  assert.equal(result.answers.category.choice, 'general');
  assert.ok(result.answers.category.confidence < 0.9);
});
