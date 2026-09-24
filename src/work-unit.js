import { randomUUID } from 'node:crypto';
import { nowIso } from './util.js';

export const WORK_UNIT_STATUSES = new Set([
  'pending',
  'active',
  'blocked',
  'verifying',
  'completed',
  'failed',
  'cancelled'
]);

export const WORK_UNIT_RISKS = new Set(['low', 'medium', 'high', 'critical']);

const TERMINAL_STATUSES = new Set(['completed', 'cancelled']);
const ALLOWED_TRANSITIONS = {
  pending: new Set(['active', 'blocked', 'cancelled']),
  active: new Set(['blocked', 'verifying', 'completed', 'failed', 'cancelled']),
  blocked: new Set(['pending', 'active', 'failed', 'cancelled']),
  verifying: new Set(['active', 'completed', 'failed', 'cancelled']),
  failed: new Set(['pending', 'active', 'cancelled']),
  completed: new Set(),
  cancelled: new Set()
};

export function createWorkUnit(input = {}, options = {}) {
  rejectModelRoutingFields(input);
  const goal = String(input.goal ?? '').trim();
  if (!goal) throw new Error('Work Unit goal is required');

  const now = options.now ?? nowIso();
  const unit = {
    id: input.id ?? `wu_${randomUUID().replaceAll('-', '').slice(0, 12)}`,
    goal,
    description: String(input.description ?? '').trim(),
    status: input.status ?? 'pending',
    risk: input.risk ?? 'medium',
    requiredEvidence: uniqueStrings(input.requiredEvidence ?? input.required_evidence ?? []),
    verification: uniqueStrings(input.verification ?? []),
    dependsOn: uniqueStrings(input.dependsOn ?? input.depends_on ?? []),
    evidence: normalizeEvidence(input.evidence ?? []),
    verificationResults: normalizeVerificationResults(input.verificationResults ?? input.verification_results ?? []),
    summary: String(input.summary ?? '').trim(),
    createdAt: input.createdAt ?? now,
    updatedAt: input.updatedAt ?? now,
    completedAt: input.completedAt ?? null,
    metadata: structuredClone(input.metadata ?? {})
  };
  validateWorkUnit(unit);
  return unit;
}

export function validateWorkUnit(unit) {
  if (!unit?.id || !/^[A-Za-z0-9._-]+$/.test(unit.id)) throw new Error('Invalid Work Unit id');
  if (!String(unit.goal ?? '').trim()) throw new Error(`Work Unit ${unit.id} goal is required`);
  if (!WORK_UNIT_STATUSES.has(unit.status)) throw new Error(`Invalid Work Unit status: ${unit.status}`);
  if (!WORK_UNIT_RISKS.has(unit.risk)) throw new Error(`Invalid Work Unit risk: ${unit.risk}`);
  if (unit.dependsOn?.includes(unit.id)) throw new Error(`Work Unit ${unit.id} cannot depend on itself`);
  rejectModelRoutingFields(unit);
  return unit;
}

export function validateWorkUnitPlan(units) {
  const normalized = (units ?? []).map((unit) => createWorkUnit(unit));
  const ids = new Set();
  for (const unit of normalized) {
    if (ids.has(unit.id)) throw new Error(`Duplicate Work Unit id: ${unit.id}`);
    ids.add(unit.id);
  }
  for (const unit of normalized) {
    for (const dependency of unit.dependsOn) {
      if (!ids.has(dependency)) throw new Error(`Work Unit ${unit.id} depends on unknown unit: ${dependency}`);
    }
  }
  detectDependencyCycles(normalized);
  return normalized;
}

export class WorkUnitManager {
  constructor(session) {
    if (!session) throw new Error('WorkUnitManager session is required');
    this.session = session;
    session.metadata ??= {};
    session.metadata.workUnits ??= {
      version: 1,
      order: [],
      items: {},
      activeId: null
    };
    session.metadata.workUnits.order ??= [];
    session.metadata.workUnits.items ??= {};
    session.metadata.workUnits.activeId ??= null;
  }

  seed(units) {
    if (!Array.isArray(units) || !units.length) return this.snapshot();
    if (this.state.order.length) throw new Error('Work Units are already initialized for this session');
    const normalized = validateWorkUnitPlan(units);
    for (const unit of normalized) {
      this.state.order.push(unit.id);
      this.state.items[unit.id] = unit;
    }
    this.ensureActive();
    return this.snapshot();
  }

  add(input) {
    const unit = createWorkUnit(input);
    if (this.state.items[unit.id]) throw new Error(`Work Unit already exists: ${unit.id}`);
    for (const dependency of unit.dependsOn) {
      if (!this.state.items[dependency]) throw new Error(`Unknown Work Unit dependency: ${dependency}`);
    }
    this.state.items[unit.id] = unit;
    this.state.order.push(unit.id);
    return structuredClone(unit);
  }

  get(id) {
    const unit = this.state.items[id];
    if (!unit) throw new Error(`Unknown Work Unit: ${id}`);
    return structuredClone(unit);
  }

  list() {
    return this.state.order.map((id) => structuredClone(this.state.items[id])).filter(Boolean);
  }

  current() {
    const id = this.state.activeId;
    if (!id) return null;
    const unit = this.state.items[id];
    if (!unit || TERMINAL_STATUSES.has(unit.status)) return null;
    return structuredClone(unit);
  }

  ready(id) {
    const unit = this.state.items[id];
    if (!unit || unit.status !== 'pending') return false;
    return unit.dependsOn.every((dependency) => this.state.items[dependency]?.status === 'completed');
  }

  activate(id) {
    const unit = this.require(id);
    if (!['pending', 'blocked', 'failed'].includes(unit.status)) {
      if (unit.status === 'active' || unit.status === 'verifying') return structuredClone(unit);
      throw new Error(`Work Unit ${id} cannot be activated from ${unit.status}`);
    }
    const unmet = unit.dependsOn.filter((dependency) => this.state.items[dependency]?.status !== 'completed');
    if (unmet.length) throw new Error(`Work Unit ${id} has unmet dependencies: ${unmet.join(', ')}`);
    this.transition(unit, 'active');
    this.state.activeId = id;
    return structuredClone(unit);
  }

  ensureActive() {
    const current = this.current();
    if (current) return current;
    this.state.activeId = null;
    const next = this.state.order.find((id) => this.ready(id));
    return next ? this.activate(next) : null;
  }

  update(id, patch = {}) {
    rejectModelRoutingFields(patch);
    const unit = this.require(id);

    if (patch.goal !== undefined) {
      const goal = String(patch.goal).trim();
      if (!goal) throw new Error('Work Unit goal cannot be empty');
      unit.goal = goal;
    }
    if (patch.description !== undefined) unit.description = String(patch.description ?? '').trim();
    if (patch.risk !== undefined) {
      if (!WORK_UNIT_RISKS.has(patch.risk)) throw new Error(`Invalid Work Unit risk: ${patch.risk}`);
      unit.risk = patch.risk;
    }
    if (patch.requiredEvidence !== undefined || patch.required_evidence !== undefined) {
      unit.requiredEvidence = uniqueStrings(patch.requiredEvidence ?? patch.required_evidence ?? []);
    }
    if (patch.verification !== undefined) unit.verification = uniqueStrings(patch.verification ?? []);
    if (patch.dependsOn !== undefined || patch.depends_on !== undefined) {
      const dependencies = uniqueStrings(patch.dependsOn ?? patch.depends_on ?? []);
      for (const dependency of dependencies) {
        if (dependency === id) throw new Error(`Work Unit ${id} cannot depend on itself`);
        if (!this.state.items[dependency]) throw new Error(`Unknown Work Unit dependency: ${dependency}`);
      }
      unit.dependsOn = dependencies;
      detectDependencyCycles(this.list().map((item) => item.id === id ? { ...item, dependsOn: dependencies } : item));
    }
    if (patch.evidence !== undefined) unit.evidence = normalizeEvidence(patch.evidence);
    if (patch.verificationResults !== undefined || patch.verification_results !== undefined) {
      unit.verificationResults = normalizeVerificationResults(patch.verificationResults ?? patch.verification_results ?? []);
    }
    if (patch.summary !== undefined) unit.summary = String(patch.summary ?? '').trim();
    if (patch.metadata !== undefined) unit.metadata = { ...unit.metadata, ...structuredClone(patch.metadata ?? {}) };

    if (patch.status && patch.status !== unit.status) {
      if (patch.status === 'completed') {
        this.assertCompletionEvidence(unit);
      }
      this.transition(unit, patch.status);
      if (patch.status === 'completed') unit.completedAt = nowIso();
      if (this.state.activeId === id && TERMINAL_STATUSES.has(patch.status)) this.state.activeId = null;
    }

    unit.updatedAt = nowIso();
    if (!this.state.activeId) this.ensureActive();
    return structuredClone(unit);
  }

  complete(id, patch = {}) {
    return this.update(id, { ...patch, status: 'completed' });
  }

  hasIncomplete() {
    return this.state.order.some((id) => {
      const status = this.state.items[id]?.status;
      return status && !TERMINAL_STATUSES.has(status);
    });
  }

  remaining() {
    return this.list().filter((unit) => !TERMINAL_STATUSES.has(unit.status));
  }

  snapshot() {
    return structuredClone(this.state);
  }

  assertCompletionEvidence(unit) {
    const evidenceRequirements = new Set(unit.evidence.map((item) => item.requirement).filter(Boolean));
    const missingEvidence = unit.requiredEvidence.filter((requirement) => !evidenceRequirements.has(requirement));
    if (missingEvidence.length) {
      throw new Error(`Work Unit ${unit.id} missing required evidence: ${missingEvidence.join(', ')}`);
    }

    const passedChecks = new Set(
      unit.verificationResults
        .filter((item) => item.status === 'passed')
        .map((item) => item.check)
        .filter(Boolean)
    );
    const missingVerification = unit.verification.filter((check) => !passedChecks.has(check));
    if (missingVerification.length) {
      throw new Error(`Work Unit ${unit.id} missing passed verification: ${missingVerification.join(', ')}`);
    }
  }

  transition(unit, nextStatus) {
    if (!WORK_UNIT_STATUSES.has(nextStatus)) throw new Error(`Invalid Work Unit status: ${nextStatus}`);
    if (!ALLOWED_TRANSITIONS[unit.status]?.has(nextStatus)) {
      throw new Error(`Work Unit ${unit.id} cannot transition ${unit.status} -> ${nextStatus}`);
    }
    unit.status = nextStatus;
    unit.updatedAt = nowIso();
  }

  require(id) {
    const unit = this.state.items[id];
    if (!unit) throw new Error(`Unknown Work Unit: ${id}`);
    return unit;
  }

  get state() {
    return this.session.metadata.workUnits;
  }
}

export function registerWorkUnitTools(registry) {
  if (!registry?.register) return registry;
  if (!registry.tools?.has('work_unit_list')) {
    registry.register({
      name: 'work_unit_list',
      description: 'Inspect persistent Work Units for the current Agent session, including dependencies, evidence requirements, verification and status.',
      permission: 'read',
      parameters: {
        type: 'object',
        properties: {},
        additionalProperties: false
      },
      execute(_args, context) {
        const manager = managerFromContext(context);
        return {
          active: manager.ensureActive(),
          units: manager.list()
        };
      }
    });
  }

  if (!registry.tools?.has('work_unit_create')) {
    registry.register({
      name: 'work_unit_create',
      description: 'Create a persistent Work Unit. Work Units describe goals/evidence/verification only and cannot choose providers, models or categories.',
      permission: 'read',
      parameters: {
        type: 'object',
        properties: {
          id: { type: 'string' },
          goal: { type: 'string' },
          description: { type: 'string' },
          risk: { type: 'string', enum: [...WORK_UNIT_RISKS] },
          required_evidence: { type: 'array', items: { type: 'string' } },
          verification: { type: 'array', items: { type: 'string' } },
          depends_on: { type: 'array', items: { type: 'string' } }
        },
        required: ['goal'],
        additionalProperties: false
      },
      execute(args, context) {
        const manager = managerFromContext(context);
        const unit = manager.add(args);
        manager.ensureActive();
        return { unit, active: manager.current() };
      }
    });
  }

  if (!registry.tools?.has('work_unit_update')) {
    registry.register({
      name: 'work_unit_update',
      description: 'Update a Work Unit status/evidence/verification. Completion is rejected until every required evidence item and verification check is satisfied.',
      permission: 'read',
      parameters: {
        type: 'object',
        properties: {
          id: { type: 'string' },
          status: { type: 'string', enum: [...WORK_UNIT_STATUSES] },
          summary: { type: 'string' },
          evidence: {
            type: 'array',
            items: {
              type: 'object',
              properties: {
                requirement: { type: 'string' },
                ref: { type: 'string' },
                summary: { type: 'string' }
              },
              required: ['requirement'],
              additionalProperties: false
            }
          },
          verification_results: {
            type: 'array',
            items: {
              type: 'object',
              properties: {
                check: { type: 'string' },
                status: { type: 'string', enum: ['passed', 'failed', 'unknown'] },
                detail: { type: 'string' }
              },
              required: ['check', 'status'],
              additionalProperties: false
            }
          }
        },
        required: ['id'],
        additionalProperties: false
      },
      execute({ id, ...patch }, context) {
        const manager = managerFromContext(context);
        const unit = manager.update(id, patch);
        return {
          unit,
          active: manager.ensureActive(),
          remaining: manager.remaining().map((item) => ({ id: item.id, goal: item.goal, status: item.status }))
        };
      }
    });
  }

  return registry;
}

export function formatWorkUnitPrompt(manager) {
  if (!manager) return '';
  const units = manager.list();
  if (!units.length) return '';
  const active = manager.ensureActive();
  const lines = units.map((unit) => {
    const marker = active?.id === unit.id ? '*' : '-';
    return `${marker} ${unit.id} [${unit.status}/${unit.risk}] ${unit.goal}`;
  });
  return [
    'Persistent Work Units are active for this session.',
    'Work Units never choose a model/provider/category. They only constrain execution goals, evidence and verification.',
    active ? `Current Work Unit: ${active.id} — ${active.goal}` : 'No Work Unit is currently ready.',
    ...lines,
    'Use work_unit_update to record evidence/verification and complete a unit. Do not return a final answer while non-terminal Work Units remain.'
  ].join('\n');
}

function managerFromContext(context) {
  if (!context?.session) throw new Error('Work Unit tool requires an active Agent session');
  return new WorkUnitManager(context.session);
}

function normalizeEvidence(values) {
  return (values ?? []).map((item) => {
    if (typeof item === 'string') return { requirement: item, ref: '', summary: '' };
    return {
      requirement: String(item?.requirement ?? '').trim(),
      ref: String(item?.ref ?? '').trim(),
      summary: String(item?.summary ?? '').trim()
    };
  }).filter((item) => item.requirement);
}

function normalizeVerificationResults(values) {
  return (values ?? []).map((item) => ({
    check: String(item?.check ?? '').trim(),
    status: ['passed', 'failed', 'unknown'].includes(item?.status) ? item.status : 'unknown',
    detail: String(item?.detail ?? '').trim()
  })).filter((item) => item.check);
}

function rejectModelRoutingFields(input) {
  for (const key of ['model', 'models', 'provider', 'category', 'reasoningEffort', 'reasoning_effort']) {
    if (Object.prototype.hasOwnProperty.call(input ?? {}, key)) {
      throw new Error(`Work Units cannot select ${key}`);
    }
  }
}

function uniqueStrings(values) {
  return [...new Set((values ?? []).map((value) => String(value).trim()).filter(Boolean))];
}

function detectDependencyCycles(units) {
  const byId = new Map(units.map((unit) => [unit.id, unit]));
  const visiting = new Set();
  const visited = new Set();

  const visit = (id) => {
    if (visited.has(id)) return;
    if (visiting.has(id)) throw new Error(`Work Unit dependency cycle detected at ${id}`);
    visiting.add(id);
    for (const dependency of byId.get(id)?.dependsOn ?? []) visit(dependency);
    visiting.delete(id);
    visited.add(id);
  };

  for (const id of byId.keys()) visit(id);
}
