import fs from 'node:fs';
import path from 'node:path';
import { spawn } from 'node:child_process';
import { pathToFileURL, fileURLToPath } from 'node:url';
import { BRAND, resolveConfigFile } from './brand.js';

export class LspManager {
  constructor(workspace, options = {}) {
    this.workspace = path.resolve(workspace);
    this.timeoutMs = Number(options.timeoutMs ?? 15000);
    this.config = options.config ?? loadLspConfig(this.workspace);
    this.clients = new Map();
  }

  async definition(file, line, character) {
    return this.#requestFor(file, 'textDocument/definition', positionParams(path.resolve(this.workspace, file), line, character));
  }

  async references(file, line, character, includeDeclaration = true) {
    return this.#requestFor(file, 'textDocument/references', {
      ...positionParams(path.resolve(this.workspace, file), line, character),
      context: { includeDeclaration }
    });
  }

  async hover(file, line, character) {
    return this.#requestFor(file, 'textDocument/hover', positionParams(path.resolve(this.workspace, file), line, character));
  }

  async rename(file, line, character, newName, { apply = false } = {}) {
    if (!String(newName ?? '').trim()) throw new Error('newName is required');
    const edit = await this.#requestFor(
      file,
      'textDocument/rename',
      {
        ...positionParams(path.resolve(this.workspace, file), line, character),
        newName: String(newName)
      }
    );
    if (!apply) return edit;
    return applyWorkspaceEdit(this.workspace, edit);
  }

  async codeActions(file, startLine, startCharacter, endLine = startLine, endCharacter = startCharacter, context = {}) {
    const absolute = resolveInside(this.workspace, file);
    return this.#requestFor(file, 'textDocument/codeAction', {
      textDocument: { uri: pathToFileURL(absolute).href },
      range: lspRange(startLine, startCharacter, endLine, endCharacter),
      context: {
        diagnostics: Array.isArray(context.diagnostics) ? context.diagnostics : [],
        ...(Array.isArray(context.only) && context.only.length ? { only: context.only } : {})
      }
    });
  }

  async applyCodeAction(file, startLine, startCharacter, endLine, endCharacter, selector = {}) {
    const actions = await this.codeActions(
      file,
      startLine,
      startCharacter,
      endLine,
      endCharacter,
      { only: selector.kind ? [String(selector.kind)] : undefined }
    );
    const candidates = (actions ?? []).filter((action) => {
      if (!action || typeof action !== 'object') return false;
      if (selector.title && action.title !== selector.title) return false;
      if (selector.kind && action.kind !== selector.kind && !String(action.kind ?? '').startsWith(`${selector.kind}.`)) return false;
      return Boolean(action.edit);
    });
    if (!candidates.length) throw new Error('No matching code action with a workspace edit');
    if (candidates.length > 1 && !selector.title) {
      throw new Error(`Multiple matching code actions; specify title. Candidates: ${candidates.map((item) => item.title).join(', ')}`);
    }
    const action = candidates[0];
    return {
      action: { title: action.title, kind: action.kind ?? null },
      ...applyWorkspaceEdit(this.workspace, action.edit)
    };
  }

  async symbols(file) {
    const absolute = resolveInside(this.workspace, file);
    const client = await this.#clientFor(absolute);
    await client.openDocument(absolute);
    return client.request('textDocument/documentSymbol', {
      textDocument: { uri: pathToFileURL(absolute).href }
    });
  }

  async diagnostics(file) {
    const absolute = resolveInside(this.workspace, file);
    const client = await this.#clientFor(absolute);
    await client.openDocument(absolute);
    try {
      const result = await client.request('textDocument/diagnostic', {
        textDocument: { uri: pathToFileURL(absolute).href }
      });
      return result?.items ?? result ?? [];
    } catch {
      await delay(80);
      return client.diagnostics.get(pathToFileURL(absolute).href) ?? [];
    }
  }

  status() {
    return Object.entries(this.config.servers ?? {}).map(([name, value]) => ({
      name,
      command: value.command,
      args: value.args ?? [],
      extensions: value.extensions ?? [],
      running: this.clients.has(name)
    }));
  }

  async close() {
    await Promise.all([...this.clients.values()].map((client) => client.close()));
    this.clients.clear();
  }

  async #requestFor(file, method, params) {
    const absolute = resolveInside(this.workspace, file);
    const client = await this.#clientFor(absolute);
    await client.openDocument(absolute);
    return client.request(method, {
      ...params,
      ...(params?.textDocument ? { textDocument: { uri: pathToFileURL(absolute).href } } : {})
    });
  }

  async #clientFor(absolute) {
    const server = resolveServer(this.config, absolute);
    if (!server) throw new Error(`No LSP server configured for ${path.extname(absolute) || absolute}`);
    if (!this.clients.has(server.name)) {
      const client = new LspClient({
        workspace: this.workspace,
        command: server.command,
        args: server.args ?? [],
        env: server.env,
        timeoutMs: this.timeoutMs
      });
      await client.start();
      this.clients.set(server.name, client);
    }
    return this.clients.get(server.name);
  }
}

export class LspClient {
  constructor({ workspace, command, args = [], env, timeoutMs = 15000 }) {
    this.workspace = workspace;
    this.command = command;
    this.args = args;
    this.env = env;
    this.timeoutMs = timeoutMs;
    this.rpc = null;
    this.opened = new Set();
    this.diagnostics = new Map();
  }

  async start() {
    this.rpc = new ContentLengthRpcClient({
      command: this.command,
      args: this.args,
      cwd: this.workspace,
      env: this.env,
      timeoutMs: this.timeoutMs
    });
    this.rpc.onNotification = (method, params) => {
      if (method === 'textDocument/publishDiagnostics') {
        this.diagnostics.set(params.uri, params.diagnostics ?? []);
      }
    };
    await this.rpc.start();
    const rootUri = pathToFileURL(this.workspace).href;
    await this.rpc.request('initialize', {
      processId: process.pid,
      clientInfo: { name: BRAND.name, version: BRAND.version },
      rootUri,
      workspaceFolders: [{ uri: rootUri, name: path.basename(this.workspace) }],
      capabilities: {
        textDocument: {
          definition: {},
          references: {},
          hover: {},
          rename: { prepareSupport: false },
          codeAction: {
            codeActionLiteralSupport: {
              codeActionKind: {
                valueSet: ['quickfix', 'refactor', 'refactor.extract', 'refactor.inline', 'refactor.rewrite', 'source']
              }
            }
          },
          documentSymbol: {},
          publishDiagnostics: {},
          diagnostic: {}
        }
      }
    });
    this.rpc.notify('initialized', {});
  }

  request(method, params) {
    if (!this.rpc) throw new Error('LSP client is not started');
    return this.rpc.request(method, params);
  }

  async openDocument(file) {
    const uri = pathToFileURL(file).href;
    if (this.opened.has(uri)) return;
    const text = fs.readFileSync(file, 'utf8');
    this.rpc.notify('textDocument/didOpen', {
      textDocument: {
        uri,
        languageId: languageId(file),
        version: 1,
        text
      }
    });
    this.opened.add(uri);
  }

  async close() {
    if (!this.rpc) return;
    try { await this.rpc.request('shutdown', null); } catch {}
    try { this.rpc.notify('exit', null); } catch {}
    await this.rpc.close();
    this.rpc = null;
  }
}

export class ContentLengthRpcClient {
  constructor({ command, args = [], cwd, env, timeoutMs = 15000 }) {
    this.command = command;
    this.args = args;
    this.cwd = cwd;
    this.env = env;
    this.timeoutMs = timeoutMs;
    this.process = null;
    this.buffer = Buffer.alloc(0);
    this.nextId = 1;
    this.pending = new Map();
    this.onNotification = () => {};
  }

  async start() {
    this.process = spawn(this.command, this.args, {
      cwd: this.cwd,
      env: { ...process.env, ...(this.env ?? {}) },
      stdio: ['pipe', 'pipe', 'pipe']
    });
    this.process.stdout.on('data', (chunk) => this.#consume(chunk));
    this.process.stderr.on('data', () => {});
    this.process.on('error', (error) => this.#rejectAll(error));
    this.process.on('exit', (code, signal) => {
      if (this.pending.size) this.#rejectAll(new Error(`LSP exited code=${code} signal=${signal}`));
    });
    await new Promise((resolve, reject) => {
      const timer = setTimeout(resolve, 25);
      this.process.once('error', (error) => { clearTimeout(timer); reject(error); });
    });
  }

  request(method, params) {
    const id = this.nextId++;
    this.#send({ jsonrpc: '2.0', id, method, params });
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        this.pending.delete(id);
        reject(new Error(`LSP request timed out: ${method}`));
      }, this.timeoutMs);
      this.pending.set(id, { resolve, reject, timer });
    });
  }

  notify(method, params) {
    this.#send({ jsonrpc: '2.0', method, params });
  }

  async close() {
    if (!this.process) return;
    const child = this.process;
    this.process = null;
    const exited = new Promise((resolve) => {
      if (child.exitCode !== null || child.signalCode !== null) resolve();
      else child.once('exit', resolve);
    });
    try { child.stdin.end(); } catch {}
    await Promise.race([exited, delay(120)]);
    if (child.exitCode === null && child.signalCode === null) {
      try { child.kill('SIGTERM'); } catch {}
      await Promise.race([exited, delay(180)]);
    }
    if (child.exitCode === null && child.signalCode === null) {
      try { child.kill('SIGKILL'); } catch {}
      await Promise.race([exited, delay(300)]);
    }
    child.stdout?.destroy();
    child.stderr?.destroy();
    child.stdin?.destroy();
    child.removeAllListeners();
  }

  #send(message) {
    const body = Buffer.from(JSON.stringify(message));
    this.process.stdin.write(`Content-Length: ${body.length}\r\n\r\n`);
    this.process.stdin.write(body);
  }

  #consume(chunk) {
    this.buffer = Buffer.concat([this.buffer, chunk]);
    while (true) {
      const headerEnd = this.buffer.indexOf('\r\n\r\n');
      if (headerEnd < 0) return;
      const header = this.buffer.subarray(0, headerEnd).toString('utf8');
      const match = header.match(/Content-Length:\s*(\d+)/i);
      if (!match) {
        this.buffer = this.buffer.subarray(headerEnd + 4);
        continue;
      }
      const length = Number(match[1]);
      const bodyStart = headerEnd + 4;
      if (this.buffer.length < bodyStart + length) return;
      const body = this.buffer.subarray(bodyStart, bodyStart + length).toString('utf8');
      this.buffer = this.buffer.subarray(bodyStart + length);
      let message;
      try { message = JSON.parse(body); } catch { continue; }
      this.#dispatch(message);
    }
  }

  #dispatch(message) {
    if (message.id !== undefined && this.pending.has(message.id)) {
      const pending = this.pending.get(message.id);
      this.pending.delete(message.id);
      clearTimeout(pending.timer);
      if (message.error) pending.reject(Object.assign(new Error(message.error.message ?? 'LSP error'), { data: message.error.data }));
      else pending.resolve(message.result);
      return;
    }
    if (message.method) this.onNotification(message.method, message.params);
  }

  #rejectAll(error) {
    for (const pending of this.pending.values()) {
      clearTimeout(pending.timer);
      pending.reject(error);
    }
    this.pending.clear();
  }
}

export function loadLspConfig(workspace) {
  const file = resolveConfigFile(workspace, 'lsp.json');
  if (fs.existsSync(file)) return normalizeConfig(JSON.parse(fs.readFileSync(file, 'utf8')));
  return normalizeConfig({
    servers: {
      java: {
        command: process.env.LUMENCORTEX_LSP_JAVA_CMD || 'jdtls',
        args: envArgs('LUMENCORTEX_LSP_JAVA_ARGS'),
        extensions: ['.java']
      },
      typescript: {
        command: process.env.LUMENCORTEX_LSP_TS_CMD || 'typescript-language-server',
        args: envArgs('LUMENCORTEX_LSP_TS_ARGS', ['--stdio']),
        extensions: ['.ts', '.tsx', '.js', '.jsx', '.mjs', '.cjs']
      },
      python: {
        command: process.env.LUMENCORTEX_LSP_PY_CMD || 'pyright-langserver',
        args: envArgs('LUMENCORTEX_LSP_PY_ARGS', ['--stdio']),
        extensions: ['.py']
      }
    }
  });
}

function normalizeConfig(config) {
  const servers = {};
  for (const [name, value] of Object.entries(config.servers ?? {})) {
    servers[name] = { name, ...value, extensions: value.extensions ?? [] };
  }
  return { ...config, servers };
}

function resolveServer(config, file) {
  const ext = path.extname(file).toLowerCase();
  return Object.values(config.servers ?? {}).find((server) => server.extensions?.includes(ext));
}

export function applyWorkspaceEdit(workspace, edit) {
  if (!edit || typeof edit !== 'object') throw new Error('LSP workspace edit is empty');
  const root = path.resolve(workspace);
  const grouped = new Map();

  const addEdits = (uri, edits) => {
    if (!uri?.startsWith('file:')) throw new Error(`Unsupported workspace edit URI: ${uri ?? '(missing)'}`);
    const file = fileURLToPath(uri);
    const relative = path.relative(root, file);
    if (relative.startsWith('..') || path.isAbsolute(relative)) throw new Error(`Workspace edit escapes workspace: ${file}`);
    if (!Array.isArray(edits)) throw new Error('Workspace edit changes must be arrays');
    const list = grouped.get(file) ?? [];
    list.push(...edits);
    grouped.set(file, list);
  };

  for (const [uri, edits] of Object.entries(edit.changes ?? {})) addEdits(uri, edits);

  for (const change of edit.documentChanges ?? []) {
    if (change?.kind) throw new Error(`Unsupported LSP resource operation: ${change.kind}`);
    if (!change?.textDocument?.uri || !Array.isArray(change.edits)) {
      throw new Error('Unsupported LSP documentChanges entry');
    }
    addEdits(change.textDocument.uri, change.edits);
  }

  if (!grouped.size) return { files: [], editCount: 0 };

  const plans = [];
  for (const [file, edits] of grouped) {
    if (!fs.existsSync(file) || !fs.statSync(file).isFile()) throw new Error(`Workspace edit target not found: ${file}`);
    const original = fs.readFileSync(file, 'utf8');
    const mapped = edits.map((item, index) => {
      const range = item?.range;
      if (!range?.start || !range?.end || typeof item.newText !== 'string') {
        throw new Error(`Invalid TextEdit at index ${index}`);
      }
      return {
        index,
        start: lspPositionToOffset(original, range.start),
        end: lspPositionToOffset(original, range.end),
        newText: item.newText
      };
    }).sort((a, b) => b.start - a.start || b.end - a.end);

    let rightBoundary = original.length + 1;
    for (const item of mapped) {
      if (item.start > item.end) throw new Error(`Invalid TextEdit range at index ${item.index}`);
      if (item.end > rightBoundary) throw new Error('Overlapping LSP text edits are not supported');
      rightBoundary = item.start;
    }

    let next = original;
    for (const item of mapped) {
      next = `${next.slice(0, item.start)}${item.newText}${next.slice(item.end)}`;
    }
    plans.push({ file, relative: path.relative(root, file).split(path.sep).join('/'), original, next, editCount: mapped.length });
  }

  const written = [];
  try {
    for (const plan of plans) {
      fs.writeFileSync(plan.file, plan.next, 'utf8');
      written.push(plan);
    }
  } catch (error) {
    for (const plan of written.reverse()) {
      try { fs.writeFileSync(plan.file, plan.original, 'utf8'); } catch {}
    }
    throw error;
  }

  return {
    files: plans.map((plan) => ({ path: plan.relative, edits: plan.editCount })),
    editCount: plans.reduce((sum, plan) => sum + plan.editCount, 0)
  };
}

function lspRange(startLine, startCharacter, endLine, endCharacter) {
  return {
    start: {
      line: Math.max(0, Number(startLine) - 1),
      character: Math.max(0, Number(startCharacter) - 1)
    },
    end: {
      line: Math.max(0, Number(endLine) - 1),
      character: Math.max(0, Number(endCharacter) - 1)
    }
  };
}

function lspPositionToOffset(text, position) {
  const line = Math.max(0, Number(position?.line ?? 0));
  const character = Math.max(0, Number(position?.character ?? 0));
  const lines = text.split(/\n/);
  if (line >= lines.length) throw new Error(`LSP position line out of range: ${line}`);
  let offset = 0;
  for (let index = 0; index < line; index += 1) offset += lines[index].length + 1;
  const current = lines[line].endsWith('\r') ? lines[line].slice(0, -1) : lines[line];
  if (character > current.length) throw new Error(`LSP position character out of range: ${character}`);
  return offset + character;
}

function positionParams(file, line, character) {
  const absolute = path.resolve(file);
  return {
    textDocument: { uri: pathToFileURL(absolute).href },
    position: { line: Math.max(0, Number(line) - 1), character: Math.max(0, Number(character) - 1) }
  };
}

function languageId(file) {
  const ext = path.extname(file).toLowerCase();
  return ({
    '.java': 'java', '.kt': 'kotlin', '.kts': 'kotlin',
    '.ts': 'typescript', '.tsx': 'typescriptreact',
    '.js': 'javascript', '.jsx': 'javascriptreact',
    '.mjs': 'javascript', '.cjs': 'javascript',
    '.py': 'python', '.go': 'go', '.rs': 'rust'
  })[ext] ?? (ext.slice(1) || 'plaintext');
}

function resolveInside(root, input) {
  const absolute = path.resolve(root, input);
  const relative = path.relative(root, absolute);
  if (relative.startsWith('..') || path.isAbsolute(relative)) throw new Error(`Path escapes workspace: ${input}`);
  return absolute;
}

function envArgs(name, fallback = []) {
  const value = process.env[name];
  return value ? value.split(/\s+/).filter(Boolean) : fallback;
}

function delay(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

export function locationToWorkspace(location, workspace) {
  const uri = location?.uri ?? location?.targetUri;
  if (!uri?.startsWith('file:')) return location;
  const file = fileURLToPath(uri);
  return {
    path: path.relative(workspace, file).split(path.sep).join('/'),
    range: location.range ?? location.targetSelectionRange ?? location.targetRange
  };
}
