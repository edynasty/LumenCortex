package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/agent"
	"github.com/edynasty/LumenCortex/internal/evaluator"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/taskstate"
	"github.com/edynasty/LumenCortex/internal/verify"
	"github.com/edynasty/LumenCortex/protocol"
)

// This file is the runtime side of the verification system: the public lifecycle-hook surface, the
// semantic verifier over host-approved semantic checks, the single independent reviewer over the
// real provider path, and the durable claim/verification host API. The loop owns when each runs;
// this file owns what it reads and what it writes.

// LifecycleEvent is a lifecycle point a host hook observes (spec §20). The values are stable.
type LifecycleEvent string

const (
	LifecyclePreToolUse     LifecycleEvent = "PreToolUse"
	LifecyclePostToolUse    LifecycleEvent = "PostToolUse"
	LifecyclePreEval        LifecycleEvent = "PreEval"
	LifecyclePostEval       LifecycleEvent = "PostEval"
	LifecyclePreSubagent    LifecycleEvent = "PreSubagent"
	LifecyclePostSubagent   LifecycleEvent = "PostSubagent"
	LifecyclePreVerify      LifecycleEvent = "PreVerify"
	LifecyclePostVerify     LifecycleEvent = "PostVerify"
	LifecycleOnFailure      LifecycleEvent = "OnFailure"
	LifecycleOnCancel       LifecycleEvent = "OnCancel"
	LifecycleOnTimeout      LifecycleEvent = "OnTimeout"
	LifecycleOnSessionClose LifecycleEvent = "OnSessionClose"
	LifecycleStop           LifecycleEvent = "Stop"
	LifecycleCompletion     LifecycleEvent = "Completion"
)

// LifecycleEvents lists every event in documentation order.
func LifecycleEvents() []LifecycleEvent {
	return []LifecycleEvent{
		LifecyclePreToolUse, LifecyclePostToolUse, LifecyclePreEval, LifecyclePostEval,
		LifecyclePreSubagent, LifecyclePostSubagent, LifecyclePreVerify, LifecyclePostVerify,
		LifecycleOnFailure, LifecycleOnCancel, LifecycleOnTimeout, LifecycleOnSessionClose,
		LifecycleStop, LifecycleCompletion,
	}
}

// HookDecision is what a host hook decided.
type HookDecision string

const (
	HookAllow    HookDecision = "allow"
	HookContinue HookDecision = "continue"
	HookDeny     HookDecision = "deny"
)

// HookFailurePolicy is what an event does when a hook errors or times out. The zero value is
// fail-closed, and a safety hook is always fail-closed.
type HookFailurePolicy string

const (
	HookFailClosed HookFailurePolicy = "fail_closed"
	HookFailOpen   HookFailurePolicy = "fail_open"
)

// LifecycleHook is a host-supplied, versioned lifecycle hook. A hook may allow, deny, continue or
// carry bounded feedback; it may not change the run's policy or authority.
type LifecycleHook interface {
	ID() string
	Version() int
	Events() []LifecycleEvent
	Priority() int
	Timeout() time.Duration
	FailurePolicy() HookFailurePolicy
	MutableArgs() bool
	Safety() bool
	Handle(ctx context.Context, req LifecycleRequest) (LifecycleResult, error)
}

// LifecycleRequest is one event as a hook sees it. It never carries hidden reasoning.
type LifecycleRequest struct {
	Event      LifecycleEvent       `json:"event"`
	SessionID  string               `json:"sessionId,omitempty"`
	RunID      string               `json:"runId,omitempty"`
	Step       int64                `json:"step,omitempty"`
	ToolCallID string               `json:"toolCallId,omitempty"`
	ToolName   string               `json:"toolName,omitempty"`
	ToolArgs   map[string]any       `json:"toolArgs,omitempty"`
	ToolResult *protocol.ToolResult `json:"toolResult,omitempty"`
	Feedback   string               `json:"feedback,omitempty"`
	Detail     map[string]any       `json:"detail,omitempty"`
}

// LifecycleResult is what a hook returns.
type LifecycleResult struct {
	Decision HookDecision   `json:"decision"`
	Reason   string         `json:"reason,omitempty"`
	Feedback string         `json:"feedback,omitempty"`
	Args     map[string]any `json:"args,omitempty"`
}

// LifecycleAudit is one hook's contribution to an event.
type LifecycleAudit struct {
	Hook        string            `json:"hook"`
	Version     int               `json:"version"`
	Event       LifecycleEvent    `json:"event"`
	Decision    HookDecision      `json:"decision"`
	Reason      string            `json:"reason,omitempty"`
	Error       string            `json:"error,omitempty"`
	ElapsedMS   int64             `json:"elapsedMs"`
	FailPolicy  HookFailurePolicy `json:"failPolicy,omitempty"`
	Overridden  string            `json:"overridden,omitempty"`
	ArgsIgnored bool              `json:"argsIgnored,omitempty"`
}

// LifecycleOutcome is a hook event's merged decision.
type LifecycleOutcome struct {
	Decision         HookDecision     `json:"decision"`
	Feedback         []string         `json:"feedback,omitempty"`
	Args             map[string]any   `json:"args,omitempty"`
	Audit            []LifecycleAudit `json:"audit,omitempty"`
	StopLimitReached bool             `json:"stopLimitReached,omitempty"`
}

// Denied reports a blocking outcome.
func (o LifecycleOutcome) Denied() bool { return o.Decision == HookDeny }

// FeedbackText joins the bounded feedback entries.
func (o LifecycleOutcome) FeedbackText() string { return strings.Join(o.Feedback, "\n") }

// hookAdapter implements verify.Hook over a public LifecycleHook.
type hookAdapter struct{ hook LifecycleHook }

func (a hookAdapter) ID() string             { return a.hook.ID() }
func (a hookAdapter) Version() int           { return a.hook.Version() }
func (a hookAdapter) Priority() int          { return a.hook.Priority() }
func (a hookAdapter) Timeout() time.Duration { return a.hook.Timeout() }
func (a hookAdapter) MutableArgs() bool      { return a.hook.MutableArgs() }
func (a hookAdapter) Safety() bool           { return a.hook.Safety() }
func (a hookAdapter) FailPolicy() verify.FailPolicy {
	if a.hook.FailurePolicy() == HookFailOpen {
		return verify.FailOpen
	}
	return verify.FailClosed
}

func (a hookAdapter) Events() []verify.Event {
	out := make([]verify.Event, 0, len(a.hook.Events()))
	for _, event := range a.hook.Events() {
		out = append(out, verify.Event(event))
	}
	return out
}

func (a hookAdapter) Run(ctx context.Context, req verify.Request) (verify.Result, error) {
	result, err := a.hook.Handle(ctx, LifecycleRequest{
		Event: LifecycleEvent(req.Event), SessionID: req.SessionID, RunID: req.RunID, Step: req.Step,
		ToolCallID: req.ToolCallID, ToolName: req.ToolName, ToolArgs: req.ToolArgs,
		ToolResult: req.ToolResult, Feedback: req.Feedback, Detail: req.Detail,
	})
	if err != nil {
		return verify.Result{}, err
	}
	return verify.Result{Decision: verify.Decision(result.Decision), Reason: result.Reason,
		Feedback: result.Feedback, Args: result.Args}, nil
}

// RegisterLifecycleHook adds a host hook. It is refused before any run starts when the hook is
// invalid or its id is taken; the registry itself is created on first use.
func (e *Engine) RegisterLifecycleHook(hook LifecycleHook) error {
	if hook == nil {
		return errors.New("lifecycle hook is required")
	}
	e.lifecycleHooksMu.Lock()
	defer e.lifecycleHooksMu.Unlock()
	if e.lifecycleHooks == nil {
		registry, err := verify.NewRegistry(verify.Version)
		if err != nil {
			return err
		}
		e.lifecycleHooks = registry
	}
	return e.lifecycleHooks.Register(hookAdapter{hook: hook})
}

// LifecycleHooks lists the registered hooks in execution order.
func (e *Engine) LifecycleHooks() []LifecycleHookInfo {
	e.lifecycleHooksMu.RLock()
	registry := e.lifecycleHooks
	e.lifecycleHooksMu.RUnlock()
	if registry == nil {
		return nil
	}
	infos := registry.Hooks()
	out := make([]LifecycleHookInfo, 0, len(infos))
	for _, info := range infos {
		events := make([]LifecycleEvent, 0, len(info.Events))
		for _, event := range info.Events {
			events = append(events, LifecycleEvent(event))
		}
		out = append(out, LifecycleHookInfo{ID: info.ID, Version: info.Version, Events: events,
			Priority: info.Priority, TimeoutMS: info.Timeout, FailurePolicy: HookFailurePolicy(info.Policy),
			MutableArgs: info.Mutable, Safety: info.Safety})
	}
	return out
}

// LifecycleHookInfo is one registered hook's declaration.
type LifecycleHookInfo struct {
	ID            string            `json:"id"`
	Version       int               `json:"version"`
	Events        []LifecycleEvent  `json:"events"`
	Priority      int               `json:"priority"`
	TimeoutMS     int64             `json:"timeoutMs"`
	FailurePolicy HookFailurePolicy `json:"failurePolicy"`
	MutableArgs   bool              `json:"mutableArgs"`
	Safety        bool              `json:"safety"`
}

// hookRegistryForRun returns the registry a run uses, nil when none is registered.
func (e *Engine) hookRegistryForRun() *verify.Registry {
	e.lifecycleHooksMu.RLock()
	defer e.lifecycleHooksMu.RUnlock()
	return e.lifecycleHooks
}

// DispatchExecutionHook runs one lifecycle event through the Engine's hooks. The host's Eval entry
// points call it around an execution, so a UI execution cannot bypass a configured safety hook. An
// Engine without hooks returns an allow outcome.
func (e *Engine) DispatchExecutionHook(ctx context.Context, req LifecycleRequest) (LifecycleOutcome, error) {
	registry := e.hookRegistryForRun()
	if registry == nil {
		return LifecycleOutcome{Decision: HookAllow}, nil
	}
	out := registry.Decide(ctx, verify.Request{
		Version: registry.Version, Event: verify.Event(req.Event), SessionID: req.SessionID,
		RunID: req.RunID, Step: req.Step, ToolCallID: req.ToolCallID, ToolName: req.ToolName,
		ToolArgs: req.ToolArgs, ToolResult: req.ToolResult, Feedback: req.Feedback, Detail: req.Detail,
	})
	converted := LifecycleOutcome{Decision: HookDecision(out.Decision), Feedback: out.Feedback,
		Args: out.Args, StopLimitReached: out.StopLimitReached}
	for _, entry := range out.Audit {
		converted.Audit = append(converted.Audit, LifecycleAudit{
			Hook: entry.Hook, Version: entry.Version, Event: LifecycleEvent(entry.Event),
			Decision: HookDecision(entry.Decision), Reason: entry.Reason, Error: entry.Err,
			ElapsedMS: entry.ElapsedMS, FailPolicy: HookFailurePolicy(entry.FailPolicy),
			Overridden: entry.Overridden, ArgsIgnored: entry.ArgsIgnored,
		})
		// The host's own Eval path publishes the same bounded audit the loop does, so a UI execution
		// is observable exactly like an agent execution.
		data := map[string]any{
			"hook": entry.Hook, "hookVersion": entry.Version, "event": string(entry.Event),
			"decision": string(entry.Decision), "elapsedMs": entry.ElapsedMS,
		}
		if entry.Reason != "" {
			data["reason"] = entry.Reason
		}
		if entry.Err != "" {
			data["error"] = entry.Err
		}
		if entry.FailPolicy != "" {
			data["failPolicy"] = string(entry.FailPolicy)
		}
		if entry.Overridden != "" {
			data["overridden"] = entry.Overridden
		}
		if entry.ArgsIgnored {
			data["argsIgnored"] = true
		}
		e.events.publish("hook."+string(entry.Event), req.SessionID, data)
		// The diagnostics journal keeps only the whitelisted identifiers: no arguments, no tool
		// output and no model text ever reach it. hookId and decision name what decided; the event
		// name is the journal row's type.
		_ = e.store.Journal(context.Background(), "hook."+string(entry.Event), map[string]any{
			"sessionId": req.SessionID,
			"hookId":    entry.Hook,
			"decision":  string(entry.Decision),
		})
	}
	return converted, nil
}

// engineVerifier decides the semantic level from the host's approved semantic checks. It runs the
// same evaluator the completion gate uses, so a semantic check is a real command or schema, never a
// model's claim. It never calls a model.
type engineVerifier struct {
	engine    *Engine
	sessionID string
	checks    runChecks
}

func (v engineVerifier) Verify(ctx context.Context, req verify.VerifyRequest) ([]verify.Verdict, error) {
	all, _ := v.checks.Configured()
	specs := v.engine.mustApprovedSpecs(ctx)
	var semanticIDs []string
	for _, id := range all {
		if spec, ok := findSpec(specs, id); ok && spec.Kind == evaluator.KindSemantic {
			semanticIDs = append(semanticIDs, id)
		}
	}
	if len(semanticIDs) == 0 {
		return nil, nil
	}
	verdicts, err := v.checks.Decide(ctx, semanticIDs, "")
	if err != nil {
		return nil, err
	}
	_, semantic := agent.CheckVerdicts(verdicts)
	for i := range semantic {
		semantic[i].Source = verify.SourceApprovedSpec
	}
	return semantic, nil
}

// mustApprovedSpecs reads the current approved specs, nil when they cannot be read. The verifier
// treats an unreadable set as no semantic evidence, never as a pass.
func (e *Engine) mustApprovedSpecs(ctx context.Context) []evaluator.Spec {
	specs, _, _, err := e.approvedChecks(ctx)
	if err != nil {
		return nil
	}
	return specs
}

// VerificationOptions configures a run's completion gate (spec §24-25). nil keeps the existing gate:
// required checks decide execution, and no semantic level is required.
type VerificationOptions struct {
	// RequireSemantic makes a semantic-level decision a condition of completion. A required claim in
	// Task State also turns it on.
	RequireSemantic bool `json:"requireSemantic,omitempty"`
	// RequireIndependent makes at least one independent source a condition of the semantic level.
	RequireIndependent bool `json:"requireIndependent,omitempty"`
	// MaxRepairs bounds the feedback → repair → recheck loop. Zero takes the runtime default.
	MaxRepairs int `json:"maxRepairs,omitempty"`
	// Review enables the single independent review, when ReviewOptions is also enabled.
	Review bool `json:"review,omitempty"`
}

// ReviewOptions opts a run into the single independent review (spec §22). It is off by default.
type ReviewOptions struct {
	Enabled bool `json:"enabled,omitempty"`
	// Model is the reviewer's catalog model. Empty uses the run's session model; a reviewer should
	// normally be a different model so the review is not the same context checking itself.
	Model config.ModelRef `json:"model,omitempty"`
}

// providerReviewer is the real provider path of the independent review: one read-only request in a
// fresh context, admitted and settled like any other model request, with its usage and cost
// reported. It never shares the run's transcript.
type providerReviewer struct {
	engine    *Engine
	sessionID string
	runID     string
	model     config.ModelRef
	timeout   time.Duration
}

// reviewTimeout bounds one review request.
const reviewTimeout = 120 * time.Second

const reviewSystemPrompt = `You are an independent reviewer. You receive a task's stated goal, the agent's final answer, its explicit claims and the verification evidence that was decided. You do not see the agent's transcript or reasoning, and you must not assume work was done because it was claimed.
Check three things only:
1. Does the final answer address the stated goal?
2. Is every required claim backed by evidence from a check, evaluation, review or official verifier rather than by the agent's own assertion?
3. Does the answer overstate what was verified (for example calling a result semantically correct because a program ran)?
Reply with one JSON object and nothing else:
{"passed": true|false, "findings": [{"severity":"blocker|major|minor|note","issue":"...","suggestion":"...","evidence":["check:..."]}]}
Set passed true only when there is no blocker or major finding.`

func (r providerReviewer) Review(ctx context.Context, req verify.ReviewRequest) (verify.ReviewResult, error) {
	cfg, err := config.Load(r.engine.workspace)
	if err != nil {
		return verify.ReviewResult{}, err
	}
	eff := cfg.Effective()
	ref := r.model
	if ref == "" {
		ref = eff.Model
	}
	if ref == "" {
		return verify.ReviewResult{}, ErrUnknownModel
	}
	resolved, err := cfg.Resolve(ref)
	if err != nil {
		return verify.ReviewResult{}, err
	}
	client, err := r.engine.providerFactory(resolved)
	if err != nil {
		return verify.ReviewResult{}, err
	}
	payload, err := json.Marshal(map[string]any{
		"goal": req.Goal, "final": truncateText(req.Final, 16<<10),
		"claims": claimViews(req.Claims), "verdicts": verdictViews(req.Verdicts),
		"evidence": req.Evidence,
	})
	if err != nil {
		return verify.ReviewResult{}, err
	}
	request := protocol.ProviderRequest{
		Messages: []protocol.Message{
			{Role: "system", Content: reviewSystemPrompt},
			{Role: "user", Content: string(payload)},
		},
		MaxTokens: 2048,
	}
	timeout := r.timeout
	if timeout <= 0 || timeout > reviewTimeout {
		timeout = reviewTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	requestID, err := r.admit(ctx, resolved)
	if err != nil {
		return verify.ReviewResult{}, err
	}
	started := time.Now()
	response, callErr := client.Complete(callCtx, request)
	elapsed := time.Since(started)
	result := verify.ReviewResult{Source: verify.SourceIndependent, RequestID: requestID, Model: string(ref), At: time.Now().UTC()}
	result.Usage = response.Usage
	if resolved.Cost != nil {
		result.CostKnown = true
		result.CostUSD = float64(response.Usage.PromptTokens)*resolved.Cost.Input/1e6 +
			float64(response.Usage.CompletionTokens)*resolved.Cost.Output/1e6
	}
	if callErr != nil {
		// The request was admitted and may have been delivered: it is settled unknown, never failed.
		reason := "error"
		if callCtx.Err() != nil {
			reason = "timeout"
		}
		_ = r.settle(requestID, session.RequestUnknown, reason, nil)
		return result, callErr
	}
	parsed, parseErr := parseReview(response.Message.Content)
	if parseErr != nil {
		_ = r.settle(requestID, session.RequestUnknown, "unparsable", nil)
		return result, parseErr
	}
	parsed.Source, parsed.RequestID, parsed.Model, parsed.Usage, parsed.At = result.Source, result.RequestID, result.Model, result.Usage, result.At
	parsed.CostUSD, parsed.CostKnown = result.CostUSD, result.CostKnown
	if err := r.settle(requestID, session.RequestAnswered, "", nil); err != nil {
		return result, err
	}
	_ = elapsed
	return parsed, nil
}

// admit records the review request before it is sent (ADR-0018). A fenced session writes a failed
// envelope and the review is refused.
func (r providerReviewer) admit(ctx context.Context, resolved config.ResolvedModel) (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	id := "rq-" + hex.EncodeToString(buf)
	adm := session.RequestAdmission{
		RequestID: id, SessionID: r.sessionID, RunID: r.runID, Purpose: session.PurposeReview,
		LineageComplete: true,
	}
	if _, err := r.engine.store.AdmitRequest(ctx, adm); err != nil {
		return "", err
	}
	return id, nil
}

func (r providerReviewer) settle(requestID string, to session.RequestState, reason string, decision json.RawMessage) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return r.engine.store.SettleRequest(ctx, requestID, session.Settlement{To: to, Reason: reason, Decision: decision})
}

// parseReview reads the reviewer's strict JSON answer.
func parseReview(content string) (verify.ReviewResult, error) {
	text := strings.TrimSpace(content)
	if start := strings.Index(text, "{"); start > 0 {
		text = text[start:]
	}
	if end := strings.LastIndex(text, "}"); end >= 0 && end+1 < len(text) {
		text = text[:end+1]
	}
	var raw struct {
		Passed   bool                   `json:"passed"`
		Findings []verify.ReviewFinding `json:"findings"`
	}
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		return verify.ReviewResult{}, fmt.Errorf("review reply is not the required JSON object: %w", err)
	}
	return verify.ReviewResult{Passed: raw.Passed, Findings: raw.Findings}, nil
}

// claimViews renders claims for a reviewer without any hidden state.
func claimViews(claims []verify.Claim) []map[string]any {
	out := make([]map[string]any, 0, len(claims))
	for _, c := range claims {
		out = append(out, map[string]any{
			"id": c.ID, "kind": c.Kind, "statement": c.Statement, "source": c.Source.Kind,
			"status": c.Status, "required": c.Required, "evidence": c.Evidence,
		})
	}
	return out
}

// verdictViews renders verification verdicts for a reviewer.
func verdictViews(verdicts []verify.Verdict) []map[string]any {
	out := make([]map[string]any, 0, len(verdicts))
	for _, v := range verdicts {
		out = append(out, map[string]any{"level": v.Level, "source": v.Source, "check": v.Check,
			"status": v.Status, "evidence": v.Evidence})
	}
	return out
}

func truncateText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := text[:limit]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + "…"
}

// revokeEvalAgent drops every grant an agent holds. A child's EXECUTOR lease on its parent's kernel
// ends with the child's run; the kernel and its owner's authority are untouched.
func (e *Engine) revokeEvalAgent(ctx context.Context, agentID string) error {
	e.evalMu.Lock()
	manager := e.evalManager
	e.evalMu.Unlock()
	if manager == nil {
		return nil
	}
	return manager.RevokeAgent(ctx, agentID)
}

// ClaimView is a claim as a host reads it.
type ClaimView struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"`
	Statement   string    `json:"statement"`
	SourceKind  string    `json:"sourceKind"`
	SourceRef   string    `json:"sourceRef,omitempty"`
	Status      string    `json:"status"`
	Required    bool      `json:"required,omitempty"`
	Evidence    []string  `json:"evidence,omitempty"`
	Fingerprint string    `json:"fingerprint,omitempty"`
	At          time.Time `json:"at"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

func claimView(c verify.Claim) ClaimView {
	return ClaimView{ID: c.ID, Kind: string(c.Kind), Statement: c.Statement, SourceKind: c.Source.Kind,
		SourceRef: c.Source.Ref, Status: string(c.Status), Required: c.Required, Evidence: c.Evidence,
		Fingerprint: c.Fingerprint, At: c.At, UpdatedAt: c.UpdatedAt}
}

// SessionClaims returns a session's explicit claims, oldest first.
func (e *Engine) SessionClaims(ctx context.Context, sessionID string) ([]ClaimView, error) {
	state, err := e.loadTaskState(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	out := make([]ClaimView, 0, len(state.Claims))
	for _, c := range state.Claims {
		out = append(out, claimView(c))
	}
	return out, nil
}

// AddSessionClaim records a host-supplied claim. A host may record a contract or fact, and evidence
// it already settled; it cannot confirm a claim without evidence. The status, evidence locators and
// fingerprint it supplies are validated and recorded as given, so a confirmation is never silently
// downgraded and never recorded without the proof it claims.
func (e *Engine) AddSessionClaim(ctx context.Context, sessionID string, claim ClaimView) (ClaimView, error) {
	lock, err := e.acquireRun(ctx, sessionID)
	if err != nil {
		return ClaimView{}, err
	}
	defer lock.Release()
	state, err := e.loadTaskState(ctx, sessionID)
	if err != nil {
		return ClaimView{}, err
	}
	added, err := state.AddHostClaim(verify.Claim{
		Kind: verify.ClaimKind(claim.Kind), Statement: claim.Statement, Required: claim.Required,
		Source:   verify.ClaimSource{Kind: verify.SourceKindHost, Ref: claim.SourceRef},
		Status:   verify.ClaimStatus(claim.Status),
		Evidence: append([]string(nil), claim.Evidence...), Fingerprint: claim.Fingerprint,
	}, time.Now().UTC())
	if err != nil {
		return ClaimView{}, err
	}
	if err := e.putClaimState(ctx, sessionID, state); err != nil {
		return ClaimView{}, err
	}
	return claimView(added), nil
}

// SettleSessionClaim moves a claim to confirmed, rejected, invalidated or revalidated. Confirmation
// and revalidation need evidence locators; a model source is refused. The fingerprint is the
// workspace state the evidence was read at.
func (e *Engine) SettleSessionClaim(ctx context.Context, sessionID, claimID, status string, evidence []string, actor string) (ClaimView, error) {
	lock, err := e.acquireRun(ctx, sessionID)
	if err != nil {
		return ClaimView{}, err
	}
	defer lock.Release()
	state, err := e.loadTaskState(ctx, sessionID)
	if err != nil {
		return ClaimView{}, err
	}
	fingerprint, err := e.workspaceFingerprint(ctx, sessionID)
	if err != nil {
		return ClaimView{}, err
	}
	source := verify.ClaimSource{Kind: verify.SourceKindUser, Ref: actor}
	settled, err := state.SettleClaim(claimID, verify.ClaimStatus(status), source, fingerprint, evidence, "", time.Now().UTC())
	if err != nil {
		return ClaimView{}, err
	}
	if err := e.putClaimState(ctx, sessionID, state); err != nil {
		return ClaimView{}, err
	}
	return claimView(settled), nil
}

// VerificationView is a session's last completion-gate outcome.
type VerificationView struct {
	Status      string            `json:"status"`
	Levels      map[string]string `json:"levels,omitempty"`
	Attempts    []AttemptView     `json:"attempts,omitempty"`
	Feedback    string            `json:"feedback,omitempty"`
	Review      *ReviewView       `json:"review,omitempty"`
	Claims      CoverageView      `json:"claims"`
	Fingerprint string            `json:"fingerprint,omitempty"`
	UpdatedAt   time.Time         `json:"updatedAt,omitempty"`
}

// AttemptView is one bounded repair attempt.
type AttemptView struct {
	Index    int       `json:"index"`
	Status   string    `json:"status"`
	Feedback string    `json:"feedback"`
	Checks   []string  `json:"checks,omitempty"`
	At       time.Time `json:"at"`
}

// ReviewView is the independent review's durable result.
type ReviewView struct {
	Passed    bool                   `json:"passed"`
	Findings  []verify.ReviewFinding `json:"findings,omitempty"`
	Source    string                 `json:"source,omitempty"`
	Model     string                 `json:"model,omitempty"`
	RequestID string                 `json:"requestId,omitempty"`
	At        time.Time              `json:"at"`
}

// CoverageView is the claim coverage at the current fingerprint.
type CoverageView struct {
	Required  int      `json:"required"`
	Confirmed int      `json:"confirmed"`
	Stale     int      `json:"stale"`
	Unsettled int      `json:"unsettled"`
	Rejected  int      `json:"rejected"`
	Missing   []string `json:"missing,omitempty"`
}

// SessionVerification returns the session's last gate outcome and its current claim coverage.
func (e *Engine) SessionVerification(ctx context.Context, sessionID string) (VerificationView, error) {
	state, err := e.loadTaskState(ctx, sessionID)
	if err != nil {
		return VerificationView{}, err
	}
	fingerprint, err := e.workspaceFingerprint(ctx, sessionID)
	if err != nil {
		return VerificationView{}, err
	}
	coverage := state.ClaimCoverage(fingerprint)
	view := VerificationView{Claims: CoverageView{Required: coverage.Required, Confirmed: coverage.Confirmed,
		Stale: coverage.Stale, Unsettled: coverage.Unsettled, Rejected: coverage.Rejected, Missing: coverage.Missing}}
	record := state.Verification
	if record == nil {
		view.Status = string(verify.StatusUnknown)
		return view, nil
	}
	view.Status, view.Levels, view.Feedback = record.Status, record.Levels, record.Feedback
	view.Fingerprint, view.UpdatedAt = record.Fingerprint, record.UpdatedAt
	for _, attempt := range record.Attempts {
		view.Attempts = append(view.Attempts, AttemptView{Index: attempt.Index, Status: string(attempt.Status),
			Feedback: attempt.Feedback, Checks: attempt.Checks, At: attempt.At})
	}
	if record.Review != nil {
		view.Review = &ReviewView{Passed: record.Review.Passed, Findings: record.Review.Findings,
			Source: record.Review.Source, Model: record.Review.Model, RequestID: record.Review.RequestID, At: record.Review.At}
	}
	return view, nil
}

// putClaimState writes the claim version on its own, based on the version the state was read as.
func (e *Engine) putClaimState(ctx context.Context, sessionID string, state taskstate.State) error {
	raw, err := state.Encode()
	if err != nil {
		return err
	}
	_, err = e.store.PutTaskState(ctx, sessionID, session.TaskStateWrite{
		Base: state.Version, Reason: taskstate.ReasonUser, JSON: raw,
	})
	return err
}

// workspaceFingerprint is the session workspace's evaluator fingerprint, "" outside git or when it
// cannot be read.
func (e *Engine) workspaceFingerprint(ctx context.Context, sessionID string) (string, error) {
	ev, err := e.sessionEvaluator(ctx, sessionID)
	if err != nil {
		return "", err
	}
	env, err := ev.Environment(ctx)
	if err != nil {
		return "", err
	}
	return env.String(), nil
}
