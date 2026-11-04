package runtime

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/taskstate"
	"github.com/edynasty/LumenCortex/protocol"
)

type lifecycleProvider struct {
	entered  chan struct{}
	calls    atomic.Int64
	deadline chan bool
	fail     bool
}

func (*lifecycleProvider) Model() string { return "lifecycle" }
func (p *lifecycleProvider) Complete(ctx context.Context, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	n := p.calls.Add(1)
	if n == 1 {
		if p.deadline != nil {
			_, ok := ctx.Deadline()
			p.deadline <- ok
		}
		close(p.entered)
	}
	if p.fail {
		return protocol.ProviderResponse{}, &protocol.ProviderError{Class: protocol.ClassAvailability, Err: errors.New("unavailable")}
	}
	<-ctx.Done()
	return protocol.ProviderResponse{}, ctx.Err()
}
func awaitLifecycle(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal("provider was not reached")
	}
}

func TestSupervisorCallerCancellationAndDeadline(t *testing.T) {
	for _, mode := range []string{"caller", "host", "deadline", "close"} {
		t.Run(mode, func(t *testing.T) {
			engine, _ := notebookEngine(t)
			s, err := NewRunSupervisor(engine)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			p := &lifecycleProvider{entered: make(chan struct{}), deadline: make(chan bool, 1)}
			id := newParentSession(t, engine, p)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			opts := AgentOptions{}
			if mode == "deadline" {
				deadline := time.Now().Add(200 * time.Millisecond)
				opts.Deadline = &deadline
			}
			if _, err := s.Start(ctx, id, opts); err != nil {
				t.Fatal(err)
			}
			awaitLifecycle(t, p.entered)
			if got := <-p.deadline; got != (mode == "deadline") {
				t.Fatalf("implicit/missing deadline=%v", got)
			}
			switch mode {
			case "caller":
				cancel()
			case "host":
				if !s.Cancel(id) {
					t.Fatal("host cancel missed run")
				}
			case "close":
				s.Close()
			}
			waitInactive(t, s, id)
			row, err := engine.store.Get(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			kind := "cancelled"
			if mode == "deadline" {
				kind = "deadline"
			}
			if row.Status != "interrupted" || !strings.Contains(string(row.Error), kind) || p.calls.Load() != 1 {
				t.Fatalf("row=%+v requests=%d", row, p.calls.Load())
			}
		})
	}
}

func TestCancelledChildRetryWaitSendsNothingMore(t *testing.T) {
	engine, _ := notebookEngine(t)
	s, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p := &lifecycleProvider{entered: make(chan struct{}), fail: true}
	id := newParentSession(t, engine, p)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &subagentController{supervisor: s, parentID: id, parentOpts: AgentOptions{Retry: &config.Retry{BaseDelayMs: 60000, MaxDelayMs: 60000}}}
	v, err := c.Spawn(ctx, "inspect", "")
	if err != nil {
		t.Fatal(err)
	}
	awaitLifecycle(t, p.entered)
	cancel()
	child := v.(SubagentNode).SessionID
	waitInactive(t, s, child)
	row, err := engine.store.Get(context.Background(), child)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != "interrupted" || !strings.Contains(string(row.Error), "cancelled") || p.calls.Load() != 1 {
		t.Fatalf("row=%+v calls=%d", row, p.calls.Load())
	}
}

func TestHostChildExecutionOverride(t *testing.T) {
	for _, limit := range []int{-1, 3} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			engine, _ := notebookEngine(t)
			p := &executionRetryProvider{}
			for i := range 10 {
				p.responses = append(p.responses, toolCallResponse(fmt.Sprint(i), "list_dir", `{"path":"."}`))
			}
			p.responses = append(p.responses, finalResponse("child done"))
			s, err := NewRunSupervisor(engine)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			id := newParentSession(t, engine, p)
			c := &subagentController{supervisor: s, parentID: id, parentOpts: AgentOptions{MaxSteps: 1, ChildExecution: &ChildExecutionOptions{MaxSteps: limit}}}
			v, err := c.Spawn(context.Background(), "inspect", "")
			if err != nil {
				t.Fatal(err)
			}
			child := v.(SubagentNode).SessionID
			waitInactive(t, s, child)
			row, err := engine.store.Get(context.Background(), child)
			if err != nil {
				t.Fatal(err)
			}
			if limit > 0 {
				if row.Status != "interrupted" || p.requests != limit {
					t.Fatalf("explicit child: %+v n=%d", row, p.requests)
				}
			} else if row.Status != "completed" || p.requests != 11 {
				t.Fatalf("unlimited override: %+v n=%d", row, p.requests)
			}
		})
	}
}

func TestChildDeadlineInheritsAndCannotExtendParent(t *testing.T) {
	for _, kind := range []string{"explore", "worker"} {
		// Engine migration/configuration is not the execution under test. Start
		// the deadline afterward so race instrumentation cannot consume it here.
		engine, id := notebookEngine(t)
		deadline := time.Now().Add(time.Second)
		later := deadline.Add(time.Hour)
		parent := AgentOptions{Deadline: &deadline, ChildExecution: &ChildExecutionOptions{Deadline: &later}}
		opts := executionOptions(parent, "", nil, "", "worker")
		if kind == "explore" {
			opts = (&subagentController{parentOpts: parent}).childOptions("", "explore")
		}
		if opts.Deadline == nil || !opts.Deadline.Equal(deadline) {
			t.Fatalf("%s child extended parent deadline: %v, want %v", kind, opts.Deadline, deadline)
		}
		p := &lifecycleProvider{entered: make(chan struct{})}
		result, err := engine.runAgentWith(context.Background(), id, p, opts)
		row, getErr := engine.store.Get(context.Background(), id)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if err == nil || result.Status != "interrupted" || !strings.Contains(string(row.Error), "deadline") || p.calls.Load() != 1 {
			t.Fatalf("%s child deadline: %+v err=%v cause=%s", kind, result, err, row.Error)
		}
	}
}

func TestCancelStopsForegroundToolAndOwnedBackgroundJob(t *testing.T) {
	engine, _ := trustedProject(t, `{}`, false)
	s, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	waiting := make(chan struct{})
	var calls atomic.Int64
	p := &exitProvider{fn: func(ctx context.Context, n int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		calls.Add(1)
		if n == 1 {
			return toolCalls(tc("bg", "bash", `{"command":"sleep 30","mode":"background"}`)), nil
		}
		if n == 2 {
			return toolCalls(tc("fg", "bash", `{"command":"echo running; sleep 30"}`)), nil
		}
		return finalResponse("should not request"), nil
	}}
	events, stop := engine.Events(128)
	defer stop()
	go func() {
		for e := range events {
			if e.Type == "tool.output" {
				select {
				case <-waiting:
				default:
					close(waiting)
				}
				return
			}
		}
	}()
	id := newParentSession(t, engine, p)
	if _, err := s.Start(context.Background(), id, AgentOptions{Policy: "full"}); err != nil {
		t.Fatal(err)
	}
	awaitLifecycle(t, waiting)
	if !s.Cancel(id) {
		t.Fatal("cancel missed run")
	}
	waitInactive(t, s, id)
	jobs, err := engine.Jobs(context.Background(), JobQuery{SessionID: id})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].State != session.JobCancelled || calls.Load() != 2 {
		t.Fatalf("jobs=%+v requests=%d", jobs, calls.Load())
	}
}

func TestPreparationStopsWithEngineCloseOrDeadline(t *testing.T) {
	for _, mode := range []string{"close", "deadline", "supervisor-deadline"} {
		t.Run(mode, func(t *testing.T) {
			engine := openTestEngine(t)
			main := &answering{final: "must not run"}
			judge := &blockingJudge{started: make(chan struct{})}
			bindRef(engine, "b0", main)
			bindRef(engine, "b1", judge)
			useGlobalConfig(t, autoConfig("test/b0", "test/b1", "high", ""))
			id := newTestSession(t, engine, "inspect").ID
			opts := AgentOptions{Effort: config.EffortAuto}
			if mode != "close" {
				d := time.Now().Add(300 * time.Millisecond)
				opts.Deadline = &d
			}
			var s *RunSupervisor
			if mode == "supervisor-deadline" {
				var err error
				s, err = NewRunSupervisor(engine)
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
			}
			finished := make(chan error, 1)
			go func() {
				var err error
				if s != nil {
					_, err = s.Start(context.Background(), id, opts)
				} else {
					_, err = engine.RunAgent(context.Background(), id, opts)
				}
				finished <- err
			}()
			awaitLifecycle(t, judge.started)
			if mode == "close" {
				if err := engine.Close(); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case err := <-finished:
				want := context.DeadlineExceeded
				if mode == "close" {
					want = context.Canceled
				}
				if !errors.Is(err, want) {
					t.Fatalf("preparation error=%v, want %v", err, want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("preparation survived its control boundary")
			}
			if main.calls.Load() != 0 || judge.calls.Load() != 1 {
				t.Fatalf("main=%d judge=%d", main.calls.Load(), judge.calls.Load())
			}
		})
	}
}

// A parent without a step limit can start more children over its life than any display bound. The
// child created after the hundredth still counts: it is an open task obligation, a running child
// against the parent's concurrency limit, and a dependent that blocks the parent's deletion.
func TestChildBeyondTheHundredthStaysVisible(t *testing.T) {
	engine, parent := notebookEngine(t)
	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Hour)
	var newest string
	for i := range MaxSubagentTreeNodes + 1 {
		id, err := newSessionID()
		if err != nil {
			t.Fatal(err)
		}
		status := "completed"
		if i == MaxSubagentTreeNodes {
			status, newest = "running", id
		}
		at := base.Add(time.Duration(i) * time.Second)
		row := session.Session{ID: id, CreatedAt: at, UpdatedAt: at, Status: status, Goal: "task", Metadata: []byte("{}")}
		if err := engine.store.CreateWithMessage(ctx, row, protocol.Message{Role: "user", Content: "task"}); err != nil {
			t.Fatal(err)
		}
		if err := engine.store.AddRelation(ctx, session.Relation{ParentSessionID: parent, ChildSessionID: id, Kind: RelationExecution, CreatedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	if active, err := supervisor.activeChildren(ctx, parent); err != nil || active != 1 {
		t.Fatalf("active children = %d, %v; want the newest running child counted", active, err)
	}
	info, err := engine.TaskState(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(info.Obligations, taskstate.ObligationTasks) {
		t.Fatalf("obligations = %v; the running child %s is not an open task", info.Obligations, newest)
	}
	preview, err := engine.DeletionPreview(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(preview.Dependents, func(d SessionDependent) bool { return d.SessionID == newest }) {
		t.Fatalf("deletion preview lists %d dependents, not the newest child", len(preview.Dependents))
	}
}
