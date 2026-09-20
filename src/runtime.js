import { AttentionEngine } from './attention.js';
import { CognitiveGraph } from './graph.js';
import { promoteNodes } from './promotion.js';
import { auditEvidence, validateBeliefEvidence } from './verification.js';
import { id, nowIso } from './util.js';
import { PersistentSearchIndex } from './search-index.js';

export class LumenCortexRuntime {
  constructor(repository) {
    this.repository = repository;
    this.searchIndex = new PersistentSearchIndex(repository.dir);
  }

  refreshSearchIndex(graphState = this.repository.graph().snapshot()) {
    return this.searchIndex.build(graphState, { graphRevision: this.repository.graphRevision() });
  }

  search(query, options = {}) {
    this.#ensureFreshSearchIndex();
    return this.searchIndex.search(query, options);
  }

  context(goal, options = {}) {
    const graph = this.repository.graph().snapshot();
    const indexed = this.#candidateIds(goal, options);
    return new AttentionEngine(graph).illuminate(goal, {
      ...options,
      candidateNodeIds: indexed.length ? indexed : options.candidateNodeIds
    });
  }

  contextMulti(goal, options = {}) {
    const graph = this.repository.graph().snapshot();
    const indexed = this.#candidateIds(goal, options);
    return new AttentionEngine(graph).illuminateMulti(goal, {
      ...options,
      candidateNodeIds: indexed.length ? indexed : options.candidateNodeIds
    });
  }

  promote(nodeIds, options = {}) {
    const current = this.repository.graph().snapshot();
    const result = promoteNodes(current, nodeIds, options);
    this.repository.writeGraph(result.graph);
    this.refreshSearchIndex(result.graph);
    this.#journal('promote', { abstractionId: result.abstraction.id, nodeIds });
    return result.abstraction;
  }

  verify() {
    const current = this.repository.graph().snapshot();
    const audit = auditEvidence(current);
    this.repository.writeGraph(audit.graph);
    const issues = validateBeliefEvidence(audit.graph);
    this.#journal('verify', { staleEvidence: audit.staleEvidence, dirtiedBeliefs: audit.dirtiedBeliefs, issues });
    return { ...audit, issues };
  }

  async execute(goal, worker, options = {}) {
    if (!worker || typeof worker.reason !== 'function') {
      throw new Error('Worker must implement async reason(input)');
    }
    const before = this.repository.graph().snapshot();
    const graph = new CognitiveGraph(before);
    const taskId = id('task');
    graph.addNode({
      id: taskId,
      kind: 'task',
      title: options.title ?? goal.slice(0, 120),
      body: goal,
      tags: ['runtime-task'],
      trustZone: 'user_provided',
      grade: 'hypothesis',
      metadata: { state: 'running', startedAt: nowIso() }
    });

    const preliminary = graph.snapshot();
    const active = new AttentionEngine(preliminary).illuminate(goal, {
      budgetTokens: options.budgetTokens,
      maxHops: options.maxHops,
      seedNodeIds: [taskId, ...(options.seedNodeIds ?? [])]
    });

    this.#journal('execute.prepare', { taskId, goal, selected: active.selectedNodes.map((n) => n.id) });
    const output = await worker.reason({
      goal,
      taskId,
      headCommit: this.repository.headCommitId(),
      activeSubgraph: {
        nodes: active.selectedNodes,
        edges: active.selectedEdges,
        usedTokens: active.usedTokens,
        budgetTokens: active.budgetTokens,
        trace: active.trace
      }
    });

    try {
      applyWorkerOperations(graph, output?.operations ?? []);
      graph.updateNode(taskId, {
        body: output?.summary ? `${goal}\n\nResult:\n${output.summary}` : goal,
        unresolved: output?.unresolved ?? [],
        metadata: {
          state: 'completed',
          startedAt: graph.requireNode(taskId).metadata.startedAt,
          completedAt: nowIso(),
          operationCount: output?.operations?.length ?? 0
        }
      });
      enforceSemanticRules(graph.snapshot());
      this.repository.writeGraph(graph.snapshot());
      const commit = options.autoCommit === false
        ? null
        : this.repository.commit(options.message ?? `cognition: ${goal.slice(0, 72)}`, {
            metadata: { taskId, runtime: true }
          });
      this.#journal('execute.commit', { taskId, commitId: commit?.id ?? null });
      return { taskId, commit, output, active };
    } catch (error) {
      this.repository.writeGraph(before);
      this.#journal('execute.rollback', { taskId, error: error.message });
      throw error;
    }
  }

  #candidateIds(goal, options) {
    const explicit = options.candidateNodeIds ?? [];
    let indexed = [];
    try {
      this.#ensureFreshSearchIndex();
      indexed = this.searchIndex.search(goal, { limit: Number(options.candidateLimit ?? 64) }).map((hit) => hit.nodeId);
    } catch {
      indexed = [];
    }
    return [...new Set([...explicit, ...indexed])];
  }

  #ensureFreshSearchIndex() {
    const revision = this.repository.graphRevision();
    if (!this.searchIndex.ready() || this.searchIndex.state?.graphRevision !== revision) {
      this.refreshSearchIndex();
    }
  }

  #journal(event, payload) {
    if (!this.repository.exists()) return;
    this.repository.appendJournal(event, payload);
  }
}

export function applyWorkerOperations(graph, operations) {
  for (const op of operations) {
    switch (op.type) {
      case 'add_node':
        graph.addNode(op.node);
        break;
      case 'update_node':
        graph.updateNode(op.id, op.patch ?? {});
        break;
      case 'remove_node':
        graph.removeNode(op.id);
        break;
      case 'add_edge':
        graph.addEdge(op.edge);
        break;
      case 'remove_edge':
        graph.removeEdge(op.id);
        break;
      default:
        throw new Error(`Unsupported worker operation: ${op.type}`);
    }
  }
  return graph;
}

export function enforceSemanticRules(graphState) {
  new CognitiveGraph(graphState).validate();
  const evidenceIssues = validateBeliefEvidence(graphState);
  if (evidenceIssues.length) throw new Error(`Evidence integrity failed: ${JSON.stringify(evidenceIssues)}`);

  for (const node of Object.values(graphState.nodes ?? {})) {
    if (['belief', 'negative'].includes(node.kind) && node.grade !== 'hypothesis' && !(node.evidenceIds?.length)) {
      throw new Error(`Non-hypothesis ${node.kind} ${node.id} must cite evidence`);
    }
    if (node.kind === 'evidence' && node.trustZone === 'model_inferred') {
      throw new Error(`Evidence node ${node.id} cannot use model_inferred trust zone`);
    }
  }
  return true;
}

