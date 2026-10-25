package runtime

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/protocol"
)

// blockingReader reads one thread, then blocks its next request until the run is cancelled, and takes
// a while to return after that, so a caller that does not wait for the run sees it still running.
type blockingReader struct {
	source  string
	calls   atomic.Int32
	blocked chan struct{}
}

func (p *blockingReader) Model() string { return "blocking" }

func (p *blockingReader) Complete(ctx context.Context, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	if p.calls.Add(1) == 1 {
		return toolCalls(tc("b1", "read_thread", `{"id":"`+p.source+`"}`)), nil
	}
	close(p.blocked)
	<-ctx.Done()
	time.Sleep(300 * time.Millisecond)
	return protocol.ProviderResponse{}, ctx.Err()
}

// countingProvider answers at once and counts its calls.
type countingProvider struct{ calls atomic.Int32 }

func (p *countingProvider) Model() string { return "counting" }

func (p *countingProvider) Complete(context.Context, protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.calls.Add(1)
	return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", Content: "done"}, FinishReason: "stop"}, nil
}

// Plan T3-S1 through the real Engine, SQLite and the agent tools. A thread that read the revoked one
// with read_thread, and a run reading it right now, are found through the edges those reads stored;
// the run in this Engine is cancelled and has ended when RevokeSource returns, and the request it had
// admitted is reported in flight, then unknown. After the revocation a fenced session sends nothing,
// runs no check, and the revoked thread is withdrawn from recall; a thread that is only a source of a
// fenced one, and an unrelated one, are untouched.
func TestRevokeSourceFencesReadersAndCancelsTheirRuns(t *testing.T) {
	useChecks(t, `{"status": {"kind": "test", "command": "true"}}`)
	engine, _ := gitEngine(t)
	ctx := context.Background()
	x := newTestSession(t, engine, "the deploy host is alpha-7")
	y := newTestSession(t, engine, "the retry policy uses jitter")
	z := newTestSession(t, engine, "an unrelated thread")

	// s read x with read_thread and found y with recall: both are its sources.
	s := newTestSession(t, engine, "a reader")
	reader := &threadProvider{responses: []protocol.ProviderResponse{
		toolCalls(tc("r1", "read_thread", `{"id":"`+x.ID+`"}`), tc("r2", "recall", `{"query":"retry policy jitter"}`)),
		{Message: protocol.Message{Role: "assistant", Content: "read"}, FinishReason: "stop"},
	}}
	if r, err := engine.runAgentWith(ctx, s.ID, reader, AgentOptions{Input: "read", MaxSteps: 3}); err != nil || r.Status != "completed" {
		t.Fatalf("reader run %+v %v", r, err)
	}

	// r is reading x right now, in this Engine.
	r := newTestSession(t, engine, "a running reader")
	blocking := &blockingReader{source: x.ID, blocked: make(chan struct{})}
	ran := make(chan AgentResult, 1)
	go func() {
		res, _ := engine.runAgentWith(ctx, r.ID, blocking, AgentOptions{Input: "read", MaxSteps: 5})
		ran <- res
	}()
	select {
	case <-blocking.blocked:
	case <-time.After(time.Minute):
		t.Fatal("the running reader never reached its second request")
	}

	if _, err := engine.RevokeSource(ctx, "file", "a.go", "user_revoked"); !errors.Is(err, ErrUnsupportedSource) {
		t.Fatalf("revoking a file: %v", err)
	}
	rep, err := engine.RevokeSource(ctx, SourceSession, x.ID, "user_revoked")
	if err != nil {
		t.Fatal(err)
	}
	// The run had ended, with its terminal state stored, before RevokeSource returned.
	if info, _ := sessionState(t, engine, r.ID); info.Status == "running" {
		t.Fatalf("RevokeSource returned while the run of %s was still running", r.ID)
	}
	if res := <-ran; res.Status == "completed" {
		t.Fatalf("the cancelled run completed: %+v", res)
	}

	for _, want := range []string{x.ID, s.ID, r.ID} {
		if !slices.Contains(rep.Sessions, want) {
			t.Errorf("report sessions %v miss %s", rep.Sessions, want)
		}
	}
	for _, not := range []string{y.ID, z.ID} {
		if slices.Contains(rep.Sessions, not) {
			t.Errorf("report sessions %v hold %s, which read nothing of x", rep.Sessions, not)
		}
	}
	if rep.Sessions[0] != x.ID || rep.Expanded || rep.Answered == 0 {
		t.Fatalf("report %+v", rep.Revocation)
	}
	if len(rep.InFlight) != 1 || rep.InFlightNow[rep.InFlight[0]] != string(session.RequestUnknown) {
		t.Fatalf("in flight %v now %v: want r's admitted request, unknown after the cancellation", rep.InFlight, rep.InFlightNow)
	}
	again, err := engine.ExposureReport(ctx, rep.Epoch)
	if err != nil || again.Epoch != rep.Epoch || !slices.Equal(again.Sessions, rep.Sessions) {
		t.Fatalf("read back %+v %v", again, err)
	}
	if _, err := engine.ExposureReport(ctx, rep.Epoch+1); !errors.Is(err, ErrNoRevocation) {
		t.Fatalf("unknown epoch: %v", err)
	}

	// A fenced session sends nothing. s answered after reading x, so its thread cannot be projected
	// (T3-S2): the Start is refused before anything is written.
	p := &countingProvider{}
	seqBefore, _ := engine.store.NewestMessageSeq(ctx, s.ID)
	if _, err := engine.runAgentWith(ctx, s.ID, p, AgentOptions{Input: "again", MaxSteps: 2}); !errors.Is(err, session.ErrFenced) || p.calls.Load() != 0 {
		t.Fatalf("fenced session run: %v, %d calls", err, p.calls.Load())
	}
	if after, _ := engine.store.NewestMessageSeq(ctx, s.ID); after != seqBefore {
		t.Fatalf("a refused Start wrote rows: %d -> %d", seqBefore, after)
	}
	// It runs no check, and stores no result.
	checks := func(id string) runChecks {
		ev, err := engine.sessionEvaluator(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		specs, _, err := engine.effectiveChecks()
		if err != nil {
			t.Fatal(err)
		}
		return runChecks{ev: ev, specs: specs, store: engine.store, sessionID: id}
	}
	stored := func(id string) int {
		rows, err := engine.CheckResults(ctx, id, -1, 0)
		if err != nil {
			t.Fatal(err)
		}
		return len(rows)
	}
	before := stored(s.ID)
	if _, err := checks(s.ID).Decide(ctx, []string{"status"}, ""); !errors.Is(err, session.ErrFenced) {
		t.Fatalf("check in a fenced session: %v", err)
	}
	if n := stored(s.ID); n != before {
		t.Fatalf("a fenced session stored check results: %d -> %d", before, n)
	}
	if v, err := checks(z.ID).Decide(ctx, []string{"status"}, ""); err != nil || len(v) != 1 || !v[0].Passed {
		t.Fatalf("check in an unrelated session: %+v %v", v, err)
	}
	// The revoked thread is withdrawn from recall and read_thread.
	hits, err := engine.Recall(ctx, "deploy host alpha-7", 8, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.ID == x.ID {
			t.Fatalf("recall found the revoked thread: %+v", h)
		}
	}
	if v, err := engine.ReadThread(ctx, x.ID); err != nil || !v.Withdrawn || v.Goal != "" {
		t.Fatalf("read_thread of the revoked thread: %+v %v", v, err)
	}
	// A source of a fenced session and an unrelated session run as before.
	for _, id := range []string{y.ID, z.ID} {
		if res, err := engine.runAgentWith(ctx, id, &countingProvider{}, AgentOptions{Input: "go", MaxSteps: 2}); err != nil || res.Status != "completed" {
			t.Fatalf("run of %s after the revocation: %+v %v", id, res, err)
		}
	}
}

// The Auto-effort judge is a request too: after a revocation the judge of a fenced session is not called.
func TestRevokedSessionDoesNotCallTheAutoJudge(t *testing.T) {
	engine := openTestEngine(t)
	main, judge := &countingProvider{}, &judgeProvider{level: "high"}
	bindRef(engine, "b0", main)
	bindRef(engine, "b1", judge)
	useGlobalConfig(t, autoConfig("test/b0", "test/b1", "high", ""))
	ctx := context.Background()
	handle := newTestSession(t, engine, "explain the build")
	if _, err := engine.RevokeSource(ctx, SourceSession, handle.ID, "user_revoked"); err != nil {
		t.Fatal(err)
	}
	_, err := engine.RunAgent(ctx, handle.ID, AgentOptions{Effort: config.EffortAuto, MaxSteps: 2})
	if !errors.Is(err, session.ErrFenced) || judge.calls.Load() != 0 || main.calls.Load() != 0 {
		t.Fatalf("auto run of a revoked session: err %v, judge %d, main %d calls", err, judge.calls.Load(), main.calls.Load())
	}
}
