import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import {
  CognitiveRepository,
  GraphGovernor,
  GraphGovernorAnalyzer,
  LLMGraphGovernorCurator,
  GraphGovernorScheduler
} from '../src/index.js';

function tempRepository(prefix = 'lcx-governor-scheduler-') {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), prefix));
  const repo = new CognitiveRepository(root);
  repo.init();
  return { root, repo };
}

function addStaleGraph(repo, count = 4) {
  const graph = repo.graph();
  graph.addNode({
    id: 'active',
    kind: 'entity',
    title: 'Active root',
    status: 'active',
    grade: 'static',
    trustZone: 'repo_trusted'
  });
  for (let index = 0; index < count; index += 1) {
    graph.addNode({
      id: `stale-${index}`,
      kind: 'evidence',
      title: `Old hypothesis ${index}`,
      body: 'obsolete low-value context',
      status: 'stale',
      grade: 'hypothesis',
      trustZone: 'model_inferred'
    });
  }
  repo.writeGraph(graph.snapshot());
}

test('Governor scheduler is opt-in and does not analyze while disabled', async () => {
  const { repo } = tempRepository();
  const governor = new GraphGovernor({ repository: repo });
  const scheduler = new GraphGovernorScheduler({
    repository: repo,
    governor,
    options: { enabled: false }
  });

  const result = await scheduler.evaluate();
  assert.equal(result.scheduled, false);
  assert.equal(result.reason, 'disabled');
  assert.equal(scheduler.status().state.lastCheckRevision, null);
  repo.close();
});

test('Governor scheduler persists a pending plan without mutating the graph', async () => {
  const { repo } = tempRepository();
  addStaleGraph(repo);
  const governor = new GraphGovernor({
    repository: repo,
    analyzer: new GraphGovernorAnalyzer({ archiveThreshold: 0.5 })
  });
  const scheduler = new GraphGovernorScheduler({
    repository: repo,
    governor,
    options: {
      enabled: true,
      useCurator: false,
      checkRevisionDelta: 1,
      cooldownMs: 0,
      archiveCandidateThreshold: 1,
      tierChangeThreshold: 999
    }
  });

  const before = repo.graph().snapshot();
  const result = await scheduler.evaluate({ now: Date.UTC(2026, 8, 26, 0, 0, 0) });
  assert.equal(result.scheduled, true);
  assert.equal(result.reason, 'pressure');
  assert.ok(result.pending.id.startsWith('govplan_'));
  assert.ok(result.pending.plan.archive.includes('stale-0'));
  assert.equal(result.pending.curator.enabled, false);
  assert.deepEqual(repo.graph().snapshot(), before, 'scheduler planning must not mutate graph');

  const reopenedScheduler = new GraphGovernorScheduler({
    repository: repo,
    governor,
    options: { enabled: true, cooldownMs: 0 }
  });
  const status = reopenedScheduler.status();
  assert.equal(status.state.pending.id, result.pending.id);
  assert.equal(status.state.pending.stale, false);

  const dryRun = reopenedScheduler.applyPending({ dryRun: true });
  assert.equal(dryRun.applied, false);
  assert.equal(reopenedScheduler.status().state.pending.id, result.pending.id);

  const applied = reopenedScheduler.applyPending();
  assert.equal(applied.applied, true);
  assert.equal(reopenedScheduler.status().state.pending, null);
  assert.equal(repo.graph().getNode('stale-0').status, 'archived');
  assert.ok(repo.journal().some((entry) => entry.event === 'governor.scheduler.planned'));
  assert.ok(repo.journal().some((entry) => entry.event === 'governor.scheduler.applied'));

  repo.close();
});

test('Governor scheduler debounces checks by revision delta and cooldown', async () => {
  const { repo } = tempRepository();
  const governor = new GraphGovernor({ repository: repo });
  const scheduler = new GraphGovernorScheduler({
    repository: repo,
    governor,
    options: {
      enabled: true,
      checkRevisionDelta: 2,
      cooldownMs: 1000,
      archiveCandidateThreshold: 999,
      canonicalizeGroupThreshold: 999,
      branchCandidateThreshold: 999,
      promotionGroupThreshold: 999,
      tierChangeThreshold: 999
    }
  });

  const firstAt = Date.UTC(2026, 8, 26, 0, 0, 0);
  const first = await scheduler.evaluate({ now: firstAt });
  assert.equal(first.scheduled, false);
  assert.equal(first.reason, 'no-pressure');

  let graph = repo.graph();
  graph.addNode({ id: 'one', kind: 'entity', title: 'One' });
  repo.writeGraph(graph.snapshot());

  const deltaBlocked = await scheduler.evaluate({ now: firstAt + 5000 });
  assert.equal(deltaBlocked.reason, 'revision-delta');

  graph = repo.graph();
  graph.addNode({ id: 'two', kind: 'entity', title: 'Two' });
  graph.addNode({ id: 'three', kind: 'entity', title: 'Three' });
  repo.writeGraph(graph.snapshot());

  const cooldownBlocked = await scheduler.evaluate({ now: firstAt + 500 });
  assert.equal(cooldownBlocked.reason, 'cooldown');
  assert.ok(cooldownBlocked.cooldownRemainingMs > 0);

  const afterCooldown = await scheduler.evaluate({ now: firstAt + 1500 });
  assert.equal(afterCooldown.reason, 'no-pressure');
  repo.close();
});

test('Governor scheduler refuses to apply a pending plan after graph revision drift', async () => {
  const { repo } = tempRepository();
  const governor = new GraphGovernor({ repository: repo });
  const scheduler = new GraphGovernorScheduler({
    repository: repo,
    governor,
    options: { enabled: true, cooldownMs: 0 }
  });

  const planned = await scheduler.evaluate({ force: true });
  assert.equal(planned.scheduled, true);

  const graph = repo.graph();
  graph.addNode({ id: 'newer', kind: 'entity', title: 'Newer context' });
  repo.writeGraph(graph.snapshot());

  assert.throws(
    () => scheduler.applyPending(),
    (error) =>
      error?.code === 'GOVERNOR_PLAN_STALE' &&
      error.currentRevision === repo.graphRevision()
  );
  assert.equal(scheduler.status().state.pending.stale, true);
  repo.close();
});

test('Governor scheduler invokes semantic Curator only when explicitly enabled', async () => {
  const { repo } = tempRepository();
  let calls = 0;
  const provider = {
    model: 'curator-test',
    async complete() {
      calls += 1;
      return {
        message: {
          role: 'assistant',
          content: JSON.stringify({
            archive: [],
            canonicalize: [],
            branch: [],
            promote: [],
            summary: 'curated plan'
          })
        },
        finishReason: 'stop'
      };
    }
  };
  const governor = new GraphGovernor({
    repository: repo,
    curator: new LLMGraphGovernorCurator({ provider })
  });

  const deterministic = new GraphGovernorScheduler({
    repository: repo,
    governor,
    options: { enabled: true, useCurator: false, cooldownMs: 0 }
  });
  const first = await deterministic.evaluate({ force: true });
  assert.equal(calls, 0);
  assert.equal(first.pending.curator.enabled, false);

  const semantic = new GraphGovernorScheduler({
    repository: repo,
    governor,
    options: { enabled: true, useCurator: true, cooldownMs: 0 }
  });
  const second = await semantic.evaluate({ force: true });
  assert.equal(calls, 1);
  assert.equal(second.pending.curator.enabled, true);
  assert.equal(second.pending.curator.model, 'curator-test');

  repo.close();
});


test('Governor scheduler auto-applies deterministic safe tier/archive changes when explicitly enabled', async () => {
  const { repo } = tempRepository();
  addStaleGraph(repo, 3);
  const governor = new GraphGovernor({
    repository: repo,
    analyzer: new GraphGovernorAnalyzer({ archiveThreshold: 0.5 })
  });
  const scheduler = new GraphGovernorScheduler({
    repository: repo,
    governor,
    options: {
      enabled: true,
      useCurator: false,
      autoApplySafe: true,
      checkRevisionDelta: 1,
      cooldownMs: 0,
      archiveCandidateThreshold: 1,
      canonicalizeGroupThreshold: 999,
      branchCandidateThreshold: 999,
      promotionGroupThreshold: 999,
      tierChangeThreshold: 999
    }
  });

  const result = await scheduler.evaluate({ now: Date.UTC(2026, 8, 26, 1, 0, 0) });
  assert.equal(result.scheduled, true);
  assert.equal(result.autoApplied, true);
  assert.ok(result.safeChangedCount > 0);
  assert.equal(result.pending, null);
  assert.equal(result.apply.automatic, true);
  assert.equal(scheduler.status().state.pending, null);
  assert.equal(scheduler.status().state.lastAutoAppliedPlanId, result.apply.planId);
  assert.equal(repo.graph().getNode('stale-0').status, 'archived');

  const appliedEvent = repo.journal().find((entry) => entry.event === 'governor.scheduler.applied');
  assert.equal(appliedEvent.payload.automatic, true);
  repo.close();
});

test('Governor scheduler never auto-applies Curator-backed plans', async () => {
  const { repo } = tempRepository();
  let calls = 0;
  const provider = {
    model: 'curator-auto-safe-test',
    async complete() {
      calls += 1;
      return {
        message: {
          role: 'assistant',
          content: JSON.stringify({
            archive: [],
            canonicalize: [],
            branch: [],
            promote: [],
            summary: 'semantic plan requires review'
          })
        },
        finishReason: 'stop'
      };
    }
  };
  const governor = new GraphGovernor({
    repository: repo,
    curator: new LLMGraphGovernorCurator({ provider })
  });
  const scheduler = new GraphGovernorScheduler({
    repository: repo,
    governor,
    options: {
      enabled: true,
      useCurator: true,
      autoApplySafe: true,
      cooldownMs: 0
    }
  });

  const result = await scheduler.evaluate({ force: true });
  assert.equal(calls, 1);
  assert.equal(result.autoApplied, false);
  assert.equal(result.autoApplySkippedReason, 'curator-plan-requires-explicit-apply');
  assert.ok(result.pending?.id);
  assert.equal(scheduler.status().state.pending.id, result.pending.id);
  repo.close();
});

test('Governor scheduler leaves a pending plan when auto-safe preview has no safe mutations', async () => {
  const { repo } = tempRepository();
  const governor = new GraphGovernor({ repository: repo });
  const scheduler = new GraphGovernorScheduler({
    repository: repo,
    governor,
    options: {
      enabled: true,
      useCurator: false,
      autoApplySafe: true,
      cooldownMs: 0
    }
  });

  const result = await scheduler.evaluate({ force: true });
  assert.equal(result.scheduled, true);
  assert.equal(result.autoApplied, false);
  assert.equal(result.autoApplySkippedReason, 'no-safe-changes');
  assert.ok(result.pending?.id);
  assert.equal(scheduler.status().state.pending.id, result.pending.id);
  repo.close();
});
