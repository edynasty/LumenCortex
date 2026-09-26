import fs from 'node:fs';
import path from 'node:path';
import { spawn } from 'node:child_process';
import { ingestWorkspace } from './ingest.js';
import { locationToWorkspace } from './lsp.js';

const DEFAULT_IGNORES = new Set(['.git', '.lumencortex', 'node_modules', 'dist', 'build', 'target', '.next', 'vendor']);

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

  schemas(allowlist) {
    const selected = allowlist
      ? [...new Set(allowlist)].map((name) => this.get(name))
      : [...this.tools.values()].filter((tool) => !tool.hidden);
    return selected.map((tool) => ({
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
      if (context.signal?.aborted || error?.name === 'AbortError') throw error;
      return { ok: false, mutatesWorkspace: Boolean(tool.mutatesWorkspace), permission: tool.permission ?? 'read', content: truncate({ error: error.message }) };
    }
  }
}

export function createCodingTools({ workspace, repository, runtime, lsp, shellTimeoutMs = 120000 } = {}) {
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
    name: 'read_files',
    description: 'Batch-read multiple known UTF-8 workspace files in one tool call. Prefer this over repeated read_file calls when several relevant paths are already known.',
    permission: 'read',
    parameters: {
      type: 'object',
      properties: {
        paths: {
          type: 'array',
          minItems: 1,
          maxItems: 32,
          items: { type: 'string' }
        },
        max_lines_per_file: { type: 'integer', minimum: 1, maximum: 800 }
      },
      required: ['paths'],
      additionalProperties: false
    },
    execute({ paths, max_lines_per_file = 400 }) {
      const seen = new Set();
      return paths.map((input) => {
        const file = resolveInside(root, input);
        const relative = normalize(path.relative(root, file));
        if (seen.has(file)) throw new Error(`duplicate read path: ${relative}`);
        seen.add(file);
        const lines = fs.readFileSync(file, 'utf8').split(/\r?\n/);
        const end = Math.min(lines.length, max_lines_per_file);
        return {
          path: relative,
          startLine: 1,
          endLine: end,
          totalLines: lines.length,
          truncated: end < lines.length,
          content: lines.slice(0, end).map((line, index) => `${index + 1}: ${line}`).join('\n')
        };
      });
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
    name: 'apply_patch',
    description: 'Apply a validated batch of exact text edits, file creates, or file deletes inside the workspace. All patches are validated before any file is mutated. Prefer this for multi-hunk or multi-file edits.',
    permission: 'write',
    mutatesWorkspace: true,
    parameters: {
      type: 'object',
      properties: {
        patches: {
          type: 'array',
          minItems: 1,
          items: {
            type: 'object',
            properties: {
              path: { type: 'string' },
              operation: { type: 'string', enum: ['update', 'create', 'delete'] },
              edits: {
                type: 'array',
                items: {
                  type: 'object',
                  properties: {
                    old_text: { type: 'string' },
                    new_text: { type: 'string' }
                  },
                  required: ['old_text', 'new_text'],
                  additionalProperties: false
                }
              },
              content: { type: 'string' }
            },
            required: ['path', 'operation'],
            additionalProperties: false
          }
        }
      },
      required: ['patches'],
      additionalProperties: false
    },
    execute({ patches }) {
      if (!Array.isArray(patches) || patches.length === 0) throw new Error('patches must contain at least one patch');
      const seen = new Set();
      const plans = [];

      for (const patch of patches) {
        const file = resolveInside(root, patch.path);
        const relative = normalize(path.relative(root, file));
        if (seen.has(file)) throw new Error(`duplicate patch path: ${relative}`);
        seen.add(file);

        if (patch.operation === 'update') {
          if (!fs.existsSync(file) || !fs.statSync(file).isFile()) throw new Error(`update target not found: ${relative}`);
          if (!Array.isArray(patch.edits) || patch.edits.length === 0) throw new Error(`update requires edits: ${relative}`);
          const original = fs.readFileSync(file, 'utf8');
          const next = applyExactEdits(original, patch.edits, relative);
          plans.push({ operation: 'update', file, relative, original, next });
        } else if (patch.operation === 'create') {
          if (fs.existsSync(file)) throw new Error(`create target already exists: ${relative}`);
          if (typeof patch.content !== 'string') throw new Error(`create requires content: ${relative}`);
          plans.push({ operation: 'create', file, relative, original: null, next: patch.content });
        } else if (patch.operation === 'delete') {
          if (!fs.existsSync(file) || !fs.statSync(file).isFile()) throw new Error(`delete target not found: ${relative}`);
          plans.push({ operation: 'delete', file, relative, original: fs.readFileSync(file, 'utf8'), next: null });
        } else {
          throw new Error(`unsupported patch operation: ${patch.operation}`);
        }
      }

      const applied = [];
      try {
        for (const plan of plans) {
          if (plan.operation === 'delete') {
            fs.unlinkSync(plan.file);
          } else {
            fs.mkdirSync(path.dirname(plan.file), { recursive: true });
            fs.writeFileSync(plan.file, plan.next, 'utf8');
          }
          applied.push(plan);
        }
      } catch (error) {
        for (const plan of applied.reverse()) {
          try {
            if (plan.operation === 'create') fs.rmSync(plan.file, { force: true });
            else {
              fs.mkdirSync(path.dirname(plan.file), { recursive: true });
              fs.writeFileSync(plan.file, plan.original, 'utf8');
            }
          } catch {}
        }
        throw error;
      }

      return plans.map((plan) => ({
        path: plan.relative,
        operation: plan.operation,
        bytes: plan.next === null ? 0 : Buffer.byteLength(plan.next)
      }));
    }
  });

  registry.register({
    name: 'shell',
    description: 'Run a shell command in the workspace. Use for tests, builds, git diff/status, and deterministic inspection.',
    permission: 'exec',
    scope: 'host',
    mutatesWorkspace: true,
    parameters: {
      type: 'object',
      properties: { command: { type: 'string' }, timeout_ms: { type: 'integer', minimum: 1000, maximum: 600000 } },
      required: ['command'],
      additionalProperties: false
    },
    async execute({ command, timeout_ms = shellTimeoutMs }, { signal, onOutput } = {}) {
      const isWindows = process.platform === 'win32';
      const executable = isWindows ? 'cmd.exe' : '/bin/sh';
      const args = isWindows ? ['/d', '/s', '/c', command] : ['-lc', command];
      const maxBuffer = 4 * 1024 * 1024;

      return await new Promise((resolve, reject) => {
        const child = spawn(executable, args, {
          cwd: root,
          stdio: ['ignore', 'pipe', 'pipe'],
          windowsHide: true,
          detached: !isWindows
        });
        let stdout = '';
        let stderr = '';
        let timedOut = false;
        let overflowed = false;
        let settled = false;
        let cancelled = false;
        let timer;

        const finish = (value, error) => {
          if (settled) return;
          settled = true;
          clearTimeout(timer);
          signal?.removeEventListener?.('abort', abort);
          if (error) reject(error);
          else resolve(value);
        };

        const append = (current, chunk) => {
          const next = current + chunk.toString('utf8');
          if (Buffer.byteLength(next) > maxBuffer) {
            overflowed = true;
            terminateProcessTree(child, isWindows, 'SIGTERM');
            return next.slice(0, maxBuffer);
          }
          return next;
        };

        child.stdout.on('data', (chunk) => {
          stdout = append(stdout, chunk);
          onOutput?.({ stream: 'stdout', chunk: chunk.toString('utf8') });
        });
        child.stderr.on('data', (chunk) => {
          stderr = append(stderr, chunk);
          onOutput?.({ stream: 'stderr', chunk: chunk.toString('utf8') });
        });
        const abort = () => {
          cancelled = true;
          terminateProcessTree(child, isWindows, 'SIGTERM');
          setTimeout(() => {
            if (!settled) terminateProcessTree(child, isWindows, 'SIGKILL');
          }, 1000).unref?.();
        };

        child.on('error', (error) => finish(null, error));
        child.on('close', (exitCode, processSignal) => {
          if (cancelled) {
            const error = new Error('Shell command cancelled');
            error.name = 'AbortError';
            finish(null, error);
            return;
          }
          finish({
            command,
            exitCode,
            signal: processSignal,
            stdout,
            stderr: overflowed
              ? `${stderr}\n[LumenCortex shell output exceeded ${maxBuffer} bytes and the process was terminated]`
              : stderr,
            timedOut,
            overflowed
          });
        });

        if (signal?.aborted) abort();
        else signal?.addEventListener?.('abort', abort, { once: true });

        timer = setTimeout(() => {
          timedOut = true;
          terminateProcessTree(child, isWindows, 'SIGTERM');
          setTimeout(() => {
            if (!settled) terminateProcessTree(child, isWindows, 'SIGKILL');
          }, 1000).unref?.();
        }, timeout_ms);
        timer.unref?.();
      });
    }
  });

  if (runtime) {
    registry.register({
      name: 'code_search',
      description: 'Fast indexed code/repository search using BM25, symbol matches and graph-backed node IDs. Prefer this over recursive text scanning in large repositories.',
      permission: 'read',
      parameters: {
        type: 'object',
        properties: {
          query: { type: 'string' },
          max_results: { type: 'integer', minimum: 1, maximum: 200 }
        },
        required: ['query'],
        additionalProperties: false
      },
      execute({ query, max_results = 40 }) {
        return runtime.search(query, { limit: max_results });
      }
    });

    registry.register({
      name: 'lumencortex_context',
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


  if (lsp) {
    registry.register({
      name: 'lsp_definition',
      description: 'Resolve a symbol definition using the configured language server.',
      permission: 'read',
      parameters: {
        type: 'object',
        properties: {
          path: { type: 'string' },
          line: { type: 'integer', minimum: 1 },
          character: { type: 'integer', minimum: 1 }
        },
        required: ['path', 'line', 'character'],
        additionalProperties: false
      },
      async execute({ path: input, line, character }) {
        const value = await lsp.definition(input, line, character);
        return normalizeLspLocations(value, root);
      }
    });

    registry.register({
      name: 'lsp_references',
      description: 'Find symbol references using the configured language server.',
      permission: 'read',
      parameters: {
        type: 'object',
        properties: {
          path: { type: 'string' },
          line: { type: 'integer', minimum: 1 },
          character: { type: 'integer', minimum: 1 },
          include_declaration: { type: 'boolean' }
        },
        required: ['path', 'line', 'character'],
        additionalProperties: false
      },
      async execute({ path: input, line, character, include_declaration = true }) {
        const value = await lsp.references(input, line, character, include_declaration);
        return normalizeLspLocations(value, root);
      }
    });

    registry.register({
      name: 'lsp_symbols',
      description: 'List structured symbols in a source file using the configured language server.',
      permission: 'read',
      parameters: {
        type: 'object',
        properties: { path: { type: 'string' } },
        required: ['path'],
        additionalProperties: false
      },
      execute: ({ path: input }) => lsp.symbols(input)
    });

    registry.register({
      name: 'lsp_hover',
      description: 'Get type/signature/hover information at a source position.',
      permission: 'read',
      parameters: {
        type: 'object',
        properties: {
          path: { type: 'string' },
          line: { type: 'integer', minimum: 1 },
          character: { type: 'integer', minimum: 1 }
        },
        required: ['path', 'line', 'character'],
        additionalProperties: false
      },
      execute: ({ path: input, line, character }) => lsp.hover(input, line, character)
    });

    registry.register({
      name: 'lsp_diagnostics',
      description: 'Get language-server diagnostics for a source file.',
      permission: 'read',
      parameters: {
        type: 'object',
        properties: { path: { type: 'string' } },
        required: ['path'],
        additionalProperties: false
      },
      execute: ({ path: input }) => lsp.diagnostics(input)
    });

    registry.register({
      name: 'lsp_rename',
      description: 'Rename a symbol through the language server and atomically apply its validated WorkspaceEdit.',
      permission: 'write',
      mutatesWorkspace: true,
      parameters: {
        type: 'object',
        properties: {
          path: { type: 'string' },
          line: { type: 'integer', minimum: 1 },
          character: { type: 'integer', minimum: 1 },
          new_name: { type: 'string', minLength: 1 }
        },
        required: ['path', 'line', 'character', 'new_name'],
        additionalProperties: false
      },
      async execute({ path: input, line, character, new_name }) {
        const edit = await lsp.rename(input, line, character, new_name);
        if (!edit) return { applied: false, editCount: 0, files: [] };
        return lsp.applyWorkspaceEdit(edit);
      }
    });

    registry.register({
      name: 'lsp_code_actions',
      description: 'List language-server code actions for a source range, including edit-backed and command-backed actions.',
      permission: 'read',
      parameters: {
        type: 'object',
        properties: {
          path: { type: 'string' },
          start_line: { type: 'integer', minimum: 1 },
          start_character: { type: 'integer', minimum: 1 },
          end_line: { type: 'integer', minimum: 1 },
          end_character: { type: 'integer', minimum: 1 },
          only: { type: 'array', items: { type: 'string' }, maxItems: 20 },
          include_diagnostics: { type: 'boolean' }
        },
        required: ['path', 'start_line', 'start_character'],
        additionalProperties: false
      },
      async execute({
        path: input,
        start_line,
        start_character,
        end_line = start_line,
        end_character = start_character,
        only,
        include_diagnostics = true
      }) {
        const diagnostics = include_diagnostics ? await lsp.diagnostics(input) : [];
        return lsp.codeActions(
          input,
          start_line,
          start_character,
          end_line,
          end_character,
          { only, diagnostics }
        );
      }
    });

    registry.register({
      name: 'lsp_code_action_apply',
      description: 'Re-query code actions for a range, select one by zero-based index, resolve it if necessary, then apply its validated WorkspaceEdit and/or execute its authorized language-server command.',
      permission: 'write',
      mutatesWorkspace: true,
      parameters: {
        type: 'object',
        properties: {
          path: { type: 'string' },
          start_line: { type: 'integer', minimum: 1 },
          start_character: { type: 'integer', minimum: 1 },
          end_line: { type: 'integer', minimum: 1 },
          end_character: { type: 'integer', minimum: 1 },
          action_index: { type: 'integer', minimum: 0 },
          only: { type: 'array', items: { type: 'string' }, maxItems: 20 },
          include_diagnostics: { type: 'boolean' }
        },
        required: ['path', 'start_line', 'start_character', 'action_index'],
        additionalProperties: false
      },
      async execute({
        path: input,
        start_line,
        start_character,
        end_line = start_line,
        end_character = start_character,
        action_index,
        only,
        include_diagnostics = true
      }) {
        const diagnostics = include_diagnostics ? await lsp.diagnostics(input) : [];
        const actions = await lsp.codeActions(
          input,
          start_line,
          start_character,
          end_line,
          end_character,
          { only, diagnostics }
        );
        const action = actions?.[action_index];
        if (!action) throw new Error(`LSP code action index out of range: ${action_index}`);
        return lsp.applyCodeAction(input, action);
      }
    });
  }

  if (repository) {
    registry.register({
      name: 'lumencortex_ingest',
      description: 'Refresh repository reality/evidence nodes after meaningful workspace changes.',
      permission: 'write',
      parameters: { type: 'object', properties: {}, additionalProperties: false },
      execute() {
        const result = ingestWorkspace(repository.graph().snapshot(), root);
        repository.writeGraph(result.graph);
        runtime?.refreshSearchIndex?.(result.graph);
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

function terminateProcessTree(child, isWindows, signal = 'SIGTERM') {
  if (!child?.pid) return;
  if (isWindows) {
    try {
      const killer = spawn('taskkill', ['/pid', String(child.pid), '/T', '/F'], {
        stdio: 'ignore',
        windowsHide: true,
        detached: true
      });
      killer.unref();
    } catch {
      child.kill(signal);
    }
    return;
  }
  try {
    process.kill(-child.pid, signal);
  } catch {
    try { child.kill(signal); } catch {}
  }
}

function applyExactEdits(source, edits, relativePath) {
  let next = source;
  for (const [index, edit] of edits.entries()) {
    const oldText = edit?.old_text;
    const newText = edit?.new_text;
    if (typeof oldText !== 'string' || oldText.length === 0) {
      throw new Error(`patch edit ${index + 1} has empty old_text: ${relativePath}`);
    }
    if (typeof newText !== 'string') {
      throw new Error(`patch edit ${index + 1} has invalid new_text: ${relativePath}`);
    }
    const first = next.indexOf(oldText);
    if (first < 0) throw new Error(`patch edit ${index + 1} old_text not found: ${relativePath}`);
    if (next.indexOf(oldText, first + oldText.length) >= 0) {
      throw new Error(`patch edit ${index + 1} old_text is ambiguous: ${relativePath}`);
    }
    next = `${next.slice(0, first)}${newText}${next.slice(first + oldText.length)}`;
  }
  return next;
}

function truncate(value, maxChars = 24000) {
  const text = typeof value === 'string' ? value : JSON.stringify(value, null, 2);
  if (text.length <= maxChars) return text;

  if (typeof value === 'string') {
    const keep = Math.max(0, maxChars - 180);
    return JSON.stringify({
      truncated: true,
      omittedChars: Math.max(0, text.length - keep),
      content: text.slice(0, keep)
    });
  }

  const clipped = clipLongStrings(value, Math.max(256, Math.floor(maxChars / 2)));
  let serialized = JSON.stringify(clipped, null, 2);
  if (serialized.length <= maxChars) return serialized;

  const compact = clipLongStrings(value, Math.max(128, Math.floor(maxChars / 4)));
  serialized = JSON.stringify(compact);
  if (serialized.length <= maxChars) return serialized;

  return JSON.stringify({
    truncated: true,
    omittedChars: Math.max(0, text.length - maxChars),
    summary: 'Tool result exceeded the output budget; large string fields were omitted.'
  });
}

function clipLongStrings(value, maxStringChars) {
  if (typeof value === 'string') {
    if (value.length <= maxStringChars) return value;
    return `${value.slice(0, maxStringChars)}\n... [truncated ${value.length - maxStringChars} chars]`;
  }
  if (Array.isArray(value)) return value.map((item) => clipLongStrings(item, maxStringChars));
  if (!value || typeof value !== 'object') return value;
  return Object.fromEntries(
    Object.entries(value).map(([key, item]) => [key, clipLongStrings(item, maxStringChars)])
  );
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


function normalizeLspLocations(value, workspace) {
  if (value === null || value === undefined) return value;
  const items = Array.isArray(value) ? value : [value];
  return items.map((item) => locationToWorkspace(item, workspace));
}
