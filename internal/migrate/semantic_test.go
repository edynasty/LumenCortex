package migrate

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSemanticUpgradePreservesResultsAndRefusesLossyDowngrade(t *testing.T) {
	dir := newFixture(t, fixtureGo)
	ws := open(t, dir, embeddedUpTo(t, 9))
	mustExec(t, ws.DB, `INSERT INTO eval_results
		SELECT id, 1, 'unit', 'answer', 'test', 'failed', 'runtime', 'fp', '["artifact"]', 't',
		'episode', 'before', 'after', 'spec', 3, 'closed' FROM sessions LIMIT 1`)
	const rowsSQL = `SELECT json_array(session_id,seq,work_unit_id,check_id,kind,status,provenance,
		fingerprint,artifacts_json,at,episode_id,pre_fingerprint,post_fingerprint,spec_digest,
		approval_epoch,episode_status) FROM eval_results ORDER BY session_id,seq`
	before := qStrings(t, ws.DB, rowsSQL)
	closeWS(t, ws)
	ws = open(t, dir, embeddedUpTo(t, 10))
	if ws.From != 9 || ws.To != 10 {
		t.Fatalf("migration %d -> %d", ws.From, ws.To)
	}
	if after := qStrings(t, ws.DB, rowsSQL); !reflect.DeepEqual(before, after) {
		t.Fatalf("historical result changed: before=%v after=%v", before, after)
	}
	for _, index := range []string{"idx_eval_results_check", "idx_eval_results_episode"} {
		if qInt(t, ws.DB, `SELECT count(*) FROM sqlite_master WHERE type='index' AND name=?`, index) != 1 {
			t.Fatalf("missing lookup index %s", index)
		}
	}
	const insert = `INSERT INTO eval_results(session_id,seq,check_id,kind,status,provenance,fingerprint,artifacts_json,at)
		SELECT session_id,2,'semantic-answer',?,'passed','runtime','fp','[]','t' FROM eval_results LIMIT 1`
	if _, err := ws.DB.Exec(insert, "not-a-kind"); err == nil {
		t.Fatal("invalid check kind was accepted")
	}
	mustExec(t, ws.DB, insert, "semantic")
	expected := qStrings(t, ws.DB, rowsSQL)
	closeWS(t, ws)
	from, now, err := Downgrade(context.Background(), dir, embeddedUpTo(t, 10), 9)
	if err == nil || from != 10 || now != 10 {
		t.Fatalf("lossy downgrade: from=%d now=%d err=%v", from, now, err)
	}
	ws = open(t, dir, embeddedUpTo(t, 10))
	if after := qStrings(t, ws.DB, rowsSQL); !reflect.DeepEqual(expected, after) {
		t.Fatalf("failed downgrade lost evidence: %v", after)
	}
	if got := qStrings(t, ws.DB, `PRAGMA integrity_check`); !reflect.DeepEqual(got, []string{"ok"}) {
		t.Fatalf("integrity: %v", got)
	}
	if n := qInt(t, ws.DB, `SELECT count(*) FROM pragma_foreign_key_check`); n != 0 {
		t.Fatalf("foreign key violations: %d", n)
	}
	// Session deletion must retain the original cascade ownership of results.
	mustExec(t, ws.DB, `DELETE FROM sessions WHERE id IN (SELECT session_id FROM eval_results)`)
	if qInt(t, ws.DB, `SELECT count(*) FROM eval_results`) != 0 {
		t.Fatal("result rows survived deletion of their owning session")
	}
	closeWS(t, ws)
	_, now, err = Downgrade(context.Background(), dir, embeddedUpTo(t, 10), 9)
	if err != nil || now != 9 {
		t.Fatalf("representable downgrade: now=%d err=%v", now, err)
	}
	if uv := qInt(t, inspect(t, filepath.Join(dir, dbFile)), `PRAGMA user_version`); uv != 9 {
		t.Fatalf("downgraded version=%d", uv)
	}
}
