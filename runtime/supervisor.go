package runtime

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/edynasty/LumenCortex/internal/resource"
)

var (
	ErrRunSupervisorClosed = errors.New("run supervisor is closed")
	ErrAgentLimitExceeded  = resource.ErrAgentLimitExceeded
)

type ActiveRun struct {
	SessionID       string    `json:"sessionId"`
	StartedAt       time.Time `json:"startedAt"`
	CancelRequested bool      `json:"cancelRequested,omitempty"`
}

type managedRun struct {
	cancel          context.CancelFunc
	done            chan struct{}
	startedAt       time.Time
	cancelRequested bool
	parentID        string
	ownerRun        *managedRun
	releaseAgent    func()
}

// RunSupervisor is the process-local source of truth for active agent executions
// owned by an Engine. Durable session lifecycle remains in SQLite; this type
// answers the separate question "is there a live goroutine executing this
// session in this process right now?".
type RunSupervisor struct {
	mu     sync.RWMutex
	engine *Engine
	runs   map[string]*managedRun
	closed bool
}

func NewRunSupervisor(engine *Engine) (*RunSupervisor, error) {
	if engine == nil {
		return nil, errors.New("engine is required")
	}
	sup := &RunSupervisor{
		engine: engine,
		runs:   map[string]*managedRun{},
	}
	// Agent schedules start their runs here (ADR-0019 §4): without a registered supervisor they stay
	// due, and registering one retries them.
	engine.setAgentRunner(sup)
	return sup, nil
}

// Start begins an agent run and returns once its goroutine has started.
//
//	take the run lock → Reconcile → validate → Start transaction (opts.Input, identity, running) → loop
//
// When another Engine (in this process or another) holds the session's run lock,
// including this supervisor's own active run, Start returns ErrRunHeld and writes
// nothing. The lock is held until the run has persisted its terminal status, and
// is released on every exit path.
func (s *RunSupervisor) Start(
	ctx context.Context,
	sessionID string,
	opts AgentOptions,
) (info SessionInfo, err error) {
	ctx, cancel := executionContext(ctx, opts)
	handedOff := false
	defer func() {
		if !handedOff {
			cancel()
		}
	}()

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return SessionInfo{}, ErrRunSupervisorClosed
	}
	s.mu.Unlock()

	lock, err := s.engine.acquireRun(ctx, sessionID)
	if err != nil {
		return SessionInfo{}, err
	}
	ctx, done := s.engine.trackRun(ctx, sessionID)
	defer func() {
		if !handedOff {
			done()
		}
	}()
	if err := ctx.Err(); err != nil {
		_ = lock.Release()
		return SessionInfo{}, err
	}
	if opts, err = s.engine.constrainChild(ctx, sessionID, opts); err != nil {
		_ = lock.Release()
		return SessionInfo{}, err
	}
	// The worktree lease (ADR-0005 §2) is held from here until the run has left the
	// supervisor, so no process can remove the worktree while the run uses it.
	_, lease, err := s.engine.leaseWorkspace(ctx, sessionID)
	if err != nil {
		_ = lock.Release()
		return SessionInfo{}, err
	}
	// The limits are read at every Start, so a saved change applies to the next run. A lower limit
	// never stops a running agent; a Start beyond it is refused at once, never queued (ADR-0008).
	releaseAgent, err := s.acquireAgent()
	if err != nil {
		_ = lease.Release()
		_ = lock.Release()
		return SessionInfo{}, err
	}
	defer func() {
		if !handedOff {
			releaseAgent()
			_ = lease.Release()
			_ = lock.Release()
		}
	}()

	runCtx := ctx
	run := &managedRun{
		cancel:       cancel,
		done:         make(chan struct{}),
		startedAt:    time.Now().UTC(),
		releaseAgent: releaseAgent,
		parentID:     opts.parentSessionID,
		ownerRun:     opts.ownerRun,
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		cancel()
		return SessionInfo{}, ErrRunSupervisorClosed
	}
	s.runs[sessionID] = run
	s.mu.Unlock()
	defer func() {
		if !handedOff {
			s.mu.Lock()
			if s.runs[sessionID] == run {
				delete(s.runs, sessionID)
			}
			s.mu.Unlock()
			cancel()
			close(run.done)
		}
	}()

	// Child lifetimes belong to this invocation, not every historical child of the session.
	opts.ownerRun = run
	if !opts.disableSubagents && opts.subagents == nil {
		opts.subagents = &subagentController{
			supervisor: s,
			parentID:   sessionID,
			parentOpts: opts,
		}
	}
	if opts.Delegation == DelegationIsolated && opts.parentSessionID == "" && opts.tasks == nil {
		opts.tasks = &taskStarter{supervisor: s, parentID: sessionID, parentOpts: opts}
	}
	prepared, err := s.engine.prepareRun(runCtx, sessionID, opts)
	if err != nil {
		return SessionInfo{}, err
	}
	prepared.finishOwned = func() { s.stopChildren(run) }
	// The Start transaction has committed: the session is durably running. Nothing
	// fallible may sit between here and the goroutine that owns the run, so the
	// returned info comes from the transaction itself.
	info = prepared.info
	handedOff = true

	s.engine.events.publish("run.started", sessionID, map[string]any{
		"startedAt": run.startedAt,
	})

	go func() {
		defer done()
		var runErr error
		// Cleanup order: the loop has already persisted the terminal status and
		// published its terminal event; the lock is released before the run leaves
		// the supervisor, so a host that sees the run stopped can Start again.
		defer func() {
			_ = lease.Release()
			_ = lock.Release()
			s.mu.Lock()
			if current, ok := s.runs[sessionID]; ok && current == run {
				delete(s.runs, sessionID)
			}
			s.mu.Unlock()
			run.releaseAgent()
			cancel()
			close(run.done)

			data := map[string]any{}
			if runErr != nil {
				data["error"] = runErr.Error()
			}
			s.engine.events.publish("run.stopped", sessionID, data)
			if opts.parentSessionID != "" {
				s.engine.checkpointSubagentStop(opts.parentSessionID, sessionID, runErr)
			}
		}()
		_, runErr = prepared.execute(runCtx)
	}()

	return info, nil
}

func (s *RunSupervisor) acquireAgent() (func(), error) {
	if _, err := s.engine.concurrency(); err != nil {
		return nil, err
	}
	return s.engine.resources.AcquireAgent()
}

// checkChildLimit refuses another child of parentID once its running children, of both kinds,
// reach concurrency.maxChildrenPerParent.
func (s *RunSupervisor) checkChildLimit(ctx context.Context, parentID string) error {
	limits, err := s.engine.concurrency()
	if err != nil {
		return err
	}
	active, err := s.activeChildren(ctx, parentID)
	if err != nil {
		return err
	}
	if active >= limits.MaxChildrenPerParent {
		return fmt.Errorf("%w (%d running, limit %d): let a running child finish (wait_subagent waits for a read-only one), then retry",
			ErrSubagentLimitExceeded, active, limits.MaxChildrenPerParent)
	}
	return nil
}

// stopChildren settles owned child runs before publishing the parent's stopped state.
func (s *RunSupervisor) stopChildren(owner *managedRun) {
	s.mu.Lock()
	var children []*managedRun
	for _, run := range s.runs {
		if run.ownerRun == owner {
			run.cancelRequested = true
			run.cancel()
			children = append(children, run)
		}
	}
	s.mu.Unlock()
	for _, run := range children {
		<-run.done
	}
}

// Cancel stops this session and its active descendants, even if the parent has already ended.
// Context cancellation also reaches children started concurrently from the parent's run context.
func (s *RunSupervisor) Cancel(sessionID string) bool {
	s.mu.Lock()
	ids := map[string]bool{sessionID: true}
	for changed := true; changed; {
		changed = false
		for id, run := range s.runs {
			if !ids[id] && ids[run.parentID] {
				ids[id], changed = true, true
			}
		}
	}
	var cancels []context.CancelFunc
	for id, run := range s.runs {
		if ids[id] {
			run.cancelRequested = true
			cancels = append(cancels, run.cancel)
		}
	}
	s.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	return len(cancels) > 0
}

func (s *RunSupervisor) Active(sessionID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.runs[sessionID]
	return ok
}

func (s *RunSupervisor) Runs() []ActiveRun {
	s.mu.RLock()
	out := make([]ActiveRun, 0, len(s.runs))
	for sessionID, run := range s.runs {
		out = append(out, ActiveRun{
			SessionID:       sessionID,
			StartedAt:       run.startedAt,
			CancelRequested: run.cancelRequested,
		})
	}
	s.mu.RUnlock()

	sort.Slice(out, func(i, j int) bool {
		if out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].SessionID < out[j].SessionID
		}
		return out[i].StartedAt.Before(out[j].StartedAt)
	})
	return out
}

func (s *RunSupervisor) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	runs := make([]*managedRun, 0, len(s.runs))
	for _, run := range s.runs {
		run.cancelRequested = true
		run.cancel()
		runs = append(runs, run)
	}
	s.mu.Unlock()

	for _, run := range runs {
		<-run.done
	}
	s.engine.clearAgentRunner(s)
}
