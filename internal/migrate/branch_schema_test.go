package migrate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The original Cognitive branch used 13/14/15 before Hub 13 reached main. Preserve those databases
// and require their original binary: they must never be adopted by relabeling user_version.
func TestCanonicalSequenceRejectsHistoricalCognitionBeforeMutation(t *testing.T) {
	for _, version := range []int{13, 14, 15} {
		var schema string
		for v := 13; v <= version; v++ {
			body, err := testdata.ReadFile(fmt.Sprintf("testdata/historical_cognition_v%d.sql", v))
			if err != nil {
				t.Fatal(err)
			}
			schema += string(body) + "\n"
		}
		for _, hot := range []bool{false, true} {
			t.Run(fmt.Sprintf("v%d/hot_%t", version, hot), func(t *testing.T) {
				for _, action := range []string{"same_version_open", "upgrade", "downgrade"} {
					t.Run(action, func(t *testing.T) {
						dir := newFixture(t, fixtureGo)
						closeWS(t, open(t, dir, embeddedUpTo(t, 12)))
						mutation := schema + fmt.Sprintf(`
INSERT INTO maintenance_state(id,project_id,policy_version,updated_at) VALUES(1,'preserve-project','p1','t');
PRAGMA user_version = %d;`, version)
						if hot {
							dir = hotWALCopy(t, dir, mutation)
							if st, err := os.Stat(filepath.Join(dir, dbFile+"-wal")); err != nil || st.Size() <= 32 {
								t.Fatalf("fixture has no committed WAL frames: %v", err)
							}
						} else {
							db := inspect(t, filepath.Join(dir, dbFile))
							mustExec(t, db, mutation)
							db.Close()
						}
						hashes, copies := fileHashes(t, dir, stateFiles...), backups(t, dir)
						var rejected error
						switch action {
						case "same_version_open", "upgrade":
							latest := version
							if action == "upgrade" {
								latest = 16
							}
							ws, err := Open(context.Background(), dir, embeddedUpTo(t, latest))
							rejected = err
							if ws != nil {
								closeWS(t, ws)
							}
						case "downgrade":
							_, _, rejected = Downgrade(context.Background(), dir, embeddedUpTo(t, 16), 12)
						}
						if !errors.Is(rejected, ErrSchemaMismatch) {
							t.Errorf("historical Cognitive schema: got %v, want ErrSchemaMismatch", rejected)
						}
						if got := fileHashes(t, dir, stateFiles...); !reflect.DeepEqual(got, hashes) {
							t.Error("schema rejection changed the historical database or WAL")
						}
						if got := backups(t, dir); !reflect.DeepEqual(got, copies) {
							t.Error("schema rejection created a backup before verifying branch identity")
						}
						if _, err := os.Stat(filepath.Join(dir, pendingMarker)); !os.IsNotExist(err) {
							t.Errorf("schema rejection created a checkpoint marker: %v", err)
						}
					})
				}
			})
		}
	}
}

func TestCanonicalSequenceRejectsIncompleteCurrentSchema(t *testing.T) {
	for _, mutation := range []string{
		`ALTER TABLE cognition_candidates DROP COLUMN purged_at`,
		`ALTER TABLE eval_results DROP COLUMN cognition_epoch`,
		`DROP TABLE hub_commands`,
		`ALTER TABLE worker_messages DROP COLUMN attempts`,
	} {
		t.Run(mutation, func(t *testing.T) {
			dir := newFixture(t, fixtureGo)
			closeWS(t, open(t, dir, embeddedUpTo(t, 16)))
			dir = hotWALCopy(t, dir, mutation)
			hashes := fileHashes(t, dir, stateFiles...)
			ws, err := Open(context.Background(), dir, embeddedUpTo(t, 16))
			if ws != nil {
				closeWS(t, ws)
			}
			if !errors.Is(err, ErrSchemaMismatch) {
				t.Errorf("current version hid incomplete schema: %v", err)
			}
			if got := fileHashes(t, dir, stateFiles...); !reflect.DeepEqual(got, hashes) {
				t.Error("schema rejection mutated an incomplete database")
			}
		})
	}
}

// Real mainline Hub rows survive cognition 14/15/16, an empty-graph rollback to main13 and re-upgrade.
// A migration failure and the scrubbed pre-upgrade backup both remain recoverable.
func TestCanonicalSequencePreservesHubAndRecoversBackup(t *testing.T) {
	ctx := context.Background()
	dir := newFixture(t, fixtureGo)
	path := filepath.Join(dir, dbFile)
	closeWS(t, open(t, dir, embeddedUpTo(t, 13)))
	db := inspect(t, path)
	mustExec(t, db, `INSERT INTO steer_messages(session_id,run_id,command_id,body,created_at) VALUES('s_done','run','command','keep instruction','t')`)
	mustExec(t, db, `INSERT INTO work_unit_leases(root_id,unit_id,holder,lease_id,expires_at,updated_at) VALUES('s_done','unit','holder','lease','2099-01-01T00:00:00Z','t')`)
	mustExec(t, db, `INSERT INTO hub_commands(command_id,session_id,verb,result,created_at) VALUES('command','s_done','steer','{}','t')`)
	before := dumpAll(t, db)
	db.Close()
	closeWS(t, open(t, dir, embeddedUpTo(t, 16)))
	var backupPath string
	for _, name := range backups(t, dir) {
		if len(name) >= len("lumencortex.db.v13.") && name[:len("lumencortex.db.v13.")] == "lumencortex.db.v13." {
			backupPath = filepath.Join(dir, "backups", name)
		}
	}
	if backupPath == "" {
		t.Fatal("upgrade did not retain the original main13 backup")
	}
	if _, _, err := Downgrade(ctx, dir, embeddedUpTo(t, 16), 13); err != nil {
		t.Fatal(err)
	}
	if got := dumpAll(t, inspect(t, path)); got != before {
		t.Fatal("empty cognition downgrade changed original Hub/session rows")
	}
	closeWS(t, open(t, dir, embeddedUpTo(t, 16)))

	restored := t.TempDir()
	body, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(restored, dbFile), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := dumpAll(t, inspect(t, filepath.Join(restored, dbFile))); got != before {
		t.Fatal("restored pre-upgrade backup changed original rows")
	}
	closeWS(t, open(t, restored, embeddedUpTo(t, 16)))
}
