import fs from 'node:fs';
import path from 'node:path';
import { applyDiff, CognitiveGraph, diffGraphs, emptyGraph, invertDiff } from './graph.js';
import { clone, hash, isEqual, nowIso } from './util.js';

const FORMAT_VERSION = 1;

export class CognitiveRepository {
  constructor(workspace = process.cwd()) {
    this.workspace = path.resolve(workspace);
    this.dir = path.join(this.workspace, '.modelweave');
  }

  init({ branch = 'main' } = {}) {
    if (this.exists()) throw new Error(`ModelWeave repository already exists: ${this.dir}`);
    fs.mkdirSync(path.join(this.dir, 'commits'), { recursive: true });
    fs.mkdirSync(path.join(this.dir, 'refs', 'heads'), { recursive: true });
    const graph = emptyGraph();
    const commit = this.#makeCommit({
      message: 'Initialize cognitive repository',
      parents: [],
      snapshot: graph,
      diff: { operations: [] },
      metadata: { genesis: true }
    });
    this.#writeCommit(commit);
    this.#writeRef(branch, commit.id);
    fs.writeFileSync(path.join(this.dir, 'HEAD'), `ref: refs/heads/${branch}\n`);
    this.#writeJson('graph.json', graph);
    this.#writeJson('config.json', { formatVersion: FORMAT_VERSION, createdAt: nowIso() });
    return commit;
  }

  exists() {
    return fs.existsSync(this.dir);
  }

  assertExists() {
    if (!this.exists()) throw new Error(`Not a ModelWeave repository: ${this.workspace}`);
  }

  graph() {
    this.assertExists();
    return new CognitiveGraph(this.#readJson('graph.json'));
  }

  writeGraph(state) {
    this.assertExists();
    new CognitiveGraph(state).validate();
    this.#writeJson('graph.json', state);
  }

  headRef() {
    this.assertExists();
    return fs.readFileSync(path.join(this.dir, 'HEAD'), 'utf8').trim();
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
    this.assertExists();
    const file = path.join(this.dir, 'commits', `${commitId}.json`);
    if (!fs.existsSync(file)) throw new Error(`Unknown cognitive commit: ${commitId}`);
    return JSON.parse(fs.readFileSync(file, 'utf8'));
  }

  status() {
    const head = this.headCommit();
    const working = this.graph().snapshot();
    return diffGraphs(head.snapshot, working);
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

  blame(objectId, limit = 50) {
    const result = [];
    for (const commit of this.log(Math.max(limit * 4, 100))) {
      const operations = (commit.diff?.operations ?? []).filter((op) => op.id === objectId);
      if (!operations.length) continue;
      result.push({
        commitId: commit.id,
        createdAt: commit.createdAt,
        message: commit.message,
        metadata: commit.metadata,
        operations: clone(operations)
      });
      if (result.length >= limit) break;
    }
    return result;
  }

  branches() {
    this.assertExists();
    const dir = path.join(this.dir, 'refs', 'heads');
    return fs.readdirSync(dir).sort().map((name) => ({ name, commitId: this.#readRef(name), current: name === this.currentBranch() }));
  }

  createBranch(name, startPoint = this.headCommitId()) {
    validateRefName(name);
    const file = path.join(this.dir, 'refs', 'heads', name);
    if (fs.existsSync(file)) throw new Error(`branch already exists: ${name}`);
    this.getCommit(startPoint);
    this.#writeRef(name, startPoint);
    return { name, commitId: startPoint };
  }

  checkout(name) {
    validateRefName(name);
    const commitId = this.#readRef(name);
    const commit = this.getCommit(commitId);
    fs.writeFileSync(path.join(this.dir, 'HEAD'), `ref: refs/heads/${name}\n`);
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
        metadata: { cherryPickOf: commitId }
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

  cherryPick(commitId, { message } = {}) {
    const target = this.getCommit(commitId);
    if (!target.parents?.length) throw new Error('Cannot cherry-pick genesis commit');
    const working = this.graph().snapshot();
    try {
      const next = applyDiff(working, target.diff, { strict: true });
      this.writeGraph(next);
    } catch (error) {
      return { conflicts: [{ commitId, message: error.message }], commit: null };
    }
    const commit = this.commit(message ?? `Cherry-pick ${commitId}: ${target.message}`, {
      metadata: { cherryPick: commitId, cherryPickOf: commitId }
    });
    return { conflicts: [], commit };
  }

  blame(nodeId, options = {}) {
    return this.blameNode(nodeId, options);
  }

  blameNode(nodeId, { limit = 200 } = {}) {
    const result = [];
    const queue = [this.headCommitId()];
    const seen = new Set();
    while (queue.length && seen.size < limit) {
      const commitId = queue.shift();
      if (!commitId || seen.has(commitId)) continue;
      seen.add(commitId);
      const commit = this.getCommit(commitId);
      const changes = (commit.diff?.operations ?? []).filter(
        (op) => op.id === nodeId && (op.type === 'put_node' || op.type === 'remove_node')
      );
      if (changes.length) {
        result.push({
          commitId: commit.id,
          message: commit.message,
          createdAt: commit.createdAt,
          parents: commit.parents,
          changes: clone(changes)
        });
      }
      for (const parent of commit.parents ?? []) queue.push(parent);
    }
    return result;
  }

  rebase(ontoBranch, { messagePrefix = 'Rebase' } = {}) {
    const branch = this.currentBranch();
    if (!branch) throw new Error('Cannot rebase detached HEAD');
    if (branch === ontoBranch) return { conflicts: [], commits: [], onto: this.headCommitId(), branch };

    const originalHeadId = this.headCommitId();
    const originalHead = this.getCommit(originalHeadId);
    const ontoId = this.#readRef(ontoBranch);
    const onto = this.getCommit(ontoId);
    const baseId = this.findMergeBase(originalHeadId, ontoId);
    if (!baseId) throw new Error('No merge base found');

    const toReplay = [];
    let cursor = originalHeadId;
    const visited = new Set();
    while (cursor && cursor !== baseId && !visited.has(cursor)) {
      visited.add(cursor);
      const commit = this.getCommit(cursor);
      toReplay.push(commit);
      cursor = commit.parents?.[0] ?? null;
    }
    toReplay.reverse();

    const originalSnapshot = this.graph().snapshot();
    const created = [];
    this.#writeRef(branch, ontoId);
    this.writeGraph(onto.snapshot);

    try {
      for (const source of toReplay) {
        const working = this.graph().snapshot();
        let next;
        try {
          next = applyDiff(working, source.diff, { strict: true });
        } catch (error) {
          throw Object.assign(new Error(error.message), { sourceCommit: source.id });
        }
        this.writeGraph(next);
        const diff = this.status();
        if (!diff.operations.length) continue;
        const commit = this.commit(`${messagePrefix}: ${source.message}`, {
          metadata: { rebaseOf: source.id, onto: ontoId, originalHead: originalHeadId }
        });
        created.push(commit);
      }
      return { conflicts: [], commits: created, onto: ontoId, originalHead: originalHeadId, branch };
    } catch (error) {
      this.#writeRef(branch, originalHeadId);
      this.writeGraph(originalSnapshot);
      return {
        conflicts: [{ commitId: error.sourceCommit ?? originalHeadId, message: error.message }],
        commits: [],
        onto: ontoId,
        originalHead: originalHeadId,
        branch
      };
    }
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

  #makeCommit({ message, parents, snapshot, diff, metadata }) {
    const createdAt = nowIso();
    const body = {
      formatVersion: FORMAT_VERSION,
      message,
      parents,
      createdAt,
      graphHash: hash(snapshot),
      diff,
      metadata,
      snapshot: clone(snapshot)
    };
    return { id: hash(body).slice(0, 16), ...body };
  }

  #writeCommit(commit) {
    fs.writeFileSync(path.join(this.dir, 'commits', `${commit.id}.json`), JSON.stringify(commit, null, 2));
  }

  #advanceHead(commitId) {
    const branch = this.currentBranch();
    if (branch) this.#writeRef(branch, commitId);
    else fs.writeFileSync(path.join(this.dir, 'HEAD'), `${commitId}\n`);
  }

  #readRef(name) {
    const file = path.join(this.dir, 'refs', 'heads', name);
    if (!fs.existsSync(file)) throw new Error(`Unknown branch: ${name}`);
    return fs.readFileSync(file, 'utf8').trim();
  }

  #writeRef(name, commitId) {
    validateRefName(name);
    fs.writeFileSync(path.join(this.dir, 'refs', 'heads', name), `${commitId}\n`);
  }

  #readJson(name) {
    return JSON.parse(fs.readFileSync(path.join(this.dir, name), 'utf8'));
  }

  #writeJson(name, value) {
    fs.writeFileSync(path.join(this.dir, name), JSON.stringify(value, null, 2));
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
