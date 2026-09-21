package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/edynasty/LumenCortex/internal/resource"
	"github.com/edynasty/LumenCortex/protocol"
)

var (
	ErrRunAlreadyActive     = errors.New("agent run is already active for this session")
	ErrRunSupervisorClosed  = errors.New("run supervisor is closed")
	ErrAgentLimitExceeded   = resource.ErrAgentLimitExceeded
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
	return &RunSupervisor{
		engine: engine,
		runs:   map[string]*managedRun{},
	}, nil
}

func (s *RunSupervisor) Start(
	ctx context.Context,
	sessionID string,
	provider protocol.Provider,
	opts AgentOptions,
) (SessionInfo, error) {
	if provider == nil {
		return SessionInfo{}, errors.New("provider is required")
	}
	_, info, err := s.engine.Session(ctx, sessionID)
	if err != nil {
		return SessionInfo{}, err
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return SessionInfo{}, ErrRunSupervisorClosed
	}
	if _, ok := s.runs[sessionID]; ok {
		s.mu.Unlock()
		return SessionInfo{}, ErrRunAlreadyActive
	}
	releaseAgent, err := s.engine.resources.AcquireAgent()
	if err != nil {
		s.mu.Unlock()
		return SessionInfo{}, err
	}

	runCtx, cancel := context.WithCancel(context.Background())
	run := &managedRun{
		cancel:       cancel,
		done:         make(chan struct{}),
		startedAt:    time.Now().UTC(),
		releaseAgent: releaseAgent,
	}
	s.runs[sessionID] = run
	s.mu.Unlock()

	info.Status = "running"
	if info.Model == "" {
		info.Model = provider.Model()
	}
	if info.Provider == "" && opts.ProviderName != "" {
		info.Provider = opts.ProviderName
	}

	s.engine.events.publish("run.started", sessionID, map[string]any{
		"startedAt": run.startedAt,
	})

	go func() {
		_, runErr := s.engine.RunAgent(runCtx, sessionID, provider, opts)

		s.mu.Lock()
		if current, ok := s.runs[sessionID]; ok && current == run {
			delete(s.runs, sessionID)
		}
		s.mu.Unlock()
		run.releaseAgent()
		close(run.done)

		data := map[string]any{}
		if runErr != nil {
			data["error"] = runErr.Error()
		}
		s.engine.events.publish("run.stopped", sessionID, data)
	}()

	return info, nil
}

func (s *RunSupervisor) Cancel(sessionID string) bool {
	s.mu.Lock()
	run := s.runs[sessionID]
	if run == nil {
		s.mu.Unlock()
		return false
	}
	run.cancelRequested = true
	cancel := run.cancel
	s.mu.Unlock()

	cancel()
	return true
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
}


func (e *Engine) RecoverStaleRuns(ctx context.Context) (int64, error) {
	errorJSON, err := json.Marshal(map[string]any{
		"message": "previous process ended while the agent run was active",
		"kind":    "stale_run_recovered",
		"at":      time.Now().UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return 0, err
	}
	count, err := e.store.InterruptRunning(ctx, errorJSON)
	if err != nil {
		return 0, err
	}
	if count > 0 {
		e.events.publish("run.recovered_stale", "", map[string]any{"count": count})
	}
	return count, nil
}
