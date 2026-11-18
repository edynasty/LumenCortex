package agent

import (
	"context"
	"slices"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/window"
	"github.com/edynasty/LumenCortex/protocol"
)

// runTranscript is the source of a run's request windows (design §5) and its run memory (§4.6).
//
// transcript holds the rows from the Task State's windowStartSeq on, oldest first, with their seqs. It
// is read once when the run starts and extended with every row the run appends. While a run holds the
// session's run lock nothing else appends rows or moves windowStartSeq (design §4.2, §4.4), so it
// stays equal to the stored transcript. Leading groups that no window can include are dropped from
// memory; trimmed counts them, so dropped-group counts still cover everything from windowStartSeq.
//
// transcriptCap is the transcript budget a capacity retry left for a model; it holds until the run
// ends (design §5).
//
// turn is the run memory of the current turn: for each assistant row this run produced after the
// last user row, the model that produced it and the echo the adapter returned with it. It is never
// written anywhere and goes when the run returns. A user row ends the turn and empties it, so it holds
// at most one turn of the transcript in memory; the window counts an entry's echo toward its row
// (Entry.EchoTokens) only while the entry is in it.
//
// mutations maps each tool-calling assistant row's answered_seq to its deterministic three-valued
// causal MutationEvidence, reconstructed from persisted execution records and transcript rows on load
// and refreshed at the end of each tool step (ADR-0024 §4.6).
type runTranscript struct {
	transcript    []window.Entry
	trimmed       int
	transcriptCap map[config.ModelRef]int
	turn          map[int64]turnEntry
	mutations     map[int64]MutationEvidence
}

type turnEntry struct {
	model config.ModelRef
	echo  protocol.Echo
}

// load reads the transcript from the run's windowStartSeq and reconstructs causal MutationEvidence for
// all answered tool-call groups in range. The rows at withheld seqs (sorted) are read with their content
// withheld: they carry content of a revoked source (window.Withhold).
func (r *run) load(ctx context.Context, store Store, sessionID string, withheld []int64) error {
	if err := store.Transcript(ctx, sessionID, r.ts.WindowStartSeq, func(seq int64, m protocol.Message) error {
		if _, found := slices.BinarySearch(withheld, seq); found {
			m = window.Withhold(m)
		}
		r.appendRow(seq, m)
		return nil
	}); err != nil {
		return err
	}
	summaries, err := store.ListAnsweredExecutions(ctx, sessionID, r.ts.WindowStartSeq)
	if err != nil {
		return err
	}
	r.populateMutations(r.transcript, summaries)
	return nil
}

// refreshGroupMutation recomputes r.mutations[answeredSeq] from the persisted execution summaries and
// transcript rows of that answered group.
func (r *run) refreshGroupMutation(ctx context.Context, store Store, sessionID string, answeredSeq int64) error {
	summaries, err := store.ListAnsweredExecutions(ctx, sessionID, answeredSeq)
	if err != nil {
		return err
	}
	var matching []session.AnsweredExecutionSummary
	for _, s := range summaries {
		if s.AnsweredSeq == answeredSeq {
			matching = append(matching, s)
		}
	}
	for i := len(r.transcript) - 1; i >= 0; i-- {
		if r.transcript[i].Seq != answeredSeq {
			continue
		}
		assistant := r.transcript[i].Msg
		var toolRows []protocol.Message
		for j := i + 1; j < len(r.transcript) && r.transcript[j].Msg.Role == "tool"; j++ {
			toolRows = append(toolRows, r.transcript[j].Msg)
		}
		if r.mutations == nil {
			r.mutations = map[int64]MutationEvidence{}
		}
		r.mutations[answeredSeq] = aggregateGroupMutation(assistant, toolRows, matching)
		return nil
	}
	return nil
}

func (r *run) populateMutations(entries []window.Entry, summaries []session.AnsweredExecutionSummary) {
	if r.mutations == nil {
		r.mutations = map[int64]MutationEvidence{}
	}
	bySeq := make(map[int64][]session.AnsweredExecutionSummary)
	for _, s := range summaries {
		bySeq[s.AnsweredSeq] = append(bySeq[s.AnsweredSeq], s)
	}
	for i := 0; i < len(entries); {
		e := entries[i]
		if e.Msg.Role != "assistant" || len(e.Msg.ToolCalls) == 0 {
			i++
			continue
		}
		j := i + 1
		var toolRows []protocol.Message
		for ; j < len(entries) && entries[j].Msg.Role == "tool"; j++ {
			toolRows = append(toolRows, entries[j].Msg)
		}
		r.mutations[e.Seq] = aggregateGroupMutation(e.Msg, toolRows, bySeq[e.Seq])
		i = j
	}
}

// appendRow records a row, read at load or written by the run, so the next window sees it. A user row
// opens a new turn: the previous turn's echo is dropped and no longer counts toward its rows.
func (r *run) appendRow(seq int64, m protocol.Message) {
	if m.Role == "user" && len(r.turn) > 0 {
		for i := len(r.transcript) - 1; i >= 0 && r.transcript[i].Msg.Role != "user"; i-- {
			r.transcript[i].EchoTokens = 0
		}
		clear(r.turn)
	}
	r.transcript = append(r.transcript, window.Entry{Seq: seq, Msg: m})
}

// appendAssistant records an assistant row this run received from model, with its echo.
func (r *run) appendAssistant(seq int64, m protocol.Message, model config.ModelRef, echo protocol.Echo) {
	r.transcript = append(r.transcript, window.Entry{Seq: seq, Msg: m, EchoTokens: echo.Tokens()})
	if r.turn == nil {
		r.turn = map[int64]turnEntry{}
	}
	r.turn[seq] = turnEntry{model: model, echo: echo}
}

// turnRows is what the turn rule reads: the rows after the last user row, each assistant row with
// the model that produced it in this run, or "" when this run did not.
func (r *run) turnRows() []TurnRow {
	start := len(r.transcript)
	for start > 0 && r.transcript[start-1].Msg.Role != "user" {
		start--
	}
	if start == len(r.transcript) {
		return nil
	}
	rows := make([]TurnRow, 0, len(r.transcript)-start)
	for _, e := range r.transcript[start:] {
		row := TurnRow{Role: e.Msg.Role}
		if e.Msg.Role == "assistant" {
			row.Producer = r.turn[e.Seq].model
		}
		rows = append(rows, row)
	}
	return rows
}

// requestEcho is ProviderRequest.Echo for built, a window of transcript: aligned with its
// messages, it holds the echo of each current-turn assistant row target produced in this run. The
// window's transcript rows are the contiguous suffix from built.FirstSeq and end its messages (I4).
// nil when there is none.
func (r *run) requestEcho(built window.Result, target config.ModelRef) []protocol.Echo {
	return r.requestEchoFrom(r.transcript, built, target)
}

func (r *run) requestEchoFrom(transcript []window.Entry, built window.Result, target config.ModelRef) []protocol.Echo {
	if len(r.turn) == 0 || built.FirstSeq < 0 {
		return nil
	}
	lastUser := -1
	for i := len(transcript) - 1; i >= 0; i-- {
		if transcript[i].Msg.Role == "user" {
			lastUser = i
			break
		}
	}
	first := len(transcript)
	for first > 0 && transcript[first-1].Seq >= built.FirstSeq {
		first--
	}
	offset := len(built.Messages) - (len(transcript) - first)
	if offset < 0 {
		return nil
	}
	var out []protocol.Echo
	for k, e := range transcript[first:] {
		if first+k <= lastUser {
			continue
		}
		t, ok := r.turn[e.Seq]
		if !ok || t.model != target || t.echo.IsZero() {
			continue
		}
		if out == nil {
			out = make([]protocol.Echo, len(built.Messages))
		}
		out[offset+k] = t.echo
	}
	return out
}

// groups splits the transcript into window groups after dropping the leading ones no window can
// include, while clamping keep so no group with Seq >= minKeepSeq (when minKeepSeq >= 0) is ever
// evicted from memory (ADR-0024 §4.6.6).
func (r *run) groups(maxContext int, minKeepSeq int64) ([][]window.Entry, error) {
	groups, err := window.Groups(r.transcript)
	if err != nil {
		return nil, err
	}
	keep, used := 0, 0
	for k := len(groups) - 1; k >= 0; k-- {
		used += window.GroupTokens(groups[k])
		if used > maxContext {
			keep = k
			break
		}
	}
	if minKeepSeq >= 0 {
		for keep > 0 && groups[keep-1][len(groups[keep-1])-1].Seq >= minKeepSeq {
			keep--
		}
	}
	if keep == 0 {
		return groups, nil
	}
	rows := 0
	for _, g := range groups[:keep] {
		rows += len(g)
	}
	n := copy(r.transcript, r.transcript[rows:])
	clear(r.transcript[n:])
	r.transcript = r.transcript[:n]
	r.trimmed += keep
	for seq := range r.turn {
		if n == 0 || seq < r.transcript[0].Seq {
			delete(r.turn, seq)
		}
	}
	for seq := range r.mutations {
		if n == 0 || seq < r.transcript[0].Seq {
			delete(r.mutations, seq)
		}
	}
	return window.Groups(r.transcript)
}

// cloneForTurnPreview constructs an in-memory preview transcript with trailing non-user EchoTokens
// zeroed and a synthetic OriginTurn appended, returning the cloned transcript and the Minimum Progress
// Guard floor seq (the first seq of the newest pre-turn group, or -1 if none) without touching SQLite.
func (r *run) cloneForTurnPreview() ([]window.Entry, int64) {
	groups, err := window.Groups(r.transcript)
	progressFloor := int64(-1)
	if err == nil && len(groups) > 0 {
		progressFloor = groups[len(groups)-1][0].Seq
	}
	clone := make([]window.Entry, len(r.transcript), len(r.transcript)+1)
	copy(clone, r.transcript)
	for i := len(clone) - 1; i >= 0 && clone[i].Msg.Role != "user"; i-- {
		clone[i].EchoTokens = 0
	}
	nextSeq := int64(0)
	if len(clone) > 0 {
		nextSeq = clone[len(clone)-1].Seq + 1
	}
	clone = append(clone, window.Entry{
		Seq: nextSeq,
		Msg: protocol.Message{Role: "user", Origin: protocol.OriginTurn, Content: protocol.TurnContent},
	})
	return clone, progressFloor
}

func minPositiveSeq(a, b int64) int64 {
	switch {
	case a < 0:
		return b
	case b < 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
}

// immutableNonMutatingAdmittedTools is the compile-time closed whitelist of non-mutating tools that
// may pass through tierAdmit (delegation/subagent starters plus the 15 read-only callback builtins).
var immutableNonMutatingAdmittedTools = map[string]bool{
	toolStartTask: true, toolSpawnSubagent: true,
	"read_file": true, "list_dir": true, "find_files": true, "search_text": true,
	"read_skill": true, "notebook_list": true, "notebook_read": true,
	"read_thread": true, "recall": true,
	"lsp_definition": true, "lsp_diagnostics": true, "lsp_document_symbols": true,
	"lsp_hover": true, "lsp_references": true, "lsp_workspace_symbols": true,
}

// ClassifyExecutionSummary maps one persisted execution summary to its deterministic three-valued
// causal MutationEvidence without consulting live tool configuration (ADR-0024 §4.6.5).
func ClassifyExecutionSummary(rec session.AnsweredExecutionSummary) MutationEvidence {
	return classifyExecutionSummary(rec)
}

func classifyExecutionSummary(rec session.AnsweredExecutionSummary) MutationEvidence {
	switch rec.ToolName {
	case "write_file", "replace_in_file", "apply_patch", "edit_lines":
		snapRef, seal := splitFilesSnapshotPostRef(rec.PostRef)
		if rec.Outcome == session.OutcomeComplete &&
			!rec.PreUnavailable &&
			hasPositiveFilesSnapshotRef(rec.PreRef) &&
			hasPositiveFilesSnapshotRef(snapRef) &&
			rec.Coverage == session.CoverageComplete {
			if seal == string(MutationProven) && rec.PreRef != snapRef {
				return MutationProven
			}
			if seal == string(MutationNone) && rec.PreRef == snapRef && rec.FileEdgeCount == 0 {
				return MutationNone
			}
		}
		return MutationUnknown
	}
	if immutableNonMutatingAdmittedTools[rec.ToolName] {
		if rec.FileEdgeCount == 0 && rec.Outcome != session.OutcomeUnknown && rec.Outcome != session.OutcomeAdmitted {
			return MutationNone
		}
		return MutationUnknown
	}
	return MutationUnknown
}

// AggregateGroupMutation combines the tool rows and execution summaries of one answered_seq group
// under the strict lattice Unknown > Proven > None (ADR-0024 §4.6.5).
func AggregateGroupMutation(assistantMsg protocol.Message, toolRows []protocol.Message, execs []session.AnsweredExecutionSummary) MutationEvidence {
	return aggregateGroupMutation(assistantMsg, toolRows, execs)
}

func aggregateGroupMutation(assistantMsg protocol.Message, toolRows []protocol.Message, execs []session.AnsweredExecutionSummary) MutationEvidence {
	if len(toolRows) < len(assistantMsg.ToolCalls) {
		return MutationUnknown
	}
	for _, tr := range toolRows {
		if tr.Origin == protocol.OriginToolInterrupted {
			return MutationUnknown
		}
	}
	hasProven := false
	for _, rec := range execs {
		switch classifyExecutionSummary(rec) {
		case MutationUnknown:
			return MutationUnknown
		case MutationProven:
			hasProven = true
		}
	}
	if hasProven {
		return MutationProven
	}
	return MutationNone
}

// DeriveRepairContext computes the forward-monotonic completion-gate repair floor from transcript and
// mutations (ADR-0024 §4.6.6).
func DeriveRepairContext(transcript []window.Entry, mutations map[int64]MutationEvidence) RepairContext {
	return deriveRepairContext(transcript, mutations)
}

func deriveRepairContext(transcript []window.Entry, mutations map[int64]MutationEvidence) repairContext {
	inactive := repairContext{BlockSeq: -1, FloorIdx: -1, MinRequiredSeq: -1}
	groups, err := window.Groups(transcript)
	if err != nil || len(groups) == 0 {
		return inactive
	}
	u := -1
	for k := len(groups) - 1; k >= 0; k-- {
		m := groups[k][0].Msg
		if m.Role == "user" && (m.Origin == "" || m.Origin == protocol.OriginImported) {
			u = k
			break
		}
	}
	activeBlock := -1
	activeProposal := -1
	preBlockFloor := -1
	postBlockProven := false
	firstMutSincePrevBlock := -1
	lastToolBeforeFirstBlock := -1

	for i := u + 1; i < len(groups); i++ {
		g := groups[i]
		head := g[0].Msg
		if head.Role == "assistant" && len(head.ToolCalls) > 0 {
			ev, ok := mutations[g[0].Seq]
			if !ok {
				var toolRows []protocol.Message
				for _, te := range g[1:] {
					toolRows = append(toolRows, te.Msg)
				}
				ev = aggregateGroupMutation(head, toolRows, nil)
			}
			if activeBlock == -1 {
				lastToolBeforeFirstBlock = i
				if (ev == MutationProven || ev == MutationUnknown) && preBlockFloor == -1 {
					preBlockFloor = i
				}
			} else {
				if (ev == MutationProven || ev == MutationUnknown) && firstMutSincePrevBlock == -1 {
					firstMutSincePrevBlock = i
				}
				if ev == MutationProven {
					postBlockProven = true
				}
			}
			continue
		}
		if head.Role == "user" && head.Origin == protocol.OriginCompletionBlocked {
			pIdx := i
			for p := i - 1; p > u; p-- {
				if groups[p][0].Msg.Role == "assistant" {
					pIdx = p
					break
				}
			}
			if activeBlock == -1 {
				if preBlockFloor == -1 {
					switch {
					case lastToolBeforeFirstBlock >= 0:
						preBlockFloor = lastToolBeforeFirstBlock
					case u >= 0:
						preBlockFloor = u
					default:
						preBlockFloor = pIdx
					}
				}
			} else if postBlockProven && firstMutSincePrevBlock >= 0 {
				preBlockFloor = firstMutSincePrevBlock
			}
			activeBlock = i
			activeProposal = pIdx
			postBlockProven = false
			firstMutSincePrevBlock = -1
		}
	}
	if activeBlock == -1 {
		return inactive
	}
	hasAssistantAfterBlock := false
	for k := activeBlock + 1; k < len(groups); k++ {
		if groups[k][0].Msg.Role == "assistant" {
			hasAssistantAfterBlock = true
			break
		}
	}
	floorIdx := activeBlock
	if !postBlockProven {
		floorIdx = min(min(preBlockFloor, activeProposal), activeBlock)
		if floorIdx < 0 {
			floorIdx = activeBlock
		}
	}
	return repairContext{
		Active:         true,
		NewestIsBlock:  !hasAssistantAfterBlock,
		BlockSeq:       groups[activeBlock][0].Seq,
		FloorIdx:       floorIdx,
		MinRequiredSeq: groups[floorIdx][0].Seq,
	}
}

// budget is the window budget of one request to c: its context, its output reservation, and its
// margin, raised to keep the transcript within a capacity retry's limit for c once one was taken.
func (r *run) budget(c Candidate, maxTokens, fixed int) window.Budget {
	b := window.Budget{Context: c.Context, Output: OutputBudget(maxTokens, c), Margin: Margin(c.Context)}
	if limit, ok := r.transcriptCap[c.Ref]; ok {
		b.Margin = max(b.Margin, b.Context-b.Output-fixed-limit)
	}
	return b
}

// capTranscript keeps the transcript of later requests to c within the budget a capacity retry chose.
func (r *run) capTranscript(ref config.ModelRef, retry window.Budget, fixed int) {
	if r.transcriptCap == nil {
		r.transcriptCap = map[config.ModelRef]int{}
	}
	r.transcriptCap[ref] = retry.Context - retry.Output - retry.Margin - fixed
}

// maxContext is the largest context of the run's models: no window holds more transcript tokens.
func (l *Loop) maxContext() int {
	largest := 0
	for _, m := range l.Models {
		largest = max(largest, m.Context)
	}
	return largest
}
