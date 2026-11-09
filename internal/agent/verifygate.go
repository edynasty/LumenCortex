package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/edynasty/LumenCortex/internal/evaluator"
	"github.com/edynasty/LumenCortex/internal/taskstate"
	"github.com/edynasty/LumenCortex/internal/verify"
	"github.com/edynasty/LumenCortex/protocol"
)

// ErrSemanticUnverified ends a run whose completion gate could not be satisfied within its repair
// budget. The task did not fail; its result is unverified, which is a different outcome.
var ErrSemanticUnverified = errors.New("completion gate did not verify the result within its repair budget")

// KindSemanticUnverified is the exit kind of a run that stopped at an unverified completion gate.
const KindSemanticUnverified = "semantic_unverified"

// VerificationOptions configures the final gate (spec §24-25). Zero values keep the existing gate:
// required checks decide execution, and no semantic level is required. A host that sets any of these
// (or supplies a verifier or a reviewer) opts the run into the verification levels, and only such a
// run is offered claim_write.
type VerificationOptions struct {
	// RequireSemantic makes a semantic-level decision a condition of completion. A required host or
	// user claim in Task State also turns it on; a model's own claim never does.
	RequireSemantic bool
	// RequireIndependent makes at least one independent source a condition of the semantic level. It
	// implies RequireSemantic: only an independent source can satisfy it.
	RequireIndependent bool
	// MaxRepairs bounds the feedback → repair → recheck loop. Zero takes DefaultMaxRepairs.
	MaxRepairs int
	// Review enables the single independent review, when the loop has a Reviewer.
	Review bool
}

// verificationGate runs the completion gate's verification levels: PreVerify hooks, the configured
// required checks, the semantic verifier, the claims' coverage, the single independent review, and
// Stop/Completion hooks. It returns the correction text to give the model, or an exit when the
// repair budget is used up. An empty correction with a nil exit means completion may proceed.
func (l *Loop) verificationGate(ctx context.Context, sessionID string, r *run, opts Options, step int64, final string, usage *protocol.Usage, executionVerdicts []evaluator.Verdict) (string, *ExitCause) {
	fingerprint := l.currentFingerprint(ctx)
	// The repair budget is the run's own: a new turn starts with a fresh one, however many attempts
	// the previous turn recorded (the record keeps them as history).
	attempt := len(r.attempts) + 1
	pre := l.runHook(ctx, sessionID, opts.RunID, verifyEventBeforeVerification, HookRequest{
		Step: step, Detail: map[string]any{"attempt": attempt},
	})
	if pre.Denied() {
		return hookFeedback(pre), nil
	}

	verdicts := executionVerdicts
	if verdicts == nil {
		// An interactive turn that changed nothing decided no checks above; decide them once here.
		var cause *ExitCause
		if verdicts, cause = l.decideRequired(ctx); cause != nil {
			return "", cause
		}
	}
	execution, semantic := CheckVerdicts(verdicts)
	requireSemantic := opts.Verification.RequireSemantic || len(r.ts.RequiredClaims()) > 0
	if l.Verifier != nil {
		verified, verifyErr := l.Verifier.Verify(ctx, verify.VerifyRequest{
			SessionID: sessionID, RunID: opts.RunID, Goal: r.ts.Objective(""), Final: final,
			Attempt: attempt, Claims: r.ts.Claims, Execution: execution,
		})
		if verifyErr != nil {
			if ctx.Err() != nil {
				return "", &ExitCause{Status: StatusInterrupted, Kind: KindCancelled, Err: verifyErr}
			}
			// A verifier that could not run decides nothing: the semantic level stays UNKNOWN rather
			// than becoming a pass.
			l.emit("verification.verifier_failed", sessionID, map[string]any{"step": step, "error": verifyErr.Error()})
		} else {
			semantic = append(semantic, verified...)
		}
	}

	// A confirmation tied to a workspace state that moved is stale: invalidate it first so the gate
	// sees the real coverage and the model is asked to revalidate.
	if changed := r.ts.InvalidateStaleClaims(fingerprint, time.Now().UTC()); len(changed) > 0 {
		l.emit("verification.claims_invalidated", sessionID, map[string]any{"step": step, "claims": changed})
	}

	if r.review != nil {
		// A moved workspace needs fresh host semantic evidence, not another review
		// and not the old verdict. Keep that review in the durable history.
		if fingerprint != "" && r.reviewFingerprint != "" && fingerprint != r.reviewFingerprint {
			requireSemantic = true
		} else {
			semantic = append(semantic, r.review.Verdict())
		}
	}
	gateIn := verify.GateInput{
		Execution:          execution,
		Semantic:           semantic,
		Claims:             r.ts.Claims,
		CurrentFingerprint: fingerprint,
		RequireSemantic:    requireSemantic,
		RequireIndependent: opts.Verification.RequireIndependent,
		MaxRepairs:         opts.Verification.MaxRepairs,
		Attempts:           append([]verify.Attempt(nil), r.attempts...),
	}
	result := verify.Evaluate(gateIn, time.Now().UTC())

	// The single independent review runs at most once per run: when the rest of the gate passed, and
	// when an independent semantic pass is the only thing between the gate and completion (the case
	// RequireIndependent creates, since only an independent source can satisfy it). A review that
	// found a blocking issue fails the semantic level; the repair is verified by the host checks,
	// never by a second review.
	if opts.Verification.Review && l.Reviewer != nil && !r.reviewed && (result.Passed || reviewCanDecide(gateIn, time.Now().UTC())) {
		r.reviewed = true
		request := verify.ReviewRequest{
			SessionID: sessionID, RunID: opts.RunID, Goal: r.ts.Objective(""), Final: final,
			Claims: r.ts.Claims, Verdicts: append(append([]verify.Verdict(nil), execution...), semantic...),
			Evidence: evidenceRefs(execution, semantic),
		}
		// The review is a model request like any other: it is admitted against the run's budget
		// before it is sent, so a request limit or token budget cannot be overrun by it.
		reviewModel, reviewInput := l.reviewAdmission(ctx, opts, request)
		if cause := r.spend.admitRequest(opts.Budget, reviewModel, reviewInput); cause != nil {
			return "", cause
		}
		reviewed, err := l.Reviewer.Review(ctx, request)
		if err != nil {
			// A review that could not run is UNKNOWN, never a pass: the gate keeps the semantic level
			// undecided and says so.
			l.emit("verification.review_failed", sessionID, map[string]any{"step": step, "error": err.Error()})
			reviewed.Passed = false
			reviewed.Source = verify.SourceIndependent
			reviewed.Findings = []verify.ReviewFinding{{Severity: verify.SeverityMajor, Issue: "the independent review could not complete: " + err.Error()}}
		}
		normalized, normErr := verify.NormalizeReview(reviewed, time.Now().UTC())
		if normErr != nil {
			l.emit("verification.review_invalid", sessionID, map[string]any{"step": step, "error": normErr.Error()})
			reviewed.Passed = false
			reviewed.Source = verify.SourceIndependent
			reviewed.Findings = []verify.ReviewFinding{{Severity: verify.SeverityMajor, Issue: "the independent review was invalid: " + normErr.Error()}}
			normalized, _ = verify.NormalizeReview(reviewed, time.Now().UTC())
		}
		r.review = &normalized
		r.reviewFingerprint = fingerprint
		r.ts.SetReview(normalized, time.Now().UTC())
		// Only an admitted model request has usage to meter. Configuration/factory
		// failures still block acceptance but must not invent request consumption.
		var total int64
		if normalized.RequestID != "" {
			if usage != nil {
				addUsage(usage, normalized.Usage)
			}
			r.spend.ModelRequests++
			total = normalized.Usage.TotalTokens
			if total == 0 {
				total = normalized.Usage.PromptTokens + normalized.Usage.CompletionTokens
			}
			r.spend.KnownTokens += total
			if normalized.CostKnown {
				r.spend.KnownCostUSD += normalized.CostUSD
			} else {
				r.spend.UnknownCostRequests++
			}
		}
		gateIn.Semantic = append(gateIn.Semantic, normalized.Verdict())
		result = verify.Evaluate(gateIn, time.Now().UTC())
		l.emit("verification.review", sessionID, map[string]any{"step": step, "passed": normalized.Passed,
			"findings": len(normalized.Findings), "model": normalized.Model,
			"tokens": total, "costUsd": normalized.CostUSD, "costKnown": normalized.CostKnown})
	}

	r.attempts = append([]verify.Attempt(nil), result.Attempts...)
	r.ts.SetVerification(result, fingerprint, time.Now().UTC())
	if err := l.flush(ctx, sessionID, r, taskstate.ReasonStep); err != nil {
		return "", &ExitCause{Status: StatusFailed, Kind: KindStorage, Err: err}
	}
	l.emit("verification.gate", sessionID, map[string]any{"step": step, "status": string(result.Status),
		"passed": result.Passed, "attempt": attempt, "exhausted": result.Exhausted,
		"claimsRequired": result.Claims.Required, "claimsConfirmed": result.Claims.Confirmed})

	post := l.runHook(ctx, sessionID, opts.RunID, verifyEventAfterVerification, HookRequest{
		Step: step, Detail: map[string]any{"status": string(result.Status), "passed": result.Passed, "attempt": attempt},
	})
	// An exhausted repair budget ends the run whatever a hook says: a PostVerify denial can correct a
	// completion, not extend the budget the host set.
	if post.Denied() && !(result.Exhausted && !result.Passed) {
		return hookFeedback(post), nil
	}
	if post.Denied() {
		l.emit("verification.post_verify_overridden", sessionID, map[string]any{"step": step, "reason": "repair_exhausted"})
	}

	if !result.Passed {
		if result.Exhausted {
			return "", &ExitCause{Status: StatusInterrupted, Kind: KindSemanticUnverified,
				Err: fmt.Errorf("%w: %s", ErrSemanticUnverified, result.Feedback)}
		}
		return result.Feedback, nil
	}

	// Stop runs only on a completion the verification levels accepted: a Stop hook can ask for more
	// work, but it can never override an independent failure (the gate above already returned).
	stop := l.runHook(ctx, sessionID, opts.RunID, verifyEventStop, HookRequest{Step: step,
		Detail: map[string]any{"status": string(result.Status)}})
	if stop.Denied() {
		return hookFeedback(stop), nil
	}
	completion := l.runHook(ctx, sessionID, opts.RunID, verifyEventCompletion, HookRequest{Step: step,
		Detail: map[string]any{"status": string(result.Status)}})
	if completion.Denied() {
		return hookFeedback(completion), nil
	}
	return "", nil
}

// verificationOffered reports that the host opted this run into the verification levels: it asked
// for one of them, or it supplied a verifier or a reviewer. Only such a run is offered claim_write:
// a model's own claim cannot decide or block a gate the host did not ask for.
func (l *Loop) verificationOffered(opts Options) bool {
	v := opts.Verification
	return v.RequireSemantic || v.RequireIndependent || v.Review || l.Verifier != nil || l.Reviewer != nil
}

// verificationRequested reports whether this completion uses the verification levels. A run with no
// host request and no required host claim keeps the pre-existing gate: no check is decided here, no
// verifier runs, and no verification record is written. A model's own claim never turns it on: only
// a host or user claim can be required (verify.Claim.CountsAsRequired).
func (l *Loop) verificationRequested(opts Options, r *run) bool {
	return l.verificationOffered(opts) || len(r.ts.RequiredClaims()) > 0
}

// completionHooks runs Stop and Completion for a completion the verification levels did not gate. It
// does nothing when the host configured no hooks, which is the legacy path.
func (l *Loop) completionHooks(ctx context.Context, sessionID string, opts Options, step int64) string {
	if l.hookRegistry() == nil {
		return ""
	}
	stop := l.runHook(ctx, sessionID, opts.RunID, verifyEventStop, HookRequest{Step: step,
		Detail: map[string]any{"status": string(verify.StatusPassed)}})
	if stop.Denied() {
		return hookFeedback(stop)
	}
	completion := l.runHook(ctx, sessionID, opts.RunID, verifyEventCompletion, HookRequest{Step: step,
		Detail: map[string]any{"status": string(verify.StatusPassed)}})
	if completion.Denied() {
		return hookFeedback(completion)
	}
	return ""
}

// requiredChecksAndVerdicts decides the required checks once and returns both the failing verdicts and
// every verdict, so the caller's correction path and the verification gate share one decision.
func (l *Loop) requiredChecksAndVerdicts(ctx context.Context) ([]evaluator.Verdict, []evaluator.Verdict, *ExitCause) {
	verdicts, cause := l.decideRequired(ctx)
	if cause != nil {
		return nil, nil, cause
	}
	return failing(verdicts), verdicts, nil
}

// decideRequired decides every configured required check and returns all verdicts, passing and
// failing. It is the gate's execution-level source; requiredChecks filters it for the existing
// correction path.
func (l *Loop) decideRequired(ctx context.Context) ([]evaluator.Verdict, *ExitCause) {
	if l.Checks == nil {
		return nil, nil
	}
	approved, err := l.Checks.SetApproved(ctx)
	if err != nil {
		cause := checkFailureExit(ctx, err)
		return nil, &cause
	}
	if !approved {
		return []evaluator.Verdict{{
			Result: evaluator.Result{Check: checkSetID, Status: evaluator.StatusSpecUnapproved, Provenance: evaluator.ProvenanceRuntime, Artifacts: []string{}},
			Tail:   "the check set changed since the host approved it (a check removed, made optional, or the set emptied)",
		}}, nil
	}
	_, required := l.Checks.Configured()
	if len(required) == 0 {
		return nil, nil
	}
	verdicts, err := l.Checks.Decide(ctx, required, "")
	if err != nil {
		cause := checkFailureExit(ctx, err)
		return nil, &cause
	}
	return verdicts, nil
}

// checkVerdicts maps evaluator verdicts to verification levels. A check of kind semantic decides the
// semantic level; every other kind decides execution. Both are host-approved definitions, never the
// model's own claim.
func CheckVerdicts(verdicts []evaluator.Verdict) (execution, semantic []verify.Verdict) {
	for _, v := range verdicts {
		level := verify.LevelExecution
		if v.Result.Kind == evaluator.KindSemantic {
			level = verify.LevelSemantic
		}
		mapped := verify.Verdict{
			Level: level, Source: verify.SourceApprovedSpec, Check: v.Result.Check,
			Status: verifyStatus(v), Detail: v.Tail, At: v.Result.At,
		}
		if v.Result.Seq > 0 {
			mapped.Evidence = []string{fmt.Sprintf("check:%s@eval:%d", v.Result.Check, v.Result.Seq)}
		}
		if level == verify.LevelSemantic {
			semantic = append(semantic, mapped)
		} else {
			execution = append(execution, mapped)
		}
	}
	return execution, semantic
}

// verifyStatus maps an evaluator verdict to the verification status, keeping an episode that did
// not close from reporting a pass.
func verifyStatus(v evaluator.Verdict) verify.Status {
	switch verdictStatus(v) {
	case evaluator.StatusPassed:
		return verify.StatusPassed
	case evaluator.StatusFailed:
		return verify.StatusFailed
	case evaluator.StatusSpecUnapproved:
		return verify.StatusSpecUnapproved
	default:
		return verify.StatusError
	}
}

// reviewCanDecide reports whether an independent semantic pass is the only thing between the gate
// and completion. The review then runs even though the gate has not passed: with RequireIndependent
// the review is the only source that can satisfy independence, so waiting for a pass it alone can
// produce would deadlock the run.
func reviewCanDecide(in verify.GateInput, now time.Time) bool {
	probe := in
	probe.Semantic = append(append([]verify.Verdict(nil), in.Semantic...), verify.Verdict{
		Level: verify.LevelSemantic, Source: verify.SourceIndependent, Check: "independent_review",
		Status: verify.StatusPassed, At: now,
	})
	return verify.Evaluate(probe, now).Passed
}

// ReviewAdmission is what a reviewer reports about the request it is about to send: the catalog
// model that will answer it and the input tokens it costs. The loop admits the review against the
// run's budget with it, so a review cannot overrun a limit the host set.
type ReviewAdmission struct {
	Model       Model
	InputTokens int
}

// ReviewBudgeter is a Reviewer that can price its own request. A reviewer that does not implement it
// is admitted against the run's session model and an estimate of the request it was handed.
type ReviewBudgeter interface {
	ReviewBudget(ctx context.Context, req verify.ReviewRequest) ReviewAdmission
}

// reviewAdmission is the model and input estimate a review is admitted with.
func (l *Loop) reviewAdmission(ctx context.Context, opts Options, req verify.ReviewRequest) (Model, int) {
	if budgeted, ok := l.Reviewer.(ReviewBudgeter); ok {
		adm := budgeted.ReviewBudget(ctx, req)
		if adm.InputTokens > 0 || adm.Model.Ref != "" {
			return adm.Model, adm.InputTokens
		}
	}
	model, ok := l.Models[opts.Session]
	if !ok {
		model = Model{Ref: opts.Session}
	}
	// A review request is the goal, the final answer, the claims and the verdicts; four characters
	// per token is the estimator the window uses.
	estimate := len(req.Goal) + len(req.Final) + 512
	for _, c := range req.Claims {
		estimate += len(c.Statement) + 64
	}
	for _, v := range req.Verdicts {
		estimate += len(v.Check) + len(v.Detail) + 64
	}
	return model, max(1, estimate/4)
}

// evidenceRefs are the locators a reviewer may cite.
func evidenceRefs(groups ...[]verify.Verdict) []string {
	var out []string
	for _, group := range groups {
		for _, v := range group {
			out = append(out, v.Evidence...)
		}
	}
	return out
}

// currentFingerprint is the workspace state the gate decides at, from the run's evidence callback.
// Without one there is no fingerprint and no confirmation can be reused.
func (l *Loop) currentFingerprint(ctx context.Context) string {
	if l.Evidence == nil {
		return ""
	}
	ev, err := l.Evidence(ctx)
	if err != nil {
		return ""
	}
	return ev.Fingerprint
}
