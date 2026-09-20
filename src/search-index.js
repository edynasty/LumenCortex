import path from 'node:path';
import { hash, tokenize } from './util.js';
import { LumenCortexDatabase } from './database.js';

export class PersistentSearchIndex {
  constructor(repositoryDir) {
    this.database = new LumenCortexDatabase(repositoryDir);
    this.file = this.database.file;
    this.state = null;
    this.load();
  }

  load() {
    const stats = this.database.searchStats();
    this.state = {
      graphRevision: stats.graphRevision,
      createdAt: stats.createdAt
    };
    return stats.ready ? this.state : null;
  }

  ready() {
    return this.database.searchIndexReady();
  }

  build(graphState, options = {}) {
    const stats = this.database.rebuildSearchIndex(graphState, {
      graphRevision: options.graphRevision ?? null,
      extractSymbols,
      searchableText,
      indexTerms
    });
    this.state = {
      graphRevision: stats.graphRevision,
      createdAt: stats.createdAt
    };
    return stats;
  }

  search(query, options = {}) {
    if (!this.ready()) return [];
    return this.database.search(query, {
      limit: options.limit ?? 50,
      identifierTerms,
      indexTerms
    });
  }

  stats() {
    return this.database.searchStats();
  }
}

export function graphFingerprint(graphState) {
  const signature = Object.values(graphState.nodes ?? {})
    .map((node) => [node.id, node.version ?? 0, node.contentHash ?? node.sourceVersion ?? '', node.status ?? 'active'])
    .sort((a, b) => a[0].localeCompare(b[0]));
  return hash(signature);
}

export function extractSymbols(node) {
  const body = String(node.body ?? '');
  const pathName = node.metadata?.path ?? '';
  const ext = path.extname(pathName).toLowerCase();
  const found = new Set();

  const patterns = [
    /\b(?:class|interface|enum|record|trait|struct|type)\s+([A-Za-z_$][\w$]*)/g,
    /\b(?:function|def|func)\s+([A-Za-z_$][\w$]*)\s*\(/g,
    /\b(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*(?:=|:)/g,
    /\b(?:public|private|protected|static|final|synchronized|abstract|native|default|async|export|override|suspend|open|internal|fun|void|int|long|double|float|boolean|String|[A-Z][\w<>?, .\[\]]*)\s+([A-Za-z_$][\w$]*)\s*\(/g
  ];
  if (ext === '.sql') patterns.push(/\b(?:table|view|procedure|function)\s+([A-Za-z_][\w$.]*)/gi);

  for (const pattern of patterns) {
    for (const match of body.matchAll(pattern)) {
      found.add(match[1]);
      found.add(match[1].toLowerCase());
    }
  }
  return [...found];
}

export function searchableText(node) {
  return [
    node.title,
    node.body,
    ...(node.tags ?? []),
    node.source?.uri ?? '',
    node.metadata?.path ?? '',
    node.metadata?.sourceKind ?? ''
  ].filter(Boolean).join('\n');
}

export function indexTerms(text) {
  const base = tokenize(text);
  const identifiers = identifierTerms(text).flatMap(splitIdentifier);
  return [...base, ...identifiers.map((x) => x.toLowerCase())].filter((x) => x.length > 1);
}

export function identifierTerms(text) {
  return String(text).match(/[A-Za-z_$][A-Za-z0-9_$]{2,}/g) ?? [];
}

function splitIdentifier(value) {
  return String(value)
    .replace(/([a-z0-9])([A-Z])/g, '$1 $2')
    .replace(/[_$.-]+/g, ' ')
    .split(/\s+/)
    .filter(Boolean);
}
