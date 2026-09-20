import fs from 'node:fs';
import path from 'node:path';
import { hash, tokenize } from './util.js';

const INDEX_VERSION = 1;

export class PersistentSearchIndex {
  constructor(repositoryDir) {
    this.file = path.join(repositoryDir, 'search-index.json');
    this.state = null;
    this.load();
  }

  load() {
    if (!fs.existsSync(this.file)) return null;
    try {
      const parsed = JSON.parse(fs.readFileSync(this.file, 'utf8'));
      if (parsed.version !== INDEX_VERSION) return null;
      this.state = parsed;
      return parsed;
    } catch {
      this.state = null;
      return null;
    }
  }

  ready() {
    return Boolean(this.state?.documents && this.state?.postings);
  }

  build(graphState, options = {}) {
    const documents = {};
    const postings = {};
    const symbols = {};
    let totalLength = 0;

    for (const node of Object.values(graphState.nodes ?? {})) {
      if (node.status === 'archived' || node.status === 'invalid') continue;
      const text = searchableText(node);
      const terms = indexTerms(text);
      if (!terms.length) continue;
      const frequencies = frequencyMap(terms);
      const doc = {
        id: node.id,
        length: terms.length,
        title: node.title ?? '',
        path: node.metadata?.path ?? sourcePath(node),
        kind: node.kind,
        sourceKind: node.metadata?.sourceKind ?? null,
        contentHash: node.contentHash ?? null
      };
      documents[node.id] = doc;
      totalLength += terms.length;

      for (const [term, tf] of frequencies) {
        (postings[term] ??= []).push([node.id, tf]);
      }

      for (const symbol of extractSymbols(node)) {
        (symbols[symbol] ??= []).push(node.id);
      }
    }

    const documentCount = Object.keys(documents).length;
    this.state = {
      version: INDEX_VERSION,
      createdAt: new Date().toISOString(),
      graphFingerprint: graphFingerprint(graphState),
      graphRevision: options.graphRevision ?? null,
      documentCount,
      averageLength: documentCount ? totalLength / documentCount : 0,
      documents,
      postings,
      symbols
    };
    fs.mkdirSync(path.dirname(this.file), { recursive: true });
    const tmp = `${this.file}.tmp`;
    fs.writeFileSync(tmp, JSON.stringify(this.state));
    fs.renameSync(tmp, this.file);
    return this.stats();
  }

  search(query, options = {}) {
    if (!this.ready()) return [];
    const limit = Math.max(1, Number(options.limit ?? 50));
    const k1 = Number(options.k1 ?? 1.35);
    const b = Number(options.b ?? 0.72);
    const maxPostingRatio = Math.max(0.001, Math.min(1, Number(options.maxPostingRatio ?? 0.08)));
    const terms = [...new Set(indexTerms(query))];
    const scores = new Map();
    const reasons = new Map();
    const N = this.state.documentCount || 1;
    const avgdl = this.state.averageLength || 1;

    let symbolHitCount = 0;
    for (const raw of identifierTerms(query)) {
      const exact = this.state.symbols[raw] ?? [];
      for (const id of exact) {
        scores.set(id, (scores.get(id) ?? 0) + 8);
        addReason(reasons, id, `symbol:${raw}`);
        symbolHitCount += 1;
      }
      const lower = raw.toLowerCase();
      if (lower !== raw) {
        for (const id of this.state.symbols[lower] ?? []) {
          scores.set(id, (scores.get(id) ?? 0) + 5);
          addReason(reasons, id, `symbol-ci:${raw}`);
          symbolHitCount += 1;
        }
      }
    }

    const postingTerms = terms
      .map((term) => ({ term, list: this.state.postings[term] ?? [] }))
      .filter((entry) => entry.list.length);
    const selective = postingTerms.filter((entry) => entry.list.length / N <= maxPostingRatio);
    const plannedTerms = selective.length || symbolHitCount
      ? selective
      : postingTerms;

    for (const { term, list } of plannedTerms) {
      const df = list.length;
      const idf = Math.log(1 + (N - df + 0.5) / (df + 0.5));
      for (const [id, tf] of list) {
        const dl = this.state.documents[id]?.length ?? avgdl;
        const score = idf * ((tf * (k1 + 1)) / (tf + k1 * (1 - b + b * dl / avgdl)));
        scores.set(id, (scores.get(id) ?? 0) + score);
        addReason(reasons, id, `bm25:${term}`);
      }
    }

    const queryLower = String(query).toLowerCase();
    for (const id of scores.keys()) {
      const doc = this.state.documents[id];
      if (doc?.path && queryLower.includes(String(doc.path).toLowerCase())) {
        scores.set(id, scores.get(id) + 4);
        addReason(reasons, id, 'path');
      }
    }

    return [...scores.entries()]
      .map(([nodeId, score]) => ({
        nodeId,
        score,
        reasons: [...(reasons.get(nodeId) ?? [])],
        ...this.state.documents[nodeId]
      }))
      .sort((a, b) => b.score - a.score || a.nodeId.localeCompare(b.nodeId))
      .slice(0, limit);
  }

  stats() {
    if (!this.ready()) return { ready: false, file: this.file };
    return {
      ready: true,
      file: this.file,
      documentCount: this.state.documentCount,
      termCount: Object.keys(this.state.postings).length,
      symbolCount: Object.keys(this.state.symbols).length,
      createdAt: this.state.createdAt,
      graphFingerprint: this.state.graphFingerprint,
      graphRevision: this.state.graphRevision ?? null
    };
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

function searchableText(node) {
  return [
    node.title,
    node.body,
    ...(node.tags ?? []),
    node.source?.uri ?? '',
    node.metadata?.path ?? '',
    node.metadata?.sourceKind ?? ''
  ].filter(Boolean).join('\n');
}

function indexTerms(text) {
  const base = tokenize(text);
  const identifiers = identifierTerms(text).flatMap(splitIdentifier);
  return [...base, ...identifiers.map((x) => x.toLowerCase())].filter((x) => x.length > 1);
}

function identifierTerms(text) {
  return String(text).match(/[A-Za-z_$][A-Za-z0-9_$]{2,}/g) ?? [];
}

function splitIdentifier(value) {
  return String(value)
    .replace(/([a-z0-9])([A-Z])/g, '$1 $2')
    .replace(/[_$.-]+/g, ' ')
    .split(/\s+/)
    .filter(Boolean);
}

function frequencyMap(values) {
  const map = new Map();
  for (const value of values) map.set(value, (map.get(value) ?? 0) + 1);
  return map;
}

function sourcePath(node) {
  const uri = node.source?.uri;
  if (!uri?.startsWith('file://')) return '';
  return uri.slice('file://'.length).split('#')[0];
}

function addReason(map, id, reason) {
  if (!map.has(id)) map.set(id, new Set());
  map.get(id).add(reason);
}
