import { CognitiveGraph } from './graph.js';
import { hash, nowIso } from './util.js';
import { propagateStaleDependents } from './invalidation.js';

export function auditEvidence(graphState, now = Date.now()) {
  const graph = new CognitiveGraph(graphState);
  const staleEvidence = [];
  const dirtiedBeliefs = [];

  for (const node of Object.values(graph.state.nodes)) {
    if (node.kind !== 'evidence') continue;
    if (node.status === 'invalid' || node.status === 'archived') continue;
    if (node.observedAt && node.ttlMs !== undefined && node.ttlMs !== null) {
      const expired = now - new Date(node.observedAt).getTime() > node.ttlMs;
      if (expired && node.status !== 'stale') {
        graph.updateNode(node.id, { status: 'stale', metadata: { staleReason: 'ttl-expired' } });
        staleEvidence.push(node.id);
      }
    }
  }

  if (staleEvidence.length) {
    dirtiedBeliefs.push(...propagateStaleDependents(graph, staleEvidence, {
      reason: 'evidence-stale'
    }));
  }

  return { graph: graph.snapshot(), staleEvidence, dirtiedBeliefs };
}

export function refreshEvidence(graphState, evidenceId, observation) {
  const graph = new CognitiveGraph(graphState);
  const node = graph.requireNode(evidenceId);
  if (node.kind !== 'evidence') throw new Error(`${evidenceId} is not an evidence node`);

  const contentHash = observation.contentHash ?? hash(observation.content ?? '');
  const changed = Boolean(node.contentHash && node.contentHash !== contentHash);
  graph.updateNode(evidenceId, {
    body: observation.body ?? node.body,
    contentHash,
    sourceVersion: observation.sourceVersion ?? node.sourceVersion,
    observedAt: observation.observedAt ?? nowIso(),
    ttlMs: observation.ttlMs ?? node.ttlMs,
    status: 'active',
    grade: observation.grade ?? node.grade,
    trustZone: observation.trustZone ?? node.trustZone,
    metadata: {
      refreshed: true,
      contentChanged: changed,
      ...(observation.metadata ?? {})
    }
  });

  const invalidated = changed
    ? propagateStaleDependents(graph, [evidenceId], { reason: 'evidence-content-changed' })
    : [];
  return { graph: graph.snapshot(), changed, invalidated };
}

export function validateBeliefEvidence(graphState) {
  const issues = [];
  for (const node of Object.values(graphState.nodes ?? {})) {
    if (!['belief', 'negative', 'abstraction'].includes(node.kind)) continue;
    for (const evidenceId of node.evidenceIds ?? []) {
      const evidence = graphState.nodes[evidenceId];
      if (!evidence) issues.push({ type: 'missing-evidence', nodeId: node.id, evidenceId });
      else if (evidence.kind !== 'evidence') issues.push({ type: 'not-evidence', nodeId: node.id, evidenceId });
    }
  }
  return issues;
}
