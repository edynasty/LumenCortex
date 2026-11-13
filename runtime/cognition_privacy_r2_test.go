package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/knowledge"
	"github.com/edynasty/LumenCortex/protocol"
)

func TestCognitionPrivacyStopsLateMaintainerWrites(t *testing.T) {
	f := newCognitionFixture(t, true)
	const sentinel = "T5A-R2-LATE-MAINTAINER-f14d9732"
	a := f.run(t, "private source", cogAnswer(sentinel))
	f.answer = func(refs []string) string {
		return candidates(candidateJSON("procedure", "private note", sentinel, refOf(t, refs, a), nil))
	}
	entered, release := make(chan struct{}), make(chan struct{})
	if err := f.engine.RegisterLifecycleHook(&platformHook{id: "hold-final", events: []LifecycleEvent{LifecycleBeforeDelivery},
		fn: func(context.Context, LifecycleRequest) (LifecycleResult, error) {
			close(entered)
			<-release
			return LifecycleResult{Decision: HookContinue}, nil
		}}); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		report MaintenanceReport
		err    error
	}
	finished := make(chan outcome, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	go func() { r, err := f.engine.MaintainCognition(ctx); finished <- outcome{r, err} }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("maintainer did not reach final delivery boundary")
	}
	deleted := make(chan error, 1)
	go func() { _, err := f.engine.DeleteSession(ctx, a, WorktreeCleanup{}); deleted <- err }()
	// The old implementation returns success while the writer is still stopped
	// immediately before AppendStep. The fixed implementation cancels and waits.
	var deletionErr error
	returnedEarly := false
	select {
	case deletionErr = <-deleted:
		returnedEarly = true
	case <-time.After(500 * time.Millisecond):
	}
	close(release)
	var got outcome
	select {
	case got = <-finished:
	case <-ctx.Done():
		t.Fatal("maintainer did not finish after privacy cancellation")
	}
	if !returnedEarly {
		select {
		case deletionErr = <-deleted:
		case <-ctx.Done():
			t.Fatal("privacy waited indefinitely for writer")
		}
	}
	if deletionErr != nil {
		t.Fatal(deletionErr)
	}
	if got.err == nil {
		t.Fatalf("maintenance committed after source deletion: %+v", got.report)
	}
	if found := textAtRest(t, f.engine.store.Path(), sentinel); len(found) != 0 {
		t.Fatalf("successful deletion was followed by private writes in %v", found)
	}
	if n := countRows(t, f.engine, `SELECT count(*) FROM agent_steps WHERE session_id = ? AND instr(json,?) > 0`, got.report.MaintenanceSessionID, sentinel); n != 0 {
		t.Fatalf("%d late private steps", n)
	}
	if m, err := f.engine.store.Get(context.Background(), got.report.MaintenanceSessionID); err != nil || (m.Final != nil && strings.Contains(*m.Final, sentinel)) {
		t.Fatalf("late final: %+v %v", m, err)
	}
	t.Logf("maintenance terminated before purge finished; pre-fix early-return=%v", returnedEarly)
}

func TestCognitionPrivacyPurgesInheritedCitedPaths(t *testing.T) {
	f := newCognitionFixture(t, true)
	const sentinel = "PRIVATE-PATH-r2-6f018372"
	path := sentinel + ".txt"
	if err := os.WriteFile(filepath.Join(f.root, path), []byte("8080"), 0600); err != nil {
		t.Fatal(err)
	}
	a := f.run(t, "inspect service", cogAnswer("8080"))
	f.propose(func(refs []string) []string {
		entry, err := json.Marshal(map[string]any{"claimType": "fact", "subject": "service port", "claim": "8080", "rationale": "source", "sourceRefs": []string{refOf(t, refs, a)}, "citedPaths": []string{path}, "assertion": portFact("8080")})
		if err != nil {
			t.Fatal(err)
		}
		return []string{string(entry)}
	})
	r := f.maintain(t)
	if r.Qualified != 1 {
		t.Fatalf("adoption: %+v", r)
	}
	p := scripted(readStep("read", r.Inserted[0]))
	readerID := f.run(t, "use service knowledge", p)
	if view := viewIn(t, p.request(t, 1), "read"); view.Applicability != knowledge.Current {
		t.Fatalf("reader: %+v", view)
	}
	if n := countRows(t, f.engine, `SELECT count(*) FROM session_messages WHERE session_id=? AND role='user' AND json_extract(json,'$.origin')='knowledge' AND instr(json,?)>0`, readerID, sentinel); n != 1 {
		t.Fatalf("fixture missing inherited private path: %d", n)
	}
	if _, err := f.engine.DeleteSession(context.Background(), a, WorktreeCleanup{}); err != nil {
		t.Fatal(err)
	}
	if found := textAtRest(t, f.engine.store.Path(), sentinel); len(found) != 0 {
		t.Fatalf("private cited path survived in %v", found)
	}
}

func TestCognitionPrivacyPurgesMaintainerEventArguments(t *testing.T) {
	f := newCognitionFixture(t, true)
	const sentinel = "T5A-R2-PRIVATE-EVENT-c49063"
	a := f.run(t, "private source", cogAnswer(sentinel))
	f.maintainer.respond = func(n int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		if n == 0 {
			return cogCall("read_thread", `{"id":"`+sentinel+`"}`), nil
		}
		return cogFinal(`{"candidates":[]}`), nil
	}
	r := f.maintain(t)
	if _, err := f.engine.DeleteSession(context.Background(), a, WorktreeCleanup{}); err != nil {
		t.Fatal(err)
	}
	if found := textAtRest(t, f.engine.store.Path(), sentinel); len(found) != 0 {
		t.Fatalf("maintainer tool arguments survived in %v (maintenance %s)", found, r.MaintenanceSessionID)
	}
}
