package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/protocol"
)

type blockingProvider struct {
	once    sync.Once
	started chan struct{}
}

func (p *blockingProvider) Model() string { return "blocking-model" }

func (p *blockingProvider) Complete(ctx context.Context, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.once.Do(func() { close(p.started) })
	<-ctx.Done()
	return protocol.ProviderResponse{}, ctx.Err()
}

func TestRunSupervisorOwnsActiveRunLifecycle(t *testing.T) {
	engine, err := Open(Options{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()

	handle, err := engine.NewSession(context.Background(), SessionOptions{Goal: "wait for cancel"})
	if err != nil {
		t.Fatal(err)
	}

	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()

	provider := &blockingProvider{started: make(chan struct{})}
	started, err := supervisor.Start(context.Background(), handle.ID, provider, AgentOptions{
		ProviderName: "blocking-provider",
		Policy:       "read-only",
	})
	if err != nil {
		t.Fatal(err)
	}
	if started.Status != "running" {
		t.Fatalf("started status=%q", started.Status)
	}
	if !supervisor.Active(handle.ID) {
		t.Fatal("run should be active immediately after Start")
	}

	select {
	case <-provider.started:
	case <-time.After(2 * time.Second):
		t.Fatal("provider request did not start")
	}

	runs := supervisor.Runs()
	if len(runs) != 1 || runs[0].SessionID != handle.ID {
		t.Fatalf("runs=%#v", runs)
	}

	if _, err := supervisor.Start(context.Background(), handle.ID, provider, AgentOptions{}); !errors.Is(err, ErrRunAlreadyActive) {
		t.Fatalf("duplicate start err=%v", err)
	}
	if !supervisor.Cancel(handle.ID) {
		t.Fatal("cancel should report an active run")
	}

	deadline := time.Now().Add(2 * time.Second)
	for supervisor.Active(handle.ID) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if supervisor.Active(handle.ID) {
		t.Fatal("run remained active after cancellation")
	}

	_, info, err := engine.Session(context.Background(), handle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != "interrupted" {
		t.Fatalf("session status=%q, want interrupted", info.Status)
	}
}

func TestRunSupervisorCloseCancelsOwnedRuns(t *testing.T) {
	engine, err := Open(Options{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()

	handle, err := engine.NewSession(context.Background(), SessionOptions{Goal: "close supervisor"})
	if err != nil {
		t.Fatal(err)
	}
	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}

	provider := &blockingProvider{started: make(chan struct{})}
	if _, err := supervisor.Start(context.Background(), handle.ID, provider, AgentOptions{Policy: "read-only"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-provider.started:
	case <-time.After(2 * time.Second):
		t.Fatal("provider request did not start")
	}

	begin := time.Now()
	supervisor.Close()
	if elapsed := time.Since(begin); elapsed > time.Second {
		t.Fatalf("supervisor close took %s", elapsed)
	}
	if supervisor.Active(handle.ID) {
		t.Fatal("run remained active after supervisor close")
	}
	if _, err := supervisor.Start(context.Background(), handle.ID, provider, AgentOptions{}); !errors.Is(err, ErrRunSupervisorClosed) {
		t.Fatalf("start after close err=%v", err)
	}
}


func TestRecoverStaleRunsInterruptsPersistedRunningSessions(t *testing.T) {
	engine, err := Open(Options{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()

	handle, err := engine.NewSession(context.Background(), SessionOptions{Goal: "recover stale"})
	if err != nil {
		t.Fatal(err)
	}
	running := "running"
	if err := engine.store.Update(context.Background(), handle.ID, session.Patch{Status: &running}); err != nil {
		t.Fatal(err)
	}

	count, err := engine.RecoverStaleRuns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("recovered=%d, want 1", count)
	}
	_, info, err := engine.Session(context.Background(), handle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != "interrupted" {
		t.Fatalf("status=%q, want interrupted", info.Status)
	}
	if len(info.Error) == 0 {
		t.Fatal("expected recovery error metadata")
	}
}


func TestRunSupervisorEnforcesMaxAgentsAndReusesReleasedSlots(t *testing.T) {
	engine, err := Open(Options{
		Workspace: t.TempDir(),
		Budget: Budget{MaxAgents: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()

	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()

	handles := make([]*SessionHandle, 3)
	providers := make([]*blockingProvider, 3)
	for i := range handles {
		handle, err := engine.NewSession(context.Background(), SessionOptions{Goal: "parallel"})
		if err != nil {
			t.Fatal(err)
		}
		handles[i] = handle
		providers[i] = &blockingProvider{started: make(chan struct{})}
	}

	for i := 0; i < 2; i++ {
		if _, err := supervisor.Start(context.Background(), handles[i].ID, providers[i], AgentOptions{Policy: "read-only"}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-providers[i].started:
		case <-time.After(2 * time.Second):
			t.Fatalf("provider %d did not start", i)
		}
	}
	if got := engine.Health().ActiveAgents; got != 2 {
		t.Fatalf("active agents=%d want=2", got)
	}

	if _, err := supervisor.Start(context.Background(), handles[2].ID, providers[2], AgentOptions{Policy: "read-only"}); !errors.Is(err, ErrAgentLimitExceeded) {
		t.Fatalf("third start err=%v", err)
	}

	if !supervisor.Cancel(handles[0].ID) {
		t.Fatal("cancel first run failed")
	}
	deadline := time.Now().Add(2 * time.Second)
	for engine.Health().ActiveAgents != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := engine.Health().ActiveAgents; got != 1 {
		t.Fatalf("active agents after cancel=%d want=1", got)
	}

	if _, err := supervisor.Start(context.Background(), handles[2].ID, providers[2], AgentOptions{Policy: "read-only"}); err != nil {
		t.Fatalf("start after release: %v", err)
	}
}
