package runtime

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/evaluator"
	"github.com/edynasty/LumenCortex/internal/knowledge"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/protocol"
)

// FABLE-04 (freshness x concurrent runs). The bound check reads a dependency outside the workspace and
// outside the process environment, so nothing Core fingerprints moves when it changes. Run X establishes
// its own PASS; while X is still running the dependency changes and run Y starts. Y never reuses X's
// PASS: it runs the check itself and reads the candidate as stale. X's older PASS no longer makes the
// candidate current for X either: a newer result of the same check at the same workspace state says
// otherwise, and Core holds it. Every observation stays, appended.
func TestFableConcurrentRunsDoNotOutliveANewerFailure(t *testing.T) {
	ctx := context.Background()
	f := newCognitionFixture(t, true)
	external := filepath.Join(t.TempDir(), "port")
	writeFile(t, external, "8080\n")
	checks := fmt.Sprintf(`{"port": {"kind": "test", "command": %q, "required": false}}`, ". ./config.env && test \"$PORT\" = 8080 && test \"$(cat "+quoteCheckPath(external)+")\" = 8080")
	f.setChecks(t, checks, map[string]knowledge.Assertion{"port": portFact("8080")})
	a := f.run(t, "inspect the external configuration", cogAnswer("the service listens on 8080"))
	report := f.adoptFacts(t, a, portFact("8080"))
	if report.Qualified != 1 || len(report.Inserted) != 1 {
		t.Fatalf("adoption %+v", report)
	}
	id := report.Inserted[0]
	f.answer = func([]string) string { return `{"candidates":[]}` }

	held, release := make(chan struct{}), make(chan struct{})
	x := &cognitionProvider{respond: func(n int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		switch n {
		case 0:
			return cogStep("r1", "read_knowledge", `{"ref":"`+id+`"}`), nil
		case 1:
			close(held)
			<-release
			return cogStep("r2", "read_knowledge", `{"ref":"`+id+`"}`), nil
		}
		return cogFinal("done"), nil
	}}
	handle, err := f.engine.NewSession(ctx, SessionOptions{Goal: "use the service port for a while"})
	if err != nil {
		t.Fatal(err)
	}
	xDone := make(chan error, 1)
	go func() {
		_, err := f.engine.runAgentWith(ctx, handle.ID, x, AgentOptions{MaxSteps: 6, Policy: "workspace"})
		xDone <- err
	}()
	select {
	case <-held:
	case err := <-xDone:
		t.Fatalf("run X ended before its second step: %v", err)
	case <-time.After(60 * time.Second):
		t.Fatal("run X did not reach its second step")
	}
	if app := startApplicability(x.request(t, 0), id); app != knowledge.Current {
		t.Fatalf("X's index %s: %q", app, indexRow(x.request(t, 0)))
	}
	if v := viewIn(t, x.request(t, 1), "r1"); v.Applicability != knowledge.Current {
		t.Fatalf("X's first read %+v", v)
	}
	fingerprint := f.observations(t, id)[0].Fingerprint

	// The dependency changes while X runs. No tracked, untracked or ignored file and no environment
	// variable changed.
	writeFile(t, external, "9090\n")
	y := scripted(readStep("r1", id))
	ySession := f.run(t, "use the service port after the change", y)
	if app := startApplicability(y.request(t, 0), id); app != knowledge.Stale {
		t.Fatalf("Y's index %s: Y reused the PASS of the concurrent run X (%q)", app, indexRow(y.request(t, 0)))
	}
	if v := viewIn(t, y.request(t, 1), "r1"); v.Applicability != knowledge.Stale {
		t.Fatalf("Y's read %+v", v)
	}
	obs := f.observations(t, id) // newest first
	if len(obs) != 3 || obs[0].SessionID != ySession || obs[0].Outcome != evaluator.StatusFailed || obs[0].Fingerprint != fingerprint ||
		obs[1].SessionID != handle.ID || obs[1].Outcome != evaluator.StatusPassed || obs[1].Purpose != session.ObservationRevalidation ||
		obs[2].Purpose != session.ObservationAdoption {
		t.Fatalf("observations after Y %+v", obs)
	}

	close(release)
	if err := <-xDone; err != nil {
		t.Fatal(err)
	}
	second := viewIn(t, x.request(t, 2), "r2")
	if second.Applicability != knowledge.Unknown {
		t.Fatalf("X still reads its older PASS as %s after a newer failure of the same check at the same workspace state", second.Applicability)
	}
	if len(second.Checks) != 1 || len(second.Checks[0].Observations) != 3 || second.Checks[0].Observations[0].Outcome != evaluator.StatusFailed {
		t.Fatalf("X's second read does not show the history %+v", second.Checks)
	}
	for _, o := range second.Checks[0].Observations {
		if o.Current {
			t.Fatalf("an observation is marked fresh for X after the newer failure: %+v", second.Checks[0].Observations)
		}
	}
	if n := len(f.observations(t, id)); n != 3 {
		t.Fatalf("a read appended an observation: %d", n)
	}
	if v := f.hostView(t, id); v.Applicability != knowledge.Unknown {
		t.Fatalf("host view %+v", v)
	}

	// The dependency is restored: the next run establishes its own PASS again, and the earlier failure
	// of another run does not hold it back.
	writeFile(t, external, "8080\n")
	z := scripted(readStep("r1", id))
	f.run(t, "use the service port after the restore", z)
	if app := startApplicability(z.request(t, 0), id); app != knowledge.Current {
		t.Fatalf("after the restore the index says %s", app)
	}
	if v := viewIn(t, z.request(t, 1), "r1"); v.Applicability != knowledge.Current {
		t.Fatalf("after the restore %+v", v)
	}
}

// FABLE-03 (privacy deletion x inheritance): what a deletion guarantees about a reader that restated,
// in its own answer, a candidate derived from the deleted session. The copies Core made are removed
// (TestIndependentPrivacyDeletion); the reader's own model output is not rewritten, and from the
// deletion on no path of Core gives it to a model again: the reader is withdrawn as a source, no
// maintenance pass reads it, read_thread and recall return none of it, the thread cannot be continued,
// and a host page marks every row withheld.
func TestFableRestatingReaderIsIsolatedByPrivacyDeletion(t *testing.T) {
	ctx := context.Background()
	f := newCognitionFixture(t, true)
	sentinel := fmt.Sprintf("FABLE-R1-PRIVATE-%d", time.Now().UnixNano())
	a := f.run(t, "investigate the service", cogAnswer("The service listens on 8080. Its codename is "+sentinel+"."))
	f.propose(func(refs []string) []string {
		return []string{candidateJSON("fact", "codename", "The codename is "+sentinel+".", refOf(t, refs, a), nil)}
	})
	report := f.maintain(t)
	if len(report.Inserted) != 1 {
		t.Fatalf("fixture %+v", report)
	}
	f.answer = func([]string) string { return `{"candidates":[]}` }
	p := &cognitionProvider{respond: func(n int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		if n == 0 {
			return cogStep("r1", "read_knowledge", `{"ref":"`+report.Inserted[0]+`"}`), nil
		}
		return cogFinal("Relying on project cognition: the codename is " + sentinel + "."), nil
	}}
	r := f.run(t, "use the project's cognition", p)
	if !strings.Contains(lastToolResult(p.request(t, 1), "r1"), sentinel) {
		t.Fatal("fixture: the reader did not receive the candidate's text")
	}
	if second := f.maintain(t); second.Sources != 1 {
		t.Fatalf("fixture: the reader was not maintained %+v", second)
	}
	maintained := len(f.batchInputs())

	if _, err := f.engine.DeleteSession(ctx, a, WorktreeCleanup{}); err != nil {
		t.Fatal(err)
	}
	if pending, err := f.engine.store.PendingPrivacyDeletion(ctx, a); err != nil || pending != nil {
		t.Fatalf("deletion pending %+v %v", pending, err)
	}
	if withdrawn, err := f.engine.store.SessionWithdrawn(ctx, r); err != nil || !withdrawn {
		t.Fatalf("the reader is still a source: %v %v", withdrawn, err)
	}
	// The copy Core made is gone from the reader's transcript.
	if n := countRows(t, f.engine, `SELECT count(*) FROM session_messages WHERE session_id = ? AND role = 'tool' AND json LIKE ?`, r, "%"+sentinel+"%"); n != 0 {
		t.Fatalf("%d tool rows of the reader still hold the candidate's text", n)
	}
	// A host page marks every row of the reader withheld.
	page, err := f.engine.MessagePage(ctx, r, -1, 100)
	if err != nil || len(page.Messages) == 0 {
		t.Fatalf("host page %+v %v", page, err)
	}
	for _, m := range page.Messages {
		if !m.Withheld {
			t.Fatalf("row %d of the exposed reader is not marked withheld", m.Seq)
		}
	}
	if view, err := f.engine.ReadThread(ctx, r); err != nil || !view.Withdrawn || strings.Contains(fmt.Sprintf("%+v", view), sentinel) {
		t.Fatalf("host thread view of the reader %+v %v", view, err)
	}
	hits, err := f.engine.Recall(ctx, sentinel, 8, "")
	if err != nil || strings.Contains(fmt.Sprintf("%+v", hits), sentinel) {
		t.Fatalf("recall %+v %v", hits, err)
	}
	// Another agent cannot read it, and no later request carries it.
	q := scripted(func(protocol.ProviderRequest) protocol.ProviderResponse {
		return cogStep("t1", "read_thread", `{"id":"`+r+`"}`)
	})
	f.run(t, "read the earlier thread", q)
	for i := range 2 {
		if strings.Contains(fmt.Sprint(q.request(t, i).Messages), sentinel) {
			t.Fatalf("request %d of another run carries the restated text", i)
		}
	}
	// The reader's thread cannot be continued.
	cont := cogAnswer("continuing")
	if _, err := f.engine.runAgentWith(ctx, r, cont, AgentOptions{Input: "continue", Policy: "workspace", MaxSteps: 2}); !errors.Is(err, session.ErrFenced) {
		t.Fatalf("continuing the exposed reader: %v", err)
	}
	if len(cont.requests) != 0 {
		t.Fatal("a continuation of the exposed reader reached the model")
	}
	// No maintenance pass reads the reader again: whatever is queued, the maintainer is sent none of it.
	for range 3 {
		if rep := f.maintain(t); !rep.Ran {
			break
		}
	}
	for _, in := range f.batchInputs()[maintained:] {
		if strings.Contains(in, sentinel) || strings.Contains(in, "session:"+r+"@") {
			t.Fatalf("a maintenance batch after the deletion holds the reader: %q", in)
		}
	}
	if st := f.state(t); st.Backlog != 0 {
		t.Fatalf("backlog after the deletion %+v", st)
	}
}
