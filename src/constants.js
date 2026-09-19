export const NODE_KINDS = new Set([
  'entity',
  'evidence',
  'belief',
  'abstraction',
  'task',
  'negative'
]);

export const EDGE_TYPES = new Set([
  'depends_on',
  'derived_from',
  'relates_to',
  'supersedes',
  'abstracts',
  'invalidates',
  'contradicts',
  'verifies',
  'causes',
  'calls',
  'affects'
]);

export const EVIDENCE_GRADES = [
  'hypothesis',
  'static',
  'tested',
  'runtime',
  'reproduced'
];

export const TRUST_ZONES = [
  'system_verified',
  'repo_trusted',
  'runtime_verified',
  'user_provided',
  'external_untrusted',
  'model_inferred'
];

export const NODE_STATUSES = [
  'active',
  'dormant',
  'stale',
  'archived',
  'invalid'
];

export const DEFAULT_EDGE_WEIGHTS = {
  causes: 1.0,
  verifies: 0.95,
  depends_on: 0.9,
  calls: 0.85,
  derived_from: 0.85,
  affects: 0.8,
  abstracts: 0.75,
  contradicts: 0.7,
  supersedes: 0.65,
  invalidates: 0.65,
  relates_to: 0.45
};

export const GRADE_WEIGHTS = {
  hypothesis: 0.45,
  static: 0.65,
  tested: 0.82,
  runtime: 0.92,
  reproduced: 1.0
};

export const TRUST_WEIGHTS = {
  system_verified: 1.0,
  repo_trusted: 0.95,
  runtime_verified: 0.95,
  user_provided: 0.85,
  external_untrusted: 0.55,
  model_inferred: 0.5
};
