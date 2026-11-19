package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/protocol"
)

const (
	r303ShaA = "1111111111111111111111111111111111111111111111111111111111111111"
	r303ShaB = "2222222222222222222222222222222222222222222222222222222222222222"
)

// r303Exec builds a persisted execution summary whose deterministic classification is ev.
func r303Exec(answeredSeq int64, id, tool string, ev MutationEvidence) session.AnsweredExecutionSummary {
	s := session.AnsweredExecutionSummary{
		AnsweredSeq: answeredSeq,
		RequestID:   "r303-req",
		ExecutionID: id,
		ToolName:    tool,
		Outcome:     session.OutcomeComplete,
		Coverage:    session.CoverageComplete,
	}
	switch tool {
	case "write_file":
		s.PreRef = "files:1:sha256:" + r303ShaA
		switch ev {
		case MutationProven:
			s.PostRef = "files:1:sha256:" + r303ShaB + ";cm=proven"
		case MutationNone:
			s.PostRef = "files:1:sha256:" + r303ShaA + ";cm=none"
		default:
			s.PostRef = "files:1:sha256:" + r303ShaB
		}
	case "bash":
		s.PreRef = "ws:sha256:" + r303ShaA
		s.PostRef = "ws:sha256:" + r303ShaB
	}
	if got := classifyExecutionSummary(s); got != ev {
		panic("r303Exec fixture classifies as " + string(got) + ", want " + string(ev))
	}
	return s
}

// TestR303CausalRefBindsAllFieldsToOneToolCall pins R3-03: the CausalEvidenceRef of a multi-call
// answered group must take ToolCallID, ToolName, ExecutionID and PostRef from one and the same tool
// call (the per-call execution mapping), and that call must be the one whose own evidence explains
// the group's MutationEvidence. The group MutationEvidence itself is unchanged.
func TestR303CausalRefBindsAllFieldsToOneToolCall(t *testing.T) {
	const aSeq = int64(10)
	type row struct {
		id     string
		origin string
	}
	type want struct {
		toolCallID, tool, execID, postRef string
		ev                                MutationEvidence
	}
	call := func(id, name string) protocol.ToolCall {
		return protocol.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(`{}`)}
	}
	wProven := r303Exec(aSeq, "ex-w", "write_file", MutationProven)
	wUnknown := r303Exec(aSeq, "ex-w", "write_file", MutationUnknown)
	wNoop := r303Exec(aSeq, "ex-w1", "write_file", MutationNone)
	w2Proven := r303Exec(aSeq, "ex-w2", "write_file", MutationProven)
	rNone := r303Exec(aSeq, "ex-r", "read_file", MutationNone)
	lNone := r303Exec(aSeq, "ex-l", "list_dir", MutationNone)

	cases := []struct {
		name  string
		calls []protocol.ToolCall
		rows  []row
		execs []session.AnsweredExecutionSummary
		want  want
	}{
		{
			name:  "write proven after a read with no execution",
			calls: []protocol.ToolCall{call("c-r", "read_file"), call("c-w", "write_file")},
			rows:  []row{{id: "c-r"}, {id: "c-w"}},
			execs: []session.AnsweredExecutionSummary{wProven},
			want:  want{"c-w", "write_file", "ex-w", wProven.PostRef, MutationProven},
		},
		{
			name:  "write proven after an admitted read",
			calls: []protocol.ToolCall{call("c-r", "read_file"), call("c-w", "write_file")},
			rows:  []row{{id: "c-r"}, {id: "c-w"}},
			execs: []session.AnsweredExecutionSummary{rNone, wProven},
			want:  want{"c-w", "write_file", "ex-w", wProven.PostRef, MutationProven},
		},
		{
			name:  "write unknown after an admitted read",
			calls: []protocol.ToolCall{call("c-r", "read_file"), call("c-w", "write_file")},
			rows:  []row{{id: "c-r"}, {id: "c-w"}},
			execs: []session.AnsweredExecutionSummary{rNone, wUnknown},
			want:  want{"c-w", "write_file", "ex-w", wUnknown.PostRef, MutationUnknown},
		},
		{
			name:  "interrupted second call without execution",
			calls: []protocol.ToolCall{call("c-r", "read_file"), call("c-b", "bash")},
			rows:  []row{{id: "c-r"}, {id: "c-b", origin: protocol.OriginToolInterrupted}},
			execs: []session.AnsweredExecutionSummary{rNone},
			want:  want{"c-b", "bash", "", "", MutationUnknown},
		},
		{
			name:  "unpaired second call",
			calls: []protocol.ToolCall{call("c-r", "read_file"), call("c-b", "bash")},
			rows:  []row{{id: "c-r"}},
			execs: []session.AnsweredExecutionSummary{rNone},
			want:  want{"c-b", "bash", "", "", MutationUnknown},
		},
		{
			name:  "same-name calls bind executions in admission order",
			calls: []protocol.ToolCall{call("c-w1", "write_file"), call("c-w2", "write_file")},
			rows:  []row{{id: "c-w1"}, {id: "c-w2"}},
			execs: []session.AnsweredExecutionSummary{wNoop, w2Proven},
			want:  want{"c-w2", "write_file", "ex-w2", w2Proven.PostRef, MutationProven},
		},
		{
			name:  "no-change group keeps the first call with only its own execution",
			calls: []protocol.ToolCall{call("c-r", "read_file"), call("c-l", "list_dir")},
			rows:  []row{{id: "c-r"}, {id: "c-l"}},
			execs: []session.AnsweredExecutionSummary{lNone},
			want:  want{"c-r", "read_file", "", "", MutationNone},
		},
		{
			name:  "single call",
			calls: []protocol.ToolCall{call("c-w", "write_file")},
			rows:  []row{{id: "c-w"}},
			execs: []session.AnsweredExecutionSummary{wProven},
			want:  want{"c-w", "write_file", "ex-w", wProven.PostRef, MutationProven},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			asst := protocol.Message{Role: "assistant", ToolCalls: tc.calls}
			meta := buildAssistantHistoryMeta(aSeq, asst)
			for i, r := range tc.rows {
				applyToolRowToHistoryMeta(&meta, aSeq+1+int64(i), protocol.Message{Role: "tool", ToolCallID: r.id, Origin: r.origin})
			}
			applyExecutionSummariesToMeta(&meta, tc.execs)
			if group := aggregateHistoryGroupMutation(meta, tc.execs); meta.MutationEvidence != group {
				t.Fatalf("group MutationEvidence = %s, want the unchanged aggregate %s", meta.MutationEvidence, group)
			}
			got := causalRefFromMeta(meta)
			w := tc.want
			if got.AnsweredSeq != aSeq || got.ToolCallID != w.toolCallID || got.ToolName != w.tool ||
				got.ExecutionID != w.execID || got.PostRef != w.postRef || got.MutationEvidence != w.ev {
				t.Fatalf("ref = %+v\nwant toolCallId=%s tool=%s exec=%s postRef=%q evidence=%s",
					got, w.toolCallID, w.tool, w.execID, w.postRef, w.ev)
			}
			if got.ExecutionID != "" {
				for _, ex := range tc.execs {
					if ex.ExecutionID == got.ExecutionID && ex.ToolName != got.ToolName {
						t.Fatalf("ref names tool %s but carries execution %s of tool %s", got.ToolName, ex.ExecutionID, ex.ToolName)
					}
				}
			}
		})
	}
}

// TestR303GatedLedgerNamesTheExecutingCallLiveAndCold reproduces the R3 review counterexample on a
// real SQLite store: after a completion-gate refusal the model sends read_file then write_file in one
// reply. The causal ledger rendered into State must name write_file with write_file's execution, and
// the live run, a cold load and the SQLite stream must agree.
func TestR303GatedLedgerNamesTheExecutingCallLiveAndCold(t *testing.T) {
	ctx := context.Background()
	rawStore, err := session.Open(filepath.Join(t.TempDir(), "r303.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rawStore.Close() }()
	const (
		sid    = "s-r303"
		runID  = "run-r303"
		maxCtx = 32768
	)
	if err := rawStore.Create(ctx, session.Session{ID: sid, Goal: "R3-03 ledger provenance", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := rawStore.BeginRun(ctx, sid, session.RunStart{
		RunID: runID,
		Input: &protocol.Message{Role: "user", Content: "fix the failing test"},
	}); err != nil {
		t.Fatal(err)
	}
	st := realSQLiteAgentStore{s: rawStore}
	rLive := &run{}
	if err := rLive.load(ctx, st, sid, nil, maxCtx); err != nil {
		t.Fatal(err)
	}

	prop := protocol.Message{Role: "assistant", Content: "done"}
	pSeq, _, err := st.AppendMessage(ctx, sid, prop, nil)
	if err != nil {
		t.Fatal(err)
	}
	rLive.appendAssistant(pSeq, prop, "p/m", protocol.Echo{})
	blk := protocol.Message{Role: "user", Origin: protocol.OriginCompletionBlocked, Content: "Completion refused: `go test ./...` fails."}
	bSeq, _, err := st.AppendMessage(ctx, sid, blk, nil)
	if err != nil {
		t.Fatal(err)
	}
	rLive.appendRow(bSeq, blk)

	f0, l0 := int64(0), bSeq
	const reqID = "r303-req"
	if _, err := st.AdmitRequest(ctx, session.RequestAdmission{
		RequestID: reqID, SessionID: sid, RunID: runID, Purpose: session.PurposeMain,
		Step: 2, FirstSeq: &f0, LastSeq: &l0, ProjectionVersion: 2,
		Refs: []string{"model:p/m", "proj:mode:compact", "ts:v0"},
	}); err != nil {
		t.Fatal(err)
	}
	asst := protocol.Message{Role: "assistant", ToolCalls: []protocol.ToolCall{
		{ID: "c-read", Name: "read_file", Arguments: json.RawMessage(`{"path":"fix.go"}`)},
		{ID: "c-write", Name: "write_file", Arguments: json.RawMessage(`{"path":"fix.go","content":"package p"}`)},
	}}
	aSeq, err := st.AppendAnswered(ctx, sid, reqID, asst)
	if err != nil {
		t.Fatal(err)
	}
	rLive.appendAssistant(aSeq, asst, "p/m", protocol.Echo{})

	readRow := protocol.Message{Role: "tool", ToolCallID: "c-read", Name: "read_file", Content: "package p // old"}
	rSeq, _, err := st.CommitToolResult(ctx, ToolResultCommit{SessionID: sid, RunID: runID, Message: readRow})
	if err != nil {
		t.Fatal(err)
	}
	rLive.appendRow(rSeq, readRow)
	if err := st.AdmitExecution(ctx, session.ExecutionAdmission{
		ExecutionID: "ex-write", SessionID: sid, RunID: runID, RequestID: reqID,
		ToolName: "write_file", ArgsDigest: "sha256:" + r303ShaA, PreRef: "files:1:sha256:" + r303ShaA,
	}); err != nil {
		t.Fatal(err)
	}
	writeRow := protocol.Message{Role: "tool", ToolCallID: "c-write", Name: "write_file", Content: "wrote fix.go"}
	wSeq, _, err := st.CommitToolResult(ctx, ToolResultCommit{
		SessionID: sid, RunID: runID, Message: writeRow,
		Execution: &session.ExecutionResult{
			ExecutionID: "ex-write", Outcome: session.OutcomeComplete, Coverage: session.CoverageComplete,
			PostRef: "files:1:sha256:" + r303ShaB + ";cm=proven",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	rLive.appendRow(wSeq, writeRow)
	if err := rLive.refreshGroupMutation(ctx, st, sid, aSeq); err != nil {
		t.Fatal(err)
	}

	live, err := rLive.deriveRepairContext(ctx, st, sid)
	if err != nil {
		t.Fatal(err)
	}
	rCold := &run{}
	if err := rCold.load(ctx, st, sid, nil, maxCtx); err != nil {
		t.Fatal(err)
	}
	cold, err := rCold.deriveRepairContext(ctx, st, sid)
	if err != nil {
		t.Fatal(err)
	}
	streamed, err := StreamRepairContextFromStore(ctx, st, sid, 0)
	if err != nil {
		t.Fatal(err)
	}
	if cold != live || streamed != live {
		t.Fatalf("repair context diverges:\nlive=%+v\ncold=%+v\nstreamed=%+v", live, cold, streamed)
	}
	if !live.Active || live.CausalProgressLen != 1 || live.PostBlockProvenCount != 1 {
		t.Fatalf("want one proven post-gate step, got %+v", live)
	}
	ref := live.CausalProgress[0]
	if ref.AnsweredSeq != aSeq || ref.ToolCallID != "c-write" || ref.ToolName != "write_file" ||
		ref.ExecutionID != "ex-write" || ref.MutationEvidence != MutationProven {
		t.Fatalf("post-gate ref = %+v, want seq=%d toolCallId=c-write tool=write_file exec=ex-write evidence=proven", ref, aSeq)
	}
	state := appendPinnedGateAndRelaxationNotice("", live, live.MinRequiredSeq, "", live.MinRequiredSeq, -1)
	if !strings.Contains(state, "tool=write_file exec=ex-write evidence=proven") || strings.Contains(state, "tool=read_file exec=ex-write") {
		t.Fatalf("ledger misattributes the write execution:\n%s", state)
	}
}
