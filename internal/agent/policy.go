package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/protocol"
)

// This file is the model-selection policy (design §4.5): pure functions over observable inputs, the
// health registry, and the decision log that records what each function read and returned. Nothing
// here performs I/O, and no function reads provider-specific errors: the adapter classifies.

// Decision kinds.
const (
	DecisionModel    = "model"
	DecisionEffort   = "effort"
	DecisionFailover = "failover"
)

// Rule ids of decisions. The pure function that applies a rule returns its id; an id never changes
// meaning. A failover entry's rule is RuleFailoverPrefix followed by the error class that moved the run.
const (
	RuleModelUser       = "model.user"       // AgentOptions.ModelRef differs from sessions.model_ref; written to it
	RuleModelSession    = "model.session"    // sessions.model_ref
	RuleModelDefault    = "model.default"    // the config model; written to sessions.model_ref
	RuleModelCandidates = "model.candidates" // the ordered, filtered candidate list of one step
	RuleEffortOverride  = "effort.override"  // AgentOptions.Effort
	RuleEffortDefault   = "effort.default"   // config default
	RuleFailoverPrefix  = "failover."
)

// DefaultUnhealthyFor is how long a model stays out of the candidate list after an availability
// failure that carried no Retry-After.
const DefaultUnhealthyFor = 30 * time.Second

// ResolveModelRef picks the model a Start runs on and names the rule (design §4.5):
//
//	requested differs from stored  → model.user    (re-pins the session)
//	otherwise stored is set        → model.session
//	otherwise                      → model.default (config model; pins a session that has none)
//
// The result is "" only when all three inputs are empty; the caller fails Start with ErrUnknownModel
// for that and for a reference outside the catalog. It never reads or writes anything.
func ResolveModelRef(requested, stored, def config.ModelRef) (config.ModelRef, string) {
	switch {
	case requested != "" && requested != stored:
		return requested, RuleModelUser
	case stored != "":
		return stored, RuleModelSession
	default:
		return def, RuleModelDefault
	}
}

// WritesModelRef reports whether a model rule persists its result to sessions.model_ref.
func WritesModelRef(rule string) bool { return rule == RuleModelUser || rule == RuleModelDefault }

// Candidate holds the effective limits of one model: config limit.context / limit.output, else
// 32 768 / 4 096.
type Candidate struct {
	Ref         config.ModelRef `json:"ref"`
	Context     int             `json:"context"`
	Output      int             `json:"output"`
	ToolCalling bool            `json:"toolCalling"`
}

// OutputBudget is the output reservation one request to c makes: the run's MaxTokens, else c's limit.
func OutputBudget(maxTokens int, c Candidate) int {
	if maxTokens > 0 {
		return maxTokens
	}
	return c.Output
}

// Margin is the share of a model's context left free for estimate error: 5%.
func Margin(contextTokens int) int { return contextTokens / 20 }

// Candidates is the ordered list one request may use: the session model first, then the config
// failover list, without duplicates. A model is dropped when it is not in models, when it has no tool
// calling, or when base plus its own margin and output reservation exceeds its own context. Models
// the health registry marks unavailable are dropped too, unless that would leave nothing: the
// registry is advisory. A nil available means every model is available.
func Candidates(session config.ModelRef, failover []config.ModelRef, models map[config.ModelRef]Candidate, base, maxTokens int, available func(config.ModelRef) bool) []Candidate {
	eligible := make([]Candidate, 0, 1+len(failover))
	seen := map[config.ModelRef]bool{}
	for _, ref := range append([]config.ModelRef{session}, failover...) {
		c, ok := models[ref]
		if !ok || seen[ref] || !c.ToolCalling || c.Context < base+Margin(c.Context)+OutputBudget(maxTokens, c) {
			continue
		}
		seen[ref] = true
		eligible = append(eligible, c)
	}
	if available == nil {
		return eligible
	}
	healthy := make([]Candidate, 0, len(eligible))
	for _, c := range eligible {
		if available(c.Ref) {
			healthy = append(healthy, c)
		}
	}
	if len(healthy) == 0 {
		return eligible
	}
	return healthy
}

// Classify reads the class of a failed Complete. The run context decides cancellation: an error is
// never "cancelled" because it unwraps to context.Canceled, since an idle timeout built from a
// derived context would look the same (design F2). Adapters return *protocol.ProviderError; any
// other error from a provider is treated as availability, the class that moves to the next model.
func Classify(runCtx context.Context, err error) protocol.ErrorClass {
	if runCtx.Err() != nil {
		return protocol.ClassCancelled
	}
	var pe *protocol.ProviderError
	if errors.As(err, &pe) && pe.Class != "" {
		if pe.Class == protocol.ClassCancelled {
			// The adapter saw a cancellation the run context does not show: it is not the run's.
			return protocol.ClassAvailability
		}
		return pe.Class
	}
	return protocol.ClassAvailability
}

// retryAfter returns the delay a provider error asked for, 0 when it did not.
func retryAfter(err error) time.Duration {
	var pe *protocol.ProviderError
	if errors.As(err, &pe) {
		return pe.RetryAfter
	}
	return 0
}

// Failover returns the index of the next candidate after cands[failed] returned class, or -1 to stop:
//
//	availability → the next candidate
//	capacity     → the next candidate with a larger context than the one that failed
//	every other class stops
func Failover(cands []Candidate, failed int, class protocol.ErrorClass) int {
	if failed < 0 || failed >= len(cands) {
		return -1
	}
	switch class {
	case protocol.ClassAvailability:
		if failed+1 < len(cands) {
			return failed + 1
		}
	case protocol.ClassCapacity:
		for i := failed + 1; i < len(cands); i++ {
			if cands[i].Context > cands[failed].Context {
				return i
			}
		}
	}
	return -1
}

var effortOrder = []config.Effort{config.EffortNone, config.EffortLow, config.EffortMedium, config.EffortHigh, config.EffortMax}

func effortIndex(e config.Effort) int { return slices.Index(effortOrder, e) }

// ValidEffort reports whether e may be requested: "" (use the default) or one of the five levels.
func ValidEffort(e config.Effort) bool { return e == "" || effortIndex(e) >= 0 }

// EffortFor is the only effort rule: the per-run override when set, else the configured default.
// "" means "send no reasoning parameter". Escalation arrives with Task State progress in M3.1.
func EffortFor(def, override config.Effort) (config.Effort, string) {
	if override != "" {
		return override, RuleEffortOverride
	}
	return def, RuleEffortDefault
}

// ClampEffort fits a requested effort to one model's reasoning.efforts: the lowest listed level not
// below the request, else the highest listed level. No list leaves the request unchanged, and ""
// (send nothing) is never changed.
func ClampEffort(e config.Effort, supported []config.Effort) config.Effort {
	if e == "" || len(supported) == 0 {
		return e
	}
	up, highest := config.Effort(""), supported[0]
	for _, s := range supported {
		if effortIndex(s) > effortIndex(highest) {
			highest = s
		}
		if effortIndex(s) >= effortIndex(e) && (up == "" || effortIndex(s) < effortIndex(up)) {
			up = s
		}
	}
	if up == "" {
		return highest
	}
	return up
}

// Health is the model health registry: one map per Engine from model reference to the time the model
// becomes available again. It is advisory, shared by every session of its Engine, cleared by a
// success, and never persisted.
type Health struct {
	mu    sync.Mutex
	until map[config.ModelRef]time.Time
	now   func() time.Time
}

// NewHealth returns an empty registry. now may be nil for the wall clock.
func NewHealth(now func() time.Time) *Health {
	if now == nil {
		now = time.Now
	}
	return &Health{until: map[config.ModelRef]time.Time{}, now: now}
}

// Available reports whether ref may be used now.
func (h *Health) Available(ref config.ModelRef) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	until, ok := h.until[ref]
	return !ok || !h.now().Before(until)
}

// Fail marks ref unavailable for retryAfter, or DefaultUnhealthyFor when the provider named no delay.
func (h *Health) Fail(ref config.ModelRef, retryAfter time.Duration) {
	if retryAfter <= 0 {
		retryAfter = DefaultUnhealthyFor
	}
	h.mu.Lock()
	h.until[ref] = h.now().Add(retryAfter)
	h.mu.Unlock()
}

// Success clears any mark on ref.
func (h *Health) Success(ref config.ModelRef) {
	h.mu.Lock()
	delete(h.until, ref)
	h.mu.Unlock()
}

// Unavailable returns, sorted, the models among refs that are unavailable now. It is the registry's
// contribution to the model.candidates decision.
func (h *Health) Unavailable(refs []config.ModelRef) []config.ModelRef {
	out := []config.ModelRef{}
	h.mu.Lock()
	defer h.mu.Unlock()
	seen := map[config.ModelRef]bool{}
	for _, ref := range refs {
		if seen[ref] {
			continue
		}
		seen[ref] = true
		if until, ok := h.until[ref]; ok && h.now().Before(until) {
			out = append(out, ref)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Decision is one decision-log entry, stored in agent_steps.json under "decisions" (I7, I14).
type Decision struct {
	Kind string `json:"kind"`
	// Inputs holds every value the rule read; replay needs nothing else.
	Inputs json.RawMessage `json:"inputs"`
	Rule   string          `json:"rule"`
	Output json.RawMessage `json:"output"`
}

type modelInputs struct {
	Requested    config.ModelRef `json:"requested"`
	Stored       config.ModelRef `json:"stored"`
	Default      config.ModelRef `json:"default"`
	ConfigDigest string          `json:"configDigest"`
}

type modelOutput struct {
	Ref   config.ModelRef `json:"ref"`
	Write bool            `json:"write"`
}

type candidateLimits struct {
	Context     int  `json:"context"`
	Output      int  `json:"output"`
	ToolCalling bool `json:"toolCalling"`
}

type candidatesInputs struct {
	Session     config.ModelRef                     `json:"session"`
	Failover    []config.ModelRef                   `json:"failover"`
	Models      map[config.ModelRef]candidateLimits `json:"models"`
	Base        int                                 `json:"base"`
	MaxTokens   int                                 `json:"maxTokens"`
	Unavailable []config.ModelRef                   `json:"unavailable"`
}

type candidatesOutput struct {
	Candidates []config.ModelRef `json:"candidates"`
}

type effortInputs struct {
	Default  config.Effort   `json:"default"`
	Override config.Effort   `json:"override"`
	Efforts  []config.Effort `json:"efforts"`
}

type effortOutput struct {
	Requested config.Effort `json:"requested"`
	Sent      config.Effort `json:"sent"`
}

type failoverInputs struct {
	Candidates []candidateRef      `json:"candidates"`
	Failed     int                 `json:"failed"`
	Class      protocol.ErrorClass `json:"class"`
}

type candidateRef struct {
	Ref     config.ModelRef `json:"ref"`
	Context int             `json:"context"`
}

type failoverOutput struct {
	Next int `json:"next"`
}

func newDecision(kind, rule string, inputs, output any) Decision {
	in, err := json.Marshal(inputs)
	if err != nil {
		panic(fmt.Sprintf("decision inputs of %s are not encodable: %v", rule, err)) // a programming error
	}
	out, err := json.Marshal(output)
	if err != nil {
		panic(fmt.Sprintf("decision output of %s is not encodable: %v", rule, err))
	}
	return Decision{Kind: kind, Rule: rule, Inputs: in, Output: out}
}

// ModelDecision records a Start's model resolution. configDigest identifies the configuration the
// default and the catalog were read from.
func ModelDecision(requested, stored, def config.ModelRef, configDigest string) (config.ModelRef, string, Decision) {
	ref, rule := ResolveModelRef(requested, stored, def)
	return ref, rule, newDecision(DecisionModel, rule,
		modelInputs{Requested: requested, Stored: stored, Default: def, ConfigDigest: configDigest},
		modelOutput{Ref: ref, Write: WritesModelRef(rule)})
}

func sliceOrEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func candidateRefs(cands []Candidate) []candidateRef {
	out := make([]candidateRef, len(cands))
	for i, c := range cands {
		out[i] = candidateRef{Ref: c.Ref, Context: c.Context}
	}
	return out
}

// CandidatesDecision computes the candidate list of one step and the decision that records it.
// unavailable is the sorted health snapshot of the models in question (Health.Unavailable).
func CandidatesDecision(session config.ModelRef, failover []config.ModelRef, models map[config.ModelRef]Candidate, base, maxTokens int, unavailable []config.ModelRef) ([]Candidate, Decision) {
	failover = sliceOrEmpty(failover)
	unavailable = sliceOrEmpty(unavailable)
	cands := Candidates(session, failover, models, base, maxTokens, availableExcept(unavailable))
	limits := map[config.ModelRef]candidateLimits{}
	for _, ref := range append([]config.ModelRef{session}, failover...) {
		if c, ok := models[ref]; ok {
			limits[ref] = candidateLimits{Context: c.Context, Output: c.Output, ToolCalling: c.ToolCalling}
		}
	}
	refs := make([]config.ModelRef, len(cands))
	for i, c := range cands {
		refs[i] = c.Ref
	}
	return cands, newDecision(DecisionModel, RuleModelCandidates,
		candidatesInputs{Session: session, Failover: failover, Models: limits, Base: base, MaxTokens: maxTokens, Unavailable: unavailable},
		candidatesOutput{Candidates: refs})
}

func availableExcept(unavailable []config.ModelRef) func(config.ModelRef) bool {
	return func(ref config.ModelRef) bool { return !slices.Contains(unavailable, ref) }
}

// EffortDecision applies EffortFor and ClampEffort for one candidate and records both.
func EffortDecision(def, override config.Effort, supported []config.Effort) (config.Effort, Decision) {
	requested, rule := EffortFor(def, override)
	sent := ClampEffort(requested, supported)
	return sent, newDecision(DecisionEffort, rule,
		effortInputs{Default: def, Override: override, Efforts: sliceOrEmpty(supported)},
		effortOutput{Requested: requested, Sent: sent})
}

// FailoverDecision applies Failover and records the move. next is -1 when the run stops.
func FailoverDecision(cands []Candidate, failed int, class protocol.ErrorClass) (int, Decision) {
	next := Failover(cands, failed, class)
	return next, newDecision(DecisionFailover, RuleFailoverPrefix+string(class),
		failoverInputs{Candidates: candidateRefs(cands), Failed: failed, Class: class},
		failoverOutput{Next: next})
}

// ReplayDecision recomputes one logged decision from its recorded inputs with the same pure functions
// and reports any difference from the recorded rule and output (I7).
func ReplayDecision(d Decision) error {
	switch {
	case d.Kind == DecisionModel && d.Rule == RuleModelCandidates:
		var in candidatesInputs
		var out candidatesOutput
		if err := decodeDecision(d, &in, &out); err != nil {
			return err
		}
		models := map[config.ModelRef]Candidate{}
		for ref, l := range in.Models {
			models[ref] = Candidate{Ref: ref, Context: l.Context, Output: l.Output, ToolCalling: l.ToolCalling}
		}
		got := Candidates(in.Session, in.Failover, models, in.Base, in.MaxTokens, availableExcept(in.Unavailable))
		refs := make([]config.ModelRef, len(got))
		for i, c := range got {
			refs[i] = c.Ref
		}
		if !slices.Equal(refs, out.Candidates) {
			return fmt.Errorf("%s: recomputed %v, logged %v", d.Rule, refs, out.Candidates)
		}
	case d.Kind == DecisionModel:
		var in modelInputs
		var out modelOutput
		if err := decodeDecision(d, &in, &out); err != nil {
			return err
		}
		ref, rule := ResolveModelRef(in.Requested, in.Stored, in.Default)
		if rule != d.Rule || ref != out.Ref || WritesModelRef(rule) != out.Write {
			return fmt.Errorf("model decision: recomputed %s via %s (write=%v), logged %s via %s (write=%v)",
				ref, rule, WritesModelRef(rule), out.Ref, d.Rule, out.Write)
		}
	case d.Kind == DecisionEffort:
		var in effortInputs
		var out effortOutput
		if err := decodeDecision(d, &in, &out); err != nil {
			return err
		}
		requested, rule := EffortFor(in.Default, in.Override)
		sent := ClampEffort(requested, in.Efforts)
		if rule != d.Rule || requested != out.Requested || sent != out.Sent {
			return fmt.Errorf("effort decision: recomputed %q/%q via %s, logged %q/%q via %s",
				requested, sent, rule, out.Requested, out.Sent, d.Rule)
		}
	case d.Kind == DecisionFailover:
		var in failoverInputs
		var out failoverOutput
		if err := decodeDecision(d, &in, &out); err != nil {
			return err
		}
		cands := make([]Candidate, len(in.Candidates))
		for i, c := range in.Candidates {
			cands[i] = Candidate{Ref: c.Ref, Context: c.Context}
		}
		next := Failover(cands, in.Failed, in.Class)
		if d.Rule != RuleFailoverPrefix+string(in.Class) || next != out.Next {
			return fmt.Errorf("failover decision: recomputed next=%d for %s, logged next=%d via %s", next, in.Class, out.Next, d.Rule)
		}
	default:
		return fmt.Errorf("unknown decision kind %q rule %q", d.Kind, d.Rule)
	}
	return nil
}

func decodeDecision(d Decision, in, out any) error {
	if err := json.Unmarshal(d.Inputs, in); err != nil {
		return fmt.Errorf("%s inputs: %w", d.Rule, err)
	}
	if err := json.Unmarshal(d.Output, out); err != nil {
		return fmt.Errorf("%s output: %w", d.Rule, err)
	}
	return nil
}

// VerifyStep checks one agent_steps.json record (I7, I14): every decision replays, and the model that
// answered (modelRef) is the first candidate or the target of a logged failover. A record without a
// modelRef (written before the decision log) is not checked.
func VerifyStep(raw json.RawMessage) error {
	var step struct {
		Step      int64           `json:"step"`
		ModelRef  config.ModelRef `json:"modelRef"`
		Decisions []Decision      `json:"decisions"`
	}
	if err := json.Unmarshal(raw, &step); err != nil {
		return err
	}
	if step.ModelRef == "" {
		return nil
	}
	var current config.ModelRef
	for i, d := range step.Decisions {
		if err := ReplayDecision(d); err != nil {
			return fmt.Errorf("step %d decision %d: %w", step.Step, i, err)
		}
		switch {
		case d.Rule == RuleModelCandidates:
			var out candidatesOutput
			if err := json.Unmarshal(d.Output, &out); err != nil {
				return err
			}
			if len(out.Candidates) == 0 {
				return fmt.Errorf("step %d: model answered although no candidate was logged", step.Step)
			}
			current = out.Candidates[0]
		case d.Kind == DecisionFailover:
			var in failoverInputs
			var out failoverOutput
			if err := decodeDecision(d, &in, &out); err != nil {
				return err
			}
			if out.Next < 0 || out.Next >= len(in.Candidates) {
				return fmt.Errorf("step %d: logged failover to index %d of %d", step.Step, out.Next, len(in.Candidates))
			}
			if in.Failed < 0 || in.Failed >= len(in.Candidates) || in.Candidates[in.Failed].Ref != current {
				return fmt.Errorf("step %d: failover left %d, but the run was on %s", step.Step, in.Failed, current)
			}
			current = in.Candidates[out.Next].Ref
		}
	}
	if current == "" {
		return fmt.Errorf("step %d: no model.candidates decision", step.Step)
	}
	if step.ModelRef != current {
		return fmt.Errorf("step %d: answered by %s, but the decision log leads to %s", step.Step, step.ModelRef, current)
	}
	return nil
}

// refList joins model references for messages.
func refList(refs []config.ModelRef) string {
	parts := make([]string, len(refs))
	for i, r := range refs {
		parts[i] = string(r)
	}
	return strings.Join(parts, ", ")
}
