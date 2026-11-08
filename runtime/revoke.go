package runtime

import (
	"context"
	"errors"
	"fmt"

	"github.com/edynasty/LumenCortex/internal/agent"
	"github.com/edynasty/LumenCortex/internal/session"
)

// ErrUnsupportedSource reports a revocation of a source kind R1a does not revoke: the unit of
// revocation is a session (DATA_AND_FAILURES §5).
var ErrUnsupportedSource = errors.New("only a session can be revoked")

// ErrNoRevocation reports an epoch no revocation was committed with.
var ErrNoRevocation = session.ErrNoRevocation

// SourceSession is the source kind RevokeSource accepts.
const SourceSession = session.SourceSession

// ExposureReport is a revocation's report: what it fenced and the exposure it found at its commit
// (session.Revocation), with the state each in-flight request has now. A request in InFlight was
// admitted before the revocation and may have been sent, even after it: InFlightNow says whether it
// was answered, became unknown or failed since, or is still admitted (its run is in another process,
// or not settled yet). Nothing in the report is content.
type ExposureReport struct {
	session.Revocation
	InFlightNow map[string]string `json:"inFlightNow"`
}

// RevokeSource revokes a source: the session ref (kind SourceSession), every session derived from it
// through recorded edges, and the children they started (session.Store.RevokeSession). The commit is
// the linearization point: after it no new request or side-effect tool call of a fenced session is
// admitted, and no new edge citing one is stored. Then the runs of fenced sessions executing in this
// Engine are cancelled and waited for. A run in another process is not cancelled: its next admission
// is refused, and what it admitted before is in the report. reason is a short machine code.
func (e *Engine) RevokeSource(ctx context.Context, kind, ref, reason string) (ExposureReport, error) {
	if kind != SourceSession {
		return ExposureReport{}, fmt.Errorf("%w: %q", ErrUnsupportedSource, kind)
	}
	rev, err := e.store.RevokeSession(ctx, ref, reason)
	if err != nil {
		return ExposureReport{}, err
	}
	fenced := make(map[string]bool, len(rev.Sessions))
	for _, id := range rev.Sessions {
		fenced[id] = true
	}
	// Sessions holds every fenced session unless the walk expanded; then every run is fenced.
	if err := e.cancelRuns(ctx, func(id string) bool { return rev.Expanded || fenced[id] }); err != nil {
		return ExposureReport{}, err
	}
	if rev.Expanded {
		if err := e.closeEval(); err != nil {
			return ExposureReport{}, err
		}
	} else {
		for _, id := range rev.Sessions {
			if err := e.closeEvalOwner(id); err != nil {
				return ExposureReport{}, err
			}
		}
	}
	e.events.publish("source.revoked", ref, map[string]any{"epoch": rev.Epoch, "sessions": rev.SessionCount, "expanded": rev.Expanded})
	return e.ExposureReport(ctx, rev.Epoch)
}

// ExposureReport reads the report of the revocation committed with epoch.
func (e *Engine) ExposureReport(ctx context.Context, epoch int64) (ExposureReport, error) {
	rev, err := e.store.Revocation(ctx, epoch)
	if err != nil {
		return ExposureReport{}, err
	}
	states, err := e.store.RequestStates(ctx, rev.InFlight)
	if err != nil {
		return ExposureReport{}, err
	}
	now := make(map[string]string, len(states))
	for id, st := range states {
		now[id] = string(st)
	}
	return ExposureReport{Revocation: rev, InFlightNow: now}, nil
}

// trackedRun is one loop executing in this Engine.
type trackedRun struct {
	cancel context.CancelFunc
	done   chan struct{}
	debug  *agent.DebugControl
}

// trackRun registers preparation through final cleanup. The caller holds the session run lock,
// so there is at most one entry per session. Closing cancels a late arrival before it does work.
func (e *Engine) trackRun(ctx context.Context, sessionID string) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	run := &trackedRun{cancel: cancel, done: make(chan struct{})}
	e.runsMu.Lock()
	if e.closing {
		cancel()
	}
	if e.runs == nil {
		e.runs = map[string]*trackedRun{}
	}
	e.runs[sessionID] = run
	e.runsMu.Unlock()
	return ctx, func() {
		e.runsMu.Lock()
		if e.runs[sessionID] == run {
			delete(e.runs, sessionID)
		}
		e.runsMu.Unlock()
		if run.debug != nil {
			run.debug.Close()
		}
		cancel()
		close(run.done)
	}
}

// cancelRuns cancels every tracked run whose session match selects and waits until each has ended,
// or until ctx is done.
func (e *Engine) cancelRuns(ctx context.Context, match func(sessionID string) bool) error {
	e.runsMu.Lock()
	var runs []*trackedRun
	for id, run := range e.runs {
		if match(evalRunOwner(id)) {
			runs = append(runs, run)
		}
	}
	e.runsMu.Unlock()
	for _, run := range runs {
		run.cancel()
	}
	for _, run := range runs {
		select {
		case <-run.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
