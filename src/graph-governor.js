import { GRADE_WEIGHTS, TRUST_WEIGHTS } from './constants.js';
import { nowIso } from './util.js';

const STATUS_WEIGHT = {
  active: 1,
  dormant: 0.62,
  stale: 0.34,
  archived: 0.08,
  invalid: 0
};

export class GraphGovernorAnalyzer {
  constructor(options = {}) {
    this.hotThreshold = Number(options.hotThreshold ?? 0.68);
    this.warmThreshold = Number(options.warmThreshold ?? 0.4);
    this.archiveThreshold = Number(options.archiveThreshold ?? 0.24);
    this.promotionMinGroup = Math.max(3, Number(options.promotionMinGroup ?? 3));
  }

  analyze(graphState, options = {}) {
    const nodes = Object.values(graphState?.nodes ?? {});
    const edges = Object.values(graphState?.edges ?? {});
    const degree = degreeMap(nodes, edges);
    const maxDegree = Math.max(1, ...degree.values());
    const values = {};

    for (const node of nodes) {
      values[node.id] = nodeValue(node, {
        degree: degree.get(node.id) ?? 0,
        maxDegree
      });
    }

    const tiers = { hot: [], warm: [], cold: [] };
    for (const node of nodes) {
      const value = values[node.id];
      if (value >= this.hotThreshold) tiers.hot.push(node.id);
      else if (value >= this.warmThreshold) tiers.warm.push(node.id);
      else tiers.cold.push(node.id);
    }

    const archiveCandidates = nodes
      .filter((node) => archiveCandidate(node, values[node.id], degree.get(node.id) ?? 0, this.archiveThreshold))
      .sort((a, b) => values[a.id] - values[b.id])
      .map((node) => ({
        nodeId: node.id,
        value: values[node.id],
        status: node.status,
        degree: degree.get(node.id) ?? 0,
        reason: 'low-value stale-or-dormant candidate'
      }));

    const canonicalizationCandidates = duplicateTitleGroups(nodes);
    const branchCandidates = edges
      .filter((edge) => edge.type === 'contradicts' || edge.type === 'invalidates')
      .map((edge) => ({
        edgeId: edge.id,
        from: edge.from,
        to: edge.to,
        relation: edge.type,
        weight: Number(edge.weight ?? 1)
      }))
      .sort((a, b) => b.weight - a.weight);

    const promotionCandidates = promotionGroups(nodes, this.promotionMinGroup);
    const staleCount = nodes.filter((node) => node.status === 'stale').length;
    const archivedCount = nodes.filter((node) => node.status === 'archived').length;
    const contradictionCount = branchCandidates.length;
    const duplicateGroupCount = canonicalizationCandidates.length;
    const nodeCount = nodes.length;
    const staleRatio = nodeCount ? staleCount / nodeCount : 0;

    const epochReasons = [];
    if (staleRatio >= Number(options.epochStaleRatio ?? 0.3)) epochReasons.push('stale-ratio');
    if (duplicateGroupCount >= Number(options.epochDuplicateGroups ?? 5)) epochReasons.push('canonicalization-backlog');
    if (nodeCount >= Number(options.epochNodeCount ?? 50000)) epochReasons.push('graph-size');

    return {
      generatedAt: nowIso(),
      metrics: {
        nodeCount,
        edgeCount: edges.length,
        staleCount,
        archivedCount,
        staleRatio,
        contradictionCount,
        duplicateGroupCount,
        promotionGroupCount: promotionCandidates.length,
        tierCounts: {
          hot: tiers.hot.length,
          warm: tiers.warm.length,
          cold: tiers.cold.length
        }
      },
      values,
      tiers,
      candidates: {
        archive: archiveCandidates,
        canonicalize: canonicalizationCandidates,
        branch: branchCandidates,
        promote: promotionCandidates
      },
      epoch: {
        recommended: epochReasons.length > 0,
        reasons: epochReasons
      }
    };
  }
}

export function validateGraphGovernorPlan(plan, graphState) {
  const errors = [];
  const warnings = [];
  const nodes = graphState?.nodes ?? {};
  const ids = new Set(Object.keys(nodes));

  const archive = uniqueStrings(plan?.archive ?? []);
  for (const id of archive) {
    const node = nodes[id];
    if (!node) {
      errors.push(`archive references unknown node: ${id}`);
      continue;
    }
    if (node.kind === 'evidence' && ['runtime', 'reproduced'].includes(node.grade)) {
      errors.push(`high-grade evidence cannot be auto-archived: ${id}`);
    }
    if (node.status === 'invalid') warnings.push(`node is already invalid: ${id}`);
  }

  const tiers = plan?.tiers ?? {};
  for (const tier of ['hot', 'warm', 'cold']) {
    for (const id of uniqueStrings(tiers[tier] ?? [])) {
      if (!ids.has(id)) errors.push(`tier ${tier} references unknown node: ${id}`);
    }
  }

  const tierMembership = new Map();
  for (const tier of ['hot', 'warm', 'cold']) {
    for (const id of uniqueStrings(tiers[tier] ?? [])) {
      const existing = tierMembership.get(id);
      if (existing && existing !== tier) errors.push(`node appears in multiple tiers: ${id}`);
      tierMembership.set(id, tier);
    }
  }

  for (const item of plan?.canonicalize ?? []) {
    if (!item?.canonical || !ids.has(item.canonical)) errors.push('canonicalize canonical node is missing or unknown');
    for (const alias of uniqueStrings(item?.aliases ?? [])) {
      if (!ids.has(alias)) errors.push(`canonicalize alias references unknown node: ${alias}`);
      if (alias === item.canonical) errors.push(`canonicalize alias equals canonical node: ${alias}`);
    }
  }

  for (const item of plan?.branch ?? []) {
    if (!item?.from || !ids.has(item.from)) errors.push('branch from node is missing or unknown');
    if (!item?.to || !ids.has(item.to)) errors.push('branch to node is missing or unknown');
  }

  for (const item of plan?.promote ?? []) {
    const children = uniqueStrings(item?.childIds ?? []);
    if (children.length < 3) warnings.push('promotion has fewer than three children');
    for (const id of children) if (!ids.has(id)) errors.push(`promotion references unknown node: ${id}`);
  }

  return {
    valid: errors.length === 0,
    errors,
    warnings,
    normalized: {
      archive,
      tiers: {
        hot: uniqueStrings(tiers.hot ?? []),
        warm: uniqueStrings(tiers.warm ?? []),
        cold: uniqueStrings(tiers.cold ?? [])
      },
      canonicalize: plan?.canonicalize ?? [],
      branch: plan?.branch ?? [],
      promote: plan?.promote ?? [],
      epoch: plan?.epoch ?? null
    }
  };
}

export class GraphGovernor {
  constructor({ repository, analyzer = new GraphGovernorAnalyzer(), curator } = {}) {
    if (!repository) throw new Error('GraphGovernor repository is required');
    this.repository = repository;
    this.analyzer = analyzer;
    this.curator = curator ?? null;
  }

  analyze(options = {}) {
    return this.analyzer.analyze(this.repository.graph().snapshot(), options);
  }

  async propose(options = {}) {
    const graph = this.repository.graph().snapshot();
    const analysis = this.analyzer.analyze(graph, options);
    if (!this.curator?.propose) {
      return {
        analysis,
        plan: deterministicSafePlan(analysis)
      };
    }
    const plan = await this.curator.propose({ graph, analysis, options });
    return { analysis, plan };
  }

  applySafe(plan, options = {}) {
    const graph = this.repository.graph();
    const snapshot = graph.snapshot();
    const validation = validateGraphGovernorPlan(plan, snapshot);
    if (!validation.valid) {
      const error = new Error(`Invalid Graph Governor plan: ${validation.errors.join('; ')}`);
      error.validation = validation;
      throw error;
    }

    const changed = [];
    const tierByNode = new Map();
    for (const tier of ['hot', 'warm', 'cold']) {
      for (const id of validation.normalized.tiers[tier]) tierByNode.set(id, tier);
    }

    for (const [id, tier] of tierByNode) {
      const node = graph.requireNode(id);
      if (node.metadata?.storageTier === tier) continue;
      graph.updateNode(id, {
        metadata: {
          storageTier: tier,
          governorUpdatedAt: nowIso()
        }
      });
      changed.push({ nodeId: id, action: 'tier', tier });
    }

    for (const id of validation.normalized.archive) {
      const node = graph.requireNode(id);
      if (node.status === 'archived') continue;
      graph.updateNode(id, {
        status: 'archived',
        metadata: {
          archivedBy: 'graph-governor',
          governorUpdatedAt: nowIso()
        }
      });
      changed.push({ nodeId: id, action: 'archive' });
    }

    if (options.dryRun) {
      return { validation, changed, applied: false };
    }

    this.repository.writeGraph(graph.snapshot());
    let commit = null;
    if (options.commit !== false && changed.length) {
      try {
        commit = this.repository.commit(options.message ?? 'governor: apply safe graph maintenance', {
          metadata: {
            governor: true,
            changedCount: changed.length,
            epoch: validation.normalized.epoch ?? null
          }
        });
      } catch (error) {
        if (!String(error.message).includes('Nothing to commit')) throw error;
      }
    }

    return { validation, changed, applied: true, commit };
  }
}

function deterministicSafePlan(analysis) {
  return {
    archive: analysis.candidates.archive.map((item) => item.nodeId),
    tiers: structuredClone(analysis.tiers),
    canonicalize: [],
    branch: [],
    promote: [],
    epoch: analysis.epoch.recommended ? {
      proposed: true,
      reasons: analysis.epoch.reasons
    } : null
  };
}

function nodeValue(node, { degree, maxDegree }) {
  const grade = GRADE_WEIGHTS[node.grade] ?? 0.45;
  const trust = TRUST_WEIGHTS[node.trustZone] ?? 0.5;
  const lifecycle = STATUS_WEIGHT[node.status] ?? 0.5;
  const centrality = Math.log2(degree + 1) / Math.log2(maxDegree + 1);
  const activation = clamp01(Number(node.metadata?.activationFrequency ?? node.metadata?.retrievalContribution ?? 0));
  return clamp01(
    0.28 * grade +
    0.2 * trust +
    0.24 * lifecycle +
    0.18 * centrality +
    0.1 * activation
  );
}

function archiveCandidate(node, value, degree, threshold) {
  if (!['stale', 'dormant'].includes(node.status)) return false;
  if (node.kind === 'task' || node.kind === 'abstraction') return false;
  if (node.kind === 'evidence' && ['runtime', 'reproduced'].includes(node.grade)) return false;
  if (degree > 1) return false;
  return value < threshold;
}

function duplicateTitleGroups(nodes) {
  const groups = new Map();
  for (const node of nodes) {
    if (node.status === 'invalid') continue;
    const title = normalizeTitle(node.title);
    if (!title) continue;
    const key = `${node.kind}:${title}`;
    const list = groups.get(key) ?? [];
    list.push(node);
    groups.set(key, list);
  }
  return [...groups.values()]
    .filter((group) => group.length > 1)
    .map((group) => ({
      kind: group[0].kind,
      normalizedTitle: normalizeTitle(group[0].title),
      nodeIds: group.map((node) => node.id),
      titles: group.map((node) => node.title)
    }));
}

function promotionGroups(nodes, minGroup) {
  const tags = new Map();
  for (const node of nodes) {
    if (node.status !== 'active' || node.kind === 'abstraction' || node.kind === 'task') continue;
    for (const tag of node.tags ?? []) {
      const normalized = String(tag).trim().toLowerCase();
      if (!normalized || ['agent-session', 'tool-observation'].includes(normalized)) continue;
      const list = tags.get(normalized) ?? [];
      list.push(node.id);
      tags.set(normalized, list);
    }
  }
  return [...tags.entries()]
    .filter(([, ids]) => ids.length >= minGroup)
    .sort((a, b) => b[1].length - a[1].length)
    .map(([tag, nodeIds]) => ({ tag, nodeIds }));
}

function degreeMap(nodes, edges) {
  const map = new Map(nodes.map((node) => [node.id, 0]));
  for (const edge of edges) {
    map.set(edge.from, (map.get(edge.from) ?? 0) + 1);
    map.set(edge.to, (map.get(edge.to) ?? 0) + 1);
  }
  return map;
}

function normalizeTitle(title) {
  return String(title ?? '')
    .toLowerCase()
    .replace(/[^a-z0-9\u4e00-\u9fff]+/g, ' ')
    .trim()
    .replace(/\s+/g, ' ');
}

function uniqueStrings(values) {
  return [...new Set((values ?? []).filter((value) => typeof value === 'string' && value))];
}

function clamp01(value) {
  return Math.max(0, Math.min(1, Number(value) || 0));
}
