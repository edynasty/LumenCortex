package evalruntime

import (
	"context"
	"fmt"
	"time"

	"github.com/edynasty/LumenCortex/evalapi"
)

// AssertBoundary inspects only the worker already owned by this manager. It
// cannot inspect a caller-selected PID and never accepts worker-reported proof.
func (m *Manager) AssertBoundary(ctx context.Context, sessionID, owner string, binding evalapi.BoundaryContext) (evalapi.BoundaryReport, error) {
	if err := m.ensureOpen(); err != nil {
		return evalapi.BoundaryReport{}, err
	}
	if err := binding.Validate(); err != nil {
		return evalapi.BoundaryReport{}, err
	}
	s, err := m.session(sessionID)
	if err != nil {
		return evalapi.BoundaryReport{}, err
	}
	if owner != s.ownerID {
		return evalapi.BoundaryReport{}, evalapi.ErrDenied
	}
	s.mu.Lock()
	w := s.worker
	generation := s.generation
	s.mu.Unlock()
	if w == nil {
		return evalapi.BoundaryReport{}, fmt.Errorf("boundary: no live worker")
	}
	report := m.inspectBoundary(ctx, w, binding)
	s.mu.Lock()
	same := s.worker == w && s.generation == generation && !s.closed
	s.mu.Unlock()
	if !same {
		report.Checks["generation"] = evalapi.BoundaryCheck{Status: evalapi.BoundaryFail, Evidence: "worker replaced during observation"}
	}
	report.Decide()
	return report, nil
}

func newBoundaryReport(binding evalapi.BoundaryContext) evalapi.BoundaryReport {
	r := evalapi.BoundaryReport{SchemaVersion: evalapi.BoundarySchemaVersion, Context: binding, CheckedAt: time.Now().UTC(), Checks: make(map[string]evalapi.BoundaryCheck)}
	for _, name := range evalapi.BoundaryRequiredChecks() {
		r.Checks[name] = evalapi.BoundaryCheck{Status: evalapi.BoundaryUnknown, Evidence: "not observed"}
	}
	return r
}

func (m *Manager) admitBoundary(ctx context.Context, w *worker, requestID string) error {
	if m.opts.Boundary == nil {
		return nil
	}
	binding := *m.opts.Boundary
	binding.RequestID = requestID
	report := m.inspectBoundary(ctx, w, binding)
	report.Decide()
	m.emit(evalapi.Event{Type: "boundary_checked", SessionID: w.s.id, OwnerID: w.s.ownerID, RequestID: requestID, Generation: w.generation, Data: report})
	return report.RequirePass(binding)
}
