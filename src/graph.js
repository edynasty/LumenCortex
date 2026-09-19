import { createEdge, createNode } from './model.js';
import { clone, isEqual, nowIso, stableStringify, uniq } from './util.js';

export function emptyGraph() {
  return { version: 1, nodes: {}, edges: {}, metadata: {} };
}

export class CognitiveGraph {
  constructor(state = emptyGraph()) {
    this.state = clone(state);
    this.validate();
  }

  snapshot() {
    return clone(this.state);
  }

  addNode(input) {
    const node = createNode(input);
    if (this.state.nodes[node.id]) throw new Error(`Node already exists: ${node.id}`);
    this.state.nodes[node.id] = node;
    return clone(node);
  }

  putNode(node) {
    const normalized = createNode(node);
    const previous = this.state.nodes[normalized.id];
    if (previous) {
      normalized.createdAt = previous.createdAt;
      normalized.updatedAt = nowIso();
      normalized.version = (previous.version ?? 1) + 1;
    }
    this.state.nodes[normalized.id] = normalized;
    this.validateNodeReferences(normalized);
    return clone(normalized);
  }

  updateNode(id, patch) {
    const current = this.requireNode(id);
    return this.putNode({ ...current, ...patch, id, metadata: { ...current.metadata, ...(patch.metadata ?? {}) } });
  }

  removeNode(id) {
    const current = this.requireNode(id);
    for (const edge of Object.values(this.state.edges)) {
      if (edge.from === id || edge.to === id) delete this.state.edges[edge.id];
    }
    // Preserve graph validity and provenance semantics when a referenced node is removed.
    for (const node of Object.values(this.state.nodes)) {
      if (node.id === id) continue;
      let changed = false;
      const patch = {};
      if ((node.evidenceIds ?? []).includes(id)) {
        patch.evidenceIds = node.evidenceIds.filter((ref) => ref !== id);
        changed = true;
      }
      if ((node.childIds ?? []).includes(id)) {
        patch.childIds = node.childIds.filter((ref) => ref !== id);
        changed = true;
      }
      if (changed) {
        patch.status = 'stale';
        patch.metadata = { ...(node.metadata ?? {}), staleReason: 'referenced-node-removed', removedNodeId: id };
        this.putNode({ ...node, ...patch });
      }
    }
    delete this.state.nodes[id];
    this.validate();
    return clone(current);
  }

  addEdge(input) {
    const edge = createEdge(input);
    if (this.state.edges[edge.id]) throw new Error(`Edge already exists: ${edge.id}`);
    this.requireNode(edge.from);
    this.requireNode(edge.to);
    this.state.edges[edge.id] = edge;
    return clone(edge);
  }

  putEdge(edge) {
    const normalized = createEdge(edge);
    this.requireNode(normalized.from);
    this.requireNode(normalized.to);
    this.state.edges[normalized.id] = normalized;
    return clone(normalized);
  }

  removeEdge(id) {
    const edge = this.state.edges[id];
    if (!edge) throw new Error(`Unknown edge: ${id}`);
    delete this.state.edges[id];
    return clone(edge);
  }

  getNode(id) {
    return this.state.nodes[id] ? clone(this.state.nodes[id]) : undefined;
  }

  requireNode(id) {
    const node = this.state.nodes[id];
    if (!node) throw new Error(`Unknown node: ${id}`);
    return clone(node);
  }

  getEdge(id) {
    return this.state.edges[id] ? clone(this.state.edges[id]) : undefined;
  }

  neighbors(id, { direction = 'both', types } = {}) {
    this.requireNode(id);
    const typeSet = types ? new Set(types) : null;
    const out = [];
    for (const edge of Object.values(this.state.edges)) {
      if (typeSet && !typeSet.has(edge.type)) continue;
      if ((direction === 'out' || direction === 'both') && edge.from === id) {
        out.push({ edge: clone(edge), node: this.requireNode(edge.to), direction: 'out' });
      }
      if ((direction === 'in' || direction === 'both') && edge.to === id) {
        out.push({ edge: clone(edge), node: this.requireNode(edge.from), direction: 'in' });
      }
    }
    return out;
  }

  findNodes(predicate) {
    return Object.values(this.state.nodes).filter(predicate).map(clone);
  }

  validate() {
    for (const edge of Object.values(this.state.edges)) {
      if (!this.state.nodes[edge.from]) throw new Error(`Edge ${edge.id} references missing source node ${edge.from}`);
      if (!this.state.nodes[edge.to]) throw new Error(`Edge ${edge.id} references missing target node ${edge.to}`);
    }
    for (const node of Object.values(this.state.nodes)) this.validateNodeReferences(node);
    return true;
  }

  validateNodeReferences(node) {
    for (const evidenceId of node.evidenceIds ?? []) {
      if (!this.state.nodes[evidenceId]) throw new Error(`Node ${node.id} references missing evidence ${evidenceId}`);
    }
    for (const childId of node.childIds ?? []) {
      if (!this.state.nodes[childId]) throw new Error(`Node ${node.id} references missing child ${childId}`);
    }
  }
}

export function diffGraphs(before, after) {
  const operations = [];
  const beforeNodes = before.nodes ?? {};
  const afterNodes = after.nodes ?? {};
  for (const key of uniq([...Object.keys(beforeNodes), ...Object.keys(afterNodes)]).sort()) {
    const b = beforeNodes[key];
    const a = afterNodes[key];
    if (!b && a) operations.push({ type: 'put_node', id: key, before: null, after: clone(a) });
    else if (b && !a) operations.push({ type: 'remove_node', id: key, before: clone(b), after: null });
    else if (!isEqual(b, a)) operations.push({ type: 'put_node', id: key, before: clone(b), after: clone(a) });
  }

  const beforeEdges = before.edges ?? {};
  const afterEdges = after.edges ?? {};
  for (const key of uniq([...Object.keys(beforeEdges), ...Object.keys(afterEdges)]).sort()) {
    const b = beforeEdges[key];
    const a = afterEdges[key];
    if (!b && a) operations.push({ type: 'put_edge', id: key, before: null, after: clone(a) });
    else if (b && !a) operations.push({ type: 'remove_edge', id: key, before: clone(b), after: null });
    else if (!isEqual(b, a)) operations.push({ type: 'put_edge', id: key, before: clone(b), after: clone(a) });
  }
  return { operations };
}

export function applyDiff(state, diff, { strict = true } = {}) {
  const next = clone(state);
  for (const op of diff.operations ?? []) {
    const collection = op.type.endsWith('node') ? next.nodes : next.edges;
    const current = collection[op.id] ?? null;
    if (strict && !isEqual(current, op.before ?? null)) {
      throw new Error(`Diff precondition failed for ${op.type}:${op.id}`);
    }
    if (op.after === null) delete collection[op.id];
    else collection[op.id] = clone(op.after);
  }
  new CognitiveGraph(next).validate();
  return next;
}

export function invertDiff(diff) {
  return {
    operations: [...(diff.operations ?? [])].reverse().map((op) => ({
      ...op,
      before: clone(op.after),
      after: clone(op.before),
      type: op.after === null
        ? (op.type.endsWith('node') ? 'put_node' : 'put_edge')
        : op.before === null
          ? (op.type.endsWith('node') ? 'remove_node' : 'remove_edge')
          : op.type
    }))
  };
}

export function graphSummary(state) {
  const kinds = {};
  for (const node of Object.values(state.nodes ?? {})) kinds[node.kind] = (kinds[node.kind] ?? 0) + 1;
  return {
    nodes: Object.keys(state.nodes ?? {}).length,
    edges: Object.keys(state.edges ?? {}).length,
    kinds,
    fingerprint: stableStringify(state).length
  };
}
