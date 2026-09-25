import { AttentionEngine } from './attention.js';
import { CognitiveGraph } from './graph.js';
import { promoteNodes } from './promotion.js';
import { auditEvidence, validateBeliefEvidence } from './verification.js';
import { id, nowIso } from './util.js';
import { PersistentSearchIndex } from './search-index.js';
import { PersistentEmbeddingIndex } from './embedding-index.js';

export class LumenCortexRuntime {
  constructor(repository, options = {}) {
    this.repository = repository;
    this.searchIndex = new PersistentSearchIndex(repository.dir);
    this.embeddingIndex = null;
    if (options.embeddingProvider) {
      this.configureEmbeddings({
        provider: options.embeddingProvider,
        model: options.embeddingModel,
        batchSize: options.embeddingBatchSize
      });
    }
    this.attentionEngine = null;
    this.attentionEngineRevision = null;
    this.attentionEngineBuilds = 0;
  }

  refreshSearchIndex(graphState, graphRevision) {
    if (!graphState) {
      const snapshot = this.repository.graphSnapshot();
      graphState = snapshot.state;
      graphRevision = snapshot.revision;
    }
    return this.searchIndex.build(graphState, {
      graphRevision: graphRevision ?? this.repository.graphRevision()
    });
  }

  search(query, options = {}) {
    this.#ensureFreshSearchIndex();
    return this.searchIndex.search(query, options);
  }

  configureEmbeddings({ provider, model, batchSize } = {}) {
    if (!provider) {
      this.embeddingIndex = null;
      return null;
    }
    this.embeddingIndex = new PersistentEmbeddingIndex(this.repository.dir, {
      provider,
      model,
      batchSize,
      database: this.searchIndex.database,
      ownsDatabase: false
    });
    return this.embeddingIndex.stats();
  }

  async refreshEmbeddingIndex(graphState, graphRevision, options = {}) {
    if (!this.embeddingIndex) throw new Error('Embedding retrieval is not configured');
    if (!graphState) {
      const snapshot = this.repository.graphSnapshot();
      graphState = snapshot.state;
      graphRevision = snapshot.revision;
    }
    return this.embeddingIndex.sync(graphState, {
      graphRevision: graphRevision ?? this.repository.graphRevision(),
      force: options.force ?? false,
      signal: options.signal
    });
  }

  async semanticSearch(query, options = {}) {
    await this.#ensureFreshEmbeddingIndex(options.signal);
    return this.embeddingIndex.search(query, options);
  }

  async hybridSearch(query, options = {}) {
    this.#ensureFreshSearchIndex();
    await this.#ensureFreshEmbeddingIndex(options.signal);
    return this.embeddingIndex.hybridSearch(query, {
      lexicalIndex: this.searchIndex,
      limit: options.limit ?? 50,
      lexicalLimit: options.lexicalLimit,
      semanticLimit: options.semanticLimit,
      semanticMinScore: options.semanticMinScore,
      rrfK: options.rrfK,
      lexicalWeight: options.lexicalWeight,
      semanticWeight: options.semanticWeight,
      signal: options.signal
    });
  }

  async contextAsync(goal, options = {}) {
    const requestedMode = String(options.retrievalMode ?? options.mode ?? '').trim().toLowerCase();
    if (requestedMode === 'hybrid') return this.contextHybrid(goal, options);
    return this.context(goal, options);
  }

  async contextHybrid(goal, options = {}) {
    if (!this.embeddingIndex) {
      const fallback = this.context(goal, { ...options, retrievalMode: 'weighted' });
      return {
        ...fallback,
        requestedMode: 'hybrid',
        fallbackReason: 'embedding-provider-unavailable'
      };
    }

    for (let attempt = 0; attempt < 2; attempt += 1) {
      this.#ensureFreshSearchIndex();
      await this.#ensureFreshEmbeddingIndex(options.signal);
      const snapshot = this.repository.graphSnapshot();
      const searchRevision = Number(this.searchIndex.state?.graphRevision ?? -1);
      const embeddingRevision = Number(this.embeddingIndex.stats().graphRevision ?? -1);
      if (
        (searchRevision !== snapshot.revision || embeddingRevision !== snapshot.revision) &&
        attempt === 0
      ) continue;

      const hits = await this.embeddingIndex.hybridSearch(goal, {
        lexicalIndex: this.searchIndex,
        limit: Number(options.candidateLimit ?? 64),
        lexicalLimit: options.lexicalLimit,
        semanticLimit: options.semanticLimit,
        semanticMinScore: options.semanticMinScore,
        rrfK: options.rrfK,
        lexicalWeight: options.lexicalWeight,
        semanticWeight: options.semanticWeight,
        signal: options.signal
      });
      const candidateNodeIds = [...new Set([
        ...(options.candidateNodeIds ?? []),
        ...hits.map((hit) => hit.nodeId)
      ])];
      const result = this.#attentionFor(snapshot).illuminate(goal, {
        ...options,
        candidateNodeIds
      });
      this.repository.touchNodeAccess?.(result.selectedNodes.map((node) => node.id));
      return {
        ...result,
        mode: 'hybrid',
        hybrid: {
          model: this.embeddingIndex.model,
          candidateCount: hits.length,
          hits
        }
      };
    }
    throw new Error('Hybrid retrieval could not stabilize against the current graph revision');
  }

  context(goal, options = {}) {
    const snapshot = this.repository.graphSnapshot();
    const indexed = this.#candidateIds(goal, options);
    const attention = this.#attentionFor(snapshot);
    const retrievalMode = normalizeRetrievalMode(options.retrievalMode ?? options.mode);
    const request = retrievalRequest(retrievalMode, {
      ...options,
      candidateNodeIds: indexed.length ? indexed : options.candidateNodeIds
    });
    const result = retrievalMode === 'associative'
      ? attention.illuminateAssociative(goal, request)
      : attention.illuminate(goal, request);
    this.repository.touchNodeAccess?.(result.selectedNodes.map((node) => node.id));
    return {
      ...result,
      mode: result.mode ?? retrievalMode
    };
  }

  contextMulti(goal, options = {}) {
    const snapshot = this.repository.graphSnapshot();
    const indexed = this.#candidateIds(goal, options);
    const result = this.#attentionFor(snapshot).illuminateMulti(goal, {
      ...options,
      candidateNodeIds: indexed.length ? indexed : options.candidateNodeIds
    });
    const touched = new Set();
    for (const view of Object.values(result)) {
      for (const node of view?.selectedNodes ?? []) touched.add(node.id);
    }
    this.repository.touchNodeAccess?.([...touched]);
    return result;
  }

  promote(nodeIds, options = {}) {
    const current = this.repository.graph().snapshot();
    const result = promoteNodes(current, nodeIds, options);
    const write = this.repository.writeGraph(result.graph);
    if (this.searchIndex.ready()) {
      this.searchIndex.sync(result.graph, { graphRevision: write.revision });
    } else {
      this.searchIndex.build(result.graph, { graphRevision: write.revision });
    }
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

  attentionCacheStats() {
    return {
      cached: Boolean(this.attentionEngine),
      revision: this.attentionEngineRevision,
      builds: this.attentionEngineBuilds
    };
  }

  #attentionFor(snapshot) {
    if (
      !this.attentionEngine ||
      this.attentionEngineRevision !== snapshot.revision
    ) {
      this.attentionEngine = new AttentionEngine(snapshot.state);
      this.attentionEngineRevision = snapshot.revision;
      this.attentionEngineBuilds += 1;
    }
    return this.attentionEngine;
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

  async #ensureFreshEmbeddingIndex(signal) {
    if (!this.embeddingIndex) throw new Error('Embedding retrieval is not configured');
    for (let attempt = 0; attempt < 2; attempt += 1) {
      const snapshot = this.repository.graphSnapshot();
      const stats = this.embeddingIndex.stats();
      if (Number(stats.graphRevision ?? -1) === snapshot.revision) return;
      try {
        await this.embeddingIndex.sync(snapshot.state, {
          graphRevision: snapshot.revision,
          signal
        });
        return;
      } catch (error) {
        if (error.code === 'EMBEDDING_REVISION_CONFLICT' && attempt === 0) continue;
        throw error;
      }
    }
  }

  #ensureFreshSearchIndex() {
    for (let attempt = 0; attempt < 2; attempt += 1) {
      const snapshot = this.repository.graphSnapshot();
      if (!this.searchIndex.ready()) {
        try {
          this.searchIndex.build(snapshot.state, { graphRevision: snapshot.revision });
          return;
        } catch (error) {
          if (error.code === 'SEARCH_REVISION_CONFLICT' && attempt === 0) continue;
          throw error;
        }
      }
      if (this.searchIndex.state?.graphRevision === snapshot.revision) return;
      try {
        this.searchIndex.sync(snapshot.state, { graphRevision: snapshot.revision });
        return;
      } catch (error) {
        if (error.code === 'SEARCH_REVISION_CONFLICT' && attempt === 0) continue;
        throw error;
      }
    }
  }

  close() {
    this.attentionEngine = null;
    this.attentionEngineRevision = null;
    this.embeddingIndex?.close?.();
    this.embeddingIndex = null;
    this.searchIndex.close?.();
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



const RETRIEVAL_PROFILES = Object.freeze({
  weighted: Object.freeze({}),
  lexical: Object.freeze({}),
  dependency: Object.freeze({
    maxHops: 5,
    edgeWeights: Object.freeze({
      depends_on: 1,
      calls: 0.95,
      abstracts: 0.85,
      derived_from: 0.8,
      relates_to: 0.25
    })
  }),
  causal: Object.freeze({
    maxHops: 5,
    edgeWeights: Object.freeze({
      causes: 1,
      affects: 0.95,
      derived_from: 0.95,
      depends_on: 0.9,
      contradicts: 0.55,
      relates_to: 0.25
    })
  }),
  historical: Object.freeze({
    maxHops: 5,
    includeArchivedSeeds: true,
    archivedPenalty: 0.55,
    stalePenalty: 0.7,
    dormantPenalty: 0.85,
    edgeWeights: Object.freeze({
      supersedes: 1,
      invalidates: 0.95,
      derived_from: 0.9,
      contradicts: 0.8,
      relates_to: 0.35
    })
  }),
  associative: Object.freeze({})
});

function normalizeRetrievalMode(value) {
  const mode = String(value ?? 'weighted').trim().toLowerCase();
  return Object.hasOwn(RETRIEVAL_PROFILES, mode) ? mode : 'weighted';
}

function retrievalRequest(mode, options) {
  const profile = RETRIEVAL_PROFILES[mode] ?? RETRIEVAL_PROFILES.weighted;
  return {
    ...profile,
    ...options,
    edgeWeights: {
      ...(profile.edgeWeights ?? {}),
      ...(options.edgeWeights ?? {})
    }
  };
}
