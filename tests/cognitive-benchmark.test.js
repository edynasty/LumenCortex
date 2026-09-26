import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import {
  buildCognitiveRoutingPredictionPrompt,
  loadCognitiveRoutingBenchmark,
  loadCognitiveRoutingPredictions,
  runCognitiveRoutingBenchmark,
  scoreCognitiveRoutingPredictions
} from '../src/cognitive-benchmark.js';

test('default cognitive routing benchmark matches labeled product expectations', () => {
  const fixture = loadCognitiveRoutingBenchmark();
  const result = runCognitiveRoutingBenchmark(fixture);

  assert.equal(result.total, 12);
  assert.equal(result.failed, 0, JSON.stringify(
    result.cases.filter((item) => !item.pass),
    null,
    2
  ));
  assert.equal(result.ok, true);
  assert.equal(result.metrics.category.accuracy, 1);
  assert.equal(result.metrics.think.accuracy, 1);
  assert.equal(result.metrics.effort.accuracy, 1);
  assert.equal(result.metrics.retrieval.accuracy, 1);
});

test('cognitive routing benchmark reports dimension-specific regressions', () => {
  const result = runCognitiveRoutingBenchmark([
    {
      id: 'intentional-regression',
      state: { goal: 'Fix a typo in README' },
      expect: {
        category: 'ultrabrain',
        think: true,
        effort: 'max',
        retrieval: 'causal'
      }
    }
  ]);

  assert.equal(result.ok, false);
  assert.equal(result.failed, 1);
  assert.deepEqual(result.cases[0].checks, {
    category: false,
    think: false,
    effort: false,
    retrieval: false
  });
  assert.equal(result.metrics.category.accuracy, 0);
});


test('external routing predictions can be scored against the same labeled fixture', () => {
  const fixture = loadCognitiveRoutingBenchmark();
  const predictions = fixture.cases.map((entry) => ({
    id: entry.id,
    category: Array.isArray(entry.expect.category) ? entry.expect.category[0] : entry.expect.category,
    think: entry.expect.think,
    effort: entry.expect.effort ?? entry.expect.effortAtLeast,
    retrieval: Array.isArray(entry.expect.retrieval) ? entry.expect.retrieval[0] : entry.expect.retrieval
  }));

  const result = scoreCognitiveRoutingPredictions(fixture, {
    source: 'inline-perfect',
    predictions
  });

  assert.equal(result.mode, 'predictions');
  assert.equal(result.ok, true);
  assert.equal(result.failed, 0);
  assert.equal(result.missing, 0);
  assert.deepEqual(result.extraPredictionIds, []);
  assert.equal(result.metrics.category.accuracy, 1);
});

test('external prediction scoring fails closed on missing cases and reports extras', () => {
  const fixture = loadCognitiveRoutingBenchmark();
  const result = scoreCognitiveRoutingPredictions(fixture, {
    source: 'partial',
    predictions: [
      {
        id: fixture.cases[0].id,
        category: fixture.cases[0].expect.category,
        think: fixture.cases[0].expect.think,
        effort: fixture.cases[0].expect.effort,
        retrieval: fixture.cases[0].expect.retrieval
      },
      {
        id: 'extra-case',
        category: 'general',
        think: false,
        effort: 'low',
        retrieval: 'lexical'
      }
    ]
  });

  assert.equal(result.ok, false);
  assert.equal(result.missing, fixture.cases.length - 1);
  assert.deepEqual(result.extraPredictionIds, ['extra-case']);
  assert.equal(result.cases[1].missing, true);
  assert.equal(result.cases[1].pass, false);
});

test('prediction loader accepts fenced JSONL output from external model runners', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'lcx-routing-predictions-'));
  const file = path.join(root, 'predictions.md');
  fs.writeFileSync(file, [
    '```json',
    '{"id":"a","category":"quick","think":false,"effort":"low","retrieval":"lexical"}',
    '```',
    '```json',
    '{"id":"b","category":"deep","think":true,"effort":"high","retrieval":"causal"}',
    '```'
  ].join('\n'));

  const loaded = loadCognitiveRoutingPredictions(file);
  assert.equal(loaded.predictions.length, 2);
  assert.equal(loaded.predictions[1].id, 'b');
});


test('external cognitive routing prompt reuses canonical runtime rubric and omits labels', () => {
  const fixture = loadCognitiveRoutingBenchmark();
  const prompt = buildCognitiveRoutingPredictionPrompt(fixture, {
    ids: ['quick-readme-typo', 'deep-concurrency-root-cause']
  });

  assert.match(prompt, /lexical is the conservative default/i);
  assert.match(prompt, /Do not upgrade retrieval merely because the task is difficult/i);
  assert.match(prompt, /quick-readme-typo/);
  assert.match(prompt, /deep-concurrency-root-cause/);
  assert.doesNotMatch(prompt, /visual-responsive-wails/);
  assert.doesNotMatch(prompt, /"expect"/);
});
