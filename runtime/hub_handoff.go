package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/agent"
	"github.com/edynasty/LumenCortex/internal/taskstate"
)

// Agent handoff (Hub §3.3, collaboration §4): stop the source, write an observable handoff note into the
// source's Task State, then start the target. The note is host- and agent-authored text — never model
// reasoning — and it is the only state that crosses the handoff besides the transcript and the task
// graph the two sessions already share.
//
// What can move, and what cannot:
//
//   - The task and its Task Lease claims move to the target when the target is another session: the
//     holder becomes the target, so exactly one worker owns each unit throughout.
//   - A worktree stays with the session that owns it. A worktree directory is bound to its session by
//     name (worktreeRole: the directory's session is the owner), so a handoff to another session while
//     the source owns a worktree is refused instead of faked: the host applies the worktree first
//     (WorktreeHandoffPlan/ApplyWorktreeHandoff) and hands off after that.
//   - The agent may change within the same session: the target is the same session with another catalog
//     agent, so its worktree, transcript and Task State continue as they are.

// HubHandoff is one handoff request.
type HubHandoff struct {
	Source    string `json:"source"`
	Target    string `json:"target,omitempty"`
	Agent     string `json:"agent,omitempty"`
	Note      string `json:"note,omitempty"`
	CommandID string `json:"commandId,omitempty"`
}

// ErrHubWorktreeOwned refuses a cross-session handoff of a worktree owner: the owner rule binds the
// directory to its session, so the worktree must be applied first.
var ErrHubWorktreeOwned = errors.New("the source owns a worktree; apply the worktree handoff before handing the task to another session")

// handoffStopWait bounds how long a handoff waits for the source run to settle.
const handoffStopWait = 20 * time.Second

// Handoff stops the source and starts the target with the handoff note, returning the target session.
func (s *RunSupervisor) Handoff(ctx context.Context, h HubHandoff) (string, error) {
	if s == nil || s.engine == nil {
		return "", ErrRunSupervisorClosed
	}
	e := s.engine
	if h.Source == "" {
		return "", errors.New("a handoff needs a source session")
	}
	if err := e.runtimeEventAccess(ctx, h.Source); err != nil {
		return "", err
	}
	sourceRow, err := e.store.Get(ctx, h.Source)
	if err != nil {
		return "", err
	}
	target := h.Target
	if target == "" {
		target = h.Source
	}
	if err := e.runtimeEventAccess(ctx, target); err != nil {
		return "", err
	}
	agentID := strings.TrimSpace(h.Agent)
	if agentID == "" {
		agentID = sourceRow.AgentID
	}
	if agentID == "" {
		agentID = config.AgentDefault
	}
	// Any configured agent may take the work over: the handoff is the host's explicit choice, and the
	// target run's own Start still narrows its grant to the agent's ceilings.
	if err := e.delegationAgent(agentID, func(config.AgentInfo) bool { return true }); err != nil {
		return "", err
	}
	// Nothing of the source may run while the target starts: the two never hold the workspace together.
	if err := s.stopForHandoff(ctx, h.Source); err != nil {
		return "", err
	}
	// Both sessions must be free before the note and the leases move.
	if s.Active(target) {
		return "", ErrRunHeld
	}
	if target != h.Source {
		identity, err := e.SessionRuntime(ctx, h.Source)
		if err != nil {
			return "", err
		}
		if identity.Kind == RuntimeWorktree && identity.Owner == h.Source {
			return "", ErrHubWorktreeOwned
		}
	}
	if err := e.writeHandoffNote(ctx, h.Source, target, agentID, h.Note); err != nil {
		return "", err
	}
	var moved int
	if target != h.Source {
		moved, err = e.TransferHubTaskLeases(ctx, h.Source, target)
		if err != nil {
			return "", err
		}
	}
	e.events.publish("agent.handoff", h.Source, map[string]any{
		"target": target, "agent": agentID, "leases": moved, "commandId": h.CommandID,
	})
	_, _ = e.store.AppendCheckpoint(context.Background(), h.Source, "agent.handoff", map[string]any{
		"target": target, "agent": agentID, "leases": moved,
	})
	// The target reads the handoff note through the Task State block of its next request, so its input
	// is only a pointer.
	opts := AgentOptions{Agent: agentID, Input: handoffInput(h.Source, target, h.Note)}
	if _, err := s.Start(ctx, target, opts); err != nil {
		// The target did not start: the claims go back so the source (or the next host action) still owns
		// the units, and the note stays as the record of what was attempted.
		if moved > 0 {
			if _, backErr := e.TransferHubTaskLeases(ctx, target, h.Source); backErr != nil {
				return "", errors.Join(err, fmt.Errorf("return %d Task Leases to %s: %w", moved, h.Source, backErr))
			}
		}
		e.events.publish("agent.handoff.failed", h.Source, map[string]any{"target": target, "error": err.Error()})
		return "", err
	}
	return target, nil
}

// stopForHandoff cancels the source's run and waits for it to leave the supervisor, so no goroutine of
// the source writes after the handoff note.
func (s *RunSupervisor) stopForHandoff(ctx context.Context, sessionID string) error {
	if !s.Active(sessionID) {
		return nil
	}
	s.Cancel(sessionID)
	deadline := time.Now().Add(handoffStopWait)
	for s.Active(sessionID) {
		if time.Now().After(deadline) {
			return fmt.Errorf("the source run of %s did not stop within %s", sessionID, handoffStopWait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
	return nil
}

// writeHandoffNote records the handoff in the source's Task State, under the run lock the stopped run
// released. The note names the target and the goal the work continues with; it never carries model
// content of its own.
func (e *Engine) writeHandoffNote(ctx context.Context, source, target, agentID, note string) error {
	lock, err := e.acquireRun(ctx, source)
	if err != nil {
		return err
	}
	defer lock.Release()
	state, err := e.loadTaskState(ctx, source)
	if err != nil {
		return err
	}
	text, err := taskstate.NormalizeNoteText(handoffNoteText(source, target, agentID, note))
	if err != nil {
		return err
	}
	before := state.Clone()
	if _, err := state.WriteNote("", text, nil, time.Now().UTC()); err != nil {
		return err
	}
	_, err = e.putTaskState(ctx, source, taskstate.ReasonUser, state, 0, agent.HostStateEdges(before, state))
	return err
}

// handoffNoteText is the note body: a bounded, explicit statement of what happened.
func handoffNoteText(source, target, agentID, note string) string {
	text := fmt.Sprintf("## Handoff\nWork handed from %s to %s under agent %s.", source, target, agentID)
	if note = strings.TrimSpace(note); note != "" {
		text += " " + note
	}
	if len(text) > taskstate.MaxNoteTextBytes {
		text = text[:taskstate.MaxNoteTextBytes]
	}
	return text
}

// handoffInput is the target's opening row: a pointer to the handoff note, not a second copy of it.
func handoffInput(source, target, note string) string {
	text := fmt.Sprintf("[runtime] Work was handed to this session from %s. Read the Handoff note in the Task State above, check the workspace state, and continue the task from there.", source)
	if strings.TrimSpace(note) != "" && source != target {
		text += " The handoff said: " + strings.TrimSpace(note)
	}
	return text
}
