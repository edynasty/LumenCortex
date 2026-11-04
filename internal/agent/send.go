package agent

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/edynasty/LumenCortex/config"
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
}

// attempt is the request that answered a step and the window it was built from.
type attempt struct {
	Response protocol.ProviderResponse
	Model    Model
	Elapsed  time.Duration
	Window   window.Result
	// RequestID is the admitted envelope of the request that was answered, and Lineage whether the
	// store kept that envelope lineage-complete (a cut ref list lowers it).
	RequestID string
	Lineage   bool
}

// candidates computes the step's ordered candidate list from the run's models and the health
// registry, and the decision that records it.
func (l *Loop) candidates(opts Options, base int) ([]Candidate, Decision) {
	refs := append([]config.ModelRef{opts.Session}, opts.Failover...)
	models := map[config.ModelRef]Candidate{}
	for _, ref := range refs {
		if m, ok := l.Models[ref]; ok {
			models[ref] = Candidate{Ref: ref, Context: m.Context, Output: m.Output, ToolCalling: m.ToolCalling}
		}
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
	return CandidatesDecision(opts.Session, opts.Failover, models, base, opts.MaxTokens, l.Health.Unavailable(refs), cooldownMs)
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

// send runs one step's request against the candidates until one answers (design §3.3 step 6). The
// window is built for each candidate with that candidate's budget from in; groups counts the groups
// from windowStartSeq on that the run still keeps. A window that does not fit a candidate moves to the
// next candidate with a larger context, and a capacity rejection is retried once on the same model
// with half the transcript (§5). After the initial sweep, availability recovery shares a finite retry
// allowance across candidates (recovery.go). Every move is appended to decisions. It returns the exit
// to take when no candidate answered.
func (l *Loop) send(
	ctx context.Context,
	sessionID string,
	step int64,
	opts Options,
	taskStateVersion int64,
	cands []Candidate,
	r *run,
	in window.Input,
	groups int,
	specs []protocol.ToolSpec,
	usage *protocol.Usage,
	decisions *[]Decision,
) (attempt, *ExitCause) {
	fixed := window.Fixed(in)
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
		// Turn rule (§4.6): a turn never spans runs or models. The turn row is a user row, its own
		// group, and commits before the request so a crash leaves it in place.
		if due, decision := TurnBoundary(r.turnRows(), cand.Ref); due {
			row := protocol.Message{Role: "user", Origin: protocol.OriginTurn, Content: protocol.TurnContent}
			seq, _, err := l.Store.AppendMessage(ctx, sessionID, row, nil)
			if err != nil {
				return attempt{}, &ExitCause{Status: StatusFailed, Kind: KindStorage, Err: err}
			}
			r.appendRow(seq, row)
			in.Transcript = r.transcript
			groups++
			*decisions = append(*decisions, decision)
			l.emit("session.message", sessionID, map[string]any{"step": step, "seq": seq, "role": "user", "origin": protocol.OriginTurn})
		}
		b := r.budget(cand, opts.MaxTokens, fixed)
		if m.DefaultContext {
			*decisions = append(*decisions, DefaultContextDecision(cand.Ref, cand.Context))
		}
		built, err := window.Build(in, b)
		if errors.Is(err, window.ErrUnpaired) {
			return attempt{}, &ExitCause{Status: StatusFailed, Kind: KindTranscriptUnpaired, Err: err}
		}
		if err != nil {
			// ErrOverflow: the newest group does not fit this model's budget; a model with a larger
			// context may take it (§5).
			l.emit("window.overflow", sessionID, map[string]any{"step": step, "modelRef": cand.Ref, "error": err.Error()})
			if !recovering {
				if next, decision := FailoverDecision(cands, i, protocol.ClassCapacity); next >= 0 {
					*decisions = append(*decisions, decision)
					i = next
					continue
				}
			}
			exhausted[i] = true
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
		if built.Dropped+r.trimmed > 0 {
			*decisions = append(*decisions, WindowDroppedDecision(cand.Ref, b, fixed, groups+r.trimmed, built.Costs))
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
			Messages: built.Messages, Tools: specs, ToolChoice: "auto", Temperature: opts.Temperature,
			MaxTokens: maxTokens, ReasoningEffort: string(sent),
			Echo: r.requestEcho(built, cand.Ref),
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
		// Every attempt (failover and capacity retries included) is a new request the budget admits
		// before its envelope exists (ADR-0019 §6).
		if cause := r.spend.admitRequest(opts.Budget, m, built.Tokens); cause != nil {
			return attempt{}, cause
		}
		requestID, lineage, cause := l.admit(ctx, sessionID, step, attempts, opts, r, built, specs)
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
			"messages": len(built.Messages), "tools": toolNames(specs), "reasoningEffort": req.ReasoningEffort,
			"candidateIndex": i, "windowFirstSeq": built.FirstSeq, "windowTokens": built.Tokens,
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
			return attempt{Response: response, Model: m, Elapsed: elapsed, Window: built, RequestID: requestID, Lineage: lineage}, nil
		}
		// A request that failed after it was sent may still be billed: its input is estimated.
		r.spend.record(m, protocol.Usage{}, built.Tokens, 0)
		l.budgetWarning(sessionID, step, opts.Budget, r.spend)
		class := Classify(ctx, err)
		// The request was sent; an error does not prove it was not delivered, so it is unknown.
		l.settle(sessionID, requestID, settleReason(class))
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
			retry, decision := CapacityRetryDecision(cand.Ref, b, fixed, built.Tokens)
			r.capTranscript(cand.Ref, retry, fixed)
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
