#!/usr/bin/env node
import fs from 'node:fs';
import path from 'node:path';
import readline from 'node:readline/promises';
import { fileURLToPath } from 'node:url';
import { CognitiveRepository } from './repository.js';
import { LumenCortexRuntime } from './runtime.js';
import { graphSummary } from './graph.js';
import { ingestWorkspace } from './ingest.js';
import { createProvider, providerInfo } from './provider.js';
import { AgentLoop } from './agent.js';
import { AgentSessionStore } from './session.js';
import { createCodingTools } from './tools.js';
import { LspManager } from './lsp.js';
import { McpManager } from './mcp.js';
import { SubagentPool, registerSubagentTools } from './subagent.js';
import { ParallelSessionRunner } from './parallel.js';
import { LumenCortexTui } from './tui.js';
import { BRAND } from './brand.js';

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
    console.log(`Initialized LumenCortex in ${repo.dir}`);
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

  let workspace;
  if (command === 'tui') {
    try {
      workspace = findWorkspace(process.cwd());
    } catch {
      workspace = process.cwd();
      const fresh = new CognitiveRepository(workspace);
      if (!fresh.exists()) fresh.init();
    }
  } else {
    workspace = findWorkspace(process.cwd());
  }
  const repo = new CognitiveRepository(workspace);
  const runtime = new LumenCortexRuntime(repo);

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
    case 'tui':
      await tuiCommand({ repo, runtime, workspace, argv: args });
      break;
    case 'parallel':
      await parallelCommand({ repo, runtime, workspace, argv: args });
      break;
    case 'index': {
      const action = args[0] ?? 'stats';
      if (action === 'build') console.log(JSON.stringify(runtime.refreshSearchIndex(), null, 2));
      else if (action === 'stats') console.log(JSON.stringify(runtime.searchIndex.stats(), null, 2));
      else fail('Usage: lcx index <build|stats>');
      break;
    }
    case 'search': {
      const parsed = parseFlags(args);
      const query = parsed.positionals.join(' ').trim();
      if (!query) fail('Usage: lcx search <query> [--limit 40]');
      console.log(JSON.stringify(runtime.search(query, { limit: Number(parsed.flags.limit ?? 40) }), null, 2));
      break;
    }
    case 'lsp':
      await lspCommand({ workspace, argv: args });
      break;
    case 'mcp':
      await mcpCommand({ workspace, argv: args });
      break;
    case 'status': {
      const diff = repo.status();
      console.log(`${diff.operations.length} uncommitted operation(s)`);
      for (const op of diff.operations) console.log(`  ${op.type.padEnd(11)} ${op.id}`);
      break;
    }
    case 'commit': {
      const message = args.join(' ').trim();
      if (!message) fail('Usage: lcx commit <message>');
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
      if (!args[0]) fail('Usage: lcx checkout <branch>');
      const commit = repo.checkout(args[0]);
      console.log(`Switched to ${args[0]} (${commit.id})`);
      break;
    }
    case 'merge': {
      if (!args[0]) fail('Usage: lcx merge <branch>');
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
      if (!args[0]) fail('Usage: lcx revert <commit>');
      const result = repo.revert(args[0]);
      if (result.conflicts.length) {
        console.error(`Revert conflict: ${result.conflicts[0].message}`);
        process.exitCode = 2;
      } else console.log(`Reverted in ${result.commit.id}`);
      break;
    }
    case 'cherry-pick': {
      if (!args[0]) fail('Usage: lcx cherry-pick <commit>');
      const result = repo.cherryPick(args[0]);
      if (result.conflicts.length) {
        console.error('Cherry-pick conflicts:');
        for (const conflict of result.conflicts) console.error(`  ${conflict.kind ?? 'commit'}:${conflict.id ?? conflict.commitId ?? 'unknown'} ${conflict.message ?? ''}`);
        process.exitCode = 2;
      } else console.log(`Cherry-picked as ${result.commit.id}`);
      break;
    }
    case 'blame': {
      if (!args[0]) fail('Usage: lcx blame <node-id> [limit]');
      console.log(JSON.stringify(repo.blame(args[0], { limit: Number(args[1] ?? 20) }), null, 2));
      break;
    }
    case 'rebase': {
      if (!args[0]) fail('Usage: lcx rebase <branch>');
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
      const index = runtime.refreshSearchIndex(result.graph);
      console.log(JSON.stringify({ ...result.stats, searchIndex: index }, null, 2));
      break;
    }
    case 'light': {
      const parsed = parseFlags(args);
      const goal = parsed.positionals.join(' ').trim();
      if (!goal) fail('Usage: lcx light <goal> [--budget 32000] [--multi]');
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
      if (!title || !args.length) fail('Usage: lcx promote <title> <nodeId> [nodeId...]');
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
  console.error(`lcx: ${error.message}`);
  if (error.sessionId) console.error(`session: ${error.sessionId}`);
  process.exitCode = 1;
}

async function agentCommand({ repo, runtime, workspace, argv }) {
  const parsed = parseFlags(argv);
  const goal = parsed.positionals.join(' ').trim();
  if (!goal && !parsed.flags.session) fail('Usage: lcx agent <goal> [--provider openrouter] [--model MODEL] [--yes]');
  const providerName = String(parsed.flags.provider ?? process.env.MODELWEAVE_PROVIDER ?? 'openrouter');
  const provider = createProvider(providerName, providerOptions(parsed));
  const json = Boolean(parsed.flags.json);
  const authorize = createAuthorizer({ yes: Boolean(parsed.flags.yes), policy: String(parsed.flags.policy ?? 'workspace'), json });
  const resources = await createHarness({ repo, runtime, workspace, provider, providerName, parsed, authorize, onEvent: json ? () => {} : renderAgentEvent });
  try {
    const result = await resources.agent.run(goal, {
      ...agentRunOptions(parsed, providerName, authorize),
      sessionId: parsed.flags.session ? String(parsed.flags.session) : undefined
    });
    if (json) console.log(JSON.stringify({ sessionId: result.session.id, final: result.final, usage: result.usage }, null, 2));
    else {
      console.log(`\n${result.final}`);
      console.log(`\n[session ${result.session.id}] requests=${result.usage.requests} tokens=${result.usage.totalTokens}`);
    }
  } finally {
    await resources.close();
  }
}

async function chatCommand({ repo, runtime, workspace, argv }) {
  const parsed = parseFlags(argv);
  const providerName = String(parsed.flags.provider ?? process.env.MODELWEAVE_PROVIDER ?? 'openrouter');
  const provider = createProvider(providerName, providerOptions(parsed));
  const terminal = readline.createInterface({ input: process.stdin, output: process.stdout });
  const authorize = createAuthorizer({ yes: Boolean(parsed.flags.yes), policy: String(parsed.flags.policy ?? 'workspace'), json: false, terminal });
  const resources = await createHarness({ repo, runtime, workspace, provider, providerName, parsed, authorize, onEvent: renderAgentEvent });
  let sessionId = parsed.flags.session ? String(parsed.flags.session) : null;
  console.log(`LumenCortex chat — ${providerName}/${provider.model}. /exit to quit.`);
  try {
    while (true) {
      const goal = (await terminal.question('lcx> ')).trim();
      if (!goal) continue;
      if (['/exit', '/quit'].includes(goal)) break;
      const result = await resources.agent.run(goal, {
        ...agentRunOptions(parsed, providerName, authorize),
        sessionId: sessionId ?? undefined
      });
      sessionId = result.session.id;
      console.log(`\n${result.final}\n`);
    }
  } finally {
    terminal.close();
    await resources.close();
  }
}

async function tuiCommand({ repo, runtime, workspace, argv }) {
  const parsed = parseFlags(argv);
  const providerName = String(parsed.flags.provider ?? process.env.MODELWEAVE_PROVIDER ?? 'openrouter');
  let provider;
  let providerError = null;
  try {
    provider = createProvider(providerName, providerOptions(parsed));
  } catch (error) {
    providerError = error;
    provider = {
      model: '(not configured)',
      async complete() { throw providerError; }
    };
  }
  // Full-screen input and permission prompts cannot own stdin simultaneously.
  // Without --yes, TUI is intentionally read-only.
  const authorize = createAuthorizer({
    yes: Boolean(parsed.flags.yes),
    policy: parsed.flags.yes ? String(parsed.flags.policy ?? 'workspace') : 'read-only',
    json: true
  });
  let tui;
  const resources = await createHarness({
    repo, runtime, workspace, provider, providerName, parsed, authorize,
    onEvent: (event) => tui?.event(event)
  });
  tui = new LumenCortexTui({
    agent: resources.agent,
    sessions: new AgentSessionStore(repo.dir),
    providerLabel: `${providerName}/${provider.model}`,
    parallelRunner: resources.parallelRunner
  });
  if (providerError) tui.events.push(`provider not configured: ${providerError.message}`);
  try {
    await tui.run({
      agentOptions: agentRunOptions(parsed, providerName, authorize),
      parallelOptions: { concurrency: Number(parsed.flags.concurrency ?? 4) }
    });
  } finally {
    await resources.close();
  }
}

async function parallelCommand({ repo, runtime, workspace, argv }) {
  const parsed = parseFlags(argv);
  const file = parsed.positionals[0];
  if (!file) fail('Usage: lcx parallel <tasks.json> [--concurrency 4] [--provider P]');
  const tasks = JSON.parse(fs.readFileSync(path.resolve(file), 'utf8'));
  if (!Array.isArray(tasks)) fail('parallel tasks file must contain a JSON array');

  const mutating = tasks.some((task) =>
    Array.isArray(task?.toolAllowlist) &&
    task.toolAllowlist.some((name) => ['write_file', 'replace_in_file', 'shell', 'lumencortex_ingest'].includes(name))
  );
  const concurrency = Number(parsed.flags.concurrency ?? 4);
  if (mutating && concurrency > 1 && !parsed.flags['unsafe-write-parallel']) {
    fail('Parallel write sessions require --unsafe-write-parallel or concurrency=1');
  }

  const providerName = String(parsed.flags.provider ?? process.env.MODELWEAVE_PROVIDER ?? 'openrouter');
  const provider = createProvider(providerName, providerOptions(parsed));
  const authorize = createAuthorizer({
    yes: Boolean(parsed.flags.yes),
    policy: parsed.flags.yes ? String(parsed.flags.policy ?? 'workspace') : 'read-only',
    json: true
  });
  const resources = await createHarness({ repo, runtime, workspace, provider, providerName, parsed, authorize, onEvent: () => {} });
  try {
    const results = await resources.parallelRunner.runTasks(tasks, {
      concurrency,
      providerName,
      maxSteps: Number(parsed.flags['max-steps'] ?? 12),
      budgetTokens: Number(parsed.flags.budget ?? 12000)
    });
    console.log(JSON.stringify(results, null, 2));
  } finally {
    await resources.close();
  }
}

async function lspCommand({ workspace, argv }) {
  const parsed = parseFlags(argv);
  const action = parsed.positionals.shift() ?? 'status';
  const lsp = new LspManager(workspace, { timeoutMs: Number(parsed.flags.timeout ?? 15000) });
  try {
    if (action === 'status') console.log(JSON.stringify(lsp.status(), null, 2));
    else if (action === 'symbols') console.log(JSON.stringify(await lsp.symbols(parsed.positionals[0]), null, 2));
    else if (['definition', 'references', 'hover'].includes(action)) {
      const [file, line, character] = parsed.positionals;
      if (!file || !line || !character) fail(`Usage: lcx lsp ${action} <file> <line> <character>`);
      const result = action === 'definition'
        ? await lsp.definition(file, Number(line), Number(character))
        : action === 'references'
          ? await lsp.references(file, Number(line), Number(character))
          : await lsp.hover(file, Number(line), Number(character));
      console.log(JSON.stringify(result, null, 2));
    } else if (action === 'diagnostics') console.log(JSON.stringify(await lsp.diagnostics(parsed.positionals[0]), null, 2));
    else fail('Usage: lcx lsp <status|symbols|definition|references|hover|diagnostics> ...');
  } finally {
    await lsp.close();
  }
}

async function mcpCommand({ workspace, argv }) {
  const parsed = parseFlags(argv);
  const action = parsed.positionals.shift() ?? 'status';
  const manager = new McpManager(workspace);
  try {
    if (action === 'status') console.log(JSON.stringify(manager.configuredServers(), null, 2));
    else if (action === 'tools') {
      const server = parsed.positionals[0];
      if (!server) fail('Usage: lcx mcp tools <server>');
      console.log(JSON.stringify(await manager.listTools(server), null, 2));
    } else if (action === 'call') {
      const [server, tool, rawArgs = '{}'] = parsed.positionals;
      if (!server || !tool) fail('Usage: lcx mcp call <server> <tool> [json-args]');
      console.log(JSON.stringify(await manager.callTool(server, tool, JSON.parse(rawArgs)), null, 2));
    } else fail('Usage: lcx mcp <status|tools|call> ...');
  } finally {
    await manager.close();
  }
}

async function createHarness({ repo, runtime, workspace, provider, providerName, parsed, authorize, onEvent }) {
  const lsp = new LspManager(workspace, { timeoutMs: Number(parsed.flags['lsp-timeout'] ?? 15000) });
  const mcp = new McpManager(workspace);
  const toolRegistry = createCodingTools({ workspace, repository: repo, runtime, lsp });

  if (!parsed.flags['no-mcp']) {
    try {
      await mcp.registerTools(toolRegistry);
    } catch (error) {
      if (parsed.flags['strict-mcp']) throw error;
      if (!parsed.flags.json) console.error(`[mcp] ${error.message}`);
    }
  }

  const pool = new SubagentPool({
    provider, repository: repo, runtime, workspace, tools: toolRegistry, authorize,
    onEvent, concurrency: Number(parsed.flags.concurrency ?? 4)
  });
  registerSubagentTools(toolRegistry, pool);

  const agent = new AgentLoop({
    provider, repository: repo, runtime, workspace, tools: toolRegistry, authorize, onEvent
  });
  const parallelRunner = new ParallelSessionRunner({
    subagentPool: pool,
    concurrency: Number(parsed.flags.concurrency ?? 4)
  });

  return {
    agent,
    pool,
    parallelRunner,
    tools: toolRegistry,
    lsp,
    mcp,
    async close() {
      await Promise.allSettled([lsp.close(), mcp.close()]);
    }
  };
}

function providerOptions(parsed) {
  return {
    model: parsed.flags.model ? String(parsed.flags.model) : undefined,
    baseURL: parsed.flags['base-url'] ? String(parsed.flags['base-url']) : undefined,
    timeoutMs: parsed.flags['timeout-ms'] ? Number(parsed.flags['timeout-ms']) : undefined
  };
}

function agentRunOptions(parsed, providerName, authorize) {
  return {
    providerName,
    maxSteps: Number(parsed.flags['max-steps'] ?? 24),
    budgetTokens: Number(parsed.flags.budget ?? 24000),
    maxTokens: parsed.flags['max-tokens'] ? Number(parsed.flags['max-tokens']) : undefined,
    llmRetries: Number(parsed.flags['llm-retries'] ?? 2),
    retryBaseMs: Number(parsed.flags['retry-base-ms'] ?? 800),
    maxToolCallsPerStep: Number(parsed.flags['max-tool-calls-per-step'] ?? Number.MAX_SAFE_INTEGER),
    toolAllowlist: parsed.flags.tools ? String(parsed.flags.tools).split(',').map((x) => x.trim()).filter(Boolean) : undefined,
    recentRounds: Number(parsed.flags['recent-rounds'] ?? 6),
    workingChars: Number(parsed.flags['working-chars'] ?? 120000),
    autoPromote: parsed.flags['no-auto-promote'] ? false : true,
    autoIngest: parsed.flags['no-ingest'] ? false : true,
    cognitiveCommit: Boolean(parsed.flags['cognitive-commit']),
    authorize
  };
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
    if (!kind || !title) fail('Usage: lcx node add <kind> <title> [body]');
    const node = graph.addNode({ kind, title, body: bodyParts.join(' '), grade: parsed.flags.grade, trustZone: parsed.flags.trust, tags: parsed.flags.tags ? String(parsed.flags.tags).split(',') : undefined, source: parsed.flags.source ? { uri: String(parsed.flags.source) } : undefined, evidenceIds: parsed.flags.evidence ? String(parsed.flags.evidence).split(',') : undefined });
    repo.writeGraph(graph.snapshot());
    console.log(node.id);
  } else if (action === 'update') {
    const parsed = parseFlags(argv);
    const id = parsed.positionals.shift();
    if (!id) fail('Usage: lcx node update <id>');
    const node = graph.updateNode(id, {
      ...(parsed.flags.title ? { title: String(parsed.flags.title) } : {}),
      ...(parsed.flags.body ? { body: String(parsed.flags.body) } : {}),
      ...(parsed.flags.status ? { status: String(parsed.flags.status) } : {}),
      ...(parsed.flags.grade ? { grade: String(parsed.flags.grade) } : {})
    });
    repo.writeGraph(graph.snapshot());
    console.log(JSON.stringify(node, null, 2));
  } else if (action === 'rm') {
    if (!argv[0]) fail('Usage: lcx node rm <id>');
    graph.removeNode(argv[0]);
    repo.writeGraph(graph.snapshot());
    console.log(`Removed ${argv[0]}`);
  } else fail('Usage: lcx node <add|update|rm> ...');
}

async function edgeCommand(repo, argv) {
  const action = argv.shift();
  const graph = repo.graph();
  if (action === 'add') {
    const [from, type, to, weight] = argv;
    if (!from || !type || !to) fail('Usage: lcx edge add <from> <type> <to> [weight]');
    const edge = graph.addEdge({ from, type, to, weight: weight === undefined ? 1 : Number(weight) });
    repo.writeGraph(graph.snapshot());
    console.log(edge.id);
  } else if (action === 'graft') {
    const [from, type, to, weight, ...reasonParts] = argv;
    if (!from || !type || !to) fail('Usage: lcx edge graft <from> <type> <to> [weight] [reason]');
    const edge = graph.graftEdge(
      { from, type, to, weight: weight === undefined ? 1 : Number(weight) },
      { reason: reasonParts.join(' ') || 'cli-graft' }
    );
    repo.writeGraph(graph.snapshot());
    console.log(`Grafted ${edge.id}`);
  } else if (action === 'cut') {
    if (!argv[0]) fail('Usage: lcx edge cut <id> [reason]');
    const edge = graph.cutEdge(argv[0], { reason: argv.slice(1).join(' ') || 'cli-attention-cut' });
    repo.writeGraph(graph.snapshot());
    console.log(`Cut ${edge.id}; edge retained but excluded from attention propagation`);
  } else if (action === 'restore') {
    if (!argv[0]) fail('Usage: lcx edge restore <id>');
    const edge = graph.restoreEdge(argv[0]);
    repo.writeGraph(graph.snapshot());
    console.log(`Restored ${edge.id}`);
  } else if (action === 'rm') {
    if (!argv[0]) fail('Usage: lcx edge rm <id>');
    graph.removeEdge(argv[0]);
    repo.writeGraph(graph.snapshot());
    console.log(`Removed ${argv[0]}`);
  } else fail('Usage: lcx edge <add|graft|cut|restore|rm> ...');
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
    if (fs.existsSync(path.join(current, '.lumencortex')) || fs.existsSync(path.join(current, '.modelweave'))) return current;
    const parent = path.dirname(current);
    if (parent === current) throw new Error('No .lcx repository found. Run `lcx init`.');
    current = parent;
  }
}

function compact(value) {
  const text = JSON.stringify(value);
  return text.length > 240 ? `${text.slice(0, 237)}...` : text;
}

function fail(message) { throw new Error(message); }

function help() {
  console.log(`LumenCortex — cognitive graph + autonomous coding harness

Agent commands:
  agent <goal> [--provider P] [--model M] [--yes] [--session ID]
  chat [--provider P] [--model M] [--yes] [--session ID]
  tui [--provider P] [--model M] [--yes]
  parallel <tasks.json> [--concurrency 4] [--unsafe-write-parallel]
  sessions [--limit 20]
  providers
  doctor [--provider P] [--model M] [--live]

Code intelligence:
  ingest [dir] [--chunk-lines 160] [--max-bytes 524288]
  index <build|stats>
  search <query> [--limit 40]
  lsp <status|symbols|definition|references|hover|diagnostics> ...
  mcp <status|tools|call> ...

Cognitive graph:
  init [dir]
  install-opencode [dir]
  status
  commit <message>
  log [limit]
  branch [name]
  checkout <branch>
  merge <branch>
  revert <commit>
  cherry-pick <commit>
  blame <node-id> [limit]
  rebase <branch>
  node add|update|rm ...
  edge add|graft|cut|restore|rm ...
  show [node-or-edge-id]
  light <goal> [--budget 32000] [--multi] [--json]
  promote <title> <nodeId> [nodeId...]
  verify

Agent controls:
  --max-steps 24
  --budget 24000
  --recent-rounds 6
  --working-chars 120000
  --timeout-ms 120000
  --llm-retries 2
  --max-tool-calls-per-step N
  --tools read_file,code_search,lsp_definition,...
  --no-mcp
  --strict-mcp
  --no-auto-promote
  --yes

Providers:
  OpenRouter free: OPENROUTER_API_KEY + openrouter/free
  DeepSeek free:  OPENROUTER_API_KEY + --provider openrouter-deepseek-free
  Groq free:      GROQ_API_KEY + openai/gpt-oss-120b
  Generic/local:  MODELWEAVE_BASE_URL, MODELWEAVE_MODEL, MODELWEAVE_API_KEY
`);
}
