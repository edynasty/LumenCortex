import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { CognitiveRepository } from '../src/repository.js';
import { LumenCortexRuntime } from '../src/runtime.js';
import { ingestWorkspace } from '../src/ingest.js';
import { createProvider } from '../src/provider.js';
import { AgentLoop } from '../src/agent.js';

const providerName = process.env.LUMENCORTEX_PROVIDER ?? 'openrouter';
const root = fs.mkdtempSync(path.join(os.tmpdir(), 'lumencortex-free-smoke-'));
fs.writeFileSync(path.join(root, 'answer.txt'), 'LUMENCORTEX_SMOKE_VALUE=42\n', 'utf8');
fs.writeFileSync(path.join(root, 'README.md'), '# Smoke workspace\nThe answer must be read from answer.txt.\n', 'utf8');

const repo = new CognitiveRepository(root);
repo.init();
const ingested = ingestWorkspace(repo.graph().snapshot(), root);
repo.writeGraph(ingested.graph);
repo.commit('ingest smoke workspace');

const runtime = new LumenCortexRuntime(repo);
const provider = createProvider(providerName, { model: process.env.LUMENCORTEX_MODEL });
const agent = new AgentLoop({
  provider,
  repository: repo,
  runtime,
  workspace: root,
  authorize: async (tool) => (tool.permission ?? 'read') === 'read',
  onEvent: (event) => {
    if (event.type === 'llm.request') console.log(`[step ${event.step}] ${event.model}`);
    if (event.type === 'tool.start') console.log(`  -> ${event.name}`);
    if (event.type === 'tool.end') console.log(`  <- ${event.name}: ${event.ok ? 'ok' : 'error'}`);
  }
});

const result = await agent.run(
  'Find the exact value of LUMENCORTEX_SMOKE_VALUE by using workspace tools. Return only the number.',
  { maxSteps: 8, budgetTokens: 8000, autoIngest: false, recordTask: false, providerName }
);

console.log(`final=${result.final.trim()}`);
console.log(`session=${result.session.id}`);
console.log(`requests=${result.usage.requests}`);
if (!/\b42\b/.test(result.final)) {
  console.error('Smoke failed: model did not return 42');
  process.exitCode = 2;
}
