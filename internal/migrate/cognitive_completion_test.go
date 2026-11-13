package migrate

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"testing/fstest"
)

func TestCognitiveCompletionMigrationRoundTripAndFailure(t *testing.T) {
	dir := newFixture(t, fixtureGo)
	path := filepath.Join(dir, dbFile)
	closeWS(t, open(t, dir, embeddedUpTo(t, 16)))
	schema, before := normalizedSchema(t, path), dumpAll(t, inspect(t, path))
	body, err := embedded.ReadFile("sql/0017_cognitive_completion.sql")
	if err != nil {
		t.Fatal(err)
	}
	fsys := embeddedFSUpTo(t, 16)
	fsys["0017_cognitive_completion.sql"] = &fstest.MapFile{Data: append(body, []byte("\nINSERT INTO no_such_table VALUES(1);")...)}
	broken, err := Load(fsys)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), dir, broken); err == nil {
		t.Fatal("partial completion migration accepted")
	}
	if normalizedSchema(t, path) != schema || dumpAll(t, inspect(t, path)) != before {
		t.Fatal("failed migration changed existing state")
	}
	closeWS(t, open(t, dir, embeddedUpTo(t, 17)))
	if _, _, err := Downgrade(context.Background(), dir, embeddedUpTo(t, 17), 16); err != nil {
		t.Fatal(err)
	}
	if normalizedSchema(t, path) != schema || dumpAll(t, inspect(t, path)) != before {
		t.Fatal("empty roundtrip lost existing state")
	}
	closeWS(t, open(t, dir, embeddedUpTo(t, 17)))
}

func TestCognitiveCompletionDowngradeRefusesNewSemantics(t *testing.T) {
	for _, setup := range []string{
		`INSERT INTO cognition_applications(id,experience_id,adoption_id,session_id,run_id,state,created_at,json) SELECT 'a','e','u',id,'run','pending','t','{}' FROM sessions LIMIT 1`,
		`INSERT INTO cognition_nodes(id,version,session_id,project_id,kind,created_at,json) SELECT 'n',1,id,'p','belief','t','{"dependencies":[{"id":"d","version":1}]}' FROM sessions LIMIT 1`,
		`INSERT INTO cognition_access(node_id,session_id,access_count,last_access_at) SELECT 'n',id,1,'t' FROM sessions LIMIT 1`,
	} {
		t.Run(setup[:35], func(t *testing.T) {
			dir := newFixture(t, fixtureGo)
			path := filepath.Join(dir, dbFile)
			closeWS(t, open(t, dir, embeddedUpTo(t, 17)))
			db := inspect(t, path)
			mustExec(t, db, setup)
			before := dumpAll(t, db)
			db.Close()
			if _, _, err := Downgrade(context.Background(), dir, embeddedUpTo(t, 17), 16); err == nil {
				t.Fatal("destructive downgrade accepted")
			}
			if dumpAll(t, inspect(t, path)) != before {
				t.Fatal("refused downgrade changed data")
			}
		})
	}
}

func TestCognitiveCompletionMissingTableRefusedWithoutMutation(t *testing.T) {
	for _, table := range []string{"cognition_applications", "cognition_proposals", "cognition_decisions", "cognition_access"} {
		t.Run(table, func(t *testing.T) {
			dir := newFixture(t, fixtureGo)
			set := embeddedUpTo(t, 17)
			closeWS(t, open(t, dir, set))
			dir = hotWALCopy(t, dir, "DROP TABLE "+table)
			before := fileHashes(t, dir, stateFiles...)
			ws, err := Open(context.Background(), dir, set)
			if ws != nil {
				closeWS(t, ws)
			}
			if !errors.Is(err, ErrSchemaMismatch) {
				t.Fatalf("partial schema opened: %v", err)
			}
			if !reflect.DeepEqual(fileHashes(t, dir, stateFiles...), before) {
				t.Fatal("refused schema changed data")
			}
		})
	}
}
