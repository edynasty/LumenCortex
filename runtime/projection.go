package runtime

import (
	"context"
	"fmt"

	"github.com/edynasty/LumenCortex/internal/session"
)

// projectRun is the Start side of DATA_AND_FAILURES §5 steps 3-4, run under the session's run lock
// after Reconcile. It returns the transcript seqs the run withholds. A session no revocation reached
// returns none. A session with a fence no Start has projected yet is projected now: when the thread
// can be projected the fences are acknowledged, so this run's requests are admitted again with the
// withheld rows replaced; when it cannot, the Start is refused with session.ErrFenced and nothing is
// written - the thread stays isolated, and the user continues in a new session (a clean task), never
// in this one with the old content resent. A revocation that commits between the read and the
// acknowledgement refuses this Start; the next one reads it.
//
// The run starts a new turn as every run does (the turn rule of internal/agent/send.go): no echo of
// an earlier run is held in memory, so none is sent.
func (e *Engine) projectRun(ctx context.Context, sessionID string) ([]int64, error) {
	p, err := e.store.Projection(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if !p.Open {
		return p.Withheld, nil
	}
	if p.Isolated != "" {
		e.events.publish("session.isolated", sessionID, map[string]any{"epoch": p.Epoch, "reason": p.Isolated})
		return nil, fmt.Errorf("%w: the thread cannot be projected (%s); continue in a new session", session.ErrFenced, p.Isolated)
	}
	if err := e.store.AcknowledgeProjection(ctx, sessionID, p.Epoch); err != nil {
		return nil, err
	}
	e.events.publish("session.projected", sessionID, map[string]any{"epoch": p.Epoch, "withheld": len(p.Withheld)})
	return p.Withheld, nil
}
