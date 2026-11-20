package agent

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/window"
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
	DecisionWindow   = "window"
	DecisionTurn     = "turn"
	// DecisionInstructions records that host instructions were appended to the run's system prefix.
	DecisionInstructions = "instructions"
)

// Rule ids of decisions. The pure function that applies a rule returns its id; an id never changes
// meaning. A failover entry's rule is RuleFailoverPrefix followed by the error class that moved the run.
const (
	RuleModelUser       = "model.user"       // AgentOptions.ModelRef differs from sessions.model_ref; written to it
	RuleModelSession    = "model.session"    // sessions.model_ref
	RuleModelAgent      = "model.agent"      // the first model of the agent's own list; written to sessions.model_ref
	RuleModelDefault    = "model.default"    // the config model; written to sessions.model_ref
	RuleModelCandidates = "model.candidates" // the ordered, filtered candidate list of one step
	// RuleModelCandidatesCooldown additionally orders restored candidates by explicit readiness.
	RuleModelCandidatesCooldown = "model.candidates.cooldown"
	// RuleModelCandidatesRetained keeps unhealthy candidates for later first attempts in this step.
	RuleModelCandidatesRetained = "model.candidates.retained"
	RuleEffortOverride          = "effort.override"  // AgentOptions.Effort
	RuleEffortDefault           = "effort.default"   // the candidate's default: its list entry, else the agent's, else config
	RuleEffortEscalated         = "effort.escalated" // config default raised while progress shows a stall
	RuleEffortCapped            = "effort.capped"    // a stall would escalate, but escalation.max allows no higher level
	// RuleEffortAuto: the Start resolved "auto" to one concrete level for this run, before the first
	// request. The classifier input text is deliberately not persisted; the decision records only
	// its source, length, ceiling, judge ref and result.
	RuleEffortAuto = "effort.auto"
	// RuleEffortAutoUnavailable: the candidate's reasoning.efforts list offers nothing at or below
	// effort.auto.max, so no reasoning parameter is sent to it.
	RuleEffortAutoUnavailable = "effort.auto_unavailable"
	RuleFailoverPrefix        = "failover."
	// RuleFailoverRecovery: the failover chain ended in an outage; the step waits and sends the request
	// again to a candidate that failed with availability (Recovery).
	RuleFailoverRecovery = "failover.recovery"
	// RuleFailoverFloorFallback: after all larger candidates failed or exhausted recovery during active
	// gate repair, selects the largest available candidate for controlled Floor Relaxation (ADR-0024 §7, F-03).
	RuleFailoverFloorFallback = "failover.floor_fallback"
	// RuleWindowDropped: older transcript groups were left out to fit the candidate's budget (§5).
	RuleWindowDropped = "window.dropped"
	// RuleWindowDefaultContext: the model configures no limit.context, so the request is budgeted on
	// config.DefaultContext (32 768 tokens); logged with every such request (design §5, R5).
	RuleWindowDefaultContext = "window.default_context"
	// RuleWindowCapacityRetry: after a capacity rejection the transcript budget is halved for the
	// one retry on the same model, and stays so for that model until the run ends (§5).
	RuleWindowCapacityRetry = "window.capacity_retry"
	// RuleWindowPrefix: stateless binary ModeFull / ModeCompact selection for a candidate (ADR-0024 §4).
	RuleWindowPrefix = "window.prefix"
	// RuleWindowFloorRelaxed: controlled Floor Relaxation on the largest available candidate after all
	// larger candidates were exhausted during active gate repair (ADR-0024 §7, F-03).
	RuleWindowFloorRelaxed = "window.floor_relaxed"
	// FloorRelaxReasonMaxCandidateOverflow is the canonical machine reason recorded by RuleWindowFloorRelaxed.
	FloorRelaxReasonMaxCandidateOverflow = "max_candidate_repair_floor_overflow"
	// RuleTurnBoundary: NeedsTurnBoundary held before a request, so a runtime row opened a new turn (§4.6).
	RuleTurnBoundary = "turn.boundary"
	// RuleInstructionsAppended: the agent's instructions were appended as the last system message of
	// every request in this run. The decision records a digest and length, never the text.
	RuleInstructionsAppended = "instructions.appended"
)

// MaxInstructionsBytes bounds an agent's instructions: extra guidance is a few paragraphs, not a
// second system prompt (config.MaxAgentInstructions).
const MaxInstructionsBytes = 16 << 10

// DefaultUnhealthyFor is how long a model stays out of the candidate list after an availability
// failure that carried no Retry-After.
const DefaultUnhealthyFor = 30 * time.Second

// ResolveModelRef picks the model a Start runs on and names the rule (design §4.5, ADR-0009):
//
//	requested differs from stored, or the agent switched → model.user    (re-pins the session)
//	the agent switched and has its own list            → model.agent   (its first model)
//	the agent switched and a config model is set        → model.default (config model)
//	otherwise stored is set                             → model.session
//	otherwise the agent has its own list                → model.agent
//	otherwise                                           → model.default (config model; pins a session that has none)
//
// agentFirst is the first model of the resolved agent's own list, "" when it inherits one; switched is
// AgentSwitched. The result is "" only when nothing applies; the caller fails Start with
// ErrUnknownModel for that and for a reference outside the catalog. It never reads or writes anything.
func ResolveModelRef(requested, stored, agentFirst, def config.ModelRef, switched bool) (config.ModelRef, string) {
	switch {
	case requested != "" && (requested != stored || switched):
		return requested, RuleModelUser
	case switched && agentFirst != "":
		return agentFirst, RuleModelAgent
	case switched && def != "":
		return def, RuleModelDefault
	case stored != "":
		return stored, RuleModelSession
	case agentFirst != "":
		return agentFirst, RuleModelAgent
	default:
		return def, RuleModelDefault
	}
}

// WritesModelRef reports whether a model rule persists its result to sessions.model_ref.
func WritesModelRef(rule string) bool {
	return rule == RuleModelUser || rule == RuleModelAgent || rule == RuleModelDefault
}

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

// Margin is the legacy 5% share kept for v1 Candidates replay compatibility.
func Margin(contextTokens int) int { return contextTokens / 20 }

// GuardTokens returns the closed-form tokenizer and protocol guard reservation for a candidate:
// max(context/16, 512) plus 512 when anthropic-messages sends non-empty tools (ADR-0024 §6).
func GuardTokens(contextTokens int, providerPkg string, toolCount int) int {
	guard := max(contextTokens/16, 512)
	if providerPkg == config.PackageAnthropicMessages && toolCount > 0 {
		guard += 512
	}
	return guard
}

// BaseGateSlack returns the worst-case token reservation for a completion-gate rejection message plus
// ProposalReserveTokens (512): 1950 tokens under compact feedback, 7718 under detailed feedback.
func BaseGateSlack(m Model, fallback PresentationOptions, mode ProjectionMode) int {
	if effectiveFeedbackVariant(m, fallback, mode) == config.ProfileCompact {
		return window.ReserveTokensForBytes(MaxCompactGateJSONBytes) + ProposalReserveTokens
	}
	return window.ReserveTokensForBytes(MaxDetailedGateJSONBytes) + ProposalReserveTokens
}

// RepairStage classifies the active completion-gate repair progression reconstructed from persisted
// messages and tool executions (ADR-0024 §7, R2-02).
type RepairStage string

const (
	RepairStageNone           RepairStage = ""
	RepairStageBlocked        RepairStage = "blocked"
	RepairStageInspecting     RepairStage = "inspecting"
	RepairStageProvenProgress RepairStage = "proven_progress"
	RepairStageUnknownMutated RepairStage = "unknown_mutated"
)

// MaxCausalProgressRefs bounds the post-gate causal execution refs retained in RepairContext.
const MaxCausalProgressRefs = 8

// CausalEvidenceRef records body-free persisted attribution for one causal tool execution in a repair cycle.
// ParentToolCallID is non-empty when the execution ran as a nested callback inside a top-level eval /
// apply_experience call (ADR-0024 R4-01).
type CausalEvidenceRef struct {
	AnsweredSeq      int64            `json:"answeredSeq"`
	ToolCallID       string           `json:"toolCallId,omitempty"`
	ParentToolCallID string           `json:"parentToolCallId,omitempty"`
	ToolName         string           `json:"toolName,omitempty"`
	ExecutionID      string           `json:"executionId,omitempty"`
	PostRef          string           `json:"postRef,omitempty"`
	MutationEvidence MutationEvidence `json:"mutationEvidence"`
}

// RepairContext describes the active completion-gate repair state, pinned gate feedback, and causal
// repair progress derived deterministically from persisted rows (ADR-0024 §4.2, §4.6.6, §7, R2-02).
// All fields are value types so RepairContext is directly comparable via == and allocation-free.
type RepairContext struct {
	Active                  bool
	NewestIsBlock           bool
	BlockSeq                int64
	ProposalSeq             int64
	BlockCount              int
	GateFeedback            string
	FloorIdx                int
	MinRequiredSeq          int64
	OriginalMinRequiredSeq  int64
	PostBlockCausalFloorSeq int64
	Stage                   RepairStage
	HasOriginalEvidence     bool
	OriginalEvidence        CausalEvidenceRef
	PostBlockProvenCount    int
	PostBlockUnknownCount   int
	CausalProgressLen       int
	CausalProgress          [MaxCausalProgressRefs]CausalEvidenceRef
	// PinGateInState is set transiently on a candidate projection attempt when Stage R2/R3 pins
	// GateFeedback + CausalProgress into State and projects Messages strictly over Seq > BlockSeq.
	PinGateInState bool
}

type repairContext = RepairContext

// EffectiveGateSlack computes the gate reservation for the current step: outside active repair,
// min(baseSlack, max(0, (poolBeforeSlack - candFixed) / 4)) (preserving full baseSlack on >=32K
// windows while retaining 75% post-prefix conversation capacity on 8K small windows, F-06); 0
// immediately after a gate block; and reloaded up to baseSlack on subsequent repair steps without
// evicting requiredRepairTokens.
func EffectiveGateSlack(poolBeforeSlack, candFixed, baseSlack, requiredRepairTokens int, repair repairContext) int {
	if !repair.Active {
		free := poolBeforeSlack - candFixed
		if free <= 0 {
			return 0
		}
		return min(baseSlack, free/4)
	}
	if repair.NewestIsBlock {
		return 0
	}
	room := poolBeforeSlack - candFixed - requiredRepairTokens
	if room <= 0 {
		return 0
	}
	return min(baseSlack, room)
}

// CandidatesByStaticPrefix filters the run's configured candidate chain purely by tool-calling support,
// static minimal prefix viability (compactFixed + GuardTokens + OutputBudget <= Context), and advisory
// health availability (ADR-0024 §5). Dynamic window capacity is decided solely by projectCandidate.
func CandidatesByStaticPrefix(
	session config.ModelRef,
	failover []config.ModelRef,
	models map[config.ModelRef]Candidate,
	compactFixed map[config.ModelRef]int,
	guards map[config.ModelRef]int,
	maxTokens int,
	available func(config.ModelRef) bool,
) []Candidate {
	eligible := make([]Candidate, 0, 1+len(failover))
	seen := map[config.ModelRef]bool{}
	for _, ref := range append([]config.ModelRef{session}, failover...) {
		c, ok := models[ref]
		if !ok || seen[ref] || !c.ToolCalling {
			continue
		}
		minNeed := compactFixed[ref] + guards[ref] + OutputBudget(maxTokens, c)
		if c.Context < minNeed {
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

var effortOrder = config.EffortLevels()

func effortIndex(e config.Effort) int { return slices.Index(effortOrder, e) }

// ValidEffort reports whether e is a concrete level or "" (use the default). "auto" is not a level.
func ValidEffort(e config.Effort) bool { return e == "" || effortIndex(e) >= 0 }

// ValidEffortRequest reports whether e may be configured or requested: "" (use the default), one of
// the concrete levels, or "auto" (resolve one level before the run's first request).
func ValidEffortRequest(e config.Effort) bool {
	return e == "" || e == config.EffortAuto || effortIndex(e) >= 0
}

// Legacy effort inputs exist only to replay historical decisions, never to control a live run.
type legacyStall struct {
	SameFailure     int `json:"sameFailure"`
	NoProgressSteps int `json:"noProgressSteps"`
}

type legacyEscalation struct {
	SameFailure     int           `json:"sameFailure"`
	NoProgressSteps int           `json:"noProgressSteps"`
	Max             config.Effort `json:"max"`
}

func legacyEffortFor(def, override config.Effort, esc *legacyEscalation, stall legacyStall) (config.Effort, string) {
	if override != "" {
		return override, RuleEffortOverride
	}
	if def == "" || esc == nil {
		return def, RuleEffortDefault
	}
	reached := func(count, threshold int) bool { return threshold > 0 && count >= threshold }
	levels := 0
	if reached(stall.SameFailure, esc.SameFailure) || reached(stall.NoProgressSteps, esc.NoProgressSteps) {
		levels = 1
	}
	if reached(stall.SameFailure, 2*esc.SameFailure) || reached(stall.NoProgressSteps, 2*esc.NoProgressSteps) {
		levels = 2
	}
	if levels == 0 {
		return def, RuleEffortDefault
	}
	idx := min(effortIndex(def)+levels, effortIndex(esc.Max))
	if idx <= effortIndex(def) {
		return def, RuleEffortCapped
	}
	return effortOrder[idx], RuleEffortEscalated
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

// CappedAutoEffort applies the auto ceiling to a classified level: min(level, ceiling). A ceiling
// that is not a concrete level means the documented default, high; a level that is not concrete
// resolves to "" (send no reasoning parameter).
func CappedAutoEffort(level, ceiling config.Effort) config.Effort {
	if !ceiling.IsLevel() {
		ceiling = config.DefaultAutoMax
	}
	if !level.IsLevel() {
		return ""
	}
	if effortIndex(level) > effortIndex(ceiling) {
		return ceiling
	}
	return level
}

// effortAtOrBelow reports whether supported lists at least one concrete level no higher than
// ceiling.
func effortAtOrBelow(supported []config.Effort, ceiling config.Effort) bool {
	limit := effortIndex(ceiling)
	if limit < 0 {
		return true // no valid ceiling: no restriction
	}
	for _, s := range supported {
		if idx := effortIndex(s); idx >= 0 && idx <= limit {
			return true
		}
	}
	return false
}

// Health is shared by an Engine and never persisted. Advisory failure marks may be bypassed when
// all models are unhealthy; explicit provider cooldowns must still be honoured before sending.
type Health struct {
	mu        sync.Mutex
	until     map[config.ModelRef]time.Time
	notBefore map[config.ModelRef]time.Time
	now       func() time.Time
}

// NewHealth returns an empty registry. now may be nil for the wall clock.
func NewHealth(now func() time.Time) *Health {
	if now == nil {
		now = time.Now
	}
	return &Health{until: map[config.ModelRef]time.Time{}, notBefore: map[config.ModelRef]time.Time{}, now: now}
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
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	if retryAfter > 0 {
		until := now.Add(retryAfter)
		if until.After(h.notBefore[ref]) {
			h.notBefore[ref] = until
		}
	} else {
		retryAfter = DefaultUnhealthyFor
	}
	until := now.Add(retryAfter)
	if until.After(h.until[ref]) {
		h.until[ref] = until
	}
}

// Success clears advisory failure state, but not a concurrent request's unexpired provider hint.
func (h *Health) Success(ref config.ModelRef) {
	h.mu.Lock()
	if until := h.notBefore[ref]; h.now().Before(until) {
		h.until[ref] = until
	} else {
		delete(h.until, ref)
		delete(h.notBefore, ref)
	}
	h.mu.Unlock()
}

// RetryDelay returns only the explicit hint, never the guessed advisory health interval.
func (h *Health) RetryDelay(ref config.ModelRef) time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	if until := h.notBefore[ref]; h.now().Before(until) {
		return until.Sub(h.now())
	}
	return 0
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
	Requested config.ModelRef `json:"requested"`
	Stored    config.ModelRef `json:"stored"`
	// AgentFirst and AgentSwitched are absent in records written before ADR-0009, which replay as
	// an agent without its own list that did not switch.
	AgentFirst    config.ModelRef `json:"agentFirst,omitempty"`
	AgentSwitched bool            `json:"agentSwitched,omitempty"`
	Default       config.ModelRef `json:"default"`
	ConfigDigest  string          `json:"configDigest"`
}

type modelOutput struct {
	Ref   config.ModelRef `json:"ref"`
	Write bool            `json:"write"`
}

// instructionsInputs is what the instructions decision records: a digest and a length, never the
// text, so the decision log cannot become a second copy of what the user typed.
type instructionsInputs struct {
	SHA256 string `json:"sha256"`
	Length int    `json:"length"`
}

type instructionsOutput struct {
	Appended bool `json:"appended"`
}

type candidateLimits struct {
	Context     int  `json:"context"`
	Output      int  `json:"output"`
	ToolCalling bool `json:"toolCalling"`
}

type candidatesInputs struct {
	Session             config.ModelRef                     `json:"session"`
	Failover            []config.ModelRef                   `json:"failover"`
	Models              map[config.ModelRef]candidateLimits `json:"models"`
	Base                int                                 `json:"base"`
	CompactFixedByModel map[config.ModelRef]int             `json:"compactFixedByModel,omitempty"`
	GuardByModel        map[config.ModelRef]int             `json:"guardByModel,omitempty"`
	MaxTokens           int                                 `json:"maxTokens"`
	Unavailable         []config.ModelRef                   `json:"unavailable"`
	CooldownMs          map[config.ModelRef]int64           `json:"cooldownMs,omitempty"`
}

type candidatesOutput struct {
	Candidates []config.ModelRef `json:"candidates"`
}

type effortInputs struct {
	Default    config.Effort     `json:"default"`
	Override   config.Effort     `json:"override"`
	Escalation *legacyEscalation `json:"escalation,omitempty"`
	Stall      legacyStall       `json:"stall"`
	// TaskState identifies the state version used to construct the request.
	TaskState int64           `json:"taskStateVersion"`
	Efforts   []config.Effort `json:"efforts"`
	// Ceiling is effort.auto.max when this run's effort came from auto, "" otherwise. A candidate
	// whose efforts list offers nothing at or below it sends no reasoning parameter.
	Ceiling config.Effort `json:"ceiling,omitempty"`
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
func ModelDecision(requested, stored, agentFirst, def config.ModelRef, switched bool, configDigest string) (config.ModelRef, string, Decision) {
	ref, rule := ResolveModelRef(requested, stored, agentFirst, def, switched)
	return ref, rule, newDecision(DecisionModel, rule,
		modelInputs{Requested: requested, Stored: stored, AgentFirst: agentFirst, AgentSwitched: switched, Default: def, ConfigDigest: configDigest},
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
func CandidatesDecision(session config.ModelRef, failover []config.ModelRef, models map[config.ModelRef]Candidate, base, maxTokens int, unavailable []config.ModelRef, cooldownMs map[config.ModelRef]int64) ([]Candidate, Decision) {
	failover = sliceOrEmpty(failover)
	unavailable = sliceOrEmpty(unavailable)
	cands := Candidates(session, failover, models, base, maxTokens, nil)
	rule := RuleModelCandidates
	if len(unavailable) > 0 || len(cooldownMs) > 0 {
		rule = RuleModelCandidatesRetained
		orderRetainedCandidates(cands, unavailable, cooldownMs)
	}
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
	return cands, newDecision(DecisionModel, rule,
		candidatesInputs{Session: session, Failover: failover, Models: limits, Base: base, MaxTokens: maxTokens, Unavailable: unavailable, CooldownMs: cooldownMs},
		candidatesOutput{Candidates: refs})
}

// CandidatesByStaticPrefixDecision computes and logs static candidate eligibility under ADR-0024.
func CandidatesByStaticPrefixDecision(
	session config.ModelRef,
	failover []config.ModelRef,
	models map[config.ModelRef]Candidate,
	compactFixed map[config.ModelRef]int,
	guards map[config.ModelRef]int,
	maxTokens int,
	unavailable []config.ModelRef,
	cooldownMs map[config.ModelRef]int64,
) ([]Candidate, Decision) {
	failover = sliceOrEmpty(failover)
	unavailable = sliceOrEmpty(unavailable)
	cands := CandidatesByStaticPrefix(session, failover, models, compactFixed, guards, maxTokens, nil)
	rule := RuleModelCandidates
	if len(unavailable) > 0 || len(cooldownMs) > 0 {
		rule = RuleModelCandidatesRetained
		orderRetainedCandidates(cands, unavailable, cooldownMs)
	}
	limits := map[config.ModelRef]candidateLimits{}
	cf := map[config.ModelRef]int{}
	gd := map[config.ModelRef]int{}
	for _, ref := range append([]config.ModelRef{session}, failover...) {
		if c, ok := models[ref]; ok {
			limits[ref] = candidateLimits{Context: c.Context, Output: c.Output, ToolCalling: c.ToolCalling}
			cf[ref] = compactFixed[ref]
			gd[ref] = guards[ref]
		}
	}
	refs := make([]config.ModelRef, len(cands))
	for i, c := range cands {
		refs[i] = c.Ref
	}
	return cands, newDecision(DecisionModel, rule,
		candidatesInputs{
			Session:             session,
			Failover:            failover,
			Models:              limits,
			CompactFixedByModel: cf,
			GuardByModel:        gd,
			MaxTokens:           maxTokens,
			Unavailable:         unavailable,
			CooldownMs:          cooldownMs,
		},
		candidatesOutput{Candidates: refs})
}

// Health changes priority, never membership. Keep each deferred model available to the initial
// sweep once its explicit hint expires; advisory-unhealthy models follow healthy ones.
func orderRetainedCandidates(cands []Candidate, unavailable []config.ModelRef, cooldownMs map[config.ModelRef]int64) {
	slices.SortStableFunc(cands, func(a, b Candidate) int {
		if cooldownMs[a.Ref] < cooldownMs[b.Ref] {
			return -1
		}
		if cooldownMs[a.Ref] > cooldownMs[b.Ref] {
			return 1
		}
		aDown, bDown := slices.Contains(unavailable, a.Ref), slices.Contains(unavailable, b.Ref)
		if aDown != bDown {
			if aDown {
				return 1
			}
			return -1
		}
		return 0
	})
}

// Stable ordering preserves configured priority among equally ready candidates. Cooling models stay
// in the list: a ready fallback can fail, and the deferred primary may then recover within the deadline.
func orderByCooldown(cands []Candidate, cooldownMs map[config.ModelRef]int64) {
	slices.SortStableFunc(cands, func(a, b Candidate) int {
		if cooldownMs[a.Ref] < cooldownMs[b.Ref] {
			return -1
		}
		if cooldownMs[a.Ref] > cooldownMs[b.Ref] {
			return 1
		}
		return 0
	})
}

func availableExcept(unavailable []config.ModelRef) func(config.ModelRef) bool {
	return func(ref config.ModelRef) bool { return !slices.Contains(unavailable, ref) }
}

// EffortDecision applies the explicit override/default and candidate clamp. Historical escalation
// inputs are accepted only by ReplayDecision, never from live configuration or tool outcomes.
func EffortDecision(def, override config.Effort, taskState int64, supported []config.Effort, autoCeiling config.Effort) (config.Effort, Decision) {
	requested, rule := def, RuleEffortDefault
	if override != "" {
		requested, rule = override, RuleEffortOverride
	}
	sent := ClampEffort(requested, supported)
	if autoCeiling != "" && len(supported) > 0 && !effortAtOrBelow(supported, autoCeiling) {
		sent = ""
		rule = RuleEffortAutoUnavailable
	}
	return sent, newDecision(DecisionEffort, rule,
		effortInputs{Default: def, Override: override, TaskState: taskState, Efforts: sliceOrEmpty(supported), Ceiling: autoCeiling},
		effortOutput{Requested: requested, Sent: sent})
}

// FailoverDecision applies Failover and records the move. next is -1 when the run stops.
func FailoverDecision(cands []Candidate, failed int, class protocol.ErrorClass) (int, Decision) {
	next := Failover(cands, failed, class)
	return next, newDecision(DecisionFailover, RuleFailoverPrefix+string(class),
		failoverInputs{Candidates: candidateRefs(cands), Failed: failed, Class: class},
		failoverOutput{Next: next})
}

type floorFallbackInputs struct {
	Candidates  []candidateRef `json:"candidates"`
	Failed      int            `json:"failed"`
	Unavailable []bool         `json:"unavailable"`
}

// SelectMaxAvailableCandidate returns the index of the non-unavailable candidate with the largest
// Context window (ties broken by lowest candidate index), or -1 when all candidates are unavailable.
func SelectMaxAvailableCandidate(cands []Candidate, unavailable []bool) int {
	best := -1
	for i, c := range cands {
		if i < len(unavailable) && unavailable[i] {
			continue
		}
		if best < 0 || c.Context > cands[best].Context {
			best = i
		}
	}
	return best
}

// FloorFallbackDecision records switching from a failed larger candidate to the largest remaining
// available candidate once all larger candidates are exhausted during gate repair (F-03).
func FloorFallbackDecision(cands []Candidate, failed int, unavailable []bool) (int, Decision) {
	unavailCopy := make([]bool, len(cands))
	copy(unavailCopy, unavailable)
	next := SelectMaxAvailableCandidate(cands, unavailCopy)
	return next, newDecision(DecisionFailover, RuleFailoverFloorFallback,
		floorFallbackInputs{Candidates: candidateRefs(cands), Failed: failed, Unavailable: unavailCopy},
		failoverOutput{Next: next})
}

type windowFloorRelaxedInputs struct {
	Ref                       config.ModelRef `json:"ref"`
	Context                   int             `json:"context"`
	UsableInput               int             `json:"usableInput"`
	Fixed                     int             `json:"fixed"`
	OriginalMinRequiredSeq    int64           `json:"originalMinRequiredSeq"`
	PreBlockAnchorSeq         int64           `json:"preBlockAnchorSeq,omitempty"`
	PostBlockCausalFloorSeq   int64           `json:"postBlockCausalFloorSeq,omitempty"`
	RelaxedFloorSeq           int64           `json:"relaxedFloorSeq"`
	BlockSeq                  int64           `json:"blockSeq"`
	NewestGroupFirstSeq       int64           `json:"newestGroupFirstSeq"`
	WindowFirstSeq            int64           `json:"windowFirstSeq"`
	WindowTokens              int             `json:"windowTokens"`
	GatePinnedInState         bool            `json:"gatePinnedInState,omitempty"`
	LargerCandidatesExhausted bool            `json:"largerCandidatesExhausted"`
}

type windowFloorRelaxedOutput struct {
	Relaxed                bool   `json:"relaxed"`
	Reason                 string `json:"reason"`
	OriginalMinRequiredSeq int64  `json:"originalMinRequiredSeq"`
	SentFirstSeq           int64  `json:"sentFirstSeq"`
	GatePinnedInState      bool   `json:"gatePinnedInState,omitempty"`
}

// WindowFloorRelaxedDecision records a controlled Floor Relaxation on the largest available candidate
// when strict MinRequiredSeq exceeded every available model window (ADR-0024 §7, F-03, R2-02).
func WindowFloorRelaxedDecision(
	ref config.ModelRef,
	contextTokens, usableInput, fixed int,
	originalMinReqSeq, preBlockAnchorSeq, postBlockCausalFloorSeq, relaxedFloorSeq, blockSeq, newestGroupFirstSeq, sentFirstSeq int64,
	windowTokens int,
	gatePinnedInState bool,
) Decision {
	return newDecision(DecisionWindow, RuleWindowFloorRelaxed,
		windowFloorRelaxedInputs{
			Ref:                       ref,
			Context:                   contextTokens,
			UsableInput:               usableInput,
			Fixed:                     fixed,
			OriginalMinRequiredSeq:    originalMinReqSeq,
			PreBlockAnchorSeq:         preBlockAnchorSeq,
			PostBlockCausalFloorSeq:   postBlockCausalFloorSeq,
			RelaxedFloorSeq:           relaxedFloorSeq,
			BlockSeq:                  blockSeq,
			NewestGroupFirstSeq:       newestGroupFirstSeq,
			WindowFirstSeq:            sentFirstSeq,
			WindowTokens:              windowTokens,
			GatePinnedInState:         gatePinnedInState,
			LargerCandidatesExhausted: true,
		},
		windowFloorRelaxedOutput{
			Relaxed:                true,
			Reason:                 FloorRelaxReasonMaxCandidateOverflow,
			OriginalMinRequiredSeq: originalMinReqSeq,
			SentFirstSeq:           sentFirstSeq,
			GatePinnedInState:      gatePinnedInState,
		},
	)
}

// EffectiveContext is the context a request is budgeted on: the configured limit.context, else
// config.DefaultContext with rule window.default_context (design §5). configured is 0 when absent.
func EffectiveContext(configured int) (int, string) {
	if configured > 0 {
		return configured, ""
	}
	return config.DefaultContext, RuleWindowDefaultContext
}

type defaultContextInputs struct {
	Ref config.ModelRef `json:"ref"`
	// Configured is the model's limit.context; 0 because it is absent.
	Configured int `json:"configured"`
}

type defaultContextOutput struct {
	Context int `json:"context"`
}

// DefaultContextDecision records that a request to ref is budgeted on the default context because the
// model configures no limit.context; context is the value the budget used.
func DefaultContextDecision(ref config.ModelRef, context int) Decision {
	return newDecision(DecisionWindow, RuleWindowDefaultContext,
		defaultContextInputs{Ref: ref}, defaultContextOutput{Context: context})
}

type windowBudget struct {
	Context int `json:"context"`
	Output  int `json:"output"`
	Margin  int `json:"margin"`
}

type windowDroppedInputs struct {
	Ref    config.ModelRef `json:"ref"`
	Budget windowBudget    `json:"budget"`
	// Fixed is the estimate of System, Task State block and tool specs.
	Fixed int `json:"fixed"`
	// Groups counts the groups from windowStartSeq on; Costs are the costs Select examined, newest first.
	Groups int   `json:"groups"`
	Costs  []int `json:"costs"`
}

type windowDroppedOutput struct {
	Included int `json:"included"`
	Dropped  int `json:"dropped"`
}

// WindowDroppedDecision records the window of one request that left groups out. groups is the
// number of groups from windowStartSeq on, including those the run no longer keeps in memory.
func WindowDroppedDecision(ref config.ModelRef, b window.Budget, fixed, groups int, costs []int) Decision {
	included := window.Select(costs, b.Context-b.Output-b.Margin-fixed)
	return newDecision(DecisionWindow, RuleWindowDropped,
		windowDroppedInputs{Ref: ref, Budget: windowBudget(b), Fixed: fixed, Groups: groups, Costs: sliceOrEmpty(costs)},
		windowDroppedOutput{Included: included, Dropped: groups - included})
}

type capacityRetryInputs struct {
	Ref      config.ModelRef `json:"ref"`
	Budget   windowBudget    `json:"budget"`
	Fixed    int             `json:"fixed"`
	Rejected int             `json:"rejectedTokens"`
}

type capacityRetryOutput struct {
	Margin int `json:"margin"`
}

type windowPrefixInputs struct {
	Ref                config.ModelRef `json:"ref"`
	ToolPref           string          `json:"toolPref,omitempty"`
	UsableFull         int             `json:"usableFull"`
	UsableCompact      int             `json:"usableCompact"`
	FullFixed          int             `json:"fullFixed"`
	CompactFixed       int             `json:"compactFixed"`
	RequiredTranscript int             `json:"requiredTranscript"`
}

type windowPrefixOutput struct {
	Mode   ProjectionMode `json:"mode"`
	Reason string         `json:"reason"`
}

// SelectWindowPrefix evaluates the 4 stateless priority branches for ModeFull vs ModeCompact (ADR-0024 §4).
func SelectWindowPrefix(toolPref string, usableFull, usableCompact, fullFixed, compactFixed, requiredTranscript int) (ProjectionMode, string) {
	if toolPref == config.ProfileCompact {
		return ModeCompact, PrefixReasonCandidateProfile
	}
	if toolPref == "" && fullFixed > usableFull/2 {
		return ModeCompact, PrefixReasonFixedPrefixBudget
	}
	if fullFixed+requiredTranscript > usableFull && compactFixed+requiredTranscript <= usableCompact {
		return ModeCompact, PrefixReasonNewestGroupFit
	}
	return ModeFull, PrefixReasonFitsFull
}

// WindowPrefixDecision selects ModeFull or ModeCompact for ref and records RuleWindowPrefix.
func WindowPrefixDecision(ref config.ModelRef, toolPref string, usableFull, usableCompact, fullFixed, compactFixed, requiredTranscript int) (ProjectionMode, string, Decision) {
	mode, reason := SelectWindowPrefix(toolPref, usableFull, usableCompact, fullFixed, compactFixed, requiredTranscript)
	return mode, reason, newDecision(DecisionWindow, RuleWindowPrefix,
		windowPrefixInputs{
			Ref:                ref,
			ToolPref:           toolPref,
			UsableFull:         usableFull,
			UsableCompact:      usableCompact,
			FullFixed:          fullFixed,
			CompactFixed:       compactFixed,
			RequiredTranscript: requiredTranscript,
		},
		windowPrefixOutput{Mode: mode, Reason: reason})
}

// CapacityRetryDecision computes the budget of the retry after the provider rejected, as too long, a
// request of rejected estimated tokens built under b, and records it.
func CapacityRetryDecision(ref config.ModelRef, b window.Budget, fixed, rejected int) (window.Budget, Decision) {
	retry := b
	retry.Margin = window.RetryMargin(b, fixed, rejected)
	return retry, newDecision(DecisionWindow, RuleWindowCapacityRetry,
		capacityRetryInputs{Ref: ref, Budget: windowBudget(b), Fixed: fixed, Rejected: rejected},
		capacityRetryOutput{Margin: retry.Margin})
}

// TurnRow is what the turn rule reads from one row after the last user row.
type TurnRow struct {
	Role string `json:"role"`
	// Producer is the model that produced an assistant row in this run; "" when this run did not.
	Producer config.ModelRef `json:"producer,omitempty"`
}

// NeedsTurnBoundary is the turn rule of design §4.6: a turn never spans runs or models. Before a
// request to target it holds when the newest row is not a user row and an assistant row after the
// last user row was not produced by target in this run. rows are the rows after the last user row.
// Once a boundary row is appended, every current-turn assistant message was produced by target in
// this run, so its echo, if the adapter returned one, is in run memory.
func NeedsTurnBoundary(rows []TurnRow, target config.ModelRef) bool {
	if len(rows) == 0 || rows[len(rows)-1].Role == "user" {
		return false
	}
	for i := len(rows) - 1; i >= 0 && rows[i].Role != "user"; i-- {
		if rows[i].Role == "assistant" && rows[i].Producer != target {
			return true
		}
	}
	return false
}

type turnInputs struct {
	Rows   []TurnRow       `json:"rows"`
	Target config.ModelRef `json:"target"`
}

type turnOutput struct {
	Origin string `json:"origin"`
}

// TurnBoundary applies the turn rule before a request to target. When a boundary is due it returns
// true and the decision that records the runtime "turn" row the loop appends. The "resume" row of a
// run that starts after a non-user row is Start's (§3.2), so the loop only ever writes "turn" rows.
func TurnBoundary(rows []TurnRow, target config.ModelRef) (bool, Decision) {
	if !NeedsTurnBoundary(rows, target) {
		return false, Decision{}
	}
	return true, newDecision(DecisionTurn, RuleTurnBoundary, turnInputs{Rows: rows, Target: target}, turnOutput{Origin: protocol.OriginTurn})
}

// ReplayDecision recomputes one logged decision from its recorded inputs with the same pure functions
// and reports any difference from the recorded rule and output (I7).
func ReplayDecision(d Decision) error {
	switch {
	case d.Kind == DecisionModel && (d.Rule == RuleModelCandidates || d.Rule == RuleModelCandidatesCooldown || d.Rule == RuleModelCandidatesRetained):
		var in candidatesInputs
		var out candidatesOutput
		if err := decodeDecision(d, &in, &out); err != nil {
			return err
		}
		models := map[config.ModelRef]Candidate{}
		for ref, l := range in.Models {
			models[ref] = Candidate{Ref: ref, Context: l.Context, Output: l.Output, ToolCalling: l.ToolCalling}
		}
		var got []Candidate
		if in.CompactFixedByModel != nil {
			avail := availableExcept(in.Unavailable)
			if d.Rule == RuleModelCandidatesRetained {
				avail = nil
			}
			got = CandidatesByStaticPrefix(in.Session, in.Failover, models, in.CompactFixedByModel, in.GuardByModel, in.MaxTokens, avail)
		} else {
			avail := availableExcept(in.Unavailable)
			if d.Rule == RuleModelCandidatesRetained {
				avail = nil
			}
			got = Candidates(in.Session, in.Failover, models, in.Base, in.MaxTokens, avail)
		}
		if d.Rule == RuleModelCandidatesCooldown {
			orderByCooldown(got, in.CooldownMs)
		}
		if d.Rule == RuleModelCandidatesRetained {
			orderRetainedCandidates(got, in.Unavailable, in.CooldownMs)
		}
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
		ref, rule := ResolveModelRef(in.Requested, in.Stored, in.AgentFirst, in.Default, in.AgentSwitched)
		if rule != d.Rule || ref != out.Ref || WritesModelRef(rule) != out.Write {
			return fmt.Errorf("model decision: recomputed %s via %s (write=%v), logged %s via %s (write=%v)",
				ref, rule, WritesModelRef(rule), out.Ref, d.Rule, out.Write)
		}
	case d.Kind == DecisionEffort && d.Rule == RuleEffortAuto:
		var in autoInputs
		var out autoOutput
		if err := decodeDecision(d, &in, &out); err != nil {
			return err
		}
		// The classifier's input text is not persisted, so replay checks the decision's invariants
		// instead of recomputing the classification (see docs/go-provider.md).
		if in.Source != AutoSourceJudge && in.Source != AutoSourceHeuristic {
			return fmt.Errorf("%s: source %q", d.Rule, in.Source)
		}
		if in.InputLength < 0 {
			return fmt.Errorf("%s: negative input length %d", d.Rule, in.InputLength)
		}
		if !in.Ceiling.IsLevel() || !out.Level.IsLevel() {
			return fmt.Errorf("%s: ceiling %q, level %q", d.Rule, in.Ceiling, out.Level)
		}
		if effortIndex(out.Level) > effortIndex(in.Ceiling) {
			return fmt.Errorf("%s: level %q is above ceiling %q", d.Rule, out.Level, in.Ceiling)
		}
	case d.Kind == DecisionEffort:
		var in effortInputs
		var out effortOutput
		if err := decodeDecision(d, &in, &out); err != nil {
			return err
		}
		requested, rule := legacyEffortFor(in.Default, in.Override, in.Escalation, in.Stall)
		sent := ClampEffort(requested, in.Efforts)
		if in.Ceiling != "" && len(in.Efforts) > 0 && !effortAtOrBelow(in.Efforts, in.Ceiling) {
			sent = ""
			rule = RuleEffortAutoUnavailable
		}
		if rule != d.Rule || requested != out.Requested || sent != out.Sent {
			return fmt.Errorf("effort decision: recomputed %q/%q via %s, logged %q/%q via %s",
				requested, sent, rule, out.Requested, out.Sent, d.Rule)
		}
	case d.Kind == DecisionWindow && d.Rule == RuleWindowDefaultContext:
		var in defaultContextInputs
		var out defaultContextOutput
		if err := decodeDecision(d, &in, &out); err != nil {
			return err
		}
		if context, rule := EffectiveContext(in.Configured); rule != d.Rule || context != out.Context {
			return fmt.Errorf("%s: recomputed context %d via %q, logged %d", d.Rule, context, rule, out.Context)
		}
	case d.Kind == DecisionFailover && d.Rule == RuleFailoverRecovery:
		var in recoveryInputs
		var out recoveryOutput
		if err := decodeDecision(d, &in, &out); err != nil {
			return err
		}
		if len(in.ReadyInMs) != len(in.Candidates) {
			return fmt.Errorf("%s: %d readiness entries for %d candidates", d.Rule, len(in.ReadyInMs), len(in.Candidates))
		}
		if next, wait := replayRecoveryPick(in.ReadyInMs, in.Failed, in.BackoffMs); next != out.Next || wait != out.WaitMs {
			return fmt.Errorf("%s: recomputed next=%d wait=%dms, logged next=%d wait=%dms", d.Rule, next, wait, out.Next, out.WaitMs)
		}
	case d.Kind == DecisionFailover && d.Rule == RuleFailoverFloorFallback:
		var in floorFallbackInputs
		var out failoverOutput
		if err := decodeDecision(d, &in, &out); err != nil {
			return err
		}
		if len(in.Unavailable) != len(in.Candidates) {
			return fmt.Errorf("%s: %d availability entries for %d candidates", d.Rule, len(in.Unavailable), len(in.Candidates))
		}
		cands := make([]Candidate, len(in.Candidates))
		for i, c := range in.Candidates {
			cands[i] = Candidate{Ref: c.Ref, Context: c.Context}
		}
		next := SelectMaxAvailableCandidate(cands, in.Unavailable)
		if next < 0 || next != out.Next {
			return fmt.Errorf("%s: recomputed next=%d, logged next=%d", d.Rule, next, out.Next)
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
	case d.Kind == DecisionWindow && d.Rule == RuleWindowDropped:
		var in windowDroppedInputs
		var out windowDroppedOutput
		if err := decodeDecision(d, &in, &out); err != nil {
			return err
		}
		got := WindowDroppedDecision(in.Ref, window.Budget(in.Budget), in.Fixed, in.Groups, in.Costs)
		var want windowDroppedOutput
		if err := json.Unmarshal(got.Output, &want); err != nil {
			return err
		}
		if want != out {
			return fmt.Errorf("%s: recomputed %+v, logged %+v", d.Rule, want, out)
		}
	case d.Kind == DecisionWindow && d.Rule == RuleWindowPrefix:
		var in windowPrefixInputs
		var out windowPrefixOutput
		if err := decodeDecision(d, &in, &out); err != nil {
			return err
		}
		mode, reason := SelectWindowPrefix(in.ToolPref, in.UsableFull, in.UsableCompact, in.FullFixed, in.CompactFixed, in.RequiredTranscript)
		if mode != out.Mode || reason != out.Reason {
			return fmt.Errorf("%s: recomputed %s/%s, logged %s/%s", d.Rule, mode, reason, out.Mode, out.Reason)
		}
	case d.Kind == DecisionWindow && d.Rule == RuleWindowFloorRelaxed:
		var in windowFloorRelaxedInputs
		var out windowFloorRelaxedOutput
		if err := decodeDecision(d, &in, &out); err != nil {
			return err
		}
		if !in.LargerCandidatesExhausted {
			return fmt.Errorf("%s: larger candidates were not exhausted", d.Rule)
		}
		if in.OriginalMinRequiredSeq < 0 || in.RelaxedFloorSeq < 0 || in.BlockSeq < 0 || in.NewestGroupFirstSeq < 0 {
			return fmt.Errorf("%s: invalid floor seqs orig=%d relaxed=%d block=%d newest=%d",
				d.Rule, in.OriginalMinRequiredSeq, in.RelaxedFloorSeq, in.BlockSeq, in.NewestGroupFirstSeq)
		}
		if in.WindowFirstSeq <= in.OriginalMinRequiredSeq {
			return fmt.Errorf("%s: windowFirstSeq %d did not relax originalMinRequiredSeq %d",
				d.Rule, in.WindowFirstSeq, in.OriginalMinRequiredSeq)
		}
		if in.WindowFirstSeq > in.RelaxedFloorSeq || in.WindowFirstSeq > in.NewestGroupFirstSeq {
			return fmt.Errorf("%s: windowFirstSeq %d evicted relaxed floor (relaxed=%d newest=%d)",
				d.Rule, in.WindowFirstSeq, in.RelaxedFloorSeq, in.NewestGroupFirstSeq)
		}
		if !in.GatePinnedInState && in.WindowFirstSeq > in.BlockSeq {
			return fmt.Errorf("%s: windowFirstSeq %d evicted blockSeq %d without gatePinnedInState",
				d.Rule, in.WindowFirstSeq, in.BlockSeq)
		}
		if in.GatePinnedInState && in.WindowFirstSeq <= in.BlockSeq {
			return fmt.Errorf("%s: windowFirstSeq %d retained blockSeq %d while gatePinnedInState=true",
				d.Rule, in.WindowFirstSeq, in.BlockSeq)
		}
		if !out.Relaxed ||
			out.Reason != FloorRelaxReasonMaxCandidateOverflow ||
			out.OriginalMinRequiredSeq != in.OriginalMinRequiredSeq ||
			out.SentFirstSeq != in.WindowFirstSeq ||
			out.GatePinnedInState != in.GatePinnedInState {
			return fmt.Errorf("%s: inconsistent output %+v for inputs %+v", d.Rule, out, in)
		}
	case d.Kind == DecisionWindow && d.Rule == RuleWindowCapacityRetry:
		var in capacityRetryInputs
		var out capacityRetryOutput
		if err := decodeDecision(d, &in, &out); err != nil {
			return err
		}
		if margin := window.RetryMargin(window.Budget(in.Budget), in.Fixed, in.Rejected); margin != out.Margin {
			return fmt.Errorf("%s: recomputed margin %d, logged %d", d.Rule, margin, out.Margin)
		}
	case d.Kind == DecisionAgent:
		if err := replayAgent(d); err != nil {
			return err
		}
	case d.Kind == DecisionInstructions:
		var in instructionsInputs
		var out instructionsOutput
		if err := decodeDecision(d, &in, &out); err != nil {
			return err
		}
		// The text is deliberately not persisted, so replay checks the decision's invariants: a
		// lowercase hex SHA-256, a length within the bound, and that it was appended.
		if len(in.SHA256) != 64 {
			return fmt.Errorf("%s: digest %q is not a SHA-256", d.Rule, in.SHA256)
		}
		if _, err := hex.DecodeString(in.SHA256); err != nil {
			return fmt.Errorf("%s: digest %q is not hex", d.Rule, in.SHA256)
		}
		if in.Length < 0 || in.Length > MaxInstructionsBytes {
			return fmt.Errorf("%s: length %d is outside 0..%d", d.Rule, in.Length, MaxInstructionsBytes)
		}
		if d.Rule != RuleInstructionsAppended || !out.Appended {
			return fmt.Errorf("%s: rule %q appended=%v", d.Kind, d.Rule, out.Appended)
		}
	case d.Kind == DecisionTurn:
		var in turnInputs
		var out turnOutput
		if err := decodeDecision(d, &in, &out); err != nil {
			return err
		}
		if due, _ := TurnBoundary(in.Rows, in.Target); d.Rule != RuleTurnBoundary || !due || out.Origin != protocol.OriginTurn {
			return fmt.Errorf("turn decision: recomputed due=%v, logged %q via %s", due, out.Origin, d.Rule)
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
// answered (modelRef) is the first candidate or the target of a logged failover. A failover must move
// within the candidate list the step logged, and when the record carries the Start's model decision,
// the candidate list must start from the model that decision resolved. A record without a modelRef
// (written before the decision log) is not checked.
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
	var current, resolved config.ModelRef
	var logged []config.ModelRef
	for i, d := range step.Decisions {
		if err := ReplayDecision(d); err != nil {
			return fmt.Errorf("step %d decision %d: %w", step.Step, i, err)
		}
		switch {
		case d.Rule == RuleModelCandidates || d.Rule == RuleModelCandidatesCooldown || d.Rule == RuleModelCandidatesRetained:
			var in candidatesInputs
			var out candidatesOutput
			if err := decodeDecision(d, &in, &out); err != nil {
				return err
			}
			if resolved != "" && in.Session != resolved {
				return fmt.Errorf("step %d: candidates start from %s, but the Start resolved %s", step.Step, in.Session, resolved)
			}
			if len(out.Candidates) == 0 {
				return fmt.Errorf("step %d: model answered although no candidate was logged", step.Step)
			}
			logged, current = out.Candidates, out.Candidates[0]
		case d.Kind == DecisionModel:
			var in modelInputs
			var out modelOutput
			if err := decodeDecision(d, &in, &out); err != nil {
				return err
			}
			resolved = out.Ref
		case d.Kind == DecisionFailover:
			var in failoverInputs
			var out failoverOutput
			if err := decodeDecision(d, &in, &out); err != nil {
				return err
			}
			refs := make([]config.ModelRef, len(in.Candidates))
			for j, c := range in.Candidates {
				refs[j] = c.Ref
			}
			if !slices.Equal(refs, logged) {
				return fmt.Errorf("step %d: failover over %v, but the step logged candidates %v", step.Step, refs, logged)
			}
			if out.Next < 0 || out.Next >= len(in.Candidates) {
				return fmt.Errorf("step %d: logged failover to index %d of %d", step.Step, out.Next, len(in.Candidates))
			}
			if in.Failed < 0 || in.Failed >= len(in.Candidates) || in.Candidates[in.Failed].Ref != current {
				return fmt.Errorf("step %d: failover left %d, but the run was on %s", step.Step, in.Failed, current)
			}
			current = in.Candidates[out.Next].Ref
		case d.Kind == DecisionWindow && d.Rule == RuleWindowFloorRelaxed:
			var in windowFloorRelaxedInputs
			var out windowFloorRelaxedOutput
			if err := decodeDecision(d, &in, &out); err != nil {
				return err
			}
			if current != "" && in.Ref != current {
				return fmt.Errorf("step %d: %s logged for %s while active candidate was %s",
					step.Step, d.Rule, in.Ref, current)
			}
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
