import test from 'node:test';
import assert from 'node:assert/strict';
import { EventEmitter } from 'node:events';
import { withProcessCancellation } from '../src/process-cancellation.js';

test('process signals abort an active run with AbortError and clean listeners', async () => {
  const source = new EventEmitter();
  let observedSignal;

  const pending = withProcessCancellation((signal) => new Promise((resolve, reject) => {
    observedSignal = signal;
    signal.addEventListener('abort', () => reject(signal.reason), { once: true });
  }), {
    source,
    signals: ['SIGINT', 'SIGTERM']
  });

  assert.equal(source.listenerCount('SIGINT'), 1);
  assert.equal(source.listenerCount('SIGTERM'), 1);
  source.emit('SIGINT');

  await assert.rejects(
    pending,
    (error) => error?.name === 'AbortError' && /SIGINT/.test(error.message)
  );
  assert.equal(observedSignal.aborted, true);
  assert.equal(source.listenerCount('SIGINT'), 0);
  assert.equal(source.listenerCount('SIGTERM'), 0);
});

test('successful runs also remove process signal listeners', async () => {
  const source = new EventEmitter();
  const result = await withProcessCancellation(async (signal) => {
    assert.equal(signal.aborted, false);
    return 'done';
  }, {
    source,
    signals: ['SIGINT']
  });

  assert.equal(result, 'done');
  assert.equal(source.listenerCount('SIGINT'), 0);
});
