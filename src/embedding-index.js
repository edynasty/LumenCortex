import { LumenCortexDatabase } from './database.js';
import { searchableText } from './search-index.js';
import { hash } from './util.js';
import { PROVIDER_PRESETS } from './provider.js';

export class OpenAICompatibleEmbeddingProvider {
  constructor({
    baseURL,
    apiKey,
    model,
    headers = {},
    fetchImpl = globalThis.fetch,
    timeoutMs = 60000
  } = {}) {
    if (!baseURL) throw new Error('Embedding provider baseURL is required');
    if (!model) throw new Error('Embedding provider model is required');
    if (typeof fetchImpl !== 'function') throw new Error('Embedding fetch implementation is required');
    this.baseURL = String(baseURL).replace(/\/$/, '');
    this.apiKey = apiKey;
    this.model = model;
    this.headers = { ...headers };
    this.fetch = fetchImpl;
    this.timeoutMs = Math.max(1, Number(timeoutMs) || 60000);
  }

  async embed(input, { signal } = {}) {
    const values = Array.isArray(input) ? input : [input];
    if (!values.length) return [];
    const controller = signal ? null : new AbortController();
    const timeout = controller ? setTimeout(() => controller.abort(), this.timeoutMs) : null;
    try {
      const response = await this.fetch(`${this.baseURL}/embeddings`, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          ...(this.apiKey ? { Authorization: `Bearer ${this.apiKey}` } : {}),
          ...this.headers
        },
        body: JSON.stringify({
          model: this.model,
          input: values.map((value) => String(value ?? ''))
        }),
        signal: signal ?? controller.signal
      });
      const text = await response.text();
      let data;
      try { data = text ? JSON.parse(text) : {}; } catch { data = { raw: text }; }
      if (!response.ok) {
        const message = data?.error?.message ?? data?.message ?? text ?? `HTTP ${response.status}`;
        const error = new Error(`Embedding request failed (${response.status}): ${message}`);
        error.status = response.status;
        error.response = data;
        throw error;
      }
      const rows = Array.isArray(data?.data) ? [...data.data] : [];
      rows.sort((a, b) => Number(a.index ?? 0) - Number(b.index ?? 0));
      if (rows.length !== values.length) {
        throw new Error(`Embedding response count mismatch: expected ${values.length}, received ${rows.length}`);
      }
      return rows.map((row, index) => normalizeEmbeddingVector(row.embedding, `response[${index}]`));
    } finally {
      if (timeout) clearTimeout(timeout);
    }
  }
}

export class PersistentEmbeddingIndex {
  constructor(repositoryDir, {
    provider,
    model = provider?.model,
    batchSize = 32,
    database = null,
    ownsDatabase = database === null
  } = {}) {
    if (!provider || typeof provider.embed !== 'function') {
      throw new Error('Embedding provider with embed(input) is required');
    }
    if (!model) throw new Error('Embedding model is required');
    this.provider = provider;
    this.model = String(model);
    this.batchSize = Math.max(1, Math.floor(Number(batchSize) || 32));
    this.database = database ?? new LumenCortexDatabase(repositoryDir);
    this.ownsDatabase = ownsDatabase;
  }

  ready() {
    return this.database.embeddingStats(this.model).count > 0;
  }

  stats() {
    return this.database.embeddingStats(this.model);
  }

  async sync(graphState, { graphRevision = null, force = false, signal } = {}) {
    const desired = embeddingDocuments(graphState);
    let manifest = new Map(
      this.database.embeddingManifest(this.model)
        .map((item) => [item.nodeId, item])
    );

    let changed = [...desired.values()].filter((item) =>
      force ||
      !manifest.has(item.nodeId) ||
      manifest.get(item.nodeId).contentHash !== item.contentHash
    );
    const removeNodeIds = [...manifest.keys()].filter((nodeId) => !desired.has(nodeId));

    let upserts = await this.#embedDocuments(changed, signal);
    const existingDimensions = new Set(
      [...manifest.values()].map((item) => item.dimension).filter((value) => value > 0)
    );
    const newDimensions = new Set(upserts.map((item) => item.vector.length));

    if (newDimensions.size > 1) {
      throw embeddingDimensionError([...newDimensions], this.model);
    }
    const newDimension = [...newDimensions][0] ?? null;
    if (
      newDimension !== null &&
      existingDimensions.size &&
      (!existingDimensions.has(newDimension) || existingDimensions.size > 1)
    ) {
      this.database.clearEmbeddings(this.model);
      manifest = new Map();
      changed = [...desired.values()];
      upserts = await this.#embedDocuments(changed, signal);
      const rebuiltDimensions = new Set(upserts.map((item) => item.vector.length));
      if (rebuiltDimensions.size > 1) {
        throw embeddingDimensionError([...rebuiltDimensions], this.model);
      }
    }

    const stats = this.database.syncEmbeddings({
      model: this.model,
      upserts,
      removeNodeIds: manifest.size ? removeNodeIds : [],
      graphRevision
    });
    return {
      ...stats,
      embedded: upserts.length,
      reused: Math.max(0, desired.size - upserts.length),
      removed: manifest.size ? removeNodeIds.length : 0
    };
  }

  searchVector(vector, options = {}) {
    return this.database.searchEmbeddings(vector, {
      model: this.model,
      limit: options.limit ?? 50,
      minScore: options.minScore ?? 0
    }).map((hit) => ({
      ...hit,
      reasons: ['embedding-cosine']
    }));
  }

  async search(query, options = {}) {
    const [vector] = await this.provider.embed([String(query ?? '')], {
      signal: options.signal
    });
    return this.searchVector(vector, options);
  }

  async hybridSearch(query, {
    lexicalIndex,
    limit = 50,
    lexicalLimit = Math.max(50, Number(limit) * 2),
    semanticLimit = Math.max(50, Number(limit) * 2),
    semanticMinScore = 0,
    rrfK = 60,
    lexicalWeight = 1,
    semanticWeight = 1,
    signal
  } = {}) {
    if (!lexicalIndex || typeof lexicalIndex.search !== 'function') {
      throw new Error('hybridSearch requires a lexical index');
    }
    const lexical = lexicalIndex.search(query, { limit: lexicalLimit });
    const semantic = await this.search(query, {
      limit: semanticLimit,
      minScore: semanticMinScore,
      signal
    });
    return reciprocalRankFusion([
      { name: 'lexical', hits: lexical, weight: lexicalWeight },
      { name: 'semantic', hits: semantic, weight: semanticWeight }
    ], { k: rrfK, limit });
  }

  close() {
    if (this.ownsDatabase) this.database.close();
  }

  async #embedDocuments(documents, signal) {
    const out = [];
    for (let start = 0; start < documents.length; start += this.batchSize) {
      const batch = documents.slice(start, start + this.batchSize);
      const vectors = await this.provider.embed(
        batch.map((item) => item.text),
        { signal }
      );
      if (vectors.length !== batch.length) {
        throw new Error(`Embedding provider returned ${vectors.length} vectors for ${batch.length} documents`);
      }
      for (let i = 0; i < batch.length; i += 1) {
        out.push({
          nodeId: batch[i].nodeId,
          contentHash: batch[i].contentHash,
          vector: normalizeEmbeddingVector(vectors[i], batch[i].nodeId)
        });
      }
    }
    return out;
  }
}

export function reciprocalRankFusion(channels, { k = 60, limit = 50 } = {}) {
  const offset = Math.max(1, Number(k) || 60);
  const max = Math.max(1, Number(limit) || 50);
  const merged = new Map();

  for (const channel of channels ?? []) {
    const name = String(channel?.name ?? 'channel');
    const weight = Number.isFinite(Number(channel?.weight)) ? Number(channel.weight) : 1;
    for (let index = 0; index < (channel?.hits ?? []).length; index += 1) {
      const hit = channel.hits[index];
      const nodeId = hit?.nodeId ?? hit?.id;
      if (!nodeId) continue;
      const current = merged.get(nodeId) ?? {
        nodeId,
        score: 0,
        reasons: new Set(),
        channelRanks: {},
        channelScores: {},
        metadata: {}
      };
      const rank = index + 1;
      current.score += weight / (offset + rank);
      current.channelRanks[name] = rank;
      current.channelScores[name] = Number(hit.score ?? 0);
      current.reasons.add(`rrf:${name}`);
      for (const reason of hit.reasons ?? []) current.reasons.add(reason);
      current.metadata = {
        ...current.metadata,
        title: current.metadata.title || hit.title || '',
        path: current.metadata.path || hit.path || '',
        kind: current.metadata.kind || hit.kind || '',
        sourceKind: current.metadata.sourceKind ?? hit.sourceKind ?? null,
        contentHash: current.metadata.contentHash ?? hit.contentHash ?? null
      };
      merged.set(nodeId, current);
    }
  }

  return [...merged.values()]
    .sort((a, b) => b.score - a.score || a.nodeId.localeCompare(b.nodeId))
    .slice(0, max)
    .map((item) => ({
      nodeId: item.nodeId,
      id: item.nodeId,
      score: item.score,
      reasons: [...item.reasons],
      channelRanks: item.channelRanks,
      channelScores: item.channelScores,
      ...item.metadata
    }));
}

export function embeddingDocuments(graphState) {
  const documents = new Map();
  for (const node of Object.values(graphState?.nodes ?? {})) {
    if (node.status === 'archived' || node.status === 'invalid') continue;
    const text = searchableText(node).trim();
    if (!text) continue;
    documents.set(node.id, {
      nodeId: node.id,
      text,
      contentHash: hash(text)
    });
  }
  return documents;
}

function normalizeEmbeddingVector(vector, label = 'embedding') {
  if (!Array.isArray(vector) && !ArrayBuffer.isView(vector)) {
    throw new Error(`${label} must be a numeric vector`);
  }
  const values = Array.from(vector, Number);
  if (!values.length || values.some((value) => !Number.isFinite(value))) {
    throw new Error(`${label} must contain finite numeric values`);
  }
  return values;
}

function embeddingDimensionError(dimensions, model) {
  const error = new Error(
    `Embedding dimension mismatch for model ${model}: ${dimensions.join(', ')}`
  );
  error.code = 'EMBEDDING_DIMENSION_MISMATCH';
  return error;
}


export function createEmbeddingProviderFromConfig(config = {}, {
  env = process.env,
  fetchImpl
} = {}) {
  if (!config?.enabled) return null;
  const providerName = String(config.provider ?? 'generic');
  const preset = PROVIDER_PRESETS[providerName] ?? {};
  const model = String(config.model ?? '').trim();
  if (!model) throw new Error('Enabled embedding retrieval requires an explicit model');

  const baseURL = String(
    config.baseURL ??
    preset.baseURL ??
    env.LUMENCORTEX_EMBEDDING_BASE_URL ??
    env.LUMENCORTEX_BASE_URL ??
    ''
  ).trim();
  if (!baseURL) throw new Error('Enabled embedding retrieval requires a baseURL');

  const apiKeyEnv = String(config.apiKeyEnv ?? '').trim();
  const apiKey = config.apiKey ??
    (apiKeyEnv ? env[apiKeyEnv] : undefined) ??
    (providerName === 'generic' ? env.LUMENCORTEX_EMBEDDING_API_KEY : undefined) ??
    (preset.apiKeyEnv ? env[preset.apiKeyEnv] : undefined) ??
    (providerName === 'generic' ? env.LUMENCORTEX_API_KEY : undefined);

  return new OpenAICompatibleEmbeddingProvider({
    baseURL,
    apiKey,
    model,
    headers: {
      ...(preset.headers ?? {}),
      ...(config.headers ?? {})
    },
    fetchImpl,
    timeoutMs: config.timeoutMs ?? 60000
  });
}

export function embeddingRuntimeConfig(profile = {}, options = {}) {
  const config = profile?.retrieval?.embeddings;
  const provider = createEmbeddingProviderFromConfig(config, options);
  if (!provider) return null;
  return {
    provider,
    model: provider.model,
    batchSize: config.batchSize,
    hybrid: {
      candidateLimit: config.candidateLimit,
      lexicalLimit: config.lexicalLimit,
      semanticLimit: config.semanticLimit,
      semanticMinScore: config.semanticMinScore,
      rrfK: config.rrfK,
      lexicalWeight: config.lexicalWeight,
      semanticWeight: config.semanticWeight
    }
  };
}
