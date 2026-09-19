import {
  DEFAULT_EDGE_WEIGHTS,
  GRADE_WEIGHTS,
  TRUST_WEIGHTS
} from './constants.js';
import { clamp, estimateTokens, lexicalScore } from './util.js';

const DEFAULTS = {
  budgetTokens: 32_000,
  maxHops: 4,
  minScore: 0.08,
  seedLimit: 8,
  decay: 0.72,
  incomingPenalty: 0.88,
  stalePenalty: 0.45,
  archivedPenalty: 0.2,
  dormantPenalty: 0.75,
  costPenalty: 0.08,
  edgeWeights: DEFAULT_EDGE_WEIGHTS
};

export class AttentionEngine {
  constructor(graphState) {
    this.graph = graphState;
    this.adjacency = buildAdjacency(graphState);
  }

  illuminate(goal, options = {}) {
    const cfg = mergeConfig(options);
    const candidates = scoreSeeds(this.graph, goal, cfg);
    const explicitSeeds = (options.seedNodeIds ?? [])
      .map((id) => this.graph.nodes[id])
      .filter(Boolean)
      .map((node) => ({ node, score: 1, reason: 'explicit-seed' }));
    const seeds = dedupeSeedEntries([...explicitSeeds, ...candidates]).slice(0, cfg.seedLimit);

    const queue = seeds.map((entry) => ({
      nodeId: entry.node.id,
      score: entry.score,
      hop: 0,
      parent: null,
      edge: null,
      reason: entry.reason
    }));
    const best = new Map();
    const trace = [];

    while (queue.length) {
      queue.sort((a, b) => b.score - a.score);
      const current = queue.shift();
      const previous = best.get(current.nodeId);
      if (previous && previous.score >= current.score) continue;
      if (current.score < cfg.minScore) continue;
      best.set(current.nodeId, current);
      trace.push({ ...current });
      if (current.hop >= cfg.maxHops) continue;

      for (const link of this.adjacency.get(current.nodeId) ?? []) {
        const target = this.graph.nodes[link.nodeId];
        if (!target) continue;
        const edgeWeight = cfg.edgeWeights[link.edge.type] ?? 0.35;
        const directionWeight = link.direction === 'out' ? 1 : cfg.incomingPenalty;
        const reliability = attentionReliability(target, cfg);
        const relevance = 0.5 + 0.5 * lexicalScore(goal, nodeText(target));
        const propagated = current.score
          * edgeWeight
          * directionWeight
          * cfg.decay
          * reliability
          * relevance;
        if (propagated < cfg.minScore) continue;
        queue.push({
          nodeId: target.id,
          score: propagated,
          hop: current.hop + 1,
          parent: current.nodeId,
          edge: link.edge.id,
          reason: `${link.direction}:${link.edge.type}`
        });
      }
    }

    const ranked = [...best.values()]
      .map((entry) => {
        const node = this.graph.nodes[entry.nodeId];
        const tokenCost = estimateNodeTokens(node);
        const utility = entry.score / (1 + cfg.costPenalty * Math.log2(tokenCost + 1));
        return { ...entry, tokenCost, utility, node };
      })
      .sort((a, b) => b.utility - a.utility || b.score - a.score);

    const selected = [];
    let used = 0;
    for (const item of ranked) {
      if (used + item.tokenCost > cfg.budgetTokens) continue;
      selected.push(item);
      used += item.tokenCost;
    }

    const selectedIds = new Set(selected.map((x) => x.nodeId));
    const edges = Object.values(this.graph.edges ?? {}).filter(
      (edge) => selectedIds.has(edge.from) && selectedIds.has(edge.to)
    );

    return {
      goal,
      budgetTokens: cfg.budgetTokens,
      usedTokens: used,
      selectedNodes: selected.map((x) => ({ ...x.node, activation: x.score, tokenCost: x.tokenCost })),
      selectedEdges: edges,
      omittedNodeIds: ranked.filter((x) => !selectedIds.has(x.nodeId)).map((x) => x.nodeId),
      trace,
      seeds: seeds.map((x) => ({ nodeId: x.node.id, score: x.score, reason: x.reason }))
    };
  }

  illuminateMulti(goal, options = {}) {
    const exploit = this.illuminate(goal, options);
    const exploitIds = new Set(exploit.selectedNodes.map((n) => n.id));

    const exploreCandidates = Object.values(this.graph.nodes ?? {})
      .filter((n) => !exploitIds.has(n.id) && n.status !== 'archived')
      .map((node) => ({ node, score: lexicalScore(goal, nodeText(node)) * attentionReliability(node, mergeConfig(options)) }))
      .sort((a, b) => b.score - a.score)
      .slice(0, 3)
      .map((x) => x.node.id);

    const contrarianSeeds = [];
    for (const edge of exploit.selectedEdges) {
      if (edge.type === 'contradicts' || edge.type === 'invalidates') {
        contrarianSeeds.push(edge.from, edge.to);
      }
    }
    for (const edge of Object.values(this.graph.edges ?? {})) {
      if ((edge.type === 'contradicts' || edge.type === 'invalidates') &&
          (exploitIds.has(edge.from) || exploitIds.has(edge.to))) {
        contrarianSeeds.push(edge.from, edge.to);
      }
    }

    const anomalySeeds = Object.values(this.graph.nodes ?? {})
      .filter((n) => n.tags?.includes('anomaly') || n.status === 'stale')
      .slice(0, 5)
      .map((n) => n.id);

    return {
      exploit,
      explore: this.illuminate(goal, {
        ...options,
        seedNodeIds: exploreCandidates,
        maxHops: (options.maxHops ?? DEFAULTS.maxHops) + 1,
        decay: Math.min(0.86, (options.decay ?? DEFAULTS.decay) + 0.08),
        edgeWeights: { ...DEFAULT_EDGE_WEIGHTS, relates_to: 0.75, affects: 0.9 }
      }),
      contrarian: this.illuminate(goal, {
        ...options,
        seedNodeIds: [...new Set(contrarianSeeds)].slice(0, 5),
        edgeWeights: { ...DEFAULT_EDGE_WEIGHTS, contradicts: 1, invalidates: 1, relates_to: 0.55 }
      }),
      anomaly: this.illuminate(goal, {
        ...options,
        seedNodeIds: anomalySeeds,
        edgeWeights: { ...DEFAULT_EDGE_WEIGHTS, affects: 0.95, causes: 1 }
      })
    };
  }
}

function scoreSeeds(graph, goal, cfg) {
  return Object.values(graph.nodes ?? {})
    .filter((node) => node.status !== 'archived' && node.status !== 'invalid')
    .map((node) => {
      const lexical = lexicalScore(goal, nodeText(node));
      const reliability = attentionReliability(node, cfg);
      const kindBoost = node.kind === 'task' ? 1.1 : node.kind === 'abstraction' ? 1.05 : 1;
      return { node, score: clamp(lexical * reliability * kindBoost, 0, 1), reason: 'lexical-seed' };
    })
    .filter((x) => x.score >= cfg.minScore)
    .sort((a, b) => b.score - a.score);
}

function buildAdjacency(graph) {
  const map = new Map();
  for (const edge of Object.values(graph.edges ?? {})) {
    if (!map.has(edge.from)) map.set(edge.from, []);
    if (!map.has(edge.to)) map.set(edge.to, []);
    map.get(edge.from).push({ edge, nodeId: edge.to, direction: 'out' });
    map.get(edge.to).push({ edge, nodeId: edge.from, direction: 'in' });
  }
  return map;
}


function attentionReliability(node, cfg) {
  // Trust/evidence quality influences attention, but must not hide a low-confidence
  // node that is lexically or causally important. Truth confidence and attention
  // are deliberately separate dimensions.
  const truth = nodeReliability(node, cfg);
  return 0.55 + 0.45 * truth;
}

function nodeReliability(node, cfg) {
  let weight = (GRADE_WEIGHTS[node.grade] ?? 0.5) * (TRUST_WEIGHTS[node.trustZone] ?? 0.6);
  if (node.status === 'stale') weight *= cfg.stalePenalty;
  if (node.status === 'archived') weight *= cfg.archivedPenalty;
  if (node.status === 'dormant') weight *= cfg.dormantPenalty;
  if (node.status === 'invalid') return 0.01;
  if (isExpired(node)) weight *= cfg.stalePenalty;
  return clamp(weight, 0.01, 1);
}

function isExpired(node) {
  if (!node.observedAt || node.ttlMs === undefined || node.ttlMs === null) return false;
  return Date.now() - new Date(node.observedAt).getTime() > node.ttlMs;
}

function nodeText(node) {
  return [node.title, node.body, ...(node.tags ?? []), node.source?.uri ?? '', JSON.stringify(node.metadata ?? {})].join(' ');
}

function estimateNodeTokens(node) {
  return estimateTokens({
    id: node.id,
    kind: node.kind,
    title: node.title,
    body: node.body,
    tags: node.tags,
    grade: node.grade,
    source: node.source,
    unresolved: node.unresolved,
    metadata: node.metadata
  });
}

function mergeConfig(options) {
  return {
    ...DEFAULTS,
    ...options,
    edgeWeights: { ...DEFAULTS.edgeWeights, ...(options.edgeWeights ?? {}) }
  };
}

function dedupeSeedEntries(entries) {
  const best = new Map();
  for (const entry of entries) {
    const previous = best.get(entry.node.id);
    if (!previous || previous.score < entry.score) best.set(entry.node.id, entry);
  }
  return [...best.values()].sort((a, b) => b.score - a.score);
}
