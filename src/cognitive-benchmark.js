import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { algorithmicAnswers, CognitiveRouter } from './cognitive-control.js';

export const DEFAULT_COGNITIVE_ROUTING_BENCHMARK = fileURLToPath(
  new URL('../benchmarks/cognitive-routing.json', import.meta.url)
);

const EFFORT_RANK = Object.freeze({
  low: 0,
  medium: 1,
  high: 2,
  max: 3
});

export function loadCognitiveRoutingBenchmark(file = DEFAULT_COGNITIVE_ROUTING_BENCHMARK) {
  const absolute = path.resolve(file);
  const parsed = JSON.parse(fs.readFileSync(absolute, 'utf8'));
  if (!Array.isArray(parsed?.cases) || !parsed.cases.length) {
    throw new Error('Cognitive routing benchmark requires at least one case');
  }
  return {
    version: Number(parsed.version ?? 1),
    name: String(parsed.name ?? path.basename(absolute)),
    source: absolute,
    cases: parsed.cases
  };
}

export function runCognitiveRoutingBenchmark(benchmark, options = {}) {
  const fixture = Array.isArray(benchmark)
    ? { version: 1, name: 'inline', source: null, cases: benchmark }
    : benchmark;
  if (!Array.isArray(fixture?.cases) || !fixture.cases.length) {
    throw new Error('Cognitive routing benchmark requires cases');
  }

  const router = options.router ?? new CognitiveRouter(options.routerOptions);
  const results = fixture.cases.map((entry, index) => evaluateCase(entry, index, router));
  const passed = results.filter((item) => item.pass).length;
  const metrics = aggregateMetrics(results);

  return {
    version: Number(fixture.version ?? 1),
    name: String(fixture.name ?? 'cognitive-routing'),
    source: fixture.source ?? null,
    total: results.length,
    passed,
    failed: results.length - passed,
    passRate: results.length ? passed / results.length : 0,
    ok: passed === results.length,
    metrics,
    cases: results
  };
}

function evaluateCase(entry, index, router) {
  const id = String(entry?.id ?? `case-${index + 1}`);
  const state = normalizeBenchmarkState(entry?.state ?? {});
  const answers = algorithmicAnswers(state);
  const decision = {
    algorithm: {
      source: 'algorithm',
      answers
    },
    models: [],
    errors: [],
    signals: answers
  };
  const route = router.route({ state, decision });
  const expected = entry?.expect ?? {};
  const checks = {};

  if (expected.category !== undefined) {
    checks.category = allowed(expected.category).includes(route.category);
  }
  if (expected.think !== undefined) {
    checks.think = route.think === Boolean(expected.think);
  }
  if (expected.effort !== undefined) {
    checks.effort = route.effort === String(expected.effort);
  } else if (expected.effortAtLeast !== undefined) {
    checks.effort = effortAtLeast(route.effort, expected.effortAtLeast);
  }
  if (expected.retrieval !== undefined) {
    checks.retrieval = allowed(expected.retrieval).includes(route.retrieval);
  }

  const pass = Object.values(checks).every(Boolean);
  return {
    id,
    description: String(entry?.description ?? ''),
    pass,
    checks,
    expected: structuredClone(expected),
    actual: {
      category: route.category,
      think: route.think,
      effort: route.effort,
      retrieval: route.retrieval,
      thinkScore: route.thinkScore,
      reasons: route.reasons
    },
    state
  };
}

function normalizeBenchmarkState(input) {
  return {
    goal: String(input.goal ?? ''),
    focus: String(input.focus ?? ''),
    step: Number(input.step ?? 0),
    progress: {
      sameFailureCount: Number(input.progress?.sameFailureCount ?? 0),
      noProgressSteps: Number(input.progress?.noProgressSteps ?? 0),
      successfulActions: Number(input.progress?.successfulActions ?? 0),
      distinctFailureCount: Number(input.progress?.distinctFailureCount ?? 0),
      maxRepeatedFailure: Number(input.progress?.maxRepeatedFailure ?? 0)
    },
    workUnit: input.workUnit ?? null,
    context: {
      selectedNodeCount: Number(input.context?.selectedNodeCount ?? 0),
      usedTokens: Number(input.context?.usedTokens ?? 0),
      budgetTokens: Number(input.context?.budgetTokens ?? 0),
      contradictionCount: Number(input.context?.contradictionCount ?? 0)
    },
    recent: Array.isArray(input.recent) ? structuredClone(input.recent) : []
  };
}

function aggregateMetrics(results) {
  const names = ['category', 'think', 'effort', 'retrieval'];
  return Object.fromEntries(names.map((name) => {
    const relevant = results.filter((item) => Object.hasOwn(item.checks, name));
    const passed = relevant.filter((item) => item.checks[name]).length;
    return [name, {
      total: relevant.length,
      passed,
      failed: relevant.length - passed,
      accuracy: relevant.length ? passed / relevant.length : null
    }];
  }));
}

function allowed(value) {
  return Array.isArray(value) ? value.map(String) : [String(value)];
}

function effortAtLeast(actual, expected) {
  const actualRank = EFFORT_RANK[String(actual)];
  const expectedRank = EFFORT_RANK[String(expected)];
  if (actualRank === undefined || expectedRank === undefined) return false;
  return actualRank >= expectedRank;
}
