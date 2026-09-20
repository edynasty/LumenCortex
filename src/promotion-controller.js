import { hash } from './util.js';

const DEFAULTS = {
  pressureThreshold: 0.72,
  nodeThreshold: 18,
  unresolvedThreshold: 6,
  maxChildren: 8,
  minChildren: 3,
  cooldownSteps: 4,
  reuseThreshold: 3,
  reuseNodeThreshold: 4
};

export class PromotionController {
  constructor(runtime, options = {}) {
    if (!runtime) throw new Error('runtime is required');
    this.runtime = runtime;
    this.options = { ...DEFAULTS, ...options };
    this.lastPromotionStep = -Infinity;
  }

  assess(context, { step = 0, activationCounts = {} } = {}) {
    const cfg = this.options;
    const nodes = context?.selectedNodes ?? [];
    const pressure = context?.budgetTokens
      ? context.usedTokens / context.budgetTokens
      : 0;
    const unresolved = nodes.reduce((sum, node) => sum + (node.unresolved?.length ?? 0), 0);
    const eligible = nodes
      .filter((node) => node.kind !== 'task' && node.kind !== 'abstraction')
      .sort((a, b) => (b.activation ?? 0) - (a.activation ?? 0));

    const reused = eligible
      .filter((node) => (activationCounts[node.id] ?? 0) >= cfg.reuseThreshold)
      .sort((a, b) => (activationCounts[b.id] ?? 0) - (activationCounts[a.id] ?? 0) || (b.activation ?? 0) - (a.activation ?? 0));

    const reasons = [];
    if (pressure >= cfg.pressureThreshold) reasons.push('context-pressure');
    if (eligible.length >= cfg.nodeThreshold) reasons.push('node-density');
    if (unresolved >= cfg.unresolvedThreshold) reasons.push('unresolved-density');
    if (reused.length >= cfg.reuseNodeThreshold) reasons.push('repeated-activation');

    const cooldownSatisfied = step - this.lastPromotionStep >= cfg.cooldownSteps;
    const shouldPromote = cooldownSatisfied &&
      eligible.length >= cfg.minChildren &&
      reasons.length > 0;

    const selected = reused.length >= cfg.reuseNodeThreshold ? reused : eligible;
    return {
      shouldPromote,
      pressure,
      unresolved,
      reused: reused.length,
      reasons,
      childIds: selected.slice(0, cfg.maxChildren).map((node) => node.id)
    };
  }

  maybePromote(goal, context, { step = 0, activationCounts = {}, metadata = {} } = {}) {
    const assessment = this.assess(context, { step, activationCounts });
    if (!assessment.shouldPromote) return { promoted: false, assessment };

    const promotionKey = hash({
      goal,
      childIds: [...assessment.childIds].sort()
    }).slice(0, 16);

    const existing = this.runtime.repository.graph().findNodes((node) =>
      node.kind === 'abstraction' &&
      node.metadata?.automaticPromotion === true &&
      node.metadata?.promotionKey === promotionKey &&
      node.status !== 'archived'
    )[0];

    if (existing) {
      this.lastPromotionStep = step;
      return { promoted: false, existing, assessment, deduplicated: true };
    }

    const abstraction = this.runtime.promote(assessment.childIds, {
      title: `Promoted context: ${String(goal).slice(0, 96)}`,
      generatedBy: 'active-promotion-controller',
      metadata: {
        automaticPromotion: true,
        promotionKey,
        trigger: {
          pressure: assessment.pressure,
          unresolved: assessment.unresolved,
          reasons: assessment.reasons,
          reused: assessment.reused,
          step
        },
        ...metadata
      }
    });

    this.lastPromotionStep = step;
    return { promoted: true, abstraction, assessment };
  }
}
