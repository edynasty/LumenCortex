import { AgentLoop } from './agent.js';

const DEFAULT_READ_TOOLS = [
  'read_file', 'list_dir', 'code_search',
  'lsp_definition', 'lsp_references', 'lsp_symbols', 'lsp_hover', 'lsp_diagnostics',
  'lumencortex_context'
];

export class SubagentPool {
  constructor({ provider, repository, runtime, workspace, tools, sessionStore, authorize, onEvent, concurrency = 4 } = {}) {
    this.provider = provider;
    this.repository = repository;
    this.runtime = runtime;
    this.workspace = workspace;
    this.tools = tools;
    this.sessionStore = sessionStore;
    this.authorize = authorize;
    this.onEvent = onEvent ?? (() => {});
    this.concurrency = Math.max(1, Number(concurrency));
  }

  async run(task, options = {}) {
    const goal = typeof task === 'string' ? task : task.goal;
    if (!goal) throw new Error('Subagent goal is required');
    const role = typeof task === 'string' ? options.role : task.role ?? options.role;
    const requestedAllowlist = task.toolAllowlist ?? options.toolAllowlist ?? DEFAULT_READ_TOOLS;
    const available = new Set(this.tools.schemas().map((schema) => schema.function.name));
    const allowlist = requestedAllowlist.filter((name) => available.has(name));
    const systemPrompt = [
      `You are a focused LumenCortex subagent${role ? ` acting as ${role}` : ''}.`,
      'Solve only the delegated goal. Inspect evidence, use tools, and return a concise finding with evidence paths.',
      'Do not broaden scope. Do not spawn more subagents.'
    ].join(' ');

    const agent = new AgentLoop({
      provider: this.provider,
      repository: this.repository,
      runtime: this.runtime,
      workspace: this.workspace,
      tools: this.tools,
      sessionStore: this.sessionStore,
      authorize: this.authorize,
      onEvent: (event) => this.onEvent({ ...event, subagent: true, role })
    });

    const result = await agent.run(goal, {
      providerName: options.providerName,
      maxSteps: Number(task.maxSteps ?? options.maxSteps ?? 10),
      budgetTokens: Number(task.budgetTokens ?? options.budgetTokens ?? 12000),
      recentRounds: Number(task.recentRounds ?? options.recentRounds ?? 3),
      workingChars: Number(task.workingChars ?? options.workingChars ?? 32000),
      maxTokens: Number(task.maxTokens ?? options.maxTokens ?? 1000),
      toolAllowlist: allowlist,
      maxToolCallsPerStep: Number(task.maxToolCallsPerStep ?? 2),
      autoPromote: false,
      autoIngest: false,
      recordTask: false,
      recordObservations: false,
      systemPrompt,
      authorize: this.authorize
    });

    return {
      goal,
      role: role ?? null,
      sessionId: result.session.id,
      final: result.final,
      usage: result.usage,
      steps: result.session.steps.length
    };
  }

  async runMany(tasks, options = {}) {
    const items = tasks.map((task, index) => ({ task, index }));
    const results = new Array(items.length);
    let cursor = 0;
    const workers = Array.from({ length: Math.min(this.concurrency, items.length || 1) }, async () => {
      while (true) {
        const current = cursor++;
        if (current >= items.length) return;
        try {
          results[current] = { ok: true, ...(await this.run(items[current].task, options)) };
        } catch (error) {
          results[current] = {
            ok: false,
            goal: typeof items[current].task === 'string' ? items[current].task : items[current].task.goal,
            error: error.message,
            sessionId: error.sessionId ?? null
          };
        }
      }
    });
    await Promise.all(workers);
    return results;
  }
}

export function registerSubagentTools(registry, pool) {
  registry.register({
    name: 'subagent_run',
    description: 'Delegate one focused read-oriented investigation to an isolated subagent session.',
    permission: 'read',
    parameters: {
      type: 'object',
      properties: {
        goal: { type: 'string' },
        role: { type: 'string' },
        max_steps: { type: 'integer', minimum: 1, maximum: 20 }
      },
      required: ['goal'],
      additionalProperties: false
    },
    execute: ({ goal, role, max_steps }) => pool.run({ goal, role, maxSteps: max_steps })
  });

  registry.register({
    name: 'subagent_parallel',
    description: 'Run several independent read-oriented investigations in parallel and return all findings.',
    permission: 'read',
    parameters: {
      type: 'object',
      properties: {
        tasks: {
          type: 'array',
          minItems: 1,
          maxItems: 8,
          items: {
            type: 'object',
            properties: { goal: { type: 'string' }, role: { type: 'string' } },
            required: ['goal'],
            additionalProperties: false
          }
        }
      },
      required: ['tasks'],
      additionalProperties: false
    },
    execute: ({ tasks }) => pool.runMany(tasks)
  });

  return registry;
}
