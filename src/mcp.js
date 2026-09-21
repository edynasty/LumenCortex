import fs from 'node:fs';
import path from 'node:path';
import { spawn } from 'node:child_process';
import { BRAND, resolveConfigFile } from './brand.js';

const MODERN_VERSION = '2026-07-28';
const LEGACY_VERSION = '2025-11-25';

export class McpManager {
  constructor(workspace, options = {}) {
    this.workspace = path.resolve(workspace);
    this.config = options.config ?? loadMcpConfig(this.workspace);
    this.clients = new Map();
  }

  configuredServers() {
    return Object.entries(this.config.servers ?? {}).map(([name, config]) => ({
      name,
      transport: config.transport ?? (config.url ? 'http' : 'stdio'),
      command: config.command,
      url: config.url
    }));
  }

  async connect(name) {
    if (this.clients.has(name)) return this.clients.get(name);
    const config = this.config.servers?.[name];
    if (!config) throw new Error(`Unknown MCP server: ${name}`);
    const client = config.url || config.transport === 'http'
      ? new HttpMcpClient(name, config)
      : new StdioMcpClient(name, { ...config, cwd: config.cwd ? path.resolve(this.workspace, config.cwd) : this.workspace });
    await client.connect();
    this.clients.set(name, client);
    return client;
  }

  async listTools(name) {
    const client = await this.connect(name);
    return client.listTools();
  }

  async callTool(server, tool, args) {
    const client = await this.connect(server);
    return client.callTool(tool, args);
  }

  async registerTools(registry, options = {}) {
    const prefix = options.prefix ?? 'mcp';
    const added = [];
    for (const { name } of this.configuredServers()) {
      const client = await this.connect(name);
      const tools = await client.listTools();
      for (const tool of tools) {
        const localName = sanitizeToolName(`${prefix}_${name}_${tool.name}`);
        registry.register({
          name: localName,
          description: `MCP ${name}/${tool.name}: ${tool.description ?? ''}`,
          permission: tool.annotations?.readOnlyHint === true ? 'read' : 'write',
          scope: 'external',
          mutatesWorkspace: tool.annotations?.readOnlyHint !== true,
          parameters: tool.inputSchema ?? { type: 'object', properties: {} },
          execute: async (args) => normalizeToolResult(await client.callTool(tool.name, args))
        });
        added.push({ server: name, remoteName: tool.name, localName });
      }
    }
    return added;
  }

  async close() {
    await Promise.all([...this.clients.values()].map((client) => client.close()));
    this.clients.clear();
  }
}

class McpClientBase {
  constructor(name) {
    this.name = name;
    this.nextId = 1;
    this.era = null;
    this.protocolVersion = null;
  }

  async connect() {
    try {
      const discovery = await this.rawRequest('server/discover', this.#params({}, { modernProbe: true }), { modernProbe: true });
      if (!discovery?.error) {
        this.era = 'modern';
        this.protocolVersion = MODERN_VERSION;
        return discovery?.result ?? discovery;
      }
    } catch {}

    const initialized = await this.rawRequest('initialize', {
      protocolVersion: LEGACY_VERSION,
      capabilities: {},
      clientInfo: { name: BRAND.name, version: BRAND.version }
    }, { legacy: true });
    if (initialized?.error) throw new Error(initialized.error.message ?? 'MCP initialize failed');
    const result = initialized?.result ?? initialized;
    this.era = 'legacy';
    this.protocolVersion = result?.protocolVersion ?? LEGACY_VERSION;
    await this.notify('notifications/initialized', {}, { legacy: true });
    return result;
  }

  async request(method, params = {}) {
    const response = await this.rawRequest(method, this.#params(params));
    if (response?.error) {
      const error = new Error(response.error.message ?? `MCP error calling ${method}`);
      error.code = response.error.code;
      error.data = response.error.data;
      throw error;
    }
    return response?.result ?? response;
  }

  async listTools() {
    const result = await this.request('tools/list', {});
    return result?.tools ?? [];
  }

  callTool(name, args = {}) {
    return this.request('tools/call', { name, arguments: args });
  }

  async notify(method, params = {}, options = {}) {
    return this.rawNotify(method, this.#params(params, options));
  }

  envelope(method, params, id) {
    return { jsonrpc: '2.0', id, method, params };
  }

  #params(params, options = {}) {
    if ((this.era === 'modern' && !options.legacy) || options.modernProbe) {
      return {
        ...(params ?? {}),
        _meta: {
          ...(params?._meta ?? {}),
          'io.modelcontextprotocol/clientInfo': { name: BRAND.name, version: BRAND.version },
          'io.modelcontextprotocol/clientCapabilities': {}
        }
      };
    }
    return params ?? {};
  }
}

export class StdioMcpClient extends McpClientBase {
  constructor(name, config) {
    super(name);
    this.config = config;
    this.process = null;
    this.pending = new Map();
    this.buffer = '';
  }

  async connect() {
    this.process = spawn(this.config.command, this.config.args ?? [], {
      cwd: this.config.cwd,
      env: { ...process.env, ...(this.config.env ?? {}) },
      stdio: ['pipe', 'pipe', 'pipe']
    });
    this.process.stdout.setEncoding('utf8');
    this.process.stdout.on('data', (chunk) => this.#consume(chunk));
    this.process.stderr.on('data', () => {});
    this.process.on('error', (error) => this.#rejectAll(error));
    this.process.on('exit', (code) => {
      if (this.pending.size) this.#rejectAll(new Error(`MCP stdio server ${this.name} exited with ${code}`));
    });
    await delay(20);
    return super.connect();
  }

  rawRequest(method, params) {
    const id = this.nextId++;
    const message = this.envelope(method, params, id);
    this.process.stdin.write(JSON.stringify(message) + '\n');
    return new Promise((resolve, reject) => {
      const timeoutMs = Number(this.config.timeoutMs ?? 20000);
      const timer = setTimeout(() => {
        this.pending.delete(id);
        reject(new Error(`MCP request timed out: ${this.name}/${method}`));
      }, timeoutMs);
      this.pending.set(id, { resolve, reject, timer });
    });
  }

  rawNotify(method, params) {
    this.process.stdin.write(JSON.stringify({ jsonrpc: '2.0', method, params }) + '\n');
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

  #consume(chunk) {
    this.buffer += chunk;
    while (true) {
      const index = this.buffer.indexOf('\n');
      if (index < 0) return;
      const line = this.buffer.slice(0, index).trim();
      this.buffer = this.buffer.slice(index + 1);
      if (!line) continue;
      let message;
      try { message = JSON.parse(line); } catch { continue; }
      if (message.id !== undefined && this.pending.has(message.id)) {
        const pending = this.pending.get(message.id);
        this.pending.delete(message.id);
        clearTimeout(pending.timer);
        pending.resolve(message);
      }
    }
  }

  #rejectAll(error) {
    for (const pending of this.pending.values()) {
      clearTimeout(pending.timer);
      pending.reject(error);
    }
    this.pending.clear();
  }
}

export class HttpMcpClient extends McpClientBase {
  constructor(name, config) {
    super(name);
    this.config = config;
    this.fetchImpl = config.fetchImpl ?? fetch;
  }

  async rawRequest(method, params, options = {}) {
    const id = this.nextId++;
    const modern = options.modernProbe || (this.era === 'modern' && !options.legacy);
    const headers = {
      'content-type': 'application/json',
      accept: 'application/json, text/event-stream',
      ...(this.config.headers ?? {})
    };
    if (modern) {
      headers['MCP-Protocol-Version'] = MODERN_VERSION;
      headers['Mcp-Method'] = method;
      if (method === 'tools/call' && params?.name) headers['Mcp-Name'] = params.name;
    }
    const response = await this.fetchImpl(this.config.url, {
      method: 'POST',
      headers,
      body: JSON.stringify(this.envelope(method, params, id))
    });
    const text = await response.text();
    const parsed = parseHttpMcpBody(text, response.headers.get('content-type'));
    if (!response.ok && !parsed?.error) {
      return { jsonrpc: '2.0', id, error: { code: response.status, message: text || response.statusText } };
    }
    return parsed;
  }

  async rawNotify(method, params) {
    await this.fetchImpl(this.config.url, {
      method: 'POST',
      headers: { 'content-type': 'application/json', ...(this.config.headers ?? {}) },
      body: JSON.stringify({ jsonrpc: '2.0', method, params })
    });
  }

  async close() {}
}

export function loadMcpConfig(workspace) {
  const file = resolveConfigFile(workspace, 'mcp.json');
  if (!fs.existsSync(file)) return { servers: {} };
  const parsed = JSON.parse(fs.readFileSync(file, 'utf8'));
  return { servers: parsed.servers ?? parsed.mcpServers ?? {} };
}

export function normalizeToolResult(result) {
  if (result === null || result === undefined) return '';
  if (typeof result === 'string') return result;
  if (Array.isArray(result?.content)) {
    return result.content.map((item) => {
      if (item.type === 'text') return item.text ?? '';
      if (item.type === 'resource') return JSON.stringify(item.resource ?? item);
      return JSON.stringify(item);
    }).join('\n');
  }
  if (result.structuredContent !== undefined) return result.structuredContent;
  return result;
}

function parseHttpMcpBody(text, contentType = '') {
  if (contentType?.includes('text/event-stream')) {
    const data = text.split(/\r?\n/)
      .filter((line) => line.startsWith('data:'))
      .map((line) => line.slice(5).trim())
      .filter((line) => line && line !== '[DONE]');
    for (const item of data.reverse()) {
      try { return JSON.parse(item); } catch {}
    }
    return null;
  }
  return text ? JSON.parse(text) : null;
}

function sanitizeToolName(value) {
  return value.replace(/[^A-Za-z0-9_-]+/g, '_').slice(0, 64);
}

function delay(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
