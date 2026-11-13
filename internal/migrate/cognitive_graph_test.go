package migrate

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func TestCognitiveGraphMigrationRoundTrip(t *testing.T) {
	dir := newFixture(t, fixtureGo)
	path := filepath.Join(dir, dbFile)
	closeWS(t, open(t, dir, embeddedUpTo(t, 15)))
	beforeSchema := normalizedSchema(t, path)
	before := dumpAll(t, inspect(t, path))
	closeWS(t, open(t, dir, embeddedUpTo(t, 16)))
	if _, err := Open(context.Background(), dir, embeddedUpTo(t, 15)); !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("old Go accepted new schema: %v", err)
	}
	if _, _, err := Downgrade(context.Background(), dir, embeddedUpTo(t, 16), 15); err != nil {
		t.Fatal(err)
	}
	if normalizedSchema(t, path) != beforeSchema || dumpAll(t, inspect(t, path)) != before {
		t.Fatal("empty graph downgrade lost existing state")
	}
	closeWS(t, open(t, dir, embeddedUpTo(t, 16)))
}

func TestCognitiveGraphMigrationFailureRollsBack(t *testing.T) {
	dir := newFixture(t, fixtureGo)
	path := filepath.Join(dir, dbFile)
	closeWS(t, open(t, dir, embeddedUpTo(t, 15)))
	beforeSchema, before := normalizedSchema(t, path), dumpAll(t, inspect(t, path))
	body, err := embedded.ReadFile("sql/0016_cognitive_graph.sql")
	if err != nil {
		t.Fatal(err)
	}
	fsys := embeddedFSUpTo(t, 15)
	fsys["0016_cognitive_graph.sql"] = &fstest.MapFile{Data: append(body, []byte("\nINSERT INTO no_such_table VALUES(1);")...)}
	broken, err := Load(fsys)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), dir, broken); err == nil || !strings.Contains(err.Error(), "0016") {
		t.Fatal(err)
	}
	if normalizedSchema(t, path) != beforeSchema || dumpAll(t, inspect(t, path)) != before {
		t.Fatal("partial migration survived")
	}
	closeWS(t, open(t, dir, embeddedUpTo(t, 16)))
}

func TestCognitiveGraphDowngradeRefusesKnowledgeAndPrivacyLoss(t *testing.T) {
	for _, setup := range []string{
		`INSERT INTO cognition_nodes(id,version,session_id,project_id,kind,created_at,json) SELECT 'n',1,id,'project','belief','t','{}' FROM sessions LIMIT 1`,
		`INSERT INTO source_fences(scope_kind,scope_ref,epoch,reason,state,tombstone_json,created_at,updated_at) VALUES('privacy','deleted',1,'privacy_delete','cleaned','{}','t','t')`,
	} {
		t.Run(setup[:20], func(t *testing.T) {
			dir := newFixture(t, fixtureGo)
			path := filepath.Join(dir, dbFile)
			closeWS(t, open(t, dir, embeddedUpTo(t, 16)))
			db := inspect(t, path)
			mustExec(t, db, setup)
			before := dumpAll(t, db)
			db.Close()
			if _, _, err := Downgrade(context.Background(), dir, embeddedUpTo(t, 16), 15); err == nil {
				t.Fatal("destructive downgrade accepted")
			}
			if dumpAll(t, inspect(t, path)) != before {
				t.Fatal("refused downgrade changed content")
			}
		})
	}
}

func TestCognitiveGraphRefusesSchemaCollision(t *testing.T) {
	dir := newFixture(t, fixtureGo)
	path := filepath.Join(dir, dbFile)
	closeWS(t, open(t, dir, embeddedUpTo(t, 15)))
	db := inspect(t, path)
	mustExec(t, db, `CREATE TABLE cognition_nodes(precious TEXT); INSERT INTO cognition_nodes VALUES('keep')`)
	before := dumpAll(t, db)
	db.Close()
	if _, err := Open(context.Background(), dir, embeddedUpTo(t, 16)); err == nil {
		t.Fatal("foreign table adopted")
	}
	if dumpAll(t, inspect(t, path)) != before {
		t.Fatal("collision damaged old state")
	}
}
