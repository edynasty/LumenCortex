package migrate

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// cognitionTables are the tables 0014 creates after the mainline Hub schema.
var cognitionTables = []string{"maintenance_state", "maintenance_sources", "maintenance_dedupe", "cognition_candidates", "cognition_observations"}

// 14 -> 13 drops exactly what 0014 added: the schema equals the original v13 schema, the rows of every
// other table are unchanged, and a re-upgrade starts with empty maintenance state (a downgrade forgets
// the cognition state; the sessions it was derived from stay).
func TestCognitionMigrationRoundTrip(t *testing.T) {
	dir := newFixture(t, fixtureGo)
	path := filepath.Join(dir, dbFile)
	closeWS(t, open(t, dir, embeddedUpTo(t, 13)))
	db := inspect(t, path)
	v12Schema := normalizedSchema(t, path)
	v12Dump := dumpAll(t, db)
	db.Close()

	set := embeddedUpTo(t, 14)
	closeWS(t, open(t, dir, set))
	v13 := inspect(t, path)
	mustExec(t, v13, `INSERT INTO maintenance_state(id, project_id, policy_version, dirty_revision, cursor_revision, updated_at)
		VALUES(1, '/w', 'p1', 3, 1, 't')`)
	mustExec(t, v13, `INSERT INTO maintenance_dedupe(dedupe_key, source_ref, revision, generation, outcome, policy_version, created_at)
		VALUES('sha256:`+strings.Repeat("a", 64)+`', 'session:s@sha256:x', 1, 1, 'no_candidate', 'p1', 't')`)
	v13.Close()

	from, now, err := Downgrade(context.Background(), dir, set, 13)
	if err != nil || from != 14 || now != 13 {
		t.Fatalf("Downgrade: from=%d now=%d err=%v", from, now, err)
	}
	if got := normalizedSchema(t, path); got != v12Schema {
		t.Errorf("schema after cognition downgrade differs from the original Hub schema:\n%s\nwant\n%s", got, v12Schema)
	}
	after := inspect(t, path)
	if got := dumpAll(t, after); got != v12Dump {
		t.Errorf("rows of other tables changed across cognition downgrade:\n%s\nwant\n%s", got, v12Dump)
	}
	after.Close()

	ws := open(t, dir, set)
	defer closeWS(t, ws)
	if ws.From != 13 || ws.To != 14 {
		t.Errorf("re-upgrade From=%d To=%d", ws.From, ws.To)
	}
	if n := qInt(t, ws.DB, `SELECT (SELECT count(*) FROM maintenance_state) + (SELECT count(*) FROM maintenance_dedupe)`); n != 0 {
		t.Errorf("re-upgrade resurrected %d maintenance rows", n)
	}
}

// A Hub database that already has an object 0014 creates is not adopted: the migration fails naming
// 0014, the version stays 13 and the database is unchanged.
func TestCognitionMigrationRefusesExistingObject(t *testing.T) {
	collisions := map[string]string{}
	for _, name := range cognitionTables {
		collisions["table "+name] = `CREATE TABLE ` + name + `(keep TEXT); INSERT INTO ` + name + ` VALUES('precious')`
	}
	collisions["index idx_cognition_candidates_claim"] = `CREATE INDEX idx_cognition_candidates_claim ON sessions(id)`
	for name, setup := range collisions {
		t.Run(name, func(t *testing.T) {
			dir := newFixture(t, fixtureGo)
			path := filepath.Join(dir, dbFile)
			closeWS(t, open(t, dir, embeddedUpTo(t, 13)))
			mustExec(t, inspect(t, path), setup)
			beforeDump := dumpAll(t, inspect(t, path))

			_, err := Open(context.Background(), dir, embeddedUpTo(t, 14))
			if err == nil || !strings.Contains(err.Error(), "0014") {
				t.Fatalf("Open err=%v, want a failure naming 0014", err)
			}
			db := inspect(t, path)
			if uv := qInt(t, db, `PRAGMA user_version`); uv != 13 {
				t.Errorf("user_version=%d, want 13", uv)
			}
			if got := dumpAll(t, db); got != beforeDump {
				t.Errorf("database changed by the refused migration:\n%s\nwant\n%s", got, beforeDump)
			}
		})
	}
}

// A failure after the last statement of 0014 rolls back every table and index it created, and the next
// Open with the correct set succeeds.
func TestCognitionMigrationFailureRollsBack(t *testing.T) {
	body, err := embedded.ReadFile("sql/0014_cognition.sql")
	if err != nil {
		t.Fatal(err)
	}
	fsys := embeddedFSUpTo(t, 13)
	fsys["0014_cognition.sql"] = &fstest.MapFile{Data: append(body, []byte("\nINSERT INTO no_such_table VALUES (1);\n")...)}
	broken, err := Load(fsys)
	if err != nil {
		t.Fatal(err)
	}

	dir := newFixture(t, fixtureGo)
	path := filepath.Join(dir, dbFile)
	closeWS(t, open(t, dir, embeddedUpTo(t, 13)))
	beforeDump := dumpAll(t, inspect(t, path))

	if _, err := Open(context.Background(), dir, broken); err == nil || !strings.Contains(err.Error(), "0014") {
		t.Fatalf("Open with a failing 0014: %v", err)
	}
	db := inspect(t, path)
	if uv := qInt(t, db, `PRAGMA user_version`); uv != 13 {
		t.Errorf("user_version=%d, want 13", uv)
	}
	if got := dumpAll(t, db); got != beforeDump {
		t.Error("a half-applied 0014 left schema or rows behind")
	}
	ws := open(t, dir, embeddedUpTo(t, 14))
	defer closeWS(t, ws)
	if ws.From != 13 || ws.To != 14 {
		t.Errorf("recovery From=%d To=%d", ws.From, ws.To)
	}
}

// seedCognitionCandidate inserts a maintenance session and one candidate before the support migration.
func seedCognitionCandidate(t *testing.T, path string) {
	t.Helper()
	db := inspect(t, path)
	defer db.Close()
	mustExec(t, db, `INSERT INTO sessions(id, created_at, updated_at, status, goal, metadata_json) VALUES('m', 't', 't', 'completed', 'g', '{}')`)
	mustExec(t, db, `INSERT INTO cognition_candidates(candidate_id, schema_version, project_id, maintenance_session_id, generation, revision,
		source_refs_json, source_digest, source_scope, claim_type, subject, subject_key, proposed_claim, rationale, claim_digest,
		cited_paths_json, check_ids_json, policy_version, status, created_at)
		VALUES('kc_v13', 1, '/w', 'm', 1, 1, '["session:s@sha256:x"]', 'sha256:x', 'project', 'fact', 'port', 'port',
		'the port is 8080', 'r', 'sha256:c', '[]', '["port"]', 'p1', 'qualified', 't')`)
}

// 15 -> 14 drops exactly what support added. A candidate written before support reads, after upgrade,
// as one with no assertion, binding or support identity and no purge. Its original row survives a
// downgrade and another upgrade.
func TestCognitionSupportMigrationRoundTrip(t *testing.T) {
	dir := newFixture(t, fixtureGo)
	path := filepath.Join(dir, dbFile)
	closeWS(t, open(t, dir, embeddedUpTo(t, 14)))
	seedCognitionCandidate(t, path)
	v13Schema := normalizedSchema(t, path)
	db := inspect(t, path)
	v13Dump := dumpAll(t, db)
	db.Close()

	set := embeddedUpTo(t, 15)
	ws := open(t, dir, set)
	if ws.From != 14 || ws.To != 15 {
		t.Errorf("upgrade From=%d To=%d", ws.From, ws.To)
	}
	var assertion, binding, support, purged string
	if err := ws.DB.QueryRow(`SELECT assertion_json, binding_digest, support_digest, purged_at FROM cognition_candidates WHERE candidate_id = 'kc_v13'`).
		Scan(&assertion, &binding, &support, &purged); err != nil {
		t.Fatal(err)
	}
	if assertion != "" || binding != "" || support != "" || purged != "" {
		t.Errorf("a pre-support candidate after upgrade: assertion=%q binding=%q support=%q purged=%q", assertion, binding, support, purged)
	}
	closeWS(t, ws)

	from, now, err := Downgrade(context.Background(), dir, set, 14)
	if err != nil || from != 15 || now != 14 {
		t.Fatalf("Downgrade: from=%d now=%d err=%v", from, now, err)
	}
	if got := normalizedSchema(t, path); got != v13Schema {
		t.Errorf("schema after support downgrade differs from the original cognition schema:\n%s\nwant\n%s", got, v13Schema)
	}
	after := inspect(t, path)
	if got := dumpAll(t, after); got != v13Dump {
		t.Errorf("rows changed across support upgrade/downgrade:\n%s\nwant\n%s", got, v13Dump)
	}
	after.Close()
	again := open(t, dir, set)
	defer closeWS(t, again)
	if again.From != 14 || again.To != 15 {
		t.Errorf("re-upgrade From=%d To=%d", again.From, again.To)
	}
}

// An existing support index is not adopted: migration 0015 fails and the v14 database stays unchanged.
func TestCognitionSupportMigrationRefusesExistingObject(t *testing.T) {
	dir := newFixture(t, fixtureGo)
	path := filepath.Join(dir, dbFile)
	closeWS(t, open(t, dir, embeddedUpTo(t, 14)))
	mustExec(t, inspect(t, path), `CREATE INDEX idx_cognition_candidates_support ON sessions(id)`)
	beforeDump := dumpAll(t, inspect(t, path))
	if _, err := Open(context.Background(), dir, embeddedUpTo(t, 15)); err == nil || !strings.Contains(err.Error(), "0015") {
		t.Fatalf("Open err=%v, want a failure naming 0015", err)
	}
	db := inspect(t, path)
	if uv := qInt(t, db, `PRAGMA user_version`); uv != 14 {
		t.Errorf("user_version=%d, want 14", uv)
	}
	if got := dumpAll(t, db); got != beforeDump {
		t.Errorf("database changed by the refused migration:\n%s\nwant\n%s", got, beforeDump)
	}
}

// A failure after the last statement of 0015 rolls back every new column/index; the next Open keeps
// the original cognition rows.
func TestCognitionSupportMigrationFailureRollsBack(t *testing.T) {
	body, err := embedded.ReadFile("sql/0015_cognition_support.sql")
	if err != nil {
		t.Fatal(err)
	}
	fsys := embeddedFSUpTo(t, 14)
	fsys["0015_cognition_support.sql"] = &fstest.MapFile{Data: append(body, []byte("\nINSERT INTO no_such_table VALUES (1);\n")...)}
	broken, err := Load(fsys)
	if err != nil {
		t.Fatal(err)
	}
	dir := newFixture(t, fixtureGo)
	path := filepath.Join(dir, dbFile)
	closeWS(t, open(t, dir, embeddedUpTo(t, 14)))
	seedCognitionCandidate(t, path)
	beforeSchema := normalizedSchema(t, path)
	beforeDump := dumpAll(t, inspect(t, path))
	if _, err := Open(context.Background(), dir, broken); err == nil || !strings.Contains(err.Error(), "0015") {
		t.Fatalf("Open with a failing 0015: %v", err)
	}
	db := inspect(t, path)
	if uv := qInt(t, db, `PRAGMA user_version`); uv != 14 {
		t.Errorf("user_version=%d, want 14", uv)
	}
	if normalizedSchema(t, path) != beforeSchema || dumpAll(t, db) != beforeDump {
		t.Error("a half-applied 0015 left schema or rows behind")
	}
	ws := open(t, dir, embeddedUpTo(t, 15))
	defer closeWS(t, ws)
	if ws.From != 14 || ws.To != 15 {
		t.Errorf("recovery From=%d To=%d", ws.From, ws.To)
	}
}
