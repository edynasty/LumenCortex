import fs from 'node:fs';
import path from 'node:path';
import { execFileSync } from 'node:child_process';
import { CognitiveGraph } from './graph.js';
import { hash, nowIso } from './util.js';

const DEFAULT_EXTENSIONS = new Set([
  '.js', '.jsx', '.ts', '.tsx', '.mjs', '.cjs',
  '.java', '.kt', '.kts', '.py', '.go', '.rs', '.sql',
  '.md', '.json', '.yaml', '.yml', '.xml', '.properties', '.toml',
  '.sh', '.css', '.scss', '.html', '.vue', '.svelte'
]);

const DEFAULT_IGNORES = new Set([
  '.git', '.modelweave', 'node_modules', 'dist', 'build', 'target', '.next', '.idea', '.vscode', 'coverage', 'vendor'
]);

export function ingestWorkspace(graphState, root, options = {}) {
  const sourceRoot = path.resolve(root);
  const graph = new CognitiveGraph(graphState);
  const extensions = new Set(options.extensions ?? DEFAULT_EXTENSIONS);
  const ignores = new Set([...(options.ignores ?? []), ...DEFAULT_IGNORES]);
  const maxFileBytes = options.maxFileBytes ?? 512 * 1024;
  const chunkLines = options.chunkLines ?? 160;
  const sourceVersion = options.sourceVersion ?? gitHead(sourceRoot);
  const files = scanFiles(sourceRoot, { extensions, ignores, maxFileBytes });
  const seen = new Set();
  const changedEvidence = new Set();
  const fileByRel = new Map();
  const javaByQualifiedName = new Map();

  const rootId = directoryId('.');
  upsertNode(graph, {
    id: rootId,
    kind: 'abstraction',
    title: path.basename(sourceRoot),
    body: `Repository root: ${sourceRoot}`,
    grade: 'static',
    trustZone: 'repo_trusted',
    metadata: { sourceKind: 'directory', path: '.', ingestRoot: sourceRoot, repositoryVersion: sourceVersion }
  });
  seen.add(rootId);

  for (const file of files) {
    const rel = normalize(path.relative(sourceRoot, file));
    const fileId = stableId('file', rel);
    fileByRel.set(rel, fileId);
    const content = fs.readFileSync(file, 'utf8');
    const lines = content.split(/\r?\n/);
    const contentHash = hash(content);
    const previousFile = graph.getNode(fileId);
    upsertNode(graph, {
      id: fileId,
      kind: 'entity',
      title: rel,
      body: `${lines.length} lines, ${Buffer.byteLength(content)} bytes`,
      tags: ['file', path.extname(rel).slice(1)],
      grade: 'static',
      trustZone: 'repo_trusted',
      source: { uri: `file://${rel}` },
      contentHash,
      sourceVersion: contentHash,
      observedAt: previousFile?.observedAt ?? nowIso(),
      metadata: { sourceKind: 'file', path: rel, ingestRoot: sourceRoot, lineCount: lines.length }
    });
    seen.add(fileId);

    if (previousFile?.contentHash && previousFile.contentHash !== contentHash) changedEvidence.add(fileId);

    const dirRel = normalize(path.dirname(rel));
    ensureDirectoryChain(graph, sourceRoot, dirRel, rootId, seen, sourceVersion);
    const parentDirId = directoryId(dirRel === '' ? '.' : dirRel);
    ensureEdge(graph, stableId('edge', `${parentDirId}:abstracts:${fileId}`), parentDirId, fileId, 'abstracts', 0.95, { structural: true });

    const chunks = chunkText(lines, chunkLines);
    for (const chunk of chunks) {
      const chunkId = stableId('chunk', `${rel}:${chunk.start}:${chunk.end}`);
      const previous = graph.getNode(chunkId);
      const chunkHash = hash(chunk.text);
      upsertNode(graph, {
        id: chunkId,
        kind: 'evidence',
        title: `${rel}:L${chunk.start}-L${chunk.end}`,
        body: chunk.text,
        tags: ['code-chunk', path.extname(rel).slice(1)],
        grade: 'static',
        trustZone: 'repo_trusted',
        source: { uri: `file://${rel}#L${chunk.start}-L${chunk.end}` },
        contentHash: chunkHash,
        sourceVersion: chunkHash,
        observedAt: previous?.observedAt ?? nowIso(),
        metadata: {
          sourceKind: 'file-chunk',
          path: rel,
          startLine: chunk.start,
          endLine: chunk.end,
          ingestRoot: sourceRoot
        }
      });
      seen.add(chunkId);
      ensureEdge(graph, stableId('edge', `${fileId}:abstracts:${chunkId}`), fileId, chunkId, 'abstracts', 1, { structural: true });
      if (previous?.contentHash && previous.contentHash !== chunkHash) changedEvidence.add(chunkId);
    }

    if (path.extname(rel) === '.java') {
      const pkg = content.match(/^\s*package\s+([\w.]+)\s*;/m)?.[1];
      const className = path.basename(rel, '.java');
      if (pkg) javaByQualifiedName.set(`${pkg}.${className}`, { rel, fileId, content });
    }
  }

  // Static dependency hints are deliberately low-cost graph edges, not beliefs.
  for (const file of files) {
    const rel = normalize(path.relative(sourceRoot, file));
    const fileId = fileByRel.get(rel);
    const content = fs.readFileSync(file, 'utf8');
    for (const depRel of resolveDependencies(rel, content, fileByRel, javaByQualifiedName)) {
      const depId = fileByRel.get(depRel);
      if (!depId || depId === fileId) continue;
      ensureEdge(graph, stableId('dep', `${fileId}:${depId}`), fileId, depId, 'depends_on', 0.85, {
        structural: true,
        discoveredBy: 'static-import-scan'
      });
    }
  }

  const archived = [];
  for (const node of Object.values(graph.state.nodes)) {
    if (node.metadata?.ingestRoot !== sourceRoot) continue;
    if (seen.has(node.id)) continue;
    if (node.status !== 'archived') {
      graph.updateNode(node.id, { status: 'archived', metadata: { archivedReason: 'source-missing' } });
      archived.push(node.id);
    }
  }

  const dirtied = dirtyDependents(graph, changedEvidence);
  return {
    graph: graph.snapshot(),
    stats: {
      root: sourceRoot,
      sourceVersion,
      files: files.length,
      nodesSeen: seen.size,
      changedEvidence: changedEvidence.size,
      dirtiedBeliefs: dirtied.length,
      archived: archived.length
    },
    changedEvidence: [...changedEvidence],
    dirtiedBeliefs: dirtied,
    archived
  };
}

function scanFiles(root, { extensions, ignores, maxFileBytes }) {
  const out = [];
  const stack = [root];
  while (stack.length) {
    const dir = stack.pop();
    for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
      if (entry.name.startsWith('.') && entry.name !== '.github') {
        if (ignores.has(entry.name) || entry.isDirectory()) continue;
      }
      if (ignores.has(entry.name)) continue;
      const full = path.join(dir, entry.name);
      if (entry.isDirectory()) {
        stack.push(full);
        continue;
      }
      if (!entry.isFile()) continue;
      const ext = path.extname(entry.name).toLowerCase();
      if (!extensions.has(ext)) continue;
      const stat = fs.statSync(full);
      if (stat.size > maxFileBytes) continue;
      if (looksBinary(full)) continue;
      out.push(full);
    }
  }
  return out.sort();
}

function looksBinary(file) {
  const fd = fs.openSync(file, 'r');
  try {
    const buffer = Buffer.alloc(4096);
    const read = fs.readSync(fd, buffer, 0, buffer.length, 0);
    for (let i = 0; i < read; i += 1) if (buffer[i] === 0) return true;
    return false;
  } finally {
    fs.closeSync(fd);
  }
}

function chunkText(lines, size) {
  const result = [];
  for (let start = 0; start < lines.length; start += size) {
    const slice = lines.slice(start, Math.min(lines.length, start + size));
    result.push({ start: start + 1, end: start + slice.length, text: slice.join('\n') });
  }
  return result;
}

function ensureDirectoryChain(graph, sourceRoot, dirRel, rootId, seen, sourceVersion) {
  if (!dirRel || dirRel === '.') return;
  const parts = dirRel.split('/').filter(Boolean);
  let current = '';
  let parentId = rootId;
  for (const part of parts) {
    current = current ? `${current}/${part}` : part;
    const currentId = directoryId(current);
    upsertNode(graph, {
      id: currentId,
      kind: 'abstraction',
      title: current,
      body: `Directory ${current}`,
      grade: 'static',
      trustZone: 'repo_trusted',
      metadata: { sourceKind: 'directory', path: current, ingestRoot: sourceRoot }
    });
    seen.add(currentId);
    ensureEdge(graph, stableId('edge', `${parentId}:abstracts:${currentId}`), parentId, currentId, 'abstracts', 0.9, { structural: true });
    parentId = currentId;
  }
}

function resolveDependencies(rel, content, fileByRel, javaByQualifiedName) {
  const result = new Set();
  const ext = path.extname(rel);
  if (['.js', '.jsx', '.ts', '.tsx', '.mjs', '.cjs'].includes(ext)) {
    const imports = [
      ...content.matchAll(/(?:from\s+|require\s*\()\s*['\"](\.[^'\"]+)['\"]/g),
      ...content.matchAll(/import\s*\(\s*['\"](\.[^'\"]+)['\"]\s*\)/g)
    ].map((m) => m[1]);
    for (const specifier of imports) {
      const resolved = resolveRelativeModule(rel, specifier, fileByRel);
      if (resolved) result.add(resolved);
    }
  }
  if (ext === '.java') {
    for (const match of content.matchAll(/^\s*import\s+([\w.]+)\s*;/gm)) {
      const target = javaByQualifiedName.get(match[1]);
      if (target) result.add(target.rel);
    }
  }
  return [...result];
}

function resolveRelativeModule(fromRel, specifier, fileByRel) {
  const base = normalize(path.posix.normalize(path.posix.join(path.posix.dirname(fromRel), specifier)));
  const candidates = [
    base,
    ...['.ts', '.tsx', '.js', '.jsx', '.mjs', '.cjs', '.json'].map((ext) => `${base}${ext}`),
    ...['index.ts', 'index.tsx', 'index.js', 'index.jsx'].map((name) => `${base}/${name}`)
  ];
  return candidates.find((candidate) => fileByRel.has(candidate)) ?? null;
}

function dirtyDependents(graph, changedEvidence) {
  if (!changedEvidence.size) return [];
  const dirtied = [];
  for (const node of Object.values(graph.state.nodes)) {
    if (!['belief', 'negative', 'abstraction'].includes(node.kind)) continue;
    const evidenceHit = (node.evidenceIds ?? []).some((id) => changedEvidence.has(id));
    const childHit = (node.childIds ?? []).some((id) => changedEvidence.has(id));
    if ((evidenceHit || childHit) && node.status !== 'stale') {
      graph.updateNode(node.id, { status: 'stale', metadata: { staleReason: 'ingested-source-changed' } });
      dirtied.push(node.id);
    }
  }
  return dirtied;
}

function upsertNode(graph, input) {
  const current = graph.getNode(input.id);
  if (!current) return graph.addNode(input);
  // Keep exact versions stable when source content has not changed.
  const comparableCurrent = { ...current, updatedAt: undefined, version: undefined, createdAt: undefined };
  const candidate = { ...current, ...input, updatedAt: undefined, version: undefined, createdAt: undefined };
  if (JSON.stringify(comparableCurrent) === JSON.stringify(candidate)) return current;
  return graph.putNode({ ...current, ...input });
}

function ensureEdge(graph, edgeId, from, to, type, weight, metadata) {
  const current = graph.getEdge(edgeId);
  if (current) return current;
  return graph.addEdge({ id: edgeId, from, to, type, weight, metadata });
}

function stableId(prefix, key) {
  return `${prefix}_${hash(key).slice(0, 14)}`;
}

function directoryId(rel) {
  return stableId('dir', rel || '.');
}

function normalize(value) {
  return value.split(path.sep).join('/').replace(/^\.\//, '');
}

function gitHead(root) {
  try {
    return execFileSync('git', ['rev-parse', 'HEAD'], { cwd: root, encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'] }).trim();
  } catch {
    return null;
  }
}
