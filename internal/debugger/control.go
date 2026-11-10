package debugger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"
)

var (
	// ErrStaleReference reports a frame or variable reference minted at an
	// earlier stop. The adapter would answer it with an empty list; the Manager
	// refuses instead so a caller never reads "no variables" as a fact.
	ErrStaleReference = errors.New("debug reference is from an earlier stop; re-read stack/scopes")
	// ErrEvaluateNotGranted is distinct from ErrDebugDisabled: the session is
	// usable and only expression evaluation was not granted by the host.
	ErrEvaluateNotGranted = errors.New("expression evaluation is not granted for this session")
)

// MaxWait caps how long a control call may wait for a stop.
const MaxWait = 30 * time.Second

const defaultWait = 10 * time.Second

// WaitOptions controls whether a control command returns when the target stops.
type WaitOptions struct {
	// Until lists the states to wait for; empty means stopped, terminated, exited and lost.
	Until []string
	// Timeout bounds the wait; zero selects the default and anything above
	// MaxWait is clamped. It never exceeds the caller's context deadline.
	Timeout time.Duration
	// Async returns right after the adapter acknowledges the command. The
	// resulting StopInfo then has reason "pending".
	Async bool
}

// FrameSummary is one bounded stack frame of a stop.
type FrameSummary struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Line int    `json:"line"`
	Path string `json:"path,omitempty"`
}

// ExceptionInfo is the adapter's bounded description of an exception stop.
type ExceptionInfo struct {
	ID          string `json:"id,omitempty"`
	Description string `json:"description,omitempty"`
	BreakMode   string `json:"breakMode,omitempty"`
	Message     string `json:"message,omitempty"`
}

// StopInfo reports why and where execution stopped. Reason is one of
// breakpoint, step, exception, pause, entry, terminated, exited, lost, timeout
// or pending.
type StopInfo struct {
	Reason    string         `json:"reason"`
	ThreadID  int            `json:"threadId,omitempty"`
	StopEpoch uint64         `json:"stopEpoch"`
	State     string         `json:"state,omitempty"`
	TopFrames []FrameSummary `json:"topFrames,omitempty"`
	Exception *ExceptionInfo `json:"exception,omitempty"`
}

type stopEvent struct {
	Reason      string `json:"reason"`
	ThreadID    int    `json:"threadId"`
	Description string `json:"description"`
	Text        string `json:"text"`
}

type exceptionBody struct {
	ExceptionID string `json:"exceptionId"`
	Description string `json:"description"`
	BreakMode   string `json:"breakMode"`
	Details     struct {
		Message string `json:"message"`
	} `json:"details"`
}

// commandFor maps a control action to its DAP command.
func commandFor(action string) (string, bool) {
	switch action {
	case "configure":
		return "configurationDone", true
	case "continue":
		return "continue", true
	case "pause":
		return "pause", true
	case "step_over":
		return "next", true
	case "step_into":
		return "stepIn", true
	case "step_out":
		return "stepOut", true
	}
	return "", false
}

// watermark captures the epoch and the channel that the next complete stop
// closes. Both are read before the request is sent, so a stop that arrives
// while the adapter answers cannot be lost.
func (m *Manager) watermark(s *session) (uint64, chan struct{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.stopReady == nil {
		s.stopReady = make(chan struct{})
	}
	return s.stopEpoch, s.stopReady
}

// ControlAndWait sends a control command and, unless asynchronous, blocks
// until the target stops or the wait expires.
func (m *Manager) ControlAndWait(ctx context.Context, c Control, w WaitOptions) (Snapshot, StopInfo, error) {
	if c.Action == "stop" {
		snapshot, _, err := m.Disconnect(ctx, c, true)
		return snapshot, StopInfo{Reason: "terminated", State: snapshot.State}, err
	}
	command, ok := commandFor(c.Action)
	if !ok {
		return Snapshot{}, StopInfo{}, ErrDenied
	}
	s, unlock, err := m.lock(ctx, c)
	if err != nil {
		return Snapshot{}, StopInfo{}, err
	}
	defer unlock()
	since, ready := m.watermark(s)
	if _, err = m.request(ctx, s, command, map[string]any{"threadId": c.ThreadID}); err != nil {
		return Snapshot{}, StopInfo{}, err
	}
	snapshot, err := m.Snapshot(c.SessionID, c.DebugID)
	if err != nil {
		return Snapshot{}, StopInfo{}, err
	}
	if w.Async {
		return snapshot, StopInfo{Reason: "pending", ThreadID: snapshot.ThreadID, StopEpoch: snapshot.StopEpoch, State: snapshot.State}, nil
	}
	timeout := w.Timeout
	if timeout <= 0 {
		timeout = defaultWait
	}
	if timeout > MaxWait {
		timeout = MaxWait
	}
	wait, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		m.mu.Lock()
		if s.lastStop != nil && s.lastStop.StopEpoch > since {
			stop := *s.lastStop
			stop.TopFrames = append([]FrameSummary(nil), s.lastStop.TopFrames...)
			snapshot := s.snapshot
			m.mu.Unlock()
			return snapshot, stop, nil
		}
		ready = s.stopReady
		m.mu.Unlock()
		select {
		case <-ready:
		case <-wait.Done():
			snapshot, snapErr := m.Snapshot(c.SessionID, c.DebugID)
			if snapErr != nil {
				return Snapshot{}, StopInfo{}, snapErr
			}
			if ctx.Err() != nil {
				return snapshot, StopInfo{}, ctx.Err()
			}
			return snapshot, StopInfo{Reason: "timeout", ThreadID: snapshot.ThreadID, StopEpoch: snapshot.StopEpoch, State: snapshot.State}, nil
		}
	}
}

// SetExceptionBreakpoints replaces the adapter's exception filters. The
// filters must come from the adapter's initialize capabilities.
func (m *Manager) SetExceptionBreakpoints(ctx context.Context, c Control, filters []string) (json.RawMessage, error) {
	if len(filters) > 16 {
		return nil, ErrDenied
	}
	s, unlock, err := m.lock(ctx, c)
	if err != nil {
		return nil, err
	}
	defer unlock()
	m.mu.Lock()
	supported := slicesClone(s.exceptionFilters)
	m.mu.Unlock()
	allowed := make(map[string]bool, len(supported))
	for _, f := range supported {
		allowed[f] = true
	}
	for _, f := range filters {
		if !allowed[f] {
			return nil, fmt.Errorf("%w: adapter supports exception filters %v", ErrDenied, supported)
		}
	}
	out := map[string]any{"filters": filters, "supported": supported}
	if len(filters) > 0 {
		raw, err := m.request(ctx, s, "setExceptionBreakpoints", map[string]any{"filters": filters})
		if err != nil {
			return nil, err
		}
		if len(raw) > 0 {
			if err = json.Unmarshal(raw, &out); err != nil {
				return nil, err
			}
		}
		out["filters"] = filters
		out["supported"] = supported
	}
	return boundJSON(mustMarshal(out), m.stopEpochOf(s)), nil
}

// Disconnect asks the adapter to terminate the debuggee, waits briefly for the
// adapter to exit, then falls back to killing it. It reports which cleanup ran:
// "disconnect", "detach" (an IDE-owned session) or "kill".
func (m *Manager) Disconnect(ctx context.Context, c Control, terminateDebuggee bool) (Snapshot, string, error) {
	s, err := m.lookup(c.SessionID, c.DebugID)
	if err != nil {
		return Snapshot{}, "", err
	}
	m.mu.Lock()
	owner, alreadyClosed := s.snapshot.Owner, s.snapshot.State == "closed" || s.snapshot.State == "cleanup_failed"
	m.mu.Unlock()
	// Stopping is never an escalation, so a stale epoch is accepted for the
	// session's owner; only another actor is refused.
	if c.Actor != "human" && c.Actor != owner {
		return Snapshot{}, "", ErrStale
	}
	if alreadyClosed {
		m.mu.Lock()
		snapshot := s.snapshot
		m.mu.Unlock()
		return snapshot, "kill", nil
	}
	m.mu.Lock()
	s.closing = true
	s.snapshot.State = "stopping"
	m.mu.Unlock()
	cleanup := "kill"
	if s.adapter.MCPServer != "" {
		cleanup = "detach"
	} else if s.client != nil {
		request, cancel := context.WithTimeout(ctx, 2*time.Second)
		if _, err := m.request(request, s, "disconnect", map[string]any{"terminateDebuggee": terminateDebuggee}); err == nil {
			cleanup = "disconnect"
			if s.process != nil {
				select {
				case <-s.process.Done():
				case <-time.After(2 * time.Second):
				}
			}
		}
		cancel()
	}
	err = m.stopSession(s)
	m.mu.Lock()
	snapshot := s.snapshot
	m.mu.Unlock()
	return snapshot, cleanup, err
}

func slicesClone(in []string) []string {
	return append([]string(nil), in...)
}

func mustMarshal(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return raw
}

// stopEpochOf reads the current stop epoch.
func (m *Manager) stopEpochOf(s *session) uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return s.stopEpoch
}

// staleReference rejects a reference minted at an earlier stop. References the
// Manager never issued are forwarded: only the adapter can judge them.
func (m *Manager) staleReference(s *session, reference int) error {
	if reference <= 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if epoch, ok := s.refEpoch[reference]; ok && epoch != s.stopEpoch {
		return fmt.Errorf("%w: reference %d belongs to stop epoch %d, current %d", ErrStaleReference, reference, epoch, s.stopEpoch)
	}
	return nil
}

func (m *Manager) recordReferences(s *session, refs ...int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.refEpoch == nil {
		s.refEpoch = make(map[int]uint64)
	}
	for _, ref := range refs {
		if ref > 0 {
			s.refEpoch[ref] = s.stopEpoch
		}
	}
}

const (
	maxInspectValueBytes  = 2 << 10
	maxInspectResultBytes = 32 << 10
)

var inspectionArrays = []string{"threads", "stackFrames", "scopes", "variables", "breakpoints"}

// boundJSON trims every string value and the whole document, flags truncation
// and stamps the stop epoch the data belongs to. An unparsable document is
// returned unchanged rather than corrupted.
func boundJSON(raw json.RawMessage, stopEpoch uint64) json.RawMessage {
	var probe any
	if err := json.Unmarshal(raw, &probe); err != nil {
		return raw
	}
	lengths := map[string]int{}
	if obj, ok := probe.(map[string]any); ok {
		for _, key := range inspectionArrays {
			if items, ok := obj[key].([]any); ok {
				lengths[key] = len(items)
			}
		}
	}
	drops := map[string]int{}
	budget := maxInspectValueBytes
	for attempt := 0; attempt < 48; attempt++ {
		var doc any
		if err := json.Unmarshal(raw, &doc); err != nil {
			return raw
		}
		if obj, ok := doc.(map[string]any); ok {
			if stopEpoch > 0 {
				obj["stopEpoch"] = stopEpoch
			}
			for key, drop := range drops {
				trimArray(obj, key, drop)
			}
		}
		trimmed := trimStrings(doc, budget)
		out, err := json.Marshal(doc)
		if err != nil {
			return raw
		}
		if len(out) <= maxInspectResultBytes {
			if trimmed || anyDrops(drops) {
				return setTruncated(out)
			}
			return out
		}
		if budget > 64 {
			budget /= 2
			continue
		}
		progress := false
		for key, length := range lengths {
			if kept := length - drops[key]; kept > 1 {
				drops[key] += max(1, kept/2)
				progress = true
			}
		}
		if !progress {
			break
		}
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return raw
	}
	if obj, ok := doc.(map[string]any); ok {
		if stopEpoch > 0 {
			obj["stopEpoch"] = stopEpoch
		}
		for key := range lengths {
			trimArray(obj, key, lengths[key]-1)
		}
	}
	return setTruncated(mustMarshal(doc))
}

func anyDrops(drops map[string]int) bool {
	for _, n := range drops {
		if n > 0 {
			return true
		}
	}
	return false
}

func setTruncated(out []byte) []byte {
	var obj map[string]any
	if json.Unmarshal(out, &obj) != nil {
		return out
	}
	obj["truncated"] = true
	raw, err := json.Marshal(obj)
	if err != nil {
		return out
	}
	return raw
}

func trimArray(doc any, key string, drop int) {
	obj, ok := doc.(map[string]any)
	if !ok || drop <= 0 {
		return
	}
	items, ok := obj[key].([]any)
	if !ok {
		return
	}
	if drop >= len(items) {
		obj[key] = []any{}
		return
	}
	obj[key] = items[:len(items)-drop]
}

// trimStrings bounds every string in the document and reports a change.
func trimStrings(value any, budget int) bool {
	switch v := value.(type) {
	case map[string]any:
		changed := false
		for k, item := range v {
			if s, ok := item.(string); ok {
				if len(s) > budget {
					v[k] = clip(s, budget)
					changed = true
				}
				continue
			}
			changed = trimStrings(item, budget) || changed
		}
		return changed
	case []any:
		changed := false
		for i, item := range v {
			if s, ok := item.(string); ok {
				if len(s) > budget {
					v[i] = clip(s, budget)
					changed = true
				}
				continue
			}
			changed = trimStrings(item, budget) || changed
		}
		return changed
	}
	return false
}

// clip cuts s to budget bytes on a rune boundary and marks the cut.
func clip(s string, budget int) string {
	if len(s) <= budget {
		return s
	}
	cut := budget
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("…[+%d bytes]", len(s)-cut)
}
