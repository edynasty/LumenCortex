package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/taskstate"
	"github.com/edynasty/LumenCortex/internal/window"
	"github.com/edynasty/LumenCortex/protocol"
)

// realSQLiteAgentStore adapts a real *session.Store (SQLite on disk) to agent.Store for F-02/F-03 tests.
type realSQLiteAgentStore struct {
	s *session.Store
}

func (a realSQLiteAgentStore) NewestMessageSeq(ctx context.Context, sessionID string) (int64, error) {
	return a.s.NewestMessageSeq(ctx, sessionID)
}

func (a realSQLiteAgentStore) Get(ctx context.Context, sessionID string) (SessionState, error) {
	v, err := a.s.Get(ctx, sessionID)
	if err != nil {
		return SessionState{}, err
	}
	var u protocol.Usage
	if len(v.Usage) > 0 {
		_ = json.Unmarshal(v.Usage, &u)
	}
	final := ""
	if v.Final != nil {
		final = *v.Final
	}
	return SessionState{ID: v.ID, Goal: v.Goal, Status: v.Status, Final: final, Usage: u}, nil
}

func (a realSQLiteAgentStore) Transcript(ctx context.Context, sessionID string, fromSeq int64, fn func(seq int64, m protocol.Message) error) error {
	return a.s.EachMessageFrom(ctx, sessionID, fromSeq, func(item session.Message) error {
		var msg protocol.Message
		if err := json.Unmarshal(item.JSON, &msg); err != nil {
			return err
		}
		if msg.Role == "" {
			msg.Role = item.Role
		}
		return fn(item.Seq, msg)
	})
}

func encodeTaskStateWrite(change *TaskStateChange) (*session.TaskStateWrite, error) {
	if change == nil {
		return nil, nil
	}
	raw, err := change.Next.Encode()
	if err != nil {
		return nil, err
	}
	return &session.TaskStateWrite{
		Base:         change.Next.Version,
		Reason:       change.Reason,
		JSON:         raw,
		InboxThrough: change.InboxThrough,
		Edges:        change.Edges,
	}, nil
}

func (a realSQLiteAgentStore) AppendMessage(ctx context.Context, sessionID string, m protocol.Message, change *TaskStateChange) (int64, int64, error) {
	w, err := encodeTaskStateWrite(change)
	if err != nil {
		return 0, 0, err
	}
	return a.s.AppendMessageTaskState(ctx, sessionID, m.Role, m, w)
}

func (a realSQLiteAgentStore) AdmitRequest(ctx context.Context, adm session.RequestAdmission) (session.RequestEnvelope, error) {
	return a.s.AdmitRequest(ctx, adm)
}

func (a realSQLiteAgentStore) AppendAnswered(ctx context.Context, sessionID, requestID string, m protocol.Message) (int64, error) {
	return a.s.AppendAnswered(ctx, sessionID, requestID, m)
}

func (a realSQLiteAgentStore) SettleRequest(ctx context.Context, requestID string, st session.Settlement) error {
	return a.s.SettleRequest(ctx, requestID, st)
}

func (a realSQLiteAgentStore) AdmitExecution(ctx context.Context, adm session.ExecutionAdmission) error {
	return a.s.AdmitExecution(ctx, adm)
}

func (a realSQLiteAgentStore) CommitToolResult(ctx context.Context, c ToolResultCommit) (int64, int64, error) {
	w, err := encodeTaskStateWrite(c.Change)
	if err != nil {
		return 0, 0, err
	}
	return a.s.CommitToolResult(ctx, session.ToolCommit{
		SessionID: c.SessionID, RunID: c.RunID, Message: c.Message, TaskState: w, Execution: c.Execution, Edges: c.Edges,
	})
}

func (a realSQLiteAgentStore) ListAnsweredExecutions(ctx context.Context, sessionID string, fromSeq int64) ([]session.AnsweredExecutionSummary, error) {
	return a.s.ListAnsweredExecutions(ctx, sessionID, fromSeq)
}

func (a realSQLiteAgentStore) NextStep(ctx context.Context, sessionID string) (int64, error) {
	return a.s.NextStep(ctx, sessionID)
}

func (a realSQLiteAgentStore) AppendStep(ctx context.Context, sessionID string, step int64, v any) error {
	return a.s.AppendStep(ctx, sessionID, step, v)
}

func (a realSQLiteAgentStore) AppendCheckpoint(ctx context.Context, sessionID, reason string, v any) error {
	_, err := a.s.AppendCheckpoint(ctx, sessionID, reason, v)
	return err
}

func (a realSQLiteAgentStore) TaskState(ctx context.Context, sessionID string) (taskstate.State, error) {
	row, err := a.s.LatestTaskState(ctx, sessionID)
	if err != nil {
		return taskstate.State{}, err
	}
	return taskstate.Decode(row.Version, row.JSON)
}

func (a realSQLiteAgentStore) InboxEdits(ctx context.Context, sessionID string) ([]taskstate.QueuedEdit, error) {
	return nil, nil
}

func (a realSQLiteAgentStore) PutTaskState(ctx context.Context, sessionID string, change TaskStateChange) (int64, error) {
	w, err := encodeTaskStateWrite(&change)
	if err != nil {
		return 0, err
	}
	return a.s.PutTaskState(ctx, sessionID, *w)
}

func (a realSQLiteAgentStore) Update(ctx context.Context, sessionID string, p SessionPatch) error {
	return a.s.Update(ctx, sessionID, session.Patch{Status: p.Status, Final: p.Final, ClearFinal: p.ClearFinal})
}

// TestF02LiveAndColdSQLiteResumeRepairFloorParity proves on a real SQLite database (session.Open)
// that live execution with maxContext body trimming and a cold SQLite Resume derive the exact same
// MinRequiredSeq (Seq of G1, never drifting forward due to maxContext body trimming) while keeping
// in-memory message bodies strictly bounded by maxContext.
func TestF02LiveAndColdSQLiteResumeRepairFloorParity(t *testing.T) {
	ctx := context.Background()
	dbDir := filepath.Join(t.TempDir(), "state")
	rawStore, err := session.Open(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if rawStore != nil {
			_ = rawStore.Close()
		}
	}()

	const (
		sid        = "s-f02-sqlite"
		run1       = "run-live-1"
		maxContext = 32768
	)
	if err := rawStore.Create(ctx, session.Session{ID: sid, Goal: "fix long gated turn", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := rawStore.BeginRun(ctx, sid, session.RunStart{
		RunID: run1,
		Input: &protocol.Message{Role: "user", Content: "investigate and fix build"},
	}); err != nil {
		t.Fatal(err)
	}

	store := realSQLiteAgentStore{s: rawStore}
	rLive := &run{}
	if err := rLive.load(ctx, store, sid, nil, maxContext); err != nil {
		t.Fatal(err)
	}

	// Execute 40 bash tool steps (~1,050 tokens each -> ~42,000 tokens > 32,768 maxContext)
	// before any gate refusal, mirroring Loop.Run step-by-step on real SQLite.
	bigOut := strings.Repeat("build diagnostic line output 0123456789\n", 80) // ~3,120 bytes -> ~1,045 tokens
	firstBashSeq := int64(-1)
	for step := 1; step <= 40; step++ {
		if _, err := rLive.groups(maxContext); err != nil {
			t.Fatal(err)
		}
		firstSeq := rLive.transcript[0].Seq
		lastSeq := rLive.transcript[len(rLive.transcript)-1].Seq
		reqID := fmt.Sprintf("req-%02d", step)
		if _, err := store.AdmitRequest(ctx, session.RequestAdmission{
			RequestID:         reqID,
			SessionID:         sid,
			RunID:             run1,
			Purpose:           session.PurposeMain,
			Step:              step,
			Attempt:           0,
			FirstSeq:          &firstSeq,
			LastSeq:           &lastSeq,
			ProjectionVersion: 2,
			Refs:              []string{"model:p/m-32k", "proj:mode:compact", "ts:v0"},
		}); err != nil {
			t.Fatal(err)
		}
		tcID := fmt.Sprintf("tc-%02d", step)
		asst := protocol.Message{
			Role:      "assistant",
			ToolCalls: []protocol.ToolCall{{ID: tcID, Name: "bash", Arguments: json.RawMessage(`{"command":"make test"}`)}},
		}
		aSeq, err := store.AppendAnswered(ctx, sid, reqID, asst)
		if err != nil {
			t.Fatal(err)
		}
		if step == 1 {
			firstBashSeq = aSeq
		}
		rLive.appendAssistant(aSeq, asst, "p/m-32k", protocol.Echo{})

		execID := fmt.Sprintf("exec-%02d", step)
		if err := store.AdmitExecution(ctx, session.ExecutionAdmission{
			ExecutionID: execID,
			SessionID:   sid,
			RunID:       run1,
			RequestID:   reqID,
			ToolName:    "bash",
			ArgsDigest:  "sha256:0000000000000000000000000000000000000000000000000000000000000000",
			PreRef:      "ws:sha256:1111111111111111111111111111111111111111111111111111111111111111",
		}); err != nil {
			t.Fatal(err)
		}
		toolMsg := protocol.Message{Role: "tool", ToolCallID: tcID, Name: "bash", Content: bigOut}
		tSeq, _, err := store.CommitToolResult(ctx, ToolResultCommit{
			SessionID: sid,
			RunID:     run1,
			Message:   toolMsg,
			Execution: &session.ExecutionResult{
				ExecutionID: execID,
				Outcome:     session.OutcomeComplete,
				Coverage:    session.CoverageComplete,
				PostRef:     "ws:sha256:1111111111111111111111111111111111111111111111111111111111111111",
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		rLive.appendRow(tSeq, toolMsg)
		if err := rLive.refreshGroupMutation(ctx, store, sid, aSeq); err != nil {
			t.Fatal(err)
		}
	}

	// Step 41: assistant proposes completion, verification gate rejects with OriginCompletionBlocked.
	if _, err := rLive.groups(maxContext); err != nil {
		t.Fatal(err)
	}
	req41 := "req-41"
	first41 := rLive.transcript[0].Seq
	last41 := rLive.transcript[len(rLive.transcript)-1].Seq
	if _, err := store.AdmitRequest(ctx, session.RequestAdmission{
		RequestID:         req41,
		SessionID:         sid,
		RunID:             run1,
		Purpose:           session.PurposeMain,
		Step:              41,
		FirstSeq:          &first41,
		LastSeq:           &last41,
		ProjectionVersion: 2,
		Refs:              []string{"model:p/m-32k", "proj:mode:compact", "ts:v0"},
	}); err != nil {
		t.Fatal(err)
	}
	propMsg := protocol.Message{Role: "assistant", Content: "All tests fixed."}
	propSeq, err := store.AppendAnswered(ctx, sid, req41, propMsg)
	if err != nil {
		t.Fatal(err)
	}
	rLive.appendAssistant(propSeq, propMsg, "p/m-32k", protocol.Echo{})

	blockMsg := protocol.Message{Role: "user", Origin: protocol.OriginCompletionBlocked, Content: "Verification failed: TestFoo still failing"}
	blockSeq, _, err := store.AppendMessage(ctx, sid, blockMsg, nil)
	if err != nil {
		t.Fatal(err)
	}
	rLive.appendRow(blockSeq, blockMsg)

	// Derive live repair context at Step 42 via HistoryGroupMeta.
	liveRepair, err := rLive.deriveRepairContext(ctx, store, sid)
	if err != nil {
		t.Fatal(err)
	}

	// Close SQLite completely and reopen a brand-new cold Resume store & run from disk!
	if err := rawStore.Close(); err != nil {
		t.Fatal(err)
	}
	rawStore = nil

	reopened, err := session.Open(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if _, err := reopened.Reconcile(ctx, sid); err != nil {
		t.Fatal(err)
	}
	if _, _, err := reopened.BeginRun(ctx, sid, session.RunStart{RunID: "run-resume-2"}); err != nil {
		t.Fatal(err)
	}

	resumeStore := realSQLiteAgentStore{s: reopened}
	rResume := &run{}
	if err := rResume.load(ctx, resumeStore, sid, nil, maxContext); err != nil {
		t.Fatal(err)
	}
	resumeRepair, err := rResume.deriveRepairContext(ctx, resumeStore, sid)
	if err != nil {
		t.Fatal(err)
	}
	resumeGroups, err := rResume.groups(maxContext)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Live MinRequiredSeq MUST equal firstBashSeq AND MUST equal cold SQLite Resume MinRequiredSeq!
	if liveRepair.MinRequiredSeq != firstBashSeq {
		t.Fatalf("F-02 DRIFT: live MinRequiredSeq = %d (drifted due to maxContext body trimming!), want firstBashSeq = %d (cold Resume MinRequiredSeq = %d)",
			liveRepair.MinRequiredSeq, firstBashSeq, resumeRepair.MinRequiredSeq)
	}
	if liveRepair.MinRequiredSeq != resumeRepair.MinRequiredSeq || liveRepair.BlockSeq != resumeRepair.BlockSeq || liveRepair.NewestIsBlock != resumeRepair.NewestIsBlock {
		t.Fatalf("F-02 MISMATCH: live repair %+v != cold SQLite Resume repair %+v", liveRepair, resumeRepair)
	}

	// 2. Full-body rResume.transcript and rLive.transcript MUST stay bounded by maxContext (NOT retaining all 42 groups in RAM!).
	bodyTokens := 0
	for _, g := range resumeGroups {
		bodyTokens += window.GroupTokens(g)
	}
	if len(resumeGroups) >= 35 {
		t.Fatalf("F-02 UNBOUNDED BODY RETENTION: rResume retained %d full-body groups (%d tokens > maxContext %d)",
			len(resumeGroups), bodyTokens, maxContext)
	}
	if len(rLive.transcript) != len(rResume.transcript) || rLive.transcript[0].Seq != rResume.transcript[0].Seq {
		t.Fatalf("live vs resume bounded body window mismatch: live len=%d first=%d vs resume len=%d first=%d",
			len(rLive.transcript), rLive.transcript[0].Seq, len(rResume.transcript), rResume.transcript[0].Seq)
	}
}

// TestF02MultiToolCallAttributionAndUserResetOnRealSQLite verifies multi-tool-call metadata
// attribution (ToolCallID, ExecutionID, Unknown veto, Proven aggregation) and reset of old repair
// cycles upon new external user input ("", OriginImported) against a real SQLite database.
func TestF02MultiToolCallAttributionAndUserResetOnRealSQLite(t *testing.T) {
	ctx := context.Background()
	dbDir := filepath.Join(t.TempDir(), "state-multi")
	rawStore, err := session.Open(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rawStore.Close() }()

	const sid = "s-f02-multi"
	if err := rawStore.Create(ctx, session.Session{ID: sid, Goal: "multi tool call test", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := rawStore.BeginRun(ctx, sid, session.RunStart{
		RunID: "run-m1",
		Input: &protocol.Message{Role: "user", Content: "turn 1"},
	}); err != nil {
		t.Fatal(err)
	}
	store := realSQLiteAgentStore{s: rawStore}
	r := &run{}
	if err := r.load(ctx, store, sid, nil, 16384); err != nil {
		t.Fatal(err)
	}

	// Step 1: assistant issues [read_file, write_file(;cm=proven), bash] -> Unknown veto!
	f1, l1 := r.transcript[0].Seq, r.transcript[len(r.transcript)-1].Seq
	if _, err := store.AdmitRequest(ctx, session.RequestAdmission{
		RequestID: "req-m1", SessionID: sid, RunID: "run-m1", Purpose: session.PurposeMain,
		Step: 1, FirstSeq: &f1, LastSeq: &l1, ProjectionVersion: 2,
		Refs: []string{"model:p/m", "proj:mode:full", "ts:v0"},
	}); err != nil {
		t.Fatal(err)
	}
	asst1 := protocol.Message{
		Role: "assistant",
		ToolCalls: []protocol.ToolCall{
			{ID: "m1-read", Name: "read_file", Arguments: json.RawMessage(`{"path":"a.go"}`)},
			{ID: "m1-write", Name: "write_file", Arguments: json.RawMessage(`{"path":"a.go","content":"v2"}`)},
			{ID: "m1-bash", Name: "bash", Arguments: json.RawMessage(`{"command":"go test"}`)},
		},
	}
	seqA1, err := store.AppendAnswered(ctx, sid, "req-m1", asst1)
	if err != nil {
		t.Fatal(err)
	}
	r.appendAssistant(seqA1, asst1, "p/m", protocol.Echo{})

	// Commit read_file (no execution row), write_file (;cm=proven), and bash (ws:...).
	tReadSeq, _, err := store.CommitToolResult(ctx, ToolResultCommit{
		SessionID: sid, RunID: "run-m1",
		Message: protocol.Message{Role: "tool", ToolCallID: "m1-read", Name: "read_file", Content: "v1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	r.appendRow(tReadSeq, protocol.Message{Role: "tool", ToolCallID: "m1-read", Name: "read_file", Content: "v1"})

	const shaA = "1111111111111111111111111111111111111111111111111111111111111111"
	const shaB = "2222222222222222222222222222222222222222222222222222222222222222"
	if err := store.AdmitExecution(ctx, session.ExecutionAdmission{
		ExecutionID: "ex-m1-write", SessionID: sid, RunID: "run-m1", RequestID: "req-m1",
		ToolName: "write_file", ArgsDigest: "sha256:" + shaA, PreRef: "files:1:sha256:" + shaA,
	}); err != nil {
		t.Fatal(err)
	}
	tWriteSeq, _, err := store.CommitToolResult(ctx, ToolResultCommit{
		SessionID: sid, RunID: "run-m1",
		Message: protocol.Message{Role: "tool", ToolCallID: "m1-write", Name: "write_file", Content: "ok"},
		Execution: &session.ExecutionResult{
			ExecutionID: "ex-m1-write", Outcome: session.OutcomeComplete, Coverage: session.CoverageComplete,
			PostRef: "files:1:sha256:" + shaB + ";cm=proven",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	r.appendRow(tWriteSeq, protocol.Message{Role: "tool", ToolCallID: "m1-write", Name: "write_file", Content: "ok"})

	if err := store.AdmitExecution(ctx, session.ExecutionAdmission{
		ExecutionID: "ex-m1-bash", SessionID: sid, RunID: "run-m1", RequestID: "req-m1",
		ToolName: "bash", ArgsDigest: "sha256:" + shaA, PreRef: "ws:sha256:" + shaA,
	}); err != nil {
		t.Fatal(err)
	}
	tBashSeq, _, err := store.CommitToolResult(ctx, ToolResultCommit{
		SessionID: sid, RunID: "run-m1",
		Message: protocol.Message{Role: "tool", ToolCallID: "m1-bash", Name: "bash", Content: "fail"},
		Execution: &session.ExecutionResult{
			ExecutionID: "ex-m1-bash", Outcome: session.OutcomeComplete, Coverage: session.CoverageComplete,
			PostRef: "ws:sha256:" + shaA,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	r.appendRow(tBashSeq, protocol.Message{Role: "tool", ToolCallID: "m1-bash", Name: "bash", Content: "fail"})
	if err := r.refreshGroupMutation(ctx, store, sid, seqA1); err != nil {
		t.Fatal(err)
	}

	// Verify HistoryGroupMeta fields: HasToolCalls, AnsweredSeq, ToolCallID, ExecutionID, ToolCalls, MutationUnknown.
	meta1 := r.historyGroups[len(r.historyGroups)-1]
	if !meta1.HasToolCalls || meta1.AnsweredSeq != seqA1 || meta1.ToolCallID != "m1-read" || meta1.ExecutionID != "ex-m1-write" {
		t.Fatalf("unexpected multi-call HistoryGroupMeta: %+v", meta1)
	}
	if len(meta1.ToolCalls) != 3 || meta1.ToolCalls[1].ExecutionID != "ex-m1-write" || meta1.ToolCalls[2].ExecutionID != "ex-m1-bash" {
		t.Fatalf("unexpected per-call ExecutionID attribution: %+v", meta1.ToolCalls)
	}
	if meta1.MutationEvidence != MutationUnknown {
		t.Fatalf("expected Unknown veto on [read, write(proven), bash], got %q", meta1.MutationEvidence)
	}

	// Proposal + Gate Block -> active repair floor at seqA1.
	propSeq, _, err := store.AppendMessage(ctx, sid, protocol.Message{Role: "assistant", Content: "done"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.appendAssistant(propSeq, protocol.Message{Role: "assistant", Content: "done"}, "p/m", protocol.Echo{})
	blkSeq, _, err := store.AppendMessage(ctx, sid, protocol.Message{Role: "user", Origin: protocol.OriginCompletionBlocked, Content: "blocked"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.appendRow(blkSeq, protocol.Message{Role: "user", Origin: protocol.OriginCompletionBlocked, Content: "blocked"})

	rep1, err := r.deriveRepairContext(ctx, store, sid)
	if err != nil || !rep1.Active || rep1.MinRequiredSeq != seqA1 {
		t.Fatalf("expected active repair at seqA1=%d, got %+v (err=%v)", seqA1, rep1, err)
	}

	// New external user input resets the prior repair cycle in both live and cold SQLite Resume!
	u2Seq, _, err := store.AppendMessage(ctx, sid, protocol.Message{Role: "user", Content: "new user request"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.appendRow(u2Seq, protocol.Message{Role: "user", Content: "new user request"})
	repAfterU2, err := r.deriveRepairContext(ctx, store, sid)
	if err != nil || repAfterU2.Active || repAfterU2.MinRequiredSeq != -1 {
		t.Fatalf("expected external user turn to reset repair context, got %+v (err=%v)", repAfterU2, err)
	}
	if len(r.historyGroups) != 1 || r.historyGroups[0].Seq != u2Seq {
		t.Fatalf("expected historyGroups trimmed to new user turn [%d], got %+v", u2Seq, r.historyGroups)
	}
}

// TestF02HistoryOverflowStreamsFromRealSQLite verifies that when a single turn exceeds
// MaxInMemoryHistoryGroups (2048), historyGroups does not grow unboundedly and deriveRepairContext
// streams deterministically from real SQLite via EachMessageFrom + ListAnsweredExecutions.
func TestF02HistoryOverflowStreamsFromRealSQLite(t *testing.T) {
	ctx := context.Background()
	dbDir := filepath.Join(t.TempDir(), "state-overflow")
	rawStore, err := session.Open(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rawStore.Close() }()

	const sid = "s-f02-stream"
	if err := rawStore.Create(ctx, session.Session{ID: sid, Goal: "2100 groups stream test", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := rawStore.BeginRun(ctx, sid, session.RunStart{
		RunID: "run-s1",
		Input: &protocol.Message{Role: "user", Content: "long turn"},
	}); err != nil {
		t.Fatal(err)
	}
	store := realSQLiteAgentStore{s: rawStore}
	rLive := &run{}
	if err := rLive.load(ctx, store, sid, nil, 8192); err != nil {
		t.Fatal(err)
	}

	// Step 1: one admitted bash call establishing the true repair floor at firstMutSeq.
	f0, l0 := rLive.transcript[0].Seq, rLive.transcript[len(rLive.transcript)-1].Seq
	if _, err := store.AdmitRequest(ctx, session.RequestAdmission{
		RequestID: "req-s0", SessionID: sid, RunID: "run-s1", Purpose: session.PurposeMain,
		Step: 1, FirstSeq: &f0, LastSeq: &l0, ProjectionVersion: 2,
		Refs: []string{"model:p/m", "proj:mode:compact", "ts:v0"},
	}); err != nil {
		t.Fatal(err)
	}
	asst0 := protocol.Message{
		Role:      "assistant",
		ToolCalls: []protocol.ToolCall{{ID: "s-tc-0", Name: "bash", Arguments: json.RawMessage(`{"command":"true"}`)}},
	}
	firstMutSeq, err := store.AppendAnswered(ctx, sid, "req-s0", asst0)
	if err != nil {
		t.Fatal(err)
	}
	rLive.appendAssistant(firstMutSeq, asst0, "p/m", protocol.Echo{})
	const sha = "3333333333333333333333333333333333333333333333333333333333333333"
	if err := store.AdmitExecution(ctx, session.ExecutionAdmission{
		ExecutionID: "ex-s0", SessionID: sid, RunID: "run-s1", RequestID: "req-s0",
		ToolName: "bash", ArgsDigest: "sha256:" + sha, PreRef: "ws:sha256:" + sha,
	}); err != nil {
		t.Fatal(err)
	}
	t0Seq, _, err := store.CommitToolResult(ctx, ToolResultCommit{
		SessionID: sid, RunID: "run-s1",
		Message: protocol.Message{Role: "tool", ToolCallID: "s-tc-0", Name: "bash", Content: "ok"},
		Execution: &session.ExecutionResult{
			ExecutionID: "ex-s0", Outcome: session.OutcomeComplete, Coverage: session.CoverageComplete,
			PostRef: "ws:sha256:" + sha,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	rLive.appendRow(t0Seq, protocol.Message{Role: "tool", ToolCallID: "s-tc-0", Name: "bash", Content: "ok"})
	if err := rLive.refreshGroupMutation(ctx, store, sid, firstMutSeq); err != nil {
		t.Fatal(err)
	}

	// Append 2,100 lightweight read_file groups (> MaxInMemoryHistoryGroups = 2048) to trigger SQLite streaming!
	for i := 1; i <= MaxInMemoryHistoryGroups+52; i++ {
		tcID := fmt.Sprintf("rf-%d", i)
		asst := protocol.Message{
			Role:      "assistant",
			ToolCalls: []protocol.ToolCall{{ID: tcID, Name: "read_file", Arguments: json.RawMessage(`{"path":"main.go"}`)}},
		}
		aSeq, _, err := store.AppendMessage(ctx, sid, asst, nil)
		if err != nil {
			t.Fatal(err)
		}
		rLive.appendAssistant(aSeq, asst, "p/m", protocol.Echo{})
		tMsg := protocol.Message{Role: "tool", ToolCallID: tcID, Name: "read_file", Content: "package main"}
		tSeq, _, err := store.AppendMessage(ctx, sid, tMsg, nil)
		if err != nil {
			t.Fatal(err)
		}
		rLive.appendRow(tSeq, tMsg)
		if i%128 == 0 {
			if _, err := rLive.groups(8192); err != nil {
				t.Fatal(err)
			}
		}
	}

	pSeq, _, err := store.AppendMessage(ctx, sid, protocol.Message{Role: "assistant", Content: "finished"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rLive.appendAssistant(pSeq, protocol.Message{Role: "assistant", Content: "finished"}, "p/m", protocol.Echo{})
	bSeq, _, err := store.AppendMessage(ctx, sid, protocol.Message{Role: "user", Origin: protocol.OriginCompletionBlocked, Content: "gate failed"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rLive.appendRow(bSeq, protocol.Message{Role: "user", Origin: protocol.OriginCompletionBlocked, Content: "gate failed"})

	if !rLive.historyOverflowed || len(rLive.historyGroups) != 0 {
		t.Fatalf("expected historyOverflowed=true and nil slice past %d groups, got overflow=%v len=%d",
			MaxInMemoryHistoryGroups, rLive.historyOverflowed, len(rLive.historyGroups))
	}

	liveRep, err := rLive.deriveRepairContext(ctx, store, sid)
	if err != nil {
		t.Fatal(err)
	}
	if !liveRep.Active || liveRep.MinRequiredSeq != firstMutSeq || liveRep.BlockSeq != bSeq {
		t.Fatalf("streaming live deriveRepairContext = %+v, want MinRequiredSeq=%d BlockSeq=%d", liveRep, firstMutSeq, bSeq)
	}

	rCold := &run{}
	if err := rCold.load(ctx, store, sid, nil, 8192); err != nil {
		t.Fatal(err)
	}
	coldRep, err := rCold.deriveRepairContext(ctx, store, sid)
	if err != nil {
		t.Fatal(err)
	}
	if coldRep != liveRep {
		t.Fatalf("cold streaming Resume repair %+v != live streaming repair %+v", coldRep, liveRep)
	}
}
