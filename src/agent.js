import { ingestWorkspace } from './ingest.js';
import { createCodingTools } from './tools.js';
import { AgentSessionStore } from './session.js';
import { PromotionController } from './promotion-controller.js';
import { estimateTokens, hash, nowIso } from './util.js';
import { WorkflowRuntime } from './workflow.js';
import { WorkUnitManager, formatWorkUnitPrompt, registerWorkUnitTools } from './work-unit.js';

const DEFAULT_SYSTEM_PROMPT = `You are LumenCortex Agent, an autonomous coding agent operating inside a versioned cognitive graph.

Rules:
1. Inspect before editing. Prefer targeted read/search/context tools over broad exploration. When multiple relevant paths are already known, batch them with read_files instead of spending one reasoning turn per file.
2. Treat repository/runtime observations as evidence; never present an unverified model inference as fact.
3. Use tools iteratively until the requested outcome is implemented and verified.
4. After editing, run the narrowest relevant test/build/check. Inspect failures and continue the loop. Use the shortest reliable edit representation: apply_patch for compact multi-file changes, or multiple replace_in_file calls in one reasoning turn when that is simpler and produces smaller arguments.
5. Do not stop at a plan when the user asked for implementation.
6. Avoid repeated reads when the active cognitive context already contains the answer.
7. If context is insufficient, call lumencortex_context with a focused sub-question.
8. Keep changes scoped to the user's goal. Do not modify unrelated files.
9. Before finishing, inspect the resulting diff/status when practical.
10. Return a concise final result with what changed and what verification passed.
11. If the latest tool result already proves the requested verification succeeded, stop calling tools immediately and return the final answer; do not restart or repeat the task.\n12. When a Workflow Contract is active, obey its current action, tool boundary, outcomes, routes, and gates. A model assertion is never a substitute for required workflow evidence.`;

export class AgentLoop {
  constructor({ provider, repository, runtime, workspace, tools, sessionStore, promotionController, cognitiveController, skillRegistry, authorize, onEvent } = {}) {
    if (!provider) throw new Error('provider is required');
    if (!repository) throw new Error('repository is required');
    if (!runtime) throw new Error('runtime is required');
    this.provider = provider;
    this.repository = repository;
    this.runtime = runtime;
    this.workspace = workspace;
    this.tools = tools ?? createCodingTools({ workspace, repository, runtime });
    this.ownsSessionStore = !sessionStore;
    this.sessions = sessionStore ?? new AgentSessionStore(repository.dir);
    this.promotionController = promotionController ?? new PromotionController(runtime);
    this.cognitiveController = cognitiveController ?? null;
    this.skillRegistry = skillRegistry ?? null;
    if (this.cognitiveController) registerWorkUnitTools(this.tools);
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
    const emptyTurnRetries = Math.max(0, Number(options.emptyTurnRetries ?? 1));
    const maxToolCallsPerStep = Math.max(1, Number(options.maxToolCallsPerStep ?? Number.MAX_SAFE_INTEGER));
    const toolAllowlist = options.toolAllowlist?.length ? [...new Set(options.toolAllowlist)] : null;
    const baseToolAllowset = toolAllowlist ? new Set(toolAllowlist) : null;
    let session;

    if (options.sessionId) {
      session = this.sessions.load(options.sessionId);
      session.status = 'running';
      delete session.error;
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
          contextHistory: [],
          cognition: { history: [], progress: {} }
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

    let skillPrompt = '';
    if (this.skillRegistry) {
      try {
        const loaded = this.skillRegistry.prompt();
        skillPrompt = loaded.prompt ?? '';
        if (loaded.skills?.length) {
          this.emit('skills.loaded', {
            sessionId: session.id,
            skills: loaded.skills.map((skill) => skill.id)
          });
        }
      } catch (error) {
        this.emit('skills.error', {
          sessionId: session.id,
          error: error.message
        });
      }
    }

    const workUnits = this.cognitiveController ? new WorkUnitManager(session) : null;
    if (workUnits && Array.isArray(options.workUnits) && options.workUnits.length && !workUnits.list().length) {
      workUnits.seed(options.workUnits);
    }

    const hadWorkflow = Boolean(session.metadata.workflow);
    const workflow = WorkflowRuntime.fromSession(session, options.workflow);
    if (workflow) {
      session.metadata.workflow = workflow.snapshot();
      if (!hadWorkflow) this.emit('workflow.start', { sessionId: session.id, workflow: workflow.summary() });
    }

    const usage = {
      promptTokens: Number(session.usage?.promptTokens ?? 0),
      completionTokens: Number(session.usage?.completionTokens ?? 0),
      totalTokens: Number(session.usage?.totalTokens ?? 0),
      requests: Number(session.usage?.requests ?? 0)
    };
    const startStep = session.steps.length;
    let lastContext = null;

    for (let turn = 1; turn <= maxSteps; turn += 1) {
      const step = startStep + turn;
      if (options.signal?.aborted) {
        throw this.interruptSession(session, step, usage, abortError(options.signal.reason));
      }
      if (workflow?.waitingHumanGates().length) {
        return this.pauseForWorkflowGate(session, step, usage, workflow, lastContext);
      }
      const stepToolAllowlist = workflow ? workflow.effectiveAllowlist(toolAllowlist) : toolAllowlist;
      const toolSchemas = this.tools.schemas(stepToolAllowlist);
      const activeWorkUnit = workUnits?.ensureActive() ?? null;
      const baseFocus = deriveFocus(session, goal, step);
      const workUnitFocus = activeWorkUnit
        ? `${baseFocus}\nwork-unit:${activeWorkUnit.id}:${activeWorkUnit.goal}`
        : baseFocus;
      const focus = workflow ? `${workUnitFocus}\nworkflow-action:${workflow.actionId()}` : workUnitFocus;
      const seedNodeIds = session.metadata.recentObservationNodeIds.slice(-8);
      const retrievalMode = normalizeAgentRetrievalMode(session.metadata.cognition.nextRetrievalMode);
      let context = this.runtime.contextAsync
        ? await this.runtime.contextAsync(focus, { budgetTokens, seedNodeIds, retrievalMode })
        : this.runtime.context(focus, { budgetTokens, seedNodeIds, retrievalMode });
      updateActivationCounts(session, context);
      const promotionResult = options.autoPromote === false
        ? { promoted: false }
        : this.promotionController.maybePromote(session.goal || goal || focus, context, {
            step,
            activationCounts: session.metadata.activationCounts,
            metadata: {
              sessionId: session.id,
              focus: focus.slice(0, 500)
            }
          });
      const promotion = promotionResult.promoted ? promotionResult.abstraction : null;
      if (promotion) {
        context = this.runtime.contextAsync
          ? await this.runtime.contextAsync(focus, {
              budgetTokens,
              seedNodeIds: [promotion.id, ...seedNodeIds],
              retrievalMode
            })
          : this.runtime.context(focus, {
              budgetTokens,
              seedNodeIds: [promotion.id, ...seedNodeIds],
              retrievalMode
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
        retrievalMode: context.mode ?? retrievalMode,
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

      let cognitivePlan = null;
      let providerCandidates = [{
        provider: this.provider,
        descriptor: { provider: options.providerName ?? session.provider ?? 'current', model: this.provider.model }
      }];
      if (this.cognitiveController) {
        try {
          cognitivePlan = await this.cognitiveController.planStep({
            goal: session.goal || goal,
            focus,
            step,
            session,
            context,
            signal: options.signal
          });
          if (cognitivePlan.providers?.length) providerCandidates = cognitivePlan.providers;
          session.metadata.cognition.nextRetrievalMode = normalizeAgentRetrievalMode(cognitivePlan.retrieval);
          this.emit('cognition.route', {
            sessionId: session.id,
            step,
            category: cognitivePlan.category,
            think: cognitivePlan.think,
            effort: cognitivePlan.effort,
            thinkScore: cognitivePlan.thinkScore,
            retrieval: cognitivePlan.retrieval,
            activeRetrievalMode: context.mode ?? retrievalMode,
            nextRetrievalMode: normalizeAgentRetrievalMode(cognitivePlan.retrieval),
            reasons: cognitivePlan.reasons,
            models: providerCandidates.map((item) => item.provider?.model ?? item.descriptor?.model ?? null),
            decisionErrors: cognitivePlan.decision?.errors ?? []
          });
        } catch (error) {
          this.emit('cognition.error', {
            sessionId: session.id,
            step,
            error: error.message
          });
        }
      }

      const requestMessages = buildWorkingMessages(session, context, {
        systemPrompt: session.metadata.systemPrompt,
        skillPrompt,
        workflowPrompt: workflow?.prompt(),
        cognitivePrompt: cognitivePlan?.prompt,
        workUnitPrompt: workUnits ? formatWorkUnitPrompt(workUnits) : '',
        recentRounds,
        workingChars
      });
      const requestedMaxTokens = adjustMaxTokensForEffort(options.maxTokens, cognitivePlan?.effort);
      let activeProvider = providerCandidates[0]?.provider ?? this.provider;

      this.emit('llm.request', {
        sessionId: session.id,
        step,
        model: activeProvider.model,
        workingMessages: requestMessages.length,
        workingTokens: requestMessages.reduce((sum, message) => sum + estimateTokens(message), 0),
        workflowAction: workflow?.actionId() ?? null,
        reasoningEffort: cognitivePlan ? (cognitivePlan.think ? cognitivePlan.effort : 'none') : null,
        availableTools: toolSchemas.map((schema) => schema.function.name)
      });

      let response;
      try {
        const completion = await completeWithProviderChain(
          providerCandidates,
          {
            messages: requestMessages,
            tools: toolSchemas,
            toolChoice: 'auto',
            temperature: options.temperature,
            maxTokens: requestedMaxTokens,
            reasoningEffort: cognitivePlan ? (cognitivePlan.think ? cognitivePlan.effort : 'none') : undefined,
            signal: options.signal
          },
          {
            retries: llmRetries,
            retryBaseMs,
            onAttempt: () => { usage.requests += 1; },
            onRetry: ({ attempt, delayMs, error, maxTokens, budgetAdjustment }) => this.emit('llm.retry', {
              sessionId: session.id,
              step,
              attempt,
              delayMs,
              error: error.message,
              status: error.status ?? null,
              maxTokens,
              budgetAdjustment
            }),
            onProviderFailure: ({ provider, descriptor, error, nextProvider, elapsedMs }) => {
              this.cognitiveController?.recordModelCall?.({
                session,
                model: provider?.model,
                descriptor,
                elapsedMs,
                ok: false,
                error
              });
              this.emit('cognition.model_chain', {
                sessionId: session.id,
                step,
                failedModel: provider?.model ?? null,
                nextModel: nextProvider?.model ?? null,
                error: error.message,
                status: error.status ?? null
              });
            },
            onProviderComplete: ({ provider, descriptor, elapsedMs }) => this.cognitiveController?.recordModelCall?.({
              session,
              model: provider?.model,
              descriptor,
              elapsedMs,
              ok: true
            })
          }
        );
        response = completion.response;
        activeProvider = completion.provider;
      } catch (error) {
        throw this.interruptSession(session, step, usage, error);
      }
      addUsage(usage, response.usage);
      let assistant = response.message;
      let calls = assistant.tool_calls ?? [];

      if (!calls.length && !String(assistant.content ?? '').trim()) {
        let recovered = null;
        for (let emptyAttempt = 1; emptyAttempt <= emptyTurnRetries; emptyAttempt += 1) {
          this.emit('llm.empty_turn', {
            sessionId: session.id,
            step,
            attempt: emptyAttempt,
            finishReason: response.finishReason,
            reasoning: assistant.reasoning ? String(assistant.reasoning).slice(0, 500) : ''
          });
          const recoveryMessages = [
            ...requestMessages,
            {
              role: 'user',
              content: 'Continue the current task. Return either valid tool_calls or a non-empty final answer. Do not return an empty assistant message.'
            }
          ];
          recovered = await completeWithRetry(
            activeProvider,
            {
              messages: recoveryMessages,
              tools: toolSchemas,
              toolChoice: 'auto',
              temperature: options.temperature,
              maxTokens: requestedMaxTokens,
              reasoningEffort: cognitivePlan ? (cognitivePlan.think ? cognitivePlan.effort : 'none') : undefined,
              signal: options.signal
            },
            {
              retries: llmRetries,
              retryBaseMs,
              onAttempt: () => { usage.requests += 1; },
              onRetry: ({ attempt, delayMs, error, maxTokens, budgetAdjustment }) => this.emit('llm.retry', {
                sessionId: session.id,
                step,
                attempt,
                delayMs,
                error: error.message,
                status: error.status ?? null,
                maxTokens,
                budgetAdjustment
              })
            }
          );
          addUsage(usage, recovered.usage);
          response = recovered;
          assistant = recovered.message;
          calls = assistant.tool_calls ?? [];
          if (calls.length || String(assistant.content ?? '').trim()) break;
        }
      }

      calls = assistant.tool_calls ?? [];
      if (calls.length > maxToolCallsPerStep) {
        const requested = calls.length;
        calls = calls.slice(0, maxToolCallsPerStep);
        assistant = { ...assistant, tool_calls: calls };
        this.emit('tools.deferred', {
          sessionId: session.id,
          step,
          requested,
          executing: calls.length,
          deferred: requested - calls.length
        });
      }

      session.messages.push(assistant);
      const stepRecord = {
        step,
        at: nowIso(),
        focus,
        contextNodeIds: context.selectedNodes.map((node) => node.id),
        contextTokens: context.usedTokens,
        promotionId: promotion?.id ?? null,
        finishReason: response.finishReason,
        toolCalls: [],
        content: assistant.content ?? '',
        workflow: workflow?.summary() ?? null,
        workUnit: activeWorkUnit ? {
          id: activeWorkUnit.id,
          goal: activeWorkUnit.goal,
          status: activeWorkUnit.status,
          risk: activeWorkUnit.risk
        } : null,
        cognition: cognitivePlan ? {
          category: cognitivePlan.category,
          think: cognitivePlan.think,
          effort: cognitivePlan.effort,
          thinkScore: cognitivePlan.thinkScore,
          retrieval: cognitivePlan.retrieval,
          activeRetrievalMode: context.mode ?? retrievalMode,
          nextRetrievalMode: session.metadata.cognition.nextRetrievalMode ?? 'weighted',
          model: activeProvider.model
        } : null
      };

      if (!calls.length) {
        if (!String(assistant.content ?? '').trim()) {
          const error = new Error('Model returned neither tool calls nor final content after empty-turn recovery');
          error.sessionId = session.id;
          session.status = 'interrupted';
          session.error = { at: nowIso(), step, name: error.name, message: error.message, status: null };
          session.usage = usage;
          this.sessions.save(session);
          throw error;
        }
        if (workflow && !workflow.canFinish()) {
          const reason = workflow.blockReason();
          stepRecord.workflow = workflow.summary();
          stepRecord.workflowBlockedFinal = true;
          session.steps.push(stepRecord);
          session.messages.push({
            role: 'user',
            content: `Workflow contract rejected completion: ${reason}. Continue the current action and obtain the required evidence or gate approval.`
          });
          session.metadata.workflow = workflow.snapshot();
          session.usage = usage;
          this.sessions.save(session);
          this.emit('workflow.blocked_final', { sessionId: session.id, step, action: workflow.actionId(), reason });
          continue;
        }
        if (workUnits?.hasIncomplete()) {
          const remaining = workUnits.remaining();
          stepRecord.workUnitBlockedFinal = true;
          session.steps.push(stepRecord);
          session.messages.push({
            role: 'user',
            content: `Work Unit completion gate rejected final completion. Remaining units: ${remaining.map((unit) => `${unit.id}[${unit.status}]: ${unit.goal}`).join('; ')}. Use work_unit_update to record required evidence/verification and complete the active unit before finishing.`
          });
          session.usage = usage;
          this.sessions.save(session);
          this.emit('work_unit.blocked_final', {
            sessionId: session.id,
            step,
            remaining: remaining.map((unit) => ({ id: unit.id, status: unit.status, goal: unit.goal }))
          });
          continue;
        }
        session.status = 'completed';
        session.final = assistant.content;
        session.usage = usage;
        session.steps.push(stepRecord);
        if (workflow) session.metadata.workflow = workflow.snapshot();
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
        } else if ((baseToolAllowset && !baseToolAllowset.has(name)) || (workflow && !workflow.isToolAllowed(name))) {
          const deniedByWorkflow = Boolean(workflow && !workflow.isToolAllowed(name));
          result = {
            ok: false,
            denied: true,
            permission: deniedByWorkflow ? 'workflow' : 'unavailable',
            content: deniedByWorkflow
              ? `Tool ${name} is not allowed by workflow action ${workflow.actionId()}`
              : `Tool ${name} is not available in the current tool working set`
          };
          if (deniedByWorkflow) this.emit('workflow.tool_denied', { sessionId: session.id, step, action: workflow.actionId(), name });
          this.emit('tool.end', {
            sessionId: session.id,
            step,
            toolCallId: call.id,
            name,
            ok: false,
            denied: true,
            permission: 'unavailable'
          });
        } else {
          this.emit('tool.start', {
            sessionId: session.id,
            step,
            toolCallId: call.id,
            name,
            args: parsed
          });
          try {
            result = await this.tools.execute(name, parsed, {
              workspace: this.workspace,
              repository: this.repository,
              runtime: this.runtime,
              session,
              step,
              authorize: options.authorize ?? this.authorize,
              signal: options.signal,
              onOutput: ({ stream, chunk }) => this.emit('tool.output', {
                sessionId: session.id,
                step,
                toolCallId: call.id,
                name,
                stream,
                chunk: String(chunk ?? '').slice(-4000)
              })
            });
          } catch (error) {
            if (options.signal?.aborted || error?.name === 'AbortError') {
              throw this.interruptSession(session, step, usage, error);
            }
            throw error;
          }
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
        this.cognitiveController?.observeTool?.({
          session,
          name,
          args: parsed,
          result,
          step
        });
        if (workflow) {
          const update = workflow.observeTool({ tool: name, args: parsed, result, step });
          session.metadata.workflow = workflow.snapshot();
          stepRecord.workflow = workflow.summary();
          if (update.changedFacts.length) {
            this.emit('workflow.facts', { sessionId: session.id, step, action: update.beforeAction, facts: update.changedFacts });
          }
          if (update.transition) {
            this.emit('workflow.transition', { sessionId: session.id, step, ...update.transition });
          }
        }
        const observationId = options.recordObservations === false ? null : recordToolObservation(this.repository, {
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
        this.runtime.refreshSearchIndex?.(refreshed.graph);
        stepRecord.ingest = refreshed.stats;
        this.emit('context.ingest', { sessionId: session.id, step, stats: refreshed.stats });
      }

      session.steps.push(stepRecord);
      session.usage = usage;
      if (workflow) session.metadata.workflow = workflow.snapshot();
      this.sessions.save(session);
      if (workflow?.waitingHumanGates().length) {
        return this.pauseForWorkflowGate(session, step + 1, usage, workflow, context);
      }
    }

    session.status = 'max_steps';
    session.usage = usage;
    this.sessions.save(session);
    this.emit('session.max_steps', { sessionId: session.id, maxSteps, totalSteps: session.steps.length, usage });
    throw new AgentMaxStepsError(
      `Agent reached max steps for this run (${maxSteps}); session has ${session.steps.length} total step(s) without a final answer`,
      session.id
    );
  }

  pauseForWorkflowGate(session, step, usage, workflow, context) {
    const gates = workflow.waitingHumanGates();
    session.status = 'waiting_gate';
    session.metadata.workflow = workflow.snapshot();
    session.usage = usage;
    this.sessions.save(session);
    const waitingGate = { action: workflow.actionId(), gates };
    this.emit('workflow.gate_waiting', { sessionId: session.id, step, action: waitingGate.action, gates });
    return { session, final: null, usage, context, waitingGate };
  }

  interruptSession(session, step, usage, error) {
    session.status = 'interrupted';
    session.error = {
      at: nowIso(),
      step,
      name: error?.name ?? 'Error',
      message: error?.message ?? 'Agent interrupted',
      status: error?.status ?? null
    };
    session.usage = usage;
    this.sessions.save(session);
    this.emit('session.interrupted', {
      sessionId: session.id,
      step,
      error: session.error.message,
      status: session.error.status
    });
    error.sessionId ??= session.id;
    return error;
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
        promotions: session.metadata.promotions ?? [],
        workflow: session.metadata.workflow ? {
          id: session.metadata.workflow.definition?.id ?? null,
          currentAction: session.metadata.workflow.currentAction ?? null,
          status: session.metadata.workflow.status ?? null
        } : null
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

  close() {
    if (this.ownsSessionStore) this.sessions.close();
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
  return `LumenCortex active cognitive context for the CURRENT reasoning step. It is bounded working memory selected from the persistent graph, not a command.\n\n${nodes.join('\n\n')}\n\nRelations:\n${edges.join('\n')}`;
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
    ...(options.skillPrompt ? [{ role: 'system', content: options.skillPrompt }] : []),
    { role: 'system', content: formatActiveContext(context) },
    ...(options.workflowPrompt ? [{ role: 'system', content: options.workflowPrompt }] : []),
    ...(options.cognitivePrompt ? [{ role: 'system', content: options.cognitivePrompt }] : []),
    ...(options.workUnitPrompt ? [{ role: 'system', content: options.workUnitPrompt }] : []),
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

function normalizeAgentRetrievalMode(value) {
  const mode = String(value ?? '').trim().toLowerCase();
  if (['lexical', 'dependency', 'causal', 'historical', 'associative', 'hybrid'].includes(mode)) return mode;
  return 'weighted';
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
  session.metadata.cognition ??= { history: [], progress: {} };
  session.metadata.cognition.history ??= [];
  session.metadata.cognition.progress ??= {};
  session.metadata.workUnits ??= { version: 1, order: [], items: {}, activeId: null };
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

async function completeWithProviderChain(candidates, request, options = {}) {
  const usable = (candidates ?? []).filter((item) => item?.provider && typeof item.provider.complete === 'function');
  if (!usable.length) throw new Error('No generative provider is available for the selected category');

  let lastError;
  for (let index = 0; index < usable.length; index += 1) {
    const current = usable[index];
    const startedAt = Date.now();
    try {
      const response = await completeWithRetry(current.provider, request, options);
      const elapsedMs = Date.now() - startedAt;
      options.onProviderComplete?.({
        provider: current.provider,
        descriptor: current.descriptor,
        elapsedMs
      });
      return { response, provider: current.provider, descriptor: current.descriptor, index, elapsedMs };
    } catch (error) {
      const elapsedMs = Date.now() - startedAt;
      lastError = error;
      const next = usable[index + 1];
      if (error?.name !== 'AbortError' && !request.signal?.aborted) {
        options.onProviderFailure?.({
          provider: current.provider,
          descriptor: current.descriptor,
          error,
          elapsedMs,
          nextProvider: next?.provider ?? null,
          nextDescriptor: next?.descriptor ?? null
        });
      }
      if (!next || error?.name === 'AbortError' || request.signal?.aborted) throw error;
    }
  }
  throw lastError ?? new Error('Category model chain failed');
}

function adjustMaxTokensForEffort(maxTokens, effort) {
  if (maxTokens === undefined || maxTokens === null) return maxTokens;
  const base = Math.max(1, Number(maxTokens));
  const multiplier = effort === 'max' ? 2 : effort === 'high' ? 1.5 : effort === 'medium' ? 1.15 : 1;
  return Math.ceil(base * multiplier);
}

async function completeWithRetry(provider, request, { retries = 2, retryBaseMs = 800, onAttempt = () => {}, onRetry = () => {} } = {}) {
  let lastError;
  const activeRequest = { ...request };
  for (let attempt = 0; attempt <= retries; attempt += 1) {
    if (activeRequest.signal?.aborted) throw abortError(activeRequest.signal.reason);
    try {
      onAttempt({ attempt: attempt + 1, maxTokens: activeRequest.maxTokens });
      return await provider.complete(activeRequest);
    } catch (error) {
      lastError = error;
      if (activeRequest.signal?.aborted) throw error;
      if (attempt >= retries || !isRetryableProviderError(error)) throw error;
      const budgetAdjustment = expandMalformedToolCallBudget(activeRequest, error);
      const delayMs = retryDelay(error, attempt, retryBaseMs);
      onRetry({
        attempt: attempt + 1,
        delayMs,
        error,
        maxTokens: activeRequest.maxTokens,
        budgetAdjustment
      });
      if (delayMs > 0) await sleep(delayMs, activeRequest.signal);
    }
  }
  throw lastError;
}

function expandMalformedToolCallBudget(request, error) {
  const message = String(error?.message ?? '').toLowerCase();
  const malformedToolCall =
    message.includes('invalid tool call arguments') ||
    message.includes('unexpected end of json') ||
    message.includes('unterminated') ||
    message.includes('tool call') && message.includes('json');
  if (!malformedToolCall) return null;

  const current = Number(request.maxTokens);
  if (!Number.isFinite(current) || current <= 0 || current >= 4096) return null;
  const next = Math.min(4096, Math.max(current + 256, Math.ceil(current * 2)));
  request.maxTokens = next;
  return { from: current, to: next, reason: 'malformed_tool_call' };
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

function sleep(ms, signal) {
  if (!signal) return new Promise((resolve) => setTimeout(resolve, ms));
  if (signal.aborted) return Promise.reject(abortError(signal.reason));
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      signal.removeEventListener('abort', onAbort);
      resolve();
    }, ms);
    const onAbort = () => {
      clearTimeout(timer);
      signal.removeEventListener('abort', onAbort);
      reject(abortError(signal.reason));
    };
    signal.addEventListener('abort', onAbort, { once: true });
  });
}

function abortError(reason) {
  if (reason instanceof Error && reason.name === 'AbortError') return reason;
  const error = new Error(
    typeof reason === 'string' && reason.trim() ? reason : 'Agent run cancelled'
  );
  error.name = 'AbortError';
  return error;
}

function addUsage(total, usage) {
  if (!usage) return;
  const prompt = usage.prompt_tokens ?? usage.input_tokens ?? 0;
  const completion = usage.completion_tokens ?? usage.output_tokens ?? 0;
  total.promptTokens += prompt;
  total.completionTokens += completion;
  total.totalTokens += usage.total_tokens ?? prompt + completion;
}
