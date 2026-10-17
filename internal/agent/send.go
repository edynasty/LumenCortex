package agent

import (
	"context"
	"errors"
	"fmt"
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
	ProviderID  string
	ModelID     string
	Context     int
	Output      int
	OutputSet   bool
	ToolCalling bool
	Efforts     []config.Effort
	Client      protocol.Provider
}

// attempt is the request that answered a step and the window it was built from.
type attempt struct {
	Response protocol.ProviderResponse
	Model    Model
	Elapsed  time.Duration
	Window   window.Result
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
	return CandidatesDecision(opts.Session, opts.Failover, models, base, opts.MaxTokens, l.Health.Unavailable(refs))
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
// with half the transcript (§5). Every move is appended to decisions. It returns the exit to take when
// no candidate answered.
func (l *Loop) send(
	ctx context.Context,
	sessionID string,
	step int64,
	opts Options,
	stall Stall,
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
		built, err := window.Build(in, b)
		if errors.Is(err, window.ErrUnpaired) {
			return attempt{}, &ExitCause{Status: StatusFailed, Kind: KindTranscriptUnpaired, Err: err}
		}
		if err != nil {
			// ErrOverflow: the newest group does not fit this model's budget; a model with a larger
			// context may take it (§5).
			next, decision := FailoverDecision(cands, i, protocol.ClassCapacity)
			l.emit("window.overflow", sessionID, map[string]any{"step": step, "modelRef": cand.Ref, "error": err.Error()})
			if next < 0 {
				kind := KindWindowOverflow
				if _, capped := r.transcriptCap[cand.Ref]; capped {
					kind = KindProviderCapacity
				}
				return attempt{}, &ExitCause{Status: StatusFailed, Kind: kind, Err: fmt.Errorf("model %s: %w", cand.Ref, err)}
			}
			*decisions = append(*decisions, decision)
			i = next
			continue
		}
		if built.Dropped+r.trimmed > 0 {
			*decisions = append(*decisions, WindowDroppedDecision(cand.Ref, b, fixed, groups+r.trimmed, built.Costs))
		}
		sent, effortDecision := EffortDecision(opts.EffortDefault, opts.Effort, opts.Escalation, stall, taskStateVersion, m.Efforts)
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
		usage.Requests++
		l.emit("llm.request", sessionID, map[string]any{
			"step": step, "modelRef": cand.Ref, "provider": m.ProviderID, "model": m.ModelID,
			"messages": len(built.Messages), "tools": toolNames(specs), "reasoningEffort": req.ReasoningEffort,
			"candidateIndex": i, "windowFirstSeq": built.FirstSeq, "windowTokens": built.Tokens,
		})
		started := time.Now()
		response, err := m.Client.Complete(ctx, req)
		elapsed := time.Since(started)
		if err == nil {
			l.Health.Success(cand.Ref)
			l.emit("llm.complete", sessionID, map[string]any{
				"step": step, "modelRef": cand.Ref, "provider": m.ProviderID, "model": m.ModelID,
				"elapsedMs": elapsed.Milliseconds(), "candidateIndex": i,
			})
			return attempt{Response: response, Model: m, Elapsed: elapsed, Window: built}, nil
		}
		class := Classify(ctx, err)
		if class == protocol.ClassCancelled {
			cause := exitFor(class, ctx.Err())
			return attempt{}, &cause
		}
		if class == protocol.ClassAvailability {
			l.Health.Fail(cand.Ref, retryAfter(err))
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
		next, decision := FailoverDecision(cands, i, class)
		event := map[string]any{
			"step": step, "modelRef": cand.Ref, "provider": m.ProviderID, "model": m.ModelID,
			"class": class, "error": err.Error(), "elapsedMs": elapsed.Milliseconds(),
		}
		if next >= 0 {
			event["nextModelRef"] = cands[next].Ref
		}
		l.emit("provider.failover", sessionID, event)
		if next < 0 {
			cause := exitFor(class, fmt.Errorf("model %s: %w", cand.Ref, err))
			return attempt{}, &cause
		}
		*decisions = append(*decisions, decision)
		i = next
	}
}
