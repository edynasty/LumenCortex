import { randomUUID } from 'node:crypto';
import { LumenCortexDatabase } from './database.js';

export class AgentSessionStore {
  constructor(repositoryDir) {
    this.database = new LumenCortexDatabase(repositoryDir);
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
    this.database.saveSession(session);
    return session;
  }

  load(id) {
    validateSessionId(id);
    const session = this.database.loadSession(id);
    if (!session) throw new Error(`Unknown session: ${id}`);
    return session;
  }

  list(limit = 20) {
    return this.database.listSessions(limit);
  }

  close() {
    this.database.close();
  }
}

function validateSessionId(id) {
  if (!/^[A-Za-z0-9._-]+$/.test(id)) throw new Error(`Invalid session id: ${id}`);
}
