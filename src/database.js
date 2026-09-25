import fs from 'node:fs';
import path from 'node:path';
import { DatabaseSync } from 'node:sqlite';

const SCHEMA_VERSION = 3;

export class LumenCortexDatabase {
  constructor(repositoryDir) {
    this.repositoryDir = path.resolve(repositoryDir);
    fs.mkdirSync(this.repositoryDir, { recursive: true });
    this.file = path.join(this.repositoryDir, 'lumencortex.db');
    this.db = new DatabaseSync(this.file);
    this.db.exec(`
      PRAGMA journal_mode = WAL;
      PRAGMA synchronous = NORMAL;
      PRAGMA foreign_keys = ON;
      PRAGMA busy_timeout = 5000;
    `);
    this.#schema();
  }

  #schema() {
    this.db.exec(`
      CREATE TABLE IF NOT EXISTS metadata (
        key TEXT PRIMARY KEY,
        value TEXT NOT NULL
      ) STRICT;

      CREATE TABLE IF NOT EXISTS repository_state (
        key TEXT PRIMARY KEY,
        value TEXT NOT NULL
      ) STRICT;

      CREATE TABLE IF NOT EXISTS graph_nodes (
        id TEXT PRIMARY KEY,
        kind TEXT NOT NULL,
        status TEXT NOT NULL,
        title TEXT NOT NULL,
        body TEXT NOT NULL,
        path TEXT,
        source_kind TEXT,
        updated_at TEXT,
        version INTEGER NOT NULL,
        json TEXT NOT NULL
      ) STRICT;

      CREATE INDEX IF NOT EXISTS idx_graph_nodes_kind_status
        ON graph_nodes(kind, status);
      CREATE INDEX IF NOT EXISTS idx_graph_nodes_path
        ON graph_nodes(path);

      CREATE TABLE IF NOT EXISTS graph_node_storage (
        node_id TEXT PRIMARY KEY,
        tier TEXT NOT NULL CHECK(tier IN ('hot', 'warm', 'cold')),
        last_access_at TEXT,
        last_tier_change_at TEXT NOT NULL,
        archived_at TEXT,
        compacted_at TEXT,
        access_count INTEGER NOT NULL DEFAULT 0,
        FOREIGN KEY(node_id) REFERENCES graph_nodes(id) ON DELETE CASCADE
      ) STRICT;

      CREATE INDEX IF NOT EXISTS idx_graph_node_storage_tier_access
        ON graph_node_storage(tier, last_access_at);
      CREATE INDEX IF NOT EXISTS idx_graph_node_storage_archive
        ON graph_node_storage(tier, archived_at);

      CREATE TABLE IF NOT EXISTS graph_edges (
        id TEXT PRIMARY KEY,
        from_id TEXT NOT NULL,
        to_id TEXT NOT NULL,
        type TEXT NOT NULL,
        weight REAL NOT NULL,
        json TEXT NOT NULL,
        FOREIGN KEY(from_id) REFERENCES graph_nodes(id) ON DELETE CASCADE,
        FOREIGN KEY(to_id) REFERENCES graph_nodes(id) ON DELETE CASCADE
      ) STRICT;

      CREATE INDEX IF NOT EXISTS idx_graph_edges_from ON graph_edges(from_id);
      CREATE INDEX IF NOT EXISTS idx_graph_edges_to ON graph_edges(to_id);
      CREATE INDEX IF NOT EXISTS idx_graph_edges_type ON graph_edges(type);

      CREATE TABLE IF NOT EXISTS cognitive_commits (
        id TEXT PRIMARY KEY,
        format_version INTEGER NOT NULL,
        message TEXT NOT NULL,
        parents_json TEXT NOT NULL,
        created_at TEXT NOT NULL,
        graph_hash TEXT NOT NULL,
        diff_json TEXT NOT NULL,
        metadata_json TEXT NOT NULL
      ) STRICT;

      CREATE INDEX IF NOT EXISTS idx_cognitive_commits_created
        ON cognitive_commits(created_at DESC);

      CREATE TABLE IF NOT EXISTS cognitive_checkpoints (
        commit_id TEXT PRIMARY KEY,
        snapshot_json TEXT NOT NULL,
        created_at TEXT NOT NULL,
        FOREIGN KEY(commit_id) REFERENCES cognitive_commits(id) ON DELETE CASCADE
      ) STRICT;

      CREATE TABLE IF NOT EXISTS cognitive_refs (
        name TEXT PRIMARY KEY,
        commit_id TEXT NOT NULL,
        FOREIGN KEY(commit_id) REFERENCES cognitive_commits(id)
      ) STRICT;

      CREATE TABLE IF NOT EXISTS sessions (
        id TEXT PRIMARY KEY,
        created_at TEXT NOT NULL,
        updated_at TEXT NOT NULL,
        status TEXT NOT NULL,
        provider TEXT,
        model TEXT,
        goal TEXT NOT NULL,
        metadata_json TEXT NOT NULL,
        final TEXT,
        usage_json TEXT,
        error_json TEXT
      ) STRICT;

      CREATE INDEX IF NOT EXISTS idx_sessions_updated
        ON sessions(updated_at DESC);

      CREATE TABLE IF NOT EXISTS session_messages (
        session_id TEXT NOT NULL,
        seq INTEGER NOT NULL,
        role TEXT NOT NULL,
        json TEXT NOT NULL,
        PRIMARY KEY(session_id, seq),
        FOREIGN KEY(session_id) REFERENCES sessions(id) ON DELETE CASCADE
      ) STRICT;

      CREATE TABLE IF NOT EXISTS agent_steps (
        session_id TEXT NOT NULL,
        step INTEGER NOT NULL,
        json TEXT NOT NULL,
        PRIMARY KEY(session_id, step),
        FOREIGN KEY(session_id) REFERENCES sessions(id) ON DELETE CASCADE
      ) STRICT;

      CREATE TABLE IF NOT EXISTS journal (
        seq INTEGER PRIMARY KEY AUTOINCREMENT,
        at TEXT NOT NULL,
        event TEXT NOT NULL,
        payload_json TEXT NOT NULL
      ) STRICT;

      CREATE INDEX IF NOT EXISTS idx_journal_event_at
        ON journal(event, at DESC);

      CREATE TABLE IF NOT EXISTS search_documents (
        node_id TEXT PRIMARY KEY,
        title TEXT NOT NULL,
        path TEXT,
        kind TEXT NOT NULL,
        source_kind TEXT,
        content_hash TEXT,
        length INTEGER NOT NULL
      ) STRICT;

      CREATE TABLE IF NOT EXISTS symbols (
        symbol TEXT NOT NULL,
        node_id TEXT NOT NULL,
        PRIMARY KEY(symbol, node_id),
        FOREIGN KEY(node_id) REFERENCES search_documents(node_id) ON DELETE CASCADE
      ) WITHOUT ROWID;

      CREATE INDEX IF NOT EXISTS idx_symbols_node ON symbols(node_id);

      CREATE TABLE IF NOT EXISTS search_dirty_nodes (
        node_id TEXT PRIMARY KEY,
        removed INTEGER NOT NULL DEFAULT 0
      ) STRICT;

      CREATE TABLE IF NOT EXISTS node_embeddings (
        node_id TEXT NOT NULL,
        model TEXT NOT NULL,
        dimension INTEGER NOT NULL,
        content_hash TEXT NOT NULL,
        vector_json TEXT NOT NULL,
        updated_at TEXT NOT NULL,
        PRIMARY KEY(node_id, model),
        FOREIGN KEY(node_id) REFERENCES graph_nodes(id) ON DELETE CASCADE
      ) STRICT;

      CREATE INDEX IF NOT EXISTS idx_node_embeddings_model
        ON node_embeddings(model, node_id);

      CREATE VIRTUAL TABLE IF NOT EXISTS node_fts USING fts5(
        node_id UNINDEXED,
        title,
        body,
        path,
        tags,
        tokenize = 'unicode61 remove_diacritics 2'
      );

      CREATE VIRTUAL TABLE IF NOT EXISTS node_fts_vocab
        USING fts5vocab(node_fts, 'row');
    `);

    this.#backfillNodeStorage();
    this.setMeta('schema_version', String(SCHEMA_VERSION));
  }

  #backfillNodeStorage() {
    const rows = this.db.prepare(`
      SELECT n.id, n.status, n.updated_at, n.json
      FROM graph_nodes n
      LEFT JOIN graph_node_storage s ON s.node_id = n.id
      WHERE s.node_id IS NULL
    `).all();
    if (!rows.length) return;

    const insert = this.db.prepare(`
      INSERT INTO graph_node_storage(
        node_id, tier, last_access_at, last_tier_change_at, archived_at, compacted_at, access_count
      ) VALUES(?, ?, NULL, ?, ?, NULL, 0)
    `);
    const now = new Date().toISOString();
    for (const row of rows) {
      const node = parseJson(row.json, {});
      const tier = storageTierForNode(node, 'warm');
      const changedAt = row.updated_at ?? now;
      insert.run(
        row.id,
        tier,
        changedAt,
        row.status === 'archived' ? changedAt : null
      );
    }
  }

  transaction(fn) {
    this.db.exec('BEGIN IMMEDIATE');
    try {
      const value = fn();
      this.db.exec('COMMIT');
      return value;
    } catch (error) {
      try { this.db.exec('ROLLBACK'); } catch {}
      throw error;
    }
  }

  close() {
    try { this.db.close(); } catch {}
  }

  getMeta(key) {
    return this.db.prepare('SELECT value FROM metadata WHERE key = ?').get(key)?.value ?? null;
  }

  setMeta(key, value) {
    this.db.prepare(`
      INSERT INTO metadata(key, value) VALUES(?, ?)
      ON CONFLICT(key) DO UPDATE SET value = excluded.value
    `).run(key, String(value));
  }

  getState(key) {
    return this.db.prepare('SELECT value FROM repository_state WHERE key = ?').get(key)?.value ?? null;
  }

  setState(key, value) {
    this.db.prepare(`
      INSERT INTO repository_state(key, value) VALUES(?, ?)
      ON CONFLICT(key) DO UPDATE SET value = excluded.value
    `).run(key, String(value));
  }

  initialized() {
    return this.getMeta('repository_initialized') === '1';
  }

  loadGraph() {
    return this.loadGraphSnapshot().state;
  }

  loadGraphSnapshot() {
    this.db.exec('BEGIN');
    try {
      const nodes = {};
      const edges = {};
      for (const row of this.db.prepare('SELECT id, json FROM graph_nodes').all()) {
        nodes[row.id] = JSON.parse(row.json);
      }
      for (const row of this.db.prepare('SELECT id, json FROM graph_edges').all()) {
        edges[row.id] = JSON.parse(row.json);
      }
      const version = Number(this.getMeta('graph_version') ?? 1);
      const metadata = parseJson(this.getMeta('graph_metadata'), {});
      const revision = this.graphRevision();
      this.db.exec('COMMIT');
      return { state: { version, nodes, edges, metadata }, revision };
    } catch (error) {
      try { this.db.exec('ROLLBACK'); } catch {}
      throw error;
    }
  }

  replaceGraph(state, { incrementRevision = true } = {}) {
    const insertNode = this.db.prepare(`
      INSERT INTO graph_nodes(id, kind, status, title, body, path, source_kind, updated_at, version, json)
      VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
    `);
    const insertEdge = this.db.prepare(`
      INSERT INTO graph_edges(id, from_id, to_id, type, weight, json)
      VALUES(?, ?, ?, ?, ?, ?)
    `);
    const insertStorage = this.db.prepare(`
      INSERT INTO graph_node_storage(
        node_id, tier, last_access_at, last_tier_change_at, archived_at, compacted_at, access_count
      ) VALUES(?, ?, NULL, ?, ?, NULL, 0)
    `);

    return this.transaction(() => {
      this.db.exec('DELETE FROM graph_edges; DELETE FROM graph_nodes;');
      const storageNow = new Date().toISOString();
      for (const node of Object.values(state.nodes ?? {})) {
        insertNode.run(
          node.id,
          node.kind,
          node.status ?? 'active',
          node.title ?? '',
          node.body ?? '',
          node.metadata?.path ?? sourcePath(node),
          node.metadata?.sourceKind ?? null,
          node.updatedAt ?? null,
          Number(node.version ?? 1),
          JSON.stringify(node)
        );
        const tier = storageTierForNode(node, 'warm');
        const tierChangedAt = node.metadata?.governorUpdatedAt ?? node.updatedAt ?? storageNow;
        insertStorage.run(
          node.id,
          tier,
          tierChangedAt,
          node.status === 'archived' ? (node.updatedAt ?? storageNow) : null
        );
      }
      for (const edge of Object.values(state.edges ?? {})) {
        insertEdge.run(
          edge.id,
          edge.from,
          edge.to,
          edge.type,
          Number(edge.weight ?? 1),
          JSON.stringify(edge)
        );
      }
      this.setMeta('graph_version', String(state.version ?? 1));
      this.setMeta('graph_metadata', JSON.stringify(state.metadata ?? {}));
      this.db.exec('DELETE FROM search_dirty_nodes;');
      const markDirty = this.db.prepare('INSERT INTO search_dirty_nodes(node_id, removed) VALUES(?, 0)');
      for (const node of Object.values(state.nodes ?? {})) markDirty.run(node.id);
      if (incrementRevision) {
        const next = Number(this.getMeta('graph_revision') ?? 0) + 1;
        this.setMeta('graph_revision', String(next));
      }
    });
  }

  syncGraph(state, { expectedRevision = null, mutationHints = null } = {}) {
    const hasHints = mutationHints?.version === 1;
    let nextNodes = null;
    let nextEdges = null;

    const upsertNode = this.db.prepare(`
      INSERT INTO graph_nodes(id, kind, status, title, body, path, source_kind, updated_at, version, json)
      VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
      ON CONFLICT(id) DO UPDATE SET
        kind = excluded.kind,
        status = excluded.status,
        title = excluded.title,
        body = excluded.body,
        path = excluded.path,
        source_kind = excluded.source_kind,
        updated_at = excluded.updated_at,
        version = excluded.version,
        json = excluded.json
    `);
    const upsertEdge = this.db.prepare(`
      INSERT INTO graph_edges(id, from_id, to_id, type, weight, json)
      VALUES(?, ?, ?, ?, ?, ?)
      ON CONFLICT(id) DO UPDATE SET
        from_id = excluded.from_id,
        to_id = excluded.to_id,
        type = excluded.type,
        weight = excluded.weight,
        json = excluded.json
    `);
    const deleteEdge = this.db.prepare('DELETE FROM graph_edges WHERE id = ?');
    const deleteNode = this.db.prepare('DELETE FROM graph_nodes WHERE id = ?');
    const dirty = this.db.prepare(`
      INSERT INTO search_dirty_nodes(node_id, removed) VALUES(?, ?)
      ON CONFLICT(node_id) DO UPDATE SET removed = excluded.removed
    `);
    const getStorage = this.db.prepare(
      'SELECT tier, last_tier_change_at, archived_at FROM graph_node_storage WHERE node_id = ?'
    );
    const upsertStorage = this.db.prepare(`
      INSERT INTO graph_node_storage(
        node_id, tier, last_access_at, last_tier_change_at, archived_at, compacted_at, access_count
      ) VALUES(?, ?, NULL, ?, ?, NULL, 0)
      ON CONFLICT(node_id) DO UPDATE SET
        tier = excluded.tier,
        last_tier_change_at = excluded.last_tier_change_at,
        archived_at = excluded.archived_at
    `);

    return this.transaction(() => {
      const currentRevision = this.graphRevision();
      if (expectedRevision !== null && Number(expectedRevision) !== currentRevision) {
        const error = new Error(
          `Graph revision conflict: expected ${expectedRevision}, current ${currentRevision}`
        );
        error.code = 'GRAPH_REVISION_CONFLICT';
        error.expectedRevision = Number(expectedRevision);
        error.currentRevision = currentRevision;
        throw error;
      }

      let changedNodeIds;
      let removedNodeIds;
      let changedEdgeIds;
      let removedEdgeIds;

      if (hasHints) {
        changedNodeIds = [...new Set(mutationHints.changedNodeIds ?? [])]
          .filter((id) => Boolean(state.nodes?.[id]));
        removedNodeIds = [...new Set(mutationHints.removedNodeIds ?? [])];
        changedEdgeIds = [...new Set(mutationHints.changedEdgeIds ?? [])]
          .filter((id) => Boolean(state.edges?.[id]));
        removedEdgeIds = [...new Set(mutationHints.removedEdgeIds ?? [])];
      } else {
        nextNodes = new Map(
          Object.values(state.nodes ?? {}).map((node) => [node.id, JSON.stringify(node)])
        );
        nextEdges = new Map(
          Object.values(state.edges ?? {}).map((edge) => [edge.id, JSON.stringify(edge)])
        );
        const existingNodes = new Map(
          this.db.prepare('SELECT id, json FROM graph_nodes').all().map((row) => [row.id, row.json])
        );
        const existingEdges = new Map(
          this.db.prepare('SELECT id, json FROM graph_edges').all().map((row) => [row.id, row.json])
        );

        changedNodeIds = [];
        removedNodeIds = [];
        changedEdgeIds = [];
        removedEdgeIds = [];

        for (const [id, json] of nextNodes) {
          if (existingNodes.get(id) !== json) changedNodeIds.push(id);
        }
        for (const id of existingNodes.keys()) {
          if (!nextNodes.has(id)) removedNodeIds.push(id);
        }
        for (const [id, json] of nextEdges) {
          if (existingEdges.get(id) !== json) changedEdgeIds.push(id);
        }
        for (const id of existingEdges.keys()) {
          if (!nextEdges.has(id)) removedEdgeIds.push(id);
        }
      }

      const metadataChanged =
        String(this.getMeta('graph_version') ?? '1') !== String(state.version ?? 1) ||
        String(this.getMeta('graph_metadata') ?? '{}') !== JSON.stringify(state.metadata ?? {});

      if (!changedNodeIds.length && !removedNodeIds.length && !changedEdgeIds.length && !removedEdgeIds.length && !metadataChanged) {
        return { changed: false, changedNodeIds: [], removedNodeIds: [], revision: currentRevision };
      }

      for (const id of removedEdgeIds) deleteEdge.run(id);
      for (const id of removedNodeIds) {
        deleteNode.run(id);
        dirty.run(id, 1);
      }
      const storageNow = new Date().toISOString();
      for (const id of changedNodeIds) {
        const node = state.nodes[id];
        upsertNode.run(
          node.id,
          node.kind,
          node.status ?? 'active',
          node.title ?? '',
          node.body ?? '',
          node.metadata?.path ?? sourcePath(node),
          node.metadata?.sourceKind ?? null,
          node.updatedAt ?? null,
          Number(node.version ?? 1),
          hasHints ? JSON.stringify(node) : nextNodes.get(id)
        );
        const currentStorage = getStorage.get(id);
        const tier = storageTierForNode(node, currentStorage?.tier ?? 'warm');
        const tierChangedAt = currentStorage?.tier === tier
          ? (currentStorage?.last_tier_change_at ?? storageNow)
          : storageNow;
        const archivedAt = node.status === 'archived'
          ? (currentStorage?.archived_at ?? storageNow)
          : null;
        upsertStorage.run(id, tier, tierChangedAt, archivedAt);
        dirty.run(id, 0);
      }
      for (const id of changedEdgeIds) {
        const edge = state.edges[id];
        upsertEdge.run(
          edge.id,
          edge.from,
          edge.to,
          edge.type,
          Number(edge.weight ?? 1),
          hasHints ? JSON.stringify(edge) : nextEdges.get(id)
        );
      }

      const revision = currentRevision + 1;
      this.setMeta('graph_version', String(state.version ?? 1));
      this.setMeta('graph_metadata', JSON.stringify(state.metadata ?? {}));
      this.setMeta('graph_revision', String(revision));

      return { changed: true, changedNodeIds, removedNodeIds, revision };
    });
  }

  graphRevision() {
    return Number(this.getMeta('graph_revision') ?? 0);
  }

  touchNodeAccess(nodeIds, at = new Date().toISOString()) {
    const ids = [...new Set((nodeIds ?? []).filter(Boolean))];
    if (!ids.length) return { touched: 0, at };
    const update = this.db.prepare(`
      UPDATE graph_node_storage
      SET last_access_at = ?, access_count = access_count + 1
      WHERE node_id = ?
    `);
    let touched = 0;
    this.transaction(() => {
      for (const id of ids) {
        const result = update.run(at, id);
        touched += Number(result.changes ?? 0);
      }
    });
    return { touched, at };
  }

  nodeStorageMap() {
    const result = {};
    for (const row of this.db.prepare(`
      SELECT node_id, tier, last_access_at, last_tier_change_at,
             archived_at, compacted_at, access_count
      FROM graph_node_storage
    `).all()) {
      result[row.node_id] = {
        tier: row.tier,
        lastAccessAt: row.last_access_at,
        lastTierChangeAt: row.last_tier_change_at,
        archivedAt: row.archived_at,
        compactedAt: row.compacted_at,
        accessCount: Number(row.access_count ?? 0)
      };
    }
    return result;
  }

  nodeStorageStats() {
    const tiers = { hot: 0, warm: 0, cold: 0 };
    for (const row of this.db.prepare(`
      SELECT tier, count(*) AS n
      FROM graph_node_storage
      GROUP BY tier
    `).all()) {
      tiers[row.tier] = Number(row.n ?? 0);
    }
    const accessed = Number(this.db.prepare(
      'SELECT count(*) AS n FROM graph_node_storage WHERE last_access_at IS NOT NULL'
    ).get()?.n ?? 0);
    const compacted = Number(this.db.prepare(
      'SELECT count(*) AS n FROM graph_node_storage WHERE compacted_at IS NOT NULL'
    ).get()?.n ?? 0);
    const accessCount = Number(this.db.prepare(
      'SELECT coalesce(sum(access_count), 0) AS n FROM graph_node_storage'
    ).get()?.n ?? 0);
    return {
      tiers,
      accessed,
      compacted,
      accessCount,
      total: tiers.hot + tiers.warm + tiers.cold
    };
  }

  listGcCandidates({ olderThanMs = 30 * 24 * 60 * 60 * 1000, limit = 500, now = Date.now() } = {}) {
    const cutoff = new Date(Number(now) - Math.max(0, Number(olderThanMs))).toISOString();
    const rows = this.db.prepare(`
      SELECT n.id, n.kind, n.status, n.title, n.json,
             s.tier, s.last_access_at, s.archived_at, s.compacted_at, s.access_count
      FROM graph_nodes n
      JOIN graph_node_storage s ON s.node_id = n.id
      WHERE n.status = 'archived'
        AND s.tier = 'cold'
        AND s.archived_at IS NOT NULL
        AND s.archived_at <= ?
      ORDER BY s.archived_at ASC, n.id ASC
      LIMIT ?
    `).all(cutoff, Math.max(1, Number(limit)));

    return rows
      .map((row) => {
        const node = parseJson(row.json, {});
        return {
          nodeId: row.id,
          kind: row.kind,
          title: row.title,
          tier: row.tier,
          archivedAt: row.archived_at,
          lastAccessAt: row.last_access_at,
          compactedAt: row.compacted_at,
          accessCount: Number(row.access_count ?? 0),
          protected: row.kind === 'evidence' && ['runtime', 'reproduced'].includes(node.grade),
          grade: node.grade ?? null
        };
      })
      .filter((item) => !item.protected);
  }

  compactDerivedNodeData(nodeIds, { at = new Date().toISOString() } = {}) {
    const ids = [...new Set((nodeIds ?? []).filter(Boolean))];
    if (!ids.length) return { compacted: 0, nodeIds: [], at };

    const deleteSymbols = this.db.prepare('DELETE FROM symbols WHERE node_id = ?');
    const deleteFts = this.db.prepare('DELETE FROM node_fts WHERE node_id = ?');
    const deleteDoc = this.db.prepare('DELETE FROM search_documents WHERE node_id = ?');
    const deleteDirty = this.db.prepare('DELETE FROM search_dirty_nodes WHERE node_id = ?');
    const deleteEmbeddings = this.db.prepare('DELETE FROM node_embeddings WHERE node_id = ?');
    const mark = this.db.prepare(
      'UPDATE graph_node_storage SET compacted_at = ? WHERE node_id = ?'
    );

    let compacted = 0;
    this.transaction(() => {
      for (const id of ids) {
        deleteSymbols.run(id);
        deleteFts.run(id);
        deleteDoc.run(id);
        deleteDirty.run(id);
        deleteEmbeddings.run(id);
        const result = mark.run(at, id);
        compacted += Number(result.changes ?? 0);
      }
      const count = Number(this.db.prepare('SELECT count(*) AS n FROM search_documents').get()?.n ?? 0);
      const average = Number(this.db.prepare('SELECT avg(length) AS n FROM search_documents').get()?.n ?? 0);
      this.setMeta('search_document_count', String(count));
      this.setMeta('search_average_length', String(average || 0));
    });

    return { compacted, nodeIds: ids, at };
  }

  compactColdArchived(options = {}) {
    const candidates = this.listGcCandidates(options);
    if (options.dryRun !== false) {
      return { dryRun: true, candidates, compacted: 0 };
    }
    const result = this.compactDerivedNodeData(candidates.map((item) => item.nodeId));
    return {
      dryRun: false,
      candidates,
      ...result
    };
  }

  saveCommit(commit) {
    this.db.prepare(`
      INSERT INTO cognitive_commits(
        id, format_version, message, parents_json, created_at, graph_hash, diff_json, metadata_json
      ) VALUES(?, ?, ?, ?, ?, ?, ?, ?)
      ON CONFLICT(id) DO UPDATE SET
        message = excluded.message,
        parents_json = excluded.parents_json,
        created_at = excluded.created_at,
        graph_hash = excluded.graph_hash,
        diff_json = excluded.diff_json,
        metadata_json = excluded.metadata_json
    `).run(
      commit.id,
      Number(commit.formatVersion ?? 1),
      commit.message,
      JSON.stringify(commit.parents ?? []),
      commit.createdAt,
      commit.graphHash,
      JSON.stringify(commit.diff ?? { operations: [] }),
      JSON.stringify(commit.metadata ?? {})
    );
  }

  getCommit(id) {
    const row = this.db.prepare('SELECT * FROM cognitive_commits WHERE id = ?').get(id);
    if (!row) return null;
    return {
      id: row.id,
      formatVersion: row.format_version,
      message: row.message,
      parents: JSON.parse(row.parents_json),
      createdAt: row.created_at,
      graphHash: row.graph_hash,
      diff: JSON.parse(row.diff_json),
      metadata: JSON.parse(row.metadata_json)
    };
  }

  hasCommit(id) {
    return Boolean(this.db.prepare('SELECT 1 ok FROM cognitive_commits WHERE id = ?').get(id));
  }

  saveCheckpoint(commitId, snapshot, createdAt = new Date().toISOString()) {
    this.db.prepare(`
      INSERT INTO cognitive_checkpoints(commit_id, snapshot_json, created_at)
      VALUES(?, ?, ?)
      ON CONFLICT(commit_id) DO UPDATE SET
        snapshot_json = excluded.snapshot_json,
        created_at = excluded.created_at
    `).run(commitId, JSON.stringify(snapshot), createdAt);
  }

  getCheckpoint(commitId) {
    const row = this.db.prepare(
      'SELECT snapshot_json FROM cognitive_checkpoints WHERE commit_id = ?'
    ).get(commitId);
    return row ? JSON.parse(row.snapshot_json) : null;
  }

  hasCheckpoint(commitId) {
    return Boolean(
      this.db.prepare('SELECT 1 ok FROM cognitive_checkpoints WHERE commit_id = ?').get(commitId)
    );
  }

  firstParent(commitId) {
    const row = this.db.prepare('SELECT parents_json FROM cognitive_commits WHERE id = ?').get(commitId);
    if (!row) return null;
    const parents = JSON.parse(row.parents_json);
    return parents?.[0] ?? null;
  }

  distanceToCheckpoint(commitId, maxDistance = 50) {
    let current = commitId;
    let distance = 0;
    while (current && distance <= maxDistance) {
      if (this.hasCheckpoint(current)) return distance;
      current = this.firstParent(current);
      distance += 1;
    }
    return distance;
  }

  setRef(name, commitId) {
    this.db.prepare(`
      INSERT INTO cognitive_refs(name, commit_id) VALUES(?, ?)
      ON CONFLICT(name) DO UPDATE SET commit_id = excluded.commit_id
    `).run(name, commitId);
  }

  getRef(name) {
    return this.db.prepare('SELECT commit_id FROM cognitive_refs WHERE name = ?').get(name)?.commit_id ?? null;
  }

  listRefs() {
    return this.db.prepare('SELECT name, commit_id FROM cognitive_refs ORDER BY name').all();
  }

  saveSession(session) {
    const upsert = this.db.prepare(`
      INSERT INTO sessions(
        id, created_at, updated_at, status, provider, model, goal,
        metadata_json, final, usage_json, error_json
      ) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
      ON CONFLICT(id) DO UPDATE SET
        updated_at = excluded.updated_at,
        status = excluded.status,
        provider = excluded.provider,
        model = excluded.model,
        goal = excluded.goal,
        metadata_json = excluded.metadata_json,
        final = excluded.final,
        usage_json = excluded.usage_json,
        error_json = excluded.error_json
    `);
    const upsertMessage = this.db.prepare(`
      INSERT INTO session_messages(session_id, seq, role, json) VALUES(?, ?, ?, ?)
      ON CONFLICT(session_id, seq) DO UPDATE SET
        role = excluded.role,
        json = excluded.json
    `);
    const upsertStep = this.db.prepare(`
      INSERT INTO agent_steps(session_id, step, json) VALUES(?, ?, ?)
      ON CONFLICT(session_id, step) DO UPDATE SET
        json = excluded.json
    `);

    this.transaction(() => {
      upsert.run(
        session.id,
        session.createdAt,
        session.updatedAt,
        session.status,
        session.provider ?? null,
        session.model ?? null,
        session.goal ?? '',
        JSON.stringify(session.metadata ?? {}),
        session.final ?? null,
        session.usage ? JSON.stringify(session.usage) : null,
        session.error ? JSON.stringify(session.error) : null
      );

      const messages = session.messages ?? [];
      messages.forEach((message, index) => {
        upsertMessage.run(session.id, index, message.role ?? '', JSON.stringify(message));
      });
      this.db.prepare('DELETE FROM session_messages WHERE session_id = ? AND seq >= ?')
        .run(session.id, messages.length);

      const steps = session.steps ?? [];
      for (const step of steps) {
        upsertStep.run(session.id, Number(step.step), JSON.stringify(step));
      }
      const maxStep = steps.length ? Math.max(...steps.map((step) => Number(step.step))) : 0;
      this.db.prepare('DELETE FROM agent_steps WHERE session_id = ? AND step > ?')
        .run(session.id, maxStep);
    });
  }

  loadSession(id) {
    const row = this.db.prepare('SELECT * FROM sessions WHERE id = ?').get(id);
    if (!row) return null;
    const messages = this.db.prepare(
      'SELECT json FROM session_messages WHERE session_id = ? ORDER BY seq'
    ).all(id).map((item) => JSON.parse(item.json));
    const steps = this.db.prepare(
      'SELECT json FROM agent_steps WHERE session_id = ? ORDER BY step'
    ).all(id).map((item) => JSON.parse(item.json));
    return {
      id: row.id,
      createdAt: row.created_at,
      updatedAt: row.updated_at,
      status: row.status,
      provider: row.provider,
      model: row.model,
      goal: row.goal,
      messages,
      steps,
      metadata: JSON.parse(row.metadata_json),
      ...(row.final !== null ? { final: row.final } : {}),
      ...(row.usage_json ? { usage: JSON.parse(row.usage_json) } : {}),
      ...(row.error_json ? { error: JSON.parse(row.error_json) } : {})
    };
  }

  listSessions(limit = 20) {
    return this.db.prepare(`
      SELECT
        s.*,
        (SELECT count(*) FROM session_messages m WHERE m.session_id = s.id) AS message_count,
        (SELECT count(*) FROM agent_steps a WHERE a.session_id = s.id) AS step_count
      FROM sessions s
      ORDER BY s.updated_at DESC
      LIMIT ?
    `).all(Number(limit)).map((row) => ({
      id: row.id,
      createdAt: row.created_at,
      updatedAt: row.updated_at,
      status: row.status,
      provider: row.provider,
      model: row.model,
      goal: row.goal,
      metadata: JSON.parse(row.metadata_json),
      ...(row.final !== null ? { final: row.final } : {}),
      ...(row.usage_json ? { usage: JSON.parse(row.usage_json) } : {}),
      ...(row.error_json ? { error: JSON.parse(row.error_json) } : {}),
      messageCount: Number(row.message_count),
      stepCount: Number(row.step_count)
    }));
  }

  appendJournal(event, payload, at = new Date().toISOString()) {
    this.db.prepare('INSERT INTO journal(at, event, payload_json) VALUES(?, ?, ?)')
      .run(at, event, JSON.stringify(payload ?? {}));
  }

  listJournal(limit = 100) {
    return this.db.prepare(
      'SELECT seq, at, event, payload_json FROM journal ORDER BY seq DESC LIMIT ?'
    ).all(Number(limit)).map((row) => ({
      seq: Number(row.seq),
      at: row.at,
      event: row.event,
      payload: JSON.parse(row.payload_json)
    }));
  }

  clearSearchIndex() {
    this.transaction(() => {
      const currentGraphRevision = this.graphRevision();
      if (graphRevision !== null && Number(graphRevision) !== currentGraphRevision) {
        const error = new Error(`Search rebuild revision conflict: expected ${graphRevision}, current ${currentGraphRevision}`);
        error.code = 'SEARCH_REVISION_CONFLICT';
        throw error;
      }
      this.db.exec('DELETE FROM symbols; DELETE FROM search_documents; DELETE FROM node_fts;');
      this.setMeta('search_index_revision', '-1');
      this.setMeta('search_index_created_at', '');
    });
  }

  rebuildSearchIndex(graphState, { graphRevision = null, extractSymbols, searchableText, indexTerms } = {}) {
    const insertDoc = this.db.prepare(`
      INSERT INTO search_documents(node_id, title, path, kind, source_kind, content_hash, length)
      VALUES(?, ?, ?, ?, ?, ?, ?)
    `);
    const insertSymbol = this.db.prepare(
      'INSERT OR IGNORE INTO symbols(symbol, node_id) VALUES(?, ?)'
    );
    const insertFts = this.db.prepare(
      'INSERT INTO node_fts(node_id, title, body, path, tags) VALUES(?, ?, ?, ?, ?)'
    );

    const createdAt = new Date().toISOString();
    let documentCount = 0;
    let totalLength = 0;

    this.transaction(() => {
      this.db.exec('DELETE FROM symbols; DELETE FROM search_documents; DELETE FROM node_fts;');
      for (const node of Object.values(graphState.nodes ?? {})) {
        if (node.status === 'archived' || node.status === 'invalid') continue;
        const text = searchableText(node);
        const terms = indexTerms(text);
        if (!terms.length) continue;
        const docPath = node.metadata?.path ?? sourcePath(node);
        insertDoc.run(
          node.id,
          node.title ?? '',
          docPath || null,
          node.kind,
          node.metadata?.sourceKind ?? null,
          node.contentHash ?? null,
          terms.length
        );
        insertFts.run(
          node.id,
          node.title ?? '',
          node.body ?? '',
          docPath ?? '',
          (node.tags ?? []).join(' ')
        );
        for (const symbol of extractSymbols(node)) {
          insertSymbol.run(symbol, node.id);
        }
        documentCount += 1;
        totalLength += terms.length;
      }
      this.db.exec('DELETE FROM search_dirty_nodes;');
      this.setMeta('search_index_revision', String(graphRevision ?? -1));
      this.setMeta('search_index_created_at', createdAt);
      this.setMeta('search_document_count', String(documentCount));
      this.setMeta('search_average_length', String(documentCount ? totalLength / documentCount : 0));
    });

    return this.searchStats();
  }

  syncSearchIndex(graphState, { graphRevision = null, extractSymbols, searchableText, indexTerms } = {}) {
    if (!this.searchIndexReady()) {
      return this.rebuildSearchIndex(graphState, { graphRevision, extractSymbols, searchableText, indexTerms });
    }

    const deleteSymbols = this.db.prepare('DELETE FROM symbols WHERE node_id = ?');
    const deleteDoc = this.db.prepare('DELETE FROM search_documents WHERE node_id = ?');
    const deleteFts = this.db.prepare('DELETE FROM node_fts WHERE node_id = ?');
    const deleteDirty = this.db.prepare('DELETE FROM search_dirty_nodes WHERE node_id = ?');
    const insertDoc = this.db.prepare(`
      INSERT INTO search_documents(node_id, title, path, kind, source_kind, content_hash, length)
      VALUES(?, ?, ?, ?, ?, ?, ?)
    `);
    const insertSymbol = this.db.prepare(
      'INSERT OR IGNORE INTO symbols(symbol, node_id) VALUES(?, ?)'
    );
    const insertFts = this.db.prepare(
      'INSERT INTO node_fts(node_id, title, body, path, tags) VALUES(?, ?, ?, ?, ?)'
    );

    this.transaction(() => {
      const currentGraphRevision = this.graphRevision();
      if (graphRevision !== null && Number(graphRevision) !== currentGraphRevision) {
        const error = new Error(
          `Search sync revision conflict: expected ${graphRevision}, current ${currentGraphRevision}`
        );
        error.code = 'SEARCH_REVISION_CONFLICT';
        throw error;
      }

      const dirtyRows = this.db.prepare('SELECT node_id, removed FROM search_dirty_nodes').all();
      for (const row of dirtyRows) {
        deleteSymbols.run(row.node_id);
        deleteFts.run(row.node_id);
        deleteDoc.run(row.node_id);

        const node = graphState.nodes?.[row.node_id];
        if (Number(row.removed) !== 1 && node && node.status !== 'archived' && node.status !== 'invalid') {
          const text = searchableText(node);
          const terms = indexTerms(text);
          if (terms.length) {
            const docPath = node.metadata?.path ?? sourcePath(node);
            insertDoc.run(
              node.id,
              node.title ?? '',
              docPath || null,
              node.kind,
              node.metadata?.sourceKind ?? null,
              node.contentHash ?? null,
              terms.length
            );
            insertFts.run(node.id, node.title ?? '', node.body ?? '', docPath ?? '', (node.tags ?? []).join(' '));
            for (const symbol of extractSymbols(node)) insertSymbol.run(symbol, node.id);
          }
        }
        deleteDirty.run(row.node_id);
      }

      this.setMeta('search_index_revision', String(graphRevision ?? currentGraphRevision));
      this.setMeta('search_index_created_at', new Date().toISOString());
      const count = Number(this.db.prepare('SELECT count(*) AS n FROM search_documents').get()?.n ?? 0);
      const average = Number(this.db.prepare('SELECT avg(length) AS n FROM search_documents').get()?.n ?? 0);
      this.setMeta('search_document_count', String(count));
      this.setMeta('search_average_length', String(average || 0));
    });

    return this.searchStats();
  }

  embeddingManifest(model) {
    const rows = this.db.prepare(`
      SELECT node_id, dimension, content_hash, updated_at
      FROM node_embeddings
      WHERE model = ?
      ORDER BY node_id
    `).all(String(model));
    return rows.map((row) => ({
      nodeId: row.node_id,
      dimension: Number(row.dimension),
      contentHash: row.content_hash,
      updatedAt: row.updated_at
    }));
  }

  syncEmbeddings({ model, upserts = [], removeNodeIds = [], graphRevision = null } = {}) {
    if (!model) throw new Error('Embedding model is required');
    const normalizedModel = String(model);
    const deleteOne = this.db.prepare(
      'DELETE FROM node_embeddings WHERE node_id = ? AND model = ?'
    );
    const upsert = this.db.prepare(`
      INSERT INTO node_embeddings(
        node_id, model, dimension, content_hash, vector_json, updated_at
      ) VALUES(?, ?, ?, ?, ?, ?)
      ON CONFLICT(node_id, model) DO UPDATE SET
        dimension = excluded.dimension,
        content_hash = excluded.content_hash,
        vector_json = excluded.vector_json,
        updated_at = excluded.updated_at
    `);
    const updatedAt = new Date().toISOString();

    this.transaction(() => {
      const currentGraphRevision = this.graphRevision();
      if (graphRevision !== null && Number(graphRevision) !== currentGraphRevision) {
        const error = new Error(
          `Embedding sync revision conflict: expected ${graphRevision}, current ${currentGraphRevision}`
        );
        error.code = 'EMBEDDING_REVISION_CONFLICT';
        error.expectedRevision = Number(graphRevision);
        error.currentRevision = currentGraphRevision;
        throw error;
      }

      for (const nodeId of new Set(removeNodeIds.filter(Boolean))) {
        deleteOne.run(nodeId, normalizedModel);
      }
      for (const item of upserts) {
        const vector = normalizeVector(item.vector);
        upsert.run(
          item.nodeId,
          normalizedModel,
          vector.length,
          String(item.contentHash ?? ''),
          JSON.stringify(vector),
          item.updatedAt ?? updatedAt
        );
      }

      this.setMeta(embeddingMetaKey('revision', normalizedModel), String(graphRevision ?? currentGraphRevision));
      this.setMeta(embeddingMetaKey('updated_at', normalizedModel), updatedAt);
    });

    return this.embeddingStats(normalizedModel);
  }

  clearEmbeddings(model) {
    if (!model) throw new Error('Embedding model is required');
    const normalizedModel = String(model);
    this.transaction(() => {
      this.db.prepare('DELETE FROM node_embeddings WHERE model = ?').run(normalizedModel);
      this.setMeta(embeddingMetaKey('revision', normalizedModel), '-1');
      this.setMeta(embeddingMetaKey('updated_at', normalizedModel), '');
    });
    return this.embeddingStats(normalizedModel);
  }

  searchEmbeddings(queryVector, { model, limit = 50, minScore = -1 } = {}) {
    if (!model) throw new Error('Embedding model is required');
    const query = normalizeVector(queryVector);
    const max = Math.max(1, Number(limit));
    const rows = this.db.prepare(`
      SELECT e.node_id, e.dimension, e.content_hash, e.vector_json,
             n.title, n.path, n.kind, n.source_kind
      FROM node_embeddings e
      JOIN graph_nodes n ON n.id = e.node_id
      WHERE e.model = ?
    `).all(String(model));

    return rows
      .map((row) => {
        if (Number(row.dimension) !== query.length) return null;
        const vector = normalizeVector(parseJson(row.vector_json, []));
        if (vector.length !== query.length) return null;
        const score = cosineSimilarity(query, vector);
        if (!Number.isFinite(score) || score < Number(minScore)) return null;
        return {
          nodeId: row.node_id,
          score,
          model: String(model),
          dimension: query.length,
          contentHash: row.content_hash,
          title: row.title ?? '',
          path: row.path ?? '',
          kind: row.kind ?? '',
          sourceKind: row.source_kind ?? null
        };
      })
      .filter(Boolean)
      .sort((a, b) => b.score - a.score || a.nodeId.localeCompare(b.nodeId))
      .slice(0, max);
  }

  embeddingStats(model = null) {
    const where = model ? ' WHERE model = ?' : '';
    const args = model ? [String(model)] : [];
    const row = this.db.prepare(`
      SELECT count(*) AS n,
             count(DISTINCT model) AS models,
             min(dimension) AS min_dimension,
             max(dimension) AS max_dimension
      FROM node_embeddings${where}
    `).get(...args);
    return {
      model: model ? String(model) : null,
      count: Number(row?.n ?? 0),
      models: Number(row?.models ?? 0),
      minDimension: row?.min_dimension == null ? null : Number(row.min_dimension),
      maxDimension: row?.max_dimension == null ? null : Number(row.max_dimension),
      graphRevision: model
        ? Number(this.getMeta(embeddingMetaKey('revision', String(model))) ?? -1)
        : null,
      updatedAt: model
        ? this.getMeta(embeddingMetaKey('updated_at', String(model)))
        : null
    };
  }

  searchIndexReady() {
    return Number(this.getMeta('search_document_count') ?? 0) > 0 ||
      Boolean(this.getMeta('search_index_created_at'));
  }

  search(query, { limit = 50, identifierTerms, indexTerms } = {}) {
    const max = Math.max(1, Number(limit));
    const scores = new Map();
    const reasons = new Map();

    for (const raw of identifierTerms(query)) {
      for (const row of this.db.prepare(
        'SELECT node_id FROM symbols WHERE symbol = ? LIMIT ?'
      ).all(raw, max * 4)) {
        addScore(scores, reasons, row.node_id, 8, `symbol:${raw}`);
      }
      const lower = raw.toLowerCase();
      if (lower !== raw) {
        for (const row of this.db.prepare(
          'SELECT node_id FROM symbols WHERE symbol = ? LIMIT ?'
        ).all(lower, max * 4)) {
          addScore(scores, reasons, row.node_id, 5, `symbol-ci:${raw}`);
        }
      }
    }

    const ftsTerms = [...new Set(indexTerms(query))]
      .filter((term) => term.length > 1)
      .slice(0, 24);
    if (ftsTerms.length) {
      const match = ftsTerms.map(quoteFts).join(' OR ');
      try {
        const rows = this.db.prepare(`
          SELECT node_id, bm25(node_fts, 0.0, 3.0, 1.0, 2.0, 0.5) AS rank
          FROM node_fts
          WHERE node_fts MATCH ?
          ORDER BY rank
          LIMIT ?
        `).all(match, max * 4);
        for (const row of rows) {
          const score = Math.max(0.001, Math.abs(Number(row.rank ?? 0)));
          addScore(scores, reasons, row.node_id, score, 'fts5');
        }
      } catch {
        // Exact symbol matches remain usable even if a malformed user query reaches FTS.
      }
    }

    const ranked = [...scores.entries()]
      .sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]))
      .slice(0, max);
    const getDoc = this.db.prepare('SELECT * FROM search_documents WHERE node_id = ?');
    return ranked.map(([nodeId, score]) => {
      const doc = getDoc.get(nodeId) ?? {};
      return {
        nodeId,
        score,
        reasons: [...(reasons.get(nodeId) ?? [])],
        id: nodeId,
        length: Number(doc.length ?? 0),
        title: doc.title ?? '',
        path: doc.path ?? '',
        kind: doc.kind ?? '',
        sourceKind: doc.source_kind ?? null,
        contentHash: doc.content_hash ?? null
      };
    });
  }

  status() {
    const scalar = (sql, key) => this.db.prepare(sql).get()?.[key] ?? null;
    const count = (table) => Number(this.db.prepare(`SELECT count(*) AS n FROM ${table}`).get()?.n ?? 0);
    const fileSize = (file) => {
      try { return fs.statSync(file).size; } catch { return 0; }
    };
    return {
      file: this.file,
      fileSizeBytes: fileSize(this.file),
      walSizeBytes: fileSize(`${this.file}-wal`),
      shmSizeBytes: fileSize(`${this.file}-shm`),
      journalMode: String(scalar('PRAGMA journal_mode', 'journal_mode') ?? ''),
      synchronous: Number(scalar('PRAGMA synchronous', 'synchronous') ?? 0),
      busyTimeoutMs: Number(scalar('PRAGMA busy_timeout', 'timeout') ?? 0),
      schemaVersion: Number(this.getMeta('schema_version') ?? 0),
      graphRevision: this.graphRevision(),
      searchRevision: Number(this.getMeta('search_index_revision') ?? -1),
      storage: this.nodeStorageStats(),
      counts: {
        graphNodes: count('graph_nodes'),
        graphEdges: count('graph_edges'),
        graphNodeStorage: count('graph_node_storage'),
        cognitiveCommits: count('cognitive_commits'),
        cognitiveCheckpoints: count('cognitive_checkpoints'),
        cognitiveRefs: count('cognitive_refs'),
        sessions: count('sessions'),
        sessionMessages: count('session_messages'),
        agentSteps: count('agent_steps'),
        journal: count('journal'),
        searchDocuments: count('search_documents'),
        symbols: count('symbols'),
        dirtySearchNodes: count('search_dirty_nodes'),
        nodeEmbeddings: count('node_embeddings')
      }
    };
  }

  integrityCheck() {
    const rows = this.db.prepare('PRAGMA integrity_check').all();
    const messages = rows.flatMap((row) => Object.values(row).map(String));
    return { ok: messages.length === 1 && messages[0].toLowerCase() === 'ok', messages };
  }

  checkpoint(mode = 'TRUNCATE') {
    const normalized = String(mode).toUpperCase();
    if (!['PASSIVE', 'FULL', 'RESTART', 'TRUNCATE'].includes(normalized)) {
      throw new Error(`Unsupported WAL checkpoint mode: ${mode}`);
    }
    const rows = this.db.prepare(`PRAGMA wal_checkpoint(${normalized})`).all();
    return { mode: normalized, rows };
  }

  searchStats() {
    const documentCount = Number(this.db.prepare(
      'SELECT count(*) AS n FROM search_documents'
    ).get()?.n ?? 0);
    const symbolCount = Number(this.db.prepare(
      'SELECT count(DISTINCT symbol) AS n FROM symbols'
    ).get()?.n ?? 0);
    let termCount = 0;
    try {
      termCount = Number(this.db.prepare('SELECT count(*) AS n FROM node_fts_vocab').get()?.n ?? 0);
    } catch {}
    return {
      ready: this.searchIndexReady(),
      file: this.file,
      documentCount,
      termCount,
      symbolCount,
      createdAt: this.getMeta('search_index_created_at'),
      graphRevision: Number(this.getMeta('search_index_revision') ?? -1)
    };
  }
}

function sourcePath(node) {
  const uri = node.source?.uri;
  if (!uri?.startsWith('file://')) return '';
  return uri.slice('file://'.length).split('#')[0];
}

function parseJson(value, fallback) {
  if (value === null || value === undefined || value === '') return fallback;
  try { return JSON.parse(value); } catch { return fallback; }
}

function quoteFts(value) {
  return `"${String(value).replaceAll('"', '""')}"`;
}

function addScore(scores, reasons, nodeId, score, reason) {
  scores.set(nodeId, (scores.get(nodeId) ?? 0) + score);
  if (!reasons.has(nodeId)) reasons.set(nodeId, new Set());
  reasons.get(nodeId).add(reason);
}


function storageTierForNode(node, fallback = 'warm') {
  const value = String(node?.metadata?.storageTier ?? fallback).toLowerCase();
  return ['hot', 'warm', 'cold'].includes(value) ? value : 'warm';
}


function embeddingMetaKey(kind, model) {
  return `embedding_index_${kind}:${String(model)}`;
}

function normalizeVector(vector) {
  if (!Array.isArray(vector) && !ArrayBuffer.isView(vector)) {
    throw new Error('Embedding vector must be an array');
  }
  const values = Array.from(vector, Number);
  if (!values.length || values.some((value) => !Number.isFinite(value))) {
    throw new Error('Embedding vector must contain finite numeric values');
  }
  return values;
}

function cosineSimilarity(left, right) {
  let dot = 0;
  let leftNorm = 0;
  let rightNorm = 0;
  for (let i = 0; i < left.length; i += 1) {
    dot += left[i] * right[i];
    leftNorm += left[i] * left[i];
    rightNorm += right[i] * right[i];
  }
  if (leftNorm <= 0 || rightNorm <= 0) return 0;
  return dot / (Math.sqrt(leftNorm) * Math.sqrt(rightNorm));
}
