#!/usr/bin/env node
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { CognitiveRepository } from './repository.js';
import { ModelWeaveRuntime } from './runtime.js';
import { graphSummary } from './graph.js';
import { ingestWorkspace } from './ingest.js';

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

  const workspace = findWorkspace(process.cwd());
  const repo = new CognitiveRepository(workspace);
  const runtime = new ModelWeaveRuntime(repo);

  switch (command) {
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
      for (const commit of repo.log(limit)) {
        console.log(`${commit.id} ${commit.createdAt} ${commit.message}`);
      }
      break;
    }
    case 'branch': {
      if (!args[0]) {
        for (const branch of repo.branches()) console.log(`${branch.current ? '*' : ' '} ${branch.name} ${branch.commitId}`);
      } else {
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
      } else if (result.alreadyUpToDate) {
        console.log('Already up to date');
      } else {
        console.log(`Merged as ${result.commit.id}`);
      }
      break;
    }
    case 'revert': {
      if (!args[0]) fail('Usage: modelweave revert <commit>');
      const result = repo.revert(args[0]);
      if (result.conflicts.length) {
        console.error(`Revert conflict: ${result.conflicts[0].message}`);
        process.exitCode = 2;
      } else {
        console.log(`Reverted in ${result.commit.id}`);
      }
      break;
    }
    case 'node': {
      await nodeCommand(repo, args);
      break;
    }
    case 'edge': {
      await edgeCommand(repo, args);
      break;
    }
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
      console.log(JSON.stringify({
        staleEvidence: result.staleEvidence,
        dirtiedBeliefs: result.dirtiedBeliefs,
        issues: result.issues
      }, null, 2));
      break;
    }
    default:
      fail(`Unknown command: ${command}`);
  }
} catch (error) {
  console.error(`modelweave: ${error.message}`);
  process.exitCode = 1;
}

async function nodeCommand(repo, args) {
  const action = args.shift();
  const graph = repo.graph();
  if (action === 'add') {
    const parsed = parseFlags(args);
    const [kind, title, ...bodyParts] = parsed.positionals;
    if (!kind || !title) fail('Usage: modelweave node add <kind> <title> [body] [--grade static] [--trust repo_trusted]');
    const node = graph.addNode({
      kind,
      title,
      body: bodyParts.join(' '),
      grade: parsed.flags.grade,
      trustZone: parsed.flags.trust,
      tags: parsed.flags.tags ? String(parsed.flags.tags).split(',') : undefined,
      source: parsed.flags.source ? { uri: String(parsed.flags.source) } : undefined,
      evidenceIds: parsed.flags.evidence ? String(parsed.flags.evidence).split(',') : undefined
    });
    repo.writeGraph(graph.snapshot());
    console.log(node.id);
  } else if (action === 'update') {
    const parsed = parseFlags(args);
    const id = parsed.positionals.shift();
    if (!id) fail('Usage: modelweave node update <id> [--title x] [--body y] [--status stale]');
    const node = graph.updateNode(id, {
      ...(parsed.flags.title ? { title: String(parsed.flags.title) } : {}),
      ...(parsed.flags.body ? { body: String(parsed.flags.body) } : {}),
      ...(parsed.flags.status ? { status: String(parsed.flags.status) } : {}),
      ...(parsed.flags.grade ? { grade: String(parsed.flags.grade) } : {})
    });
    repo.writeGraph(graph.snapshot());
    console.log(JSON.stringify(node, null, 2));
  } else if (action === 'rm') {
    const id = args[0];
    if (!id) fail('Usage: modelweave node rm <id>');
    graph.removeNode(id);
    repo.writeGraph(graph.snapshot());
    console.log(`Removed ${id}`);
  } else {
    fail('Usage: modelweave node <add|update|rm> ...');
  }
}

async function edgeCommand(repo, args) {
  const action = args.shift();
  const graph = repo.graph();
  if (action === 'add') {
    const [from, type, to, weight] = args;
    if (!from || !type || !to) fail('Usage: modelweave edge add <from> <type> <to> [weight]');
    const edge = graph.addEdge({ from, type, to, weight: weight === undefined ? 1 : Number(weight) });
    repo.writeGraph(graph.snapshot());
    console.log(edge.id);
  } else if (action === 'rm') {
    if (!args[0]) fail('Usage: modelweave edge rm <id>');
    graph.removeEdge(args[0]);
    repo.writeGraph(graph.snapshot());
    console.log(`Removed ${args[0]}`);
  } else {
    fail('Usage: modelweave edge <add|rm> ...');
  }
}

function printLight(name, result) {
  console.log(`\n[${name}] ${result.usedTokens}/${result.budgetTokens} estimated tokens`);
  for (const node of result.selectedNodes) {
    console.log(`${node.activation.toFixed(3)} ${String(node.tokenCost).padStart(5)} ${node.id} ${node.kind} ${node.title}`);
  }
}

function parseFlags(argv) {
  const positionals = [];
  const flags = {};
  for (let i = 0; i < argv.length; i += 1) {
    const value = argv[i];
    if (!value.startsWith('--')) {
      positionals.push(value);
      continue;
    }
    const key = value.slice(2);
    const next = argv[i + 1];
    if (next === undefined || next.startsWith('--')) flags[key] = true;
    else {
      flags[key] = next;
      i += 1;
    }
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

function fail(message) {
  throw new Error(message);
}

function help() {
  console.log(`ModelWeave — versioned cognitive graph runtime\n\nCommands:\n  init [dir]\n  install-opencode [dir]\n  status\n  commit <message>\n  log [limit]\n  branch [name]\n  checkout <branch>\n  merge <branch>\n  revert <commit>\n  node add <kind> <title> [body] [--grade G] [--trust Z] [--tags a,b] [--source URI] [--evidence id,id]\n  node update <id> [--title x] [--body y] [--status S] [--grade G]\n  node rm <id>\n  ingest [dir] [--chunk-lines 160] [--max-bytes 524288]\n  edge add <from> <type> <to> [weight]\n  edge rm <id>\n  show [node-or-edge-id]\n  light <goal> [--budget 32000] [--multi] [--json]\n  promote <title> <nodeId> [nodeId...]\n  verify\n`);
}
