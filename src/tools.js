import fs from 'node:fs';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import { ingestWorkspace } from './ingest.js';

const DEFAULT_IGNORES = new Set(['.git', '.modelweave', 'node_modules', 'dist', 'build', 'target', '.next', 'vendor']);

export class ToolRegistry {
  constructor() {
    this.tools = new Map();
  }

  register(tool) {
    if (!tool?.name || typeof tool.execute !== 'function') throw new Error('Invalid tool registration');
    if (this.tools.has(tool.name)) throw new Error(`Tool already registered: ${tool.name}`);
    this.tools.set(tool.name, tool);
    return this;
  }

  schemas() {
    return [...this.tools.values()].map((tool) => ({
      type: 'function',
      function: {
        name: tool.name,
        description: tool.description ?? '',
        parameters: tool.parameters ?? { type: 'object', properties: {} }
      }
    }));
  }

  get(name) {
    const tool = this.tools.get(name);
    if (!tool) throw new Error(`Unknown tool: ${name}`);
    return tool;
  }

  async execute(name, args, context = {}) {
    const tool = this.get(name);
    const normalizedArgs = normalizeArguments(tool, args ?? {});
    if (context.authorize && !(await context.authorize(tool, normalizedArgs))) {
      return { ok: false, denied: true, content: `Permission denied for tool ${name}` };
    }
    try {
      const value = await tool.execute(normalizedArgs, context);
      return { ok: true, mutatesWorkspace: Boolean(tool.mutatesWorkspace), permission: tool.permission ?? 'read', content: truncate(value) };
    } catch (error) {
      return { ok: false, mutatesWorkspace: Boolean(tool.mutatesWorkspace), permission: tool.permission ?? 'read', content: truncate({ error: error.message }) };
    }
  }
}

export function createCodingTools({ workspace, repository, runtime, shellTimeoutMs = 120000 } = {}) {
  if (!workspace) throw new Error('workspace is required');
  const root = path.resolve(workspace);
  const registry = new ToolRegistry();

  registry.register({
    name: 'read_file',
    description: 'Read a UTF-8 file inside the workspace. Use line ranges for large files.',
    permission: 'read',
    parameters: {
      type: 'object',
      properties: {
        path: { type: 'string' },
        start_line: { type: 'integer', minimum: 1 },
        end_line: { type: 'integer', minimum: 1 }
      },
      required: ['path'],
      additionalProperties: false
    },
    execute({ path: input, start_line = 1, end_line }) {
      const file = resolveInside(root, input);
      const lines = fs.readFileSync(file, 'utf8').split(/\r?\n/);
      const start = Math.max(1, start_line);
      const end = Math.min(lines.length, end_line ?? Math.min(lines.length, start + 399));
      return {
        path: normalize(path.relative(root, file)),
        startLine: start,
        endLine: end,
        totalLines: lines.length,
        content: lines.slice(start - 1, end).map((line, index) => `${start + index}: ${line}`).join('\n')
      };
    }
  });

  registry.register({
    name: 'list_dir',
    description: 'List files and directories inside the workspace.',
    permission: 'read',
    parameters: {
      type: 'object',
      properties: { path: { type: 'string' }, max_entries: { type: 'integer', minimum: 1, maximum: 2000 } },
      additionalProperties: false
    },
    execute({ path: input = '.', max_entries = 500 }) {
      const dir = resolveInside(root, input);
      return fs.readdirSync(dir, { withFileTypes: true }).slice(0, max_entries).map((entry) => ({
        name: entry.name,
        type: entry.isDirectory() ? 'directory' : entry.isFile() ? 'file' : 'other'
      }));
    }
  });

  registry.register({
    name: 'search_text',
    description: 'Search text recursively in workspace files. Returns matching file, line and excerpt.',
    permission: 'read',
    parameters: {
      type: 'object',
      properties: {
        query: { type: 'string' },
        path: { type: 'string' },
        max_results: { type: 'integer', minimum: 1, maximum: 500 },
        case_sensitive: { type: 'boolean' }
      },
      required: ['query'],
      additionalProperties: false
    },
    execute({ query, path: input = '.', max_results = 100, case_sensitive = false }) {
      const base = resolveInside(root, input);
      const needle = case_sensitive ? query : query.toLowerCase();
      const results = [];
      for (const file of walkFiles(base)) {
        let text;
        try { text = fs.readFileSync(file, 'utf8'); } catch { continue; }
        if (text.includes('\0')) continue;
        const lines = text.split(/\r?\n/);
        for (let i = 0; i < lines.length; i += 1) {
          const haystack = case_sensitive ? lines[i] : lines[i].toLowerCase();
          if (!haystack.includes(needle)) continue;
          results.push({ path: normalize(path.relative(root, file)), line: i + 1, text: lines[i].slice(0, 500) });
          if (results.length >= max_results) return results;
        }
      }
      return results;
    }
  });

  registry.register({
    name: 'write_file',
    description: 'Create or completely replace a UTF-8 file inside the workspace.',
    permission: 'write',
    mutatesWorkspace: true,
    parameters: {
      type: 'object',
      properties: { path: { type: 'string' }, content: { type: 'string' } },
      required: ['path', 'content'],
      additionalProperties: false
    },
    execute({ path: input, content }) {
      const file = resolveInside(root, input);
      fs.mkdirSync(path.dirname(file), { recursive: true });
      fs.writeFileSync(file, content, 'utf8');
      return { path: normalize(path.relative(root, file)), bytes: Buffer.byteLength(content) };
    }
  });

  registry.register({
    name: 'replace_in_file',
    description: 'Replace one exact text occurrence in a UTF-8 workspace file. Fails if old_text is absent or ambiguous.',
    permission: 'write',
    mutatesWorkspace: true,
    parameters: {
      type: 'object',
      properties: {
        path: { type: 'string' },
        old_text: { type: 'string' },
        new_text: { type: 'string' }
      },
      required: ['path', 'old_text', 'new_text'],
      additionalProperties: false
    },
    execute({ path: input, old_text, new_text }) {
      const file = resolveInside(root, input);
      const text = fs.readFileSync(file, 'utf8');
      const first = text.indexOf(old_text);
      if (first < 0) throw new Error('old_text not found');
      if (text.indexOf(old_text, first + old_text.length) >= 0) throw new Error('old_text is ambiguous; provide a larger unique block');
      const next = `${text.slice(0, first)}${new_text}${text.slice(first + old_text.length)}`;
      fs.writeFileSync(file, next, 'utf8');
      return { path: normalize(path.relative(root, file)), replaced: true };
    }
  });

  registry.register({
    name: 'shell',
    description: 'Run a shell command in the workspace. Use for tests, builds, git diff/status, and deterministic inspection.',
    permission: 'exec',
    mutatesWorkspace: true,
    parameters: {
      type: 'object',
      properties: { command: { type: 'string' }, timeout_ms: { type: 'integer', minimum: 1000, maximum: 600000 } },
      required: ['command'],
      additionalProperties: false
    },
    execute({ command, timeout_ms = shellTimeoutMs }) {
      const isWindows = process.platform === 'win32';
      const result = spawnSync(isWindows ? 'cmd.exe' : '/bin/sh', isWindows ? ['/d', '/s', '/c', command] : ['-lc', command], {
        cwd: root,
        encoding: 'utf8',
        timeout: timeout_ms,
        maxBuffer: 4 * 1024 * 1024
      });
      return {
        command,
        exitCode: result.status,
        signal: result.signal,
        stdout: result.stdout ?? '',
        stderr: result.stderr ?? '',
        timedOut: Boolean(result.error?.code === 'ETIMEDOUT')
      };
    }
  });

  if (runtime) {
    registry.register({
      name: 'modelweave_context',
      description: 'Illuminate the persistent cognitive graph for a focused sub-question.',
      permission: 'read',
      parameters: {
        type: 'object',
        properties: { goal: { type: 'string' }, budget_tokens: { type: 'integer', minimum: 1000, maximum: 200000 }, multi: { type: 'boolean' } },
        required: ['goal'],
        additionalProperties: false
      },
      execute({ goal, budget_tokens = 16000, multi = false }) {
        return multi ? runtime.contextMulti(goal, { budgetTokens: budget_tokens }) : runtime.context(goal, { budgetTokens: budget_tokens });
      }
    });
  }

  if (repository) {
    registry.register({
      name: 'modelweave_ingest',
      description: 'Refresh repository reality/evidence nodes after meaningful workspace changes.',
      permission: 'write',
      parameters: { type: 'object', properties: {}, additionalProperties: false },
      execute() {
        const result = ingestWorkspace(repository.graph().snapshot(), root);
        repository.writeGraph(result.graph);
        return result.stats;
      }
    });
  }

  return registry;
}

export function resolveInside(root, input) {
  const resolvedRoot = path.resolve(root);
  const resolved = path.resolve(resolvedRoot, input || '.');
  const relative = path.relative(resolvedRoot, resolved);
  if (relative.startsWith('..') || path.isAbsolute(relative)) throw new Error(`Path escapes workspace: ${input}`);
  return resolved;
}

function *walkFiles(root) {
  const stack = [root];
  while (stack.length) {
    const current = stack.pop();
    const stat = fs.statSync(current);
    if (stat.isFile()) { yield current; continue; }
    for (const entry of fs.readdirSync(current, { withFileTypes: true })) {
      if (DEFAULT_IGNORES.has(entry.name)) continue;
      const full = path.join(current, entry.name);
      if (entry.isDirectory()) stack.push(full);
      else if (entry.isFile() && fs.statSync(full).size <= 1024 * 1024) yield full;
    }
  }
}

function truncate(value, maxChars = 24000) {
  const text = typeof value === 'string' ? value : JSON.stringify(value, null, 2);
  if (text.length <= maxChars) return text;
  return `${text.slice(0, maxChars)}\n... [truncated ${text.length - maxChars} chars]`;
}

function normalize(value) {
  return value.split(path.sep).join('/');
}


function normalizeArguments(tool, args) {
  if (!args || typeof args !== 'object' || Array.isArray(args)) return args;
  const properties = tool.parameters?.properties ?? {};
  const normalized = { ...args };
  for (const key of Object.keys(args)) {
    const snake = key.replace(/[A-Z]/g, (letter) => `_${letter.toLowerCase()}`);
    if (snake !== key && properties[snake] && normalized[snake] === undefined) {
      normalized[snake] = args[key];
      delete normalized[key];
    }
  }
  return normalized;
}
