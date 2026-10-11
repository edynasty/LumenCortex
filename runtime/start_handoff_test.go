package runtime

import (
	"context"
	"database/sql"
	"errors"
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

// Once the Start transaction commits, Start hands the run to its goroutine without a
// fallible step in between (§3.2). "Start failed, the row says running, nobody owns
// the run" must be impossible even if the caller's context dies at the commit.
func TestStartHandsOffTheCommittedRunEvenWhenTheCallerContextDiesAtTheCommit(t *testing.T) {
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

	info, err := supervisor.Start(ctx, handle.ID, provider, AgentOptions{Input: "next task", ProviderName: "beta", MaxSteps: 2})
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
	if info.Status != "running" || info.Provider != "beta" || info.Model != "model-beta" || info.Final != nil || len(info.Error) != 0 {
		t.Fatalf("Start returned %+v, want the committed running state with the previous outcome cleared", info)
	}
	if stored, _ := sessionState(t, engine, handle.ID); stored.Status != "running" || stored.Provider != "beta" || stored.Model != "model-beta" || stored.Final != nil || len(stored.Error) != 0 {
		t.Fatalf("stored session %+v", stored)
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

	// The run owns the lock while it executes: another Engine is refused and writes nothing.
	if !supervisor.Active(handle.ID) {
		t.Fatal("the run has no owner after a successful Start")
	}
	other := openEngineAt(t, workspace)
	otherSupervisor, err := NewRunSupervisor(other)
	if err != nil {
		t.Fatal(err)
	}
	defer otherSupervisor.Close()
	if _, err := otherSupervisor.Start(context.Background(), handle.ID, completingProvider("x"), AgentOptions{Input: "refused"}); !errors.Is(err, ErrRunHeld) {
		t.Fatalf("Start from another Engine during the run: %v, want ErrRunHeld", err)
	}
	if got := len(transcript(t, handle)); got != len(rows) {
		t.Fatalf("the refused Start wrote rows: %d -> %d", len(rows), got)
	}

	// The run reaches its proper terminal state and then frees the lock.
	close(release)
	waitUntilIdle(t, supervisor, handle.ID, 10*time.Second)
	final, cause := sessionState(t, engine, handle.ID)
	if final.Status != "completed" || cause != nil || final.Final == nil || *final.Final != "all done" {
		t.Fatalf("terminal state: status=%q cause=%+v final=%v", final.Status, cause, final.Final)
	}
	lock, err := engine.acquireRun(context.Background(), handle.ID)
	if err != nil {
		t.Fatalf("the run lock was not released after the terminal state: %v", err)
	}
	lock.Release()
}
