package runtime

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/taskstate"
	"github.com/edynasty/LumenCortex/protocol"
)

// handoffProvider answers runs and records what was in flight, so a test can prove that two sessions never
// hold a model request at the same time. hold keeps every request open until the gate closes.
type handoffProvider struct {
	mu       sync.Mutex
	inflight int
	max      int
	requests [][]protocol.Message
	hold     bool
	gate     chan struct{}
	closed   bool
}

func (*handoffProvider) Model() string { return "handoff-fixture" }

func (p *handoffProvider) Complete(ctx context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.mu.Lock()
	p.inflight++
	p.max = max(p.max, p.inflight)
	p.requests = append(p.requests, req.Messages)
	gate, hold := p.gate, p.hold
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.inflight--
		p.mu.Unlock()
	}()
	if hold {
		select {
		case <-ctx.Done():
			return protocol.ProviderResponse{}, ctx.Err()
		case <-gate:
		}
	}
	return protocol.ProviderResponse{Message: protocol.Message{Content: "done"}, FinishReason: "stop"}, nil
}

func (p *handoffProvider) seen(text string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, messages := range p.requests {
		if strings.Contains(rendered(messages), text) {
			return true
		}
	}
	return false
}

func (p *handoffProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.requests)
}

func (p *handoffProvider) waitRequest(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for p.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if p.count() == 0 {
		t.Fatal("no run reached a model request")
	}
}

func (p *handoffProvider) waitCount(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for p.count() < n && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if p.count() < n {
		t.Fatalf("only %d model requests reached the provider, want %d", p.count(), n)
	}
}

func (p *handoffProvider) release() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed {
		p.closed = true
		close(p.gate)
	}
}

// TestHubHandoffStopsSourceAndStartsTarget pins the handoff contract: the source stops before the target
// starts, the handoff note is observable in the source's Task State and the target's first request, and the
// two never hold the model at once.
func TestHubHandoffStopsSourceAndStartsTarget(t *testing.T) {
	useGlobalConfig(t, withConcurrency(8, 4))
	e := openTestEngine(t)
	sup, err := NewRunSupervisor(e)
	if err != nil {
		t.Fatal(err)
	}
	defer sup.Close()
	p := &handoffProvider{hold: true, gate: make(chan struct{})}
	source := newParentSession(t, e, p)
	target := newParentSession(t, e, p)
	ctx := context.Background()
	if _, err := sup.Start(ctx, source, AgentOptions{Policy: "read-only", Interactive: true, MaxSteps: 3}); err != nil {
		t.Fatal(err)
	}
	p.waitRequest(t)
	if !sup.Active(source) {
		t.Fatal("the source was not running when the handoff began")
	}
	started := time.Now()
	got, err := sup.Handoff(ctx, HubHandoff{Source: source, Target: target, Note: "continue the review", CommandID: "handoff-1"})
	if err != nil {
		t.Fatal(err)
	}
	if got != target {
		t.Fatalf("handoff target %s", got)
	}
	if sup.Active(source) {
		t.Fatal("the source is still running after the handoff")
	}
	if elapsed := time.Since(started); elapsed > handoffStopWait {
		t.Fatalf("handoff took %s", elapsed)
	}
	if !sup.Active(target) {
		t.Fatal("the target did not start")
	}
	// The note is in the source's Task State, bounded by the note limit.
	state, err := e.loadTaskState(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, note := range state.Notes {
		if strings.Contains(note.Text, "## Handoff") && strings.Contains(note.Text, target) {
			found = true
		}
	}
	if !found {
		t.Fatalf("the source's Task State has no handoff note: %+v", state.Notes)
	}
	if len(state.Notes) > taskstate.MaxNotes {
		t.Fatal("the handoff wrote more notes than the limit allows")
	}
	// The target's request carries the handoff pointer (its opening runtime row). The target's goroutine
	// reaches its first request after Start returns, so wait for it.
	p.waitCount(t, 2)
	if !p.seen("Work was handed to this session") {
		var users []string
		p.mu.Lock()
		for _, messages := range p.requests {
			for _, row := range messages {
				if row.Role == "user" {
					users = append(users, row.Content)
				}
			}
		}
		p.mu.Unlock()
		t.Fatalf("the target never read the handoff; user rows=%q", strings.Join(users, " | "))
	}
	p.release()
	waitInactive(t, sup, target)
	if p.max > 1 {
		t.Fatalf("source and target held the model at once (max in flight %d)", p.max)
	}
}

// TestHubHandoffMovesTaskLeasesAndReturnsOnFailure pins the ownership transfer: the units a source held
// belong to the target after the handoff, and a handoff whose target cannot start moves nothing.
func TestHubHandoffMovesTaskLeasesAndReturnsOnFailure(t *testing.T) {
	useGlobalConfig(t, withConcurrency(8, 4))
	e := openTestEngine(t)
	sup, err := NewRunSupervisor(e)
	if err != nil {
		t.Fatal(err)
	}
	defer sup.Close()
	p := &handoffProvider{hold: true, gate: make(chan struct{})}
	source := newParentSession(t, e, p)
	target := newParentSession(t, e, p)
	ctx := context.Background()
	if err := e.ConfigureHub(ctx, source, []HubTaskSpec{{ID: "u1", Goal: "HUB:unit"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ClaimHubTask(ctx, HubLeaseRequest{Root: source, Unit: "u1", Holder: source, TTL: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if _, err := sup.Handoff(ctx, HubHandoff{Source: source, Target: target, Note: "own the unit"}); err != nil {
		t.Fatal(err)
	}
	// The target is live and holds the unit while it works.
	leases, err := e.HubTaskLeases(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].Holder != target {
		t.Fatalf("leases after handoff %+v", leases)
	}
	// A target that cannot start leaves the claim where it was: no unit is left unowned.
	fenced := newParentSession(t, e, p)
	if _, err := e.RevokeSource(ctx, SourceSession, fenced, "handoff_test"); err != nil {
		t.Fatal(err)
	}
	if _, err := sup.Handoff(ctx, HubHandoff{Source: source, Target: fenced}); err == nil {
		t.Fatal("handoff to a revoked session was accepted")
	}
	leases, err = e.HubTaskLeases(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].Holder != target {
		t.Fatalf("a failed handoff moved the claim: %+v", leases)
	}
	p.release()
	waitInactive(t, sup, target)
}

// TestHubHandoffRefusesCrossSessionWorktreeOwner pins the ownership rule: a worktree is bound to the
// session that owns it, so handing a task to another session while the source owns one is refused.
func TestHubHandoffRefusesCrossSessionWorktreeOwner(t *testing.T) {
	useGlobalConfig(t, withConcurrency(8, 4))
	root, _ := taskRepo(t)
	e, sup := openTaskEngine(t, root)
	defer e.Close()
	defer sup.Close()
	p := &handoffProvider{}
	owner := newParentSession(t, e, p)
	other := newParentSession(t, e, p)
	ctx := context.Background()
	if _, err := e.AttachWorktree(ctx, owner, "HEAD"); err != nil {
		t.Fatal(err)
	}
	if _, err := sup.Handoff(ctx, HubHandoff{Source: owner, Target: other}); !errors.Is(err, ErrHubWorktreeOwned) {
		t.Fatalf("cross-session worktree handoff: %v", err)
	}
	if sup.Active(owner) || sup.Active(other) {
		t.Fatal("a refused handoff started a run")
	}
}
