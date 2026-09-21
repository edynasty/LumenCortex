import test from 'node:test';
import assert from 'node:assert/strict';
import { WorkflowRuntime, evaluateWorkflowCondition, validateWorkflowDefinition } from '../src/workflow.js';

function definition() {
  return {
    version: 1,
    id: 'verified-fix',
    entry: 'diagnose',
    facts: { tests: { failed: false, passed: false }, implementation: { changed: false } },
    actions: {
      diagnose: {
        allowedTools: ['shell', 'read_files'],
        outcomes: [{
          when: { all: [
            { tool: 'shell' },
            { arg: 'command', contains: 'test' },
            { result: 'exitCode', notEquals: 0 }
          ] },
          set: { 'tests.failed': true }
        }],
        routes: [{ to: 'repair', when: { fact: 'tests.failed', equals: true } }]
      },
      repair: {
        allowedTools: ['read_files', 'replace_in_file'],
        outcomes: [{ when: { tool: 'replace_in_file', ok: true }, set: { 'implementation.changed': true } }],
        completeWhen: { fact: 'implementation.changed', equals: true },
        routes: [{ to: 'verify' }]
      },
      verify: {
        terminal: true,
        allowedTools: ['shell'],
        outcomes: [{
          when: { all: [
            { tool: 'shell' },
            { arg: 'command', contains: 'test' },
            { result: 'exitCode', equals: 0 }
          ] },
          set: { 'tests.passed': true }
        }],
        completeWhen: { fact: 'tests.passed', equals: true }
      }
    }
  };
}

test('workflow facts drive deterministic action transitions', () => {
  const workflow = new WorkflowRuntime(definition());
  assert.equal(workflow.actionId(), 'diagnose');
  assert.deepEqual(workflow.effectiveAllowlist(null), ['shell', 'read_files']);

  workflow.observeTool({
    tool: 'shell',
    args: { command: 'npm test' },
    result: { ok: true, content: JSON.stringify({ exitCode: 1 }) },
    step: 1
  });
  assert.equal(workflow.actionId(), 'repair');
  assert.equal(workflow.facts.tests.failed, true);
  assert.equal(workflow.factSources['tests.failed'].source, 'tool');

  workflow.observeTool({
    tool: 'replace_in_file',
    args: { path: 'src/a.js' },
    result: { ok: true, content: '{}' },
    step: 2
  });
  assert.equal(workflow.actionId(), 'verify');
  assert.equal(workflow.canFinish(), false);

  workflow.observeTool({
    tool: 'shell',
    args: { command: 'npm test' },
    result: { ok: true, content: JSON.stringify({ exitCode: 0 }) },
    step: 3
  });
  assert.equal(workflow.canFinish(), true);
  assert.equal(workflow.status, 'ready_to_finish');
});

test('human gates pause until an explicit approval fact is recorded', () => {
  const workflow = new WorkflowRuntime({
    version: 1,
    id: 'release',
    entry: 'review',
    facts: { verified: true },
    actions: {
      review: {
        terminal: true,
        completeWhen: { fact: 'verified', equals: true },
        gates: [{ id: 'release-approval', type: 'human', title: 'Approve release' }]
      }
    }
  });
  assert.equal(workflow.canFinish(), false);
  assert.equal(workflow.status, 'waiting_gate');
  assert.deepEqual(workflow.waitingHumanGates().map((gate) => gate.id), ['release-approval']);

  workflow.approve('release-approval', { actor: 'tester' });
  assert.equal(workflow.canFinish(), true);
  assert.equal(workflow.status, 'ready_to_finish');
  assert.equal(workflow.factSources['gates.release-approval'].source, 'human');
});

test('persisted workflow state rejects definition drift on resume', () => {
  const original = definition();
  const runtime = new WorkflowRuntime(original);
  runtime.observeTool({
    tool: 'shell',
    args: { command: 'npm test' },
    result: { ok: true, content: JSON.stringify({ exitCode: 1 }) }
  });
  const session = { metadata: { workflow: runtime.snapshot() } };
  const resumed = WorkflowRuntime.fromSession(session);
  assert.equal(resumed.actionId(), 'repair');

  const changed = definition();
  changed.actions.verify.description = 'changed contract';
  assert.throws(() => WorkflowRuntime.fromSession(session, changed), /differs from the contract persisted/);
});

test('condition evaluator reads tool args, JSON result fields and facts', () => {
  const context = {
    facts: { tests: { passed: true } },
    tool: 'shell',
    args: { command: 'npm test' },
    result: { ok: true, content: JSON.stringify({ exitCode: 0 }) },
    resultData: { exitCode: 0 }
  };
  assert.equal(evaluateWorkflowCondition({
    all: [
      { tool: 'shell' },
      { ok: true },
      { arg: 'command', contains: 'test' },
      { result: 'exitCode', equals: 0 },
      { fact: 'tests.passed', equals: true }
    ]
  }, context), true);
});

test('definition validation rejects routes to unknown actions', () => {
  assert.throws(() => validateWorkflowDefinition({
    version: 1,
    id: 'bad',
    entry: 'a',
    actions: { a: { routes: [{ to: 'missing' }] } }
  }), /unknown action/);
});
