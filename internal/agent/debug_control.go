package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// DebugBoundary contains observable execution metadata only, never model content.
type DebugBoundary struct {
	Event      string `json:"event"`
	Step       int64  `json:"step"`
	Tool       string `json:"tool,omitempty"`
	ToolCallID string `json:"toolCallId,omitempty"`
	Agent      string `json:"agent,omitempty"`
	Failed     bool   `json:"failed,omitempty"`
	Denied     bool   `json:"denied,omitempty"`
}

type DebugBreakpoint struct {
	ID      string `json:"id"`
	Event   string `json:"event"`
	Enabled bool   `json:"enabled"`
	Once    bool   `json:"once,omitempty"`
	Tool    string `json:"tool,omitempty"`
	Agent   string `json:"agent,omitempty"`
	// Conditions are exact matches on observable metadata, not executable expressions.
	Conditions map[string]string `json:"conditions,omitempty"`
}

type DebugOptions struct {
	PauseOnStart bool              `json:"pauseOnStart,omitempty"`
	Breakpoints  []DebugBreakpoint `json:"breakpoints,omitempty"`
}

type DebugSnapshot struct {
	RunID       string            `json:"runId"`
	State       string            `json:"state"`
	Revision    uint64            `json:"revision"`
	Boundary    DebugBoundary     `json:"boundary"`
	Breakpoints []DebugBreakpoint `json:"breakpoints"`
	Reason      string            `json:"reason,omitempty"`
}

var ErrDebugStale = errors.New("debug control is stale; refresh the run snapshot")
var ErrDebugState = errors.New("debug command is not valid in the current state")

func ValidDebugEvent(event string) bool {
	switch event {
	case "BeforeModelCall", "AfterModelCall", "BeforeToolCall", "AfterToolCall", "ToolFailure", "PermissionDenied", "BeforeSubagentStart", "AfterSubagentEnd", "WorkflowTransition", "BeforeDelivery":
		return true
	}
	return false
}

func copyBreakpoints(in []DebugBreakpoint) ([]DebugBreakpoint, error) {
	if len(in) > 64 {
		return nil, errors.New("at most 64 breakpoints are allowed")
	}
	out := make([]DebugBreakpoint, len(in))
	ids := make(map[string]bool, len(in))
	for i, b := range in {
		if b.ID == "" || len(b.ID) > 128 || ids[b.ID] || !ValidDebugEvent(b.Event) || len(b.Tool) > 128 || len(b.Agent) > 128 {
			return nil, errors.New("invalid breakpoint identity or event")
		}
		ids[b.ID] = true
		out[i] = b
		out[i].Conditions = make(map[string]string, len(b.Conditions))
		for k, v := range b.Conditions {
			switch k {
			case "step", "tool", "agent", "failed", "denied":
			default:
				return nil, errors.New("unsupported breakpoint condition")
			}
			if len(v) > 128 {
				return nil, errors.New("breakpoint condition too long")
			}
			out[i].Conditions[k] = v
		}
	}
	return out, nil
}

// DebugControl belongs to one live trackedRun. No process suspension or replay is used.
type DebugControl struct {
	mu            sync.Mutex
	snapshot      DebugSnapshot
	changed       chan struct{}
	mode          string
	emit          func(DebugSnapshot)
	pendingReason string
}

func NewDebugControl(runID string, opts *DebugOptions, emit func(DebugSnapshot)) (*DebugControl, error) {
	d := &DebugControl{changed: make(chan struct{}), emit: emit, snapshot: DebugSnapshot{RunID: runID, State: "running", Revision: 1}}
	if opts != nil {
		var err error
		d.snapshot.Breakpoints, err = copyBreakpoints(opts.Breakpoints)
		if err != nil {
			return nil, err
		}
		if opts.PauseOnStart {
			d.snapshot.State = "pause_requested"
		}
	}
	return d, nil
}
func (d *DebugControl) snapshotLocked() DebugSnapshot {
	s := d.snapshot
	s.Breakpoints, _ = copyBreakpoints(s.Breakpoints)
	return s
}
func (d *DebugControl) Snapshot() DebugSnapshot {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.snapshotLocked()
}
func (d *DebugControl) wakeLocked() {
	close(d.changed)
	d.changed = make(chan struct{})
	d.snapshot.Revision++
}

// Command is called only after the Engine has validated live ownership and fences.
func (d *DebugControl) Command(revision uint64, action string, breakpoints []DebugBreakpoint) (DebugSnapshot, error) {
	var copied []DebugBreakpoint
	if action == "breakpoints" {
		var err error
		copied, err = copyBreakpoints(breakpoints)
		if err != nil {
			return DebugSnapshot{}, err
		}
	}
	d.mu.Lock()
	if revision != d.snapshot.Revision {
		d.mu.Unlock()
		return DebugSnapshot{}, ErrDebugStale
	}
	if d.snapshot.State == "closed" {
		d.mu.Unlock()
		return DebugSnapshot{}, ErrDebugState
	}
	switch action {
	case "pause":
		if d.snapshot.State != "paused" {
			d.snapshot.State = "pause_requested"
		}
		d.mode = ""
	case "continue", "step_agent", "step_model", "step_tool", "step_turn":
		if d.snapshot.State != "paused" {
			d.mu.Unlock()
			return DebugSnapshot{}, ErrDebugState
		}
		d.snapshot.State = "running"
		d.snapshot.Reason = ""
		d.mode = action
	case "breakpoints":
		d.snapshot.Breakpoints = copied
	default:
		d.mu.Unlock()
		return DebugSnapshot{}, ErrDebugState
	}
	d.wakeLocked()
	s := d.snapshotLocked()
	d.mu.Unlock()
	if d.emit != nil {
		d.emit(s)
	}
	return s, nil
}
func (d *DebugControl) Close() {
	d.mu.Lock()
	d.snapshot.State = "closed"
	d.wakeLocked()
	s := d.snapshotLocked()
	d.mu.Unlock()
	if d.emit != nil {
		d.emit(s)
	}
}

func matchesBreakpoint(b DebugBreakpoint, p DebugBoundary) bool {
	if !b.Enabled || b.Event != p.Event || (b.Tool != "" && b.Tool != p.Tool) || (b.Agent != "" && b.Agent != p.Agent) {
		return false
	}
	for k, v := range b.Conditions {
		var got string
		switch k {
		case "step":
			got = fmt.Sprint(p.Step)
		case "tool":
			got = p.Tool
		case "agent":
			got = p.Agent
		case "failed":
			got = fmt.Sprint(p.Failed)
		case "denied":
			got = fmt.Sprint(p.Denied)
		}
		if got != v {
			return false
		}
	}
	return true
}

// Observe latches an asynchronous child completion; the parent pauses only at its
// own next safe boundary. The child is never held while its parent waits for it.
func (d *DebugControl) Observe(point DebugBoundary) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.snapshot.State == "closed" {
		return
	}
	for i, breakpoint := range d.snapshot.Breakpoints {
		if matchesBreakpoint(breakpoint, point) {
			if d.pendingReason == "" {
				d.pendingReason = breakpoint.ID
			}
			if breakpoint.Once {
				d.snapshot.Breakpoints[i].Enabled = false
			}
			return
		}
	}
}

func (d *DebugControl) Boundary(ctx context.Context, p DebugBoundary) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	if d.snapshot.State == "closed" {
		d.mu.Unlock()
		return ErrDebugState
	}
	d.snapshot.Boundary = p
	reason := d.pendingReason
	d.pendingReason = ""
	if d.snapshot.State == "pause_requested" {
		reason = "pause"
	}
	switch d.mode {
	case "step_agent":
		reason = "step_agent"
	case "step_model":
		if p.Event == "AfterModelCall" || p.Event == "BeforeDelivery" {
			reason = "step_model"
		}
	case "step_tool":
		if p.Event == "AfterToolCall" || p.Event == "BeforeDelivery" {
			reason = "step_tool"
		}
	case "step_turn":
		if p.Event == "BeforeModelCall" || p.Event == "BeforeDelivery" {
			reason = "step_turn"
		}
	}
	for i, b := range d.snapshot.Breakpoints {
		if matchesBreakpoint(b, p) {
			reason = b.ID
			if b.Once {
				d.snapshot.Breakpoints[i].Enabled = false
			}
			break
		}
	}
	if reason == "" {
		d.mu.Unlock()
		return ctx.Err()
	}
	d.snapshot.State = "paused"
	d.snapshot.Reason = reason
	d.mode = ""
	d.wakeLocked()
	s := d.snapshotLocked()
	d.mu.Unlock()
	if d.emit != nil {
		d.emit(s)
	}
	for {
		d.mu.Lock()
		state := d.snapshot.State
		changed := d.changed
		if state == "pause_requested" {
			d.snapshot.State = "paused"
			state = "paused"
		}
		d.mu.Unlock()
		if state == "closed" {
			return ErrDebugState
		}
		if state != "paused" {
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

func (l *Loop) debugBoundary(ctx context.Context, p DebugBoundary) error {
	if l.DebugBoundary == nil {
		return ctx.Err()
	}
	return l.DebugBoundary(ctx, p)
}
