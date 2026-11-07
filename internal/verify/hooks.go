// Package verify holds the runtime's versioned lifecycle hooks and its independent verification
// rules: what a check, a model's own claim and an independent reviewer may each decide, and how a
// bounded failure feedback becomes a repair attempt without ever widening the run's authority.
//
// Nothing here calls a model, reads a provider, or writes durable state. Hooks are supplied by the
// host; the loop runs them at the events it declares and records their audit entries. The rules are
// pure so the loop can apply them before it accepts a completion.
package verify

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/edynasty/LumenCortex/protocol"
)

// Version is the hook protocol version. A registry reports it and refuses a hook that implements
// another one, so an event or decision can never silently change meaning between versions.
const Version = "1.0"

// Event is one lifecycle point a hook may observe.
type Event string

const (
	EventPreToolUse     Event = "PreToolUse"
	EventPostToolUse    Event = "PostToolUse"
	EventPreEval        Event = "PreEval"
	EventPostEval       Event = "PostEval"
	EventPreSubagent    Event = "PreSubagent"
	EventPostSubagent   Event = "PostSubagent"
	EventPreVerify      Event = "PreVerify"
	EventPostVerify     Event = "PostVerify"
	EventOnFailure      Event = "OnFailure"
	EventOnCancel       Event = "OnCancel"
	EventOnTimeout      Event = "OnTimeout"
	EventOnSessionClose Event = "OnSessionClose"
	EventStop           Event = "Stop"
	EventCompletion     Event = "Completion"
)

// Events is every lifecycle event in documentation order.
func Events() []Event {
	return []Event{
		EventPreToolUse, EventPostToolUse, EventPreEval, EventPostEval, EventPreSubagent,
		EventPostSubagent, EventPreVerify, EventPostVerify, EventOnFailure, EventOnCancel,
		EventOnTimeout, EventOnSessionClose, EventStop, EventCompletion,
	}
}

// Valid reports whether e is a lifecycle event.
func (e Event) Valid() bool {
	for _, known := range Events() {
		if e == known {
			return true
		}
	}
	return false
}

// Decision is what a hook decided at its event.
type Decision string

const (
	// DecisionAllow permits the action. Like Continue it never overrides a later Deny.
	DecisionAllow Decision = "allow"
	// DecisionContinue has no opinion: the remaining hooks of the event still run.
	DecisionContinue Decision = "continue"
	// DecisionDeny blocks the action at an event that can block (PreToolUse, PreEval,
	// PreSubagent, PreVerify, Stop, Completion). At an observing event it is reported, not enforced.
	DecisionDeny Decision = "deny"
)

// Valid reports whether d is a decision.
func (d Decision) Valid() bool {
	return d == DecisionAllow || d == DecisionContinue || d == DecisionDeny
}

// FailPolicy is what an event does when a hook errors or times out. The zero value is FailClosed:
// a hook must opt into fail-open explicitly, and a safety hook never can.
type FailPolicy string

const (
	FailClosed FailPolicy = "fail_closed"
	FailOpen   FailPolicy = "fail_open"
)

// Hook is one host-supplied lifecycle hook. Implementations must be safe for concurrent Decide
// calls; the registry may run different events concurrently.
type Hook interface {
	// ID identifies the hook. It is unique in a registry and stable across runs.
	ID() string
	// Version is the hook's own version, at least 1. It is reported with every audit entry.
	Version() int
	// Events are the lifecycle events the hook observes; at least one.
	Events() []Event
	// Priority orders hooks of one event: lower runs first, then ID. Equal priorities are legal.
	Priority() int
	// Timeout bounds one Run call. Zero takes DefaultHookTimeout; the registry caps it.
	Timeout() time.Duration
	// FailPolicy decides what an error or timeout does. Zero means FailClosed.
	FailPolicy() FailPolicy
	// MutableArgs reports whether the hook may replace a tool call's arguments. A hook that returns
	// arguments without declaring this is ignored, and the audit entry says so.
	MutableArgs() bool
	// Safety marks a hook that guards permissions, sandbox or authority. A safety hook is always
	// fail-closed, whatever FailPolicy says, and may not be overridden by a later hook.
	Safety() bool
	// Run decides the event. An error is handled by the hook's fail policy.
	Run(ctx context.Context, req Request) (Result, error)
}

// DefaultHookTimeout bounds a hook that declares none.
const DefaultHookTimeout = 5 * time.Second

// MaxHookTimeout is the longest timeout a hook may declare.
const MaxHookTimeout = 30 * time.Second

// Request is one event as a hook sees it. It carries no hidden reasoning: every field is an
// observable input the loop already holds.
type Request struct {
	// Version is the hook protocol version the loop runs.
	Version   string
	Event     Event
	SessionID string
	RunID     string
	Step      int64
	// ToolCallID and ToolName name the call at a tool event; both are empty otherwise.
	ToolCallID string
	ToolName   string
	ToolArgs   map[string]any
	// ToolResult is set at PostToolUse, PostEval and PostSubagent.
	ToolResult *protocol.ToolResult
	// Feedback is the bounded feedback the event already holds, for Stop and Completion.
	Feedback string
	// Detail is bounded, typed context for the event (check verdicts, review findings). It must not
	// carry model reasoning.
	Detail map[string]any
}

// Result is what a hook returns.
type Result struct {
	Decision Decision
	// Reason is a short machine-readable reason, at most 256 bytes.
	Reason string
	// Feedback is what the model is told when this decision blocks or corrects it. The registry
	// bounds it; the loop appends it as one completion_blocked row or tool feedback.
	Feedback string
	// Args replaces tool arguments when the hook declares MutableArgs.
	Args map[string]any
}

// Audit is one hook's contribution to an event, recorded by the loop as an event/checkpoint. It
// holds no arguments and no feedback text: only what decided and why.
type Audit struct {
	Hook      string   `json:"hook"`
	Version   int      `json:"version"`
	Event     Event    `json:"event"`
	Decision  Decision `json:"decision"`
	Reason    string   `json:"reason,omitempty"`
	Err       string   `json:"error,omitempty"`
	ElapsedMS int64    `json:"elapsedMs"`
	// FailPolicy is the policy that applied when the hook failed.
	FailPolicy FailPolicy `json:"failPolicy,omitempty"`
	// Overridden is set when the registry replaced the hook's decision, e.g. a Stop denial past the
	// finite limit.
	Overridden string `json:"overridden,omitempty"`
	// ArgsIgnored is set when a hook returned arguments it may not change.
	ArgsIgnored bool `json:"argsIgnored,omitempty"`
}

// Outcome is the registry's decision for one event.
type Outcome struct {
	Decision Decision
	// Feedback is every hook's bounded feedback, in order.
	Feedback []string
	// Args is the replacement arguments when a mutable hook supplied them and the event was not
	// denied.
	Args  map[string]any
	Audit []Audit
	// StopDenials counts the Stop denials of this session before the event.
	StopDenials int
	// StopLimitReached reports that a Stop denial was overridden so the run can end.
	StopLimitReached bool
}

// Denied reports a blocking outcome.
func (o Outcome) Denied() bool { return o.Decision == DecisionDeny }

// FeedbackText joins the bounded feedback entries.
func (o Outcome) FeedbackText() string { return strings.Join(o.Feedback, "\n") }

// Registry holds a host's hooks. It is safe for concurrent use.
type Registry struct {
	// Version is the hook protocol version the host built the registry for.
	Version string
	// MaxStopDenials is how many Stop denials one session may produce before the registry allows
	// the run to end anyway. Zero takes DefaultStopDenials.
	MaxStopDenials int
	// MaxFeedbackBytes bounds one outcome's feedback. Zero takes DefaultFeedbackBytes.
	MaxFeedbackBytes int

	mu          sync.Mutex
	hooks       []Hook
	stopDenials map[string]int
}

const (
	// DefaultStopDenials bounds consecutive Stop denials per session. A hook cannot hold a run
	// open forever.
	DefaultStopDenials = 3
	// DefaultFeedbackBytes bounds the feedback one event carries to the model.
	DefaultFeedbackBytes = 4096
	// MaxReasonBytes bounds a hook's reason string.
	MaxReasonBytes = 256
	// maxAuditEntries bounds the audit entries one event returns.
	maxAuditEntries = 64
)

// NewRegistry returns an empty registry for the current hook protocol version. A version other
// than Version is refused: an old hook would not know the events it is asked about.
func NewRegistry(version string) (*Registry, error) {
	if version == "" {
		version = Version
	}
	if version != Version {
		return nil, fmt.Errorf("unsupported hook protocol version %q (this runtime speaks %s)", version, Version)
	}
	return &Registry{Version: version, stopDenials: map[string]int{}}, nil
}

// Register adds one hook. It refuses an invalid hook and a duplicate id before any event runs.
func (r *Registry) Register(h Hook) error {
	if r == nil {
		return errors.New("verify: nil registry")
	}
	if h == nil {
		return errors.New("verify: nil hook")
	}
	id := strings.TrimSpace(h.ID())
	if id == "" || len(id) > 128 {
		return fmt.Errorf("verify: hook id must be 1-128 bytes")
	}
	if h.Version() < 1 {
		return fmt.Errorf("verify: hook %q has no version", id)
	}
	events := h.Events()
	if len(events) == 0 {
		return fmt.Errorf("verify: hook %q observes no event", id)
	}
	for _, event := range events {
		if !event.Valid() {
			return fmt.Errorf("verify: hook %q observes unknown event %q", id, event)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.hooks {
		if existing.ID() == id {
			return fmt.Errorf("verify: duplicate hook id %q", id)
		}
	}
	r.hooks = append(r.hooks, h)
	return nil
}

// HookInfo is one registered hook's declaration.
type HookInfo struct {
	ID       string     `json:"id"`
	Version  int        `json:"version"`
	Events   []Event    `json:"events"`
	Priority int        `json:"priority"`
	Timeout  int64      `json:"timeoutMs"`
	Policy   FailPolicy `json:"failPolicy"`
	Mutable  bool       `json:"mutableArgs"`
	Safety   bool       `json:"safety"`
}

// Hooks lists the registered hooks in execution order (priority, then id).
func (r *Registry) Hooks() []HookInfo {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	hooks := append([]Hook(nil), r.hooks...)
	r.mu.Unlock()
	sortHooks(hooks)
	out := make([]HookInfo, 0, len(hooks))
	for _, h := range hooks {
		events := append([]Event(nil), h.Events()...)
		out = append(out, HookInfo{ID: h.ID(), Version: h.Version(), Events: events, Priority: h.Priority(),
			Timeout: int64(hookTimeout(h)) / int64(time.Millisecond), Policy: effectivePolicy(h), Mutable: h.MutableArgs(), Safety: h.Safety()})
	}
	return out
}

func sortHooks(hooks []Hook) {
	sort.SliceStable(hooks, func(i, j int) bool {
		if hooks[i].Priority() != hooks[j].Priority() {
			return hooks[i].Priority() < hooks[j].Priority()
		}
		return hooks[i].ID() < hooks[j].ID()
	})
}

func hookTimeout(h Hook) time.Duration {
	timeout := h.Timeout()
	if timeout <= 0 {
		return DefaultHookTimeout
	}
	if timeout > MaxHookTimeout {
		return MaxHookTimeout
	}
	return timeout
}

// effectivePolicy is the policy an error uses: a safety hook is always fail-closed, and an
// undeclared policy is fail-closed.
func effectivePolicy(h Hook) FailPolicy {
	if h.Safety() {
		return FailClosed
	}
	if h.FailPolicy() == FailOpen {
		return FailOpen
	}
	return FailClosed
}

// ResetStopDenials clears the finite Stop-denial count of a session. A host that resumes a session
// calls it when a new user turn starts, so the new turn gets its own bounded denials.
func (r *Registry) ResetStopDenials(sessionID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	delete(r.stopDenials, sessionID)
	r.mu.Unlock()
}

// Decide runs every hook observing req.Event in deterministic order and merges their decisions.
// A Deny ends the event. An error or timeout follows the hook's fail policy; a safety hook is
// always fail-closed. Feedback is bounded, arguments are applied only from a mutable hook, and a
// Stop denial past MaxStopDenials is overridden so a run always ends.
func (r *Registry) Decide(ctx context.Context, req Request) Outcome {
	if r == nil {
		return Outcome{Decision: DecisionAllow}
	}
	if !req.Event.Valid() {
		return Outcome{Decision: DecisionDeny, Audit: []Audit{{Event: req.Event, Decision: DecisionDeny, Reason: "unknown event"}}}
	}
	if req.Version == "" {
		req.Version = r.Version
	}
	r.mu.Lock()
	hooks := append([]Hook(nil), r.hooks...)
	maxStop := r.MaxStopDenials
	maxFeedback := r.MaxFeedbackBytes
	priorStopDenials := r.stopDenials[req.SessionID]
	r.mu.Unlock()
	if maxStop <= 0 {
		maxStop = DefaultStopDenials
	}
	if maxFeedback <= 0 {
		maxFeedback = DefaultFeedbackBytes
	}
	sortHooks(hooks)

	out := Outcome{Decision: DecisionAllow, StopDenials: priorStopDenials}
	feedbackBytes := 0
	for _, h := range hooks {
		if !observes(h, req.Event) {
			continue
		}
		if len(out.Audit) >= maxAuditEntries {
			break
		}
		runCtx, cancel := context.WithTimeout(ctx, hookTimeout(h))
		started := time.Now()
		result, err := h.Run(runCtx, req)
		cancel()
		entry := Audit{Hook: h.ID(), Version: h.Version(), Event: req.Event,
			ElapsedMS: time.Since(started).Milliseconds()}
		if err != nil {
			policy := effectivePolicy(h)
			entry.FailPolicy = policy
			entry.Err = truncate(err.Error(), MaxReasonBytes)
			if policy == FailOpen {
				entry.Decision = DecisionContinue
				out.Audit = append(out.Audit, entry)
				continue
			}
			entry.Decision = DecisionDeny
			entry.Reason = "hook failed (fail-closed)"
			out.Audit = append(out.Audit, entry)
			out.Decision = DecisionDeny
			return out
		}
		decision := result.Decision
		if decision == "" {
			decision = DecisionContinue
		}
		if !decision.Valid() {
			entry.Err = "invalid decision " + string(decision)
			entry.Decision = DecisionDeny
			entry.FailPolicy = effectivePolicy(h)
			if entry.FailPolicy == FailOpen {
				entry.Decision = DecisionContinue
				out.Audit = append(out.Audit, entry)
				continue
			}
			out.Audit = append(out.Audit, entry)
			out.Decision = DecisionDeny
			return out
		}
		entry.Decision = decision
		entry.Reason = truncate(result.Reason, MaxReasonBytes)
		if result.Args != nil {
			if h.MutableArgs() && !h.Safety() {
				if out.Args == nil {
					out.Args = map[string]any{}
				}
				for key, value := range result.Args {
					out.Args[key] = value
				}
			} else {
				entry.ArgsIgnored = true
			}
		}
		if text := strings.TrimSpace(result.Feedback); text != "" && feedbackBytes < maxFeedback {
			remaining := maxFeedback - feedbackBytes
			text = truncate(text, remaining)
			out.Feedback = append(out.Feedback, text)
			feedbackBytes += len(text)
		}
		if decision == DecisionDeny {
			if req.Event == EventStop {
				// A Stop denial is bounded per session: the run must always be able to end.
				if priorStopDenials >= maxStop {
					entry.Overridden = "stop_denial_limit"
					entry.Decision = DecisionAllow
					out.Audit = append(out.Audit, entry)
					out.StopLimitReached = true
					continue
				}
				r.mu.Lock()
				r.stopDenials[req.SessionID] = priorStopDenials + 1
				r.mu.Unlock()
				out.StopDenials = priorStopDenials + 1
			}
			out.Audit = append(out.Audit, entry)
			out.Decision = DecisionDeny
			return out
		}
		out.Audit = append(out.Audit, entry)
	}
	if req.Event == EventStop && out.Decision != DecisionDeny {
		r.mu.Lock()
		delete(r.stopDenials, req.SessionID)
		r.mu.Unlock()
	}
	return out
}

func observes(h Hook, event Event) bool {
	for _, e := range h.Events() {
		if e == event {
			return true
		}
	}
	return false
}

// truncate cuts s to at most limit bytes at a rune boundary and marks the cut.
func truncate(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	marker := "…"
	if limit <= len(marker) {
		return s[:limit]
	}
	cut := s[:limit-len(marker)]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + marker
}
