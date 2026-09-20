import { ingestWorkspace } from './ingest.js';
import { createCodingTools } from './tools.js';
import { AgentSessionStore } from './session.js';
import { PromotionController } from './promotion-controller.js';
import { estimateTokens, hash, nowIso } from './util.js';

const DEFAULT_SYSTEM_PROMPT = `You are ModelWeave Agent, an autonomous coding agent operating inside a versioned cognitive graph.

Rules:
1. Inspect before editing. Prefer targeted read/search/context tools over broad exploration.
2. Treat repository/runtime observations as evidence; never present an unverified model inference as fact.
3. Use tools iteratively until the requested outcome is implemented and verified.
4. After editing, run the narrowest relevant test/build/check. Inspect failures and continue the loop.
5. Do not stop at a plan when the user asked for implementation.
6. Avoid repeated reads when the active cognitive context already contains the answer.
7. If context is insufficient, call modelweave_context with a focused sub-question.
8. Keep changes scoped to the user's goal. Do not modify unrelated files.
9. Before finishing, inspect the resulting diff/status when practical.
10. Return a concise final result with what changed and what verification passed.`;

export class AgentLoop {
  constructor({ provider, repository, runtime, workspace, tools, sessionStore, promotionController, authorize, onEvent } = {}) {
    if (!provider) throw new Error('provider is required');
    if (!repository) throw new Error('repository is required');
    if (!runtime) throw new Error('runtime is required');
    this.provider = provider;
    this.repository = repository;
    this.runtime = runtime;
    this.workspace = workspace;
    this.tools = tools ?? createCodingTools({ workspace, repository, runtime });
    this.sessions = sessionStore ?? new AgentSessionStore(repository.dir);
    this.promotionController = promotionController ?? new PromotionController(runtime);
    this.authorize = authorize;
    this.onEvent = onEvent ?? (() => {});
  }

  async run(goal, options = {}) {
    const maxSteps = Number(options.maxSteps ?? 24);
    const budgetTokens = Number(options.budgetTokens ?? 24000);
    const recentRounds = Number(options.recentRounds ?? 4);
    const workingChars = Number(options.workingChars ?? options.maxWorkingChars ?? 48000);
    const llmRetries = Math.max(0, Number(options.llmRetries ?? 2));
    const retryBaseMs = Math.max(0, Number(options.retryBaseMs ?? 800));
    let session;

    if (options.sessionId) {
      session = this.sessions.load(options.sessionId);
      session.status = 'running';
      if (goal) {
        session.goal = goal;
        session.messages.push({ role: 'user', content: goal });
      }
    } else {
      session = this.sessions.create({
        goal,
        provider: options.providerName ?? null,
        model: this.provider.model,
        messages: [{ role: 'user', content: goal }],
        metadata: {
          budgetTokens,
          maxSteps,
          recentRounds,
          workingChars,
          systemPrompt: options.systemPrompt ?? DEFAULT_SYSTEM_PROMPT,
          activationCounts: {},
          recentObservationNodeIds: [],
          promotions: [],
          contextHistory: []
        }
      });
    }

    normalizeSessionMetadata(session, {
      budgetTokens,
      maxSteps,
      recentRounds,
      workingChars,
      systemPrompt: options.systemPrompt ?? DEFAULT_SYSTEM_PROMPT
    });

    const usage = { promptTokens: 0, completionTokens: 0, totalTokens: 0, requests: 0 };
    let lastContext = null;

    for (let step = 1; step <= maxSteps; step += 1) {
      const focus = deriveFocus(session, goal, step);
      const seedNodeIds = session.metadata.recentObservationNodeIds.slice(-8);
      let context = this.runtime.context(focus, { budgetTokens, seedNodeIds });
      updateActivationCounts(session, context);
      const promotionResult = options.autoPromote === false
        ? { promoted: false }
        : this.promotionController.maybePromote(focus, context, {
            step,
            activationCounts: session.metadata.activationCounts,
            metadata: { sessionId: session.id }
          });
      const promotion = promotionResult.promoted ? promotionResult.abstraction : null;
      if (promotion) {
        context = this.runtime.context(focus, {
          budgetTokens,
          seedNodeIds: [promotion.id, ...seedNodeIds]
        });
        const record = {
          abstractionId: promotion.id,
          step,
          pressure: promotionResult.assessment?.pressure ?? 0,
          reasons: promotionResult.assessment?.reasons ?? [],
          childIds: promotion.childIds ?? []
        };
        session.metadata.promotions.push(record);
        this.emit('context.promote', {
          sessionId: session.id,
          step,
          abstractionId: promotion.id,
          childCount: promotion.childIds?.length ?? 0,
          reasons: record.reasons
        });
      }
      lastContext = context;
      session.metadata.contextHistory.push({
        step,
        at: nowIso(),
        focus,
        nodeIds: context.selectedNodes.map((node) => node.id),
        usedTokens: context.usedTokens,
        budgetTokens: context.budgetTokens,
        promotionId: promotion?.id ?? null
      });

      if (step === 1) {
        this.emit('session.start', {
          sessionId: session.id,
          goal,
          budgetTokens,
          maxSteps,
          selectedNodes: context.selectedNodes.length,
          contextTokens: context.usedTokens
        });
      } else {
        this.emit('context.move', {
          sessionId: session.id,
          step,
          focus,
          selectedNodes: context.selectedNodes.length,
          contextTokens: context.usedTokens
        });
      }

      const requestMessages = buildWorkingMessages(session, context, {
        systemPrompt: session.metadata.systemPrompt,
        recentRounds,
        workingChars
      });

      this.emit('llm.request', {
        sessionId: session.id,
        step,
        model: this.provider.model,
        workingMessages: requestMessages.length,
        workingTokens: requestMessages.reduce((sum, message) => sum + estimateTokens(message), 0)
      });

      let response;
      try {
        response = await completeWithRetry(
          this.provider,
          {
            messages: requestMessages,
            tools: this.tools.schemas(),
            toolChoice: 'auto',
            temperature: options.temperature,
            maxTokens: options.maxTokens
          },
          {
            retries: llmRetries,
            retryBaseMs,
            onAttempt: () => { usage.requests += 1; },
            onRetry: ({ attempt, delayMs, error }) => this.emit('llm.retry', {
              sessionId: session.id,
              step,
              attempt,
              delayMs,
              error: error.message,
              status: error.status ?? null
            })
          }
        );
      } catch (error) {
        session.status = 'interrupted';
        session.error = {
          at: nowIso(),
          step,
          name: error.name,
          message: error.message,
          status: error.status ?? null
        };
        session.usage = usage;
        this.sessions.save(session);
        this.emit('session.interrupted', {
          sessionId: session.id,
          step,
          error: error.message,
          status: error.status ?? null
        });
        error.sessionId ??= session.id;
        throw error;
      }
      addUsage(usage, response.usage);
      const assistant = response.message;
      session.messages.push(assistant);
      const calls = assistant.tool_calls ?? [];
      const stepRecord = {
        step,
        at: nowIso(),
        focus,
        contextNodeIds: context.selectedNodes.map((node) => node.id),
        contextTokens: context.usedTokens,
        promotionId: promotion?.id ?? null,
        finishReason: response.finishReason,
        toolCalls: [],
        content: assistant.content ?? ''
      };

      if (!calls.length) {
        if (!assistant.content) throw new Error('Model returned neither tool calls nor final content');
        session.status = 'completed';
        session.final = assistant.content;
        session.usage = usage;
        session.steps.push(stepRecord);
        this.sessions.save(session);
        this.recordTask(session, context, options);
        this.emit('session.complete', { sessionId: session.id, step, usage, final: assistant.content });
        return { session, final: assistant.content, usage, context };
      }

      let workspaceMutated = false;
      this.emit('tools.requested', {
        sessionId: session.id,
        step,
        count: calls.length,
        names: calls.map((call) => call.function?.name)
      });

      for (const call of calls) {
        const name = call.function?.name;
        let parsed;
        try { parsed = JSON.parse(call.function?.arguments || '{}'); }
        catch (error) { parsed = { __parse_error: error.message, __raw: call.function?.arguments }; }

        let result;
        if (parsed.__parse_error) {
          result = {
            ok: false,
            content: JSON.stringify({
              error: `Invalid tool arguments JSON: ${parsed.__parse_error}`,
              raw: parsed.__raw
            })
          };
        } else {
          this.emit('tool.start', {
            sessionId: session.id,
            step,
            toolCallId: call.id,
            name,
            args: parsed
          });
          result = await this.tools.execute(name, parsed, {
            workspace: this.workspace,
            repository: this.repository,
            runtime: this.runtime,
            authorize: options.authorize ?? this.authorize
          });
          this.emit('tool.end', {
            sessionId: session.id,
            step,
            toolCallId: call.id,
            name,
            ok: result.ok,
            denied: result.denied,
            permission: result.permission
          });
        }

        workspaceMutated ||= Boolean(result.mutatesWorkspace && result.ok);
        const observationId = recordToolObservation(this.repository, {
          sessionId: session.id,
          step,
          call,
          name,
          args: parsed,
          result
        });
        if (observationId) {
          session.metadata.recentObservationNodeIds.push(observationId);
          session.metadata.recentObservationNodeIds = session.metadata.recentObservationNodeIds.slice(-16);
        }

        stepRecord.toolCalls.push({
          id: call.id,
          name,
          args: parsed,
          ok: result.ok,
          denied: result.denied ?? false,
          observationId
        });
        session.messages.push({
          role: 'tool',
          tool_call_id: call.id,
          name,
          content: result.content
        });
      }

      if (workspaceMutated && options.autoIngest !== false) {
        const refreshed = ingestWorkspace(this.repository.graph().snapshot(), this.workspace);
        this.repository.writeGraph(refreshed.graph);
        stepRecord.ingest = refreshed.stats;
        this.emit('context.ingest', { sessionId: session.id, step, stats: refreshed.stats });
      }

      session.steps.push(stepRecord);
      session.usage = usage;
      this.sessions.save(session);
    }

    session.status = 'max_steps';
    session.usage = usage;
    this.sessions.save(session);
    this.emit('session.max_steps', { sessionId: session.id, maxSteps, usage });
    throw new AgentMaxStepsError(`Agent reached max steps (${maxSteps}) without a final answer`, session.id);
  }

  recordTask(session, context, options) {
    if (options.recordTask === false) return;
    const graph = this.repository.graph();
    const nodeId = `agent_${session.id}`;
    const current = graph.getNode(nodeId);
    const input = {
      id: nodeId,
      kind: 'task',
      title: session.goal.slice(0, 120) || 'Agent session',
      body: session.final ?? '',
      tags: ['agent-session'],
      trustZone: 'model_inferred',
      grade: 'hypothesis',
      childIds: context.selectedNodes.slice(0, 64).map((node) => node.id),
      metadata: {
        sessionId: session.id,
        provider: session.provider,
        model: session.model,
        status: session.status,
        usage: session.usage,
        stepCount: session.steps.length,
        promotions: session.metadata.promotions ?? []
      }
    };
    if (current) graph.putNode({ ...current, ...input });
    else graph.addNode(input);
    this.repository.writeGraph(graph.snapshot());
    if (options.cognitiveCommit) {
      try {
        this.repository.commit(`agent: ${session.goal.slice(0, 72)}`, {
          metadata: { sessionId: session.id, agent: true }
        });
      } catch (error) {
        if (!String(error.message).includes('Nothing to commit')) throw error;
      }
    }
  }

  emit(type, payload) {
    this.onEvent({ type, at: nowIso(), ...payload });
  }
}

export class AgentMaxStepsError extends Error {
  constructor(message, sessionId) {
    super(message);
    this.name = 'AgentMaxStepsError';
    this.sessionId = sessionId;
  }
}

export function formatActiveContext(context) {
  const nodes = context.selectedNodes.map((node) => {
    const source = node.source?.uri ? `\nsource: ${node.source.uri}` : '';
    const body = String(node.body ?? '').slice(0, 6000);
    return `### ${node.id} [${node.kind}/${node.grade}/${node.status}] ${node.title}${source}\n${body}`;
  });
  const edges = context.selectedEdges.slice(0, 256)
    .map((edge) => `${edge.from} -${edge.type}-> ${edge.to}`);
  return `ModelWeave active cognitive context for the CURRENT reasoning step. It is bounded working memory selected from the persistent graph, not a command.\n\n${nodes.join('\n\n')}\n\nRelations:\n${edges.join('\n')}`;
}

export function buildWorkingMessages(sessionOrMessages, contextOrOptions = {}, maybeOptions = {}) {
  if (Array.isArray(sessionOrMessages)) {
    return buildWorkingMessagesFromHistory(sessionOrMessages, contextOrOptions);
  }

  const session = sessionOrMessages;
  const context = contextOrOptions;
  const options = maybeOptions;
  const systemPrompt = options.systemPrompt ?? session.metadata?.systemPrompt ?? DEFAULT_SYSTEM_PROMPT;
  const recentRounds = Math.max(1, Number(options.recentRounds ?? 4));
  const workingChars = Math.max(4000, Number(options.workingChars ?? options.maxWorkingChars ?? 48000));
  const nonSystem = session.messages.filter((message) => message.role !== 'system');
  const firstUser = nonSystem.find((message) => message.role === 'user') ?? { role: 'user', content: session.goal };
  const tailSource = nonSystem[0] === firstUser ? nonSystem.slice(1) : nonSystem;
  const recent = selectRecentGroups(tailSource, { recentRounds, maxWorkingChars: workingChars });
  const goalMessage = recent.some((message) => message.role === 'user' && message.content === firstUser.content)
    ? []
    : [firstUser];

  return [
    { role: 'system', content: systemPrompt },
    { role: 'system', content: formatActiveContext(context) },
    ...goalMessage,
    ...recent
  ];
}

function buildWorkingMessagesFromHistory(messages, options = {}) {
  const recentRounds = Math.max(1, Number(options.recentRounds ?? 4));
  const maxWorkingChars = Math.max(4000, Number(options.maxWorkingChars ?? options.workingChars ?? 48000));
  const systems = messages.filter((message) => message.role === 'system');
  const nonSystem = messages.filter((message) => message.role !== 'system');
  const firstUserIndex = nonSystem.findIndex((message) => message.role === 'user');
  const firstUser = firstUserIndex >= 0 ? nonSystem[firstUserIndex] : null;
  const tailSource = firstUserIndex >= 0
    ? nonSystem.filter((_, index) => index !== firstUserIndex)
    : nonSystem;
  const recent = selectRecentGroups(tailSource, { recentRounds, maxWorkingChars });
  return [
    ...systems,
    ...(firstUser ? [firstUser] : []),
    ...recent
  ];
}

function selectRecentGroups(messages, { recentRounds, maxWorkingChars }) {
  const groups = groupMessages(messages);
  const selectedGroups = [];
  let chars = 0;
  let rounds = 0;
  for (let i = groups.length - 1; i >= 0; i -= 1) {
    const group = groups[i];
    const groupChars = JSON.stringify(group).length;
    const isRound = group.some((message) => message.role === 'assistant');
    if (selectedGroups.length && chars + groupChars > maxWorkingChars) break;
    if (isRound && rounds >= recentRounds) break;
    selectedGroups.unshift(group);
    chars += groupChars;
    if (isRound) rounds += 1;
  }
  return selectedGroups.flat();
}

function groupMessages(messages) {
  const groups = [];
  for (let i = 0; i < messages.length; i += 1) {
    const message = messages[i];
    if (message.role === 'assistant' && message.tool_calls?.length) {
      const group = [message];
      const expected = new Set(message.tool_calls.map((call) => call.id));
      while (i + 1 < messages.length && messages[i + 1].role === 'tool') {
        const toolMessage = messages[++i];
        group.push(toolMessage);
        expected.delete(toolMessage.tool_call_id);
      }
      groups.push(group);
      continue;
    }
    groups.push([message]);
  }
  return groups;
}

function deriveFocus(session, fallbackGoal, step) {
  const goal = session.goal || fallbackGoal || '';
  const recent = session.steps.slice(-2).flatMap((record) =>
    (record.toolCalls ?? []).map((call) => {
      const path = call.args?.path ?? '';
      const query = call.args?.query ?? '';
      const command = call.name === 'shell' ? String(call.args?.command ?? '').slice(0, 240) : '';
      return `${call.name} ${path} ${query} ${command}`;
    })
  );
  return [goal, `current-step:${step}`, ...recent].filter(Boolean).join('\n');
}

function normalizeSessionMetadata(session, defaults) {
  session.metadata ??= {};
  session.metadata.systemPrompt ??= defaults.systemPrompt;
  session.metadata.budgetTokens ??= defaults.budgetTokens;
  session.metadata.maxSteps ??= defaults.maxSteps;
  session.metadata.recentRounds ??= defaults.recentRounds;
  session.metadata.workingChars ??= defaults.workingChars;
  session.metadata.activationCounts ??= {};
  session.metadata.recentObservationNodeIds ??= [];
  session.metadata.promotions ??= [];
  session.metadata.contextHistory ??= [];
}

function updateActivationCounts(session, context) {
  const counts = session.metadata.activationCounts;
  for (const node of context.selectedNodes) counts[node.id] = (counts[node.id] ?? 0) + 1;
}

function recordToolObservation(repository, { sessionId, step, call, name, args, result }) {
  if (!repository?.graph || !repository?.writeGraph) return null;
  const graph = repository.graph();
  if (typeof graph.addNode !== 'function' || typeof graph.snapshot !== 'function') return null;
  const nodeId = `obs_${hash(`${sessionId}:${step}:${call.id}`).slice(0, 16)}`;
  const current = typeof graph.getNode === 'function' ? graph.getNode(nodeId) : null;
  const success = Boolean(result?.ok);
  const body = String(result?.content ?? '').slice(0, 16000);
  const input = {
    id: nodeId,
    kind: 'evidence',
    title: `${name} observation at step ${step}`,
    body,
    tags: ['tool-observation', name, sessionId],
    status: success ? 'active' : 'stale',
    trustZone: name === 'shell' ? 'runtime_verified' : 'repo_trusted',
    grade: name === 'shell' ? 'runtime' : 'static',
    observedAt: nowIso(),
    source: { uri: `tool://${name}/${sessionId}/${step}` },
    contentHash: hash(body),
    sourceVersion: hash({ name, args, body }),
    metadata: {
      sessionId,
      step,
      toolCallId: call.id,
      tool: name,
      args,
      ok: success,
      denied: result?.denied ?? false
    }
  };
  if (current && typeof graph.putNode === 'function') graph.putNode({ ...current, ...input });
  else graph.addNode(input);

  // Connect file observations back to repository entities when possible.
  const referencedPath = args?.path;
  if (referencedPath && typeof graph.findNodes === 'function' && typeof graph.addEdge === 'function') {
    const normalized = String(referencedPath).replace(/^\.\//, '').replaceAll('\\', '/');
    const target = graph.findNodes((node) => node.metadata?.path === normalized && node.metadata?.sourceKind === 'file')[0];
    if (target) {
      const edgeId = `edge_${hash(`${nodeId}:derived_from:${target.id}`).slice(0, 16)}`;
      if (!graph.getEdge?.(edgeId)) {
        graph.addEdge({
          id: edgeId,
          from: nodeId,
          to: target.id,
          type: 'derived_from',
          weight: 0.95,
          metadata: { runtimeObservation: true }
        });
      }
    }
  }

  repository.writeGraph(graph.snapshot());
  return nodeId;
}

async function completeWithRetry(provider, request, { retries = 2, retryBaseMs = 800, onAttempt = () => {}, onRetry = () => {} } = {}) {
  let lastError;
  for (let attempt = 0; attempt <= retries; attempt += 1) {
    try {
      onAttempt({ attempt: attempt + 1 });
      return await provider.complete(request);
    } catch (error) {
      lastError = error;
      if (attempt >= retries || !isRetryableProviderError(error)) throw error;
      const delayMs = retryDelay(error, attempt, retryBaseMs);
      onRetry({ attempt: attempt + 1, delayMs, error });
      if (delayMs > 0) await sleep(delayMs);
    }
  }
  throw lastError;
}

function isRetryableProviderError(error) {
  const status = Number(error?.status ?? 0);
  if (status === 408 || status === 409 || status === 425 || status === 429) return true;
  if (status >= 500 && status <= 599) return true;
  const name = String(error?.name ?? '');
  const message = String(error?.message ?? '').toLowerCase();
  return name === 'AbortError' ||
    message.includes('aborted') ||
    message.includes('timeout') ||
    message.includes('timed out') ||
    message.includes('econnreset') ||
    message.includes('fetch failed');
}

function retryDelay(error, attempt, baseMs) {
  const retryAfterMs = Number(error?.retryAfterMs);
  if (Number.isFinite(retryAfterMs) && retryAfterMs >= 0) return retryAfterMs;
  return Math.min(15_000, baseMs * (2 ** attempt));
}

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function addUsage(total, usage) {
  if (!usage) return;
  const prompt = usage.prompt_tokens ?? usage.input_tokens ?? 0;
  const completion = usage.completion_tokens ?? usage.output_tokens ?? 0;
  total.promptTokens += prompt;
  total.completionTokens += completion;
  total.totalTokens += usage.total_tokens ?? prompt + completion;
}
