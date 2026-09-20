#!/usr/bin/env node
import fs from 'node:fs';
import path from 'node:path';
import readline from 'node:readline/promises';
import { fileURLToPath } from 'node:url';
import { CognitiveRepository } from './repository.js';
import { ModelWeaveRuntime } from './runtime.js';
import { graphSummary } from './graph.js';
import { ingestWorkspace } from './ingest.js';
import { createProvider, providerInfo } from './provider.js';
import { AgentLoop } from './agent.js';
import { AgentSessionStore } from './session.js';

const args = process.argv.slice(2);
const command = args.shift();

try {
  if (!command || ['help', '--help', '-h'].includes(command)) {
    help();
    process.exit(0);
  }

  if (command === 'init') {
    const target = path.resolve(args[0] ?? process.cwd());
    fs.mkdirSync(target, { recursive: true });
    const repo = new CognitiveRepository(target);
    const commit = repo.init();
    console.log(`Initialized ModelWeave in ${repo.dir}`);
    console.log(`Genesis ${commit.id}`);
    process.exit(0);
  }

  if (command === 'install-opencode') {
    const target = path.resolve(args[0] ?? process.cwd());
    const sourceDir = fileURLToPath(new URL('../integrations/opencode/', import.meta.url));
    const toolsDir = path.join(target, '.opencode', 'tools');
    fs.mkdirSync(toolsDir, { recursive: true });
    for (const file of fs.readdirSync(sourceDir)) {
      if (!file.endsWith('.ts')) continue;
      fs.copyFileSync(path.join(sourceDir, file), path.join(toolsDir, file));
      console.log(`Installed ${path.join('.opencode', 'tools', file)}`);
    }
    process.exit(0);
  }

  if (command === 'providers') {
    for (const item of providerInfo()) {
      const configured = Boolean(process.env[item.apiKeyEnv]) || (item.name === 'generic' && process.env.MODELWEAVE_REQUIRE_API_KEY === 'false');
      console.log(`${configured ? '✓' : '○'} ${item.name.padEnd(12)} ${item.defaultModel.padEnd(28)} ${item.apiKeyEnv}`);
      console.log(`  ${item.baseURL}`);
    }
    process.exit(0);
  }

  const workspace = findWorkspace(process.cwd());
  const repo = new CognitiveRepository(workspace);
  const runtime = new ModelWeaveRuntime(repo);

  switch (command) {
    case 'agent':
      await agentCommand({ repo, runtime, workspace, argv: args });
      break;
    case 'chat':
      await chatCommand({ repo, runtime, workspace, argv: args });
      break;
    case 'sessions': {
      const parsed = parseFlags(args);
      const store = new AgentSessionStore(repo.dir);
      console.log(JSON.stringify(store.list(Number(parsed.flags.limit ?? 20)), null, 2));
      break;
    }
    case 'doctor':
      await doctorCommand(args);
      break;
    case 'status': {
      const diff = repo.status();
      console.log(`${diff.operations.length} uncommitted operation(s)`);
      for (const op of diff.operations) console.log(`  ${op.type.padEnd(11)} ${op.id}`);
      break;
    }
    case 'commit': {
      const message = args.join(' ').trim();
      if (!message) fail('Usage: modelweave commit <message>');
      const commit = repo.commit(message);
      console.log(`${commit.id} ${commit.message}`);
      break;
    }
    case 'log': {
      const limit = Number(args[0] ?? 20);
      for (const commit of repo.log(limit)) console.log(`${commit.id} ${commit.createdAt} ${commit.message}`);
      break;
    }
    case 'branch': {
      if (!args[0]) for (const branch of repo.branches()) console.log(`${branch.current ? '*' : ' '} ${branch.name} ${branch.commitId}`);
      else {
        const branch = repo.createBranch(args[0]);
        console.log(`Created ${branch.name} at ${branch.commitId}`);
      }
      break;
    }
    case 'checkout': {
      if (!args[0]) fail('Usage: modelweave checkout <branch>');
      const commit = repo.checkout(args[0]);
      console.log(`Switched to ${args[0]} (${commit.id})`);
      break;
    }
    case 'merge': {
      if (!args[0]) fail('Usage: modelweave merge <branch>');
      const result = repo.merge(args[0]);
      if (result.conflicts.length) {
        console.error('Merge conflicts:');
        for (const conflict of result.conflicts) console.error(`  ${conflict.kind}:${conflict.id}`);
        process.exitCode = 2;
      } else if (result.alreadyUpToDate) console.log('Already up to date');
      else console.log(`Merged as ${result.commit.id}`);
      break;
    }
    case 'revert': {
      if (!args[0]) fail('Usage: modelweave revert <commit>');
      const result = repo.revert(args[0]);
      if (result.conflicts.length) {
        console.error(`Revert conflict: ${result.conflicts[0].message}`);
        process.exitCode = 2;
      } else console.log(`Reverted in ${result.commit.id}`);
      break;
    }
    case 'cherry-pick': {
      if (!args[0]) fail('Usage: modelweave cherry-pick <commit>');
      const result = repo.cherryPick(args[0]);
      if (result.conflicts.length) {
        console.error('Cherry-pick conflicts:');
        for (const conflict of result.conflicts) console.error(`  ${conflict.kind ?? 'commit'}:${conflict.id ?? conflict.commitId ?? 'unknown'} ${conflict.message ?? ''}`);
        process.exitCode = 2;
      } else console.log(`Cherry-picked as ${result.commit.id}`);
      break;
    }
    case 'blame': {
      if (!args[0]) fail('Usage: modelweave blame <node-id> [limit]');
      console.log(JSON.stringify(repo.blame(args[0], { limit: Number(args[1] ?? 20) }), null, 2));
      break;
    }
    case 'rebase': {
      if (!args[0]) fail('Usage: modelweave rebase <branch>');
      const result = repo.rebase(args[0]);
      if (result.conflicts.length) {
        console.error('Rebase conflicts:');
        for (const conflict of result.conflicts) console.error(`  ${conflict.kind}:${conflict.id}`);
        process.exitCode = 2;
      } else console.log(`Rebased ${result.branch} onto ${args[0]} (${result.commits.length} replayed commit(s))`);
      break;
    }
    case 'node':
      await nodeCommand(repo, args);
      break;
    case 'edge':
      await edgeCommand(repo, args);
      break;
    case 'show': {
      const graph = repo.graph();
      if (!args[0]) console.log(JSON.stringify(graphSummary(graph.snapshot()), null, 2));
      else console.log(JSON.stringify(graph.getNode(args[0]) ?? graph.getEdge(args[0]), null, 2));
      break;
    }
    case 'ingest': {
      const parsed = parseFlags(args);
      const source = path.resolve(parsed.positionals[0] ?? workspace);
      const result = ingestWorkspace(repo.graph().snapshot(), source, {
        chunkLines: Number(parsed.flags['chunk-lines'] ?? 160),
        maxFileBytes: Number(parsed.flags['max-bytes'] ?? 524288)
      });
      repo.writeGraph(result.graph);
      console.log(JSON.stringify(result.stats, null, 2));
      break;
    }
    case 'light': {
      const parsed = parseFlags(args);
      const goal = parsed.positionals.join(' ').trim();
      if (!goal) fail('Usage: modelweave light <goal> [--budget 32000] [--multi]');
      const options = { budgetTokens: Number(parsed.flags.budget ?? 32000) };
      if (parsed.flags.multi) {
        const lights = runtime.contextMulti(goal, options);
        if (parsed.flags.json) console.log(JSON.stringify(lights, null, 2));
        else for (const [name, result] of Object.entries(lights)) printLight(name, result);
      } else {
        const result = runtime.context(goal, options);
        if (parsed.flags.json) console.log(JSON.stringify(result, null, 2));
        else printLight('spotlight', result);
      }
      break;
    }
    case 'promote': {
      const title = args.shift();
      if (!title || !args.length) fail('Usage: modelweave promote <title> <nodeId> [nodeId...]');
      const abstraction = runtime.promote(args, { title });
      console.log(`${abstraction.id} ${abstraction.title}`);
      break;
    }
    case 'verify': {
      const result = runtime.verify();
      console.log(JSON.stringify({ staleEvidence: result.staleEvidence, dirtiedBeliefs: result.dirtiedBeliefs, issues: result.issues }, null, 2));
      break;
    }
    default:
      fail(`Unknown command: ${command}`);
  }
} catch (error) {
  console.error(`modelweave: ${error.message}`);
  if (error.sessionId) console.error(`session: ${error.sessionId}`);
  process.exitCode = 1;
}

async function agentCommand({ repo, runtime, workspace, argv }) {
  const parsed = parseFlags(argv);
  const goal = parsed.positionals.join(' ').trim();
  if (!goal && !parsed.flags.session) fail('Usage: modelweave agent <goal> [--provider openrouter] [--model MODEL] [--yes]');
  const providerName = String(parsed.flags.provider ?? process.env.MODELWEAVE_PROVIDER ?? 'openrouter');
  const provider = createProvider(providerName, {
    model: parsed.flags.model ? String(parsed.flags.model) : undefined,
    baseURL: parsed.flags['base-url'] ? String(parsed.flags['base-url']) : undefined,
    timeoutMs: parsed.flags['timeout-ms'] ? Number(parsed.flags['timeout-ms']) : undefined
  });
  const json = Boolean(parsed.flags.json);
  const authorize = createAuthorizer({ yes: Boolean(parsed.flags.yes), policy: String(parsed.flags.policy ?? 'workspace'), json });
  const agent = new AgentLoop({
    provider,
    repository: repo,
    runtime,
    workspace,
    authorize,
    onEvent: json ? () => {} : renderAgentEvent
  });
  const result = await agent.run(goal, {
    providerName,
    sessionId: parsed.flags.session ? String(parsed.flags.session) : undefined,
    maxSteps: Number(parsed.flags['max-steps'] ?? 24),
    budgetTokens: Number(parsed.flags.budget ?? 24000),
    maxTokens: parsed.flags['max-tokens'] ? Number(parsed.flags['max-tokens']) : undefined,
    llmRetries: Number(parsed.flags['llm-retries'] ?? 2),
    retryBaseMs: Number(parsed.flags['retry-base-ms'] ?? 800),
    maxToolCallsPerStep: Number(parsed.flags['max-tool-calls-per-step'] ?? Number.MAX_SAFE_INTEGER),
    toolAllowlist: parsed.flags.tools ? String(parsed.flags.tools).split(',').map(x => x.trim()).filter(Boolean) : undefined,
    recentRounds: Number(parsed.flags['recent-rounds'] ?? 6),
    maxWorkingChars: Number(parsed.flags['working-chars'] ?? 120000),
    autoPromote: parsed.flags['no-auto-promote'] ? false : true,
    autoIngest: parsed.flags['no-ingest'] ? false : true,
    cognitiveCommit: Boolean(parsed.flags['cognitive-commit']),
    authorize
  });
  if (json) console.log(JSON.stringify({ sessionId: result.session.id, final: result.final, usage: result.usage }, null, 2));
  else {
    console.log(`\n${result.final}`);
    console.log(`\n[session ${result.session.id}] requests=${result.usage.requests} tokens=${result.usage.totalTokens}`);
  }
}

async function chatCommand({ repo, runtime, workspace, argv }) {
  const parsed = parseFlags(argv);
  const providerName = String(parsed.flags.provider ?? process.env.MODELWEAVE_PROVIDER ?? 'openrouter');
  const provider = createProvider(providerName, {
    model: parsed.flags.model ? String(parsed.flags.model) : undefined,
    baseURL: parsed.flags['base-url'] ? String(parsed.flags['base-url']) : undefined,
    timeoutMs: parsed.flags['timeout-ms'] ? Number(parsed.flags['timeout-ms']) : undefined
  });
  const terminal = readline.createInterface({ input: process.stdin, output: process.stdout });
  const authorize = createAuthorizer({ yes: Boolean(parsed.flags.yes), policy: String(parsed.flags.policy ?? 'workspace'), json: false, terminal });
  const agent = new AgentLoop({ provider, repository: repo, runtime, workspace, authorize, onEvent: renderAgentEvent });
  let sessionId = parsed.flags.session ? String(parsed.flags.session) : null;
  console.log(`ModelWeave chat — ${providerName}/${provider.model}. /exit to quit.`);
  try {
    while (true) {
      const goal = (await terminal.question('mw> ')).trim();
      if (!goal) continue;
      if (['/exit', '/quit'].includes(goal)) break;
      const result = await agent.run(goal, {
        providerName,
        sessionId: sessionId ?? undefined,
        maxSteps: Number(parsed.flags['max-steps'] ?? 24),
        budgetTokens: Number(parsed.flags.budget ?? 24000),
        llmRetries: Number(parsed.flags['llm-retries'] ?? 2),
        retryBaseMs: Number(parsed.flags['retry-base-ms'] ?? 800),
        maxToolCallsPerStep: Number(parsed.flags['max-tool-calls-per-step'] ?? Number.MAX_SAFE_INTEGER),
        toolAllowlist: parsed.flags.tools ? String(parsed.flags.tools).split(',').map(x => x.trim()).filter(Boolean) : undefined,
        recentRounds: Number(parsed.flags['recent-rounds'] ?? 4),
        workingChars: Number(parsed.flags['working-chars'] ?? 48000),
        autoPromote: parsed.flags['no-auto-promote'] ? false : true,
        cognitiveCommit: Boolean(parsed.flags['cognitive-commit']),
        authorize
      });
      sessionId = result.session.id;
      console.log(`\n${result.final}\n`);
    }
  } finally {
    terminal.close();
  }
}

async function doctorCommand(argv) {
  const parsed = parseFlags(argv);
  const providerName = String(parsed.flags.provider ?? process.env.MODELWEAVE_PROVIDER ?? 'openrouter');
  const info = providerInfo().find((item) => item.name === providerName);
  if (!info) fail(`Unknown provider: ${providerName}`);
  console.log(`provider: ${providerName}`);
  console.log(`baseURL: ${parsed.flags['base-url'] ?? info.baseURL}`);
  console.log(`model: ${parsed.flags.model ?? info.defaultModel}`);
  console.log(`credential: ${process.env[info.apiKeyEnv] ? `${info.apiKeyEnv} is set` : `${info.apiKeyEnv} is NOT set`}`);
  if (parsed.flags.live) {
    const provider = createProvider(providerName, {
      model: parsed.flags.model ? String(parsed.flags.model) : undefined,
      baseURL: parsed.flags['base-url'] ? String(parsed.flags['base-url']) : undefined,
      timeoutMs: parsed.flags['timeout-ms'] ? Number(parsed.flags['timeout-ms']) : undefined
    });
    const result = await provider.complete({ messages: [{ role: 'user', content: 'Reply with exactly: MODELWEAVE_OK' }] });
    console.log(`live: ${result.message.content}`);
  }
}

function createAuthorizer({ yes, policy, json, terminal: sharedTerminal }) {
  const allowed = new Set(policy === 'read-only' ? ['read'] : policy === 'workspace' ? ['read', 'write', 'exec'] : ['read', 'write', 'exec']);
  return async (tool, args) => {
    const permission = tool.permission ?? 'read';
    if (!allowed.has(permission)) return false;
    if (permission === 'read' || yes) return true;
    if (!process.stdin.isTTY || json) return false;
    const terminal = sharedTerminal ?? readline.createInterface({ input: process.stdin, output: process.stdout });
    try {
      const preview = JSON.stringify(args).slice(0, 500);
      const answer = (await terminal.question(`Allow ${permission} tool ${tool.name} ${preview}? [y/N] `)).trim().toLowerCase();
      return answer === 'y' || answer === 'yes';
    } finally {
      if (!sharedTerminal) terminal.close();
    }
  };
}

function renderAgentEvent(event) {
  if (event.type === 'session.start') console.log(`[agent] session=${event.sessionId} budget=${event.budgetTokens}t maxSteps=${event.maxSteps}`);
  else if (event.type === 'llm.request') console.log(`[agent] step ${event.step} → ${event.model}`);
  else if (event.type === 'llm.retry') console.log(`  ↻ LLM retry ${event.attempt} in ${event.delayMs}ms: ${event.error}`);
  else if (event.type === 'tools.deferred') console.log(`  ⇢ tool fanout bounded: executing ${event.executing}/${event.requested}, deferred ${event.deferred}`);
  else if (event.type === 'llm.empty_turn') console.log(`  ↻ empty assistant turn, recovery attempt ${event.attempt}`);
  else if (event.type === 'tool.start') console.log(`  → ${event.name} ${compact(event.args)}`);
  else if (event.type === 'tool.end') console.log(`  ← ${event.name} ${event.ok ? 'ok' : event.denied ? 'denied' : 'error'}`);
  else if (event.type === 'context.refresh') console.log(`  💡 context refreshed (${event.selectedNodes} nodes/${event.contextTokens}t)`);
  else if (event.type === 'context.move') console.log(`  ☼ light moved: ${event.selectedNodes} nodes/${event.contextTokens}t`);
  else if (event.type === 'context.promote') console.log(`  ↑ promoted ${event.childCount} nodes → ${event.abstractionId}`);
  else if (event.type === 'context.ingest') console.log(`  ↻ graph refreshed (${event.stats.changedEvidence} changed evidence)`);
  else if (event.type === 'session.complete') console.log(`[agent] completed in ${event.step} step(s)`);
}

async function nodeCommand(repo, argv) {
  const action = argv.shift();
  const graph = repo.graph();
  if (action === 'add') {
    const parsed = parseFlags(argv);
    const [kind, title, ...bodyParts] = parsed.positionals;
    if (!kind || !title) fail('Usage: modelweave node add <kind> <title> [body]');
    const node = graph.addNode({ kind, title, body: bodyParts.join(' '), grade: parsed.flags.grade, trustZone: parsed.flags.trust, tags: parsed.flags.tags ? String(parsed.flags.tags).split(',') : undefined, source: parsed.flags.source ? { uri: String(parsed.flags.source) } : undefined, evidenceIds: parsed.flags.evidence ? String(parsed.flags.evidence).split(',') : undefined });
    repo.writeGraph(graph.snapshot());
    console.log(node.id);
  } else if (action === 'update') {
    const parsed = parseFlags(argv);
    const id = parsed.positionals.shift();
    if (!id) fail('Usage: modelweave node update <id>');
    const node = graph.updateNode(id, {
      ...(parsed.flags.title ? { title: String(parsed.flags.title) } : {}),
      ...(parsed.flags.body ? { body: String(parsed.flags.body) } : {}),
      ...(parsed.flags.status ? { status: String(parsed.flags.status) } : {}),
      ...(parsed.flags.grade ? { grade: String(parsed.flags.grade) } : {})
    });
    repo.writeGraph(graph.snapshot());
    console.log(JSON.stringify(node, null, 2));
  } else if (action === 'rm') {
    if (!argv[0]) fail('Usage: modelweave node rm <id>');
    graph.removeNode(argv[0]);
    repo.writeGraph(graph.snapshot());
    console.log(`Removed ${argv[0]}`);
  } else fail('Usage: modelweave node <add|update|rm> ...');
}

async function edgeCommand(repo, argv) {
  const action = argv.shift();
  const graph = repo.graph();
  if (action === 'add') {
    const [from, type, to, weight] = argv;
    if (!from || !type || !to) fail('Usage: modelweave edge add <from> <type> <to> [weight]');
    const edge = graph.addEdge({ from, type, to, weight: weight === undefined ? 1 : Number(weight) });
    repo.writeGraph(graph.snapshot());
    console.log(edge.id);
  } else if (action === 'graft') {
    const [from, type, to, weight, ...reasonParts] = argv;
    if (!from || !type || !to) fail('Usage: modelweave edge graft <from> <type> <to> [weight] [reason]');
    const edge = graph.graftEdge(
      { from, type, to, weight: weight === undefined ? 1 : Number(weight) },
      { reason: reasonParts.join(' ') || 'cli-graft' }
    );
    repo.writeGraph(graph.snapshot());
    console.log(`Grafted ${edge.id}`);
  } else if (action === 'cut') {
    if (!argv[0]) fail('Usage: modelweave edge cut <id> [reason]');
    const edge = graph.cutEdge(argv[0], { reason: argv.slice(1).join(' ') || 'cli-attention-cut' });
    repo.writeGraph(graph.snapshot());
    console.log(`Cut ${edge.id}; edge retained but excluded from attention propagation`);
  } else if (action === 'restore') {
    if (!argv[0]) fail('Usage: modelweave edge restore <id>');
    const edge = graph.restoreEdge(argv[0]);
    repo.writeGraph(graph.snapshot());
    console.log(`Restored ${edge.id}`);
  } else if (action === 'rm') {
    if (!argv[0]) fail('Usage: modelweave edge rm <id>');
    graph.removeEdge(argv[0]);
    repo.writeGraph(graph.snapshot());
    console.log(`Removed ${argv[0]}`);
  } else fail('Usage: modelweave edge <add|graft|cut|restore|rm> ...');
}

function printLight(name, result) {
  console.log(`\n[${name}] ${result.usedTokens}/${result.budgetTokens} estimated tokens`);
  for (const node of result.selectedNodes) console.log(`${node.activation.toFixed(3)} ${String(node.tokenCost).padStart(5)} ${node.id} ${node.kind} ${node.title}`);
}

function parseFlags(argv) {
  const positionals = [];
  const flags = {};
  for (let i = 0; i < argv.length; i += 1) {
    const value = argv[i];
    if (!value.startsWith('--')) { positionals.push(value); continue; }
    const [rawKey, inline] = value.slice(2).split(/=(.*)/s, 2);
    if (inline !== undefined) { flags[rawKey] = inline; continue; }
    const next = argv[i + 1];
    if (next === undefined || next.startsWith('--')) flags[rawKey] = true;
    else { flags[rawKey] = next; i += 1; }
  }
  return { positionals, flags };
}

function findWorkspace(start) {
  let current = path.resolve(start);
  while (true) {
    if (fs.existsSync(path.join(current, '.modelweave'))) return current;
    const parent = path.dirname(current);
    if (parent === current) throw new Error('No .modelweave repository found. Run `modelweave init`.');
    current = parent;
  }
}

function compact(value) {
  const text = JSON.stringify(value);
  return text.length > 240 ? `${text.slice(0, 237)}...` : text;
}

function fail(message) { throw new Error(message); }

function help() {
  console.log(`ModelWeave — cognitive graph + autonomous coding agent\n\nAgent commands:\n  agent <goal> [--provider openrouter|groq|deepseek|generic] [--model MODEL] [--max-steps 24] [--budget 24000] [--recent-rounds 6] [--working-chars 120000] [--timeout-ms 120000] [--llm-retries 2] [--max-tool-calls-per-step N] [--tools read_file,write_file,shell] [--no-auto-promote] [--yes] [--session ID]\n  chat [--provider P] [--model M] [--yes] [--session ID]\n  sessions [--limit 20]\n  providers\n  doctor [--provider P] [--model M] [--live]\n\nCognitive graph commands:\n  init [dir]\n  install-opencode [dir]\n  status\n  commit <message>\n  log [limit]\n  branch [name]\n  checkout <branch>\n  merge <branch>\n  revert <commit>\n  node add|update|rm ...\n  edge add|graft|cut|restore|rm ...\n  ingest [dir] [--chunk-lines 160] [--max-bytes 524288]\n  show [node-or-edge-id]\n  light <goal> [--budget 32000] [--multi] [--json]\n  promote <title> <nodeId> [nodeId...]\n  verify\n\nProviders:\n  OpenRouter free: OPENROUTER_API_KEY + model openrouter/free\n  Groq free:       GROQ_API_KEY + model openai/gpt-oss-120b\n  Generic/local:   MODELWEAVE_BASE_URL, MODELWEAVE_MODEL, MODELWEAVE_API_KEY\n`);
}
