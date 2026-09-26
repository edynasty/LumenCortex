import test from 'node:test';
import assert from 'node:assert/strict';
import {
  loadCognitiveRoutingBenchmark,
  runCognitiveRoutingBenchmark
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
