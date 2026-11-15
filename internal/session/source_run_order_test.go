package session

import (
	"context"
	"testing"
)

func sourceRun(t *testing.T, store *Store, id string) string {
	t.Helper()
	var run string
	if err := store.db.QueryRow(`SELECT `+sourceRunSQL+` FROM sessions s WHERE s.id=?`, id).Scan(&run); err != nil {
		t.Fatal(err)
	}
	return run
}

// ICRA-V-001. The run a source names is the newest started run. BeginRun commits agent.start strictly
// after every main request of the previous run, so its row order decides; wall-clock text does not.
// RFC3339Nano drops trailing zeros (".12345Z" sorts after ".123456Z") and the clock can step back, so
// comparing created_at with at as strings attributed a run that failed before its first main request
// to the previous run.
func TestSourceRunPrefersStartedRunOverWallClockText(t *testing.T) {
	ctx := context.Background()
	for name, times := range map[string][2]string{
		"trimmed fraction":   {"2026-11-15T00:00:00.12345Z", "2026-11-15T00:00:00.123456Z"},
		"clock stepped back": {"2026-11-15T00:00:01Z", "2026-11-15T00:00:00.5Z"},
		"same tick":          {"2026-11-15T00:00:00.5Z", "2026-11-15T00:00:00.5Z"},
	} {
		t.Run(name, func(t *testing.T) {
			store := gapStore(t)
			gapSession(t, store, "s", "completed", "")
			if got := sourceRun(t, store, "s"); got != "" {
				t.Fatalf("source run of a session that never ran = %q", got)
			}
			// A main request without agent.start (a run committed before BeginRun recorded it) still names its run.
			reach(t, store, "s", "r1", PurposeMain, RequestAnswered)
			if got := sourceRun(t, store, "s"); got != "run1" {
				t.Fatalf("legacy main request: source run = %q, want run1", got)
			}
			if _, _, err := store.BeginRun(ctx, "s", RunStart{RunID: "run2"}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.Exec(`UPDATE request_envelopes SET created_at=? WHERE request_id='r1'`, times[0]); err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.Exec(`UPDATE session_checkpoints SET at=? WHERE session_id='s' AND reason='agent.start'`, times[1]); err != nil {
				t.Fatal(err)
			}
			if got := sourceRun(t, store, "s"); got != "run2" {
				t.Fatalf("run2 failed before its first main request: source run = %q, want run2", got)
			}
			a := mainAdmission("s", "r2")
			a.RunID = "run2"
			admit(t, store, a)
			if got := sourceRun(t, store, "s"); got != "run2" {
				t.Fatalf("after run2's main request: source run = %q, want run2", got)
			}
		})
	}
}
