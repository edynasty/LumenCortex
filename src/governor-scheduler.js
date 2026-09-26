import { hash, nowIso } from './util.js';
import { validateGraphGovernorPlan } from './graph-governor.js';

const STATE_KEY = 'graph_governor_scheduler_state_v1';

export const DEFAULT_GOVERNOR_SCHEDULER = Object.freeze({
  enabled: false,
  useCurator: false,
  autoApplySafe: false,
  checkRevisionDelta: 25,
  cooldownMs: 30 * 60 * 1000,
  archiveCandidateThreshold: 8,
  canonicalizeGroupThreshold: 3,
  branchCandidateThreshold: 3,
  promotionGroupThreshold: 3,
  tierChangeThreshold: 25
});

export class GraphGovernorScheduler {
  constructor({ repository, governor, options = {} } = {}) {
    if (!repository) throw new Error('GraphGovernorScheduler repository is required');
    if (!governor) throw new Error('GraphGovernorScheduler governor is required');
    this.repository = repository;
    this.governor = governor;
    this.options = normalizeGovernorSchedulerOptions(options);
  }

  status(now = Date.now()) {
    const state = this.#loadState();
    const revision = this.repository.graphRevision();
    const pending = state.pending
      ? {
          ...state.pending,
          stale: Number(state.pending.graphRevision) !== revision
        }
      : null;
    return {
      enabled: this.options.enabled,
      useCurator: this.options.useCurator,
      revision,
      config: { ...this.options },
      state: {
        ...state,
        pending
      },
      cooldownRemainingMs: cooldownRemaining(state, this.options, now)
    };
  }

  async evaluate(options = {}) {
    const cfg = normalizeGovernorSchedulerOptions({
      ...this.options,
      ...options
    });
    const force = Boolean(options.force);
    const nowMs = normalizeNow(options.now);
    const at = new Date(nowMs).toISOString();
    const revision = this.repository.graphRevision();
    const state = this.#loadState();

    if (!cfg.enabled && !force) {
      return this.#result({
        scheduled: false,
        reason: 'disabled',
        revision,
        state
      });
    }

    const revisionDelta = state.lastCheckRevision == null
      ? null
      : revision - Number(state.lastCheckRevision);

    if (!force && state.lastCheckRevision != null && revisionDelta < cfg.checkRevisionDelta) {
      return this.#result({
        scheduled: false,
        reason: 'revision-delta',
        revision,
        revisionDelta,
        state
      });
    }

    const remaining = cooldownRemaining(state, cfg, nowMs);
    if (!force && remaining > 0) {
      return this.#result({
        scheduled: false,
        reason: 'cooldown',
        revision,
        revisionDelta,
        cooldownRemainingMs: remaining,
        state
      });
    }

    const analysis = this.governor.analyze({ now: nowMs });
    const pressure = governancePressure(
      analysis,
      this.repository.database?.nodeStorageMap?.() ?? {},
      cfg
    );

    state.lastCheckAt = at;
    state.lastCheckRevision = revision;
    state.lastAnalysis = {
      metrics: analysis.metrics,
      pressure
    };

    if (!force && !pressure.triggered) {
      this.#saveState(state);
      this.#journal('governor.scheduler.checked', {
        revision,
        pressure,
        scheduled: false
      });
      return this.#result({
        scheduled: false,
        reason: 'no-pressure',
        revision,
        revisionDelta,
        analysis: state.lastAnalysis,
        state
      });
    }

    const proposal = await this.governor.propose({
      now: nowMs,
      curator: cfg.useCurator
    });
    const graph = this.repository.graph().snapshot();
    const validation = proposal.validation ?? validateGraphGovernorPlan(proposal.plan, graph);
    if (!validation.valid) {
      const error = new Error(
        `Graph Governor scheduler produced invalid plan: ${validation.errors.join('; ')}`
      );
      error.validation = validation;
      throw error;
    }

    const plan = validation.normalized;
    const planId = `govplan_${hash({
      revision,
      pressure: pressure.reasons,
      plan
    }).slice(0, 16)}`;

    if (state.pending && state.pending.id !== planId) {
      this.#journal('governor.scheduler.superseded', {
        previousPlanId: state.pending.id,
        previousRevision: state.pending.graphRevision,
        nextPlanId: planId,
        nextRevision: revision
      });
    }

    state.lastPlanAt = at;
    state.lastPlanRevision = revision;
    state.lastPlanId = planId;
    state.pending = {
      id: planId,
      createdAt: at,
      graphRevision: revision,
      triggers: pressure.reasons,
      curator: proposal.curator ?? null,
      plan
    };
    this.#saveState(state);
    this.#journal('governor.scheduler.planned', {
      planId,
      revision,
      triggers: pressure.reasons,
      curator: proposal.curator ?? null,
      counts: planCounts(plan)
    });

    if (cfg.autoApplySafe) {
      if (cfg.useCurator || proposal.curator?.enabled) {
        this.#journal('governor.scheduler.auto_apply_skipped', {
          planId,
          revision,
          reason: 'curator-plan-requires-explicit-apply'
        });
        return this.#result({
          scheduled: true,
          reason: force ? 'forced' : 'pressure',
          revision,
          revisionDelta,
          analysis: state.lastAnalysis,
          pending: state.pending,
          autoApplied: false,
          autoApplySkippedReason: 'curator-plan-requires-explicit-apply',
          state
        });
      }

      const preview = this.governor.applySafe(plan, {
        dryRun: true,
        commit: false
      });
      const safeChangedCount = Number(preview.changed?.length ?? 0);
      if (safeChangedCount > 0) {
        const applied = this.applyPending({
          automatic: true,
          semantic: false,
          createEpoch: false,
          message: `governor: auto-apply safe plan ${planId}`
        });
        const nextState = this.#loadState();
        return this.#result({
          scheduled: true,
          reason: force ? 'forced' : 'pressure',
          revision,
          revisionDelta,
          analysis: state.lastAnalysis,
          pending: null,
          autoApplied: true,
          safeChangedCount,
          apply: applied,
          state: nextState
        });
      }

      this.#journal('governor.scheduler.auto_apply_skipped', {
        planId,
        revision,
        reason: 'no-safe-changes'
      });
      return this.#result({
        scheduled: true,
        reason: force ? 'forced' : 'pressure',
        revision,
        revisionDelta,
        analysis: state.lastAnalysis,
        pending: state.pending,
        autoApplied: false,
        autoApplySkippedReason: 'no-safe-changes',
        state
      });
    }

    return this.#result({
      scheduled: true,
      reason: force ? 'forced' : 'pressure',
      revision,
      revisionDelta,
      analysis: state.lastAnalysis,
      pending: state.pending,
      autoApplied: false,
      state
    });
  }

  applyPending(options = {}) {
    const state = this.#loadState();
    const pending = state.pending;
    if (!pending) throw new Error('Graph Governor scheduler has no pending plan');

    if (options.planId && String(options.planId) !== pending.id) {
      throw new Error(
        `Pending Graph Governor plan changed: expected ${options.planId}, current ${pending.id}`
      );
    }

    const revision = this.repository.graphRevision();
    if (Number(pending.graphRevision) !== revision) {
      const error = new Error(
        `Pending Graph Governor plan is stale: planned at revision ${pending.graphRevision}, current ${revision}`
      );
      error.code = 'GOVERNOR_PLAN_STALE';
      error.planRevision = Number(pending.graphRevision);
      error.currentRevision = revision;
      throw error;
    }

    const result = this.governor.applyPlan(pending.plan, {
      semantic: Boolean(options.semantic),
      createEpoch: Boolean(options.createEpoch),
      dryRun: Boolean(options.dryRun),
      commit: options.commit,
      message: options.message ?? `governor: apply scheduled plan ${pending.id}`
    });

    if (options.dryRun) {
      return {
        planId: pending.id,
        pending: true,
        ...result
      };
    }

    const appliedAt = nowIso();
    state.lastApplyAt = appliedAt;
    state.lastApplyRevision = this.repository.graphRevision();
    state.lastAppliedPlanId = pending.id;
    if (options.automatic) {
      state.lastAutoApplyAt = appliedAt;
      state.lastAutoAppliedPlanId = pending.id;
    }
    state.pending = null;
    this.#saveState(state);
    this.#journal('governor.scheduler.applied', {
      planId: pending.id,
      plannedRevision: pending.graphRevision,
      appliedRevision: state.lastApplyRevision,
      semantic: Boolean(options.semantic),
      epoch: result.epoch ?? null,
      changedCount: result.changed?.length ?? 0,
      automatic: Boolean(options.automatic)
    });
    return {
      planId: pending.id,
      pending: false,
      automatic: Boolean(options.automatic),
      ...result
    };
  }

  clearPending(reason = 'manual-clear') {
    const state = this.#loadState();
    if (!state.pending) return { cleared: false, pending: null };
    const pending = state.pending;
    state.pending = null;
    this.#saveState(state);
    this.#journal('governor.scheduler.cleared', {
      planId: pending.id,
      graphRevision: pending.graphRevision,
      reason: String(reason).slice(0, 500)
    });
    return { cleared: true, pending };
  }

  #result(value) {
    return {
      ...value,
      config: { ...this.options }
    };
  }

  #loadState() {
    const raw = this.repository.database?.getMeta?.(STATE_KEY);
    if (!raw) return emptySchedulerState();
    try {
      const parsed = JSON.parse(raw);
      return {
        ...emptySchedulerState(),
        ...parsed,
        version: 1
      };
    } catch (error) {
      const wrapped = new Error('Invalid persisted Graph Governor scheduler state');
      wrapped.cause = error;
      throw wrapped;
    }
  }

  #saveState(state) {
    this.repository.database?.setMeta?.(STATE_KEY, JSON.stringify({
      ...state,
      version: 1
    }));
  }

  #journal(event, payload) {
    this.repository.appendJournal?.(event, payload);
  }
}

export function normalizeGovernorSchedulerOptions(input = {}) {
  return {
    enabled: input.enabled === true,
    useCurator: input.useCurator === true,
    autoApplySafe: input.autoApplySafe === true,
    checkRevisionDelta: positiveInteger(
      input.checkRevisionDelta,
      DEFAULT_GOVERNOR_SCHEDULER.checkRevisionDelta
    ),
    cooldownMs: nonNegativeNumber(
      input.cooldownMs,
      DEFAULT_GOVERNOR_SCHEDULER.cooldownMs
    ),
    archiveCandidateThreshold: positiveInteger(
      input.archiveCandidateThreshold,
      DEFAULT_GOVERNOR_SCHEDULER.archiveCandidateThreshold
    ),
    canonicalizeGroupThreshold: positiveInteger(
      input.canonicalizeGroupThreshold,
      DEFAULT_GOVERNOR_SCHEDULER.canonicalizeGroupThreshold
    ),
    branchCandidateThreshold: positiveInteger(
      input.branchCandidateThreshold,
      DEFAULT_GOVERNOR_SCHEDULER.branchCandidateThreshold
    ),
    promotionGroupThreshold: positiveInteger(
      input.promotionGroupThreshold,
      DEFAULT_GOVERNOR_SCHEDULER.promotionGroupThreshold
    ),
    tierChangeThreshold: positiveInteger(
      input.tierChangeThreshold,
      DEFAULT_GOVERNOR_SCHEDULER.tierChangeThreshold
    )
  };
}

export function governancePressure(analysis, storageByNode = {}, options = {}) {
  const cfg = normalizeGovernorSchedulerOptions(options);
  const counts = {
    archive: analysis?.candidates?.archive?.length ?? 0,
    canonicalize: analysis?.candidates?.canonicalize?.length ?? 0,
    branch: analysis?.candidates?.branch?.length ?? 0,
    promote: analysis?.candidates?.promote?.length ?? 0,
    tierChanges: countTierChanges(analysis?.tiers, storageByNode)
  };
  const reasons = [];
  if (analysis?.epoch?.recommended) {
    reasons.push(...(analysis.epoch.reasons ?? []).map((reason) => `epoch:${reason}`));
  }
  if (counts.archive >= cfg.archiveCandidateThreshold) reasons.push('archive-backlog');
  if (counts.canonicalize >= cfg.canonicalizeGroupThreshold) reasons.push('canonicalization-backlog');
  if (counts.branch >= cfg.branchCandidateThreshold) reasons.push('branch-backlog');
  if (counts.promote >= cfg.promotionGroupThreshold) reasons.push('promotion-backlog');
  if (counts.tierChanges >= cfg.tierChangeThreshold) reasons.push('tier-drift');
  return {
    triggered: reasons.length > 0,
    reasons: [...new Set(reasons)],
    counts
  };
}

function countTierChanges(tiers = {}, storageByNode = {}) {
  let count = 0;
  for (const tier of ['hot', 'warm', 'cold']) {
    for (const nodeId of tiers?.[tier] ?? []) {
      const current = String(storageByNode?.[nodeId]?.tier ?? 'warm').toLowerCase();
      if (current !== tier) count += 1;
    }
  }
  return count;
}

function cooldownRemaining(state, cfg, nowMs) {
  if (!state.lastCheckAt || cfg.cooldownMs <= 0) return 0;
  const last = new Date(state.lastCheckAt).getTime();
  if (!Number.isFinite(last)) return 0;
  return Math.max(0, cfg.cooldownMs - Math.max(0, nowMs - last));
}

function emptySchedulerState() {
  return {
    version: 1,
    lastCheckAt: null,
    lastCheckRevision: null,
    lastAnalysis: null,
    lastPlanAt: null,
    lastPlanRevision: null,
    lastPlanId: null,
    lastApplyAt: null,
    lastApplyRevision: null,
    lastAppliedPlanId: null,
    lastAutoApplyAt: null,
    lastAutoAppliedPlanId: null,
    pending: null
  };
}

function normalizeNow(value) {
  if (value instanceof Date) return value.getTime();
  const number = Number(value ?? Date.now());
  return Number.isFinite(number) ? number : Date.now();
}

function positiveInteger(value, fallback) {
  const number = Math.floor(Number(value));
  return Number.isFinite(number) && number >= 1 ? number : fallback;
}

function nonNegativeNumber(value, fallback) {
  const number = Number(value);
  return Number.isFinite(number) && number >= 0 ? number : fallback;
}

function planCounts(plan) {
  return {
    archive: plan?.archive?.length ?? 0,
    hot: plan?.tiers?.hot?.length ?? 0,
    warm: plan?.tiers?.warm?.length ?? 0,
    cold: plan?.tiers?.cold?.length ?? 0,
    canonicalize: plan?.canonicalize?.length ?? 0,
    branch: plan?.branch?.length ?? 0,
    promote: plan?.promote?.length ?? 0,
    epoch: Boolean(plan?.epoch?.proposed)
  };
}
