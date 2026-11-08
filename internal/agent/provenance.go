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

// Tools whose result carries other sessions: the subagent tools (internal/toolset/subagents.go), the
// thread tools (internal/toolset/threads.go) and the notebook reads (internal/toolset/notebook.go).
const (
	toolWaitSubagent  = "wait_subagent"
	toolListSubagents = "list_subagents"
	toolReadThread    = "read_thread"
	toolRecall        = "recall"
	toolNotebookList  = "notebook_list"
	toolNotebookRead  = "notebook_read"
	maxResultSources  = session.MaxEdgesPerCommit
)

// subagentNode is the part of a subagent tool result the edges need: spawn_subagent returns a node,
// wait_subagent {"node": node, ...}, list_subagents a tree of nodes. A withdrawn node carries no
// content of its child, so it is no source.
type subagentNode struct {
	SessionID string         `json:"sessionId"`
	Withdrawn bool           `json:"withdrawn,omitempty"`
	Children  []subagentNode `json:"children,omitempty"`
}

// threadResult is the part of a thread tool result the edges need: read_thread returns the thread and
// its children (a child's goal is its content, unless it is withdrawn), recall {"hits": [...]}.
type threadResult struct {
	ID       string `json:"id"`
	Children []struct {
		ID        string `json:"id"`
		Withdrawn bool   `json:"withdrawn"`
	} `json:"children"`
	Withdrawn bool `json:"withdrawn"`
	Hits      []struct {
		ID string `json:"id"`
	} `json:"hits"`
}

// resultSessions returns the sessions whose content a successful tool result carries: the spawned,
// waited or listed children of a subagent tool (depth first), the thread read_thread returned and its
// listed children, every thread recall found, or the session whose notebooks notebook_list or
// notebook_read returned ({"session": id}, no source when withdrawn). own, the reading session, is
// never a source of itself. Anything else names none. ok is false when the result could not be read
// or held more sessions than one commit stores, so what is recorded is incomplete.
func resultSessions(name, own string, result protocol.ToolResult) (sessions []string, ok bool) {
	if !result.OK {
		return nil, true
	}
	switch name {
	case "worker_inbox":
		var page struct {
			Messages []struct {
				SenderID  string `json:"senderId"`
				Withdrawn bool   `json:"withdrawn"`
			} `json:"messages"`
		}
		if err := json.Unmarshal([]byte(result.Content), &page); err != nil {
			return nil, false
		}
		seen := map[string]bool{}
		for _, message := range page.Messages {
			if !message.Withdrawn && message.SenderID != "" && message.SenderID != own && !seen[message.SenderID] {
				seen[message.SenderID] = true
				sessions = append(sessions, message.SenderID)
			}
		}
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
		if w.Node.Withdrawn {
			return nil, true
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
				if n.SessionID != "" && !n.Withdrawn {
					sessions = append(sessions, n.SessionID)
				}
				walk(n.Children)
			}
		}
		walk(nodes)
	case toolReadThread, toolRecall:
		var t threadResult
		if err := json.Unmarshal([]byte(result.Content), &t); err != nil {
			return nil, false
		}
		if name == toolReadThread {
			if t.ID == "" {
				return nil, false
			}
			if !t.Withdrawn && t.ID != own {
				sessions = append(sessions, t.ID)
			}
			for _, c := range t.Children {
				if !c.Withdrawn && c.ID != "" && c.ID != own {
					sessions = append(sessions, c.ID)
				}
			}
		}
		for _, h := range t.Hits {
			if h.ID != "" && h.ID != own {
				sessions = append(sessions, h.ID)
			}
		}
	case toolNotebookList, toolNotebookRead:
		var n struct {
			Session   string `json:"session"`
			Withdrawn bool   `json:"withdrawn"`
		}
		if err := json.Unmarshal([]byte(result.Content), &n); err != nil || n.Session == "" {
			return nil, false
		}
		if !n.Withdrawn && n.Session != own {
			sessions = append(sessions, n.Session)
		}
	default:
		return nil, true
	}
	if len(sessions) > maxResultSources {
		return sessions[:maxResultSources], false
	}
	return sessions, true
}

// sessionResultEdges are the edges a tool result carrying other sessions makes (resultSessions):
// spawn_subagent's child session derives from the request that spawned it; the tool row of any other
// such tool derives from every session it carries, as of that session's newest transcript seq read now
// (an upper bound of what the row shows). These are the edges a revocation of one of those sessions
// finds this one by, and a commit after that revocation is refused by. A seq that cannot be read
// leaves that edge without a seq, which the Store stores incomplete.
func (l *Loop) sessionResultEdges(ctx context.Context, sessionID string, call protocol.ToolCall, result protocol.ToolResult, requestID string, lineage bool) []session.DerivedEdge {
	sources, ok := resultSessions(call.Name, sessionID, result)
	var edges []session.DerivedEdge
	for _, source := range sources {
		if call.Name == toolSpawnSubagent {
			edges = append(edges, session.DerivedEdge{
				Kind: "child_session", TargetRef: sessionRef(source), SourceKind: session.SourceRequest, SourceRef: requestID,
				OriginClass: session.OriginAgentGen, LineageComplete: lineage && ok,
			})
			continue
		}
		from := sessionRef(source)
		if seq, err := l.Store.NewestMessageSeq(ctx, source); err == nil && seq >= 0 {
			from = sessionAtRef(source, seq)
		}
		edges = append(edges, session.DerivedEdge{
			Kind: "message", TargetRef: toolRowRef(requestID, call.ID), SourceKind: session.SourceSession, SourceRef: source,
			OriginClass: session.OriginAgentGen, DerivedFrom: from, LineageComplete: ok,
		})
	}
	return edges
}
