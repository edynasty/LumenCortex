import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { AgentLoop } from '../src/agent.js';
import { SubagentPool } from '../src/subagent.js';
import { ToolRegistry } from '../src/tools.js';
import {
  SkillRegistry,
  SKILL_SCOPES,
  projectSkillRoot
} from '../src/skills.js';

function agentFixture() {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'lcx-node-skills-agent-'));
  const repoDir = path.join(root, '.lumencortex');
  fs.mkdirSync(repoDir, { recursive: true });
  const graphState = { version: 1, nodes: {}, edges: {}, metadata: {} };
  const repository = {
    dir: repoDir,
    _graph: graphState,
    graph() {
      return {
        snapshot: () => structuredClone(this._graph),
        getNode: () => undefined,
        addNode: (node) => { this._graph.nodes[node.id] = node; return node; },
        putNode: (node) => { this._graph.nodes[node.id] = node; return node; }
      };
    },
    writeGraph(graph) { this._graph = structuredClone(graph); },
    commit() { return { id: 'commit1' }; }
  };
  const runtime = {
    context() {
      return {
        mode: 'weighted',
        selectedNodes: [],
        selectedEdges: [],
        usedTokens: 0,
        budgetTokens: 1000
      };
    },
    promote() { throw new Error('promotion should not run'); }
  };
  return { root, repository, runtime };
}

test('Node Skill registry layers project over global and persists enable state', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'lcx-node-skills-'));
  const globalRoot = path.join(root, 'global-skills');
  const projectRoot = path.join(root, 'project', '.lumencortex', 'skills');
  const registry = new SkillRegistry({ globalRoot, projectRoot });

  registry.save(SKILL_SCOPES.GLOBAL, 'verify', [
    '---',
    'name: Global Verify',
    'description: global verification rule',
    '---',
    '',
    'Always run global checks.'
  ].join('\n'));
  registry.save(SKILL_SCOPES.PROJECT, 'verify', [
    '---',
    'name: Project Verify',
    'description: project override',
    '---',
    '',
    'Run the narrow project test first.'
  ].join('\n'));
  registry.save(SKILL_SCOPES.PROJECT, 'docs', '# Docs\n\nKeep docs synchronized.');

  const effective = registry.list(SKILL_SCOPES.EFFECTIVE);
  assert.equal(effective.length, 2);
  const verify = effective.find((item) => item.id === 'verify');
  assert.equal(verify.name, 'Project Verify');
  assert.equal(verify.scope, 'project');
  assert.equal(verify.overridden, true);

  registry.setEnabled(SKILL_SCOPES.PROJECT, 'docs', false);
  assert.equal(
    registry.list(SKILL_SCOPES.EFFECTIVE).find((item) => item.id === 'docs').enabled,
    false
  );
  const prompt = registry.prompt();
  assert.match(prompt.prompt, /Run the narrow project test first/);
  assert.doesNotMatch(prompt.prompt, /Keep docs synchronized/);

  registry.setEnabled(SKILL_SCOPES.PROJECT, 'docs', true);
  assert.match(registry.prompt().prompt, /Keep docs synchronized/);
});

test('Node Agent injects enabled Skills into the model working set', async () => {
  const { root, repository, runtime } = agentFixture();
  const registry = new SkillRegistry({
    globalRoot: path.join(root, 'global-skills'),
    projectRoot: projectSkillRoot(root)
  });
  registry.save('project', 'testing', '# Testing\n\nRUN_SKILL_SENTINEL before finishing.');

  const requests = [];
  const events = [];
  const provider = {
    model: 'mock',
    async complete(request) {
      requests.push(request);
      return {
        message: { role: 'assistant', content: 'done' },
        finishReason: 'stop'
      };
    }
  };
  const agent = new AgentLoop({
    provider,
    repository,
    runtime,
    workspace: root,
    tools: new ToolRegistry(),
    skillRegistry: registry,
    onEvent: (event) => events.push(event)
  });

  const result = await agent.run('verify skill injection', {
    maxSteps: 1,
    autoIngest: false,
    autoPromote: false,
    recordTask: false,
    recordObservations: false
  });

  assert.equal(result.final, 'done');
  const systemText = requests[0].messages
    .filter((message) => message.role === 'system')
    .map((message) => message.content)
    .join('\n');
  assert.match(systemText, /RUN_SKILL_SENTINEL/);
  assert.ok(events.some((event) =>
    event.type === 'skills.loaded' &&
    event.skills?.includes('testing')
  ));
});

test('delegated Node Subagent receives the same effective Skills', async () => {
  const { root, repository, runtime } = agentFixture();
  const registry = new SkillRegistry({
    globalRoot: path.join(root, 'global-skills'),
    projectRoot: projectSkillRoot(root)
  });
  registry.save('project', 'subagent-rule', '# Subagent Rule\n\nSUBAGENT_SKILL_SENTINEL.');

  const requests = [];
  const provider = {
    model: 'mock',
    async complete(request) {
      requests.push(request);
      return {
        message: { role: 'assistant', content: 'subagent done' },
        finishReason: 'stop'
      };
    }
  };
  const pool = new SubagentPool({
    provider,
    repository,
    runtime,
    workspace: root,
    tools: new ToolRegistry(),
    skillRegistry: registry,
    concurrency: 1
  });

  const result = await pool.run({
    goal: 'inspect one thing',
    maxSteps: 1,
    toolAllowlist: []
  }, {
    maxTokens: 200
  });

  assert.equal(result.final, 'subagent done');
  const systemText = requests[0].messages
    .filter((message) => message.role === 'system')
    .map((message) => message.content)
    .join('\n');
  assert.match(systemText, /SUBAGENT_SKILL_SENTINEL/);
});
