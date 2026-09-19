import {
  EDGE_TYPES,
  EVIDENCE_GRADES,
  NODE_KINDS,
  NODE_STATUSES,
  TRUST_ZONES
} from './constants.js';
import { id, nowIso } from './util.js';

export function createNode(input) {
  if (!NODE_KINDS.has(input.kind)) throw new Error(`Unsupported node kind: ${input.kind}`);
  const timestamp = nowIso();
  const node = {
    id: input.id ?? id('node'),
    kind: input.kind,
    title: input.title?.trim() || input.kind,
    body: input.body ?? '',
    tags: [...new Set(input.tags ?? [])],
    status: input.status ?? 'active',
    trustZone: input.trustZone ?? inferTrustZone(input.kind),
    grade: input.grade ?? inferGrade(input.kind),
    createdAt: input.createdAt ?? timestamp,
    updatedAt: input.updatedAt ?? timestamp,
    version: input.version ?? 1,
    metadata: input.metadata ?? {}
  };
  if (!NODE_STATUSES.includes(node.status)) throw new Error(`Unsupported node status: ${node.status}`);
  if (!TRUST_ZONES.includes(node.trustZone)) throw new Error(`Unsupported trust zone: ${node.trustZone}`);
  if (!EVIDENCE_GRADES.includes(node.grade)) throw new Error(`Unsupported evidence grade: ${node.grade}`);

  if (input.evidenceIds) node.evidenceIds = [...new Set(input.evidenceIds)];
  if (input.childIds) node.childIds = [...new Set(input.childIds)];
  if (input.unresolved) node.unresolved = [...input.unresolved];
  if (input.source) node.source = { ...input.source };
  if (input.observedAt) node.observedAt = input.observedAt;
  if (input.ttlMs !== undefined) node.ttlMs = input.ttlMs;
  if (input.contentHash) node.contentHash = input.contentHash;
  if (input.sourceVersion) node.sourceVersion = input.sourceVersion;
  if (input.validity) node.validity = { ...input.validity };
  return node;
}

export function createEdge(input) {
  if (!EDGE_TYPES.has(input.type)) throw new Error(`Unsupported edge type: ${input.type}`);
  return {
    id: input.id ?? id('edge'),
    from: input.from,
    to: input.to,
    type: input.type,
    weight: input.weight ?? 1,
    createdAt: input.createdAt ?? nowIso(),
    metadata: input.metadata ?? {}
  };
}

function inferTrustZone(kind) {
  if (kind === 'evidence') return 'repo_trusted';
  if (kind === 'belief' || kind === 'abstraction' || kind === 'negative') return 'model_inferred';
  return 'user_provided';
}

function inferGrade(kind) {
  return kind === 'evidence' ? 'static' : 'hypothesis';
}
