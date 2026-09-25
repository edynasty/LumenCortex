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
  diversityLambda: 1,
  diversityCandidateLimit: 256,
  pprRestart: 0.2,
  pprIterations: 12,
  pprTolerance: 0.00001,
  associativeNodeLimit: 512,
  associativeMinScore: 0.001,
  edgeWeights: DEFAULT_EDGE_WEIGHTS
};

export class AttentionEngine {
  constructor(graphState) {
    this.graph = graphState;
    this.adjacency = buildAdjacency(graphState);
  }

  illuminate(goal, options = {}) {
    const cfg = mergeConfig(options);
    const candidates = scoreSeeds(this.graph, goal, cfg, options.candidateNodeIds);
    const explicitSeeds = (options.seedNodeIds ?? [])
      .map((id) => this.graph.nodes[id])
      .filter(Boolean)
      .map((node) => ({ node, score: 1, reason: 'explicit-seed' }));
    const seeds = dedupeSeedEntries([...explicitSeeds, ...candidates]).slice(0, cfg.seedLimit);

    const queue = new StableMaxPriorityQueue();
    for (const entry of seeds) {
      queue.push({
        nodeId: entry.node.id,
        score: entry.score,
        hop: 0,
        parent: null,
        edge: null,
        reason: entry.reason
      });
    }
    const best = new Map();
    const trace = [];

    while (queue.length) {
      const current = queue.pop();
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

    const { selected, used } = selectRankedWithinBudget(ranked, cfg);

    const selectedIds = new Set(selected.map((x) => x.nodeId));
    const edges = Object.values(this.graph.edges ?? {}).filter(
      (edge) => edgeParticipates(edge) && selectedIds.has(edge.from) && selectedIds.has(edge.to)
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

  illuminateAssociative(goal, options = {}) {
    const cfg = mergeConfig(options);
    const candidates = scoreSeeds(this.graph, goal, cfg, options.candidateNodeIds);
    const explicitSeeds = (options.seedNodeIds ?? [])
      .map((id) => this.graph.nodes[id])
      .filter(Boolean)
      .map((node) => ({ node, score: 1, reason: 'explicit-seed' }));
    const seeds = dedupeSeedEntries([...explicitSeeds, ...candidates]).slice(0, cfg.seedLimit);

    if (!seeds.length) {
      return {
        mode: 'associative',
        goal,
        budgetTokens: cfg.budgetTokens,
        usedTokens: 0,
        selectedNodes: [],
        selectedEdges: [],
        omittedNodeIds: [],
        trace: [],
        seeds: [],
        iterations: 0,
        neighborhoodNodeCount: 0
      };
    }

    const neighborhood = collectBoundedNeighborhood(
      this.graph,
      this.adjacency,
      seeds.map((entry) => entry.node.id),
      cfg.maxHops,
      cfg.associativeNodeLimit
    );
    const nodeIds = [...neighborhood.depth.keys()];
    const nodeSet = new Set(nodeIds);

    const seedWeights = new Map();
    let seedTotal = 0;
    for (const entry of seeds) {
      if (!nodeSet.has(entry.node.id)) continue;
      const weight = Math.max(cfg.associativeMinScore, Number(entry.score) || 0);
      seedWeights.set(entry.node.id, weight);
      seedTotal += weight;
    }
    const seedDistribution = new Map(
      [...seedWeights.entries()].map(([nodeId, weight]) => [nodeId, weight / seedTotal])
    );

    let ranks = new Map(seedDistribution);
    let iterations = 0;
    for (let iteration = 0; iteration < cfg.pprIterations; iteration += 1) {
      iterations = iteration + 1;
      const next = new Map();
      for (const [nodeId, probability] of seedDistribution) {
        next.set(nodeId, cfg.pprRestart * probability);
      }

      for (const nodeId of nodeIds) {
        const mass = ranks.get(nodeId) ?? 0;
        if (mass <= 0) continue;
        const weighted = [];
        let totalWeight = 0;
        for (const link of this.adjacency.get(nodeId) ?? []) {
          if (!nodeSet.has(link.nodeId)) continue;
          const target = this.graph.nodes[link.nodeId];
          if (!target) continue;
          const weight = associativeTransitionWeight(goal, target, link, cfg);
          if (weight <= 0) continue;
          weighted.push({ nodeId: link.nodeId, weight });
          totalWeight += weight;
        }

        const propagatedMass = (1 - cfg.pprRestart) * mass;
        if (totalWeight <= 0) {
          for (const [seedId, probability] of seedDistribution) {
            next.set(seedId, (next.get(seedId) ?? 0) + propagatedMass * probability);
          }
          continue;
        }
        for (const item of weighted) {
          next.set(
            item.nodeId,
            (next.get(item.nodeId) ?? 0) + propagatedMass * (item.weight / totalWeight)
          );
        }
      }

      let delta = 0;
      for (const nodeId of nodeIds) {
        delta += Math.abs((next.get(nodeId) ?? 0) - (ranks.get(nodeId) ?? 0));
      }
      ranks = next;
      if (delta <= cfg.pprTolerance) break;
    }

    const ranked = nodeIds
      .map((nodeId) => {
        const score = ranks.get(nodeId) ?? 0;
        const node = this.graph.nodes[nodeId];
        const tokenCost = estimateNodeTokens(node);
        const utility = score / (1 + cfg.costPenalty * Math.log2(tokenCost + 1));
        return {
          nodeId,
          score,
          hop: neighborhood.depth.get(nodeId) ?? 0,
          parent: null,
          edge: null,
          reason: 'associative-ppr',
          tokenCost,
          utility,
          node
        };
      })
      .filter((entry) => entry.score >= cfg.associativeMinScore)
      .sort((a, b) => b.utility - a.utility || b.score - a.score || a.nodeId.localeCompare(b.nodeId));

    const { selected, used } = selectRankedWithinBudget(ranked, cfg);

    const selectedIds = new Set(selected.map((item) => item.nodeId));
    const edges = Object.values(this.graph.edges ?? {}).filter(
      (edge) => edgeParticipates(edge) && selectedIds.has(edge.from) && selectedIds.has(edge.to)
    );
    const trace = ranked.map(({ nodeId, score, hop, parent, edge, reason }) => ({
      nodeId,
      score,
      hop,
      parent,
      edge,
      reason
    }));

    return {
      mode: 'associative',
      goal,
      budgetTokens: cfg.budgetTokens,
      usedTokens: used,
      selectedNodes: selected.map((item) => ({
        ...item.node,
        activation: item.score,
        tokenCost: item.tokenCost
      })),
      selectedEdges: edges,
      omittedNodeIds: ranked.filter((item) => !selectedIds.has(item.nodeId)).map((item) => item.nodeId),
      trace,
      seeds: seeds.map((entry) => ({
        nodeId: entry.node.id,
        score: entry.score,
        reason: entry.reason
      })),
      iterations,
      neighborhoodNodeCount: nodeIds.length
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
      if (edgeParticipates(edge) &&
          (edge.type === 'contradicts' || edge.type === 'invalidates') &&
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

function collectBoundedNeighborhood(graph, adjacency, seedNodeIds, maxHops, nodeLimit) {
  const depth = new Map();
  const queue = [];
  for (const nodeId of seedNodeIds) {
    if (!graph.nodes?.[nodeId] || depth.has(nodeId) || depth.size >= nodeLimit) continue;
    depth.set(nodeId, 0);
    queue.push(nodeId);
  }

  let index = 0;
  while (index < queue.length && depth.size < nodeLimit) {
    const nodeId = queue[index++];
    const currentDepth = depth.get(nodeId) ?? 0;
    if (currentDepth >= maxHops) continue;
    for (const link of adjacency.get(nodeId) ?? []) {
      if (depth.has(link.nodeId) || !graph.nodes?.[link.nodeId]) continue;
      depth.set(link.nodeId, currentDepth + 1);
      queue.push(link.nodeId);
      if (depth.size >= nodeLimit) break;
    }
  }
  return { depth };
}

function associativeTransitionWeight(goal, target, link, cfg) {
  const edgeWeight = cfg.edgeWeights[link.edge.type] ?? 0.35;
  const directionWeight = link.direction === 'out' ? 1 : cfg.incomingPenalty;
  const reliability = attentionReliability(target, cfg);
  const relevance = 0.5 + 0.5 * lexicalScore(goal, nodeText(target));
  return Math.max(0, edgeWeight * directionWeight * reliability * relevance);
}

function scoreSeeds(graph, goal, cfg, candidateNodeIds) {
  const source = candidateNodeIds?.length
    ? candidateNodeIds.map((id) => graph.nodes?.[id]).filter(Boolean)
    : Object.values(graph.nodes ?? {});
  return source
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
    if (!edgeParticipates(edge)) continue;
    if (!map.has(edge.from)) map.set(edge.from, []);
    if (!map.has(edge.to)) map.set(edge.to, []);
    map.get(edge.from).push({ edge, nodeId: edge.to, direction: 'out' });
    map.get(edge.to).push({ edge, nodeId: edge.from, direction: 'in' });
  }
  return map;
}


function edgeParticipates(edge) {
  return edge?.metadata?.attentionCut !== true;
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

function selectRankedWithinBudget(ranked, cfg) {
  const rawLambda = Number(cfg.diversityLambda ?? 1);
  const lambda = Number.isFinite(rawLambda) ? clamp(rawLambda, 0, 1) : 1;
  if (lambda >= 0.999999) {
    const selected = [];
    let used = 0;
    for (const item of ranked) {
      if (used + item.tokenCost > cfg.budgetTokens) continue;
      selected.push(item);
      used += item.tokenCost;
    }
    return { selected, used };
  }

  const rawLimit = Number(cfg.diversityCandidateLimit ?? 256);
  const limit = Number.isFinite(rawLimit) ? Math.max(1, Math.floor(rawLimit)) : 256;
  const pool = ranked.slice(0, limit);
  const overflow = ranked.slice(limit);
  const selected = [];
  const remaining = new Set(pool.map((_, index) => index));
  const maxUtility = Math.max(0.0000001, ...pool.map((item) => Number(item.utility ?? 0)));
  let used = 0;

  while (remaining.size) {
    let bestIndex = null;
    let bestMMR = -Infinity;
    for (const index of remaining) {
      const item = pool[index];
      if (used + item.tokenCost > cfg.budgetTokens) continue;
      const relevance = Number(item.utility ?? 0) / maxUtility;
      let redundancy = 0;
      for (const chosen of selected) {
        redundancy = Math.max(
          redundancy,
          lexicalScore(nodeText(item.node), nodeText(chosen.node))
        );
      }
      const mmr = lambda * relevance - (1 - lambda) * redundancy;
      const currentBest = bestIndex === null ? null : pool[bestIndex];
      if (
        mmr > bestMMR ||
        (mmr === bestMMR && compareRanked(item, currentBest) < 0)
      ) {
        bestIndex = index;
        bestMMR = mmr;
      }
    }
    if (bestIndex === null) break;
    const item = pool[bestIndex];
    remaining.delete(bestIndex);
    selected.push(item);
    used += item.tokenCost;
  }

  for (const item of overflow) {
    if (used + item.tokenCost > cfg.budgetTokens) continue;
    selected.push(item);
    used += item.tokenCost;
  }
  return { selected, used };
}

function compareRanked(left, right) {
  if (!right) return -1;
  if (left.utility !== right.utility) return right.utility - left.utility;
  if (left.score !== right.score) return right.score - left.score;
  return String(left.nodeId).localeCompare(String(right.nodeId));
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


class StableMaxPriorityQueue {
  constructor() {
    this.heap = [];
    this.sequence = 0;
  }

  get length() {
    return this.heap.length;
  }

  push(item) {
    const entry = { item, sequence: this.sequence++ };
    this.heap.push(entry);
    let index = this.heap.length - 1;
    while (index > 0) {
      const parent = Math.floor((index - 1) / 2);
      if (!priorityHigher(this.heap[index], this.heap[parent])) break;
      [this.heap[index], this.heap[parent]] = [this.heap[parent], this.heap[index]];
      index = parent;
    }
  }

  pop() {
    if (!this.heap.length) return undefined;
    const top = this.heap[0];
    const tail = this.heap.pop();
    if (this.heap.length && tail) {
      this.heap[0] = tail;
      let index = 0;
      while (true) {
        const left = index * 2 + 1;
        const right = left + 1;
        let next = index;
        if (left < this.heap.length && priorityHigher(this.heap[left], this.heap[next])) next = left;
        if (right < this.heap.length && priorityHigher(this.heap[right], this.heap[next])) next = right;
        if (next === index) break;
        [this.heap[index], this.heap[next]] = [this.heap[next], this.heap[index]];
        index = next;
      }
    }
    return top.item;
  }
}

function priorityHigher(left, right) {
  if (left.item.score !== right.item.score) return left.item.score > right.item.score;
  return left.sequence < right.sequence;
}
