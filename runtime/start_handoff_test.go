package runtime

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/protocol"
)

// commitCancelledContext is a caller context that cancels itself at the instant the
// Start transaction commits: it reports Done/Err as cancelled as soon as an
// independent connection sees the session row as running. Anything Start does with
// the caller's context after the commit therefore fails, deterministically and
// without timing: the old post-commit engine.Session(ctx) read did.
type commitCancelledContext struct {
	context.Context
	committed func() bool

	mu     sync.Mutex
	done   chan struct{}
	closed bool
}

func newCommitCancelledContext(committed func() bool) *commitCancelledContext {
	return &commitCancelledContext{Context: context.Background(), committed: committed, done: make(chan struct{})}
}

func (c *commitCancelledContext) check() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed && c.committed() {
		c.closed = true
		close(c.done)
	}
}

func (c *commitCancelledContext) Done() <-chan struct{} { c.check(); return c.done }

func (c *commitCancelledContext) Err() error {
	c.check()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return context.Canceled
	}
	return nil
}

// A committed Start is handed off even if caller cancellation arrives at the commit. Its goroutine
// must then persist interruption and release ownership rather than leaving a running orphan.
func TestCommittedStartSettlesCallerCancellation(t *testing.T) {
	workspace := t.TempDir()
	engine := openEngineAt(t, workspace)
	handle := newTestSession(t, engine, "handoff")
	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()

	side, err := sql.Open("sqlite3", "file:"+engine.store.Path()+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	defer side.Close()
	// The previous outcome Start has to clear: a stored cause and a final answer.
	if _, err := side.Exec(`UPDATE sessions SET status = 'interrupted', final = 'old final',
error_json = '{"kind":"cancelled","message":"old","at":"2026-01-01T00:00:00Z"}', provider = 'alpha', model = 'model-alpha' WHERE id = ?`, handle.ID); err != nil {
		t.Fatal(err)
	}
	statusRunning := func() bool {
		var status string
		return side.QueryRow(`SELECT status FROM sessions WHERE id = ?`, handle.ID).Scan(&status) == nil && status == "running"
	}
	ctx := newCommitCancelledContext(statusRunning)

	release := make(chan struct{})
	provider := modelProvider{&exitProvider{fn: func(rctx context.Context, _ int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		select {
		case <-release:
			return protocol.ProviderResponse{Message: protocol.Message{Content: "all done"}}, nil
		case <-rctx.Done():
			return protocol.ProviderResponse{}, rctx.Err()
		}
	}}, "model-beta"}

	info, err := supervisor.startWith(ctx, handle.ID, provider, AgentOptions{Input: "next task", MaxSteps: 2})
	if err != nil {
		// The bug's shape: Start failed after the commit. Say which half-state it left.
		var status string
		_ = side.QueryRow(`SELECT status FROM sessions WHERE id = ?`, handle.ID).Scan(&status)
		t.Fatalf("Start returned %v; durable status=%q, active run=%v (error + running + no owner is the forbidden state)", err, status, supervisor.Active(handle.ID))
	}
	// Start no longer consults ctx after the commit, so look at it now: the commit
	// happened, hence the caller context is cancelled from here on.
	if ctx.Err() == nil {
		t.Fatal("the caller context is not cancelled: the commit the seam watches for did not happen")
	}

	// The returned info is the committed state.
	if info.Status != "running" || info.Provider != "test" || info.Model != "b0" || info.ModelRef != "test/b0" || info.Final != nil || len(info.Error) != 0 {
		t.Fatalf("Start returned %+v, want the committed running state with the previous outcome cleared", info)
	}
	rows := transcript(t, handle)
	inputs := 0
	for _, row := range rows {
		if row.Role == "user" && row.Content == "next task" {
			inputs++
		}
	}
	if inputs != 1 {
		t.Fatalf("Input appears %d times, want exactly once", inputs)
	}

	waitUntilIdle(t, supervisor, handle.ID, 10*time.Second)
	final, cause := sessionState(t, engine, handle.ID)
	if final.Status != "interrupted" || cause == nil || cause.Kind != "cancelled" || final.Final != nil {
		t.Fatalf("terminal state: status=%q cause=%+v final=%v", final.Status, cause, final.Final)
	}
	lock, err := engine.acquireRun(context.Background(), handle.ID)
	if err != nil {
		t.Fatalf("the run lock was not released after the terminal state: %v", err)
	}
	lock.Release()
}
