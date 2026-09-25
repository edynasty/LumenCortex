import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

export const SKILL_SCOPES = Object.freeze({
  GLOBAL: 'global',
  PROJECT: 'project',
  EFFECTIVE: 'effective'
});

export const SKILL_LIMITS = Object.freeze({
  maxSkills: 128,
  maxSkillBytes: 128 << 10,
  maxPromptBytes: 256 << 10,
  maxStateBytes: 64 << 10
});

const SKILL_ID = /^[A-Za-z0-9._-]{1,64}$/;

export class SkillRegistry {
  constructor({ globalRoot = defaultGlobalSkillRoot(), projectRoot } = {}) {
    this.globalRoot = globalRoot ? path.resolve(globalRoot) : '';
    this.projectRoot = projectRoot ? path.resolve(projectRoot) : '';
    if (!this.globalRoot && !this.projectRoot) {
      throw new Error('At least one Skill root is required');
    }
  }

  list(scope = SKILL_SCOPES.EFFECTIVE) {
    return sortSkills([...this.#rawScope(scope).values()].map((item) => publicSkill(item)));
  }

  content(scope, id) {
    validateSkillId(id);
    const item = this.#rawScope(scope).get(id);
    if (!item) {
      const error = new Error(`Unknown Skill: ${id}`);
      error.code = 'ENOENT';
      throw error;
    }
    if (item.error) throw new Error(item.error);
    return item.content;
  }

  save(scope, id, content) {
    validateSkillId(id);
    if (!String(content ?? '').trim()) throw new Error('Skill content is required');
    if (Buffer.byteLength(content) > SKILL_LIMITS.maxSkillBytes) {
      throw new Error('Skill content exceeds 128 KiB limit');
    }
    const root = this.#root(scope);
    const dir = path.join(root, id);
    fs.mkdirSync(dir, { recursive: true });
    const file = path.join(dir, 'SKILL.md');
    atomicWrite(file, String(content), 0o644);
    return publicSkill(this.#scanRoot(root, scope).get(id));
  }

  delete(scope, id) {
    validateSkillId(id);
    const root = this.#root(scope);
    const file = path.join(root, id, 'SKILL.md');
    try { fs.unlinkSync(file); } catch (error) {
      if (error?.code !== 'ENOENT') throw error;
    }
    const state = loadState(this.#statePath(scope));
    delete state.skills[id];
    saveState(this.#statePath(scope), state);
  }

  setEnabled(scope, id, enabled) {
    validateSkillId(id);
    if (![SKILL_SCOPES.GLOBAL, SKILL_SCOPES.PROJECT].includes(scope)) {
      throw new Error('Skill enable scope must be global or project');
    }
    const effective = this.#rawScope(SKILL_SCOPES.EFFECTIVE);
    if (!effective.has(id)) throw new Error(`Unknown Skill: ${id}`);
    if (scope === SKILL_SCOPES.GLOBAL && !this.#scanRoot(this.globalRoot, SKILL_SCOPES.GLOBAL).has(id)) {
      throw new Error(`Skill is not defined in global scope: ${id}`);
    }
    const state = loadState(this.#statePath(scope));
    state.skills[id] = { enabled: Boolean(enabled) };
    saveState(this.#statePath(scope), state);
  }

  prompt() {
    const items = this.#rawEffective();
    const ids = [...items.keys()].sort();
    let body = '';
    const skills = [];

    for (const id of ids) {
      const item = items.get(id);
      if (!item.enabled || item.error) continue;
      const header = `\n\n## Skill: ${item.name} (${item.id})\n`;
      let remaining = SKILL_LIMITS.maxPromptBytes - Buffer.byteLength(body);
      if (remaining <= Buffer.byteLength(header)) break;
      body += header;
      remaining = SKILL_LIMITS.maxPromptBytes - Buffer.byteLength(body);
      const content = truncateUtf8(item.content, remaining);
      body += content;
      skills.push(publicSkill(item));
      if (Buffer.byteLength(body) >= SKILL_LIMITS.maxPromptBytes) break;
    }

    if (!body) return { prompt: '', skills };
    return {
      prompt: 'The following LumenCortex Skills are enabled for this workspace. Treat them as task-specific development instructions unless they conflict with higher-priority instructions.' + body,
      skills
    };
  }

  #rawScope(scope) {
    if (scope === SKILL_SCOPES.GLOBAL) {
      return applyScopeState(
        this.#scanRoot(this.globalRoot, SKILL_SCOPES.GLOBAL),
        loadState(this.#statePath(SKILL_SCOPES.GLOBAL))
      );
    }
    if (scope === SKILL_SCOPES.PROJECT) {
      return applyScopeState(
        this.#scanRoot(this.projectRoot, SKILL_SCOPES.PROJECT),
        loadState(this.#statePath(SKILL_SCOPES.PROJECT))
      );
    }
    if (scope === SKILL_SCOPES.EFFECTIVE || !scope) return this.#rawEffective();
    throw new Error(`Unknown Skill scope: ${scope}`);
  }

  #rawEffective() {
    const global = this.#scanRoot(this.globalRoot, SKILL_SCOPES.GLOBAL);
    const project = this.#scanRoot(this.projectRoot, SKILL_SCOPES.PROJECT);
    const globalState = loadState(this.#statePath(SKILL_SCOPES.GLOBAL));
    const projectState = loadState(this.#statePath(SKILL_SCOPES.PROJECT));
    const out = new Map();

    for (const [id, raw] of global) {
      const item = { ...raw, enabled: globalState.skills[id]?.enabled ?? true };
      if (Object.hasOwn(projectState.skills, id)) item.enabled = Boolean(projectState.skills[id].enabled);
      out.set(id, item);
    }
    for (const [id, raw] of project) {
      out.set(id, {
        ...raw,
        enabled: projectState.skills[id]?.enabled ?? true,
        overridden: global.has(id)
      });
    }
    return out;
  }

  #scanRoot(root, scope) {
    const out = new Map();
    if (!root) return out;
    let entries;
    try {
      entries = fs.readdirSync(root, { withFileTypes: true });
    } catch (error) {
      if (error?.code === 'ENOENT') return out;
      throw error;
    }
    entries.sort((left, right) => left.name.localeCompare(right.name));

    for (const entry of entries) {
      if (out.size >= SKILL_LIMITS.maxSkills) break;
      if (!entry.isDirectory() || !SKILL_ID.test(entry.name)) continue;
      const file = path.join(root, entry.name, 'SKILL.md');
      let stat;
      try { stat = fs.statSync(file); } catch (error) {
        if (error?.code === 'ENOENT') continue;
        throw error;
      }
      const item = {
        id: entry.name,
        name: entry.name,
        description: '',
        scope,
        path: file,
        enabled: true,
        overridden: false,
        bytes: stat.size,
        error: '',
        content: ''
      };
      if (stat.size > SKILL_LIMITS.maxSkillBytes) {
        item.error = 'SKILL.md exceeds 128 KiB limit';
        out.set(item.id, item);
        continue;
      }
      try {
        item.content = fs.readFileSync(file, 'utf8');
        const metadata = parseSkillMetadata(item.id, item.content);
        item.name = metadata.name;
        item.description = metadata.description;
      } catch (error) {
        item.error = error.message;
      }
      out.set(item.id, item);
    }
    return out;
  }

  #root(scope) {
    if (scope === SKILL_SCOPES.GLOBAL) {
      if (!this.globalRoot) throw new Error('Global Skill root is unavailable');
      return this.globalRoot;
    }
    if (scope === SKILL_SCOPES.PROJECT) {
      if (!this.projectRoot) throw new Error('Project Skill root is unavailable');
      return this.projectRoot;
    }
    throw new Error('Skill write scope must be global or project');
  }

  #statePath(scope) {
    let root = '';
    try { root = this.#root(scope); } catch { return ''; }
    return path.join(path.dirname(root), 'skills-state.json');
  }
}

export function defaultGlobalSkillRoot({ env = process.env, platform = process.platform, home = os.homedir() } = {}) {
  if (env.LUMENCORTEX_SKILLS_GLOBAL_ROOT) return path.resolve(env.LUMENCORTEX_SKILLS_GLOBAL_ROOT);
  let configRoot;
  if (platform === 'darwin') configRoot = path.join(home, 'Library', 'Application Support');
  else if (platform === 'win32') configRoot = env.APPDATA || path.join(home, 'AppData', 'Roaming');
  else configRoot = env.XDG_CONFIG_HOME || path.join(home, '.config');
  return path.join(configRoot, 'lumencortex', 'skills');
}

export function projectSkillRoot(workspace) {
  return path.join(path.resolve(workspace), '.lumencortex', 'skills');
}

export function parseSkillMetadata(id, content) {
  let name = id;
  let description = '';
  const normalized = String(content ?? '').replace(/^\uFEFF/, '').replaceAll('\r\n', '\n');
  let body = normalized;
  if (normalized.startsWith('---\n')) {
    const end = normalized.indexOf('\n---\n', 4);
    if (end >= 0) {
      const front = normalized.slice(4, end);
      body = normalized.slice(end + 5);
      for (const line of front.split('\n')) {
        const separator = line.indexOf(':');
        if (separator < 0) continue;
        const key = line.slice(0, separator).trim();
        const value = line.slice(separator + 1).trim().replace(/^[`']|[`']$/g, '');
        if (key === 'name' && value) name = value;
        else if (key === 'description') description = value;
      }
    }
  }
  if (!description) {
    for (const line of body.split('\n')) {
      const value = line.trim();
      if (!value || value.startsWith('#')) continue;
      description = value;
      break;
    }
  }
  return {
    name: [...name].slice(0, 128).join(''),
    description: [...description].slice(0, 512).join('')
  };
}

function validateSkillId(id) {
  if (!SKILL_ID.test(String(id ?? '').trim())) {
    throw new Error('Skill id must match [A-Za-z0-9._-] and be at most 64 characters');
  }
}

function publicSkill(item) {
  return {
    id: item.id,
    name: item.name,
    description: item.description || '',
    scope: item.scope,
    path: item.path,
    enabled: Boolean(item.enabled),
    overridden: Boolean(item.overridden),
    bytes: Number(item.bytes ?? 0),
    error: item.error || ''
  };
}

function sortSkills(items) {
  return items.sort((left, right) => left.id.localeCompare(right.id));
}

function applyScopeState(items, state) {
  for (const [id, raw] of items) {
    items.set(id, {
      ...raw,
      enabled: state.skills[id]?.enabled ?? true
    });
  }
  return items;
}

function loadState(file) {
  const state = { skills: {} };
  if (!file) return state;
  let raw;
  try { raw = fs.readFileSync(file); } catch (error) {
    if (error?.code === 'ENOENT') return state;
    throw error;
  }
  if (raw.length > SKILL_LIMITS.maxStateBytes) throw new Error('Skill state exceeds runtime limit');
  const parsed = JSON.parse(raw.toString('utf8'));
  return {
    skills: parsed?.skills && typeof parsed.skills === 'object' && !Array.isArray(parsed.skills)
      ? parsed.skills
      : {}
  };
}

function saveState(file, state) {
  if (!file) throw new Error('Skill state path is unavailable');
  fs.mkdirSync(path.dirname(file), { recursive: true, mode: 0o700 });
  const raw = JSON.stringify(state, null, 2) + '\n';
  if (Buffer.byteLength(raw) > SKILL_LIMITS.maxStateBytes) {
    throw new Error('Skill state exceeds runtime limit');
  }
  atomicWrite(file, raw, 0o600);
}

function atomicWrite(file, content, mode) {
  const tmp = `${file}.tmp-${process.pid}-${Date.now()}`;
  fs.writeFileSync(tmp, content, { encoding: 'utf8', mode });
  try {
    fs.renameSync(tmp, file);
  } catch (error) {
    try { fs.rmSync(tmp, { force: true }); } catch {}
    throw error;
  }
}

function truncateUtf8(value, maxBytes) {
  if (maxBytes <= 0) return '';
  const buffer = Buffer.from(String(value ?? ''), 'utf8');
  if (buffer.length <= maxBytes) return buffer.toString('utf8');
  let end = Math.min(buffer.length, maxBytes);
  while (end > 0 && (buffer[end] & 0b11000000) === 0b10000000) end -= 1;
  return buffer.subarray(0, end).toString('utf8');
}
