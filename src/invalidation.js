import { CognitiveGraph } from './graph.js';

const STALE_KINDS = new Set(['belief', 'negative', 'abstraction']);
const PROPAGATING_EDGE_TYPES = new Set(['derived_from', 'depends_on', 'abstracts']);

export function propagateStaleDependents(graph, sourceIds, options = {}) {
  if (!(graph instanceof CognitiveGraph)) throw new Error('propagateStaleDependents requires a CognitiveGraph');
  const roots = [...new Set((sourceIds ?? []).filter((id) => graph.state.nodes[id]))];
  if (!roots.length) return [];

  const reverse = buildReverseDependencyMap(graph.state);
  const queue = roots.map((id) => ({ nodeId: id, rootId: id, depth: 0 }));
  const seen = new Set(queue.map((entry) => `${entry.rootId}\0${entry.nodeId}`));
  const impacted = new Map();

  while (queue.length) {
    const current = queue.shift();
    for (const link of reverse.get(current.nodeId) ?? []) {
      const dependent = graph.state.nodes[link.nodeId];
      if (!dependent) continue;

      let impact = impacted.get(dependent.id);
      if (!impact) {
        impact = {
          roots: new Set(),
          via: new Set(),
          minDepth: current.depth + 1
        };
        impacted.set(dependent.id, impact);
      }
      impact.roots.add(current.rootId);
      impact.via.add(`${link.relation}:${current.nodeId}`);
      impact.minDepth = Math.min(impact.minDepth, current.depth + 1);

      const key = `${current.rootId}\0${dependent.id}`;
      if (seen.has(key)) continue;
      seen.add(key);
      queue.push({ nodeId: dependent.id, rootId: current.rootId, depth: current.depth + 1 });
    }
  }

  const dirtied = [];
  for (const [nodeId, impact] of impacted) {
    const node = graph.state.nodes[nodeId];
    if (!node || !STALE_KINDS.has(node.kind)) continue;
    if (node.status === 'archived' || node.status === 'invalid' || node.status === 'stale') continue;
    graph.updateNode(nodeId, {
      status: 'stale',
      metadata: {
        staleReason: options.reason ?? 'dependency-stale',
        staleSourceIds: [...impact.roots].sort(),
        staleDepth: impact.minDepth,
        staleVia: [...impact.via].sort().slice(0, 32),
        stalePropagation: true
      }
    });
    dirtied.push(nodeId);
  }
  return dirtied;
}

export function buildReverseDependencyMap(graphState) {
  const reverse = new Map();
  const add = (dependencyId, dependentId, relation) => {
    if (!dependencyId || !dependentId || dependencyId === dependentId) return;
    if (!graphState.nodes?.[dependencyId] || !graphState.nodes?.[dependentId]) return;
    const links = reverse.get(dependencyId) ?? [];
    if (!links.some((link) => link.nodeId === dependentId && link.relation === relation)) {
      links.push({ nodeId: dependentId, relation });
      reverse.set(dependencyId, links);
    }
  };

  for (const node of Object.values(graphState.nodes ?? {})) {
    for (const evidenceId of node.evidenceIds ?? []) add(evidenceId, node.id, 'evidenceIds');
    for (const childId of node.childIds ?? []) add(childId, node.id, 'childIds');
  }
  for (const edge of Object.values(graphState.edges ?? {})) {
    if (!PROPAGATING_EDGE_TYPES.has(edge.type)) continue;
    add(edge.to, edge.from, edge.type);
  }
  return reverse;
}
