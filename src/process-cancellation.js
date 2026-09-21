export async function withProcessCancellation(run, {
  source = process,
  signals = ['SIGINT', 'SIGTERM']
} = {}) {
  if (typeof run !== 'function') throw new Error('run callback is required');
  const controller = new AbortController();
  const handlers = new Map();

  for (const signalName of signals) {
    const handler = () => {
      if (controller.signal.aborted) return;
      const error = new Error(`Agent run interrupted by ${signalName}`);
      error.name = 'AbortError';
      controller.abort(error);
    };
    handlers.set(signalName, handler);
    source.on(signalName, handler);
  }

  try {
    return await run(controller.signal);
  } finally {
    for (const [signalName, handler] of handlers) {
      source.off(signalName, handler);
    }
  }
}
