import fs from 'node:fs';
import path from 'node:path';

const MAX_HISTORY = 256;
const COMPARATORS = new Set(['equals', 'notEquals', 'exists', 'contains', 'matches', 'in', 'gt', 'gte', 'lt', 'lte']);

export function loadWorkflowFile(filePath) {
  const absolute = path.resolve(filePath);
  return validateWorkflowDefinition(JSON.parse(fs.readFileSync(absolute, 'utf8')));
}

export function validateWorkflowDefinition(input) {
  if (!input || typeof input !== 'object' || Array.isArray(input)) {
    throw new Error('Workflow definition must be an object');
  }
  const definition = structuredClone(input);
  definition.version ??= 1;
  if (definition.version !== 1) throw new Error(`Unsupported workflow version: ${definition.version}`);
  if (!nonEmpty(definition.id)) throw new Error('Workflow id is required');
  if (!definition.actions || typeof definition.actions !== 'object' || Array.isArray(definition.actions)) {
    throw new Error('Workflow actions must be an object keyed by action id');
  }
  const actionIds = Object.keys(definition.actions);
  if (!actionIds.length) throw new Error('Workflow must define at least one action');
  definition.entry ??= actionIds[0];
  if (!definition.actions[definition.entry]) throw new Error(`Unknown workflow entry action: ${definition.entry}`);
  definition.title ??= definition.id;
  definition.facts = normalizeFacts(definition.facts ?? {});

  for (const actionId of actionIds) {
    const source = definition.actions[actionId];
    if (!source || typeof source !== 'object' || Array.isArray(source)) {
      throw new Error(`Workflow action ${actionId} must be an object`);
    }
    const action = { ...source };
    action.id = actionId;
    action.title ??= actionId;
    action.description ??= '';
    action.terminal = Boolean(action.terminal);
    action.allowedTools = normalizeTools(action.allowedTools, actionId);
    action.outcomes = normalizeArray(action.outcomes, 'outcomes', actionId).map((outcome, index) => {
      if (!outcome || typeof outcome !== 'object' || Array.isArray(outcome)) {
        throw new Error(`Outcome ${actionId}[${index}] must be an object`);
      }
      validateCondition(outcome.when ?? true, `outcome ${actionId}[${index}].when`);
      if (!outcome.set || typeof outcome.set !== 'object' || Array.isArray(outcome.set)) {
        throw new Error(`Outcome ${actionId}[${index}] requires a set object`);
      }
      return {
        id: outcome.id ?? `${actionId}.outcome.${index + 1}`,
        when: outcome.when ?? true,
        set: structuredClone(outcome.set)
      };
    });
    action.routes = normalizeArray(action.routes, 'routes', actionId).map((route, index) => {
      if (!route || typeof route !== 'object' || Array.isArray(route) || !nonEmpty(route.to)) {
        throw new Error(`Route ${actionId}[${index}] requires a target action`);
      }
      validateCondition(route.when ?? true, `route ${actionId}[${index}].when`);
      return { to: route.to, when: route.when ?? true };
    });
    action.gates = normalizeArray(action.gates, 'gates', actionId).map((gate, index) => normalizeGate(gate, actionId, index));
    if (action.requires != null) validateCondition(action.requires, `action ${actionId}.requires`);
    if (action.completeWhen != null) validateCondition(action.completeWhen, `action ${actionId}.completeWhen`);
    definition.actions[actionId] = action;
  }

  for (const [actionId, action] of Object.entries(definition.actions)) {
    for (const route of action.routes) {
      if (!definition.actions[route.to]) throw new Error(`Action ${actionId} routes to unknown action: ${route.to}`);
    }
  }
  return definition;
}

export function evaluateWorkflowCondition(condition, context = {}) {
  if (condition == null) return true;
  if (typeof condition === 'boolean') return condition;
  if (!condition || typeof condition !== 'object' || Array.isArray(condition)) return false;
  if (Array.isArray(condition.all)) return condition.all.every((item) => evaluateWorkflowCondition(item, context));
  if (Array.isArray(condition.any)) return condition.any.some((item) => evaluateWorkflowCondition(item, context));
  if (Object.hasOwn(condition, 'not')) return !evaluateWorkflowCondition(condition.not, context);

  const checks = [];
  if (Object.hasOwn(condition, 'tool')) {
    const tools = Array.isArray(condition.tool) ? condition.tool : [condition.tool];
    checks.push(tools.includes(context.tool));
  }
  if (Object.hasOwn(condition, 'ok')) checks.push(Boolean(context.result?.ok) === Boolean(condition.ok));
  if (Object.hasOwn(condition, 'fact')) checks.push(compareValue(getPath(context.facts ?? {}, condition.fact), condition));
  if (Object.hasOwn(condition, 'arg')) checks.push(compareValue(getPath(context.args ?? {}, condition.arg), condition));
  if (Object.hasOwn(condition, 'result')) checks.push(compareValue(lookupResult(context, condition.result), condition));
  return checks.length > 0 && checks.every(Boolean);
}

export class WorkflowRuntime {
  constructor(definition, state = null) {
    this.definition = validateWorkflowDefinition(definition);
    const restored = state ? structuredClone(state) : null;
    this.currentAction = restored?.currentAction ?? this.definition.entry;
    if (!this.definition.actions[this.currentAction]) {
      throw new Error(`Persisted workflow references unknown action: ${this.currentAction}`);
    }
    this.facts = restored?.facts ? structuredClone(restored.facts) : structuredClone(this.definition.facts);
    this.factSources = restored?.factSources ? structuredClone(restored.factSources) : {};
    this.history = Array.isArray(restored?.history) ? structuredClone(restored.history).slice(-MAX_HISTORY) : [];
    this.status = restored?.status ?? 'running';

    if (!restored) {
      seedFactSources(this.factSources, this.facts);
      this.history.push({ at: nowIso(), type: 'start', action: this.currentAction });
    }
    this.assertCurrentRequirements();
    this.advance('hydrate');
  }

  static fromSession(session, suppliedDefinition) {
    const persisted = session?.metadata?.workflow ?? null;
    if (!persisted && !suppliedDefinition) return null;
    if (persisted) {
      if (!persisted.definition) throw new Error('Persisted workflow is missing its definition');
      const storedDefinition = validateWorkflowDefinition(persisted.definition);
      if (suppliedDefinition) {
        const candidate = validateWorkflowDefinition(suppliedDefinition);
        if (canonical(candidate) !== canonical(storedDefinition)) {
          throw new Error('Workflow definition differs from the contract persisted in this session');
        }
      }
      return new WorkflowRuntime(storedDefinition, persisted);
    }
    return new WorkflowRuntime(suppliedDefinition);
  }

  action() { return this.definition.actions[this.currentAction]; }
  actionId() { return this.currentAction; }

  isToolAllowed(name) {
    const allowed = this.action().allowedTools;
    return allowed == null || allowed.includes(name);
  }

  effectiveAllowlist(baseAllowlist) {
    const actionTools = this.action().allowedTools;
    if (actionTools == null) return baseAllowlist == null ? null : [...baseAllowlist];
    if (baseAllowlist == null) return [...actionTools];
    const base = new Set(baseAllowlist);
    return actionTools.filter((name) => base.has(name));
  }

  observeTool({ tool, args = {}, result = {}, step = null } = {}) {
    const before = this.currentAction;
    const changedFacts = [];
    const action = this.action();
    const context = {
      facts: this.facts,
      tool,
      args,
      result,
      resultData: parseResultContent(result?.content)
    };

    for (const outcome of action.outcomes) {
      if (!evaluateWorkflowCondition(outcome.when, context)) continue;
      for (const [factPath, value] of Object.entries(outcome.set)) {
        const previous = getPath(this.facts, factPath);
        setPath(this.facts, factPath, structuredClone(value));
        this.factSources[factPath] = {
          at: nowIso(),
          source: 'tool',
          action: before,
          outcome: outcome.id,
          tool,
          step,
          args: compact(args),
          result: compactResult(result, context.resultData)
        };
        if (!same(previous, value)) changedFacts.push({ path: factPath, previous, value });
      }
    }

    this.history.push({
      at: nowIso(),
      type: 'tool',
      action: before,
      tool,
      step,
      ok: Boolean(result?.ok),
      facts: changedFacts.map((item) => item.path)
    });
    this.trimHistory();
    const transition = this.advance(`tool:${tool}`);
    return { changedFacts, transition, beforeAction: before, currentAction: this.currentAction, status: this.status };
  }

  approve(gateId, { actor = 'human' } = {}) {
    const gate = this.action().gates.find((item) => item.id === gateId);
    if (!gate) throw new Error(`Unknown gate ${gateId} for action ${this.currentAction}`);
    if (gate.type !== 'human') throw new Error(`Gate ${gateId} is not a human gate`);
    const factPath = gate.fact ?? `gates.${gate.id}`;
    const value = Object.hasOwn(gate, 'equals') ? gate.equals : true;
    setPath(this.facts, factPath, structuredClone(value));
    this.factSources[factPath] = { at: nowIso(), source: 'human', actor, action: this.currentAction, gate: gate.id };
    this.history.push({ at: nowIso(), type: 'approve', action: this.currentAction, gate: gate.id, actor });
    this.trimHistory();
    const transition = this.advance(`approve:${gate.id}`);
    return { gate: gate.id, fact: factPath, value, transition, currentAction: this.currentAction, status: this.status };
  }

  canFinish() {
    return this.action().terminal && this.actionSatisfied(this.action());
  }

  waitingHumanGates() {
    const action = this.action();
    if (!this.baseCompletionSatisfied(action)) return [];
    const nonHumanBlocked = action.gates.some((gate) => gate.type !== 'human' && !this.gateSatisfied(gate));
    if (nonHumanBlocked) return [];
    return action.gates
      .filter((gate) => gate.type === 'human' && !this.gateSatisfied(gate))
      .map((gate) => ({
        id: gate.id,
        title: gate.title,
        description: gate.description,
        fact: gate.fact ?? `gates.${gate.id}`
      }));
  }

  pendingGates() {
    return this.action().gates
      .filter((gate) => !this.gateSatisfied(gate))
      .map((gate) => ({ id: gate.id, type: gate.type, title: gate.title, description: gate.description }));
  }

  blockReason() {
    const pending = this.pendingGates();
    if (pending.length) return `workflow action ${this.currentAction} has pending gate(s): ${pending.map((gate) => gate.id).join(', ')}`;
    if (!this.baseCompletionSatisfied(this.action())) return `workflow action ${this.currentAction} has not satisfied its completion evidence`;
    if (!this.action().terminal) return `workflow is not at a terminal action; current action is ${this.currentAction}`;
    return 'workflow completion is not yet proven';
  }

  prompt() {
    const action = this.action();
    const payload = {
      workflow: { id: this.definition.id, title: this.definition.title, status: this.status },
      currentAction: {
        id: this.currentAction,
        title: action.title,
        description: action.description,
        terminal: action.terminal,
        allowedTools: action.allowedTools,
        completeWhen: action.completeWhen,
        gates: action.gates.map((gate) => ({ id: gate.id, type: gate.type, title: gate.title, satisfied: this.gateSatisfied(gate) }))
      },
      facts: this.facts
    };
    return [
      'LumenCortex Workflow Contract is ACTIVE and binding.',
      'Reason freely inside the current action, but obey its tool boundary and completion evidence.',
      'Do not claim completion until the runtime reaches a satisfied terminal action.',
      JSON.stringify(payload, null, 2).slice(0, 12000)
    ].join('\n\n');
  }

  summary() {
    const action = this.action();
    return {
      id: this.definition.id,
      title: this.definition.title,
      currentAction: this.currentAction,
      currentTitle: action.title,
      terminal: action.terminal,
      status: this.status,
      allowedTools: action.allowedTools == null ? null : [...action.allowedTools],
      canFinish: this.canFinish(),
      pendingGates: this.pendingGates(),
      facts: structuredClone(this.facts)
    };
  }

  snapshot() {
    return {
      version: 1,
      definition: structuredClone(this.definition),
      currentAction: this.currentAction,
      facts: structuredClone(this.facts),
      factSources: structuredClone(this.factSources),
      history: structuredClone(this.history.slice(-MAX_HISTORY)),
      status: this.status
    };
  }

  advance(reason = 'evaluate') {
    let transition = null;
    for (let guard = 0; guard < 32; guard += 1) {
      const action = this.action();
      const waiting = this.waitingHumanGates();
      if (waiting.length) {
        this.status = 'waiting_gate';
        break;
      }
      if (!this.actionSatisfied(action)) {
        this.status = 'running';
        break;
      }
      if (action.terminal) {
        this.status = 'ready_to_finish';
        break;
      }
      const route = action.routes.find((candidate) => evaluateWorkflowCondition(candidate.when, { facts: this.facts }));
      if (!route) {
        this.status = 'running';
        break;
      }
      const target = this.definition.actions[route.to];
      if (target.requires != null && !evaluateWorkflowCondition(target.requires, { facts: this.facts })) {
        this.status = 'blocked';
        break;
      }
      const from = this.currentAction;
      this.currentAction = route.to;
      transition = { from, to: route.to, reason };
      this.history.push({ at: nowIso(), type: 'transition', from, to: route.to, reason });
      this.trimHistory();
    }
    return transition;
  }

  actionSatisfied(action) {
    return this.baseCompletionSatisfied(action) && action.gates.every((gate) => this.gateSatisfied(gate));
  }

  baseCompletionSatisfied(action) {
    if (action.completeWhen != null) return evaluateWorkflowCondition(action.completeWhen, { facts: this.facts });
    if (action.terminal) return true;
    if (action.routes.length) return action.routes.some((route) => evaluateWorkflowCondition(route.when, { facts: this.facts }));
    return false;
  }

  gateSatisfied(gate) {
    if (gate.type === 'human') {
      const actual = getPath(this.facts, gate.fact ?? `gates.${gate.id}`);
      const expected = Object.hasOwn(gate, 'equals') ? gate.equals : true;
      return same(actual, expected);
    }
    return evaluateWorkflowCondition(gate.condition, { facts: this.facts });
  }

  assertCurrentRequirements() {
    const action = this.action();
    if (action.requires != null && !evaluateWorkflowCondition(action.requires, { facts: this.facts })) {
      throw new Error(`Workflow action requirements are not satisfied for ${this.currentAction}`);
    }
  }

  trimHistory() {
    if (this.history.length > MAX_HISTORY) this.history.splice(0, this.history.length - MAX_HISTORY);
  }
}

function normalizeGate(gate, actionId, index) {
  if (!gate || typeof gate !== 'object' || Array.isArray(gate)) throw new Error(`Gate ${actionId}[${index}] must be an object`);
  const normalized = { ...gate };
  normalized.id ??= `${actionId}.gate.${index + 1}`;
  normalized.title ??= normalized.id;
  normalized.description ??= '';
  normalized.type ??= 'condition';
  if (!['condition', 'human'].includes(normalized.type)) throw new Error(`Unsupported gate type ${normalized.type} for ${normalized.id}`);
  if (normalized.type === 'human') normalized.fact ??= `gates.${normalized.id}`;
  else {
    if (normalized.condition == null) throw new Error(`Condition gate ${normalized.id} requires condition`);
    validateCondition(normalized.condition, `gate ${normalized.id}.condition`);
  }
  return normalized;
}

function validateCondition(condition, label) {
  if (condition == null || typeof condition === 'boolean') return;
  if (!condition || typeof condition !== 'object' || Array.isArray(condition)) throw new Error(`Invalid workflow condition at ${label}`);
  if (Object.hasOwn(condition, 'all')) {
    if (!Array.isArray(condition.all)) throw new Error(`${label}.all must be an array`);
    condition.all.forEach((item, index) => validateCondition(item, `${label}.all[${index}]`));
    return;
  }
  if (Object.hasOwn(condition, 'any')) {
    if (!Array.isArray(condition.any)) throw new Error(`${label}.any must be an array`);
    condition.any.forEach((item, index) => validateCondition(item, `${label}.any[${index}]`));
    return;
  }
  if (Object.hasOwn(condition, 'not')) {
    validateCondition(condition.not, `${label}.not`);
    return;
  }
  const selectors = ['fact', 'tool', 'ok', 'arg', 'result'].filter((key) => Object.hasOwn(condition, key));
  if (!selectors.length) throw new Error(`Condition at ${label} has no selector`);
  for (const key of Object.keys(condition)) {
    if (selectors.includes(key) || COMPARATORS.has(key)) continue;
    throw new Error(`Unknown workflow condition key ${key} at ${label}`);
  }
}

function compareValue(actual, condition) {
  if (Object.hasOwn(condition, 'exists')) return condition.exists ? actual !== undefined : actual === undefined;
  if (Object.hasOwn(condition, 'equals')) return same(actual, condition.equals);
  if (Object.hasOwn(condition, 'notEquals')) return !same(actual, condition.notEquals);
  if (Object.hasOwn(condition, 'contains')) {
    if (typeof actual === 'string') return actual.includes(String(condition.contains));
    if (Array.isArray(actual)) return actual.some((item) => same(item, condition.contains));
    return false;
  }
  if (Object.hasOwn(condition, 'matches')) {
    try { return new RegExp(String(condition.matches)).test(String(actual ?? '')); }
    catch { return false; }
  }
  if (Object.hasOwn(condition, 'in')) return Array.isArray(condition.in) && condition.in.some((item) => same(actual, item));
  if (Object.hasOwn(condition, 'gt')) return Number(actual) > Number(condition.gt);
  if (Object.hasOwn(condition, 'gte')) return Number(actual) >= Number(condition.gte);
  if (Object.hasOwn(condition, 'lt')) return Number(actual) < Number(condition.lt);
  if (Object.hasOwn(condition, 'lte')) return Number(actual) <= Number(condition.lte);
  return Boolean(actual);
}

function lookupResult(context, resultPath) {
  const fromParsed = getPath(context.resultData, resultPath);
  if (fromParsed !== undefined) return fromParsed;
  return getPath(context.result ?? {}, resultPath);
}

function parseResultContent(content) {
  if (content == null) return {};
  if (typeof content === 'object') return content;
  try {
    const parsed = JSON.parse(String(content));
    return parsed && typeof parsed === 'object' ? parsed : { content: parsed };
  } catch {
    return { content: String(content) };
  }
}

function getPath(target, dottedPath) {
  if (!dottedPath) return target;
  return String(dottedPath).split('.').reduce((value, key) => value == null ? undefined : value[key], target);
}

function setPath(target, dottedPath, value) {
  const parts = String(dottedPath).split('.').filter(Boolean);
  if (!parts.length) throw new Error('Fact path cannot be empty');
  let cursor = target;
  for (const part of parts.slice(0, -1)) {
    if (!cursor[part] || typeof cursor[part] !== 'object' || Array.isArray(cursor[part])) cursor[part] = {};
    cursor = cursor[part];
  }
  cursor[parts.at(-1)] = value;
}

function seedFactSources(target, facts, prefix = '') {
  for (const [key, value] of Object.entries(facts ?? {})) {
    const factPath = prefix ? `${prefix}.${key}` : key;
    if (value && typeof value === 'object' && !Array.isArray(value)) seedFactSources(target, value, factPath);
    else target[factPath] = { at: nowIso(), source: 'initial' };
  }
}

function normalizeFacts(value) {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error('Workflow facts must be an object');
  return structuredClone(value);
}
function normalizeTools(value, actionId) {
  if (value == null) return null;
  if (!Array.isArray(value) || value.some((item) => !nonEmpty(item))) throw new Error(`allowedTools for ${actionId} must be an array of tool names`);
  return [...new Set(value)];
}
function normalizeArray(value, name, actionId) {
  if (value == null) return [];
  if (!Array.isArray(value)) throw new Error(`${name} for ${actionId} must be an array`);
  return value;
}
function compactResult(result, parsed) {
  return { ok: Boolean(result?.ok), denied: Boolean(result?.denied), permission: result?.permission ?? null, data: compact(parsed) };
}
function compact(value) {
  const text = JSON.stringify(value ?? {});
  if (text.length <= 1200) return structuredClone(value ?? {});
  return { truncated: true, preview: text.slice(0, 1200) };
}
function same(left, right) { return canonical(left) === canonical(right); }
function canonical(value) { return JSON.stringify(sortValue(value)); }
function sortValue(value) {
  if (Array.isArray(value)) return value.map(sortValue);
  if (!value || typeof value !== 'object') return value;
  return Object.fromEntries(Object.keys(value).sort().map((key) => [key, sortValue(value[key])]));
}
function nonEmpty(value) { return typeof value === 'string' && value.trim().length > 0; }
function nowIso() { return new Date().toISOString(); }
