import test from 'node:test';
import assert from 'node:assert/strict';
import crypto from 'node:crypto';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { LumenCortexDatabase, MigratedDatabaseError } from '../src/database.js';
import { AgentSessionStore } from '../src/session.js';
import { CognitiveRepository } from '../src/repository.js';

// M2.1: the Go runtime owns a database once PRAGMA user_version >= 1. The legacy Node runtime must
// refuse it before any write: no pragma that changes the header, no schema initialisation, no
// schema_version row.

function files(dir) {
  return Object.fromEntries(fs.readdirSync(dir).sort().map((name) => {
    const bytes = fs.readFileSync(path.join(dir, name));
    return [name, crypto.createHash('sha256').update(bytes).digest('hex')];
  }));
}

// A database as the Go runtime leaves it: user_version 1, journal_mode WAL, a session, no schema_version.
function migratedState(version = 1) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'lcx-migrated-'));
  const db = new DatabaseSync(path.join(dir, 'lumencortex.db'));
  db.exec(`
    PRAGMA journal_mode = WAL;
    CREATE TABLE metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL) STRICT;
    CREATE TABLE sessions (id TEXT PRIMARY KEY, goal TEXT NOT NULL) STRICT;
    INSERT INTO sessions VALUES ('s1', 'goal');
    PRAGMA user_version = ${version};
    PRAGMA wal_checkpoint(TRUNCATE);
  `);
  db.close();
  return dir;
}

for (const version of [1, 2, 7]) {
  test(`Node refuses a database at user_version ${version} without writing`, () => {
    const dir = migratedState(version);
    const before = files(dir);

    assert.throws(
      () => new LumenCortexDatabase(dir),
      (error) => error instanceof MigratedDatabaseError
        && error.code === 'LCX_DATABASE_MIGRATED'
        && error.userVersion === version
        && /Go runtime/.test(error.message)
    );

    // Every Node entry point opens the same class; the stores must fail the same way.
    assert.throws(() => new AgentSessionStore(dir), MigratedDatabaseError);

    const after = files(dir);
    assert.equal(after['lumencortex.db'], before['lumencortex.db'], 'main database file changed');
    for (const name of Object.keys(after)) {
      assert.ok(name in before || name.endsWith('-wal') || name.endsWith('-shm'), `unexpected new file ${name}`);
    }
    const check = new DatabaseSync(path.join(dir, 'lumencortex.db'));
    try {
      const tables = check.prepare("SELECT name FROM sqlite_master WHERE type='table' ORDER BY name").all().map((r) => r.name);
      assert.deepEqual(tables, ['metadata', 'sessions'], 'Node schema initialisation ran');
      assert.equal(check.prepare("SELECT count(*) AS n FROM metadata WHERE key='schema_version'").get().n, 0);
    } finally {
      check.close();
    }
  });
}

test('Node refuses a migrated database through the repository entry point', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'lcx-migrated-ws-'));
  const state = path.join(root, '.lumencortex');
  fs.mkdirSync(state);
  const db = new DatabaseSync(path.join(state, 'lumencortex.db'));
  db.exec('PRAGMA user_version = 1');
  db.close();
  assert.throws(() => new CognitiveRepository(root), MigratedDatabaseError);
});

test('Node still opens a legacy version-0 database and initialises it as before', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'lcx-legacy-'));
  const first = new LumenCortexDatabase(dir);
  assert.equal(first.db.prepare('PRAGMA user_version').get().user_version, 0);
  assert.equal(first.getMeta('schema_version'), '3');
  first.db.close();
  // Reopening an initialised version-0 database keeps working.
  const second = new LumenCortexDatabase(dir);
  assert.equal(second.getMeta('schema_version'), '3');
  second.db.close();
});
