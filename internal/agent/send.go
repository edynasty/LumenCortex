package agent

import (
	"fmt"
	"time"

	"context"
	"github.com/edynasty/LumenCortex/config"
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

// attempt is the request that answered a step.
type attempt struct {
	Response protocol.ProviderResponse
	Model    Model
	Elapsed  time.Duration
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
		// The capacity retry with a halved transcript is M3.2; until then a request no candidate
		// with a larger context can take fails the run.
		return ExitCause{Status: StatusFailed, Kind: KindProviderCapacity, Err: err}
	default:
		return ExitCause{Status: StatusInterrupted, Kind: KindProviderAvailability, Err: err}
	}
}

// send runs one step's request against the candidates until one answers (design §3.3 step 6). Every
// move is appended to decisions. It returns the exit to take when no candidate answered.
func (l *Loop) send(
	ctx context.Context,
	sessionID string,
	step int64,
	opts Options,
	cands []Candidate,
	messages []protocol.Message,
	specs []protocol.ToolSpec,
	usage *protocol.Usage,
	decisions *[]Decision,
) (attempt, *ExitCause) {
	for i := 0; ; {
		cand := cands[i]
		m := l.Models[cand.Ref]
		sent, effortDecision := EffortDecision(opts.EffortDefault, opts.Effort, m.Efforts)
		*decisions = append(*decisions, effortDecision)
		// max_tokens is what the caller asked for, else what the model's configuration says, else
		// nothing: the effective default only reserves capacity (D4). It is never scaled by effort.
		maxTokens := opts.MaxTokens
		if maxTokens <= 0 && m.OutputSet {
			maxTokens = m.Output
		}
		req := protocol.ProviderRequest{
			Messages: messages, Tools: specs, ToolChoice: "auto", Temperature: opts.Temperature,
			MaxTokens: maxTokens, ReasoningEffort: string(sent),
		}
		usage.Requests++
		l.emit("llm.request", sessionID, map[string]any{
			"step": step, "modelRef": cand.Ref, "provider": m.ProviderID, "model": m.ModelID,
			"messages": len(messages), "tools": toolNames(specs), "reasoningEffort": req.ReasoningEffort,
			"candidateIndex": i,
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
			return attempt{Response: response, Model: m, Elapsed: elapsed}, nil
		}
		class := Classify(ctx, err)
		if class == protocol.ClassCancelled {
			cause := exitFor(class, ctx.Err())
			return attempt{}, &cause
		}
		if class == protocol.ClassAvailability {
			l.Health.Fail(cand.Ref, retryAfter(err))
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
