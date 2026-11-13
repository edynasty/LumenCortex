package runtime

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/internal/knowledge"
	"github.com/edynasty/LumenCortex/internal/session"
)

// The identities the recovery depends on, at the store: a processing record whose maintenance session
// is fenced no longer stands and resolves to its redo key; a skip and a standing record resolve to
// themselves.
func TestFableProcessingKeysFollowFencedPasses(t *testing.T) {
	ctx := context.Background()
	f := newCognitionFixture(t, true)
	g := f.run(t, "find the build command", cogAnswer("make build"))
	h := f.run(t, "find the service port", cogAnswer("8080"))
	f.propose(func(refs []string) []string {
		return []string{candidateJSON("procedure", "build", "Build with make build.", refOf(t, refs, g), nil)}
	})
	batch := f.maintain(t)
	version, err := f.engine.store.SourceVersion(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	key := knowledge.DedupeKey(knowledge.SourceRef{Kind: knowledge.KindSession, ID: h, Digest: version}, session.SourceScopeProject)
	resolved, processed, err := f.engine.store.ProcessingKeys(ctx, []string{key, knowledge.Digest("unknown")})
	if err != nil || resolved[0] != key || !processed[key] || resolved[1] != knowledge.Digest("unknown") || processed[resolved[1]] {
		t.Fatalf("a standing record: %v %v %v", resolved, processed, err)
	}
	if _, err := f.engine.RevokeSource(ctx, SourceSession, g, "user_withdrawal"); err != nil {
		t.Fatal(err)
	}
	redo := knowledge.RedoKey(key, batch.MaintenanceSessionID)
	resolved, processed, err = f.engine.store.ProcessingKeys(ctx, []string{key})
	if err != nil || resolved[0] != redo || processed[redo] || redo == key || !strings.HasPrefix(redo, "sha256:") || len(redo) != len(key) {
		t.Fatalf("a record of a fenced pass: %v %v %v (redo %s)", resolved, processed, err, redo)
	}
	if n := countRows(t, f.engine, `SELECT count(*) FROM maintenance_dedupe WHERE dedupe_key = ? AND maintenance_session_id = ?`, key, batch.MaintenanceSessionID); n != 1 {
		t.Fatalf("the earlier record is gone (%d)", n)
	}
	f.answer = func([]string) string { return `{"candidates":[]}` }
	if r := f.maintain(t); r.Sources != 1 {
		t.Fatalf("the recovery pass %+v", r)
	}
	resolved, processed, err = f.engine.store.ProcessingKeys(ctx, []string{key})
	if err != nil || resolved[0] != redo || !processed[redo] {
		t.Fatalf("after the recovery: %v %v %v", resolved, processed, err)
	}
	if got := fmt.Sprint(f.dedupeRows(t, h)); strings.Count(got, "@") != 2 {
		t.Fatalf("processing records %s", got)
	}
}
