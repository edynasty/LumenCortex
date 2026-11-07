#!/usr/bin/env node
'use strict';
/**
 * LumenCortex persistent JavaScript worker.
 *
 * JSON-RPC 2.0 over fd 3 (host -> worker) and fd 4 (worker -> host), one JSON
 * object per line. User output is captured through patched process.stdout /
 * process.stderr writers, so it never corrupts the control channel.
 *
 * Persistence is a vm context per session: top-level declarations are captured
 * after each cell and re-exported as globals for the next one. `var`, functions,
 * classes, `let`/`const` and destructuring all survive a successful cell.
 * Synchronous cells are bounded by a vm timeout; a worker that must be killed
 * loses its kernel and the host starts a new generation (interrupt is declared
 * unsupported for that reason).
 */

const fs = require('node:fs');
const vm = require('node:vm');
const path = require('node:path');
const util = require('node:util');
const base64 = (buf) => Buffer.from(buf).toString('base64');

const IN_FD = 3;
const OUT_FD = 4;
const MAX_FRAME = 4 * 1024 * 1024;
const MAX_REPR = 8 * 1024;
const MAX_TRACEBACK = 32 * 1024;
const PNG_MAGIC = Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]);

function writeMessage(obj) {
  const data = Buffer.from(JSON.stringify(obj) + '\n', 'utf8');
  if (data.length > MAX_FRAME) throw new Error('outgoing frame exceeds limit');
  fs.writeSync(OUT_FD, data);
}

function reply(id, result, error) {
  const msg = { jsonrpc: '2.0', id };
  if (error !== undefined && error !== null) msg.error = error;
  else msg.result = result;
  writeMessage(msg);
}

class Bounded {
  constructor(head = 64 * 1024, tail = 64 * 1024) {
    this.headLimit = head;
    this.tailLimit = tail;
    this.head = '';
    this.tail = '';
    this.total = 0;
  }
  write(chunk) {
    const text = typeof chunk === 'string' ? chunk : Buffer.from(chunk).toString('utf8');
    this.total += text.length;
    let rest = text;
    if (this.head.length < this.headLimit) {
      const room = this.headLimit - this.head.length;
      this.head += rest.slice(0, room);
      rest = rest.slice(room);
    }
    if (rest) {
      this.tail += rest;
      if (this.tail.length > this.tailLimit) this.tail = this.tail.slice(this.tail.length - this.tailLimit);
    }
    return true;
  }
  get value() {
    const kept = this.head.length + this.tail.length;
    if (this.total <= kept) return this.head + this.tail;
    return this.head + `\n... [truncated ${this.total - kept} characters] ...\n` + this.tail;
  }
  get truncated() {
    return this.total > this.head.length + this.tail.length;
  }
}

// ---------------------------------------------------------------------------
// Source analysis: mask strings/comments, find top-level declarations and
// top-level static imports, and rewrite imports into await import() calls.

function maskCode(code) {
  let out = '';
  let i = 0;
  const n = code.length;
  while (i < n) {
    const c = code[i];
    const d = code[i + 1];
    if (c === '/' && d === '/') {
      while (i < n && code[i] !== '\n') { out += ' '; i++; }
      continue;
    }
    if (c === '/' && d === '*') {
      out += '  ';
      i += 2;
      while (i < n && !(code[i] === '*' && code[i + 1] === '/')) {
        out += code[i] === '\n' ? '\n' : ' ';
        i++;
      }
      if (i < n) { out += '  '; i += 2; }
      continue;
    }
    if (c === '"' || c === "'") {
      out += ' ';
      i++;
      while (i < n) {
        if (code[i] === '\\') { out += '  '; i += 2; continue; }
        if (code[i] === c) { out += ' '; i++; break; }
        out += code[i] === '\n' ? '\n' : ' ';
        i++;
      }
      continue;
    }
    if (c === '`') {
      out += ' ';
      i++;
      while (i < n) {
        if (code[i] === '\\') { out += '  '; i += 2; continue; }
        if (code[i] === '`') { out += ' '; i++; break; }
        out += code[i] === '\n' ? '\n' : ' ';
        i++;
      }
      continue;
    }
    out += c;
    i++;
  }
  return out;
}

function lastSignificant(s, i) {
  while (i >= 0) {
    const c = s[i];
    if (c !== ' ' && c !== '\t' && c !== '\r') return c;
    i--;
  }
  return '';
}

function nextSignificant(s, i) {
  while (i < s.length) {
    const c = s[i];
    if (c !== ' ' && c !== '\t' && c !== '\r' && c !== '\n') return c;
    i++;
  }
  return '';
}

const CONTINUE_BEFORE = ',=+-*/%&|^<>!?:([{';
const CONTINUE_AFTER = '.=+-*/%&|^<>?:,)]}';

function declarationEnd(masked, start) {
  let depth = 0;
  let j = start;
  while (j < masked.length) {
    const c = masked[j];
    if ('([{'.includes(c)) depth++;
    else if (')]}'.includes(c)) {
      if (depth === 0) break;
      depth--;
    } else if (depth === 0 && c === ';') break;
    else if (depth === 0 && c === '\n') {
      const prev = lastSignificant(masked, j - 1);
      const next = nextSignificant(masked, j + 1);
      if (prev && CONTINUE_BEFORE.includes(prev)) { j++; continue; }
      if (next && CONTINUE_AFTER.includes(next)) { j++; continue; }
      break;
    }
    j++;
  }
  return j;
}

function splitTopLevel(text, sep) {
  const parts = [];
  let depth = 0;
  let start = 0;
  for (let i = 0; i < text.length; i++) {
    const c = text[i];
    if ('([{'.includes(c)) depth++;
    else if (')]}'.includes(c)) depth--;
    else if (c === sep && depth === 0) {
      parts.push(text.slice(start, i));
      start = i + 1;
    }
  }
  parts.push(text.slice(start));
  return parts;
}

function stripDefault(text) {
  let depth = 0;
  for (let i = 0; i < text.length; i++) {
    const c = text[i];
    if ('([{'.includes(c)) depth++;
    else if (')]}'.includes(c)) depth--;
    else if (c === '=' && depth === 0) {
      const next = text[i + 1];
      const prev = text[i - 1];
      if (next === '=' || next === '>' || prev === '=' || prev === '!' || prev === '<' || prev === '>') continue;
      return text.slice(0, i);
    }
  }
  return text;
}

function bindingNames(pattern, out) {
  let text = String(pattern).trim();
  if (!text) return;
  if (text.startsWith('...')) text = text.slice(3).trim();
  text = stripDefault(text).trim();
  if (!text) return;
  if (text.startsWith('{')) {
    const close = matchingBrace(text, 0);
    const inner = text.slice(1, close < 0 ? text.length : close);
    for (const element of splitTopLevel(inner, ',')) {
      let part = element.trim();
      if (!part) continue;
      if (part.startsWith('...')) { bindingNames(part.slice(3), out); continue; }
      const colon = topLevelColon(part);
      if (colon >= 0) { bindingNames(part.slice(colon + 1), out); continue; }
      const id = /^([A-Za-z_$][\w$]*)/.exec(part);
      if (id) out.add(id[1]);
    }
    return;
  }
  if (text.startsWith('[')) {
    const close = matchingBrace(text, 0);
    const inner = text.slice(1, close < 0 ? text.length : close);
    for (const element of splitTopLevel(inner, ',')) {
      if (element.trim()) bindingNames(element, out);
    }
    return;
  }
  const id = /^([A-Za-z_$][\w$]*)/.exec(text);
  if (id) out.add(id[1]);
}

function matchingBrace(text, openIndex) {
  const open = text[openIndex];
  const close = open === '{' ? '}' : open === '[' ? ']' : ')';
  let depth = 0;
  for (let i = openIndex; i < text.length; i++) {
    if (text[i] === open) depth++;
    else if (text[i] === close) {
      depth--;
      if (depth === 0) return i;
    }
  }
  return -1;
}

function topLevelColon(text) {
  let depth = 0;
  for (let i = 0; i < text.length; i++) {
    const c = text[i];
    if ('([{'.includes(c)) depth++;
    else if (')]}'.includes(c)) depth--;
    else if (c === ':' && depth === 0) return i;
  }
  return -1;
}

function parseImportStatement(text) {
  const trimmed = text.trim().replace(/;$/, '').trim();
  let m;
  if ((m = /^import\s+(['"])([^'"]+)\1$/.exec(trimmed))) {
    return { names: [], replacement: `await import(${JSON.stringify(m[2])});` };
  }
  m = /^import\s+(?:([A-Za-z_$][\w$]*)\s*(?:,\s*)?)?(?:\*\s+as\s+([A-Za-z_$][\w$]*))?(?:\s*,\s*)?(?:\{([^}]*)\})?\s*from\s*(['"])([^'"]+)\4$/.exec(trimmed);
  if (!m) return null;
  const [, defaultName, namespaceName, namedText, , specifier] = m;
  const mod = JSON.stringify(specifier);
  const pieces = [];
  const names = [];
  if (namespaceName) {
    pieces.push(`const ${namespaceName} = await import(${mod});`);
    names.push(namespaceName);
  }
  if (defaultName) {
    pieces.push(`const ${defaultName} = (await import(${mod})).default;`);
    names.push(defaultName);
  }
  if (namedText) {
    const pairs = [];
    for (const element of splitTopLevel(namedText, ',')) {
      const part = element.trim();
      if (!part) continue;
      const asMatch = /^([A-Za-z_$][\w$]*)\s+as\s+([A-Za-z_$][\w$]*)$/.exec(part);
      if (asMatch) { pairs.push(`${asMatch[1]}: ${asMatch[2]}`); names.push(asMatch[2]); }
      else if (/^[A-Za-z_$][\w$]*$/.test(part)) { pairs.push(part); names.push(part); }
    }
    if (pairs.length) pieces.push(`const { ${pairs.join(', ')} } = await import(${mod});`);
  }
  if (!pieces.length) return null;
  return { names, replacement: pieces.join(' ') };
}

function analyze(code) {
  const masked = maskCode(code);
  const names = new Set();
  const edits = [];
  const depths = new Int32Array(masked.length + 1);
  let depth = 0;
  for (let i = 0; i < masked.length; i++) {
    depths[i] = depth;
    const c = masked[i];
    if ('([{'.includes(c)) depth++;
    else if (')]}'.includes(c)) depth = Math.max(0, depth - 1);
  }
  depths[masked.length] = depth;
  const isStatementStart = (index) => {
    if (depths[index] !== 0) return false;
    if (index === 0) return true;
    const prev = lastSignificant(masked, index - 1);
    if (prev === ';' || prev === '}' || prev === '') return true;
    // Automatic semicolon insertion: a newline at depth 0 starts a statement.
    for (let k = index - 1; k >= 0; k--) {
      const c = masked[k];
      if (c === '\n') return true;
      if (c !== ' ' && c !== '\t' && c !== '\r') break;
    }
    return false;
  };
  let i = 0;
  while (i < masked.length) {
    const c = masked[i];
    if (c === ' ' || c === '\t' || c === '\r' || c === '\n') { i++; continue; }
    if (!isStatementStart(i)) { i++; continue; }
    const rest = masked.slice(i);
    let m;
    if ((m = /^import\b/.exec(rest))) {
      const end = declarationEnd(masked, i + m[0].length);
      const original = code.slice(i, end);
      const parsed = parseImportStatement(original);
      if (parsed) {
        edits.push({ start: i, end, replacement: parsed.replacement });
        for (const name of parsed.names) names.add(name);
      }
      i = end;
      continue;
    }
    if ((m = /^(?:export\s+)?(?:async\s+)?function\s*\*?\s*([A-Za-z_$][\w$]*)/.exec(rest))) {
      names.add(m[1]);
      i += m[0].length;
      continue;
    }
    if ((m = /^(?:export\s+)?class\s+([A-Za-z_$][\w$]*)/.exec(rest))) {
      names.add(m[1]);
      i += m[0].length;
      continue;
    }
    if ((m = /^(?:const|let|var)\s+/.exec(rest))) {
      const declStart = i + m[0].length;
      const end = declarationEnd(masked, declStart);
      const declaration = masked.slice(declStart, end);
      for (const part of splitTopLevel(declaration, ',')) {
        if (part.trim()) bindingNames(part, names);
      }
      i = end;
      continue;
    }
    i++;
  }
  let transformed = code;
  for (let k = edits.length - 1; k >= 0; k--) {
    const edit = edits[k];
    transformed = transformed.slice(0, edit.start) + edit.replacement + transformed.slice(edit.end);
  }
  return { transformed, names: [...names] };
}

// splitCompletion separates a trailing simple expression from the statements
// before it, so a cell that ends with an expression reports its value (REPL
// semantics) without evaluating anything twice.
function splitCompletion(code) {
  const trimmed = code.replace(/\s+$/, '');
  if (!trimmed) return { body: code, expression: '' };
  // Candidate: text after the last top-level ';' (covers "const a = 1; a * 2"),
  // else the last line.
  let splitAt = -1;
  let depth = 0;
  const masked = maskCode(trimmed);
  for (let i = 0; i < masked.length; i++) {
    const c = masked[i];
    if ('([{'.includes(c)) depth++;
    else if (')]}'.includes(c)) depth = Math.max(0, depth - 1);
    else if (c === ';' && depth === 0) splitAt = i;
  }
  let body;
  let candidate;
  if (splitAt >= 0) {
    body = trimmed.slice(0, splitAt);
    candidate = trimmed.slice(splitAt + 1).trim();
  } else {
    const lines = trimmed.split('\n');
    let index = lines.length - 1;
    while (index >= 0 && !lines[index].trim()) index--;
    if (index < 0) return { body: code, expression: '' };
    candidate = lines[index].trim();
    body = lines.slice(0, index).join('\n');
  }
  if (!candidate) return { body: code, expression: '' };
  if (/^(?:const|let|var|function|class|if|for|while|do|switch|try|throw|return|import|export|\}|\{)/.test(candidate)) {
    return { body: code, expression: '' };
  }
  if (/[{(,=+\-*/%&|^<>?:]$/.test(candidate)) {
    return { body: code, expression: '' };
  }
  const expression = candidate.replace(/;\s*$/, '');
  if (!expression) return { body: code, expression: '' };
  return { body, expression };
}

// ---------------------------------------------------------------------------

class Worker {
  constructor() {
    this.generation = 1;
    this.state = Object.create(null);
    this.config = {};
    this.tools = [];
    this.toolNames = new Set();
    this.artifactDir = null;
    this.workspace = process.cwd();
    this.limits = {};
    this.callSeq = 0;
    this.pending = new Map();
    this.artifacts = [];
    this.out = null;
    this.err = null;
    this.interruptRequested = false;
    this.context = null;
  }

  makeContext() {
    const self = this;
    const sandbox = {
      console: {
        log: (...args) => self.writeOut(util.format(...args) + '\n'),
        info: (...args) => self.writeOut(util.format(...args) + '\n'),
        warn: (...args) => self.writeErr(util.format(...args) + '\n'),
        error: (...args) => self.writeErr(util.format(...args) + '\n'),
        debug: (...args) => self.writeErr(util.format(...args) + '\n'),
      },
      process,
      Buffer,
      URL,
      URLSearchParams,
      TextEncoder,
      TextDecoder,
      AbortController,
      AbortSignal,
      setTimeout,
      clearTimeout,
      setInterval,
      clearInterval,
      setImmediate,
      clearImmediate,
      queueMicrotask,
      structuredClone,
      atob,
      btoa,
      require: require,
      lc_tool: (name, args) => self.callTool(name, args || {}),
      lc_tools: () => JSON.parse(JSON.stringify(self.tools)),
      lc_artifact: (data, name, mime) => self.artifact(data, name, mime),
      display: (value) => self.display(value),
      lc_display: (value) => self.display(value),
      __lc_state: this.state,
    };
    sandbox.globalThis = sandbox;
    this.context = vm.createContext(sandbox, { name: 'lumen-cortex-cell' });
    return this.context;
  }

  // syncStateFromGlobals copies live globals back into the persisted state, so a
  // value mutated by a later cell is not overwritten by a stale snapshot.
  syncStateFromGlobals() {
    if (!this.context) return;
    for (const key of Object.keys(this.state)) {
      try {
        const current = this.context[key];
        if (current !== undefined) this.state[key] = current;
      } catch (e) { /* keep the previous value */ }
    }
  }

  writeOut(text) {
    if (this.out) this.out.write(text);
  }

  writeErr(text) {
    if (this.err) this.err.write(text);
  }

  safeName(name, fallback = 'artifact') {
    const base = path.basename(String(name || '').replace(/\\/g, '/')).trim();
    const cleaned = base.replace(/[^A-Za-z0-9._\- ]/g, '').replace(/\.\./g, '_').replace(/^[.\s]+|[.\s]+$/g, '');
    return cleaned || fallback;
  }

  mimeFor(name, fallback = 'application/octet-stream') {
    const ext = path.extname(name || '').toLowerCase();
    const table = {
      '.txt': 'text/plain', '.csv': 'text/csv', '.json': 'application/json',
      '.png': 'image/png', '.jpg': 'image/jpeg', '.jpeg': 'image/jpeg',
      '.svg': 'image/svg+xml', '.html': 'text/html', '.md': 'text/markdown',
      '.pdf': 'application/pdf', '.bin': 'application/octet-stream',
    };
    return table[ext] || fallback;
  }

  artifactLimit() {
    const value = Number(this.limits.artifactBytes || 0);
    return value > 0 ? value : 8 << 20;
  }

  artifact(data, name, mime) {
    if (!this.artifactDir) throw new Error('artifact directory is not configured');
    let payload;
    let safeName = name;
    if (data && typeof data === 'object' && typeof data.toBuffer === 'function') {
      payload = data.toBuffer();
    } else if (typeof data === 'string') {
      if (fs.existsSync(data) && fs.statSync(data).isFile()) {
        payload = fs.readFileSync(data);
        if (!safeName) safeName = path.basename(data);
      } else {
        payload = Buffer.from(data, 'utf8');
      }
    } else if (data instanceof ArrayBuffer) {
      payload = Buffer.from(data);
    } else if (ArrayBuffer.isView(data)) {
      payload = Buffer.from(data.buffer, data.byteOffset, data.byteLength);
    } else {
      payload = Buffer.from(String(data), 'utf8');
    }
    const limit = this.artifactLimit();
    if (payload.length > limit) throw new Error(`artifact exceeds the ${limit} byte limit`);
    const safe = this.safeName(safeName || 'artifact.bin');
    fs.mkdirSync(this.artifactDir, { recursive: true });
    fs.writeFileSync(path.join(this.artifactDir, safe), payload);
    const record = { name: safe, mime: mime || this.mimeFor(safe), size: payload.length };
    this.artifacts = this.artifacts.filter((a) => a.name !== safe);
    this.artifacts.push(record);
    return record;
  }

  display(value) {
    const payload = displayPayload(value);
    if (!payload) return null;
    writeMessage({
      jsonrpc: '2.0',
      method: 'event',
      params: { type: 'display', data: { mime: payload.mime, encoding: 'utf-8', text: payload.text, size: payload.size } },
    });
    return payload;
  }

  collectArtifacts() {
    if (this.artifactDir && fs.existsSync(this.artifactDir)) {
      for (const entry of fs.readdirSync(this.artifactDir).sort()) {
        const target = path.join(this.artifactDir, entry);
        let stat;
        try { stat = fs.statSync(target); } catch (e) { continue; }
        if (!stat.isFile()) continue;
        if (this.artifacts.some((a) => a.name === entry)) continue;
        this.artifacts.push({ name: entry, mime: this.mimeFor(entry), size: stat.size });
      }
    }
    const result = this.artifacts.slice();
    this.artifacts = [];
    return result;
  }

  async callTool(name, args) {
    if (!this.toolNames.has(name)) {
      const err = new Error(`tool ${JSON.stringify(name)} is not authorized for this execution`);
      err.name = 'ToolDenied';
      throw err;
    }
    const id = `w${++this.callSeq}`;
    writeMessage({ jsonrpc: '2.0', id, method: 'tool/call', params: { callId: id, name, arguments: args } });
    const response = await new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject });
    });
    if (response.denied) {
      const err = new Error(response.content || 'tool call denied');
      err.name = 'ToolDenied';
      throw err;
    }
    if (!response.ok) {
      const err = new Error(response.content || 'tool call failed');
      err.name = 'ToolFailed';
      throw err;
    }
    return response.content;
  }

  async execute(params) {
    const code = String(params.code || '');
    if (Array.isArray(params.tools)) {
      this.tools = params.tools;
      this.toolNames = new Set(this.tools.map((t) => t && t.name).filter(Boolean));
    }
    const started = Date.now();
    const out = new Bounded();
    const err = new Bounded();
    this.out = out;
    this.err = err;
    this.interruptRequested = false;
    const budget = params.budget || {};
    let remaining = Number(budget.wallSeconds || 0) * 1000;
    if (params.deadlineUnixMs) remaining = Math.max(1, Number(params.deadlineUnixMs) - Date.now());
    if (!Number.isFinite(remaining) || remaining <= 0) remaining = 300000;
    let status = 'completed';
    let error = '';
    let traceback = '';
    const displays = [];
    const context = this.context || this.makeContext();
    try {
      let value;
      if (this.tryExpression(code)) {
        // Expression mode: restore the persisted bindings, then evaluate.
        const script = new vm.Script(`(async () => { Object.assign(globalThis, __lc_state); return (${code}\n); })()`,
          { filename: '<cell>', importModuleDynamically: this.importHook() });
        value = await this.awaitCell(script.runInContext(context, { timeout: remaining }), remaining);
        this.syncStateFromGlobals();
      } else {
        const analysis = analyze(code);
        const completion = splitCompletion(analysis.transformed);
        const capture = analysis.names
          .map((name) => `try { __lc_capture[${JSON.stringify(name)}] = ${name}; } catch (e) {}`)
          .join('\n');
        const lastExpression = completion.expression ? `(${completion.expression})` : 'undefined';
        const wrapped = `(async () => {\nObject.assign(globalThis, __lc_state);\n${completion.body}\n;const __lc_capture = {};\n${capture}\n;Object.assign(globalThis, __lc_capture);\nconst __lc_last = ${lastExpression};\nreturn { capture: __lc_capture, last: __lc_last };\n})()`;
        const script = new vm.Script(wrapped, { filename: '<cell>', importModuleDynamically: this.importHook() });
        const outcome = await this.awaitCell(script.runInContext(context, { timeout: remaining }), remaining);
        if (outcome && typeof outcome === 'object') {
          for (const [key, captured] of Object.entries(outcome.capture || {})) this.state[key] = captured;
          value = outcome.last;
        }
        this.syncStateFromGlobals();
      }
      if (value !== undefined && value !== null) {
        this.state._ = value;
        const payload = displayPayload(value);
        if (payload) displays.push({ mime: payload.mime, encoding: 'utf-8', text: payload.text, size: payload.size });
      }
    } catch (exc) {
      if (exc && (exc.name === 'Interrupt' || exc.__lcInterrupt)) {
        status = 'cancelled';
        error = 'Interrupt';
      } else if (exc && (exc.__lcTimeout || exc.code === 'ERR_SCRIPT_EXECUTION_TIMEOUT' || /Script execution timed out/.test(String(exc.message || '')))) {
        status = 'timeout';
        error = 'TimeoutError: cell exceeded its wall budget';
      } else {
        status = 'failed';
        error = `${exc && exc.name ? exc.name : 'Error'}: ${exc && exc.message ? exc.message : String(exc)}`;
        traceback = String((exc && exc.stack) || '').slice(0, MAX_TRACEBACK);
      }
    } finally {
      this.out = null;
      this.err = null;
    }
    return {
      status,
      stdout: out.value,
      stderr: err.value,
      error,
      traceback,
      stateStatus: 'ok',
      exitCode: 0,
      displays,
      artifacts: this.collectArtifacts(),
      durationMs: Date.now() - started,
      outputTruncated: out.truncated || err.truncated,
    };
  }

  importHook() {
    if (vm.constants && vm.constants.USE_MAIN_CONTEXT_DEFAULT_LOADER !== undefined) {
      return vm.constants.USE_MAIN_CONTEXT_DEFAULT_LOADER;
    }
    return (specifier) => import(specifier);
  }

  tryExpression(code) {
    try {
      // eslint-disable-next-line no-new
      new vm.Script(`(${code}\n)`, { filename: '<probe>' });
      return true;
    } catch (e) {
      return false;
    }
  }

  awaitCell(value, remaining) {
    const timeout = new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        const err = new Error('cell exceeded its wall budget');
        err.__lcTimeout = true;
        reject(err);
      }, Math.max(1, remaining));
      if (timer.unref) timer.unref();
    });
    const interrupted = new Promise((resolve, reject) => {
      this.interruptWaiter = () => {
        const err = new Error('interrupted');
        err.__lcInterrupt = true;
        reject(err);
      };
    });
    const work = Promise.resolve(value);
    return Promise.race([work, timeout, interrupted]).finally(() => {
      this.interruptWaiter = null;
    });
  }

  initialize(params) {
    this.config = params || {};
    this.workspace = this.config.workspace || process.cwd();
    this.artifactDir = this.config.artifactDir || null;
    this.limits = this.config.limits || {};
    this.tools = this.config.tools || [];
    this.toolNames = new Set(this.tools.map((t) => t.name).filter(Boolean));
    try { process.chdir(this.workspace); } catch (e) { /* keep the inherited cwd */ }
    this.makeContext();
    return {
      spiVersion: this.config.spiVersion || '1.0',
      runtimeVersion: process.versions.node,
      language: 'javascript',
      pid: process.pid,
      capabilities: ['persistent_execution', 'modules', 'file_io', 'tool_callback',
        'artifact', 'display', 'subprocess', 'notebook'],
    };
  }

  reset() {
    this.state = Object.create(null);
    this.artifacts = [];
    this.callSeq = 0;
    this.pending.clear();
    this.makeContext();
    this.generation += 1;
    return { generation: this.generation, pid: process.pid };
  }

  health() {
    return { status: 'ok', generation: this.generation, pid: process.pid, workspace: this.workspace };
  }

  async dispatch(msg) {
    const method = msg.method;
    const id = msg.id;
    const params = msg.params || {};
    if (id === undefined || id === null) {
      if (method === 'interrupt') {
        this.interruptRequested = true;
        if (this.interruptWaiter) this.interruptWaiter();
      }
      return;
    }
    try {
      let result;
      if (method === 'initialize') result = this.initialize(params);
      else if (method === 'execute') result = await this.execute(params);
      else if (method === 'reset') result = this.reset();
      else if (method === 'health') result = this.health();
      else if (method === 'close') {
        reply(id, { closed: true });
        process.exit(0);
      } else {
        reply(id, undefined, { code: -32601, message: `unknown method ${JSON.stringify(method)}` });
        return;
      }
      reply(id, result);
    } catch (exc) {
      reply(id, undefined, { code: -32603, message: `${exc && exc.name ? exc.name : 'Error'}: ${exc && exc.message ? exc.message : String(exc)}` });
    }
  }
}

function displayPayload(value) {
  try {
    if (value === undefined || value === null) return null;
    if (Buffer.isBuffer(value) && value.length >= 8 && value.subarray(0, 8).equals(PNG_MAGIC)) {
      return { mime: 'image/png', text: base64(value), size: value.length };
    }
    if (typeof value === 'object' && typeof value.toBuffer === 'function') {
      const buf = value.toBuffer();
      if (buf.length >= 8 && buf.subarray(0, 8).equals(PNG_MAGIC)) {
        return { mime: 'image/png', text: base64(buf), size: buf.length };
      }
    }
    if (typeof value === 'object' && typeof value._repr_html_ === 'function') {
      const text = String(value._repr_html_()).slice(0, MAX_REPR);
      return { mime: 'text/html', text, size: text.length };
    }
    if (typeof value === 'string') {
      return { mime: 'text/plain', text: value.slice(0, MAX_REPR), size: value.length };
    }
    let text;
    try { text = util.inspect(value, { depth: 2, maxArrayLength: 100, breakLength: 120 }); } catch (e) { text = String(value); }
    if (text.length > MAX_REPR) text = text.slice(0, MAX_REPR) + '... [repr truncated]';
    return { mime: 'text/plain', text, size: text.length };
  } catch (e) {
    return null;
  }
}

// ---------------------------------------------------------------------------
// Protocol pump. Responses to our tool calls resolve pending promises; host
// requests are queued and handled one at a time.

function main() {
  const worker = new Worker();
  const queue = [];
  let running = false;
  let closed = false;

  const pumpQueue = async () => {
    if (running) return;
    running = true;
    while (queue.length) {
      const message = queue.shift();
      try {
        await worker.dispatch(message);
      } catch (exc) {
        try { reply(message.id, undefined, { code: -32603, message: String(exc && exc.message ? exc.message : exc) }); } catch (e) { /* ignore */ }
      }
    }
    running = false;
    if (closed && queue.length === 0) process.exit(0);
  };

  const stream = fs.createReadStream(null, { fd: IN_FD, autoClose: false, highWaterMark: 1 << 16 });
  let buffer = '';
  stream.on('data', (chunk) => {
    buffer += chunk.toString('utf8');
    let index;
    while ((index = buffer.indexOf('\n')) >= 0) {
      const line = buffer.slice(0, index);
      buffer = buffer.slice(index + 1);
      if (!line.trim()) continue;
      if (line.length > MAX_FRAME) {
        try { reply(null, undefined, { code: -32700, message: 'incoming frame exceeds limit' }); } catch (e) { /* ignore */ }
        process.exit(2);
      }
      let message;
      try { message = JSON.parse(line); } catch (e) {
        try { reply(null, undefined, { code: -32700, message: 'invalid JSON frame' }); } catch (err) { /* ignore */ }
        continue;
      }
      if (message.id !== undefined && message.id !== null && message.method === undefined) {
        const pending = worker.pending.get(String(message.id));
        if (pending) {
          worker.pending.delete(String(message.id));
          pending.resolve(message.result || {});
        }
        continue;
      }
      if (message.method === undefined) continue;
      if (message.id === undefined || message.id === null) {
        worker.dispatch(message).catch(() => {});
        continue;
      }
      queue.push(message);
      pumpQueue();
    }
  });
  stream.on('end', () => { closed = true; if (!running) process.exit(0); });
  stream.on('error', () => process.exit(2));
}

main();
