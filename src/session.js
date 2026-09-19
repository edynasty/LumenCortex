import fs from 'node:fs';
import path from 'node:path';
import { randomUUID } from 'node:crypto';

export class AgentSessionStore {
  constructor(repositoryDir) {
    this.dir = path.join(repositoryDir, 'sessions');
    fs.mkdirSync(this.dir, { recursive: true });
  }

  create(input = {}) {
    const now = new Date().toISOString();
    const session = {
      id: input.id ?? `session_${randomUUID().replaceAll('-', '').slice(0, 16)}`,
      createdAt: now,
      updatedAt: now,
      status: 'running',
      provider: input.provider ?? null,
      model: input.model ?? null,
      goal: input.goal ?? '',
      messages: input.messages ?? [],
      steps: input.steps ?? [],
      metadata: input.metadata ?? {}
    };
    this.save(session);
    return session;
  }

  save(session) {
    session.updatedAt = new Date().toISOString();
    fs.writeFileSync(this.file(session.id), JSON.stringify(session, null, 2));
    return session;
  }

  load(id) {
    const file = this.file(id);
    if (!fs.existsSync(file)) throw new Error(`Unknown session: ${id}`);
    return JSON.parse(fs.readFileSync(file, 'utf8'));
  }

  list(limit = 20) {
    return fs.readdirSync(this.dir)
      .filter((name) => name.endsWith('.json'))
      .map((name) => JSON.parse(fs.readFileSync(path.join(this.dir, name), 'utf8')))
      .sort((a, b) => String(b.updatedAt).localeCompare(String(a.updatedAt)))
      .slice(0, limit)
      .map(({ messages, steps, ...meta }) => ({ ...meta, messageCount: messages.length, stepCount: steps.length }));
  }

  file(id) {
    if (!/^[A-Za-z0-9._-]+$/.test(id)) throw new Error(`Invalid session id: ${id}`);
    return path.join(this.dir, `${id}.json`);
  }
}
