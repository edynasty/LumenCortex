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
const Version = "2.0"

// Event is one lifecycle point a hook may observe.
type Event string

const (
	EventBeforeToolCall           Event = "BeforeToolCall"
	EventAfterToolCall            Event = "AfterToolCall"
	EventBeforeEval               Event = "BeforeEval"
	EventAfterEval                Event = "AfterEval"
	EventBeforeSubagentStart      Event = "BeforeSubagentStart"
	EventAfterSubagentStart       Event = "AfterSubagentStart"
	EventBeforeVerification       Event = "BeforeVerification"
	EventAfterVerification        Event = "AfterVerification"
	EventOnFailure                Event = "OnFailure"
	EventOnCancel                 Event = "OnCancel"
	EventOnTimeout                Event = "OnTimeout"
	EventSessionEnd               Event = "SessionEnd"
	EventStop                     Event = "Stop"
	EventCompletion               Event = "Completion"
	EventSessionStart             Event = "SessionStart"
	EventBeforeModelCall          Event = "BeforeModelCall"
	EventAfterModelCall           Event = "AfterModelCall"
	EventToolFailure              Event = "ToolFailure"
	EventBeforeSubagentEnd        Event = "BeforeSubagentEnd"
	EventAfterSubagentEnd         Event = "AfterSubagentEnd"
	EventBeforeDebugStart         Event = "BeforeDebugStart"
	EventAfterDebugStop           Event = "AfterDebugStop"
	EventBeforeWorkflowTransition Event = "BeforeWorkflowTransition"
	EventAfterWorkflowTransition  Event = "AfterWorkflowTransition"
	EventBeforeDelivery           Event = "BeforeDelivery"
	EventAfterDelivery            Event = "AfterDelivery"
	EventBeforeCompact            Event = "BeforeCompact"
	EventAfterCompact             Event = "AfterCompact"
)

// Events is every lifecycle event in documentation order.
func Events() []Event {
	return []Event{
		EventBeforeToolCall, EventAfterToolCall, EventBeforeEval, EventAfterEval, EventBeforeSubagentStart,
		EventAfterSubagentStart, EventBeforeVerification, EventAfterVerification, EventOnFailure, EventOnCancel,
		EventOnTimeout, EventSessionEnd, EventStop, EventCompletion,
		EventSessionStart, EventBeforeModelCall, EventAfterModelCall, EventToolFailure,
		EventBeforeSubagentEnd, EventAfterSubagentEnd, EventBeforeDebugStart, EventAfterDebugStop,
		EventBeforeWorkflowTransition, EventAfterWorkflowTransition, EventBeforeDelivery, EventAfterDelivery,
		EventBeforeCompact, EventAfterCompact,
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

// observation reports events that cannot authorize or undo completed work.
func (e Event) observation() bool {
	switch e {
	case EventAfterModelCall, EventAfterToolCall, EventToolFailure, EventAfterSubagentStart,
		EventBeforeSubagentEnd, EventAfterSubagentEnd, EventAfterEval, EventAfterDebugStop,
		EventAfterWorkflowTransition, EventAfterDelivery, EventAfterCompact, EventSessionEnd,
		EventOnFailure, EventOnCancel, EventOnTimeout:
		return true
	}
	return false
}

// Decision is what a hook decided at its event.
type Decision string

const (
	// DecisionContinue has no opinion: the remaining hooks of the event still run.
	DecisionContinue Decision = "continue"
	// DecisionDeny blocks admission. At an observing event it is recorded only.
	DecisionDeny                Decision = "deny"
	DecisionModifyAllowedFields Decision = "modify_allowed_fields"
)

// Valid reports whether d is a decision.
func (d Decision) Valid() bool {
	return d == DecisionContinue || d == DecisionDeny || d == DecisionModifyAllowedFields
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
	// ToolResult is set for completed tool, Eval and subagent operations.
	ToolResult *protocol.ToolResult
	// Feedback is the bounded feedback the event already holds, for Stop and Completion.
	Feedback string
	// Detail is bounded, typed context for the event (check verdicts, review findings). It must not
	// carry model reasoning.
	Detail map[string]any
	// ValidateArgs checks replacements before later hooks or execution. Without
	// the host's schema validator, argument modification is refused.
	ValidateArgs func(map[string]any) error
}

// Result is what a hook returns.
type Result struct {
	Decision Decision
	// Reason is a short machine-readable reason, at most 256 bytes.
	Reason string
	// Feedback is what the model is told when this decision blocks or corrects it. The registry
	// bounds it; the loop appends it as one completion_blocked row or tool feedback.
	Feedback string
	// Args patches tool arguments only with DecisionModifyAllowedFields.
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
	// Args is the arguments after every mutable hook's replacement, and only when one changed them
	// and the event was not denied. It is a copy: the caller's map was never shared with a hook.
	Args  map[string]any
	Audit []Audit
	// StopDenials counts the Stop denials of this session before the event.
	StopDenials int
	// StopLimitReached reports that a Stop denial was overridden so the run can end.
	StopLimitReached bool
	// Denials counts the blocking denials (PreVerify, PostVerify, Completion) of this session for
	// the event before it.
	Denials int
	// DenialLimitReached reports that a blocking denial past the finite limit was overridden so a
	// completion gate cannot hold a run open forever.
	DenialLimitReached bool
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
	// MaxBlockingDenials is how many denials one session may produce at each blocking completion
	// event (PreVerify, PostVerify, Completion) before the registry allows the run to continue
	// anyway. Zero takes DefaultStopDenials.
	MaxBlockingDenials int
	// MaxFeedbackBytes bounds one outcome's feedback. Zero takes DefaultFeedbackBytes.
	MaxFeedbackBytes int

	mu          sync.Mutex
	hooks       []Hook
	stopDenials map[string]int
	// blockingDenials counts consecutive denials per session and blocking event.
	blockingDenials map[string]int
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

type hookDepthKey struct{}

const maxHookDepth = 4

// NewRegistry returns an empty registry for the current hook protocol version. A version other
// than Version is refused: an old hook would not know the events it is asked about.
func NewRegistry(version string) (*Registry, error) {
	if version == "" {
		version = Version
	}
	if version != Version {
		return nil, fmt.Errorf("unsupported hook protocol version %q (this runtime speaks %s)", version, Version)
	}
	return &Registry{Version: version, stopDenials: map[string]int{}, blockingDenials: map[string]int{}}, nil
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

// ResetDenials clears every finite denial count of a session, the Stop and the blocking completion
// events included. The loop calls it when a run starts, so each turn gets its own bounds.
func (r *Registry) ResetDenials(sessionID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	delete(r.stopDenials, sessionID)
	for key := range r.blockingDenials {
		if strings.HasPrefix(key, sessionID+"\x00") {
			delete(r.blockingDenials, key)
		}
	}
	r.mu.Unlock()
}

// Decide runs every hook observing req.Event in deterministic order and merges their decisions.
// A Deny ends the event. An error or timeout follows the hook's fail policy; a safety hook is
// always fail-closed. Feedback is bounded, and a Stop or blocking-completion denial past its finite
// limit is overridden so a run always ends.
//
// Arguments are copied for every hook, so no hook holds a live reference to the call's arguments or
// result. A mutable hook's replacement becomes the arguments every later hook sees, and is validated
// before it is applied; a safety hook runs after the mutable hooks, on the final arguments, and its
// own replacement is refused.
func (r *Registry) Decide(ctx context.Context, req Request) Outcome {
	if r == nil {
		return Outcome{Decision: DecisionContinue}
	}
	if !req.Event.Valid() {
		return Outcome{Decision: DecisionDeny, Audit: []Audit{{Event: req.Event, Decision: DecisionDeny, Reason: "unknown event"}}}
	}
	depth, _ := ctx.Value(hookDepthKey{}).(int)
	if depth >= maxHookDepth {
		return Outcome{Decision: DecisionDeny, Audit: []Audit{{Event: req.Event, Decision: DecisionDeny, Reason: "hook recursion limit"}}}
	}
	ctx = context.WithValue(ctx, hookDepthKey{}, depth+1)
	if req.Version == "" {
		req.Version = r.Version
	}
	denialKey := req.SessionID + "\x00" + string(req.Event)
	r.mu.Lock()
	hooks := append([]Hook(nil), r.hooks...)
	maxStop := r.MaxStopDenials
	maxBlocking := r.MaxBlockingDenials
	maxFeedback := r.MaxFeedbackBytes
	priorStopDenials := r.stopDenials[req.SessionID]
	priorDenials := r.blockingDenials[denialKey]
	r.mu.Unlock()
	if maxStop <= 0 {
		maxStop = DefaultStopDenials
	}
	if maxBlocking <= 0 {
		maxBlocking = DefaultStopDenials
	}
	if maxFeedback <= 0 {
		maxFeedback = DefaultFeedbackBytes
	}
	sortHooks(hooks)

	out := Outcome{Decision: DecisionContinue, StopDenials: priorStopDenials, Denials: priorDenials}
	feedbackBytes := 0
	// The working arguments: a copy the caller's map never shares with a hook. replaced reports a
	// mutable hook changed them, so only a real replacement travels back to the loop.
	working := cloneArgs(req.ToolArgs)
	replaced := false
	blockingEvent := req.Event == EventBeforeVerification || req.Event == EventAfterVerification || req.Event == EventCompletion

	// Two passes: ordinary hooks decide the arguments, then safety hooks decide the final ones. A
	// safety hook never sees arguments a later mutable hook could still replace.
	for _, safety := range []bool{false, true} {
		for _, h := range hooks {
			if !observes(h, req.Event) || h.Safety() != safety {
				continue
			}
			if len(out.Audit) >= maxAuditEntries {
				return Outcome{Decision: DecisionDeny, Audit: append(out.Audit, Audit{Event: req.Event, Decision: DecisionDeny, Reason: "hook audit limit"})}
			}
			hookReq := req
			hookReq.ToolArgs = cloneArgs(working)
			hookReq.ToolResult = cloneToolResult(req.ToolResult)
			hookReq.Detail = cloneDetail(req.Detail)
			runCtx, cancel := context.WithTimeout(ctx, hookTimeout(h))
			started := time.Now()
			result, err := runBounded(runCtx, h, hookReq)
			cancel()
			entry := Audit{Hook: h.ID(), Version: h.Version(), Event: req.Event,
				ElapsedMS: time.Since(started).Milliseconds()}
			if err != nil {
				policy := effectivePolicy(h)
				if req.Event.observation() && !h.Safety() {
					policy = FailOpen
				}
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
				if req.Event.observation() && !h.Safety() {
					entry.FailPolicy = FailOpen
				}
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
				switch {
				case decision == DecisionModifyAllowedFields && h.MutableArgs() && !h.Safety() && req.Event == EventBeforeToolCall && req.ValidateArgs != nil:
					next := cloneArgs(working)
					if next == nil {
						next = map[string]any{}
					}
					for key, value := range result.Args {
						next[key] = cloneValue(value)
					}
					if verr := req.ValidateArgs(cloneArgs(next)); verr != nil {
						entry.ArgsIgnored = true
						entry.Decision = DecisionDeny
						entry.Reason = "replacement arguments are invalid"
						entry.Err = truncate(verr.Error(), MaxReasonBytes)
						out.Feedback = appendBoundedFeedback(out.Feedback, "the hook's replacement arguments are invalid for this tool: "+verr.Error(), &feedbackBytes, maxFeedback)
						out.Audit = append(out.Audit, entry)
						out.Decision = DecisionDeny
						return out
					}
					working, replaced = next, true
				default:
					// A hook that may not change arguments, and a safety hook, cannot: the
					// replacement is reported and dropped.
					entry.ArgsIgnored = true
				}
			}
			if text := strings.TrimSpace(result.Feedback); text != "" {
				out.Feedback = appendBoundedFeedback(out.Feedback, text, &feedbackBytes, maxFeedback)
			}
			if decision == DecisionDeny {
				if req.Event.observation() && !h.Safety() {
					entry.Overridden = "observation_only"
					out.Audit = append(out.Audit, entry)
					continue
				}
				switch {
				case req.Event == EventStop:
					// A Stop denial is bounded per session: the run must always be able to end.
					if !h.Safety() && priorStopDenials >= maxStop {
						entry.Overridden = "stop_denial_limit"
						entry.Decision = DecisionContinue
						out.Audit = append(out.Audit, entry)
						out.StopLimitReached = true
						continue
					}
					r.mu.Lock()
					r.stopDenials[req.SessionID] = priorStopDenials + 1
					r.mu.Unlock()
					out.StopDenials = priorStopDenials + 1
				case blockingEvent:
					// A completion gate hook is bounded too: a denial the model cannot satisfy must
					// not hold the run open past the repair budget.
					if !h.Safety() && priorDenials >= maxBlocking {
						entry.Overridden = "denial_limit"
						entry.Decision = DecisionContinue
						out.Audit = append(out.Audit, entry)
						out.DenialLimitReached = true
						continue
					}
					r.mu.Lock()
					r.blockingDenials[denialKey] = priorDenials + 1
					r.mu.Unlock()
					out.Denials = priorDenials + 1
				}
				out.Audit = append(out.Audit, entry)
				out.Decision = DecisionDeny
				return out
			}
			out.Audit = append(out.Audit, entry)
		}
	}
	if out.Decision != DecisionDeny {
		r.mu.Lock()
		switch req.Event {
		case EventStop:
			delete(r.stopDenials, req.SessionID)
		case EventBeforeVerification, EventAfterVerification, EventCompletion:
			delete(r.blockingDenials, denialKey)
		}
		r.mu.Unlock()
	}
	if replaced && out.Decision != DecisionDeny {
		out.Args = working
	}
	return out
}

// appendBoundedFeedback appends text while the event's feedback budget allows, truncating the last
// entry. It keeps one event's feedback bounded whatever the hooks say.
func appendBoundedFeedback(feedback []string, text string, used *int, max int) []string {
	text = strings.TrimSpace(text)
	if text == "" || *used >= max {
		return feedback
	}
	text = truncate(text, max-*used)
	*used += len(text)
	return append(feedback, text)
}

// runBounded runs one hook and returns as soon as it decides, its timeout passes, or ctx ends. A
// hook that ignores its context cannot block the run: the registry stops waiting for it and applies
// the hook's fail policy. The goroutine's channel is buffered, so a late return is dropped and the
// goroutine exits.
func runBounded(ctx context.Context, h Hook, req Request) (Result, error) {
	type outcome struct {
		result Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				done <- outcome{err: fmt.Errorf("hook panicked: %v", recovered)}
			}
		}()
		result, err := h.Run(ctx, req)
		done <- outcome{result: result, err: err}
	}()
	select {
	case out := <-done:
		return out.result, out.err
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
}

// cloneArgs copies a tool argument map, nested containers included, so a hook never holds a live
// reference to the call's arguments. Scalar values are copied as they are: a hook's replacement keeps
// the Go type it returned.
func cloneArgs(args map[string]any) map[string]any {
	if args == nil {
		return nil
	}
	out := make(map[string]any, len(args))
	for key, value := range args {
		out[key] = cloneValue(value)
	}
	return out
}

// cloneValue copies a decoded JSON value one level deeper.
func cloneValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		return cloneArgs(v)
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = cloneValue(item)
		}
		return out
	default:
		return value
	}
}

// cloneToolResult copies the result a post hook observes, so a hook cannot rewrite what the model
// is shown.
func cloneToolResult(result *protocol.ToolResult) *protocol.ToolResult {
	if result == nil {
		return nil
	}
	copied := *result
	return &copied
}

// cloneDetail copies the event detail map, so a hook cannot change what a later hook or the audit
// sees.
func cloneDetail(detail map[string]any) map[string]any {
	if detail == nil {
		return nil
	}
	out := make(map[string]any, len(detail))
	for k, v := range detail {
		out[k] = cloneValue(v)
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
