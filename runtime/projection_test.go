package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/window"
	"github.com/edynasty/LumenCortex/protocol"
)

// Plan T3-S2 through the real Engine and SQLite. r read x with read_thread and was cancelled by the
// revocation of x before any answer was stored after the read; s read x and answered. The Engine is
// then closed and opened again: the projection is read from the store, not from memory. The next Start
// of r projects its thread: the request it sends keeps the read's tool row, with the content withheld,
// and nothing of x; the run completes and r's later Starts are admitted. s and x are refused at the
// Start, before anything is admitted or written.
func TestStartProjectsAThreadThatReadARevokedSource(t *testing.T) {
	workspace := t.TempDir()
	engine, err := Open(Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	x := newTestSession(t, engine, "the deploy host is alpha-7")
	if res, err := engine.runAgentWith(ctx, x.ID, &threadProvider{responses: nil}, AgentOptions{Input: "the deploy host is alpha-7", MaxSteps: 2}); err != nil || res.Status != "completed" {
		t.Fatalf("source run %+v %v", res, err)
	}

	s := newTestSession(t, engine, "an exposed reader")
	reader := &threadProvider{responses: []protocol.ProviderResponse{toolCalls(tc("r1", "read_thread", `{"id":"`+x.ID+`"}`))}}
	if res, err := engine.runAgentWith(ctx, s.ID, reader, AgentOptions{Input: "read", MaxSteps: 3}); err != nil || res.Status != "completed" {
		t.Fatalf("reader run %+v %v", res, err)
	}

	r := newTestSession(t, engine, "a cancelled reader")
	blocking := &blockingReader{source: x.ID, blocked: make(chan struct{})}
	ran := make(chan struct{})
	go func() {
		defer close(ran)
		_, _ = engine.runAgentWith(ctx, r.ID, blocking, AgentOptions{Input: "read", MaxSteps: 5})
	}()
	select {
	case <-blocking.blocked:
	case <-time.After(time.Minute):
		t.Fatal("the reader never reached its second request")
	}
	if _, err := engine.RevokeSource(ctx, SourceSession, x.ID, "user_revoked"); err != nil {
		t.Fatal(err)
	}
	<-ran
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	if engine, err = Open(Options{Workspace: workspace}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })

	for _, id := range []string{x.ID, s.ID} {
		p := &countingProvider{}
		if _, err := engine.runAgentWith(ctx, id, p, AgentOptions{Input: "again", MaxSteps: 2}); !errors.Is(err, session.ErrFenced) || p.calls.Load() != 0 {
			t.Fatalf("Start of %s: %v, %d calls; want ErrFenced and no request", id, err, p.calls.Load())
		}
	}

	next := &threadProvider{}
	res, err := engine.runAgentWith(ctx, r.ID, next, AgentOptions{Input: "go on without it", MaxSteps: 2})
	if err != nil || res.Status != "completed" || len(next.requests) != 1 {
		t.Fatalf("projected run %+v %v, %d requests", res, err, len(next.requests))
	}
	withheld := false
	for _, m := range next.requests[0].Messages {
		if strings.Contains(m.Content, "alpha-7") {
			t.Fatalf("the projected request carries the revoked content in a %s row: %q", m.Role, m.Content)
		}
		if m.Role == "tool" && m.ToolCallID == "b1" {
			withheld = m.Content == window.WithheldContent
		}
	}
	if !withheld {
		t.Fatalf("the read's tool row is not in the request withheld: %+v", next.requests[0].Messages)
	}
	if res, err := engine.runAgentWith(ctx, r.ID, &threadProvider{}, AgentOptions{Input: "once more", MaxSteps: 2}); err != nil || res.Status != "completed" {
		t.Fatalf("later run of the projected thread %+v %v", res, err)
	}
}
