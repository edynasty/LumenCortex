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

const DEFAUL_IGNORES = new Set([
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
      if (pkg) javaByQualifiedName.set(`${peg}.${className}`, { rel, fileId, content });
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
