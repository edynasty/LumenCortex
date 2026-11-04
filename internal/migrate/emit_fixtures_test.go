package migrate

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestEmitVersionFixtures writes one legacy workspace per schema version 1..5 for the cross-binary
// migration round trip (T1-S7 item 6, docs/reports/cognition/t1-s7-migration-roundtrip.sh):
// $LCX_EMIT_FIXTURES/v<n>/.lumencortex/lumencortex.db holds the Go fixture's sessions and rows,
// migrated to version n with the numbered migrations as they shipped, and no workspace binding (an
// early database). It is tooling, not an assertion: without LCX_EMIT_FIXTURES it skips.
func TestEmitVersionFixtures(t *testing.T) {
	out := os.Getenv("LCX_EMIT_FIXTURES")
	if out == "" {
		t.Skip("tooling only: set LCX_EMIT_FIXTURES to a directory")
	}
	for from := 1; from <= 5; from++ {
		dir := newFixture(t, fixtureGo)
		closeWS(t, open(t, dir, embeddedUpTo(t, from)))
		raw, err := os.ReadFile(filepath.Join(dir, dbFile))
		if err != nil {
			t.Fatal(err)
		}
		state := filepath.Join(out, fmt.Sprintf("v%d", from), ".lumencortex")
		if err := os.MkdirAll(state, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(state, dbFile), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
