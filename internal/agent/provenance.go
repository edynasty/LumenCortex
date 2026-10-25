package agent

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/taskstate"
	"github.com/edynasty/LumenCortex/protocol"
)

// Derived edges of message-level bodies (ADR-0018, plan T1-S5). Targets and sources are locators,
// never text:
//
//	ts:<kind>:<id>@sha256:<hex>   one version of a Task State entry (taskstate.EntryRef)
//	session:<id>                  a child session (target of a child_session edge)
//	session:<id>@<seq>            a child session as of its transcript seq (DerivedFrom of a result)
//	tool:<requestId>:<callId>     the tool row answering call callId of the request requestId
//	task_result:<id>@<seq>        the task_result row delivering child id as of its seq

func sessionRef(id string) string { return "session:" + id }

func sessionAtRef(id string, seq int64) string {
	return "session:" + id + "@" + strconv.FormatInt(seq, 10)
}

func toolRowRef(requestID, callID string) string { return "tool:" + requestID + ":" + callID }

func taskResultRef(id string, seq int64) string {
	return "task_result:" + id + "@" + strconv.FormatInt(seq, 10)
}

// modelStateEdges are the edges of the Task State entries a tool call of request requestID wrote: every
// entry new or changed between before and after derives from that request, and so does a child session
// a task gained (start_task). lineage is the request envelope's.
func modelStateEdges(before, after taskstate.State, requestID string, lineage bool) []session.DerivedEdge {
	var edges []session.DerivedEdge
	for _, e := range taskstate.ChangedEntries(before, after) {
		edges = append(edges, session.DerivedEdge{
			Kind: "message", TargetRef: e.Ref, SourceKind: session.SourceRequest, SourceRef: requestID,
			OriginClass: session.OriginAgentGen, LineageComplete: lineage,
		})
	}
	for _, c := range taskstate.StartedChildren(before, after) {
		edges = append(edges, session.DerivedEdge{
			Kind: "child_session", TargetRef: sessionRef(c.SessionID), SourceKind: session.SourceRequest, SourceRef: requestID,
			OriginClass: session.OriginAgentGen, LineageComplete: lineage,
		})
	}
	return edges
}

// HostStateEdges are the edges of a Task State version a host write makes, outside any tool call:
//   - a user rewrite of agent text (an entry with From) derives from the version it replaced: a user
//     decision about wording whose facts keep the replaced version's provenance;
//   - a child the user started from a proposal derives from the proposal entry;
//   - a task result delivered with its cursor derives from the child as of the delivered seq.
//
// A new entry the user wrote has no edge: what the user pasted cannot be traced. The Store stores each
// edge incomplete when its source is (session.insertEdges).
func HostStateEdges(before, after taskstate.State) []session.DerivedEdge {
	var edges []session.DerivedEdge
	for _, e := range taskstate.ChangedEntries(before, after) {
		if e.From == "" {
			continue
		}
		edges = append(edges, session.DerivedEdge{
			Kind: "message", TargetRef: e.Ref, SourceKind: session.SourceMessage, SourceRef: e.From,
			OriginClass: session.OriginUserDecision, DerivedFrom: e.From, LineageComplete: true,
		})
	}
	for _, c := range taskstate.StartedChildren(before, after) {
		if c.Proposal == "" {
			continue
		}
		edges = append(edges, session.DerivedEdge{
			Kind: "child_session", TargetRef: sessionRef(c.SessionID), SourceKind: session.SourceMessage, SourceRef: c.Proposal,
			OriginClass: session.OriginAgentGen, DerivedFrom: c.Proposal, LineageComplete: true,
		})
	}
	for _, d := range taskstate.Deliveries(before, after) {
		edges = append(edges, session.DerivedEdge{
			Kind: "message", TargetRef: taskResultRef(d.SessionID, d.Seq), SourceKind: session.SourceSession, SourceRef: d.SessionID,
			OriginClass: session.OriginAgentGen, DerivedFrom: sessionAtRef(d.SessionID, d.Seq), LineageComplete: true,
		})
	}
	return edges
}

// Subagent tools whose result carries child sessions (internal/toolset/subagents.go).
const (
	toolWaitSubagent   = "wait_subagent"
	toolListSubagents  = "list_subagents"
	maxSubagentSources = session.MaxEdgesPerCommit
)

// subagentNode is the part of a subagent tool result the edges need: spawn_subagent returns a node,
// wait_subagent {"node": node, ...}, list_subagents a tree of nodes.
type subagentNode struct {
	SessionID string         `json:"sessionId"`
	Children  []subagentNode `json:"children,omitempty"`
}

// subagentChildren returns the child sessions a successful subagent tool result names: the spawned
// child, the waited child, or every listed child (depth first). Anything else names none. ok is false
// when the result could not be read or held more children than one commit stores, so what is recorded
// is incomplete.
func subagentChildren(name string, result protocol.ToolResult) (children []string, ok bool) {
	if !result.OK {
		return nil, true
	}
	switch name {
	case toolSpawnSubagent:
		var n subagentNode
		if err := json.Unmarshal([]byte(result.Content), &n); err != nil || n.SessionID == "" {
			return nil, false
		}
		return []string{n.SessionID}, true
	case toolWaitSubagent:
		var w struct {
			Node subagentNode `json:"node"`
		}
		if err := json.Unmarshal([]byte(result.Content), &w); err != nil || w.Node.SessionID == "" {
			return nil, false
		}
		return []string{w.Node.SessionID}, true
	case toolListSubagents:
		var nodes []subagentNode
		if err := json.Unmarshal([]byte(result.Content), &nodes); err != nil {
			return nil, false
		}
		var walk func([]subagentNode)
		walk = func(nodes []subagentNode) {
			for _, n := range nodes {
				if n.SessionID != "" {
					children = append(children, n.SessionID)
				}
				walk(n.Children)
			}
		}
		walk(nodes)
		if len(children) > maxSubagentSources {
			return children[:maxSubagentSources], false
		}
		return children, true
	}
	return nil, true
}

// subagentEdges are the edges a subagent tool result makes: spawn_subagent's child session derives from
// the request that spawned it; the tool row of wait_subagent or list_subagents derives from every child
// it reports, as of the child's newest transcript seq read now (an upper bound of what the row shows).
// A child seq that cannot be read leaves that edge without a seq, which the Store stores incomplete.
func (l *Loop) subagentEdges(ctx context.Context, call protocol.ToolCall, result protocol.ToolResult, requestID string, lineage bool) []session.DerivedEdge {
	children, ok := subagentChildren(call.Name, result)
	var edges []session.DerivedEdge
	for _, child := range children {
		if call.Name == toolSpawnSubagent {
			edges = append(edges, session.DerivedEdge{
				Kind: "child_session", TargetRef: sessionRef(child), SourceKind: session.SourceRequest, SourceRef: requestID,
				OriginClass: session.OriginAgentGen, LineageComplete: lineage && ok,
			})
			continue
		}
		from := sessionRef(child)
		if seq, err := l.Store.NewestMessageSeq(ctx, child); err == nil && seq >= 0 {
			from = sessionAtRef(child, seq)
		}
		edges = append(edges, session.DerivedEdge{
			Kind: "message", TargetRef: toolRowRef(requestID, call.ID), SourceKind: session.SourceSession, SourceRef: child,
			OriginClass: session.OriginAgentGen, DerivedFrom: from, LineageComplete: ok,
		})
	}
	return edges
}
