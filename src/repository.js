import fs from 'node:fs';
import path from 'node:path';
import { applyDiff, CognitiveGraph, diffGraphs, emptyGraph, invertDiff } from './graph.js';
import { clone, hash, isEqual, nowIso } from './util.js';
import { resolveStateDir } from './brand.js';
import { LumenCortexDatabase } from './database.js';

const FORMAT_VERSION = 2;

export class CognitiveRepository {
  constructor(workspace = process.cwd()) {
    this.workspace = path.resolve(workspace);
    this.dir = resolveStateDir(this.workspace);
    this.database = new LumenCortexDatabase(this.dir);
    this.commitCache = new Map();
    this.lastGraphRevision = null;
    this.graphCache = null;
    this.graphCacheRevision = null;
    this.#migrateFileStoreIfNeeded();
  }

  init({ branch = 'main' } = {}) {
    if (this.exists()) throw new Error(`LumenCortex repository already exists: ${this.dir}`);
    const graph = emptyGraph();
    this.database.replaceGraph(graph, { incrementRevision: false });
    this.database.setMeta('graph_revision', '1');

    const commit = this.#makeCommit({
      message: 'Initialize cognitive repository',
      parents: [],
      snapshot: graph,
      diff: { operations: [] },
      metadata: { genesis: true }
    });
    this.#writeCommit(commit);
    this.#writeRef(branch, commit.id);
    this.database.setState('HEAD', `ref: refs/heads/${branch}`);
    this.database.setMeta('format_version', String(FORMAT_VERSION));
    this.database.setMeta('created_at', nowIso());
    this.database.setMeta('repository_initialized', '1');
    this.lastGraphRevision = 1;
    this.graphCache = clone(graph);
    this.graphCacheRevision = 1;
    return commit;
  }

  exists() {
    return this.database.initialized();
  }

  assertExists() {
    if (!this.exists()) throw new Error(`Not a LumenCortex repository: ${this.workspace}`);
  }

  graphSnapshot() {
    this.assertExists();
    this.#ensureGraphCache();
    return { state: clone(this.graphCache), revision: this.graphCacheRevision };
  }

  graph() {
    this.assertExists();
    this.#ensureGraphCache();
    return new CognitiveGraph(this.graphCache);
  }

  writeGraph(state) {
    this.assertExists();
    new CognitiveGraph(state).validate();
    const expectedRevision = this.lastGraphRevision ?? this.database.graphRevision();
    const result = this.database.syncGraph(state, { expectedRevision });
    this.lastGraphRevision = result.revision;
    this.graphCache = clone(state);
    this.graphCacheRevision = result.revision;
    return result;
  }

  graphRevision() {
    this.assertExists();
    return this.database.graphRevision();
  }

  headRef() {
    this.assertExists();
    const value = this.database.getState('HEAD');
    if (!value) throw new Error('Repository HEAD is missing');
    return value;
  }

  currentBranch() {
    const head = this.headRef();
    return head.startsWith('ref: refs/heads/') ? head.slice('ref: refs/heads/'.length) : null;
  }

  headCommitId() {
    const branch = this.currentBranch();
    if (branch) return this.#readRef(branch);
    return this.headRef();
  }

  headCommit() {
    return this.getCommit(this.headCommitId());
  }

  getCommit(commitId) {
    const cached = this.commitCache.get(commitId);
    if (cached) return clone(cached);

    this.assertExists();
    const raw = this.database.getCommit(commitId);
    if (!raw) throw new Error(`Unknown cognitive commit: ${commitId}`);

    let snapshot;
    if (!raw.parents?.length) {
      snapshot = applyDiff(emptyGraph(), raw.diff ?? { operations: [] }, { strict: false });
    } else {
      const parent = this.getCommit(raw.parents[0]);
      snapshot = applyDiff(parent.snapshot, raw.diff ?? { operations: [] }, { strict: true });
    }
    const commit = { ...raw, snapshot };
    this.commitCache.set(commitId, clone(commit));
    return clone(commit);
  }

  status() {
    const head = this.headCommit();
    return diffGraphs(head.snapshot, this.graph().snapshot());
  }

  commit(message, { metadata = {}, additionalParents = [] } = {}) {
    this.assertExists();
    const parent = this.headCommit();
    const snapshot = this.graph().snapshot();
    const diff = diffGraphs(parent.snapshot, snapshot);
    if (!diff.operations.length && !additionalParents.length) throw new Error('Nothing to commit');

    const commit = this.#makeCommit({
      message,
      parents: [parent.id, ...additionalParents],
      snapshot,
      diff,
      metadata
    });
    this.#writeCommit(commit);
    this.#advanceHead(commit.id);
    return commit;
  }

  log(limit = 20) {
    const result = [];
    let id = this.headCommitId();
    const seen = new Set();
    while (id && result.length < limit && !seen.has(id)) {
      seen.add(id);
      const commit = this.getCommit(id);
      result.push(commit);
      id = commit.parents?.[0] ?? null;
    }
    return result;
  }

  blame(objectId, options = 50) {
    const limit = typeof options === 'number' ? options : Number(options?.limit ?? 50);
    const result = [];
    const queue = [this.headCommitId()];
    const seen = new Set();

    while (queue.length && result.length < limit) {
      const commitId = queue.shift();
      if (!commitId || seen.has(commitId)) continue;
      seen.add(commitId);
      const commit = this.getCommit(commitId);
      const operations = (commit.diff?.operations ?? []).filter((op) => op.id === objectId);
      if (operations.length) {
        result.push({
          commitId: commit.id,
          createdAt: commit.createdAt,
          message: commit.message,
          metadata: commit.metadata,
          parents: commit.parents ?? [],
          operations: clone(operations)
        });
      }
      for (const parent of commit.parents ?? []) {
        if (!seen.has(parent)) queue.push(parent);
      }
    }
    return result;
  }

  blameNode(objectId, options = 50) {
    return this.blame(objectId, options);
  }

  branches() {
    this.assertExists();
    return this.database.listRefs().map((row) => ({
      name: row.name,
      commitId: row.commit_id,
      current: row.name === this.currentBranch()
    }));
  }

  createBranch(name, startPoint = this.headCommitId()) {
    validateRefName(name);
    if (this.database.getRef(name)) throw new Error(`branch already exists: ${name}`);
    this.getCommit(startPoint);
    this.#writeRef(name, startPoint);
    return { name, commitId: startPoint };
  }

  checkout(name) {
    validateRefName(name);
    const commitId = this.#readRef(name);
    const commit = this.getCommit(commitId);
    this.database.setState('HEAD', `ref: refs/heads/${name}`);
    this.writeGraph(commit.snapshot);
    return commit;
  }

  merge(theirsBranch, { message = `Merge ${theirsBranch}` } = {}) {
    const oursBranch = this.currentBranch();
    if (!oursBranch) throw new Error('Cannot merge into detached HEAD');
    const ours = this.headCommit();
    const theirsId = this.#readRef(theirsBranch);
    const theirs = this.getCommit(theirsId);
    if (ours.id === theirs.id) return { alreadyUpToDate: true, conflicts: [], commit: ours };

    const baseId = this.findMergeBase(ours.id, theirs.id);
    if (!baseId) throw new Error('No merge base found');
    const base = this.getCommit(baseId);
    const merged = threeWayMergeGraph(base.snapshot, ours.snapshot, theirs.snapshot);
    if (merged.conflicts.length) return { alreadyUpToDate: false, conflicts: merged.conflicts, commit: null };

    this.writeGraph(merged.graph);
    const commit = this.commit(message, {
      additionalParents: [theirs.id],
      metadata: { merge: { ours: oursBranch, theirs: theirsBranch, base: base.id } }
    });
    return { alreadyUpToDate: false, conflicts: [], commit };
  }

  cherryPick(commitId, { message } = {}) {
    const target = this.getCommit(commitId);
    if (!target.parents?.length) throw new Error('Cannot cherry-pick genesis commit');
    const parent = this.getCommit(target.parents[0]);
    const ours = this.graph().snapshot();
    const merged = threeWayMergeGraph(parent.snapshot, ours, target.snapshot);
    if (merged.conflicts.length) return { conflicts: merged.conflicts, commit: null };

    this.writeGraph(merged.graph);
    let commit;
    try {
      commit = this.commit(message ?? `Cherry-pick ${commitId}: ${target.message}`, {
        metadata: { cherryPickOf: commitId, cherryPick: commitId }
      });
    } catch (error) {
      if (!String(error.message).includes('Nothing to commit')) throw error;
      commit = this.headCommit();
    }
    return { conflicts: [], commit };
  }

  rebase(ontoBranch) {
    const branch = this.currentBranch();
    if (!branch) throw new Error('Cannot rebase detached HEAD');
    if (branch === ontoBranch) throw new Error('Cannot rebase a branch onto itself');

    const sourceHead = this.headCommit();
    const ontoId = this.#readRef(ontoBranch);
    const onto = this.getCommit(ontoId);
    const baseId = this.findMergeBase(sourceHead.id, ontoId);
    if (!baseId) throw new Error('No merge base found');

    const replay = [];
    let cursor = sourceHead;
    while (cursor.id !== baseId) {
      replay.push(cursor);
      const parentId = cursor.parents?.[0];
      if (!parentId) throw new Error('Rebase first-parent history does not reach merge base');
      cursor = this.getCommit(parentId);
    }
    replay.reverse();

    let working = clone(onto.snapshot);
    let parentId = onto.id;
    const pending = [];
    for (const original of replay) {
      const originalParent = this.getCommit(original.parents[0]);
      const merged = threeWayMergeGraph(originalParent.snapshot, working, original.snapshot);
      if (merged.conflicts.length) {
        return { conflicts: merged.conflicts, commits: [], onto: onto.id, branch };
      }
      const diff = diffGraphs(working, merged.graph);
      const rebased = this.#makeCommit({
        message: original.message,
        parents: [parentId],
        snapshot: merged.graph,
        diff,
        metadata: {
          ...(original.metadata ?? {}),
          rebaseOf: original.id,
          rebaseOnto: onto.id
        }
      });
      pending.push(rebased);
      working = merged.graph;
      parentId = rebased.id;
    }

    for (const commit of pending) this.#writeCommit(commit);
    this.writeGraph(working);
    this.#writeRef(branch, parentId);
    return { conflicts: [], commits: pending, onto: onto.id, branch };
  }

  revert(commitId, { message = `Revert ${commitId}` } = {}) {
    const target = this.getCommit(commitId);
    if (!target.parents?.length) throw new Error('Cannot revert genesis commit');
    const working = this.graph().snapshot();
    const inverse = invertDiff(target.diff);
    try {
      const next = applyDiff(working, inverse, { strict: true });
      this.writeGraph(next);
    } catch (error) {
      return { conflicts: [{ commitId, message: error.message }], commit: null };
    }
    return { conflicts: [], commit: this.commit(message, { metadata: { reverts: commitId } }) };
  }

  findMergeBase(leftId, rightId) {
    const leftAncestors = new Map();
    const queue = [[leftId, 0]];
    while (queue.length) {
      const [id, depth] = queue.shift();
      if (leftAncestors.has(id)) continue;
      leftAncestors.set(id, depth);
      for (const parent of this.getCommit(id).parents ?? []) queue.push([parent, depth + 1]);
    }

    const rightQueue = [[rightId, 0]];
    let best = null;
    while (rightQueue.length) {
      const [id, depth] = rightQueue.shift();
      if (leftAncestors.has(id)) {
        const score = depth + leftAncestors.get(id);
        if (!best || score < best.score) best = { id, score };
      }
      for (const parent of this.getCommit(id).parents ?? []) rightQueue.push([parent, depth + 1]);
    }
    return best?.id ?? null;
  }

  appendJournal(event, payload) {
    this.database.appendJournal(event, payload);
  }

  journal(limit = 100) {
    return this.database.listJournal(limit);
  }

  #makeCommit({ message, parents, snapshot, diff, metadata }) {
    const createdAt = nowIso();
    const body = {
      formatVersion: FORMAT_VERSION,
      message,
      parents,
      createdAt,
      graphHash: hash(snapshot),
      diff,
      metadata
    };
    return { id: hash(body).slice(0, 16), ...body, snapshot: clone(snapshot) };
  }

  #writeCommit(commit) {
    this.database.saveCommit(commit);
    this.commitCache.set(commit.id, clone(commit));
  }

  #advanceHead(commitId) {
    const branch = this.currentBranch();
    if (branch) this.#writeRef(branch, commitId);
    else this.database.setState('HEAD', commitId);
  }

  #readRef(name) {
    const commitId = this.database.getRef(name);
    if (!commitId) throw new Error(`Unknown branch: ${name}`);
    return commitId;
  }

  #writeRef(name, commitId) {
    validateRefName(name);
    this.database.setRef(name, commitId);
  }

  #ensureGraphCache() {
    const revision = this.database.graphRevision();
    if (this.graphCache && this.graphCacheRevision === revision) {
      this.lastGraphRevision = revision;
      return;
    }
    const snapshot = this.database.loadGraphSnapshot();
    this.graphCache = snapshot.state;
    this.graphCacheRevision = snapshot.revision;
    this.lastGraphRevision = snapshot.revision;
  }

  #migrateFileStoreIfNeeded() {
    if (this.database.initialized()) return;
    const graphFile = path.join(this.dir, 'graph.json');
    if (!fs.existsSync(graphFile)) return;

    const graph = JSON.parse(fs.readFileSync(graphFile, 'utf8'));
    new CognitiveGraph(graph).validate();
    this.database.replaceGraph(graph, { incrementRevision: false });
    const revisionFile = path.join(this.dir, 'graph.revision');
    const revision = fs.existsSync(revisionFile)
      ? Number(fs.readFileSync(revisionFile, 'utf8').trim()) || 1
      : 1;
    this.database.setMeta('graph_revision', String(revision));

    const commitsDir = path.join(this.dir, 'commits');
    if (fs.existsSync(commitsDir)) {
      for (const name of fs.readdirSync(commitsDir).filter((x) => x.endsWith('.json'))) {
        const commit = JSON.parse(fs.readFileSync(path.join(commitsDir, name), 'utf8'));
        this.database.saveCommit(commit);
      }
    }

    const refsDir = path.join(this.dir, 'refs', 'heads');
    if (fs.existsSync(refsDir)) {
      for (const refFile of walkFiles(refsDir)) {
        const name = path.relative(refsDir, refFile).split(path.sep).join('/');
        const value = fs.readFileSync(refFile, 'utf8').trim();
        if (value) this.database.setRef(name, value);
      }
    }

    const headFile = path.join(this.dir, 'HEAD');
    if (fs.existsSync(headFile)) {
      this.database.setState('HEAD', fs.readFileSync(headFile, 'utf8').trim());
    }

    const sessionsDir = path.join(this.dir, 'sessions');
    if (fs.existsSync(sessionsDir)) {
      for (const name of fs.readdirSync(sessionsDir).filter((x) => x.endsWith('.json'))) {
        const session = JSON.parse(fs.readFileSync(path.join(sessionsDir, name), 'utf8'));
        this.database.saveSession(session);
      }
    }

    const journalFile = path.join(this.dir, 'journal.jsonl');
    if (fs.existsSync(journalFile)) {
      for (const line of fs.readFileSync(journalFile, 'utf8').split(/\r?\n/).filter(Boolean)) {
        try {
          const entry = JSON.parse(line);
          const { at, event, ...payload } = entry;
          this.database.appendJournal(event ?? 'legacy', payload, at ?? nowIso());
        } catch {}
      }
    }

    this.database.setMeta('format_version', String(FORMAT_VERSION));
    this.database.setMeta('repository_initialized', '1');
    this.database.setMeta('migrated_from_json_at', nowIso());
    this.lastGraphRevision = this.database.graphRevision();
    this.graphCache = clone(graph);
    this.graphCacheRevision = this.lastGraphRevision;
    this.#archiveLegacyStore();
  }

  #archiveLegacyStore() {
    const names = [
      'graph.json', 'graph.revision', 'config.json', 'HEAD',
      'commits', 'refs', 'sessions', 'journal.jsonl', 'search-index.json'
    ].filter((name) => fs.existsSync(path.join(this.dir, name)));
    if (!names.length) return;
    const backup = path.join(this.dir, `json-backup-${Date.now()}`);
    fs.mkdirSync(backup, { recursive: true });
    for (const name of names) {
      fs.renameSync(path.join(this.dir, name), path.join(backup, name));
    }
  }
}

export function threeWayMergeGraph(base, ours, theirs) {
  const conflicts = [];
  const graph = clone(ours);
  mergeCollection('node', base.nodes ?? {}, ours.nodes ?? {}, theirs.nodes ?? {}, graph.nodes, conflicts);
  mergeCollection('edge', base.edges ?? {}, ours.edges ?? {}, theirs.edges ?? {}, graph.edges, conflicts);
  if (!conflicts.length) new CognitiveGraph(graph).validate();
  return { graph, conflicts };
}

function mergeCollection(kind, base, ours, theirs, target, conflicts) {
  const ids = new Set([...Object.keys(base), ...Object.keys(ours), ...Object.keys(theirs)]);
  for (const id of ids) {
    const b = base[id] ?? null;
    const o = ours[id] ?? null;
    const t = theirs[id] ?? null;
    if (isEqual(o, t)) continue;
    if (isEqual(o, b)) {
      if (t === null) delete target[id];
      else target[id] = clone(t);
      continue;
    }
    if (isEqual(t, b)) continue;
    conflicts.push({ kind, id, base: clone(b), ours: clone(o), theirs: clone(t) });
  }
}

function validateRefName(name) {
  if (!/^[A-Za-z0-9._/-]+$/.test(name) || name.includes('..') || name.startsWith('/') || name.endsWith('/')) {
    throw new Error(`Invalid branch name: ${name}`);
  }
}


function walkFiles(root) {
  const result = [];
  for (const entry of fs.readdirSync(root, { withFileTypes: true })) {
    const absolute = path.join(root, entry.name);
    if (entry.isDirectory()) result.push(...walkFiles(absolute));
    else if (entry.isFile()) result.push(absolute);
  }
  return result;
}
