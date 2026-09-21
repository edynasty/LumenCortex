import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { spawnSync } from 'node:child_process';
import { AgentSessionStore } from '../src/session.js';
import { WorkflowRuntime } from '../src/workflow.js';

const cli = fileURLToPath(new URL('../src/cli.js', import.meta.url));

function run(cwd, args) {
  return spawnSync(process.execPath, [cli, ...args], { cwd, encoding: 'utf8', env: process.env });
}

test('workflow CLI validates contracts and persists human gate approval', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'lcx-workflow-cli-'));
  const init = run(root, ['init', root]);
  assert.equal(init.status, 0, init.stderr);

  const definition = {
    version: 1,
    id: 'cli-gate',
    entry: 'review',
    facts: { verified: true },
    actions: {
      review: {
        terminal: true,
        completeWhen: { fact: 'verified', equals: true },
        gates: [{ id: 'approve', type: 'human', title: 'Approve result' }]
      }
    }
  };
  fs.writeFileSync(path.join(root, 'workflow.json'), JSON.stringify(definition));

  const validate = run(root, ['workflow', 'validate', 'workflow.json']);
  assert.equal(validate.status, 0, validate.stderr);
  assert.equal(JSON.parse(validate.stdout).valid, true);

  const store = new AgentSessionStore(path.join(root, '.lumencortex'));
  const workflow = new WorkflowRuntime(definition);
  const session = store.create({ goal: 'test gate', metadata: { workflow: workflow.snapshot() } });
  store.close();

  const status = run(root, ['workflow', 'status', session.id]);
  assert.equal(status.status, 0, status.stderr);
  assert.equal(JSON.parse(status.stdout).status, 'waiting_gate');

  const approve = run(root, ['workflow', 'approve', session.id, 'approve', '--actor', 'cli-test']);
  assert.equal(approve.status, 0, approve.stderr);
  assert.equal(JSON.parse(approve.stdout).workflow.canFinish, true);

  const verifyStore = new AgentSessionStore(path.join(root, '.lumencortex'));
  const saved = verifyStore.load(session.id);
  verifyStore.close();
  assert.equal(saved.metadata.workflow.factSources['gates.approve'].source, 'human');
  assert.equal(saved.metadata.workflow.factSources['gates.approve'].actor, 'cli-test');
});
