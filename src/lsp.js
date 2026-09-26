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

  async rename(file, line, character, newName) {
    if (!String(newName ?? '').trim()) throw new Error('LSP rename requires a non-empty new name');
    return this.#requestFor(
      file,
      'textDocument/rename',
      {
        ...positionParams(path.resolve(this.workspace, file), line, character),
        newName: String(newName)
      }
    );
  }

  async codeActions(file, startLine, startCharacter, endLine = startLine, endCharacter = startCharacter, options = {}) {
    const absolute = resolveInside(this.workspace, file);
    const client = await this.#clientFor(absolute);
    await client.openDocument(absolute);
    const diagnostics = options.diagnostics ?? [];
    return await client.request('textDocument/codeAction', {
      textDocument: { uri: pathToFileURL(absolute).href },
      range: {
        start: lspPosition(startLine, startCharacter),
        end: lspPosition(endLine, endCharacter)
      },
      context: {
        diagnostics,
        ...(options.only?.length ? { only: options.only } : {})
      }
    }) ?? [];
  }

  async resolveCodeAction(file, action) {
    if (!action || typeof action !== 'object') throw new Error('LSP code action is required');
    if (action.edit || typeof action.command === 'string') return action;
    const absolute = resolveInside(this.workspace, file);
    const client = await this.#clientFor(absolute);
    await client.openDocument(absolute);
    return client.request('codeAction/resolve', action);
  }

  async applyWorkspaceEdit(edit) {
    const openRenames = [];
    const openDeletes = [];
    const planned = planLspWorkspaceEdit(this.workspace, edit);

    for (const operation of planned.resourceOperations) {
      if (operation.ignored) continue;
      if (operation.kind === 'rename') {
        const from = resolveInside(this.workspace, operation.oldPath);
        const server = resolveServer(this.config, from);
        const client = server ? this.clients.get(server.name) : null;
        if (client?.isDocumentOpen(from)) {
          openRenames.push({ client, from, to: resolveInside(this.workspace, operation.newPath) });
        }
      } else if (operation.kind === 'delete') {
        const file = resolveInside(this.workspace, operation.path);
        const server = resolveServer(this.config, file);
        const client = server ? this.clients.get(server.name) : null;
        if (client?.isDocumentOpen(file)) openDeletes.push({ client, file });
      }
    }

    const result = applyLspWorkspaceEdit(this.workspace, edit, { planned });

    for (const item of openDeletes) await item.client.closeDocument(item.file);
    for (const item of openRenames) {
      await item.client.closeDocument(item.from);
      if (fs.existsSync(item.to)) {
        const targetClient = await this.#clientFor(item.to);
        await targetClient.openDocument(item.to);
      }
    }

    const renamedSources = new Set(openRenames.map((item) => path.resolve(item.from)));
    for (const changed of result.files) {
      const absolute = resolveInside(this.workspace, changed.path);
      if (!changed.exists || renamedSources.has(path.resolve(absolute))) continue;
      const server = resolveServer(this.config, absolute);
      const client = server ? this.clients.get(server.name) : null;
      if (client) await client.updateDocument(absolute);
    }
    return result;
  }

  async applyCodeAction(file, action) {
    const resolved = await this.resolveCodeAction(file, action);
    const command = codeActionCommand(resolved);
    let editResult = null;
    if (resolved?.edit) editResult = await this.applyWorkspaceEdit(resolved.edit);

    let commandResult = null;
    if (command) {
      const absolute = resolveInside(this.workspace, file);
      const client = await this.#clientFor(absolute);
      if (fs.existsSync(absolute)) await client.openDocument(absolute);
      commandResult = await client.executeCommand(command);
    }

    if (!editResult && !command) {
      throw new Error('LSP code action did not provide a WorkspaceEdit or executable command');
    }

    return {
      action: {
        title: resolved?.title ?? action.title ?? command?.title ?? '',
        kind: resolved?.kind ?? action.kind ?? null
      },
      ...(editResult ?? {
        applied: false,
        editCount: 0,
        resourceOperationCount: 0,
        resourceOperations: [],
        files: []
      }),
      command: command ? {
        command: command.command,
        title: command.title ?? resolved?.title ?? action.title ?? '',
        executed: true,
        result: commandResult
      } : null
    };
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
        timeoutMs: this.timeoutMs,
        applyWorkspaceEdit: async (params) => {
          try {
            await this.applyWorkspaceEdit(params?.edit);
            return { applied: true };
          } catch (error) {
            return {
              applied: false,
              failureReason: error.message
            };
          }
        }
      });
      await client.start();
      this.clients.set(server.name, client);
    }
    return this.clients.get(server.name);
  }
}

export class LspClient {
  constructor({ workspace, command, args = [], env, timeoutMs = 15000, applyWorkspaceEdit }) {
    this.workspace = workspace;
    this.command = command;
    this.args = args;
    this.env = env;
    this.timeoutMs = timeoutMs;
    this.applyWorkspaceEdit = applyWorkspaceEdit ?? null;
    this.serverApplyEditDepth = 0;
    this.rpc = null;
    this.opened = new Set();
    this.versions = new Map();
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
    this.rpc.onRequest = async (method, params) => {
      if (method === 'workspace/applyEdit') {
        if (this.serverApplyEditDepth <= 0 || !this.applyWorkspaceEdit) {
          return {
            applied: false,
            failureReason: 'No explicitly authorized LSP command is active'
          };
        }
        return this.applyWorkspaceEdit(params);
      }
      if (method === 'workspace/configuration') {
        return Array.isArray(params?.items) ? params.items.map(() => null) : [];
      }
      if (method === 'workspace/workspaceFolders') {
        const uri = pathToFileURL(this.workspace).href;
        return [{ uri, name: path.basename(this.workspace) }];
      }
      if (method === 'client/registerCapability' || method === 'client/unregisterCapability') {
        return null;
      }
      const error = new Error(`Unsupported LSP server request: ${method}`);
      error.rpcCode = -32601;
      throw error;
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
          documentSymbol: {},
          publishDiagnostics: {},
          diagnostic: {},
          rename: { prepareSupport: false },
          codeAction: {
            resolveSupport: { properties: ['edit'] }
          }
        },
        workspace: {
          applyEdit: true,
          workspaceEdit: {
            documentChanges: true,
            resourceOperations: ['create', 'rename', 'delete']
          }
        }
      }
    });
    this.rpc.notify('initialized', {});
  }

  request(method, params) {
    if (!this.rpc) throw new Error('LSP client is not started');
    return this.rpc.request(method, params);
  }

  async executeCommand(command) {
    if (!command || typeof command.command !== 'string' || !command.command.trim()) {
      throw new Error('LSP executeCommand requires a command name');
    }
    this.serverApplyEditDepth += 1;
    try {
      return await this.request('workspace/executeCommand', {
        command: command.command,
        arguments: Array.isArray(command.arguments) ? command.arguments : []
      });
    } finally {
      this.serverApplyEditDepth = Math.max(0, this.serverApplyEditDepth - 1);
    }
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
    this.versions.set(uri, 1);
  }

  isDocumentOpen(file) {
    return this.opened.has(pathToFileURL(file).href);
  }

  async updateDocument(file) {
    const uri = pathToFileURL(file).href;
    if (!this.opened.has(uri)) return;
    const version = Number(this.versions.get(uri) ?? 1) + 1;
    const text = fs.readFileSync(file, 'utf8');
    this.rpc.notify('textDocument/didChange', {
      textDocument: { uri, version },
      contentChanges: [{ text }]
    });
    this.versions.set(uri, version);
  }

  async closeDocument(file) {
    const uri = pathToFileURL(file).href;
    if (!this.opened.has(uri)) return false;
    this.rpc.notify('textDocument/didClose', {
      textDocument: { uri }
    });
    this.opened.delete(uri);
    this.versions.delete(uri);
    this.diagnostics.delete(uri);
    return true;
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
    this.onRequest = async (method) => {
      const error = new Error(`Unsupported JSON-RPC request: ${method}`);
      error.rpcCode = -32601;
      throw error;
    };
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
    if (message.id !== undefined && message.method === undefined && this.pending.has(message.id)) {
      const pending = this.pending.get(message.id);
      this.pending.delete(message.id);
      clearTimeout(pending.timer);
      if (message.error) pending.reject(Object.assign(new Error(message.error.message ?? 'LSP error'), { data: message.error.data }));
      else pending.resolve(message.result);
      return;
    }

    if (message.id !== undefined && message.method) {
      Promise.resolve()
        .then(() => this.onRequest(message.method, message.params))
        .then((result) => {
          this.#send({
            jsonrpc: '2.0',
            id: message.id,
            result: result ?? null
          });
        })
        .catch((error) => {
          this.#send({
            jsonrpc: '2.0',
            id: message.id,
            error: {
              code: Number.isInteger(error?.rpcCode) ? error.rpcCode : -32603,
              message: error?.message ?? 'LSP client request handler failed'
            }
          });
        });
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

function codeActionCommand(action) {
  if (!action || typeof action !== 'object') return null;
  if (typeof action.command === 'string' && action.command.trim()) {
    return {
      title: action.title ?? '',
      command: action.command,
      arguments: Array.isArray(action.arguments) ? action.arguments : []
    };
  }
  if (
    action.command &&
    typeof action.command === 'object' &&
    typeof action.command.command === 'string' &&
    action.command.command.trim()
  ) {
    return {
      title: action.command.title ?? action.title ?? '',
      command: action.command.command,
      arguments: Array.isArray(action.command.arguments) ? action.command.arguments : []
    };
  }
  return null;
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

export function applyLspWorkspaceEdit(workspace, edit, options = {}) {
  const root = path.resolve(workspace);
  const planned = options.planned ?? planLspWorkspaceEdit(root, edit);
  const changed = planned.files.filter((item) => workspaceFileChanged(item.initial, item.final));
  const createdDirectories = new Set();

  try {
    for (const item of changed.filter((entry) => entry.final.exists)) {
      ensureWorkspaceParentDirectories(root, item.file, createdDirectories);
      writeWorkspaceFileAtomic(item.file, item.final.content, item.final.mode);
    }
    for (const item of changed.filter((entry) => !entry.final.exists && entry.initial.exists)) {
      fs.unlinkSync(item.file);
    }
  } catch (error) {
    const rollbackErrors = rollbackWorkspaceFiles(root, changed, createdDirectories);
    if (rollbackErrors.length) error.rollbackErrors = rollbackErrors;
    throw error;
  }

  return {
    applied: true,
    editCount: planned.editCount,
    resourceOperationCount: planned.resourceOperations.length,
    resourceOperations: planned.resourceOperations.map((item) => ({ ...item })),
    files: changed.map((item) => ({
      path: item.path,
      edits: item.edits,
      bytes: item.final.exists ? Buffer.byteLength(item.final.content) : 0,
      exists: item.final.exists,
      action: workspaceFileAction(item)
    }))
  };
}

export function planLspWorkspaceEdit(workspace, edit) {
  if (!edit || typeof edit !== 'object') throw new Error('LSP WorkspaceEdit is required');
  const root = path.resolve(workspace);
  const initial = new Map();
  const current = new Map();
  const editCounts = new Map();
  const resourceOperations = [];
  let editCount = 0;

  const load = (uri, label = 'WorkspaceEdit') => {
    const file = workspaceFileFromUri(root, uri, label);
    if (!current.has(file)) {
      const snapshot = readWorkspaceFileSnapshot(root, file);
      initial.set(file, cloneWorkspaceFileState(snapshot));
      current.set(file, cloneWorkspaceFileState(snapshot));
    }
    return { file, state: current.get(file) };
  };

  const applyText = (uri, edits, label = 'text edit') => {
    const { file, state } = load(uri, label);
    if (!state.exists) {
      throw new Error(`LSP WorkspaceEdit target not found: ${normalizeWorkspacePath(root, file)}`);
    }
    if (!Array.isArray(edits)) throw new Error(`Invalid LSP text edits for ${normalizeWorkspacePath(root, file)}`);
    state.content = applyLspTextEdits(
      state.content,
      edits,
      normalizeWorkspacePath(root, file)
    );
    editCounts.set(file, Number(editCounts.get(file) ?? 0) + edits.length);
    editCount += edits.length;
  };

  for (const [uri, edits] of Object.entries(edit.changes ?? {})) {
    applyText(uri, edits, 'WorkspaceEdit changes');
  }

  for (const change of edit.documentChanges ?? []) {
    if (change?.textDocument?.uri && Array.isArray(change.edits)) {
      applyText(change.textDocument.uri, change.edits, 'TextDocumentEdit');
      continue;
    }

    const kind = String(change?.kind ?? '');
    if (kind === 'create') {
      const { file, state } = load(change.uri, 'CreateFile');
      const filePath = normalizeWorkspacePath(root, file);
      const overwrite = Boolean(change.options?.overwrite);
      const ignoreIfExists = Boolean(change.options?.ignoreIfExists);
      let ignored = false;
      if (state.exists) {
        if (overwrite) {
          state.content = '';
        } else if (ignoreIfExists) {
          ignored = true;
        } else {
          throw new Error(`LSP CreateFile target already exists: ${filePath}`);
        }
      } else {
        state.exists = true;
        state.content = '';
        state.mode = 0o666;
      }
      resourceOperations.push({
        kind: 'create',
        path: filePath,
        overwrite,
        ignoreIfExists,
        ignored
      });
      continue;
    }

    if (kind === 'rename') {
      const source = load(change.oldUri, 'RenameFile oldUri');
      const target = load(change.newUri, 'RenameFile newUri');
      const oldPath = normalizeWorkspacePath(root, source.file);
      const newPath = normalizeWorkspacePath(root, target.file);
      const overwrite = Boolean(change.options?.overwrite);
      const ignoreIfExists = Boolean(change.options?.ignoreIfExists);
      let ignored = false;

      if (source.file === target.file) {
        ignored = true;
      } else if (!source.state.exists) {
        throw new Error(`LSP RenameFile source not found: ${oldPath}`);
      } else if (target.state.exists && !overwrite) {
        if (ignoreIfExists) ignored = true;
        else throw new Error(`LSP RenameFile target already exists: ${newPath}`);
      }

      if (!ignored) {
        target.state.exists = true;
        target.state.content = source.state.content;
        target.state.mode = source.state.mode;
        source.state.exists = false;
        source.state.content = '';
      }
      resourceOperations.push({
        kind: 'rename',
        oldPath,
        newPath,
        overwrite,
        ignoreIfExists,
        ignored
      });
      continue;
    }

    if (kind === 'delete') {
      const { file, state } = load(change.uri, 'DeleteFile');
      const filePath = normalizeWorkspacePath(root, file);
      const ignoreIfNotExists = Boolean(change.options?.ignoreIfNotExists);
      const recursive = Boolean(change.options?.recursive);
      let ignored = false;
      if (!state.exists) {
        if (ignoreIfNotExists) ignored = true;
        else throw new Error(`LSP DeleteFile target not found: ${filePath}`);
      } else {
        state.exists = false;
        state.content = '';
      }
      resourceOperations.push({
        kind: 'delete',
        path: filePath,
        recursive,
        ignoreIfNotExists,
        ignored
      });
      continue;
    }

    throw new Error(`Unsupported LSP WorkspaceEdit resource operation: ${kind || 'resource-operation'}`);
  }

  const files = [...new Set([...initial.keys(), ...current.keys()])]
    .sort((left, right) => left.localeCompare(right))
    .map((file) => ({
      file,
      path: normalizeWorkspacePath(root, file),
      initial: cloneWorkspaceFileState(initial.get(file) ?? missingWorkspaceFileState()),
      final: cloneWorkspaceFileState(current.get(file) ?? missingWorkspaceFileState()),
      edits: Number(editCounts.get(file) ?? 0)
    }));

  validateWorkspacePlanParents(root, files);

  return {
    editCount,
    resourceOperations,
    files
  };
}

export function normalizeWorkspaceEdit(workspace, edit) {
  const planned = planLspWorkspaceEdit(workspace, edit);
  if (planned.resourceOperations.length) {
    throw new Error('normalizeWorkspaceEdit only supports text-only WorkspaceEdit values');
  }
  const root = path.resolve(workspace);
  const grouped = new Map();
  for (const [uri, edits] of Object.entries(edit.changes ?? {})) {
    grouped.set(workspaceFileFromUri(root, uri, 'WorkspaceEdit changes'), [...edits]);
  }
  for (const change of edit.documentChanges ?? []) {
    if (change?.textDocument?.uri && Array.isArray(change.edits)) {
      const file = workspaceFileFromUri(root, change.textDocument.uri, 'TextDocumentEdit');
      const list = grouped.get(file) ?? [];
      list.push(...change.edits);
      grouped.set(file, list);
    }
  }
  return grouped;
}

function applyLspTextEdits(text, edits, displayPath) {
  const ranged = edits.map((entry, index) => {
    if (!entry?.range || typeof entry.newText !== 'string') {
      throw new Error(`Unsupported LSP text edit at ${displayPath}#${index + 1}`);
    }
    return {
      index,
      start: lspOffset(text, entry.range.start),
      end: lspOffset(text, entry.range.end),
      newText: entry.newText
    };
  }).sort((a, b) => a.start - b.start || a.end - b.end || a.index - b.index);

  for (let index = 1; index < ranged.length; index += 1) {
    const previous = ranged[index - 1];
    const current = ranged[index];
    if (current.start < previous.end) {
      throw new Error(`Overlapping LSP edits are not supported: ${displayPath}`);
    }
  }

  let next = text;
  for (const item of [...ranged].sort((a, b) => b.start - a.start || b.end - a.end || b.index - a.index)) {
    next = `${next.slice(0, item.start)}${item.newText}${next.slice(item.end)}`;
  }
  return next;
}

function workspaceFileFromUri(root, uri, label) {
  if (!String(uri ?? '').startsWith('file:')) {
    throw new Error(`Unsupported LSP ${label} URI: ${uri ?? '(missing)'}`);
  }
  return resolveInside(root, fileURLToPath(uri));
}

function readWorkspaceFileSnapshot(root, file) {
  assertWorkspaceResourcePath(root, file);
  let stat;
  try {
    stat = fs.lstatSync(file);
  } catch (error) {
    if (error?.code === 'ENOENT') return missingWorkspaceFileState();
    throw error;
  }
  if (stat.isSymbolicLink()) {
    throw new Error(`LSP WorkspaceEdit refuses symbolic link target: ${normalizeWorkspacePath(root, file)}`);
  }
  if (!stat.isFile()) {
    throw new Error(`LSP WorkspaceEdit resource is not a regular file: ${normalizeWorkspacePath(root, file)}`);
  }
  return {
    exists: true,
    content: fs.readFileSync(file, 'utf8'),
    mode: stat.mode & 0o777
  };
}

function assertWorkspaceResourcePath(root, file) {
  resolveInside(root, file);
  const rootPath = path.resolve(root);
  if (path.resolve(file) === rootPath) {
    throw new Error('LSP WorkspaceEdit resource cannot be the workspace root');
  }
  let cursor = path.dirname(file);
  while (cursor !== rootPath) {
    let stat;
    try {
      stat = fs.lstatSync(cursor);
    } catch (error) {
      if (error?.code === 'ENOENT') {
        cursor = path.dirname(cursor);
        continue;
      }
      throw error;
    }
    if (stat.isSymbolicLink()) {
      throw new Error(`LSP WorkspaceEdit refuses symbolic link parent: ${normalizeWorkspacePath(rootPath, cursor)}`);
    }
    if (!stat.isDirectory()) {
      throw new Error(`LSP WorkspaceEdit parent is not a directory: ${normalizeWorkspacePath(rootPath, cursor)}`);
    }
    cursor = path.dirname(cursor);
  }
}

function validateWorkspacePlanParents(root, files) {
  const finalByPath = new Map(files.map((item) => [path.resolve(item.file), item.final]));
  for (const item of files) {
    if (!item.final.exists) continue;
    assertWorkspaceResourcePath(root, item.file);
    let cursor = path.dirname(item.file);
    const rootPath = path.resolve(root);
    while (cursor !== rootPath) {
      const virtual = finalByPath.get(path.resolve(cursor));
      if (virtual?.exists) {
        throw new Error(
          `LSP WorkspaceEdit parent would be a file: ${normalizeWorkspacePath(root, cursor)}`
        );
      }
      cursor = path.dirname(cursor);
    }
  }
}

function missingWorkspaceFileState() {
  return { exists: false, content: '', mode: 0o666 };
}

function cloneWorkspaceFileState(state) {
  return {
    exists: Boolean(state?.exists),
    content: String(state?.content ?? ''),
    mode: Number(state?.mode ?? 0o666)
  };
}

function workspaceFileChanged(initial, final) {
  return initial.exists !== final.exists ||
    (initial.exists && final.exists && (
      initial.content !== final.content ||
      initial.mode !== final.mode
    ));
}

function workspaceFileAction(item) {
  if (!item.initial.exists && item.final.exists) return 'create';
  if (item.initial.exists && !item.final.exists) return 'delete';
  return 'modify';
}

function ensureWorkspaceParentDirectories(root, file, createdDirectories) {
  const rootPath = path.resolve(root);
  const pending = [];
  let cursor = path.dirname(file);
  while (cursor !== rootPath) {
    if (fs.existsSync(cursor)) break;
    pending.push(cursor);
    cursor = path.dirname(cursor);
  }
  for (const dir of pending.reverse()) {
    fs.mkdirSync(dir);
    createdDirectories.add(dir);
  }
}

function writeWorkspaceFileAtomic(file, content, mode = 0o666) {
  const tmp = path.join(
    path.dirname(file),
    `.${path.basename(file)}.lcx-lsp-${process.pid}-${Date.now()}-${Math.random().toString(16).slice(2)}`
  );
  fs.writeFileSync(tmp, content, { encoding: 'utf8', mode });
  try {
    fs.renameSync(tmp, file);
  } catch (error) {
    try { fs.rmSync(tmp, { force: true }); } catch {}
    throw error;
  }
  try { fs.chmodSync(file, mode); } catch {}
}

function rollbackWorkspaceFiles(root, changed, createdDirectories) {
  const errors = [];
  for (const item of [...changed].reverse()) {
    try {
      if (item.initial.exists) {
        ensureWorkspaceParentDirectories(root, item.file, new Set());
        writeWorkspaceFileAtomic(item.file, item.initial.content, item.initial.mode);
      } else if (fs.existsSync(item.file)) {
        const stat = fs.lstatSync(item.file);
        if (stat.isFile() && !stat.isSymbolicLink()) fs.unlinkSync(item.file);
      }
    } catch (error) {
      errors.push({
        path: item.path,
        error: error.message
      });
    }
  }
  for (const dir of [...createdDirectories].sort((a, b) => b.length - a.length)) {
    try { fs.rmdirSync(dir); } catch {}
  }
  return errors;
}

function lspOffset(text, position) {
  const line = Math.max(0, Number(position?.line ?? 0));
  const character = Math.max(0, Number(position?.character ?? 0));
  let lineStart = 0;
  let currentLine = 0;

  while (currentLine < line) {
    const newline = text.indexOf('\n', lineStart);
    if (newline < 0) throw new Error(`LSP position line out of range: ${line}`);
    lineStart = newline + 1;
    currentLine += 1;
  }

  let lineEnd = text.indexOf('\n', lineStart);
  if (lineEnd < 0) lineEnd = text.length;
  let contentEnd = lineEnd;
  if (contentEnd > lineStart && text[contentEnd - 1] === '\r') contentEnd -= 1;
  const length = contentEnd - lineStart;
  if (character > length) {
    throw new Error(`LSP position character out of range: ${character} > ${length}`);
  }
  return lineStart + character;
}

function lspPosition(line, character) {
  return {
    line: Math.max(0, Number(line) - 1),
    character: Math.max(0, Number(character) - 1)
  };
}

function normalizeWorkspacePath(root, file) {
  return path.relative(root, file).split(path.sep).join('/');
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
