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
	Mode                   ProjectionMode
	Reason                 string
	SentLayers             []SystemLayer
	System                 []protocol.Message
	Tools                  []protocol.ToolSpec
	ToolTokens             int
	FullFixed              int
	CompactFixed           int
	Fixed                  int
	GuardTokens            int
	BaseGateSlack          int
	EffectiveGateSlack     int
	UsableFull             int
	UsableCompact          int
	UsableInput            int
	MinRequiredSeq         int64
	OriginalMinRequiredSeq int64
	FloorRelaxed           bool
	FloorRelaxedReason     string
	FloorRelaxedDecision   *Decision
	Budget                 window.Budget
	Window                 window.Result
	Echo                   []protocol.Echo
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

// buildSystemMessages is the request's fixed system prefix: the default prompt, then each labelled
// layer in order (the global instructions, the project instructions, the skills prompt), then the
// agent's instructions. Instructions never replace or precede the default prompt, and every layer is
// a message of its own so the model can attribute it to its source.
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

// compactToolPrefForCandidate returns the tool description variant used for ModeCompact on cand.
// Models >=16K keep built-in tool descriptions verbatim unless ProfileCompact was configured; small
// windows (e.g. 8K under Policy "full") whose verbatim built-in schemas would fail static admission
// automatically fall back to the REV-F06 safety-preserving built-in compact descriptions (F-06).
func compactToolPrefForCandidate(
	cand Candidate,
	guard, outBudget int,
	compactSystem []protocol.Message,
	state protocol.Message,
	included []StepTool,
	toolPref string,
) string {
	if toolPref == config.ProfileCompact {
		return config.ProfileCompact
	}
	verbatimTools := presentStepTools(included, toolPref, ModeCompact)
	verbatimFixed := window.Fixed(window.Input{
		System:     compactSystem,
		State:      state,
		ToolTokens: window.Estimate(verbatimTools),
	})
	if cand.Context < verbatimFixed+guard+outBudget {
		return config.ProfileCompact
	}
	return toolPref
}

func formatCausalEvidenceSummary(ref CausalEvidenceRef) string {
	parts := []string{fmt.Sprintf("seq=%d", ref.AnsweredSeq)}
	if ref.ToolName != "" {
		parts = append(parts, "tool="+ref.ToolName)
	}
	if ref.ExecutionID != "" {
		parts = append(parts, "exec="+ref.ExecutionID)
	}
	parts = append(parts, "evidence="+string(ref.MutationEvidence))
	return strings.Join(parts, " ")
}

func appendFloorRelaxationNotice(stateText string, repair repairContext, strictMinReqSeq int64) string {
	origAnchor := repair.OriginalMinRequiredSeq
	if origAnchor < 0 {
		origAnchor = strictMinReqSeq
	}
	notice := fmt.Sprintf(
		"[Context Window Notice — Floor Relaxation]\n"+
			"Required repair history starting at seq %d (original anchor seq %d) exceeds the largest available model window, so earlier messages prior to the transmitted window have been omitted. "+
			"Active completion-gate feedback (seq %d) and the latest atomic step are preserved in the message window. "+
			"Before modifying code or proposing completion, re-inspect current workspace state (for example via read_file, search_text, or git status/git diff) rather than relying on omitted earlier steps.",
		strictMinReqSeq, origAnchor, repair.BlockSeq,
	)
	if strings.TrimSpace(stateText) == "" {
		return notice
	}
	return stateText + "\n\n" + notice
}

func appendPinnedGateAndRelaxationNotice(stateText string, repair repairContext, strictMinReqSeq int64, formattedGateFeedback string) string {
	origAnchor := repair.OriginalMinRequiredSeq
	if origAnchor < 0 {
		origAnchor = strictMinReqSeq
	}
	var b strings.Builder
	if strings.TrimSpace(stateText) != "" {
		b.WriteString(stateText)
		b.WriteString("\n\n")
	}
	fmt.Fprintf(&b, "[Pinned Completion-Gate State — seq %d (refusal #%d, stage: %s)]\n",
		repair.BlockSeq, max(1, repair.BlockCount), repair.Stage)
	if strings.TrimSpace(formattedGateFeedback) != "" {
		b.WriteString(formattedGateFeedback)
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "Causal repair ledger: originalAnchorSeq=%d, strictFloorSeq=%d, postGateProven=%d, postGateUnknown=%d",
		origAnchor, strictMinReqSeq, repair.PostBlockProvenCount, repair.PostBlockUnknownCount)
	if repair.HasOriginalEvidence {
		fmt.Fprintf(&b, "; originalEvidence=[%s]", formatCausalEvidenceSummary(repair.OriginalEvidence))
	}
	if repair.CausalProgressLen > 0 {
		b.WriteString("; recentPostGateMutations=[")
		for i := 0; i < repair.CausalProgressLen; i++ {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(formatCausalEvidenceSummary(repair.CausalProgress[i]))
		}
		b.WriteByte(']')
	}
	b.WriteString("\n\n")
	fmt.Fprintf(&b,
		"[Context Window Notice — Floor Relaxation]\n"+
			"Transcript messages from seq %d through gate refusal seq %d and older post-gate steps prior to the transmitted rolling suffix have been omitted to fit the largest available model window; "+
			"active gate feedback (seq %d) and causal repair progress are pinned above, and the latest atomic step is preserved. "+
			"Before modifying code or proposing completion, re-inspect current workspace state (for example via read_file, search_text, or git status/git diff) rather than assuming omitted steps.",
		origAnchor, repair.BlockSeq, repair.BlockSeq,
	)
	return b.String()
}

func sliceTranscriptAfterSeq(transcript []window.Entry, afterSeq int64) ([]window.Entry, int, error) {
	groups, err := window.Groups(transcript)
	if err != nil {
		return nil, 0, err
	}
	cutGroups := 0
	cutEntries := 0
	for _, g := range groups {
		if g[len(g)-1].Seq <= afterSeq {
			cutGroups++
			cutEntries += len(g)
			continue
		}
		break
	}
	return transcript[cutEntries:], cutGroups, nil
}

// projectCandidateForSend runs strict projectCandidate first; only when allowFloorRelaxation is true
// (after all larger candidates were exhausted) and strict MinRequiredSeq overflowed does it evaluate
// monotonic 3-stage controlled Floor Relaxation on the largest available candidate (ADR-0024 §7, F-03, R2-02):
//   - Stage R1: retain [BlockSeq .. LastSeq] verbatim in Messages (no GateFeedback duplication in State);
//   - Stage R2: pin Gate State + Causal Ledger in State and project Seq > BlockSeq from PostBlockCausalFloorSeq;
//   - Stage R3: pin Gate State + Causal Ledger in State and roll Seq > BlockSeq to fit the newest atomic group.
func projectCandidateForSend(
	r *run,
	cand Candidate,
	m Model,
	opts Options,
	canonical canonicalStepInput,
	transcript []window.Entry,
	repair repairContext,
	minRequiredSeq int64,
	turnProgressSeq int64,
	allowFloorRelaxation bool,
) (candidateProjection, Decision, error) {
	proj, prefixDec, err := projectCandidate(r, cand, m, opts, canonical, transcript, repair, minRequiredSeq)
	if err == nil || !allowFloorRelaxation || !repair.Active || minRequiredSeq < 0 || !errors.Is(err, window.ErrOverflow) {
		return proj, prefixDec, err
	}
	txGroups, gerr := window.Groups(transcript)
	if gerr != nil {
		return candidateProjection{}, Decision{}, gerr
	}
	if len(txGroups) == 0 {
		return candidateProjection{}, Decision{}, err
	}
	newestGroupFirstSeq := txGroups[len(txGroups)-1][0].Seq
	newestRequiredSeq := newestGroupFirstSeq
	if turnProgressSeq >= 0 {
		newestRequiredSeq = minPositiveSeq(turnProgressSeq, newestRequiredSeq)
	}
	if newestRequiredSeq < 0 {
		return candidateProjection{}, Decision{}, err
	}

	finalizeRelaxed := func(relProj candidateProjection, relPrefixDec Decision, relaxedFloorSeq int64, gatePinned bool) (candidateProjection, Decision, error) {
		if relProj.Window.FirstSeq <= minRequiredSeq {
			return relProj, relPrefixDec, nil
		}
		floorDec := WindowFloorRelaxedDecision(
			cand.Ref,
			cand.Context,
			relProj.UsableInput,
			relProj.Fixed,
			minRequiredSeq,
			repair.OriginalMinRequiredSeq,
			repair.PostBlockCausalFloorSeq,
			relaxedFloorSeq,
			repair.BlockSeq,
			newestGroupFirstSeq,
			relProj.Window.FirstSeq,
			relProj.Window.Tokens,
			gatePinned,
		)
		relProj.OriginalMinRequiredSeq = minRequiredSeq
		relProj.FloorRelaxed = true
		relProj.FloorRelaxedReason = FloorRelaxReasonMaxCandidateOverflow
		relProj.FloorRelaxedDecision = &floorDec
		return relProj, relPrefixDec, nil
	}

	// Stage R1: keep [BlockSeq .. LastSeq] intact in Messages when BlockSeq is still present in transcript.
	if len(transcript) > 0 && transcript[0].Seq <= repair.BlockSeq {
		floorR1 := minPositiveSeq(repair.BlockSeq, newestRequiredSeq)
		r1Canonical := canonical
		r1Canonical.State = protocol.Message{
			Role:    canonical.State.Role,
			Origin:  canonical.State.Origin,
			Content: appendFloorRelaxationNotice(canonical.State.Content, repair, minRequiredSeq),
		}
		r1Repair := repair
		r1Repair.MinRequiredSeq = floorR1
		r1Repair.PinGateInState = false
		if r1Proj, r1PrefixDec, r1Err := projectCandidate(r, cand, m, opts, r1Canonical, transcript, r1Repair, floorR1); r1Err == nil {
			return finalizeRelaxed(r1Proj, r1PrefixDec, floorR1, false)
		}
	}

	// Stages R2 & R3: [BlockSeq .. LastSeq] overflowed (or BlockSeq was trimmed past maxContext).
	// Pin Gate State + Causal Repair Ledger in State and project strictly over Seq > BlockSeq so
	// OriginCompletionBlocked never appears in both State and Messages.
	afterBlock, preSlicedGroups, sliceErr := sliceTranscriptAfterSeq(transcript, repair.BlockSeq)
	if sliceErr != nil {
		return candidateProjection{}, Decision{}, sliceErr
	}
	if len(afterBlock) == 0 {
		return candidateProjection{}, Decision{}, err
	}

	// Stage R2: drop older post-block MutationNone read/chat bodies while keeping all post-block
	// Proven/Unknown causal groups [PostBlockCausalFloorSeq .. LastSeq] when they fit.
	if repair.PostBlockCausalFloorSeq > repair.BlockSeq && afterBlock[0].Seq <= repair.PostBlockCausalFloorSeq {
		floorR2 := minPositiveSeq(repair.PostBlockCausalFloorSeq, newestRequiredSeq)
		r2Repair := repair
		r2Repair.MinRequiredSeq = floorR2
		r2Repair.PinGateInState = true
		if r2Proj, r2PrefixDec, r2Err := projectCandidate(r, cand, m, opts, canonical, afterBlock, r2Repair, floorR2); r2Err == nil {
			r2Proj.Window.Dropped += preSlicedGroups
			return finalizeRelaxed(r2Proj, r2PrefixDec, floorR2, true)
		}
	}

	// Stage R3: post-block causal groups themselves exceed the largest available window; greedily
	// roll Seq > BlockSeq to retain the maximum suffix containing newestRequiredSeq while keeping
	// Gate State + Causal Progress Ledger pinned in State.
	floorR3 := newestRequiredSeq
	r3Repair := repair
	r3Repair.MinRequiredSeq = floorR3
	r3Repair.PinGateInState = true
	r3Proj, r3PrefixDec, r3Err := projectCandidate(r, cand, m, opts, canonical, afterBlock, r3Repair, floorR3)
	if r3Err != nil {
		return candidateProjection{}, Decision{}, r3Err
	}
	r3Proj.Window.Dropped += preSlicedGroups
	return finalizeRelaxed(r3Proj, r3PrefixDec, floorR3, true)
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
	outBudget := OutputBudget(opts.MaxTokens, cand)
	guard := GuardTokens(cand.Context, m.ProviderPkg, len(canonical.IncludedTools))

	fullState := canonical.State
	compactState := canonical.State
	if repair.PinGateInState {
		fullGate := presentFeedback(repair.GateFeedback, effectiveFeedbackVariant(m, opts.Presentation, ModeFull))
		compactGate := presentFeedback(repair.GateFeedback, effectiveFeedbackVariant(m, opts.Presentation, ModeCompact))
		strictMin := repair.OriginalMinRequiredSeq
		if strictMin < 0 {
			strictMin = minRequiredSeq
		}
		fullState = protocol.Message{
			Role:    canonical.State.Role,
			Origin:  canonical.State.Origin,
			Content: appendPinnedGateAndRelaxationNotice(canonical.State.Content, repair, strictMin, fullGate),
		}
		compactState = protocol.Message{
			Role:    canonical.State.Role,
			Origin:  canonical.State.Origin,
			Content: appendPinnedGateAndRelaxationNotice(canonical.State.Content, repair, strictMin, compactGate),
		}
	}

	fullLayers := projectLayers(canonical.Layers, ModeFull)
	fullSystem := buildSystemMessages(canonical.SystemPrompt, fullLayers, canonical.Instructions)
	fullTools := presentStepTools(canonical.IncludedTools, toolPref, ModeFull)

	compactLayers := projectLayers(canonical.Layers, ModeCompact)
	compactSystem := buildSystemMessages(canonical.SystemPrompt, compactLayers, canonical.Instructions)
	compactToolPref := compactToolPrefForCandidate(cand, guard, outBudget, compactSystem, compactState, canonical.IncludedTools, toolPref)
	compactTools := presentStepTools(canonical.IncludedTools, compactToolPref, ModeCompact)

	// Step 2: Fixed prefixes, candidate base pool, and required transcript suffix tokens.
	fullToolTokens := window.Estimate(fullTools)
	compactToolTokens := window.Estimate(compactTools)
	fullFixed := window.Fixed(window.Input{System: fullSystem, State: fullState, ToolTokens: fullToolTokens})
	compactFixed := window.Fixed(window.Input{System: compactSystem, State: compactState, ToolTokens: compactToolTokens})

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
		if len(txGroups) > 0 && txGroups[0][0].Seq > repair.MinRequiredSeq {
			requiredRepairTokens = max(requiredRepairTokens, cand.Context+1)
		}
	}
	requiredTranscript := 0
	if minRequiredSeq >= 0 {
		for _, g := range txGroups {
			if g[len(g)-1].Seq >= minRequiredSeq {
				requiredTranscript += window.GroupTokens(g)
			}
		}
		if len(txGroups) > 0 && txGroups[0][0].Seq > minRequiredSeq {
			requiredTranscript = max(requiredTranscript, cand.Context+1)
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
	sentState := fullState
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
		sentState = compactState
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
		State:      sentState,
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
		cand := Candidate{Ref: ref, Context: m.Context, Output: m.Output, ToolCalling: m.ToolCalling}
		models[ref] = cand
		guard := GuardTokens(m.Context, m.ProviderPkg, len(canonical.IncludedTools))
		outBudget := OutputBudget(opts.MaxTokens, cand)
		toolPref := effectiveVariant(m.Presentation.ToolDescriptions, opts.Presentation.ToolDescriptions)
		compactPref := compactToolPrefForCandidate(cand, guard, outBudget, compactSystem, canonical.State, canonical.IncludedTools, toolPref)
		compactTools := presentStepTools(canonical.IncludedTools, compactPref, ModeCompact)
		compactFixed[ref] = window.Fixed(window.Input{
			System:     compactSystem,
			State:      canonical.State,
			ToolTokens: window.Estimate(compactTools),
		})
		guards[ref] = guard
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
	strictFloorOverflowed := map[int]bool{}
	unavailableForRelax := make([]bool, len(cands))
	floorRelaxedMode := false
	floorRelaxationAttempted := false

	tryEnterFloorRelaxation := func(failedIdx int) (int, bool) {
		if floorRelaxationAttempted {
			return -1, false
		}
		rep, repErr := r.deriveRepairContext(ctx, l.Store, sessionID)
		if repErr != nil || !rep.Active || rep.MinRequiredSeq < 0 {
			return -1, false
		}
		hasCandidateForRelax := false
		for k := range cands {
			if !unavailableForRelax[k] && strictFloorOverflowed[k] {
				hasCandidateForRelax = true
				break
			}
		}
		if !hasCandidateForRelax {
			return -1, false
		}
		floorRelaxationAttempted = true
		bestIdx := SelectMaxAvailableCandidate(cands, unavailableForRelax)
		if bestIdx < 0 {
			return -1, false
		}
		exhausted[bestIdx] = false
		if bestIdx != failedIdx {
			_, fbDec := FloorFallbackDecision(cands, failedIdx, unavailableForRelax)
			*decisions = append(*decisions, fbDec)
		}
		floorRelaxedMode = true
		recovering = false
		pendingRetry = false
		return bestIdx, true
	}

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
			for k := range down.ready {
				unavailableForRelax[k] = true
			}
			if nextIdx, ok := tryEnterFloorRelaxation(failed); ok {
				if failure != nil {
					failure["nextModelRef"] = cands[nextIdx].Ref
					l.emit("provider.failover", sessionID, failure)
				}
				return nextIdx, nil
			}
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
			unavailableForRelax[failed] = true
			for k := range down.ready {
				unavailableForRelax[k] = true
			}
			if nextIdx, ok := tryEnterFloorRelaxation(failed); ok {
				retry["stop"] = stop
				retry["floorFallbackModelRef"] = cands[nextIdx].Ref
				l.emit("provider.retry_stopped", sessionID, retry)
				return nextIdx, nil
			}
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
	handleOverflow := func(idx int, cand Candidate, overErr error) (int, *ExitCause) {
		l.emit("window.overflow", sessionID, map[string]any{"step": step, "modelRef": cand.Ref, "error": overErr.Error()})
		exhausted[idx] = true
		if !floorRelaxedMode {
			if rep, repErr := r.deriveRepairContext(ctx, l.Store, sessionID); repErr == nil && rep.Active && rep.MinRequiredSeq >= 0 {
				strictFloorOverflowed[idx] = true
			}
		}
		if !recovering {
			if next, decision := FailoverDecision(cands, idx, protocol.ClassCapacity); next >= 0 {
				*decisions = append(*decisions, decision)
				return next, nil
			}
		}
		if len(down.ready) == 0 {
			if nextIdx, ok := tryEnterFloorRelaxation(idx); ok {
				return nextIdx, nil
			}
			kind := KindWindowOverflow
			if _, capped := r.transcriptCap[cand.Ref]; capped {
				kind = KindProviderCapacity
			}
			return -1, &ExitCause{Status: StatusFailed, Kind: kind, Err: fmt.Errorf("model %s: %w", cand.Ref, overErr)}
		}
		// Another candidate failed with availability and may answer once it recovers.
		recovering = true
		return recoverFrom(idx, protocol.ClassCapacity, overErr, nil)
	}
	for i := 0; ; {
		cand := cands[i]
		m := l.Models[cand.Ref]
		repair, repairErr := r.deriveRepairContext(ctx, l.Store, sessionID)
		if repairErr != nil {
			return attempt{}, &ExitCause{Status: StatusFailed, Kind: KindStorage, Err: repairErr}
		}
		due, _ := TurnBoundary(r.turnRows(), cand.Ref)
		previewTranscript := r.transcript
		minReqSeq := repair.MinRequiredSeq
		turnProgressSeq := int64(-1)
		if due {
			previewTranscript, turnProgressSeq = r.cloneForTurnPreview()
			minReqSeq = minPositiveSeq(turnProgressSeq, repair.MinRequiredSeq)
		}
		proj, prefixDec, err := projectCandidateForSend(r, cand, m, opts, canonical, previewTranscript, repair, minReqSeq, turnProgressSeq, floorRelaxedMode)
		if errors.Is(err, window.ErrUnpaired) {
			return attempt{}, &ExitCause{Status: StatusFailed, Kind: KindTranscriptUnpaired, Err: err}
		}
		if err != nil {
			// ErrOverflow (including minRequiredSeq floor or TurnBoundary dry-run overflow):
			// move to a larger candidate or outage recovery without writing OriginTurn to SQLite (§4.8).
			next, cause := handleOverflow(i, cand, err)
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
			repair, err = r.deriveRepairContext(ctx, l.Store, sessionID)
			if err != nil {
				return attempt{}, &ExitCause{Status: StatusFailed, Kind: KindStorage, Err: err}
			}
			if g, gerr := r.groups(l.maxContext()); gerr == nil {
				groups = len(g)
			}
		}
		// Turn rule (§4.7, §4.8): re-check TurnBoundary after BeforeModelCall / applyPendingInputs.
		// When rebuiltInputs changed State or transcript, dry-run projectCandidate on an in-memory clone BEFORE
		// writing OriginTurn to SQLite so post-hook overflow neither leaves orphan OriginTurn rows nor wipes r.turn.
		var turnDec Decision
		due, turnDec = TurnBoundary(r.turnRows(), cand.Ref)
		minReqSeq = repair.MinRequiredSeq
		turnProgressSeq = -1
		if rebuiltInputs {
			postPreview := r.transcript
			if due {
				postPreview, turnProgressSeq = r.cloneForTurnPreview()
				minReqSeq = minPositiveSeq(turnProgressSeq, repair.MinRequiredSeq)
			}
			var rebuildErr error
			proj, prefixDec, rebuildErr = projectCandidateForSend(r, cand, m, opts, canonical, postPreview, repair, minReqSeq, turnProgressSeq, floorRelaxedMode)
			if errors.Is(rebuildErr, window.ErrUnpaired) {
				return attempt{}, &ExitCause{Status: StatusFailed, Kind: KindTranscriptUnpaired, Err: rebuildErr}
			}
			if rebuildErr != nil {
				next, cause := handleOverflow(i, cand, rebuildErr)
				if cause != nil {
					return attempt{}, cause
				}
				i = next
				continue
			}
		}
		if due {
			if g, gerr := window.Groups(r.transcript); gerr == nil && len(g) > 0 {
				turnProgressSeq = g[len(g)-1][0].Seq
				minReqSeq = minPositiveSeq(turnProgressSeq, repair.MinRequiredSeq)
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
			var commitProjErr error
			proj, prefixDec, commitProjErr = projectCandidateForSend(r, cand, m, opts, canonical, r.transcript, repair, minReqSeq, turnProgressSeq, floorRelaxedMode)
			if errors.Is(commitProjErr, window.ErrUnpaired) {
				return attempt{}, &ExitCause{Status: StatusFailed, Kind: KindTranscriptUnpaired, Err: commitProjErr}
			}
			if commitProjErr != nil {
				next, cause := handleOverflow(i, cand, commitProjErr)
				if cause != nil {
					return attempt{}, cause
				}
				i = next
				continue
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
		if proj.FloorRelaxed && proj.FloorRelaxedDecision != nil {
			*decisions = append(*decisions, *proj.FloorRelaxedDecision)
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
			unavailableForRelax[i] = true
		}
		if class == protocol.ClassAuth || class == protocol.ClassBadRequest {
			unavailableForRelax[i] = true
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
			unavailableForRelax[i] = true
			if nextIdx, ok := tryEnterFloorRelaxation(i); ok {
				failure["nextModelRef"] = cands[nextIdx].Ref
				l.emit("provider.failover", sessionID, failure)
				i = nextIdx
				continue
			}
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
