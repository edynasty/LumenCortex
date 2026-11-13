package runtime

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/knowledge"
)

// Manual maintenance owns its verification and failure settlement, not just the
// model's tracked RunAgent call. Close must cancel and await that entire pass.
func TestCognitionCloseWaitsForManualVerification(t *testing.T) {
	url, entered, release := freshnessEpisodeGate(t)
	f := newCognitionFixture(t, true)
	checks := fmt.Sprintf(`{"port":{"kind":"test","command":%q,"required":false}}`, "curl --noproxy '*' -fsS '"+url+"'")
	f.setChecks(t, checks, map[string]knowledge.Assertion{"port": portFact("8080")})
	source := f.run(t, "record the service port", cogAnswer("8080"))
	f.propose(func(refs []string) []string {
		return []string{factJSON("service port", []string{refOf(t, refs, source)}, nil, portFact("8080"))}
	})
	done := make(chan error, 1)
	go func() {
		_, err := f.engine.MaintainCognition(context.Background())
		done <- err
	}()
	defer func() {
		close(release)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("maintenance did not settle after gate release")
		}
	}()
	select {
	case <-entered:
	case err := <-done:
		done <- err
		t.Fatalf("pass ended before verification: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("verification did not enter its check")
	}
	if err := f.engine.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		done <- err
		if err == nil {
			t.Fatal("shutdown committed cancelled maintenance")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close returned while manual maintenance verification was still running")
	}
	if _, err := f.engine.MaintainCognition(context.Background()); err == nil {
		t.Fatal("closed Engine admitted a new maintenance pass")
	}
	reopened, err := Open(Options{Workspace: f.root})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	st, err := reopened.CognitionState(context.Background())
	if err != nil || st.LeaseID != "" || st.Cursor != 0 || st.Dirty == 0 {
		t.Fatalf("shutdown lost failure settlement: %+v %v", st, err)
	}
}
