export class ParallelSessionRunner {
  constructor({ subagentPool, concurrency = 4 } = {}) {
    if (!subagentPool) throw new Error('subagentPool is required');
    this.pool = subagentPool;
    this.concurrency = Math.max(1, Number(concurrency));
  }

  async runTasks(tasks, options = {}) {
    const normalized = normalizeTasks(tasks);
    if (!normalized.length) return [];
    const previous = this.pool.concurrency;
    this.pool.concurrency = Number(options.concurrency ?? this.concurrency);
    try {
      return await this.pool.runMany(normalized, {
        ...options,
        toolAllowlist: options.toolAllowlist
      });
    } finally {
      this.pool.concurrency = previous;
    }
  }
}

export function loadParallelTasks(value) {
  if (Array.isArray(value)) return normalizeTasks(value);
  if (typeof value === 'string') {
    const parsed = JSON.parse(value);
    return normalizeTasks(parsed);
  }
  return [];
}

function normalizeTasks(tasks) {
  if (!Array.isArray(tasks)) throw new Error('Parallel tasks must be an array');
  return tasks.map((task) => {
    if (typeof task === 'string') return { goal: task };
    if (!task?.goal) throw new Error('Each parallel task requires goal');
    return {
      goal: String(task.goal),
      role: task.role ? String(task.role) : undefined,
      maxSteps: task.maxSteps,
      budgetTokens: task.budgetTokens,
      toolAllowlist: task.toolAllowlist
    };
  });
}
