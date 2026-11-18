package agent

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/taskstate"
	"github.com/edynasty/LumenCortex/internal/verify"
	"github.com/edynasty/LumenCortex/internal/window"
	"github.com/edynasty/LumenCortex/protocol"
)

// Model is one catalog model the run may use: its effective limits and the client built for it
// before the run started. Efforts is the model's reasoning.efforts list.
type Model struct {
	Ref config.ModelRef
	// ProviderID and ModelID are the provider id and upstream model id, recorded in the step
	// record for display.
	ProviderID string
	ModelID    string
	Context    int
	// DefaultContext reports that Context is config.DefaultContext because the model configures no
	// limit.context; every request budgeted on it logs rule window.default_context (design §5).
	DefaultContext bool
	Output         int
	OutputSet      bool
	ToolCalling    bool
	Efforts        []config.Effort
	Client         protocol.Provider
	// Price is the model's configured cost, nil when it has none: its spend is unknown (ADR-0019 §6).
	Price *config.Cost
	// ProviderPkg is the normalized wire protocol ("openai-compatible" or "anthropic-messages").
	ProviderPkg string
	// Presentation holds this candidate model's explicit ToolDescriptions and Feedback preferences.
	Presentation PresentationOptions
}

// attempt is the request that answered a step and the window it was built from.
type attempt struct {
	Response protocol.ProviderResponse
	Model    Model
	Mode     ProjectionMode
	Elapsed  time.Duration
	Window   window.Result
	// RequestID is the admitted envelope of the request that was answered, and Lineage whether the
	// store kept that envelope lineage-complete (a cut ref list lowers it).
	RequestID string
	Lineage   bool
}

// canonicalStepInput holds the immutable step-level canonical prefix and filtered tools before
// candidate-specific projection (ADR-0024 §4.2).
type canonicalStepInput struct {
	SystemPrompt  string
	Layers        []SystemLayer
	Instructions  string
	State         protocol.Message
	StateRefs     []string
	IncludedTools []StepTool
}

// candidateProjection is the deterministic output of the 7-step acyclic projectCandidate pipeline
// for one candidate attempt (ADR-0024 §4.2, §4.3).
type candidateProjection struct {
	Mode               ProjectionMode
	Reason             string
	SentLayers         []SystemLayer
	System             []protocol.Message
	Tools              []protocol.ToolSpec
	ToolTokens         int
	FullFixed          int
	CompactFixed       int
	Fixed              int
	GuardTokens        int
	BaseGateSlack      int
	EffectiveGateSlack int
	UsableFull         int
	UsableCompact      int
	UsableInput        int
	MinRequiredSeq     int64
	Budget             window.Budget
	Window             window.Result
	Echo               []protocol.Echo
}

func projectLayers(layers []SystemLayer, mode ProjectionMode) []SystemLayer {
	if len(layers) == 0 {
		return nil
	}
	out := make([]SystemLayer, len(layers))
	for i, layer := range layers {
		out[i] = layer
		if mode == ModeCompact && strings.TrimSpace(layer.CompactText) != "" {
			out[i].Text = layer.CompactText
		}
	}
	return out
}

func buildSystemMessages(systemPrompt string, layers []SystemLayer, instructions string) []protocol.Message {
	system := []protocol.Message{{Role: "system", Content: systemPrompt}}
	for _, layer := range layers {
		if strings.TrimSpace(layer.Text) == "" {
			continue
		}
		system = append(system, protocol.Message{Role: "system", Content: layer.Text})
	}
	if strings.TrimSpace(instructions) != "" {
		system = append(system, protocol.Message{Role: "system", Content: instructions})
	}
	return system
}

// projectCandidate executes the 7-step stateless acyclic projection for one candidate model without
// mutating canonical inputs (ADR-0024 §4.3).
func projectCandidate(
	r *run,
	cand Candidate,
	m Model,
	opts Options,
	canonical canonicalStepInput,
	transcript []window.Entry,
	repair repairContext,
	minRequiredSeq int64,
) (candidateProjection, Decision, error) {
	// Step 1: Full and Compact static prefixes (fresh allocations, zero mutation of canonical).
	toolPref := effectiveVariant(m.Presentation.ToolDescriptions, opts.Presentation.ToolDescriptions)
	fullLayers := projectLayers(canonical.Layers, ModeFull)
	fullSystem := buildSystemMessages(canonical.SystemPrompt, fullLayers, canonical.Instructions)
	fullTools := presentStepTools(canonical.IncludedTools, toolPref, ModeFull)

	compactLayers := projectLayers(canonical.Layers, ModeCompact)
	compactSystem := buildSystemMessages(canonical.SystemPrompt, compactLayers, canonical.Instructions)
	compactTools := presentStepTools(canonical.IncludedTools, toolPref, ModeCompact)

	// Step 2: Fixed prefixes, candidate base pool, and required transcript suffix tokens.
	fullToolTokens := window.Estimate(fullTools)
	compactToolTokens := window.Estimate(compactTools)
	fullFixed := window.Fixed(window.Input{System: fullSystem, State: canonical.State, ToolTokens: fullToolTokens})
	compactFixed := window.Fixed(window.Input{System: compactSystem, State: canonical.State, ToolTokens: compactToolTokens})

	outBudget := OutputBudget(opts.MaxTokens, cand)
	guard := GuardTokens(cand.Context, m.ProviderPkg, len(canonical.IncludedTools))
	poolBeforeSlack := cand.Context - outBudget - guard

	txGroups, err := window.Groups(transcript)
	if err != nil {
		return candidateProjection{}, Decision{}, err
	}
	requiredRepairTokens := 0
	if repair.Active && repair.MinRequiredSeq >= 0 {
		for _, g := range txGroups {
			if g[len(g)-1].Seq >= repair.MinRequiredSeq {
				requiredRepairTokens += window.GroupTokens(g)
			}
		}
	}
	requiredTranscript := 0
	if minRequiredSeq >= 0 {
		for _, g := range txGroups {
			if g[len(g)-1].Seq >= minRequiredSeq {
				requiredTranscript += window.GroupTokens(g)
			}
		}
	} else if len(txGroups) > 0 {
		requiredTranscript = window.GroupTokens(txGroups[len(txGroups)-1])
	}

	// Step 3: BaseGateSlack for both modes.
	baseSlackFull := BaseGateSlack(m, opts.Presentation, ModeFull)
	baseSlackCompact := BaseGateSlack(m, opts.Presentation, ModeCompact)

	// Step 4: EffectiveGateSlack for both modes.
	effSlackFull := EffectiveGateSlack(poolBeforeSlack, fullFixed, baseSlackFull, requiredRepairTokens, repair)
	effSlackCompact := EffectiveGateSlack(poolBeforeSlack, compactFixed, baseSlackCompact, requiredRepairTokens, repair)

	// Step 5: Final Margin and UsableInput for both modes (incorporating per-candidate CapacityRetry cap).
	marginFull := guard + effSlackFull
	marginCompact := guard + effSlackCompact
	if r != nil {
		if limit, ok := r.transcriptCap[cand.Ref]; ok {
			marginFull = max(marginFull, cand.Context-outBudget-fullFixed-limit)
			marginCompact = max(marginCompact, cand.Context-outBudget-compactFixed-limit)
		}
	}
	usableFull := cand.Context - outBudget - marginFull
	usableCompact := cand.Context - outBudget - marginCompact

	// Step 6: Stateless binary mode selection.
	mode, reason, prefixDec := WindowPrefixDecision(cand.Ref, toolPref, usableFull, usableCompact, fullFixed, compactFixed, requiredTranscript)

	// Step 7: Build window and echo strictly from the selected mode's projection, enforcing minRequiredSeq.
	sentLayers := fullLayers
	sentSystem := fullSystem
	sentTools := fullTools
	sentToolTokens := fullToolTokens
	candFixed := fullFixed
	baseSlack := baseSlackFull
	effSlack := effSlackFull
	usableInput := usableFull
	selectedMargin := marginFull
	if mode == ModeCompact {
		sentLayers = compactLayers
		sentSystem = compactSystem
		sentTools = compactTools
		sentToolTokens = compactToolTokens
		candFixed = compactFixed
		baseSlack = baseSlackCompact
		effSlack = effSlackCompact
		usableInput = usableCompact
		selectedMargin = marginCompact
	}

	candBudget := window.Budget{Context: cand.Context, Output: outBudget, Margin: selectedMargin}
	candIn := window.Input{
		System:     sentSystem,
		State:      canonical.State,
		Transcript: transcript,
		ToolTokens: sentToolTokens,
	}
	built, err := window.Build(candIn, candBudget)
	if err != nil {
		return candidateProjection{}, Decision{}, err
	}
	if minRequiredSeq >= 0 && (built.FirstSeq < 0 || built.FirstSeq > minRequiredSeq) {
		return candidateProjection{}, Decision{}, fmt.Errorf(
			"%w: required context floor seq %d evicted (window firstSeq=%d)",
			window.ErrOverflow, minRequiredSeq, built.FirstSeq,
		)
	}
	var echo []protocol.Echo
	if r != nil {
		echo = r.requestEchoFrom(transcript, built, cand.Ref)
	}
	return candidateProjection{
		Mode:               mode,
		Reason:             reason,
		SentLayers:         sentLayers,
		System:             sentSystem,
		Tools:              sentTools,
		ToolTokens:         sentToolTokens,
		FullFixed:          fullFixed,
		CompactFixed:       compactFixed,
		Fixed:              candFixed,
		GuardTokens:        guard,
		BaseGateSlack:      baseSlack,
		EffectiveGateSlack: effSlack,
		UsableFull:         usableFull,
		UsableCompact:      usableCompact,
		UsableInput:        usableInput,
		MinRequiredSeq:     minRequiredSeq,
		Budget:             candBudget,
		Window:             built,
		Echo:               echo,
	}, prefixDec, nil
}

// candidatesByStaticPrefix computes the step's ordered candidate list using each candidate's static
// minimal ModeCompact prefix and GuardTokens, leaving dynamic window checks to projectCandidate.
func (l *Loop) candidatesByStaticPrefix(opts Options, canonical canonicalStepInput) ([]Candidate, Decision) {
	refs := append([]config.ModelRef{opts.Session}, opts.Failover...)
	models := map[config.ModelRef]Candidate{}
	compactFixed := map[config.ModelRef]int{}
	guards := map[config.ModelRef]int{}
	compactLayers := projectLayers(canonical.Layers, ModeCompact)
	compactSystem := buildSystemMessages(canonical.SystemPrompt, compactLayers, canonical.Instructions)
	for _, ref := range refs {
		m, ok := l.Models[ref]
		if !ok {
			continue
		}
		models[ref] = Candidate{Ref: ref, Context: m.Context, Output: m.Output, ToolCalling: m.ToolCalling}
		toolPref := effectiveVariant(m.Presentation.ToolDescriptions, opts.Presentation.ToolDescriptions)
		compactTools := presentStepTools(canonical.IncludedTools, toolPref, ModeCompact)
		compactFixed[ref] = window.Fixed(window.Input{
			System:     compactSystem,
			State:      canonical.State,
			ToolTokens: window.Estimate(compactTools),
		})
		guards[ref] = GuardTokens(m.Context, m.ProviderPkg, len(canonical.IncludedTools))
	}
	var cooldownMs map[config.ModelRef]int64
	for _, ref := range refs {
		if delay := l.Health.RetryDelay(ref); delay > 0 {
			if cooldownMs == nil {
				cooldownMs = make(map[config.ModelRef]int64)
			}
			cooldownMs[ref] = ceilMs(delay)
		}
	}
	return CandidatesByStaticPrefixDecision(opts.Session, opts.Failover, models, compactFixed, guards, opts.MaxTokens, l.Health.Unavailable(refs), cooldownMs)
}

// exitFor maps the class of the error that ended a step to the run's exit.
func exitFor(class protocol.ErrorClass, err error) ExitCause {
	switch class {
	case protocol.ClassCancelled:
		return ExitCause{Status: StatusInterrupted, Kind: KindCancelled, Err: err}
	case protocol.ClassAuth:
		return ExitCause{Status: StatusFailed, Kind: KindProviderAuth, Err: err}
	case protocol.ClassBadRequest:
		return ExitCause{Status: StatusFailed, Kind: KindProviderBadRequest, Err: err}
	case protocol.ClassCapacity:
		// Reached once the retry with a halved transcript (§5) was rejected or overflowed too and no
		// candidate with a larger context is left; a context reset or a larger model clears it.
		return ExitCause{Status: StatusFailed, Kind: KindProviderCapacity, Err: err}
	default:
		return ExitCause{Status: StatusInterrupted, Kind: KindProviderAvailability, Err: err}
	}
}

// send runs one step's request against the candidates until one answers (ADR-0024 §4.3, §4.7, §4.8).
// Each candidate attempt runs the stateless 7-step projectCandidate pipeline from canonical; TurnBoundary
// is dry-run on an in-memory clone with trailing non-user EchoTokens zeroed before committing OriginTurn;
// BeforeModelCall steer injections rebuild the projection and Echo before constructing ProviderRequest.
func (l *Loop) send(
	ctx context.Context,
	sessionID string,
	goal string,
	step int64,
	opts Options,
	taskStateVersion int64,
	cands []Candidate,
	r *run,
	canonical canonicalStepInput,
	groups int,
	usage *protocol.Usage,
	decisions *[]Decision,
) (attempt, *ExitCause) {
	retried := map[config.ModelRef]bool{}
	attempts := 0
	// After the failover chain has ended once, the step is recovering: every failure goes to
	// recoverFrom. exhausted holds the candidates capacity rejected for good; they are not sent again.
	var down outage
	recovering := false
	exhausted := map[int]bool{}
	pendingRetry := false
	// recoverFrom logs the failure of cands[failed] (failure is the provider.failover event of a sent
	// request, nil when nothing was sent), then waits and returns the candidate to send to next, or the
	// exit when the step stops recovering.
	recoverFrom := func(failed int, class protocol.ErrorClass, err error, failure map[string]any) (int, *ExitCause) {
		deadline, _ := ctx.Deadline()
		now := time.Now()
		readyIn := down.readyIn(len(cands), exhausted, now)
		for i, cand := range cands {
			if readyIn[i] >= 0 {
				readyIn[i] = max(readyIn[i], l.Health.RetryDelay(cand.Ref))
			}
		}
		backoff := opts.Recovery.backoff(down.retries+1, rand.Float64())
		next, wait, decision := RecoveryDecision(cands, failed, class, readyIn, backoff)
		if next < 0 && class == protocol.ClassCapacity {
			if failure != nil {
				l.emit("provider.failover", sessionID, failure)
			}
			cause := exitFor(class, fmt.Errorf("model %s: %w", cands[failed].Ref, err))
			return -1, &cause
		}
		stop := opts.Recovery.stopFor(next, wait, down.start, now, deadline, down.retries)
		reason, status := failureDetail(err)
		if failure != nil {
			if stop == "" {
				failure["nextModelRef"] = cands[next].Ref
			}
			l.emit("provider.failover", sessionID, failure)
		}
		retry := map[string]any{
			"step": step, "modelRef": cands[failed].Ref, "class": class, "failure": reason, "status": status,
			"error": err.Error(), "failedRequests": down.failures, "retries": down.retries,
			"retryAfterMs": retryAfter(err).Milliseconds(), "delayMs": wait.Milliseconds(),
			"outageMs": now.Sub(down.start).Milliseconds(),
		}
		if stop != "" {
			retry["stop"] = stop
			l.emit("provider.retry_stopped", sessionID, retry)
			cause := exitFor(protocol.ClassAvailability, &RecoveryError{
				Failures: down.failures, Retries: down.retries, Outage: now.Sub(down.start), Stop: stop, Wait: wait, Last: down.last,
			})
			return -1, &cause
		}
		*decisions = append(*decisions, decision)
		retry["retry"] = down.retries + 1
		retry["nextModelRef"] = cands[next].Ref
		l.emit("provider.retry", sessionID, retry)
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			cause := exitFor(protocol.ClassCancelled, ctx.Err())
			return -1, &cause
		case <-timer.C:
		}
		pendingRetry = true
		return next, nil
	}
	for i := 0; ; {
		cand := cands[i]
		m := l.Models[cand.Ref]
		repair := deriveRepairContext(r.transcript, r.mutations)
		due, _ := TurnBoundary(r.turnRows(), cand.Ref)
		previewTranscript := r.transcript
		minReqSeq := repair.MinRequiredSeq
		if due {
			var turnProgressSeq int64
			previewTranscript, turnProgressSeq = r.cloneForTurnPreview()
			minReqSeq = minPositiveSeq(turnProgressSeq, repair.MinRequiredSeq)
		}
		proj, prefixDec, err := projectCandidate(r, cand, m, opts, canonical, previewTranscript, repair, minReqSeq)
		if errors.Is(err, window.ErrUnpaired) {
			return attempt{}, &ExitCause{Status: StatusFailed, Kind: KindTranscriptUnpaired, Err: err}
		}
		if err != nil {
			// ErrOverflow (including minRequiredSeq floor or TurnBoundary dry-run overflow):
			// move to a larger candidate without writing OriginTurn to SQLite (§4.8).
			l.emit("window.overflow", sessionID, map[string]any{"step": step, "modelRef": cand.Ref, "error": err.Error()})
			exhausted[i] = true
			if !recovering {
				if next, decision := FailoverDecision(cands, i, protocol.ClassCapacity); next >= 0 {
					*decisions = append(*decisions, decision)
					i = next
					continue
				}
			}
			if len(down.ready) == 0 {
				kind := KindWindowOverflow
				if _, capped := r.transcriptCap[cand.Ref]; capped {
					kind = KindProviderCapacity
				}
				return attempt{}, &ExitCause{Status: StatusFailed, Kind: kind, Err: fmt.Errorf("model %s: %w", cand.Ref, err)}
			}
			// Another candidate failed with availability and may answer once it recovers.
			recovering = true
			next, cause := recoverFrom(i, protocol.ClassCapacity, err, nil)
			if cause != nil {
				return attempt{}, cause
			}
			i = next
			continue
		}
		// Nothing is sent without a durable envelope. A context that ended before this point sends
		// nothing and records nothing; a fenced session or a storage failure ends the run here.
		if err := ctx.Err(); err != nil {
			cause := exitFor(protocol.ClassCancelled, err)
			return attempt{}, &cause
		}
		// Preserve the initial sweep whenever this candidate's first attempt can still fit. Only an
		// impossible cooldown may yield to recovery of a candidate already attempted this step.
		if delay := l.Health.RetryDelay(cand.Ref); delay > 0 && down.failures > 0 {
			now := time.Now()
			deadline, _ := ctx.Deadline()
			blocked := (!deadline.IsZero() && !now.Add(delay).Before(deadline)) ||
				(opts.Recovery.MaxOutage > 0 && now.Add(delay).Sub(down.start) > opts.Recovery.MaxOutage)
			if blocked {
				ready := down.readyIn(len(cands), exhausted, now)
				for other, remaining := range ready {
					if remaining >= 0 {
						ready[other] = max(remaining, l.Health.RetryDelay(cands[other].Ref))
					}
				}
				// The upper jitter bound is conservative: if it fits, recoverFrom's sampled wait fits.
				next, wait := recoveryPick(ready, i, opts.Recovery.backoff(down.retries+1, 1))
				if opts.Recovery.stopFor(next, wait, down.start, now, deadline, down.retries) == "" {
					recovering = true
					next, cause := recoverFrom(i, protocol.ClassAvailability, down.last, nil)
					if cause != nil {
						return attempt{}, cause
					}
					i = next
					continue
				}
			}
		}
		if cause := l.waitForCooldown(ctx, sessionID, step, cand.Ref, opts.Recovery, down.start); cause != nil {
			return attempt{}, cause
		}
		if err := l.debugBoundary(ctx, DebugBoundary{Event: "BeforeModelCall", Step: step}); err != nil {
			cause := debugFailureExit(err)
			return attempt{}, &cause
		}
		// F-STEER-1 & ADR-0024 §4.7: a run released from BeforeModelCall may have queued steers, inbox
		// edits or worker messages. Apply them before committing OriginTurn or building ProviderRequest.
		rebuiltInputs, err := l.applyPendingInputs(ctx, sessionID, goal, opts, r, nil)
		if err != nil {
			cause := ExitCause{Status: StatusFailed, Kind: KindStorage, Err: err}
			return attempt{}, &cause
		}
		if rebuiltInputs {
			taskStateVersion = r.ts.Version
			renderedState := taskstate.RenderBlock(goal, r.ts, r.wf)
			canonical.State = protocol.Message{Role: "user", Content: renderedState.Text}
			canonical.StateRefs = renderedState.ItemRefs
			repair = deriveRepairContext(r.transcript, r.mutations)
			if g, gerr := r.groups(l.maxContext(), repair.MinRequiredSeq); gerr == nil {
				groups = len(g)
			}
		}
		// Turn rule (§4.8): re-check TurnBoundary after BeforeModelCall / applyPendingInputs and commit
		// the durable OriginTurn row only when still required.
		var turnDec Decision
		due, turnDec = TurnBoundary(r.turnRows(), cand.Ref)
		minReqSeq = repair.MinRequiredSeq
		if due {
			if g, gerr := window.Groups(r.transcript); gerr == nil && len(g) > 0 {
				minReqSeq = minPositiveSeq(g[len(g)-1][0].Seq, repair.MinRequiredSeq)
			}
			row := protocol.Message{Role: "user", Origin: protocol.OriginTurn, Content: protocol.TurnContent}
			seq, _, err := l.Store.AppendMessage(ctx, sessionID, row, nil)
			if err != nil {
				return attempt{}, &ExitCause{Status: StatusFailed, Kind: KindStorage, Err: err}
			}
			r.appendRow(seq, row)
			groups++
			*decisions = append(*decisions, turnDec)
			l.emit("session.message", sessionID, map[string]any{"step": step, "seq": seq, "role": "user", "origin": protocol.OriginTurn})
		}
		if due || rebuiltInputs {
			var rebuildErr error
			proj, prefixDec, rebuildErr = projectCandidate(r, cand, m, opts, canonical, r.transcript, repair, minReqSeq)
			if errors.Is(rebuildErr, window.ErrUnpaired) {
				return attempt{}, &ExitCause{Status: StatusFailed, Kind: KindTranscriptUnpaired, Err: rebuildErr}
			}
			if rebuildErr != nil {
				l.emit("window.overflow", sessionID, map[string]any{"step": step, "modelRef": cand.Ref, "error": rebuildErr.Error()})
				exhausted[i] = true
				if !recovering {
					if next, decision := FailoverDecision(cands, i, protocol.ClassCapacity); next >= 0 {
						*decisions = append(*decisions, decision)
						i = next
						continue
					}
				}
				return attempt{}, &ExitCause{Status: StatusFailed, Kind: KindWindowOverflow,
					Err: fmt.Errorf("model %s: %w", cand.Ref, rebuildErr)}
			}
		}
		built := proj.Window
		if built.Dropped > 0 {
			pre := l.runHook(ctx, sessionID, opts.RunID, verify.EventBeforeCompact, HookRequest{Step: step, Detail: map[string]any{"droppedGroups": built.Dropped}})
			if pre.Denied() {
				return attempt{}, &ExitCause{Status: StatusInterrupted, Kind: "hook_denied", Err: errors.New(hookFeedback(pre))}
			}
			l.runHook(ctx, sessionID, opts.RunID, verify.EventAfterCompact, HookRequest{Step: step, Detail: map[string]any{"droppedGroups": built.Dropped}})
		}
		if m.DefaultContext {
			*decisions = append(*decisions, DefaultContextDecision(cand.Ref, cand.Context))
		}
		*decisions = append(*decisions, prefixDec)
		if built.Dropped+r.trimmed > 0 {
			*decisions = append(*decisions, WindowDroppedDecision(cand.Ref, proj.Budget, proj.Fixed, groups+r.trimmed, built.Costs))
		}
		def, ceiling := opts.EffortDefault, opts.EffortAutoCeiling
		if ce, ok := opts.CandidateEfforts[cand.Ref]; ok {
			def, ceiling = ce.Default, ce.AutoCeiling
		}
		sent, effortDecision := EffortDecision(def, opts.Effort, taskStateVersion, m.Efforts, ceiling)
		*decisions = append(*decisions, effortDecision)
		// max_tokens is what the caller asked for, else what the model's configuration says, else
		// nothing: the effective default only reserves capacity (D4). It is never scaled by effort.
		maxTokens := opts.MaxTokens
		if maxTokens <= 0 && m.OutputSet {
			maxTokens = m.Output
		}
		req := protocol.ProviderRequest{
			Messages: built.Messages, Tools: proj.Tools, ToolChoice: "auto", Temperature: opts.Temperature,
			MaxTokens: maxTokens, ReasoningEffort: string(sent),
			Echo: proj.Echo,
		}
		// Every attempt (failover and capacity retries included) is a new request the budget admits
		// before its envelope exists (ADR-0019 §6).
		if cause := r.spend.admitRequest(opts.Budget, m, built.Tokens); cause != nil {
			return attempt{}, cause
		}
		requestID, lineage, cause := l.admit(ctx, sessionID, step, attempts, opts, cand.Ref, proj.SentLayers, proj.Mode, taskStateVersion, canonical.StateRefs, built, proj.Tools)
		if cause != nil {
			return attempt{}, cause
		}
		attempts++
		if pendingRetry {
			down.retries++
			pendingRetry = false
		}
		l.emit("llm.request", sessionID, map[string]any{
			"step": step, "modelRef": cand.Ref, "provider": m.ProviderID, "model": m.ModelID,
			"messages": len(built.Messages), "tools": toolNames(proj.Tools), "reasoningEffort": req.ReasoningEffort,
			"candidateIndex": i, "windowFirstSeq": built.FirstSeq, "windowTokens": built.Tokens,
			"projectionMode": string(proj.Mode), "projectionReason": proj.Reason,
		})
		if err := ctx.Err(); err != nil {
			l.settle(sessionID, requestID, reasonCancelled)
			cause := exitFor(protocol.ClassCancelled, err)
			return attempt{}, &cause
		}
		usage.Requests++
		started := time.Now()
		response, err := l.complete(ctx, sessionID, requestID, m, req)
		elapsed := time.Since(started)
		if err == nil {
			r.spend.record(m, response.Usage, built.Tokens, window.Estimate(response.Message))
			l.budgetWarning(sessionID, step, opts.Budget, r.spend)
			l.Health.Success(cand.Ref)
			l.emit("llm.complete", sessionID, map[string]any{
				"step": step, "modelRef": cand.Ref, "provider": m.ProviderID, "model": m.ModelID,
				"elapsedMs": elapsed.Milliseconds(), "candidateIndex": i,
			})
			if down.failures > 0 {
				l.emit("provider.recovered", sessionID, map[string]any{
					"step": step, "modelRef": cand.Ref, "failedRequests": down.failures, "retries": down.retries,
					"outageMs": time.Since(down.start).Milliseconds(),
				})
			}
			return attempt{Response: response, Model: m, Mode: proj.Mode, Elapsed: elapsed, Window: built, RequestID: requestID, Lineage: lineage}, nil
		}
		// A request that failed after it was sent may still be billed: its input is estimated.
		r.spend.record(m, protocol.Usage{}, built.Tokens, 0)
		l.budgetWarning(sessionID, step, opts.Budget, r.spend)
		class := Classify(ctx, err)
		// The request was sent; an error does not prove it was not delivered, so it is unknown.
		l.settle(sessionID, requestID, settleReason(class))
		if pauseErr := l.debugBoundary(ctx, DebugBoundary{Event: "AfterModelCall", Step: step, Failed: true}); pauseErr != nil {
			cause := debugFailureExit(pauseErr)
			return attempt{}, &cause
		}
		if class == protocol.ClassCancelled {
			cause := exitFor(class, ctx.Err())
			return attempt{}, &cause
		}
		if class == protocol.ClassAvailability {
			l.Health.Fail(cand.Ref, retryAfter(err))
			down.fail(i, started, time.Now(), retryAfter(err), fmt.Errorf("model %s: %w", cand.Ref, err))
		}
		if class == protocol.ClassCapacity && !retried[cand.Ref] {
			// The window is deterministic, so the same request would be rejected again: retry once
			// on this model with half the transcript, and keep that limit for it until the run ends.
			retried[cand.Ref] = true
			retry, decision := CapacityRetryDecision(cand.Ref, proj.Budget, proj.Fixed, built.Tokens)
			r.capTranscript(cand.Ref, retry, proj.Fixed)
			*decisions = append(*decisions, decision)
			l.emit("window.capacity_retry", sessionID, map[string]any{
				"step": step, "modelRef": cand.Ref, "error": err.Error(), "rejectedTokens": built.Tokens,
			})
			continue
		}
		if class == protocol.ClassCapacity {
			exhausted[i] = true
		}
		reason, status := failureDetail(err)
		failure := map[string]any{
			"step": step, "modelRef": cand.Ref, "provider": m.ProviderID, "model": m.ModelID,
			"class": class, "failure": reason, "status": status, "error": err.Error(), "elapsedMs": elapsed.Milliseconds(),
		}
		if !recovering {
			if next, decision := FailoverDecision(cands, i, class); next >= 0 {
				failure["nextModelRef"] = cands[next].Ref
				l.emit("provider.failover", sessionID, failure)
				*decisions = append(*decisions, decision)
				i = next
				continue
			}
		}
		// The failover chain has ended. Only an outage is waited out: auth and bad requests stop the
		// run, and so does capacity when no candidate failed with availability.
		if (class != protocol.ClassAvailability && class != protocol.ClassCapacity) || len(down.ready) == 0 {
			l.emit("provider.failover", sessionID, failure)
			cause := exitFor(class, fmt.Errorf("model %s: %w", cand.Ref, err))
			return attempt{}, &cause
		}
		recovering = true
		next, cause := recoverFrom(i, class, err, failure)
		if cause != nil {
			return attempt{}, cause
		}
		i = next
	}
}
