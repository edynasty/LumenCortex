package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/edynasty/LumenCortex/internal/diag"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/taskstate"
	"github.com/edynasty/LumenCortex/internal/threadctx"
	"github.com/edynasty/LumenCortex/protocol"
)

const (
	MaxContextNodes   = 128
	MaxContextEvents  = 64
	MaxAttentionItems = 8
	MaxAttentionBytes = 4096
)

// ContextNode is a reference to existing state, not a second fact store. Validation
// describes the source check; even record_checked does not mean semantic truth.
type ContextNode struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Summary    string `json:"summary,omitempty"`
	SourceRef  string `json:"sourceRef"`
	EventID    string `json:"eventId,omitempty"`
	Status     string `json:"status"`
	Validation string `json:"validation"`
	Freshness  string `json:"freshness"`
}
type ContextLink struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
}
type AttentionItem struct {
	NodeID     string `json:"nodeId"`
	Reason     string `json:"reason"`
	Summary    string `json:"summary"`
	SourceRef  string `json:"sourceRef"`
	Validation string `json:"validation"`
	Freshness  string `json:"freshness"`
}
type ContextGraphView struct {
	SessionID          string          `json:"sessionId"`
	StateVersion       int64           `json:"stateVersion"`
	CapturedAt         time.Time       `json:"capturedAt"`
	Nodes              []ContextNode   `json:"nodes"`
	Links              []ContextLink   `json:"links"`
	Attention          []AttentionItem `json:"attention"`
	Truncated          bool            `json:"truncated"`
	PrunedThrough      uint64          `json:"prunedThrough"`
	RejectedCandidates int             `json:"rejectedCandidates"`
	Sources            []string        `json:"sources,omitempty"`
}

// GetContextGraph reads a bounded neighborhood of the selected session. It never
// writes Workflow facts or changes references, verification, or delivery status.
func (e *Engine) GetContextGraph(ctx context.Context, id string) (ContextGraphView, error) {
	if err := e.runtimeEventAccess(ctx, id); err != nil {
		return ContextGraphView{}, err
	}
	state, err := e.loadTaskState(ctx, id)
	if err != nil {
		return ContextGraphView{}, err
	}
	workspace, err := e.agentWorkspace(ctx, id)
	if err != nil {
		return ContextGraphView{}, err
	}
	notes := make([]ThreadNote, len(state.Notes))
	for i, n := range state.Notes {
		notes[i] = threadctx.NoteView(n)
	}
	if err := e.applyNotes(ctx, workspace, id, true, state.Notes, notes); err != nil {
		return ContextGraphView{}, err
	}
	return e.contextGraph(ctx, id, state, notes)
}

func (e *Engine) contextGraph(ctx context.Context, id string, state taskstate.State, notes []ThreadNote) (ContextGraphView, error) {
	view := ContextGraphView{SessionID: id, StateVersion: state.Version, CapturedAt: time.Now().UTC(), Nodes: []ContextNode{}, Links: []ContextLink{}, Attention: []AttentionItem{}}
	if err := e.runtimeEventAccess(ctx, id); err != nil {
		return view, err
	}
	page, err := e.QueryRuntimeEvents(ctx, RuntimeEventQuery{SessionID: id, Limit: 1})
	if err != nil {
		return view, err
	}
	view.PrunedThrough = page.PrunedThrough
	events, truncated, err := e.store.ContextEvents(ctx, id, MaxContextEvents)
	if err != nil {
		return view, err
	}
	view.Truncated = truncated
	root := "session:" + id
	view.Nodes = append(view.Nodes, ContextNode{ID: root, Kind: "session", SourceRef: root, Status: "context", Validation: "record_checked", Freshness: "current"})
	seen := map[string]bool{root: true}
	add := func(n ContextNode, relation string) {
		if seen[n.ID] {
			return
		}
		if len(view.Nodes) >= MaxContextNodes {
			view.Truncated = true
			return
		}
		seen[n.ID] = true
		n.Summary = threadctx.Cut(diag.MaskSecrets(n.Summary), 300)
		n.SourceRef = diag.MaskSecrets(n.SourceRef)
		view.Nodes = append(view.Nodes, n)
		view.Links = append(view.Links, ContextLink{From: root, To: n.ID, Kind: relation})
	}
	// Verification is taken from the authoritative Task State, never event payload.
	// Historical delivery is not a fresh filesystem verification and cannot be reused.
	if v := state.Verification; v != nil {
		freshness := "unknown"
		fingerprint, err := e.workspaceFingerprint(ctx, id)
		if err != nil {
			return view, err
		}
		if fingerprint != "" && v.Fingerprint != "" {
			freshness = "stale"
			if fingerprint == v.Fingerprint {
				freshness = "current"
			}
		}
		add(ContextNode{ID: "verification", Kind: "verification", Summary: "Verification: " + v.Status, SourceRef: fmt.Sprintf("task_state:%d:verification", state.Version), Status: v.Status, Validation: "record_checked", Freshness: freshness}, "records")
	}
	if d := state.DeliveryResult; d != nil {
		add(ContextNode{ID: "delivery", Kind: "delivery", Summary: "Last delivery verification: " + d.Status, SourceRef: fmt.Sprintf("task_state:%d:delivery", state.Version), Status: d.Status, Validation: "record_checked", Freshness: "historical"}, "records")
	}
	for _, c := range state.Claims {
		if c.CountsAsRequired() && string(c.Status) != "confirmed" && string(c.Status) != "revalidated" {
			add(ContextNode{ID: "claim:" + c.ID, Kind: "verification", Summary: "Unresolved required claim: " + c.Statement, SourceRef: fmt.Sprintf("task_state:%d:claim:%s", state.Version, c.ID), Status: string(c.Status), Validation: "record_checked", Freshness: "current"}, "requires")
		}
	}
	if wf := state.Workflow; wf != nil {
		add(ContextNode{ID: "workflow", Kind: "workflow", Summary: "Workflow action: " + wf.CurrentAction + "; status: " + wf.Status, SourceRef: fmt.Sprintf("task_state:%d:workflow", state.Version), Status: wf.Status, Validation: "record_checked", Freshness: "current"}, "constrains")
	}
	workers, err := e.hubTasks(ctx, id, state)
	if err != nil {
		return view, err
	}
	workerByID := make(map[string]HubTask, len(workers))
	for _, worker := range workers {
		if worker.WorkerID != "" {
			workerByID[worker.WorkerID] = worker
		}
	}
	for _, event := range events {
		if event.Type == "tool.end" || event.Type == "eval.callback" {
			node, ok, err := e.contextExecution(ctx, id, event)
			if err != nil {
				return view, err
			}
			if !ok {
				view.RejectedCandidates++
				continue
			}
			add(node, "observed")
		} else if event.Type == "worker.result.ready" {
			worker, ok := workerByID[event.WorkerID]
			if !ok || worker.Error != "" {
				view.RejectedCandidates++
				continue
			}
			add(ContextNode{ID: "worker:" + worker.WorkerID, Kind: "worker", Summary: "Worker result: " + worker.State + "; verification: " + worker.Verification, SourceRef: "session:" + worker.WorkerID, EventID: event.ID, Status: worker.State, Validation: "record_checked", Freshness: "current"}, "delegates")
			if seen["worker:"+worker.WorkerID] && !slices.Contains(view.Sources, worker.WorkerID) {
				view.Sources = append(view.Sources, worker.WorkerID)
			}
		} else {
			// State records above are authoritative. An event is only a discovery cue.
			// Do not infer facts or successful verification from a caller-supplied event.
			view.RejectedCandidates++
		}
	}
	for _, ref := range state.References {
		if ref.State == taskstate.RefExcluded || ref.State == taskstate.RefSuggested {
			continue
		}
		if ref.Kind == taskstate.RefThread {
			if err := e.runtimeEventAccess(ctx, ref.Target); err != nil {
				continue
			}
		}
		provenance, err := e.SourceStatus(ctx, id, taskstate.EntryRef(taskstate.EntryReference, ref.ID, ref))
		if err != nil {
			return view, err
		}
		if provenance.Withdrawn {
			continue
		}
		validation := "unverified"
		if provenance.Recorded && provenance.LineageComplete {
			validation = "record_checked"
		}
		add(ContextNode{ID: "reference:" + ref.ID, Kind: "reference", Summary: ref.Kind + ": " + ref.Target, SourceRef: ref.Target, Status: ref.State, Validation: validation, Freshness: "unknown"}, "includes")
		if ref.Kind == taskstate.RefThread && seen["reference:"+ref.ID] && !slices.Contains(view.Sources, ref.Target) {
			view.Sources = append(view.Sources, ref.Target)
		}
	}
	for _, note := range notes {
		if note.Applicability == threadctx.ApplicabilityWithdrawn || note.Applicability == threadctx.ApplicabilityMissing {
			continue
		}
		validation := "unverified"
		if !note.LineageIncomplete && note.Origin != "" {
			validation = "record_checked"
		}
		add(ContextNode{ID: "note:" + note.ID, Kind: "note", Summary: note.Text, SourceRef: "note:" + note.ID, Status: "context", Validation: validation, Freshness: note.Applicability}, "cites")
	}
	view.Attention = contextAttention(view.Nodes)
	// Revalidate cross-session sources before returning a cached projection.
	for _, source := range view.Sources {
		if err := e.runtimeEventAccess(ctx, source); err != nil {
			return ContextGraphView{}, err
		}
	}
	if err := e.runtimeEventAccess(ctx, id); err != nil {
		return ContextGraphView{}, err
	}
	latest, err := e.loadTaskState(ctx, id)
	if err != nil {
		return ContextGraphView{}, err
	}
	if latest.Version != state.Version {
		return ContextGraphView{}, errors.New("context changed during projection; refresh")
	}
	return view, nil
}

func (e *Engine) contextExecution(ctx context.Context, id string, event Event) (ContextNode, bool, error) {
	var fields struct {
		ExecutionID string `json:"executionId"`
		Name        string `json:"name"`
		Tool        string `json:"tool"`
		OK          bool   `json:"ok"`
	}
	if err := json.Unmarshal(event.Data, &fields); err != nil {
		return ContextNode{}, false, err
	}
	tool := fields.Name
	if tool == "" {
		tool = fields.Tool
	}
	node := ContextNode{ID: "event:" + event.ID, Kind: "tool", SourceRef: "event:" + event.ID, EventID: event.ID, Validation: "record_checked", Freshness: "historical"}
	if fields.ExecutionID != "" {
		x, err := e.store.Execution(ctx, fields.ExecutionID)
		if errors.Is(err, session.ErrExecutionNotFound) {
			return node, false, nil
		}
		if err != nil {
			return node, false, err
		}
		if x.SessionID != id || x.RunID != event.RunID || x.ToolName != tool || !x.LineageComplete || x.Outcome == session.OutcomeAdmitted {
			return node, false, nil
		}
		request, err := e.store.Request(ctx, x.RequestID)
		if err != nil {
			return node, false, err
		}
		if request.SessionID != id || request.RunID != event.RunID || !request.LineageComplete || request.AnsweredSeq == nil {
			return node, false, nil
		}
		projection, err := e.store.Projection(ctx, id)
		if err != nil {
			return node, false, err
		}
		if projection.Isolated != "" || slices.Contains(projection.Withheld, *request.AnsweredSeq) {
			return node, false, nil
		}
		node.SourceRef = "execution:" + x.ExecutionID
		node.Status = string(x.Outcome)
	} else {
		// Read-only/denied calls have no side-effect execution row. Require the
		// matching committed tool message before accepting the host outcome metadata.
		if event.RunID == "" || event.CorrelationID == "" {
			return node, false, nil
		}
		rows, err := e.store.RuntimeToolMessages(ctx, id, event.RunID, event.CorrelationID)
		if err != nil {
			return node, false, err
		}
		messages, err := e.hostMessages(ctx, id, rows)
		if err != nil {
			return node, false, err
		}
		foundCall, foundResult := false, false
		for _, row := range messages {
			if row.Withheld {
				return node, false, nil
			}
			var m protocol.Message
			if err := json.Unmarshal(row.JSON, &m); err != nil {
				return node, false, err
			}
			for _, call := range m.ToolCalls {
				if call.ID == event.CorrelationID && call.Name == tool {
					foundCall = true
				}
			}
			if m.ToolCallID == event.CorrelationID {
				foundResult = true
				node.Status = "failed"
				if fields.OK {
					node.Status = "completed"
				}
				node.SourceRef = fmt.Sprintf("message:%s:%d", id, row.Seq)
			}
		}
		if !foundCall || !foundResult {
			return node, false, nil
		}
	}
	node.Summary = tool + ": " + node.Status + " (execution outcome, not semantic verification)"
	return node, true, nil
}

func contextAttention(nodes []ContextNode) []AttentionItem {
	type candidate struct {
		node     ContextNode
		priority int
		reason   string
	}
	candidates := make([]candidate, 0, len(nodes))
	for _, node := range nodes {
		if node.Validation != "record_checked" {
			continue
		}
		priority, reason := 0, ""
		switch node.Kind {
		case "verification":
			if node.Status != "passed" || node.Freshness != "current" {
				priority, reason = 5, "unresolved_verification"
			}
		case "delivery":
			if node.Status != DeliveryVerified {
				priority, reason = 5, "unresolved_delivery"
			}
		case "tool":
			if node.Status != "completed" {
				priority, reason = 4, "recent_tool_failure"
			}
		case "reference":
			priority, reason = 3, "selected_reference"
		case "worker":
			priority, reason = 2, "worker_result"
		case "workflow":
			priority, reason = 1, "workflow_constraint"
		}
		if priority > 0 {
			candidates = append(candidates, candidate{node, priority, reason})
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].priority > candidates[j].priority })
	out := []AttentionItem{}
	bytes := 2
	for _, c := range candidates {
		item := AttentionItem{NodeID: c.node.ID, Reason: c.reason, Summary: c.node.Summary, SourceRef: c.node.SourceRef, Validation: c.node.Validation, Freshness: c.node.Freshness}
		raw, _ := json.Marshal(item)
		if len(out) == MaxAttentionItems || bytes+len(raw)+1 > MaxAttentionBytes {
			break
		}
		out = append(out, item)
		bytes += len(raw) + 1
	}
	return out
}
