import fs from 'node:fs';
import path from 'node:path';
import { DatabaseSync } from 'node:sqlite';

const SCHEMA_VERSION = 1;

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

    this.setMeta('schema_version', String(SCHEMA_VERSION));
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
    return { version, nodes, edges, metadata };
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

    return this.transaction(() => {
      this.db.exec('DELETE FROM graph_edges; DELETE FROM graph_nodes;');
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
      if (incrementRevision) {
        const next = Number(this.getMeta('graph_revision') ?? 0) + 1;
        this.setMeta('graph_revision', String(next));
      }
    });
  }

  graphRevision() {
    return Number(this.getMeta('graph_revision') ?? 0);
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
    const insertMessage = this.db.prepare(
      'INSERT INTO session_messages(session_id, seq, role, json) VALUES(?, ?, ?, ?)'
    );
    const insertStep = this.db.prepare(
      'INSERT INTO agent_steps(session_id, step, json) VALUES(?, ?, ?)'
    );

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
      this.db.prepare('DELETE FROM session_messages WHERE session_id = ?').run(session.id);
      this.db.prepare('DELETE FROM agent_steps WHERE session_id = ?').run(session.id);
      (session.messages ?? []).forEach((message, index) => {
        insertMessage.run(session.id, index, message.role ?? '', JSON.stringify(message));
      });
      for (const step of session.steps ?? []) {
        insertStep.run(session.id, Number(step.step), JSON.stringify(step));
      }
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
      this.setMeta('search_index_revision', String(graphRevision ?? -1));
      this.setMeta('search_index_created_at', createdAt);
      this.setMeta('search_document_count', String(documentCount));
      this.setMeta('search_average_length', String(documentCount ? totalLength / documentCount : 0));
    });

    return this.searchStats();
  }

  searchIndexReady() {
    return Number(this.getMeta('search_document_count') ?? 0) > 0 ||
      this.getMeta('search_index_created_at') !== null;
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
