package agent

import (
	"context"
	"slices"
	"strings"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/window"
	"github.com/edynasty/LumenCortex/protocol"
)

// MaxInMemoryHistoryGroups bounds the body-free HistoryGroupMeta slice kept in memory per active
// external user turn (ADR-0024 §4.6.6, F-02, R2-04). When exceeded, settled oldest groups are folded
// incrementally into baseSM in batches of historyFoldBatchGroups so deriveRepairContext runs in O(1)
// amortized memory and zero per-step SQLite scans (with StreamRepairContextFromStore retained as a
// stateless SQLite streaming fallback/verifier).
const (
	MaxInMemoryHistoryGroups = 2048
	historyFoldBatchGroups   = MaxInMemoryHistoryGroups / 2
)

// HistoryToolCallRef records body-free attribution for one tool call within an answered_seq group.
type HistoryToolCallRef struct {
	ToolCallID  string
	Name        string
	AnsweredSeq int64
	ExecutionID string
}

// HistoryGroupMeta holds lightweight body-free metadata for one atomic transcript group in the active
// external user turn (ADR-0024 §4.2, §4.6.6, F-02). No message Content is retained here.
type HistoryGroupMeta struct {
	Seq              int64
	LastSeq          int64
	Role             string
	Origin           string
	HasToolCalls     bool
	ExpectedCalls    int
	PairedResults    int
	HasInterrupted   bool
	HasImportedTool  bool
	HasDebugToolCall bool
	AnsweredSeq      int64
	ToolCallID       string
	PrimaryToolName  string
	ExecutionID      string
	PrimaryPostRef   string
	GateFeedback     string
	ToolCalls        []HistoryToolCallRef
	MutationEvidence MutationEvidence
}

// runTranscript is the source of a run's request windows (design §5) and its run memory (§4.6).
//
// transcript holds only the bounded tail of rows (within maxContext permanent message tokens) from
// TaskState.windowStartSeq on, oldest first, with their seqs. Leading groups that no window can
// include are dropped from memory; trimmed counts them, so dropped-group counts still cover everything
// from windowStartSeq.
//
// historyGroups retains body-free HistoryGroupMeta for the active external user turn (bounded by
// MaxInMemoryHistoryGroups, falling back to SQLite streaming when historyOverflowed is true) so live
// runs and cold Resumes compute bit-identical repair floors without retaining unbounded message bodies.
//
// transcriptCap is the transcript budget a capacity retry left for a model; it holds until the run
// ends (design §5).
//
// turn is the run memory of the current turn: for each assistant row this run produced after the
// last user row, the model that produced it and the echo the adapter returned with it. It is never
// written anywhere and goes when the run returns. A user row ends the turn and empties it, so it holds
// at most one turn of the transcript in memory; the window counts an entry's echo toward its row
// (Entry.EchoTokens) only while the entry is in it.
type runTranscript struct {
	transcript        []window.Entry
	trimmed           int
	transcriptCap     map[config.ModelRef]int
	turn              map[int64]turnEntry
	mutations         map[int64]MutationEvidence
	historyGroups     []HistoryGroupMeta
	historyOverflowed bool
	baseSM            repairStateMachine
	hasBaseSM         bool
	loadExecsBySeq    map[int64][]session.AnsweredExecutionSummary
	latestUserSeq     int64
	latestUserSet     bool
}

type turnEntry struct {
	model config.ModelRef
	echo  protocol.Echo
}

// load streams the transcript from the run's windowStartSeq, trims full-body entries to maxContext on
// the fly so message bodies never grow unboundedly in RAM, and populates causal MutationEvidence on
// the active turn's HistoryGroupMeta (including settled prefixes folded into baseSM during load).
// The rows at withheld seqs (sorted) are read with their content withheld: they carry content of a
// revoked source (window.Withhold).
func (r *run) load(ctx context.Context, store Store, sessionID string, withheld []int64, maxContext int) error {
	summaries, err := store.ListAnsweredExecutions(ctx, sessionID, r.ts.WindowStartSeq)
	if err != nil {
		return err
	}
	bySeq := make(map[int64][]session.AnsweredExecutionSummary, len(summaries))
	for _, s := range summaries {
		bySeq[s.AnsweredSeq] = append(bySeq[s.AnsweredSeq], s)
	}
	r.loadExecsBySeq = bySeq
	defer func() { r.loadExecsBySeq = nil }()

	rowsSinceTrim := 0
	if err := store.Transcript(ctx, sessionID, r.ts.WindowStartSeq, func(seq int64, m protocol.Message) error {
		if _, found := slices.BinarySearch(withheld, seq); found {
			m = window.Withhold(m)
		}
		r.appendRow(seq, m)
		rowsSinceTrim++
		if maxContext > 0 && rowsSinceTrim >= 128 && !r.hasOpenTranscriptToolGroup() {
			rowsSinceTrim = 0
			if _, err := r.groups(maxContext); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if maxContext > 0 && !r.hasOpenTranscriptToolGroup() {
		if _, err := r.groups(maxContext); err != nil {
			return err
		}
	}
	r.populateMutations(r.transcript, summaries)
	r.populateHistoryMutations(summaries)
	return nil
}

func (r *run) hasOpenTranscriptToolGroup() bool {
	for i := len(r.transcript) - 1; i >= 0; i-- {
		m := r.transcript[i].Msg
		if m.Role == "tool" {
			continue
		}
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			answered := len(r.transcript) - 1 - i
			return answered < len(m.ToolCalls)
		}
		return false
	}
	return false
}

// refreshGroupMutation recomputes causal MutationEvidence for answeredSeq from persisted execution
// summaries across both HistoryGroupMeta and any retained full-body transcript entry.
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
	for i := len(r.historyGroups) - 1; i >= 0; i-- {
		if r.historyGroups[i].HasToolCalls && r.historyGroups[i].AnsweredSeq == answeredSeq {
			applyExecutionSummariesToMeta(&r.historyGroups[i], matching)
			break
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

func (r *run) populateHistoryMutations(summaries []session.AnsweredExecutionSummary) {
	if len(r.historyGroups) == 0 {
		return
	}
	bySeq := make(map[int64][]session.AnsweredExecutionSummary, len(summaries))
	for _, s := range summaries {
		bySeq[s.AnsweredSeq] = append(bySeq[s.AnsweredSeq], s)
	}
	for i := range r.historyGroups {
		if r.historyGroups[i].HasToolCalls {
			applyExecutionSummariesToMeta(&r.historyGroups[i], bySeq[r.historyGroups[i].AnsweredSeq])
		}
	}
}

func buildAssistantHistoryMeta(seq int64, m protocol.Message) HistoryGroupMeta {
	meta := HistoryGroupMeta{
		Seq:              seq,
		LastSeq:          seq,
		Role:             "assistant",
		Origin:           m.Origin,
		AnsweredSeq:      -1,
		MutationEvidence: MutationNone,
	}
	if len(m.ToolCalls) == 0 {
		return meta
	}
	calls := make([]HistoryToolCallRef, len(m.ToolCalls))
	hasDebug := false
	for i, tc := range m.ToolCalls {
		calls[i] = HistoryToolCallRef{
			ToolCallID:  tc.ID,
			Name:        tc.Name,
			AnsweredSeq: seq,
		}
		if strings.HasPrefix(tc.Name, "debug_") {
			hasDebug = true
		}
	}
	meta.HasToolCalls = true
	meta.ExpectedCalls = len(m.ToolCalls)
	meta.HasDebugToolCall = hasDebug
	meta.AnsweredSeq = seq
	meta.ToolCallID = m.ToolCalls[0].ID
	meta.PrimaryToolName = m.ToolCalls[0].Name
	meta.ToolCalls = calls
	meta.MutationEvidence = MutationUnknown
	return meta
}

func applyToolRowToHistoryMeta(meta *HistoryGroupMeta, seq int64, m protocol.Message) bool {
	if meta == nil || meta.Role != "assistant" || !meta.HasToolCalls {
		return false
	}
	matched := false
	for i := range meta.ToolCalls {
		if meta.ToolCalls[i].ToolCallID == m.ToolCallID {
			matched = true
			break
		}
	}
	if !matched {
		return false
	}
	if seq > meta.LastSeq {
		meta.LastSeq = seq
	}
	meta.PairedResults++
	if m.Origin == protocol.OriginToolInterrupted {
		meta.HasInterrupted = true
	}
	if m.Origin == protocol.OriginImported {
		meta.HasImportedTool = true
	}
	meta.MutationEvidence = aggregateHistoryGroupMutation(*meta, nil)
	return true
}

func applyExecutionSummariesToMeta(meta *HistoryGroupMeta, execs []session.AnsweredExecutionSummary) {
	if meta == nil || !meta.HasToolCalls {
		return
	}
	meta.ExecutionID = ""
	meta.PrimaryPostRef = ""
	for i := range meta.ToolCalls {
		meta.ToolCalls[i].ExecutionID = ""
	}
	if len(execs) > 0 {
		meta.ExecutionID = execs[0].ExecutionID
		meta.PrimaryPostRef = execs[0].PostRef
		if meta.PrimaryToolName == "" {
			meta.PrimaryToolName = execs[0].ToolName
		}
		used := make([]bool, len(meta.ToolCalls))
		for _, ex := range execs {
			for i := range meta.ToolCalls {
				if !used[i] && meta.ToolCalls[i].Name == ex.ToolName {
					meta.ToolCalls[i].ExecutionID = ex.ExecutionID
					used[i] = true
					break
				}
			}
		}
	}
	meta.MutationEvidence = aggregateHistoryGroupMutation(*meta, execs)
}

func (r *run) foldSettledHistoryPrefix() {
	foldN := min(historyFoldBatchGroups, len(r.historyGroups))
	if foldN <= 0 {
		return
	}
	if !r.hasBaseSM {
		r.baseSM = newRepairStateMachine()
		r.hasBaseSM = true
	}
	for i := 0; i < foldN; i++ {
		if r.loadExecsBySeq != nil && r.historyGroups[i].HasToolCalls {
			applyExecutionSummariesToMeta(&r.historyGroups[i], r.loadExecsBySeq[r.historyGroups[i].AnsweredSeq])
		}
		r.baseSM.step(r.historyGroups[i])
	}
	n := copy(r.historyGroups, r.historyGroups[foldN:])
	clear(r.historyGroups[n:])
	r.historyGroups = r.historyGroups[:n]
	r.historyOverflowed = true
}

func (r *run) pushHistoryGroup(meta HistoryGroupMeta) {
	if len(r.historyGroups) >= MaxInMemoryHistoryGroups {
		r.foldSettledHistoryPrefix()
	}
	r.historyGroups = append(r.historyGroups, meta)
}

func (r *run) recordHistoryRow(seq int64, m protocol.Message) {
	switch m.Role {
	case "user":
		if m.Origin == "" || m.Origin == protocol.OriginImported {
			r.latestUserSeq = seq
			r.latestUserSet = true
			clear(r.historyGroups)
			r.historyGroups = r.historyGroups[:0]
			r.historyOverflowed = false
			r.baseSM = newRepairStateMachine()
			r.hasBaseSM = false
		}
		gateFeedback := ""
		if m.Origin == protocol.OriginCompletionBlocked {
			gateFeedback = m.Content
		}
		r.pushHistoryGroup(HistoryGroupMeta{
			Seq:              seq,
			LastSeq:          seq,
			Role:             "user",
			Origin:           m.Origin,
			AnsweredSeq:      -1,
			GateFeedback:     gateFeedback,
			MutationEvidence: MutationNone,
		})
	case "assistant":
		r.pushHistoryGroup(buildAssistantHistoryMeta(seq, m))
	case "tool":
		if len(r.historyGroups) == 0 {
			return
		}
		applyToolRowToHistoryMeta(&r.historyGroups[len(r.historyGroups)-1], seq, m)
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
	r.recordHistoryRow(seq, m)
}

// appendAssistant records an assistant row this run received from model, with its echo.
func (r *run) appendAssistant(seq int64, m protocol.Message, model config.ModelRef, echo protocol.Echo) {
	r.transcript = append(r.transcript, window.Entry{Seq: seq, Msg: m, EchoTokens: echo.Tokens()})
	if r.turn == nil {
		r.turn = map[int64]turnEntry{}
	}
	r.turn[seq] = turnEntry{model: model, echo: echo}
	r.recordHistoryRow(seq, m)
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

func groupPermanentTokens(g []window.Entry) int {
	cost := 0
	for _, e := range g {
		cost += window.Estimate(e.Msg)
	}
	return cost
}

// groups splits the transcript into window groups after dropping leading full-body groups whose
// permanent message token suffix exceeds maxContext. Repair floor tracking lives in body-free
// HistoryGroupMeta (F-02), so message bodies are never retained past maxContext.
func (r *run) groups(maxContext int) ([][]window.Entry, error) {
	groups, err := window.Groups(r.transcript)
	if err != nil {
		return nil, err
	}
	if maxContext <= 0 {
		return groups, nil
	}
	keep, used := 0, 0
	for k := len(groups) - 1; k >= 0; k-- {
		used += groupPermanentTokens(groups[k])
		if used > maxContext {
			keep = k
			break
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

// AggregateHistoryGroupMutation computes the three-valued causal MutationEvidence of one body-free
// HistoryGroupMeta and its persisted execution summaries under Unknown > Proven > None.
func AggregateHistoryGroupMutation(meta HistoryGroupMeta, execs []session.AnsweredExecutionSummary) MutationEvidence {
	return aggregateHistoryGroupMutation(meta, execs)
}

func aggregateHistoryGroupMutation(meta HistoryGroupMeta, execs []session.AnsweredExecutionSummary) MutationEvidence {
	if !meta.HasToolCalls {
		return MutationNone
	}
	if meta.PairedResults < meta.ExpectedCalls {
		return MutationUnknown
	}
	if meta.Origin == protocol.OriginImported || meta.HasInterrupted || meta.HasImportedTool || meta.HasDebugToolCall {
		return MutationUnknown
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

// AggregateGroupMutation combines the tool rows and execution summaries of one answered_seq group
// under the strict lattice Unknown > Proven > None (ADR-0024 §4.6.5).
func AggregateGroupMutation(assistantMsg protocol.Message, toolRows []protocol.Message, execs []session.AnsweredExecutionSummary) MutationEvidence {
	return aggregateGroupMutation(assistantMsg, toolRows, execs)
}

func aggregateGroupMutation(assistantMsg protocol.Message, toolRows []protocol.Message, execs []session.AnsweredExecutionSummary) MutationEvidence {
	if len(toolRows) < len(assistantMsg.ToolCalls) {
		return MutationUnknown
	}
	if assistantMsg.Origin == protocol.OriginImported {
		return MutationUnknown
	}
	for _, tc := range assistantMsg.ToolCalls {
		if strings.HasPrefix(tc.Name, "debug_") {
			return MutationUnknown
		}
	}
	for _, tr := range toolRows {
		if tr.Origin == protocol.OriginToolInterrupted || tr.Origin == protocol.OriginImported {
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

func causalRefFromMeta(g HistoryGroupMeta) CausalEvidenceRef {
	seq := g.AnsweredSeq
	if seq < 0 {
		seq = g.Seq
	}
	return CausalEvidenceRef{
		AnsweredSeq:      seq,
		ToolCallID:       g.ToolCallID,
		ToolName:         g.PrimaryToolName,
		ExecutionID:      g.ExecutionID,
		PostRef:          g.PrimaryPostRef,
		MutationEvidence: g.MutationEvidence,
	}
}

// repairStateMachine executes the forward-monotonic completion-gate repair automaton over a stream
// or slice of body-free HistoryGroupMeta entries (ADR-0024 §4.6.6, §7, F-02, R2-02).
// Value-only fields make copying repairStateMachine O(1) and completely free of slice/pointer aliasing.
type repairStateMachine struct {
	idx                      int
	userIdx                  int
	userSeq                  int64
	blockCount               int
	activeBlockIdx           int
	activeBlockSeq           int64
	activeGateFeedback       string
	activeProposalIdx        int
	activeProposalSeq        int64
	preBlockFloorIdx         int
	preBlockFloorSeq         int64
	hasPreBlockFloorRef      bool
	preBlockFloorRef         CausalEvidenceRef
	postBlockProven          bool
	postBlockProvenCount     int
	postBlockUnknownCount    int
	lastMutationWasUnknown   bool
	firstMutSincePrevIdx     int
	firstMutSincePrevSeq     int64
	hasFirstMutSincePrevRef  bool
	firstMutSincePrevRef     CausalEvidenceRef
	lastToolBeforeFirstIdx   int
	lastToolBeforeFirstSeq   int64
	hasLastToolBeforeFirst   bool
	lastToolBeforeFirstRef   CausalEvidenceRef
	lastAssistantIdx         int
	lastAssistantSeq         int64
	hasAssistantAfterBlocked bool
	causalProgressLen        int
	causalProgress           [MaxCausalProgressRefs]CausalEvidenceRef
}

func newRepairStateMachine() repairStateMachine {
	return repairStateMachine{
		userIdx:                -1,
		userSeq:                -1,
		activeBlockIdx:         -1,
		activeBlockSeq:         -1,
		activeProposalIdx:      -1,
		activeProposalSeq:      -1,
		preBlockFloorIdx:       -1,
		preBlockFloorSeq:       -1,
		firstMutSincePrevIdx:   -1,
		firstMutSincePrevSeq:   -1,
		lastToolBeforeFirstIdx: -1,
		lastToolBeforeFirstSeq: -1,
		lastAssistantIdx:       -1,
		lastAssistantSeq:       -1,
	}
}

func (sm *repairStateMachine) resetForExternalUser(idx int, seq int64) {
	*sm = newRepairStateMachine()
	sm.idx = idx + 1
	sm.userIdx = idx
	sm.userSeq = seq
}

func (sm *repairStateMachine) appendCausalProgress(ref CausalEvidenceRef) {
	if sm.causalProgressLen < MaxCausalProgressRefs {
		sm.causalProgress[sm.causalProgressLen] = ref
		sm.causalProgressLen++
		return
	}
	copy(sm.causalProgress[:], sm.causalProgress[1:])
	sm.causalProgress[MaxCausalProgressRefs-1] = ref
}

func (sm *repairStateMachine) step(g HistoryGroupMeta) {
	i := sm.idx
	if g.Role == "user" && (g.Origin == "" || g.Origin == protocol.OriginImported) {
		sm.resetForExternalUser(i, g.Seq)
		return
	}
	sm.idx++

	if g.Role == "assistant" {
		if sm.activeBlockIdx >= 0 {
			sm.hasAssistantAfterBlocked = true
		}
		if g.HasToolCalls {
			ev := g.MutationEvidence
			ref := causalRefFromMeta(g)
			if sm.activeBlockIdx == -1 {
				sm.lastToolBeforeFirstIdx = i
				sm.lastToolBeforeFirstSeq = g.Seq
				sm.hasLastToolBeforeFirst = true
				sm.lastToolBeforeFirstRef = ref
				if (ev == MutationProven || ev == MutationUnknown) && sm.preBlockFloorIdx == -1 {
					sm.preBlockFloorIdx = i
					sm.preBlockFloorSeq = g.Seq
					sm.hasPreBlockFloorRef = true
					sm.preBlockFloorRef = ref
				}
			} else if ev == MutationProven || ev == MutationUnknown {
				if sm.firstMutSincePrevIdx == -1 {
					sm.firstMutSincePrevIdx = i
					sm.firstMutSincePrevSeq = g.Seq
					sm.hasFirstMutSincePrevRef = true
					sm.firstMutSincePrevRef = ref
				}
				if ev == MutationProven {
					sm.postBlockProven = true
					sm.postBlockProvenCount++
					sm.lastMutationWasUnknown = false
				} else {
					sm.postBlockUnknownCount++
					sm.lastMutationWasUnknown = true
				}
				sm.appendCausalProgress(ref)
			}
			sm.lastAssistantIdx = i
			sm.lastAssistantSeq = g.Seq
			return
		}
		sm.lastAssistantIdx = i
		sm.lastAssistantSeq = g.Seq
		return
	}

	if g.Role == "user" && g.Origin == protocol.OriginCompletionBlocked {
		pIdx := i
		pSeq := g.Seq
		if sm.lastAssistantIdx > sm.userIdx {
			pIdx = sm.lastAssistantIdx
			pSeq = sm.lastAssistantSeq
		}
		if sm.activeBlockIdx == -1 {
			if sm.preBlockFloorIdx == -1 {
				switch {
				case sm.lastToolBeforeFirstIdx >= 0:
					sm.preBlockFloorIdx = sm.lastToolBeforeFirstIdx
					sm.preBlockFloorSeq = sm.lastToolBeforeFirstSeq
					sm.hasPreBlockFloorRef = sm.hasLastToolBeforeFirst
					sm.preBlockFloorRef = sm.lastToolBeforeFirstRef
				case sm.userIdx >= 0:
					sm.preBlockFloorIdx = sm.userIdx
					sm.preBlockFloorSeq = sm.userSeq
					sm.hasPreBlockFloorRef = false
					sm.preBlockFloorRef = CausalEvidenceRef{}
				default:
					sm.preBlockFloorIdx = pIdx
					sm.preBlockFloorSeq = pSeq
					sm.hasPreBlockFloorRef = false
					sm.preBlockFloorRef = CausalEvidenceRef{}
				}
			}
		} else if sm.postBlockProven && sm.firstMutSincePrevIdx >= 0 {
			sm.preBlockFloorIdx = sm.firstMutSincePrevIdx
			sm.preBlockFloorSeq = sm.firstMutSincePrevSeq
			sm.hasPreBlockFloorRef = sm.hasFirstMutSincePrevRef
			sm.preBlockFloorRef = sm.firstMutSincePrevRef
		}
		sm.blockCount++
		sm.activeBlockIdx = i
		sm.activeBlockSeq = g.Seq
		sm.activeGateFeedback = g.GateFeedback
		sm.activeProposalIdx = pIdx
		sm.activeProposalSeq = pSeq
		sm.postBlockProven = false
		sm.postBlockProvenCount = 0
		sm.postBlockUnknownCount = 0
		sm.lastMutationWasUnknown = false
		sm.firstMutSincePrevIdx = -1
		sm.firstMutSincePrevSeq = -1
		sm.hasFirstMutSincePrevRef = false
		sm.firstMutSincePrevRef = CausalEvidenceRef{}
		sm.causalProgressLen = 0
		sm.causalProgress = [MaxCausalProgressRefs]CausalEvidenceRef{}
		sm.hasAssistantAfterBlocked = false
	}
}

func (sm *repairStateMachine) finish() repairContext {
	if sm.activeBlockIdx == -1 {
		return repairContext{
			BlockSeq:                -1,
			ProposalSeq:             -1,
			FloorIdx:                -1,
			MinRequiredSeq:          -1,
			OriginalMinRequiredSeq:  -1,
			PostBlockCausalFloorSeq: -1,
		}
	}
	origFloorSeq := sm.activeBlockSeq
	if sm.preBlockFloorSeq >= 0 && sm.preBlockFloorSeq < origFloorSeq {
		origFloorSeq = sm.preBlockFloorSeq
	}
	if sm.activeProposalSeq >= 0 && sm.activeProposalSeq < origFloorSeq {
		origFloorSeq = sm.activeProposalSeq
	}

	floorIdx := sm.activeBlockIdx
	floorSeq := sm.activeBlockSeq
	if !sm.postBlockProven {
		if sm.preBlockFloorIdx >= 0 && sm.preBlockFloorIdx < floorIdx {
			floorIdx = sm.preBlockFloorIdx
			floorSeq = sm.preBlockFloorSeq
		}
		if sm.activeProposalIdx >= 0 && sm.activeProposalIdx < floorIdx {
			floorIdx = sm.activeProposalIdx
			floorSeq = sm.activeProposalSeq
		}
	}

	stage := RepairStageInspecting
	if !sm.hasAssistantAfterBlocked {
		stage = RepairStageBlocked
	} else if sm.postBlockUnknownCount > 0 && (sm.postBlockProvenCount == 0 || sm.lastMutationWasUnknown) {
		stage = RepairStageUnknownMutated
	} else if sm.postBlockProvenCount > 0 {
		stage = RepairStageProvenProgress
	}

	return repairContext{
		Active:                  true,
		NewestIsBlock:           !sm.hasAssistantAfterBlocked,
		BlockSeq:                sm.activeBlockSeq,
		ProposalSeq:             sm.activeProposalSeq,
		BlockCount:              sm.blockCount,
		GateFeedback:            sm.activeGateFeedback,
		FloorIdx:                floorIdx,
		MinRequiredSeq:          floorSeq,
		OriginalMinRequiredSeq:  origFloorSeq,
		PostBlockCausalFloorSeq: sm.firstMutSincePrevSeq,
		Stage:                   stage,
		HasOriginalEvidence:     sm.hasPreBlockFloorRef,
		OriginalEvidence:        sm.preBlockFloorRef,
		PostBlockProvenCount:    sm.postBlockProvenCount,
		PostBlockUnknownCount:   sm.postBlockUnknownCount,
		CausalProgressLen:       sm.causalProgressLen,
		CausalProgress:          sm.causalProgress,
	}
}

// DeriveRepairContextFromHistory computes the deterministic completion-gate repair floor from a slice
// of body-free HistoryGroupMeta entries (ADR-0024 §4.6.6, F-02).
func DeriveRepairContextFromHistory(groups []HistoryGroupMeta) RepairContext {
	sm := newRepairStateMachine()
	for _, g := range groups {
		sm.step(g)
	}
	return sm.finish()
}

// HistoryGroupsFromEntries converts window entries and an optional mutations map into body-free
// HistoryGroupMeta records for pure unit tests and static callers.
func HistoryGroupsFromEntries(transcript []window.Entry, mutations map[int64]MutationEvidence) ([]HistoryGroupMeta, bool) {
	groups, err := window.Groups(transcript)
	if err != nil {
		return nil, false
	}
	out := make([]HistoryGroupMeta, 0, len(groups))
	for _, g := range groups {
		head := g[0].Msg
		switch head.Role {
		case "user":
			gateFeedback := ""
			if head.Origin == protocol.OriginCompletionBlocked {
				gateFeedback = head.Content
			}
			out = append(out, HistoryGroupMeta{
				Seq:              g[0].Seq,
				LastSeq:          g[len(g)-1].Seq,
				Role:             "user",
				Origin:           head.Origin,
				AnsweredSeq:      -1,
				GateFeedback:     gateFeedback,
				MutationEvidence: MutationNone,
			})
		case "assistant":
			meta := buildAssistantHistoryMeta(g[0].Seq, head)
			for _, te := range g[1:] {
				applyToolRowToHistoryMeta(&meta, te.Seq, te.Msg)
			}
			if mutations != nil {
				if ev, ok := mutations[g[0].Seq]; ok {
					meta.MutationEvidence = ev
				}
			}
			out = append(out, meta)
		}
	}
	return out, true
}

// DeriveRepairContext computes the forward-monotonic completion-gate repair floor from transcript and
// mutations using the shared HistoryGroupMeta state machine (ADR-0024 §4.6.6).
func DeriveRepairContext(transcript []window.Entry, mutations map[int64]MutationEvidence) RepairContext {
	return deriveRepairContext(transcript, mutations)
}

func deriveRepairContext(transcript []window.Entry, mutations map[int64]MutationEvidence) repairContext {
	meta, ok := HistoryGroupsFromEntries(transcript, mutations)
	if !ok || len(meta) == 0 {
		return repairContext{
			BlockSeq:                -1,
			ProposalSeq:             -1,
			FloorIdx:                -1,
			MinRequiredSeq:          -1,
			OriginalMinRequiredSeq:  -1,
			PostBlockCausalFloorSeq: -1,
		}
	}
	return DeriveRepairContextFromHistory(meta)
}

// deriveRepairContext resolves the run's current repairContext in O(1) bounded memory and zero
// per-step SQLite scans by continuing from baseSM (when older prefixes were folded) across the
// bounded in-memory HistoryGroupMeta tail (ADR-0024 §4.6.6, F-02, R2-04). If historyOverflowed was
// set externally without baseSM or in-memory groups, StreamRepairContextFromStore reconstructs
// deterministically from SQLite.
func (r *run) deriveRepairContext(ctx context.Context, store Store, sessionID string) (repairContext, error) {
	if r.hasBaseSM {
		sm := r.baseSM
		for _, g := range r.historyGroups {
			sm.step(g)
		}
		return sm.finish(), nil
	}
	if !r.historyOverflowed {
		if len(r.historyGroups) == 0 && len(r.transcript) > 0 {
			return deriveRepairContext(r.transcript, r.mutations), nil
		}
		return DeriveRepairContextFromHistory(r.historyGroups), nil
	}
	if store == nil {
		return deriveRepairContext(r.transcript, r.mutations), nil
	}
	fromSeq := r.ts.WindowStartSeq
	if r.latestUserSet && r.latestUserSeq > fromSeq {
		fromSeq = r.latestUserSeq
	}
	return StreamRepairContextFromStore(ctx, store, sessionID, fromSeq)
}

// StreamRepairContextFromStore reconstructs RepairContext directly from SQLite via Store.Transcript
// (EachMessageFrom) + ListAnsweredExecutions without retaining message bodies in memory.
func StreamRepairContextFromStore(ctx context.Context, store Store, sessionID string, fromSeq int64) (RepairContext, error) {
	summaries, err := store.ListAnsweredExecutions(ctx, sessionID, fromSeq)
	if err != nil {
		return repairContext{BlockSeq: -1, FloorIdx: -1, MinRequiredSeq: -1}, err
	}
	bySeq := make(map[int64][]session.AnsweredExecutionSummary, len(summaries))
	for _, s := range summaries {
		bySeq[s.AnsweredSeq] = append(bySeq[s.AnsweredSeq], s)
	}

	sm := newRepairStateMachine()
	var cur *HistoryGroupMeta
	flushCur := func() {
		if cur == nil {
			return
		}
		if cur.HasToolCalls {
			applyExecutionSummariesToMeta(cur, bySeq[cur.AnsweredSeq])
		}
		sm.step(*cur)
		cur = nil
	}

	if err := store.Transcript(ctx, sessionID, fromSeq, func(seq int64, m protocol.Message) error {
		switch m.Role {
		case "user":
			flushCur()
			gateFeedback := ""
			if m.Origin == protocol.OriginCompletionBlocked {
				gateFeedback = m.Content
			}
			meta := HistoryGroupMeta{
				Seq:              seq,
				LastSeq:          seq,
				Role:             "user",
				Origin:           m.Origin,
				AnsweredSeq:      -1,
				GateFeedback:     gateFeedback,
				MutationEvidence: MutationNone,
			}
			sm.step(meta)
		case "assistant":
			flushCur()
			meta := buildAssistantHistoryMeta(seq, m)
			cur = &meta
		case "tool":
			if cur != nil {
				applyToolRowToHistoryMeta(cur, seq, m)
			}
		}
		return nil
	}); err != nil {
		return repairContext{BlockSeq: -1, FloorIdx: -1, MinRequiredSeq: -1}, err
	}
	flushCur()
	return sm.finish(), nil
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
