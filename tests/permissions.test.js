import test from 'node:test';
import assert from 'node:assert/strict';
import { normalizePolicy, policyAllowsTool } from '../src/permissions.js';

test('permission policies have distinct scope semantics', () => {
  const workspaceRead = { permission: 'read', scope: 'workspace' };
  const workspaceWrite = { permission: 'write', scope: 'workspace' };
  const hostExec = { permission: 'exec', scope: 'host' };
  const externalRead = { permission: 'read', scope: 'external' };
  const externalWrite = { permission: 'write', scope: 'external' };

  assert.equal(policyAllowsTool(workspaceRead, 'read-only'), true);
  assert.equal(policyAllowsTool(workspaceWrite, 'read-only'), false);
  assert.equal(policyAllowsTool(hostExec, 'read-only'), false);

  assert.equal(policyAllowsTool(workspaceRead, 'workspace'), true);
  assert.equal(policyAllowsTool(workspaceWrite, 'workspace'), true);
  assert.equal(policyAllowsTool(externalRead, 'workspace'), true);
  assert.equal(policyAllowsTool(externalWrite, 'workspace'), false);
  assert.equal(policyAllowsTool(hostExec, 'workspace'), false);

  assert.equal(policyAllowsTool(workspaceWrite, 'full'), true);
  assert.equal(policyAllowsTool(externalWrite, 'full'), true);
  assert.equal(policyAllowsTool(hostExec, 'full'), true);
});

test('unknown policy fails closed instead of becoming full access', () => {
  assert.throws(() => normalizePolicy('workspcae'), /Unknown policy/);
  assert.throws(
    () => policyAllowsTool({ permission: 'exec', scope: 'host' }, 'everything'),
    /Unknown policy/
  );
});

test('tools without an explicit scope are treated as workspace tools', () => {
  assert.equal(policyAllowsTool({ permission: 'write' }, 'workspace'), true);
  assert.equal(policyAllowsTool({ permission: 'exec' }, 'workspace'), false);
});
