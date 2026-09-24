import fs from 'node:fs';
import path from 'node:path';
import { createProvider, PROVIDER_PRESETS } from './provider.js';
import { clamp, hash } from './util.js';

export const BUILTIN_CATEGORY_DESCRIPTIONS = {
  quick: 'Small bounded work with obvious local scope.',
  general: 'Normal coding, implementation, and reasoning work.',
  deep: 'Difficult multi-step reasoning, debugging, refactoring, or architecture work.',
  ultrabrain: 'Exceptionally difficult work with repeated failures, contradictions, or major uncertainty.',
  'visual-engineering': 'UI, layout, interaction, visual implementation, and frontend design work.',
  research: 'Broad investigation, evidence gathering, comparison, and synthesis.',
  writing: 'Documentation, technical writing, and explanatory content.'
};

const QUICK_TERMS = /\b(typo|rename|format|lint|small|tiny|quick|one[- ]?line|single[- ]?file|copy change)\b/i;
const VISUAL_TERMS = /\b(ui|ux|css|layout|frontend|front-end|visual|design|responsive|react|vue|svelte|wails|figma)\b/i;
const RESEARCH_TERMS = /\b(research|investigate|compare|paper|papers|source|sources|latest|benchmark|survey|literature|web search)\b/i;
const WRITING_TERMS = /\b(readme|documentation|docs|write|rewrite|copy|guide|tutorial|explain)\b/i;
const DEEP_TERMS = /\b(debug|deadlock|race|concurrency|architecture|migration|refactor|security|performance|root cause|multi[- ]?module|cross[- ]?module|distributed|transaction)\b/i;
const HIGH_RISK_TERMS = /\b(delete|drop|migration|production|security|auth|credential|payment|billing|database|schema|release|deploy)\b/i;

export function loadCognitiveProfile(workspace, options = {}) {
  const file = options.file ?? path.join(workspace, '.lumencortex', 'cognition.json');
  let user = {};
  if (file && fs.existsSync(file)) {
    user = JSON.parse(fs.readFileSync(file, 'utf8'));
  }

  const categories = {};
  for (const [name, description] of Object.entries(BUILTIN_CATEGORY_DESCRIPTIONS)) {
    categories[name] = { description, models: [], default: name === 'general' };
  }
  for (const [name, entry] of Object.entries(user.categories ?? {})) {
    categories[name] = {
      ...(categories[name] ?? { description: '', models: [], default: false }),
      ...entry,
      models: Array.isArray(entry?.models) ? entry.models : []
    };
  }

  const explicitDefaults = Object.entries(categories).filter(([, entry]) => entry.default);
  if (explicitDefaults.length > 1) {
    const keep = explicitDefaults.find(([name]) => name === 'general')?.[0] ?? explicitDefaults[0][0];
    for (const [name, entry] of Object.entries(categories)) entry.default = name === keep;
  } else if (!explicitDefaults.length) {
    categories.general ??= { description: BUILTIN_CATEGORY_DESCRIPTIONS.general, models: [] };
    categories.general.default = true;
  }

  return {
    version: 1,
    source: fs.existsSync(file) ? file : 'built-in',
    decision: {
      policy: user.decision?.policy ?? 'first',
      providers: Array.isArray(user.decision?.providers) ? user.decision.providers : []
    },
    categories,
    telemetry: {
      enabled: user.telemetry?.enabled !== false
    },
    health: {
      failureThreshold: Math.max(1, Number(user.health?.failureThreshold ?? 3)),
      cooldownMs: Math.max(1000, Number(user.health?.cooldownMs ?? 30000))
    },
    governor: user.governor ? {
      enabled: user.governor.enabled !== false,
      provider: user.governor.provider,
      model: user.governor.model,
      baseURL: user.governor.baseURL,
      timeoutMs: user.governor.timeoutMs,
      reasoningEffort: user.governor.reasoningEffort ?? user.governor.reasoning_effort ?? 'high',
      maxTokens: Number(user.governor.maxTokens ?? user.governor.max_tokens ?? 6000)
    } : null
  };
}

export class AlgorithmDecisionProvider {
  constructor(options = {}) {
    this.name = options.name ?? 'algorithm';
  }

  async decide({ state }) {
    return {
      source: this.name,
      answers: algorithmicAnswers(state)
    };
  }
}

export class SystemOneDecisionProvider {
  constructor({
    name = 'system-one',
    baseURL,
    apiKey,
    model,
    headers = {},
    fetchImpl = globalThis.fetch,
    timeoutMs = 3000
  } = {}) {
    if (!baseURL) throw new Error('Decision provider baseURL is required');
    if (typeof fetchImpl !== 'function') throw new Error('Decision provider fetch implementation is required');
    this.name = name;
    this.baseURL = baseURL.replace(/\/$/, '');
    this.apiKey = apiKey;
    this.model = model;
    this.headers = headers;
    this.fetch = fetchImpl;
    this.timeoutMs = timeoutMs;
  }

  async decide({ state, questions, signal } = {}) {
    const controller = signal ? null : new AbortController();
    const timeout = controller ? setTimeout(() => controller.abort(), this.timeoutMs) : null;
    try {
      const response = await this.fetch(`${this.baseURL}/v1/systemone`, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          ...(this.apiKey ? { Authorization: `Bearer ${this.apiKey}` } : {}),
          ...this.headers
        },
        body: JSON.stringify({
          state,
          questions,
          ...(this.model ? { model: this.model } : {})
        }),
        signal: signal ?? controller.signal
      });
      const text = await response.text();
      let data;
      try { data = text ? JSON.parse(text) : {}; } catch { data = { raw: text }; }
      if (!response.ok) {
        const message = data?.detail ?? data?.error?.message ?? data?.message ?? text ?? `HTTP ${response.status}`;
        const error = new Error(`Decision request failed (${response.status}): ${typeof message === 'string' ? message : JSON.stringify(message)}`);
        error.status = response.status;
        error.response = data;
        throw error;
      }
      return {
        source: this.name,
        model: data.model ?? this.model ?? null,
        answers: data.answers ?? {},
        usage: data.usage ?? null,
        raw: data
      };
    } finally {
      if (timeout) clearTimeout(timeout);
    }
  }
}

export function createDecisionProvider(config, options = {}) {
  if (!config || config === 'algorithm' || config.type === 'algorithm') {
    return new AlgorithmDecisionProvider();
  }

  const normalized = typeof config === 'string' ? { type: config } : config;
  const type = String(normalized.type ?? normalized.provider ?? 'systemone').toLowerCase();
  if (!['jev', 'laya', 'systemone'].includes(type)) {
    throw new Error(`Unknown decision provider type: ${type}`);
  }

  const defaults = type === 'jev'
    ? { baseURL: 'https://api.typesafe.ai', apiKeyEnv: 'TYPESAFE_API_KEY', model: 'jev-latest' }
    : type === 'laya'
      ? { baseURL: 'http://127.0.0.1:8000', apiKeyEnv: 'LAYA_API_KEY', model: undefined }
      : { baseURL: undefined, apiKeyEnv: undefined, model: undefined };

  return new SystemOneDecisionProvider({
    name: normalized.name ?? type,
    baseURL: normalized.baseURL ?? defaults.baseURL,
    apiKey: normalized.apiKey ?? (normalized.apiKeyEnv ? process.env[normalized.apiKeyEnv] : defaults.apiKeyEnv ? process.env[defaults.apiKeyEnv] : undefined),
    model: normalized.model ?? defaults.model,
    headers: normalized.headers,
    fetchImpl: options.fetchImpl ?? normalized.fetchImpl,
    timeoutMs: normalized.timeoutMs ?? options.timeoutMs ?? 3000
  });
}

export class ProviderHealthRegistry {
  constructor({ failureThreshold = 3, cooldownMs = 30000, now = () => Date.now() } = {}) {
    this.failureThreshold = Math.max(1, Number(failureThreshold));
    this.cooldownMs = Math.max(1000, Number(cooldownMs));
    this.now = now;
    this.states = new Map();
  }

  isAvailable(key) {
    const state = this.states.get(key);
    if (!state?.openUntil) return true;
    return this.now() >= state.openUntil;
  }

  recordSuccess(key) {
    const current = this.states.get(key) ?? emptyHealthState();
    this.states.set(key, {
      ...current,
      successes: current.successes + 1,
      consecutiveFailures: 0,
      openUntil: null,
      lastStatus: null,
      lastError: null,
      updatedAt: this.now()
    });
  }

  recordFailure(key, error = {}) {
    const current = this.states.get(key) ?? emptyHealthState();
    const consecutiveFailures = current.consecutiveFailures + 1;
    this.states.set(key, {
      ...current,
      failures: current.failures + 1,
      consecutiveFailures,
      openUntil: consecutiveFailures >= this.failureThreshold
        ? this.now() + this.cooldownMs
        : current.openUntil,
      lastStatus: error.status ?? null,
      lastError: error.message ?? String(error),
      updatedAt: this.now()
    });
  }

  snapshot() {
    return Object.fromEntries(
      [...this.states.entries()].map(([key, state]) => [
        key,
        {
          ...state,
          available: this.isAvailable(key),
          cooldownRemainingMs: state.openUntil
            ? Math.max(0, state.openUntil - this.now())
            : 0
        }
      ])
    );
  }
}

export class DecisionLayer {
  constructor({
    providers = [],
    policy = 'first',
    algorithm = new AlgorithmDecisionProvider(),
    healthRegistry = null
  } = {}) {
    this.algorithm = algorithm;
    this.providers = providers;
    this.policy = policy;
    this.healthRegistry = healthRegistry;
  }

  async decide(input = {}) {
    const algorithm = await this.algorithm.decide(input);
    const models = [];
    const errors = [];

    for (const provider of this.providers) {
      const healthKey = decisionHealthKey(provider);
      if (this.healthRegistry && !this.healthRegistry.isAvailable(healthKey)) {
        errors.push({
          provider: provider.name ?? 'decision-provider',
          message: 'circuit-open',
          status: null,
          skipped: true
        });
        continue;
      }

      try {
        const result = await provider.decide(input);
        this.healthRegistry?.recordSuccess(healthKey);
        models.push(result);
        if (this.policy !== 'all') break;
      } catch (error) {
        this.healthRegistry?.recordFailure(healthKey, error);
        errors.push({
          provider: provider.name ?? 'decision-provider',
          message: error.message,
          status: error.status ?? null
        });
      }
    }

    return {
      algorithm,
      models,
      errors,
      signals: mergeDecisionSignals(algorithm, models)
    };
  }
}

export class ProgressMonitor {
  constructor(snapshot = {}) {
    this.failures = structuredClone(snapshot.failures ?? {});
    this.sameFailureCount = Number(snapshot.sameFailureCount ?? 0);
    this.noProgressSteps = Number(snapshot.noProgressSteps ?? 0);
    this.successfulActions = Number(snapshot.successfulActions ?? 0);
    this.lastFailureSignature = snapshot.lastFailureSignature ?? null;
  }

  observeTool({ name, args, result, step }) {
    if (result?.ok) {
      this.successfulActions += 1;
      if (result?.mutatesWorkspace || name === 'shell') this.noProgressSteps = 0;
      return;
    }

    this.noProgressSteps += 1;
    const signature = failureSignature({ name, args, result });
    const entry = this.failures[signature] ?? {
      signature,
      count: 0,
      firstStep: step,
      lastStep: step,
      tool: name
    };
    entry.count += 1;
    entry.lastStep = step;
    this.failures[signature] = entry;
    this.lastFailureSignature = signature;
    this.sameFailureCount = entry.count;
  }

  snapshot() {
    return {
      failures: structuredClone(this.failures),
      sameFailureCount: this.sameFailureCount,
      noProgressSteps: this.noProgressSteps,
      successfulActions: this.successfulActions,
      lastFailureSignature: this.lastFailureSignature,
      distinctFailureCount: Object.keys(this.failures).length,
      maxRepeatedFailure: Math.max(0, ...Object.values(this.failures).map((item) => Number(item.count ?? 0)))
    };
  }
}

export class CognitiveRouter {
  constructor(options = {}) {
    this.categoryConfidenceThreshold = Number(options.categoryConfidenceThreshold ?? 0.55);
    this.thinkThreshold = Number(options.thinkThreshold ?? 0.56);
  }

  route({ state, decision }) {
    const algorithm = decision?.algorithm?.answers ?? algorithmicAnswers(state);
    const signals = decision?.signals ?? algorithm;
    const modelCategory = signals.category;
    const algorithmCategory = algorithm.category;

    const category = modelCategory?.choice && Number(modelCategory.confidence ?? 0) >= this.categoryConfidenceThreshold
      ? modelCategory.choice
      : algorithmCategory?.choice ?? 'general';

    let thinkScore = Number(algorithm.need_think?.noul ?? 0.25);
    const modelThink = Number(signals.need_think?.noul);
    if (Number.isFinite(modelThink)) thinkScore = Math.max(thinkScore, modelThink * 0.9);

    const progress = state.progress ?? {};
    thinkScore += Math.min(0.35, Number(progress.maxRepeatedFailure ?? progress.sameFailureCount ?? 0) * 0.12);
    thinkScore += Math.min(0.2, Number(progress.noProgressSteps ?? 0) * 0.06);
    if (Number(signals.stuck?.noul ?? 0) > 0.7) thinkScore += 0.12;
    if (Number(signals.evidence_sufficient?.noul ?? 1) < 0.35) thinkScore += 0.08;
    thinkScore = clamp(thinkScore, 0, 1);

    const think = thinkScore >= this.thinkThreshold;
    const effort = selectEffort(thinkScore, progress);
    const reasons = [];
    if (think) reasons.push('deliberation-value');
    if (Number(progress.maxRepeatedFailure ?? 0) >= 2) reasons.push('repeated-failure');
    if (Number(signals.stuck?.noul ?? 0) > 0.7) reasons.push('stuck');
    if (Number(signals.evidence_sufficient?.noul ?? 1) < 0.35) reasons.push('insufficient-evidence');

    return {
      category,
      think,
      effort,
      thinkScore,
      reasons,
      retrieval: signals.retrieval?.choice ?? algorithm.retrieval?.choice ?? 'lexical'
    };
  }
}

export class CategoryResolver {
  constructor({
    profile,
    fallbackProvider,
    fallbackProviderName,
    fallbackModel,
    providerFactory = createProvider,
    healthRegistry = null
  } = {}) {
    this.profile = profile;
    this.fallbackProvider = fallbackProvider;
    this.fallbackProviderName = fallbackProviderName;
    this.fallbackModel = fallbackModel ?? fallbackProvider?.model;
    this.providerFactory = providerFactory;
    this.healthRegistry = healthRegistry;
    this.cache = new Map();
  }

  resolveChain(categoryName) {
    const categories = this.profile?.categories ?? {};
    const category = categories[categoryName] ?? this.defaultCategory();
    const chain = Array.isArray(category?.models) ? category.models : [];
    const resolved = [];
    const skipped = [];

    for (const entry of chain) {
      try {
        const descriptor = normalizeModelDescriptor(entry, this.fallbackProviderName);
        const healthKey = modelHealthKey(descriptor);
        if (this.healthRegistry && !this.healthRegistry.isAvailable(healthKey)) {
          skipped.push({ descriptor, reason: 'circuit-open' });
          continue;
        }
        const provider = this.#provider(descriptor);
        if (provider) resolved.push({ provider, descriptor, healthKey });
      } catch (error) {
        skipped.push({
          descriptor: normalizeModelDescriptor(entry, this.fallbackProviderName),
          reason: error.message
        });
      }
    }

    if (!resolved.length && this.fallbackProvider) {
      const descriptor = {
        provider: this.fallbackProviderName ?? 'current',
        model: this.fallbackModel ?? this.fallbackProvider.model,
        fallback: true
      };
      const healthKey = modelHealthKey(descriptor);
      if (!this.healthRegistry || this.healthRegistry.isAvailable(healthKey)) {
        resolved.push({
          provider: this.fallbackProvider,
          descriptor,
          healthKey
        });
      } else {
        skipped.push({ descriptor, reason: 'circuit-open' });
      }
    }

    return {
      category: categoryName in categories ? categoryName : this.defaultCategoryName(),
      entries: resolved,
      skipped
    };
  }

  defaultCategoryName() {
    return Object.entries(this.profile?.categories ?? {}).find(([, entry]) => entry.default)?.[0] ?? 'general';
  }

  defaultCategory() {
    return this.profile?.categories?.[this.defaultCategoryName()] ?? { models: [] };
  }

  #provider(descriptor) {
    if (
      this.fallbackProvider &&
      descriptor.provider === this.fallbackProviderName &&
      (!descriptor.model || descriptor.model === this.fallbackProvider.model)
    ) return this.fallbackProvider;

    const key = JSON.stringify(descriptor);
    if (this.cache.has(key)) return this.cache.get(key);
    const provider = this.providerFactory(descriptor.provider ?? this.fallbackProviderName, {
      model: descriptor.model,
      baseURL: descriptor.baseURL,
      timeoutMs: descriptor.timeoutMs,
      headers: descriptor.headers
    });
    this.cache.set(key, provider);
    return provider;
  }
}

export class CognitiveController {
  constructor({ decisionLayer, router, categoryResolver, profile, healthRegistry } = {}) {
    this.healthRegistry = healthRegistry ?? new ProviderHealthRegistry(profile?.health);
    this.decisionLayer = decisionLayer ?? new DecisionLayer({ healthRegistry: this.healthRegistry });
    this.router = router ?? new CognitiveRouter();
    this.categoryResolver = categoryResolver;
    this.profile = profile;
  }

  async planStep({ goal, focus, step, session, context, signal } = {}) {
    session.metadata ??= {};
    session.metadata.cognition ??= { history: [], progress: {} };
    const progress = new ProgressMonitor(session.metadata.cognition.progress);
    const state = buildDecisionState({ goal, focus, step, session, context, progress: progress.snapshot() });
    const questions = buildDecisionQuestions(this.profile?.categories);
    const decision = await this.decisionLayer.decide({ state, questions, signal });
    const route = this.router.route({ state, decision });
    const chain = this.categoryResolver?.resolveChain(route.category) ?? { category: route.category, entries: [] };
    const plan = {
      ...route,
      category: chain.category,
      providers: chain.entries,
      decision,
      prompt: cognitivePrompt(route)
    };

    session.metadata.cognition.history ??= [];
    session.metadata.cognition.history.push({
      step,
      category: plan.category,
      think: plan.think,
      effort: plan.effort,
      thinkScore: plan.thinkScore,
      retrieval: plan.retrieval,
      reasons: plan.reasons,
      decisionErrors: decision.errors,
      skippedModels: chain.skipped ?? [],
      models: plan.providers.map((item) => item.provider?.model ?? item.descriptor?.model ?? null)
    });
    session.metadata.cognition.providerHealth = this.healthRegistry.snapshot();
    session.metadata.cognition.history = session.metadata.cognition.history.slice(-64);
    return plan;
  }

  observeTool({ session, name, args, result, step }) {
    session.metadata ??= {};
    session.metadata.cognition ??= { history: [], progress: {} };
    const progress = new ProgressMonitor(session.metadata.cognition.progress);
    progress.observeTool({ name, args, result, step });
    session.metadata.cognition.progress = progress.snapshot();
  }

  recordModelCall({ session, model, descriptor, elapsedMs, ok, error }) {
    if (!model || !Number.isFinite(Number(elapsedMs))) return;
    session.metadata ??= {};
    session.metadata.cognition ??= { history: [], progress: {} };
    session.metadata.cognition.modelTelemetry ??= {};
    const current = session.metadata.cognition.modelTelemetry[model] ?? {
      calls: 0,
      failures: 0,
      ewmaLatencyMs: null,
      lastLatencyMs: null
    };
    const latency = Math.max(0, Number(elapsedMs));
    const alpha = 0.2;
    current.calls += 1;
    if (!ok) current.failures += 1;
    current.lastLatencyMs = latency;
    current.ewmaLatencyMs = current.ewmaLatencyMs == null
      ? latency
      : alpha * latency + (1 - alpha) * current.ewmaLatencyMs;
    session.metadata.cognition.modelTelemetry[model] = current;

    const healthKey = modelHealthKey({
      provider: descriptor?.provider ?? 'current',
      model
    });
    if (ok) this.healthRegistry.recordSuccess(healthKey);
    else this.healthRegistry.recordFailure(healthKey, error ?? new Error('provider call failed'));
    session.metadata.cognition.providerHealth = this.healthRegistry.snapshot();
  }
}

export function createCognitiveController({
  workspace,
  profileFile,
  fallbackProvider,
  fallbackProviderName,
  fallbackModel,
  providerFactory = createProvider,
  decisionFetchImpl
} = {}) {
  const profile = loadCognitiveProfile(workspace, {
    file: profileFile,
    fallbackProviderName,
    fallbackModel
  });

  const healthRegistry = new ProviderHealthRegistry(profile.health);
  const modelDecisionProviders = [];
  for (const entry of profile.decision.providers) {
    if (entry === 'algorithm' || entry?.type === 'algorithm') continue;
    try {
      modelDecisionProviders.push(createDecisionProvider(entry, { fetchImpl: decisionFetchImpl }));
    } catch {
      // Invalid optional decision routes do not prevent deterministic cognition.
    }
  }

  const decisionLayer = new DecisionLayer({
    providers: modelDecisionProviders,
    policy: profile.decision.policy,
    healthRegistry
  });
  const categoryResolver = new CategoryResolver({
    profile,
    fallbackProvider,
    fallbackProviderName,
    fallbackModel,
    providerFactory,
    healthRegistry
  });

  return new CognitiveController({
    profile,
    decisionLayer,
    router: new CognitiveRouter(),
    categoryResolver,
    healthRegistry
  });
}

export function buildDecisionQuestions(categories = {}) {
  const criteria = {};
  for (const [name, entry] of Object.entries(categories)) {
    criteria[name] = entry.description || name;
  }
  return {
    category: {
      type: 'choice',
      instructions: 'Which work category best matches the current task state?',
      criteria
    },
    need_think: {
      type: 'noul',
      instructions: 'Would deliberate multi-step reasoning materially improve the next decision?'
    },
    evidence_sufficient: {
      type: 'noul',
      instructions: 'Is the currently selected evidence sufficient for the next action?'
    },
    stuck: {
      type: 'noul',
      instructions: 'Is the current strategy stuck or repeating without useful progress?'
    },
    retrieval: {
      type: 'choice',
      instructions: 'Which retrieval direction is most useful next?',
      criteria: {
        lexical: 'Exact or lexical lookup is sufficient.',
        dependency: 'Follow calls, imports, dependencies, and structural relations.',
        causal: 'Follow causes, derived evidence, effects, and failure chains.',
        historical: 'Use prior sessions, changes, superseded facts, or temporal history.'
      }
    }
  };
}

export function algorithmicAnswers(state = {}) {
  const text = [state.goal, state.focus, state.workUnit?.goal].filter(Boolean).join(' ');
  const progress = state.progress ?? {};
  const scores = {
    quick: 0.12,
    general: 0.3,
    deep: 0.12,
    ultrabrain: 0.02,
    'visual-engineering': 0.04,
    research: 0.04,
    writing: 0.04
  };

  if (QUICK_TERMS.test(text) || text.length < 80) scores.quick += 0.35;
  if (VISUAL_TERMS.test(text)) scores['visual-engineering'] += 0.7;
  if (RESEARCH_TERMS.test(text)) scores.research += 0.65;
  if (WRITING_TERMS.test(text)) scores.writing += 0.45;
  if (DEEP_TERMS.test(text)) scores.deep += 0.6;
  if (Number(progress.maxRepeatedFailure ?? 0) >= 2) scores.deep += 0.3;
  if (Number(progress.maxRepeatedFailure ?? 0) >= 3 || Number(progress.noProgressSteps ?? 0) >= 4) scores.ultrabrain += 0.85;

  const probabilities = normalizeScores(scores);
  const [category, categoryProbability] = Object.entries(probabilities).sort((a, b) => b[1] - a[1])[0];

  let think = 0.18;
  if (DEEP_TERMS.test(text)) think += 0.32;
  if (RESEARCH_TERMS.test(text)) think += 0.16;
  if (HIGH_RISK_TERMS.test(text)) think += 0.15;
  if (text.length > 500) think += 0.08;
  think += Math.min(0.35, Number(progress.maxRepeatedFailure ?? 0) * 0.12);
  think += Math.min(0.2, Number(progress.noProgressSteps ?? 0) * 0.05);
  think = clamp(think, 0, 1);

  const selectedNodes = Number(state.context?.selectedNodeCount ?? 0);
  const evidenceSufficient = clamp(selectedNodes ? 0.35 + Math.min(0.5, selectedNodes / 20) : 0.25, 0, 0.9);
  const stuck = clamp(
    Number(progress.maxRepeatedFailure ?? 0) * 0.24 + Number(progress.noProgressSteps ?? 0) * 0.12,
    0,
    1
  );

  const retrieval = /history|previous|before|commit|version|regression/i.test(text)
    ? 'historical'
    : /cause|why|root|failure|bug|debug/i.test(text)
      ? 'causal'
      : /dependency|import|call|reference|symbol/i.test(text)
        ? 'dependency'
        : 'lexical';

  return {
    category: {
      type: 'choice',
      choice: category,
      probabilities,
      confidence: categoryProbability
    },
    need_think: { type: 'noul', noul: think },
    evidence_sufficient: { type: 'noul', noul: evidenceSufficient },
    stuck: { type: 'noul', noul: stuck },
    retrieval: { type: 'choice', choice: retrieval, confidence: 0.65 }
  };
}

function mergeDecisionSignals(algorithm, models) {
  const merged = structuredClone(algorithm.answers ?? {});
  for (const result of models) {
    for (const [key, answer] of Object.entries(result.answers ?? {})) {
      if (!answer || typeof answer !== 'object') continue;
      if (answer.type === 'choice' || answer.choice !== undefined) {
        const confidence = Number(answer.confidence ?? topProbability(answer.probabilities) ?? 0);
        if (confidence >= 0.5) merged[key] = { ...answer, confidence };
      } else if (answer.type === 'noul' || answer.noul !== undefined) {
        const probability = Number(answer.noul);
        if (Number.isFinite(probability)) merged[key] = { ...answer, noul: clamp(probability, 0, 1) };
      } else if (answer.type === 'score' || answer.score !== undefined) {
        merged[key] = answer;
      }
    }
  }
  return merged;
}

function buildDecisionState({ goal, focus, step, session, context, progress }) {
  const workUnitState = session?.metadata?.workUnits;
  const activeWorkUnit = workUnitState?.activeId
    ? workUnitState.items?.[workUnitState.activeId]
    : null;
  return {
    goal: goal ?? session?.goal ?? '',
    focus: focus ?? '',
    step: Number(step ?? 0),
    progress,
    workUnit: activeWorkUnit ? {
      id: activeWorkUnit.id,
      goal: activeWorkUnit.goal,
      status: activeWorkUnit.status,
      risk: activeWorkUnit.risk,
      requiredEvidenceCount: activeWorkUnit.requiredEvidence?.length ?? 0,
      verificationCount: activeWorkUnit.verification?.length ?? 0
    } : null,
    context: {
      selectedNodeCount: context?.selectedNodes?.length ?? 0,
      usedTokens: context?.usedTokens ?? 0,
      budgetTokens: context?.budgetTokens ?? 0,
      contradictionCount: (context?.selectedEdges ?? []).filter((edge) => edge.type === 'contradicts' || edge.type === 'invalidates').length
    },
    recent: (session?.steps ?? []).slice(-3).map((record) => ({
      step: record.step,
      tools: (record.toolCalls ?? []).map((call) => ({ name: call.name, ok: call.ok }))
    }))
  };
}

function cognitivePrompt(route) {
  if (!route.think) {
    return `Cognitive policy for this step: category=${route.category}; deliberate Think mode is not required. Stay focused, use evidence, and verify the next concrete action.`;
  }
  return `Cognitive policy for this step: category=${route.category}; Think mode is active; reasoning effort=${route.effort}. Deliberate before acting: identify the most plausible hypotheses, missing evidence, and verification path. Do not expose private chain-of-thought; return only decisions, tool calls, concise rationale, and results.`;
}

function selectEffort(score, progress = {}) {
  if (Number(progress.maxRepeatedFailure ?? 0) >= 4) return 'max';
  if (score >= 0.86) return 'max';
  if (score >= 0.7) return 'high';
  if (score >= 0.48) return 'medium';
  return 'low';
}

function normalizeScores(scores) {
  const entries = Object.entries(scores).map(([key, value]) => [key, Math.max(0.001, Number(value) || 0)]);
  const total = entries.reduce((sum, [, value]) => sum + value, 0);
  return Object.fromEntries(entries.map(([key, value]) => [key, value / total]));
}

function topProbability(probabilities) {
  if (!probabilities || typeof probabilities !== 'object') return null;
  return Math.max(...Object.values(probabilities).map(Number).filter(Number.isFinite), 0);
}

function failureSignature({ name, args, result }) {
  const content = String(result?.content ?? '')
    .replace(/\b\d{4,}\b/g, '#')
    .replace(/0x[0-9a-f]+/gi, '0x#')
    .replace(/\/[^\s:]+/g, '/PATH')
    .slice(0, 1600);
  return hash({
    tool: name,
    denied: Boolean(result?.denied),
    args: normalizeFailureArgs(args),
    content
  }).slice(0, 20);
}

function normalizeFailureArgs(args) {
  if (!args || typeof args !== 'object') return args;
  return Object.fromEntries(
    Object.entries(args).map(([key, value]) => [
      key,
      key === 'command' ? String(value).replace(/\b\d{4,}\b/g, '#').slice(0, 400) : value
    ])
  );
}

function normalizeModelDescriptor(entry, fallbackProviderName) {
  if (typeof entry === 'object' && entry) {
    return {
      provider: entry.provider ?? fallbackProviderName,
      model: entry.model,
      baseURL: entry.baseURL,
      timeoutMs: entry.timeoutMs,
      headers: entry.headers
    };
  }

  const raw = String(entry);
  const colon = raw.indexOf(':');
  if (colon > 0) {
    return { provider: raw.slice(0, colon), model: raw.slice(colon + 1) };
  }

  const slash = raw.indexOf('/');
  if (slash > 0) {
    const prefix = raw.slice(0, slash);
    if (PROVIDER_PRESETS[prefix]) return { provider: prefix, model: raw.slice(slash + 1) };
  }

  return { provider: fallbackProviderName, model: raw };
}


function emptyHealthState() {
  return {
    successes: 0,
    failures: 0,
    consecutiveFailures: 0,
    openUntil: null,
    lastStatus: null,
    lastError: null,
    updatedAt: null
  };
}

function decisionHealthKey(provider) {
  return `decision:${provider?.name ?? 'unknown'}:${provider?.model ?? 'default'}`;
}

export function modelHealthKey(descriptor = {}) {
  return `model:${descriptor.provider ?? 'current'}:${descriptor.model ?? 'default'}`;
}
