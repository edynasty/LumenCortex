package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/protocol"
)

// faultStore is memoryStore with injectable failures that behave like the real store:
// writes made with a cancelled context fail.
type faultStore struct {
	*memoryStore
	failUpdate func(SessionPatch) error
	failAppend func(protocol.Message) error

	probeMu       sync.Mutex
	terminalCtxs  []context.Context // contexts of the Updates that wrote a terminal status
	terminalCalls int
}

func (s *faultStore) Update(ctx context.Context, id string, p SessionPatch) error {
	if s.failUpdate != nil {
		if err := s.failUpdate(p); err != nil {
			return err
		}
	}
	if p.Status != nil && *p.Status != StatusRunning {
		s.probeMu.Lock()
		s.terminalCalls++
		s.terminalCtxs = append(s.terminalCtxs, ctx)
		s.probeMu.Unlock()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.memoryStore.Update(ctx, id, p)
}

func (s *faultStore) AppendMessage(ctx context.Context, id string, m protocol.Message) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if s.failAppend != nil {
		if err := s.failAppend(m); err != nil {
			return 0, err
		}
	}
	return s.memoryStore.AppendMessage(ctx, id, m)
}

// scriptTools runs fn for every tool call; the tool set is a single "shell" tool.
type scriptTools struct {
	fn       func(ctx context.Context, name string, args map[string]any) (protocol.ToolResult, error)
	executed int
}

func (t *scriptTools) Specs([]string) []protocol.ToolSpec {
	return []protocol.ToolSpec{{Name: "shell"}}
}

func (t *scriptTools) Execute(ctx context.Context, name string, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	t.executed++
	return t.fn(ctx, name, args)
}

// funcProvider answers every request with fn.
type funcProvider struct {
	fn func(ctx context.Context, call int, req protocol.ProviderRequest) (protocol.ProviderResponse, error)
	n  int
}

func (p *funcProvider) Model() string { return "fake" }

func (p *funcProvider) Complete(ctx context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.n++
	return p.fn(ctx, p.n, req)
}

func shellCall(id string) protocol.ProviderResponse {
	return protocol.ProviderResponse{Message: protocol.Message{ToolCalls: []protocol.ToolCall{call(id, "shell", `{"command":"x"}`)}}}
}

func newState(id string) *memoryStore {
	return &memoryStore{state: SessionState{ID: id, Goal: "goal", Status: "created", Metadata: map[string]any{}}, steps: map[int64]any{}}
}

func assertExit(t *testing.T, store *memoryStore, result Result, wantStatus, wantKind string) {
	t.Helper()
	if store.state.Status != wantStatus {
		t.Fatalf("persisted status = %q, want %q", store.state.Status, wantStatus)
	}
	if result.Status != wantStatus {
		t.Fatalf("result status = %q, want %q", result.Status, wantStatus)
	}
	if wantKind == "" {
		if store.cause != nil {
			t.Fatalf("a %s run must store no cause, got %v", wantStatus, store.cause)
		}
		return
	}
	if store.cause["kind"] != wantKind {
		t.Fatalf("persisted kind = %v, want %q (cause %v)", store.cause["kind"], wantKind, store.cause)
	}
	if message, _ := store.cause["message"].(string); message == "" {
		t.Fatalf("cause has no message: %v", store.cause)
	}
	if at, _ := store.cause["at"].(string); at == "" {
		t.Fatalf("cause has no timestamp: %v", store.cause)
	} else if _, err := time.Parse(time.RFC3339Nano, at); err != nil {
		t.Fatalf("cause timestamp %q: %v", at, err)
	}
}

func TestEveryExitPathStoresStatusAndCause(t *testing.T) {
	storageErr := errors.New("disk I/O error")
	cases := []struct {
		name       string
		wantStatus string
		wantKind   string
		wantErr    func(error) bool
		run        func(t *testing.T) (*memoryStore, Result, error)
	}{
		{
			name: "cancelled before the first step", wantStatus: StatusInterrupted, wantKind: KindCancelled,
			wantErr: func(err error) bool { return errors.Is(err, context.Canceled) },
			run: func(t *testing.T) (*memoryStore, Result, error) {
				store := newState("c1")
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				r, err := (&Loop{Provider: cancelProvider{}, Store: store, Tools: fakeTools{}}).Run(ctx, "c1", Options{MaxSteps: 2})
				return store, r, err
			},
		},
		{
			name: "cancelled while the provider is working", wantStatus: StatusInterrupted, wantKind: KindCancelled,
			wantErr: func(err error) bool { return errors.Is(err, context.Canceled) },
			run: func(t *testing.T) (*memoryStore, Result, error) {
				store := newState("c2")
				ctx, cancel := context.WithCancel(context.Background())
				provider := &funcProvider{fn: func(ctx context.Context, _ int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
					cancel()
					<-ctx.Done()
					return protocol.ProviderResponse{}, ctx.Err()
				}}
				r, err := (&Loop{Provider: provider, Store: store, Tools: fakeTools{}}).Run(ctx, "c2", Options{MaxSteps: 2})
				return store, r, err
			},
		},
		{
			name: "cancelled while a tool runs and the tool returns a result, not an error", wantStatus: StatusInterrupted, wantKind: KindCancelled,
			wantErr: func(err error) bool { return errors.Is(err, context.Canceled) },
			run: func(t *testing.T) (*memoryStore, Result, error) {
				store := newState("c3")
				ctx, cancel := context.WithCancel(context.Background())
				// The shell runner reports a killed command like this: OK=false, no error.
				tools := &scriptTools{fn: func(ctx context.Context, _ string, _ map[string]any) (protocol.ToolResult, error) {
					cancel()
					<-ctx.Done()
					return protocol.ToolResult{OK: false, Content: `{"exitCode":-1,"cancelled":true}`}, nil
				}}
				provider := &funcProvider{fn: func(context.Context, int, protocol.ProviderRequest) (protocol.ProviderResponse, error) {
					return shellCall("t1"), nil
				}}
				r, err := (&Loop{Provider: provider, Store: store, Tools: tools}).Run(ctx, "c3", Options{MaxSteps: 3})
				for _, m := range store.messages {
					if m.Role == "tool" {
						t.Errorf("the output of a tool stopped by cancellation must not be saved as a result: %+v", m)
					}
				}
				return store, r, err
			},
		},
		{
			name: "cancelled while a tool runs and the tool returns the context error", wantStatus: StatusInterrupted, wantKind: KindCancelled,
			wantErr: func(err error) bool { return errors.Is(err, context.Canceled) },
			run: func(t *testing.T) (*memoryStore, Result, error) {
				store := newState("c4")
				ctx, cancel := context.WithCancel(context.Background())
				tools := &scriptTools{fn: func(ctx context.Context, _ string, _ map[string]any) (protocol.ToolResult, error) {
					cancel()
					<-ctx.Done()
					return protocol.ToolResult{}, ctx.Err()
				}}
				provider := &funcProvider{fn: func(context.Context, int, protocol.ProviderRequest) (protocol.ProviderResponse, error) {
					return shellCall("t1"), nil
				}}
				r, err := (&Loop{Provider: provider, Store: store, Tools: tools}).Run(ctx, "c4", Options{MaxSteps: 3})
				return store, r, err
			},
		},
		{
			name: "a storage error that is really the cancellation", wantStatus: StatusInterrupted, wantKind: KindCancelled,
			wantErr: func(err error) bool { return errors.Is(err, context.Canceled) },
			run: func(t *testing.T) (*memoryStore, Result, error) {
				store := newState("c5")
				fs := &faultStore{memoryStore: store}
				ctx, cancel := context.WithCancel(context.Background())
				provider := &funcProvider{fn: func(context.Context, int, protocol.ProviderRequest) (protocol.ProviderResponse, error) {
					cancel() // the run is cancelled after the answer arrived; saving it fails with the context error
					return shellCall("t1"), nil
				}}
				r, err := (&Loop{Provider: provider, Store: fs, Tools: fakeTools{}}).Run(ctx, "c5", Options{MaxSteps: 3})
				return store, r, err
			},
		},
		{
			name: "max steps", wantStatus: StatusInterrupted, wantKind: KindMaxSteps,
			wantErr: func(err error) bool { return err != nil && strings.Contains(err.Error(), "max steps") },
			run: func(t *testing.T) (*memoryStore, Result, error) {
				store := newState("m1")
				provider := &funcProvider{fn: func(context.Context, int, protocol.ProviderRequest) (protocol.ProviderResponse, error) {
					return shellCall("t"), nil
				}}
				r, err := (&Loop{Provider: provider, Store: store, Tools: fakeTools{}}).Run(context.Background(), "m1", Options{MaxSteps: 1})
				return store, r, err
			},
		},
		{
			name: "empty response", wantStatus: StatusInterrupted, wantKind: KindEmptyResponse,
			wantErr: func(err error) bool { return err != nil },
			run: func(t *testing.T) (*memoryStore, Result, error) {
				store := newState("e1")
				provider := &funcProvider{fn: func(context.Context, int, protocol.ProviderRequest) (protocol.ProviderResponse, error) {
					return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant"}}, nil
				}}
				r, err := (&Loop{Provider: provider, Store: store, Tools: fakeTools{}}).Run(context.Background(), "e1", Options{MaxSteps: 2})
				return store, r, err
			},
		},
		{
			name: "provider unavailable", wantStatus: StatusInterrupted, wantKind: KindProviderAvailability,
			wantErr: func(err error) bool { return err != nil && strings.Contains(err.Error(), "upstream down") },
			run: func(t *testing.T) (*memoryStore, Result, error) {
				store := newState("p1")
				provider := &funcProvider{fn: func(context.Context, int, protocol.ProviderRequest) (protocol.ProviderResponse, error) {
					return protocol.ProviderResponse{}, errors.New("upstream down")
				}}
				r, err := (&Loop{Provider: provider, Store: store, Tools: fakeTools{}}).Run(context.Background(), "p1", Options{MaxSteps: 2})
				return store, r, err
			},
		},
		{
			name: "storage error while saving the answer", wantStatus: StatusFailed, wantKind: KindStorage,
			wantErr: func(err error) bool { return errors.Is(err, storageErr) },
			run: func(t *testing.T) (*memoryStore, Result, error) {
				store := newState("s1")
				fs := &faultStore{memoryStore: store, failAppend: func(m protocol.Message) error {
					if m.Role == "assistant" {
						return storageErr
					}
					return nil
				}}
				provider := &funcProvider{fn: func(context.Context, int, protocol.ProviderRequest) (protocol.ProviderResponse, error) {
					return protocol.ProviderResponse{Message: protocol.Message{Content: "done"}}, nil
				}}
				r, err := (&Loop{Provider: provider, Store: fs, Tools: fakeTools{}}).Run(context.Background(), "s1", Options{MaxSteps: 2})
				return store, r, err
			},
		},
		{
			name: "storage error while saving a tool result", wantStatus: StatusFailed, wantKind: KindStorage,
			wantErr: func(err error) bool { return errors.Is(err, storageErr) },
			run: func(t *testing.T) (*memoryStore, Result, error) {
				store := newState("s2")
				fs := &faultStore{memoryStore: store, failAppend: func(m protocol.Message) error {
					if m.Role == "tool" {
						return storageErr
					}
					return nil
				}}
				provider := &funcProvider{fn: func(context.Context, int, protocol.ProviderRequest) (protocol.ProviderResponse, error) {
					return shellCall("t1"), nil
				}}
				r, err := (&Loop{Provider: provider, Store: fs, Tools: fakeTools{}}).Run(context.Background(), "s2", Options{MaxSteps: 2})
				return store, r, err
			},
		},
		{
			name: "panic in a tool", wantStatus: StatusFailed, wantKind: KindPanic,
			wantErr: func(err error) bool { return err != nil && strings.Contains(err.Error(), "panic: tool exploded") },
			run: func(t *testing.T) (*memoryStore, Result, error) {
				store := newState("k1")
				tools := &scriptTools{fn: func(context.Context, string, map[string]any) (protocol.ToolResult, error) {
					panic("tool exploded")
				}}
				provider := &funcProvider{fn: func(context.Context, int, protocol.ProviderRequest) (protocol.ProviderResponse, error) {
					return shellCall("t1"), nil
				}}
				r, err := (&Loop{Provider: provider, Store: store, Tools: tools}).Run(context.Background(), "k1", Options{MaxSteps: 2})
				return store, r, err
			},
		},
		{
			name: "panic in the provider after the run was cancelled", wantStatus: StatusFailed, wantKind: KindPanic,
			wantErr: func(err error) bool { return err != nil && strings.Contains(err.Error(), "panic: provider exploded") },
			run: func(t *testing.T) (*memoryStore, Result, error) {
				store := newState("k2")
				ctx, cancel := context.WithCancel(context.Background())
				provider := &funcProvider{fn: func(context.Context, int, protocol.ProviderRequest) (protocol.ProviderResponse, error) {
					cancel()
					panic("provider exploded")
				}}
				r, err := (&Loop{Provider: provider, Store: store, Tools: fakeTools{}}).Run(ctx, "k2", Options{MaxSteps: 2})
				return store, r, err
			},
		},
		{
			name: "completed", wantStatus: StatusCompleted, wantKind: "",
			wantErr: func(err error) bool { return err == nil },
			run: func(t *testing.T) (*memoryStore, Result, error) {
				store := newState("d1")
				provider := &funcProvider{fn: func(context.Context, int, protocol.ProviderRequest) (protocol.ProviderResponse, error) {
					return protocol.ProviderResponse{Message: protocol.Message{Content: "all done"}}, nil
				}}
				r, err := (&Loop{Provider: provider, Store: store, Tools: fakeTools{}}).Run(context.Background(), "d1", Options{MaxSteps: 2})
				if store.state.Final != "all done" {
					t.Errorf("final = %q", store.state.Final)
				}
				return store, r, err
			},
		},
		{
			name: "workflow human gate", wantStatus: StatusWaitingGate, wantKind: KindWorkflowGate,
			wantErr: func(err error) bool { return err == nil },
			run: func(t *testing.T) (*memoryStore, Result, error) {
				store := newState("g1")
				workflow := []byte(`{"id":"approval","facts":{"ready":true},"actions":{"done":{"terminal":true,"completeWhen":{"fact":"ready","equals":true},"gates":[{"id":"deny","type":"human","fact":"approved","equals":false}]}}}`)
				provider := &funcProvider{fn: func(context.Context, int, protocol.ProviderRequest) (protocol.ProviderResponse, error) {
					t.Error("the provider must not be called while a human gate is pending")
					return protocol.ProviderResponse{}, errors.New("unexpected call")
				}}
				r, err := (&Loop{Provider: provider, Store: store, Tools: fakeTools{}}).Run(context.Background(), "g1", Options{MaxSteps: 2, WorkflowJSON: workflow})
				if !r.WaitingGate {
					t.Errorf("result.WaitingGate = false")
				}
				return store, r, err
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, result, err := tc.run(t)
			if !tc.wantErr(err) {
				t.Fatalf("unexpected error: %v", err)
			}
			assertExit(t, store, result, tc.wantStatus, tc.wantKind)
		})
	}
}

func TestFinishIgnoresRunCancellationAndHasItsOwnDeadline(t *testing.T) {
	store := newState("f1")
	fs := &faultStore{memoryStore: store}
	ctx, cancel := context.WithCancel(context.Background())
	provider := &funcProvider{fn: func(ctx context.Context, _ int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		cancel()
		<-ctx.Done()
		return protocol.ProviderResponse{}, ctx.Err()
	}}
	if _, err := (&Loop{Provider: provider, Store: fs, Tools: fakeTools{}}).Run(ctx, "f1", Options{MaxSteps: 2}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if len(fs.terminalCtxs) != 1 {
		t.Fatalf("terminal writes = %d, want 1", len(fs.terminalCtxs))
	}
	terminal := fs.terminalCtxs[0]
	if terminal.Err() != nil && !errors.Is(terminal.Err(), context.Canceled) {
		t.Fatalf("terminal context err = %v", terminal.Err())
	}
	deadline, ok := terminal.Deadline()
	if !ok {
		t.Fatal("the terminal write must carry its own deadline")
	}
	if until := time.Until(deadline); until > finishTimeout {
		t.Fatalf("deadline %v away, want at most %v", until, finishTimeout)
	}
	if store.state.Status != StatusInterrupted || store.cause["kind"] != KindCancelled {
		t.Fatalf("status=%q cause=%v: the cancelled run was not recorded", store.state.Status, store.cause)
	}
}

func TestFinishFailureLeavesRunningRowAndNextRunRecovers(t *testing.T) {
	store := newState("ff1")
	diskFull := errors.New("database or disk is full")
	failing := true
	fs := &faultStore{memoryStore: store, failUpdate: func(p SessionPatch) error {
		if failing && p.Status != nil && *p.Status != StatusRunning {
			return diskFull
		}
		return nil
	}}
	var events []string
	emit := func(e Event) { events = append(events, e.Type) }

	ctx, cancel := context.WithCancel(context.Background())
	blocking := &funcProvider{fn: func(ctx context.Context, _ int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		cancel()
		<-ctx.Done()
		return protocol.ProviderResponse{}, ctx.Err()
	}}
	result, err := (&Loop{Provider: blocking, Store: fs, Tools: fakeTools{}, Emit: emit}).Run(ctx, "ff1", Options{MaxSteps: 2})
	if !errors.Is(err, context.Canceled) || !errors.Is(err, diskFull) {
		t.Fatalf("the error must carry the exit cause and the failed write, got %v", err)
	}
	if store.state.Status != StatusRunning || result.Status != StatusRunning {
		t.Fatalf("a failed Finish must leave the row running: row=%q result=%q", store.state.Status, result.Status)
	}
	for _, e := range events {
		if strings.HasPrefix(e, "session.") {
			t.Fatalf("terminal event %q published although Finish failed", e)
		}
	}

	failing = false
	completing := &funcProvider{fn: func(context.Context, int, protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		return protocol.ProviderResponse{Message: protocol.Message{Content: "recovered"}}, nil
	}}
	result, err = (&Loop{Provider: completing, Store: fs, Tools: fakeTools{}, Emit: emit}).Run(context.Background(), "ff1", Options{MaxSteps: 2})
	if err != nil {
		t.Fatal(err)
	}
	assertExit(t, store, result, StatusCompleted, "")
	if store.state.Final != "recovered" {
		t.Fatalf("final = %q", store.state.Final)
	}
}

func TestInvalidToolArgumentsAreKeptAsStringAndAnsweredWithToolError(t *testing.T) {
	cases := map[string]string{
		"truncated object": `{"command":`,
		"plain text":       `rm -rf /tmp/x`,
		"not an object":    `["a"]`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			store := newState("a1")
			tools := &scriptTools{fn: func(context.Context, string, map[string]any) (protocol.ToolResult, error) {
				return protocol.ToolResult{OK: true, Content: `{}`}, nil
			}}
			provider := &funcProvider{fn: func(_ context.Context, n int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
				if n == 1 {
					return protocol.ProviderResponse{Message: protocol.Message{ToolCalls: []protocol.ToolCall{
						{ID: "bad", Name: "shell", Arguments: json.RawMessage(raw)},
					}}}, nil
				}
				return protocol.ProviderResponse{Message: protocol.Message{Content: "fixed it"}}, nil
			}}
			result, err := (&Loop{Provider: provider, Store: store, Tools: tools}).Run(context.Background(), "a1", Options{MaxSteps: 3})
			if err != nil {
				t.Fatal(err)
			}
			assertExit(t, store, result, StatusCompleted, "")
			if tools.executed != 0 {
				t.Fatalf("a call with invalid arguments was executed %d times", tools.executed)
			}
			// goal, assistant call, tool error, final answer
			if len(store.messages) != 4 {
				t.Fatalf("messages = %+v", store.messages)
			}
			stored := store.messages[1].ToolCalls[0].Arguments
			if !json.Valid(stored) {
				t.Fatalf("stored arguments are not valid JSON: %s", stored)
			}
			if raw != `["a"]` {
				var text string
				if err := json.Unmarshal(stored, &text); err != nil || text != raw {
					t.Fatalf("stored arguments = %s, want the raw text %q as a JSON string", stored, raw)
				}
			}
			answer := store.messages[2]
			if answer.Role != "tool" || answer.ToolCallID != "bad" || !strings.Contains(answer.Content, "invalid tool arguments") {
				t.Fatalf("tool answer = %+v", answer)
			}
			if !json.Valid([]byte(answer.Content)) {
				t.Fatalf("tool error is not valid JSON: %s", answer.Content)
			}
		})
	}
}
