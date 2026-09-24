import { GRADE_WEIGHTS, TRUST_WEIGHTS } from './constants.js';
import { hash, nowIso } from './util.js';

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

export class LLMGraphGovernorCurator {
  constructor({ provider, maxTokens = 6000, reasoningEffort = 'high' } = {}) {
    if (!provider || typeof provider.complete !== 'function') {
      throw new Error('LLMGraphGovernorCurator provider is required');
    }
    this.provider = provider;
    this.maxTokens = Math.max(512, Number(maxTokens));
    this.reasoningEffort = reasoningEffort;
  }

  async propose({ graph, analysis, options = {} } = {}) {
    const payload = buildGovernorCandidateContext(graph, analysis, options);
    const response = await this.provider.complete({
      messages: [
        {
          role: 'system',
          content: GRAPH_GOVERNOR_SYSTEM_PROMPT
        },
        {
          role: 'user',
          content: JSON.stringify(payload)
        }
      ],
      maxTokens: Number(options.maxTokens ?? this.maxTokens),
      reasoningEffort: options.reasoningEffort ?? this.reasoningEffort
    });
    const raw = parseGovernorJson(response?.message?.content ?? '');
    return normalizeCuratorPlan(raw, analysis);
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
    const canonical = item?.canonical ? nodes[item.canonical] : null;
    if (!canonical) {
      errors.push('canonicalize canonical node is missing or unknown');
      continue;
    }
    if (canonical.metadata?.canonicalNodeId) {
      errors.push(`canonical node is itself an alias: ${canonical.id}`);
    }

    const aliases = uniqueStrings(item?.aliases ?? []);
    if (!aliases.length) warnings.push(`canonicalize has no aliases: ${canonical.id}`);
    for (const aliasId of aliases) {
      const alias = nodes[aliasId];
      if (!alias) {
        errors.push(`canonicalize alias references unknown node: ${aliasId}`);
        continue;
      }
      if (aliasId === canonical.id) errors.push(`canonicalize alias equals canonical node: ${aliasId}`);
      if (alias.kind !== canonical.kind) {
        errors.push(`canonicalize kind mismatch: ${canonical.id}(${canonical.kind}) vs ${aliasId}(${alias.kind})`);
      }
      if (alias.metadata?.canonicalNodeId && alias.metadata.canonicalNodeId !== canonical.id) {
        errors.push(`alias already canonicalized elsewhere: ${aliasId} -> ${alias.metadata.canonicalNodeId}`);
      }
      if (
        ['evidence', 'belief', 'negative', 'abstraction'].includes(canonical.kind) &&
        (GRADE_WEIGHTS[alias.grade] ?? 0) > (GRADE_WEIGHTS[canonical.grade] ?? 0)
      ) {
        errors.push(`canonical node has lower evidence grade than alias: ${canonical.id} < ${aliasId}`);
      }
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
      epoch: plan?.epoch ?? null,
      summary: String(plan?.summary ?? '').slice(0, 4000)
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
    const validation = validateGraphGovernorPlan(plan, graph);
    return {
      analysis,
      plan: validation.normalized,
      validation,
      curator: {
        enabled: true,
        model: this.curator.provider?.model ?? null
      }
    };
  }

  applySafe(plan, options = {}) {
    return this.applyPlan(plan, {
      ...options,
      semantic: false,
      createEpoch: false
    });
  }

  applyPlan(plan, options = {}) {
    const graph = this.repository.graph();
    const snapshot = graph.snapshot();
    const validation = validateGraphGovernorPlan(plan, snapshot);
    if (!validation.valid) {
      const error = new Error(`Invalid Graph Governor plan: ${validation.errors.join('; ')}`);
      error.validation = validation;
      throw error;
    }

    const beforeAnalysis = this.analyzer.analyze(snapshot);
    const changed = [];
    const deferred = {
      branch: validation.normalized.branch,
      promote: validation.normalized.promote
    };
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

    if (options.semantic) {
      for (const item of validation.normalized.canonicalize) {
        const canonical = graph.requireNode(item.canonical);
        for (const aliasId of uniqueStrings(item.aliases ?? [])) {
          const alias = graph.requireNode(aliasId);
          if (alias.metadata?.canonicalNodeId !== canonical.id) {
            graph.updateNode(aliasId, {
              metadata: {
                canonicalNodeId: canonical.id,
                canonicalizedBy: 'graph-governor',
                canonicalizedAt: nowIso(),
                canonicalizationReason: String(item.reason ?? '').slice(0, 1000)
              }
            });
            changed.push({
              nodeId: aliasId,
              action: 'canonicalize',
              canonicalNodeId: canonical.id
            });
          }

          const edgeId = `edge_${hash(`canonicalizes:${canonical.id}:${aliasId}`).slice(0, 16)}`;
          if (!graph.getEdge(edgeId)) {
            graph.addEdge({
              id: edgeId,
              from: canonical.id,
              to: aliasId,
              type: 'canonicalizes',
              weight: 1,
              metadata: {
                governor: true,
                reason: String(item.reason ?? '').slice(0, 1000)
              }
            });
            changed.push({
              edgeId,
              action: 'canonicalizes-edge',
              from: canonical.id,
              to: aliasId
            });
          }
        }
      }
    } else if (validation.normalized.canonicalize.length) {
      deferred.canonicalize = validation.normalized.canonicalize;
    }

    const afterGovernanceSnapshot = graph.snapshot();
    const afterAnalysis = this.analyzer.analyze(afterGovernanceSnapshot);
    let epoch = null;

    if (options.createEpoch) {
      const sourceCommit = typeof this.repository.headCommitId === 'function'
        ? this.repository.headCommitId()
        : null;
      const epochId = options.epochId ?? `epoch_${hash({
        sourceCommit,
        changed,
        summary: validation.normalized.summary,
        reasons: validation.normalized.epoch?.reasons ?? []
      }).slice(0, 16)}`;
      const childIds = uniqueStrings(
        changed
          .map((item) => item.nodeId)
          .filter((id) => id && afterGovernanceSnapshot.nodes?.[id])
      ).slice(0, 128);

      graph.addNode({
        id: epochId,
        kind: 'abstraction',
        title: options.epochTitle ?? `Cortex Epoch ${nowIso()}`,
        body: validation.normalized.summary || 'Graph Governor maintenance epoch',
        tags: ['cortex-epoch', 'graph-governor'],
        trustZone: 'system_verified',
        grade: 'static',
        childIds,
        metadata: {
          cortexEpoch: true,
          sourceCommit,
          rollbackTarget: sourceCommit,
          reasons: validation.normalized.epoch?.reasons ?? [],
          metricsBefore: beforeAnalysis.metrics,
          metricsAfter: afterAnalysis.metrics,
          semantic: Boolean(options.semantic),
          changedCount: changed.length
        }
      });
      changed.push({ nodeId: epochId, action: 'cortex-epoch' });
      epoch = {
        id: epochId,
        sourceCommit,
        rollbackTarget: sourceCommit,
        reasons: validation.normalized.epoch?.reasons ?? [],
        metricsBefore: beforeAnalysis.metrics,
        metricsAfter: afterAnalysis.metrics
      };
    }

    if (options.dryRun) {
      return {
        validation,
        changed,
        deferred,
        epoch,
        applied: false
      };
    }

    this.repository.writeGraph(graph.snapshot());
    let commit = null;
    if (options.commit !== false && changed.length) {
      try {
        commit = this.repository.commit(
          options.message ?? (epoch ? `governor: cortex epoch ${epoch.id}` : 'governor: apply graph maintenance'),
          {
            metadata: {
              governor: true,
              semantic: Boolean(options.semantic),
              changedCount: changed.length,
              epoch,
              planSummary: validation.normalized.summary
            }
          }
        );
      } catch (error) {
        if (!String(error.message).includes('Nothing to commit')) throw error;
      }
    }

    return {
      validation,
      changed,
      deferred,
      epoch,
      applied: true,
      commit
    };
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


const GRAPH_GOVERNOR_SYSTEM_PROMPT = [
  'You are the semantic Curator for LumenCortex Graph Governor.',
  'You do not mutate the graph. You only propose a GraphMutationPlan over the candidate IDs provided.',
  'Return one JSON object only. Do not use markdown fences.',
  'Preserve provenance. Prefer archive/tiering over deletion. Never invent node IDs.',
  'Canonicalization means choosing a canonical node and alias nodes without deleting provenance.',
  'Branch means preserving competing hypotheses, not choosing a winner.',
  'Promotion means proposing an abstraction over existing children.',
  'The deterministic Validator may reject any unsafe or invalid proposal.',
  'Schema:',
  JSON.stringify({
    archive: ['node-id'],
    tiers: { hot: ['node-id'], warm: ['node-id'], cold: ['node-id'] },
    canonicalize: [{ canonical: 'node-id', aliases: ['node-id'], reason: 'short reason' }],
    branch: [{ from: 'node-id', to: 'node-id', reason: 'short reason' }],
    promote: [{ title: 'abstraction title', childIds: ['node-id'], reason: 'short reason' }],
    epoch: { proposed: false, reasons: ['reason'] },
    summary: 'short plan summary'
  })
].join('\n');

function buildGovernorCandidateContext(graph, analysis, options = {}) {
  const nodes = graph?.nodes ?? {};
  const maxGroups = Math.max(1, Number(options.maxCandidateGroups ?? 64));
  const maxNodes = Math.max(10, Number(options.maxCandidateNodes ?? 256));
  const candidateIds = new Set();

  for (const item of analysis?.candidates?.archive ?? []) candidateIds.add(item.nodeId);
  for (const group of (analysis?.candidates?.canonicalize ?? []).slice(0, maxGroups)) {
    for (const id of group.nodeIds ?? []) candidateIds.add(id);
  }
  for (const item of (analysis?.candidates?.branch ?? []).slice(0, maxGroups)) {
    candidateIds.add(item.from);
    candidateIds.add(item.to);
  }
  for (const group of (analysis?.candidates?.promote ?? []).slice(0, maxGroups)) {
    for (const id of group.nodeIds ?? []) candidateIds.add(id);
  }
  for (const id of (analysis?.tiers?.hot ?? []).slice(0, 32)) candidateIds.add(id);

  const summaries = {};
  for (const id of [...candidateIds].slice(0, maxNodes)) {
    const node = nodes[id];
    if (!node) continue;
    summaries[id] = {
      id: node.id,
      kind: node.kind,
      title: node.title,
      status: node.status,
      grade: node.grade,
      trustZone: node.trustZone,
      tags: (node.tags ?? []).slice(0, 12),
      body: String(node.body ?? '').slice(0, 800),
      metadata: {
        path: node.metadata?.path ?? null,
        sourceKind: node.metadata?.sourceKind ?? null,
        storageTier: node.metadata?.storageTier ?? null
      }
    };
  }

  return {
    objective: 'Propose long-horizon graph maintenance. Do not execute changes.',
    metrics: analysis?.metrics ?? {},
    currentTiers: {
      hot: (analysis?.tiers?.hot ?? []).slice(0, maxNodes),
      warm: (analysis?.tiers?.warm ?? []).slice(0, maxNodes),
      cold: (analysis?.tiers?.cold ?? []).slice(0, maxNodes)
    },
    candidates: {
      archive: (analysis?.candidates?.archive ?? []).slice(0, maxGroups),
      canonicalize: (analysis?.candidates?.canonicalize ?? []).slice(0, maxGroups),
      branch: (analysis?.candidates?.branch ?? []).slice(0, maxGroups),
      promote: (analysis?.candidates?.promote ?? []).slice(0, maxGroups),
      epoch: analysis?.epoch ?? null
    },
    nodes: summaries
  };
}

function normalizeCuratorPlan(raw, analysis) {
  return {
    archive: uniqueStrings(raw?.archive ?? []),
    tiers: {
      hot: uniqueStrings(raw?.tiers?.hot ?? analysis?.tiers?.hot ?? []),
      warm: uniqueStrings(raw?.tiers?.warm ?? analysis?.tiers?.warm ?? []),
      cold: uniqueStrings(raw?.tiers?.cold ?? analysis?.tiers?.cold ?? [])
    },
    canonicalize: Array.isArray(raw?.canonicalize) ? raw.canonicalize : [],
    branch: Array.isArray(raw?.branch) ? raw.branch : [],
    promote: Array.isArray(raw?.promote) ? raw.promote : [],
    epoch: raw?.epoch ?? (
      analysis?.epoch?.recommended
        ? { proposed: true, reasons: analysis.epoch.reasons }
        : null
    ),
    summary: String(raw?.summary ?? '').slice(0, 4000)
  };
}

function parseGovernorJson(content) {
  const text = String(content ?? '').trim();
  if (!text) throw new Error('Graph Governor Curator returned empty output');

  const unfenced = text
    .replace(/^\s*```(?:json)?\s*/i, '')
    .replace(/\s*```\s*$/i, '')
    .trim();

  try {
    return JSON.parse(unfenced);
  } catch {}

  const start = unfenced.indexOf('{');
  const end = unfenced.lastIndexOf('}');
  if (start >= 0 && end > start) {
    try {
      return JSON.parse(unfenced.slice(start, end + 1));
    } catch {}
  }

  throw new Error('Graph Governor Curator returned invalid JSON');
}
