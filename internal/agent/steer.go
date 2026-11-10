package agent

import (
	"context"
	"fmt"

	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/taskstate"
	"github.com/edynasty/LumenCortex/internal/window"
	"github.com/edynasty/LumenCortex/protocol"
)

// Push delivery of the Hub control plane's host inputs into a run's request window (Hub §3.3,
// collaboration §4). Both inputs share one injection point so one rule governs the window the run's
// model actually reads:
//
//   - free-text steer: an instruction the host queued for this run; it becomes a user row;
//   - worker messages: root-scoped messages another agent sent; they become user rows too, and the
//     delivery is recorded so an unacked message is redelivered at most once per run.
//
// The point is called at every step start and, again, when a run is released from its
// BeforeModelCall pause: a steer queued behind the pause must reach the request the pause guarded, not
// the one after it (F-STEER-1).

// SteerStore is the durable steer inbox of the run's store. A Store without it injects no steer.
type SteerStore interface {
	PendingSteers(ctx context.Context, sessionID string, limit int) ([]session.SteerMessage, error)
	MarkSteerApplied(ctx context.Context, sessionID string, seq, appliedSeq int64) error
}

// WorkerMessageStore is the root's push message queue. A Store without it injects no message.
type WorkerMessageStore interface {
	PendingWorkerMessages(ctx context.Context, root, recipient, runID string, limit int) ([]session.WorkerMessage, error)
	MarkWorkerMessagesDelivered(ctx context.Context, root, recipient, runID string, messages []session.DeliveredWorkerMessage) error
}

// Bounds of one injection: a window carries at most this many steers and messages, so a flood cannot
// crowd out the transcript. Everything beyond stays queued for the next request.
const (
	maxSteersPerWindow   = 8
	maxMessagesPerWindow = 4
)

// applyPendingInputs applies the host inputs queued since the last read and reports whether the request
// window must be rebuilt. in is nil at a step start, where the window is built from the refreshed run
// state afterwards; it is the window being prepared when a run was paused at BeforeModelCall.
func (l *Loop) applyPendingInputs(ctx context.Context, sessionID, goal string, opts Options, r *run, in *window.Input) (bool, error) {
	applied, err := l.applyInbox(ctx, sessionID, r)
	if err != nil {
		return false, err
	}
	changed, err := l.applySteers(ctx, sessionID, r)
	if err != nil {
		return false, err
	}
	changed = changed || applied
	messages, err := l.applyWorkerMessages(ctx, sessionID, opts, r)
	if err != nil {
		return false, err
	}
	changed = changed || messages
	if !changed || in == nil {
		return changed, nil
	}
	// The request is built from the run's state, so both parts of the window are rebuilt: the Task
	// State block (a brief or resolution may have changed) and the transcript (the aside rows).
	in.State = protocol.Message{Role: "user", Content: taskstate.Render(goal, r.ts, r.wf)}
	in.Transcript = r.transcript
	return true, nil
}

// applySteers writes each queued steer message into the transcript as a bounded user aside and records
// the row it became. Application order is the inbox sequence, so several concurrent steers keep the
// order the host queued them in.
func (l *Loop) applySteers(ctx context.Context, sessionID string, r *run) (bool, error) {
	store, ok := l.Store.(SteerStore)
	if !ok {
		return false, nil
	}
	pending, err := store.PendingSteers(ctx, sessionID, maxSteersPerWindow)
	if err != nil || len(pending) == 0 {
		return false, err
	}
	for _, m := range pending {
		row := protocol.Message{Role: "user", Origin: protocol.OriginSteer, Content: protocol.SteerHeader + "\n" + m.Body}
		seq, _, err := l.Store.AppendMessage(ctx, sessionID, row, nil)
		if err != nil {
			return false, err
		}
		r.appendRow(seq, row)
		if err := store.MarkSteerApplied(ctx, sessionID, m.Seq, seq); err != nil {
			// The row is in the transcript; a failed mark would redeliver it at the next window, so the
			// run stops rather than duplicating the host's instruction.
			return false, fmt.Errorf("record steer %d applied: %w", m.Seq, err)
		}
		l.emit("agent.steer.applied", sessionID, map[string]any{
			"steerSeq": m.Seq, "requestSeq": seq, "commandId": m.CommandID,
		})
	}
	return true, nil
}

// applyWorkerMessages writes the root's undelivered messages for this recipient into the transcript as
// one bounded aside each and records the delivery. The recipient does not call worker_inbox for them;
// an explicit inbox read still works and acknowledges.
func (l *Loop) applyWorkerMessages(ctx context.Context, sessionID string, opts Options, r *run) (bool, error) {
	store, ok := l.Store.(WorkerMessageStore)
	if !ok || opts.WorkerRoot == "" {
		return false, nil
	}
	pending, err := store.PendingWorkerMessages(ctx, opts.WorkerRoot, sessionID, opts.RunID, maxMessagesPerWindow)
	if err != nil || len(pending) == 0 {
		return false, err
	}
	delivered := make([]session.DeliveredWorkerMessage, 0, len(pending))
	for _, m := range pending {
		body := m.Body
		if m.Withdrawn {
			body = "[withdrawn: the sender's content is no longer readable]"
		}
		row := protocol.Message{Role: "user", Origin: protocol.OriginWorkerMessage,
			Content: fmt.Sprintf("%s From %s, message %s:\n%s", protocol.WorkerMessageHeader, m.SenderID, m.ID, body)}
		seq, _, err := l.Store.AppendMessage(ctx, sessionID, row, nil)
		if err != nil {
			return false, err
		}
		r.appendRow(seq, row)
		delivered = append(delivered, session.DeliveredWorkerMessage{Seq: m.Seq, TranscriptSeq: seq})
	}
	if err := store.MarkWorkerMessagesDelivered(ctx, opts.WorkerRoot, sessionID, opts.RunID, delivered); err != nil {
		// The rows are in the transcript; a failed mark would deliver them again, so the run stops.
		return false, fmt.Errorf("record %d worker messages delivered: %w", len(delivered), err)
	}
	l.emit("worker.message.delivered", sessionID, map[string]any{"count": len(delivered), "rootId": opts.WorkerRoot})
	return true, nil
}
