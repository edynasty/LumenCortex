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

export function loadCognitiveRoutingPredictions(file) {
  const absolute = path.resolve(file);
  const raw = fs.readFileSync(absolute, 'utf8').trim();
  if (!raw) throw new Error('Cognitive routing predictions file is empty');

  let parsed;
  try {
    parsed = JSON.parse(raw);
  } catch {
    const lines = raw
      .split(/\r?\n/)
      .map((line) => line.trim())
      .filter((line) => line && !line.startsWith('\\`\\`\\`'));
    parsed = lines.map((line, index) => {
      try { return JSON.parse(line); }
      catch (error) {
        throw new Error(`Invalid cognitive routing prediction JSONL at line ${index + 1}: ${error.message}`);
      }
    });
  }

  const predictions = Array.isArray(parsed)
    ? parsed
    : Array.isArray(parsed?.predictions)
      ? parsed.predictions
      : [];
  if (!predictions.length) throw new Error('Cognitive routing predictions require at least one prediction');
  return {
    source: absolute,
    predictions
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
    mode: 'deterministic',
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

export function scoreCognitiveRoutingPredictions(benchmark, predictionInput) {
  const fixture = Array.isArray(benchmark)
    ? { version: 1, name: 'inline', source: null, cases: benchmark }
    : benchmark;
  if (!Array.isArray(fixture?.cases) || !fixture.cases.length) {
    throw new Error('Cognitive routing benchmark requires cases');
  }

  const predictions = Array.isArray(predictionInput)
    ? predictionInput
    : predictionInput?.predictions;
  if (!Array.isArray(predictions) || !predictions.length) {
    throw new Error('Cognitive routing predictions require predictions');
  }

  const byId = new Map(
    predictions
      .filter((item) => item?.id)
      .map((item) => [String(item.id), item])
  );

  const results = fixture.cases.map((entry, index) => {
    const id = String(entry?.id ?? `case-${index + 1}`);
    const prediction = byId.get(id) ?? null;
    const expected = entry?.expect ?? {};
    const actual = prediction ? normalizePrediction(prediction) : null;
    const checks = actual
      ? checksForActual(expected, actual)
      : Object.fromEntries(
          expectedDimensions(expected).map((name) => [name, false])
        );
    return {
      id,
      description: String(entry?.description ?? ''),
      pass: Boolean(actual) && Object.values(checks).every(Boolean),
      missing: !actual,
      checks,
      expected: structuredClone(expected),
      actual
    };
  });

  const fixtureIds = new Set(results.map((item) => item.id));
  const extraPredictionIds = predictions
    .map((item) => String(item?.id ?? ''))
    .filter((id) => id && !fixtureIds.has(id));
  const passed = results.filter((item) => item.pass).length;

  return {
    mode: 'predictions',
    version: Number(fixture.version ?? 1),
    name: String(fixture.name ?? 'cognitive-routing'),
    source: fixture.source ?? null,
    predictionSource: predictionInput?.source ?? null,
    total: results.length,
    passed,
    failed: results.length - passed,
    missing: results.filter((item) => item.missing).length,
    passRate: results.length ? passed / results.length : 0,
    ok: passed === results.length,
    metrics: aggregateMetrics(results),
    extraPredictionIds,
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
  const actual = {
    category: route.category,
    think: route.think,
    effort: route.effort,
    retrieval: route.retrieval,
    thinkScore: route.thinkScore,
    reasons: route.reasons
  };
  const checks = checksForActual(expected, actual);
  const pass = Object.values(checks).every(Boolean);
  return {
    id,
    description: String(entry?.description ?? ''),
    pass,
    checks,
    expected: structuredClone(expected),
    actual,
    state
  };
}

function checksForActual(expected, actual) {
  const checks = {};
  if (expected.category !== undefined) {
    checks.category = allowed(expected.category).includes(String(actual.category));
  }
  if (expected.think !== undefined) {
    checks.think = Boolean(actual.think) === Boolean(expected.think);
  }
  if (expected.effort !== undefined) {
    checks.effort = String(actual.effort) === String(expected.effort);
  } else if (expected.effortAtLeast !== undefined) {
    checks.effort = effortAtLeast(actual.effort, expected.effortAtLeast);
  }
  if (expected.retrieval !== undefined) {
    checks.retrieval = allowed(expected.retrieval).includes(String(actual.retrieval));
  }
  return checks;
}

function expectedDimensions(expected) {
  const out = [];
  if (expected.category !== undefined) out.push('category');
  if (expected.think !== undefined) out.push('think');
  if (expected.effort !== undefined || expected.effortAtLeast !== undefined) out.push('effort');
  if (expected.retrieval !== undefined) out.push('retrieval');
  return out;
}

function normalizePrediction(input) {
  return {
    category: String(input.category ?? ''),
    think: Boolean(input.think),
    effort: String(input.effort ?? ''),
    retrieval: String(input.retrieval ?? '')
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
