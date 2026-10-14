package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/protocol"
)

// These tests cover the Start transaction (design §3.2, M1.2): after Reconcile and
// validation, Input, the provider/model identity and the running transition commit
// together or not at all.

// modelProvider reports a model name, so the identity written by Start is visible.
type modelProvider struct {
	*exitProvider
	model string
}

func (p modelProvider) Model() string { return p.model }

type startSnapshot struct {
	Status   string
	Provider string
	Model    string
	ModelRef string
	Final    string
	Cause    string
	Rows     []protocol.Message
}

func snapshotStart(t *testing.T, engine *Engine, handle *SessionHandle) startSnapshot {
	t.Helper()
	info, _ := sessionState(t, engine, handle.ID)
	snap := startSnapshot{Status: info.Status, Provider: info.Provider, Model: info.Model, ModelRef: info.ModelRef, Cause: string(info.Error), Rows: transcript(t, handle)}
	if info.Final != nil {
		snap.Final = *info.Final
	}
	return snap
}

func TestFailedStartTransactionLeavesNoTraceAndReleasesTheLock(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, engine *Engine, supervisor *RunSupervisor, id string)
		want  string // status before the failed Start
	}{
		{
			name: "completed run with a final answer",
			setup: func(t *testing.T, engine *Engine, supervisor *RunSupervisor, id string) {
				provider := modelProvider{completingProvider("first answer"), "model-alpha"}
				if _, err := supervisor.startWith(context.Background(), id, provider, AgentOptions{MaxSteps: 2}); err != nil {
					t.Fatal(err)
				}
				waitUntilIdle(t, supervisor, id, 10*time.Second)
			},
			want: "completed",
		},
		{
			name: "cancelled run with a stored cause",
			setup: func(t *testing.T, engine *Engine, supervisor *RunSupervisor, id string) {
				blocking := &blockingProvider{started: make(chan struct{})}
				if _, err := supervisor.startWith(context.Background(), id, blocking, AgentOptions{MaxSteps: 2}); err != nil {
					t.Fatal(err)
				}
				select {
				case <-blocking.started:
				case <-time.After(10 * time.Second):
					t.Fatal("provider never called")
				}
				supervisor.Cancel(id)
				waitUntilIdle(t, supervisor, id, 10*time.Second)
			},
			want: "interrupted",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine := openTestEngine(t)
			handle := newTestSession(t, engine, "atomic start")
			supervisor, err := NewRunSupervisor(engine)
			if err != nil {
				t.Fatal(err)
			}
			defer supervisor.Close()
			tc.setup(t, engine, supervisor, handle.ID)
			before := snapshotStart(t, engine, handle)
			if before.Status != tc.want || before.Provider != "test" || before.ModelRef != "test/b0" {
				t.Fatalf("setup left status=%q provider=%q modelRef=%q", before.Status, before.Provider, before.ModelRef)
			}

			// The failing statement is the status update, the last one of the
			// transaction: the Input row and the identity update before it must roll back.
			side, err := sql.Open("sqlite3", "file:"+engine.store.Path()+"?_busy_timeout=5000")
			if err != nil {
				t.Fatal(err)
			}
			defer side.Close()
			if _, err := side.Exec(`CREATE TRIGGER inject_start_failure BEFORE UPDATE OF status ON sessions
WHEN NEW.status = 'running'
BEGIN SELECT RAISE(ABORT, 'database or disk is full (injected)'); END`); err != nil {
				t.Fatal(err)
			}

			provider := modelProvider{completingProvider("never runs"), "model-beta"}
			_, err = supervisor.startWith(context.Background(), handle.ID, provider, AgentOptions{Input: "follow-up", MaxSteps: 2})
			if err == nil || !strings.Contains(err.Error(), "injected") {
				t.Fatalf("Start err = %v, want the injected storage failure", err)
			}
			if provider.n != 0 {
				t.Fatalf("the provider was called %d times for a Start that failed", provider.n)
			}
			if after := snapshotStart(t, engine, handle); !reflect.DeepEqual(after, before) {
				t.Fatalf("a failed Start changed the session:\nbefore %+v\nafter  %+v", before, after)
			}
			if supervisor.Active(handle.ID) {
				t.Fatal("a failed Start left an active run")
			}
			lock, err := engine.acquireRun(context.Background(), handle.ID)
			if err != nil {
				t.Fatalf("the run lock was not released after the failed Start: %v", err)
			}
			lock.Release()

			if _, err := side.Exec(`DROP TRIGGER inject_start_failure`); err != nil {
				t.Fatal(err)
			}
			provider = modelProvider{completingProvider("second answer"), "model-beta"}
			if _, err := supervisor.startWith(context.Background(), handle.ID, provider, AgentOptions{Input: "follow-up", MaxSteps: 2}); err != nil {
				t.Fatalf("Start after the failed Start: %v", err)
			}
			waitUntilIdle(t, supervisor, handle.ID, 10*time.Second)
			final := snapshotStart(t, engine, handle)
			if final.Status != "completed" || final.Provider != "test" || final.Model != "b2" || final.Final != "second answer" || final.Cause != "" {
				t.Fatalf("second Start: %+v", final)
			}
			follow := 0
			for _, row := range final.Rows {
				if row.Role == "user" && row.Content == "follow-up" {
					follow++
				}
			}
			if follow != 1 {
				t.Fatalf("the follow-up Input is in the transcript %d times, want 1", follow)
			}
		})
	}
}

// When Start returns, Input, the identity and the running status are one committed
// state, already visible when the loop makes its first provider call.
func TestSuccessfulStartCommitsInputAndRunningBeforeTheLoop(t *testing.T) {
	engine := openTestEngine(t)
	handle := newTestSession(t, engine, "commit together")
	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()

	first := modelProvider{completingProvider("done"), "model-alpha"}
	if _, err := supervisor.startWith(context.Background(), handle.ID, first, AgentOptions{MaxSteps: 2}); err != nil {
		t.Fatal(err)
	}
	waitUntilIdle(t, supervisor, handle.ID, 10*time.Second)
	if info, _ := sessionState(t, engine, handle.ID); info.Final == nil {
		t.Fatal("setup: no final answer")
	}

	var seen startSnapshot
	var seenAt int
	probe := &exitProvider{fn: func(_ context.Context, call int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		if call == 1 {
			seen = snapshotStart(t, engine, handle)
			seenAt = len(seen.Rows)
		}
		return protocol.ProviderResponse{Message: protocol.Message{Content: "second"}}, nil
	}}
	if _, err := supervisor.startWith(context.Background(), handle.ID, modelProvider{probe, "model-beta"}, AgentOptions{Input: "next task", MaxSteps: 2}); err != nil {
		t.Fatal(err)
	}
	waitUntilIdle(t, supervisor, handle.ID, 10*time.Second)

	if seen.Status != "running" || seen.Final != "" || seen.Cause != "" {
		t.Fatalf("at the first provider call: status=%q final=%q cause=%q, want running with the previous outcome cleared", seen.Status, seen.Final, seen.Cause)
	}
	// The second Start names a different model than the one the session is pinned to (rule
	// model.user), so the identity and the pin move together with Input and running.
	if seen.Provider != "test" || seen.Model != "b1" || seen.ModelRef != "test/b1" {
		t.Fatalf("at the first provider call the identity is %s/%s (%s), want test/b1 (test/b1)", seen.Provider, seen.Model, seen.ModelRef)
	}
	if seenAt == 0 || seen.Rows[seenAt-1].Role != "user" || seen.Rows[seenAt-1].Content != "next task" {
		raw, _ := json.Marshal(seen.Rows)
		t.Fatalf("Input is not the newest row at the first provider call: %s", raw)
	}
}
