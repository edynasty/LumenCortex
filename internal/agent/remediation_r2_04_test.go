package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/protocol"
)

type countingAgentStore struct {
	Store
	transcriptScans atomic.Int64
	transcriptRows  atomic.Int64
	execListQueries atomic.Int64
}

func (c *countingAgentStore) resetCounts() {
	c.transcriptScans.Store(0)
	c.transcriptRows.Store(0)
	c.execListQueries.Store(0)
}

func (c *countingAgentStore) Transcript(ctx context.Context, sessionID string, fromSeq int64, fn func(seq int64, msg protocol.Message) error) error {
	c.transcriptScans.Add(1)
	return c.Store.Transcript(ctx, sessionID, fromSeq, func(seq int64, msg protocol.Message) error {
		c.transcriptRows.Add(1)
		return fn(seq, msg)
	})
}

func (c *countingAgentStore) ListAnsweredExecutions(ctx context.Context, sessionID string, fromSeq int64) ([]session.AnsweredExecutionSummary, error) {
	c.execListQueries.Add(1)
	return c.Store.ListAnsweredExecutions(ctx, sessionID, fromSeq)
}

func seedR204RangeOnRealSQLite(
	t *testing.T,
	ctx context.Context,
	st Store,
	sid string,
	rLive *run,
	allGroups *[]HistoryGroupMeta,
	fromGroup int,
	toGroup int,
	body string,
	maxCtx int,
) {
	t.Helper()
	const (
		shaA = "1111111111111111111111111111111111111111111111111111111111111111"
		shaB = "2222222222222222222222222222222222222222222222222222222222222222"
	)
	for g := fromGroup; g <= toGroup; g++ {
		switch {
		case g == 5 || g == 2048 || g == 6001:
			// Admitted bash or write_file causal step at critical boundaries (pre-fold, exact 2048 cliff, post-6000).
			lastSeq := int64(0)
			if len(rLive.transcript) > 0 {
				lastSeq = rLive.transcript[len(rLive.transcript)-1].Seq
			}
			f0, l0 := int64(0), lastSeq
			reqID := fmt.Sprintf("r204-req-%d", g)
			if _, err := st.AdmitRequest(ctx, session.RequestAdmission{
				RequestID: reqID, SessionID: sid, RunID: "run-r204", Purpose: session.PurposeMain,
				Step: g, FirstSeq: &f0, LastSeq: &l0, ProjectionVersion: 2,
				Refs: []string{"model:p/m", "proj:mode:compact", "ts:v0"},
			}); err != nil {
				t.Fatal(err)
			}
			toolName := "bash"
			argsRaw := json.RawMessage(`{"command":"make test"}`)
			preRef := "ws:sha256:" + shaA
			postRef := "ws:sha256:" + shaA
			if g == 6001 {
				toolName = "write_file"
				argsRaw = json.RawMessage(`{"path":"fix.go","content":"package p"}`)
				preRef = "files:1:sha256:" + shaA
				postRef = "files:1:sha256:" + shaB + ";cm=proven"
			}
			tcID := fmt.Sprintf("c-%d", g)
			asst := protocol.Message{
				Role:      "assistant",
				ToolCalls: []protocol.ToolCall{{ID: tcID, Name: toolName, Arguments: argsRaw}},
			}
			aSeq, err := st.AppendAnswered(ctx, sid, reqID, asst)
			if err != nil {
				t.Fatal(err)
			}
			exID := fmt.Sprintf("r204-ex-%d", g)
			if err := st.AdmitExecution(ctx, session.ExecutionAdmission{
				ExecutionID: exID, SessionID: sid, RunID: "run-r204", RequestID: reqID,
				ToolCallID: tcID,
				ToolName:   toolName, ArgsDigest: "sha256:" + shaA, PreRef: preRef,
			}); err != nil {
				t.Fatal(err)
			}
			tMsg := protocol.Message{Role: "tool", ToolCallID: tcID, Name: toolName, Content: body}
			tSeq, _, err := st.CommitToolResult(ctx, ToolResultCommit{
				SessionID: sid, RunID: "run-r204", Message: tMsg,
				Execution: &session.ExecutionResult{
					ExecutionID: exID, Outcome: session.OutcomeComplete, Coverage: session.CoverageComplete, PostRef: postRef,
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			rLive.appendAssistant(aSeq, asst, "p/m", protocol.Echo{})
			rLive.appendRow(tSeq, tMsg)
			if err := rLive.refreshGroupMutation(ctx, st, sid, aSeq); err != nil {
				t.Fatal(err)
			}
			ev := MutationUnknown
			if g == 6001 {
				ev = MutationProven
			}
			meta := buildAssistantHistoryMeta(aSeq, asst)
			applyToolRowToHistoryMeta(&meta, tSeq, tMsg)
			meta.ExecutionID = exID
			meta.PrimaryPostRef = postRef
			meta.ToolCalls[0].ExecutionID = exID
			meta.MutationEvidence = ev
			*allGroups = append(*allGroups, meta)

		case g == 1500:
			// Completion-gate refusal group (proposal + OriginCompletionBlocked) at group 1500-1501.
			prop := protocol.Message{Role: "assistant", Content: " proposing completion "}
			pSeq, _, err := st.AppendMessage(ctx, sid, prop, nil)
			if err != nil {
				t.Fatal(err)
			}
			rLive.appendAssistant(pSeq, prop, "p/m", protocol.Echo{})
			*allGroups = append(*allGroups, buildAssistantHistoryMeta(pSeq, prop))

			g++
			blk := protocol.Message{
				Role:    "user",
				Origin:  protocol.OriginCompletionBlocked,
				Content: "Completion refused at gate #1: failing check `go test ./internal/agent`.",
			}
			bSeq, _, err := st.AppendMessage(ctx, sid, blk, nil)
			if err != nil {
				t.Fatal(err)
			}
			rLive.appendRow(bSeq, blk)
			*allGroups = append(*allGroups, HistoryGroupMeta{
				Seq: bSeq, LastSeq: bSeq, Role: "user", Origin: protocol.OriginCompletionBlocked,
				AnsweredSeq: -1, GateFeedback: blk.Content, MutationEvidence: MutationNone,
			})

		default:
			tcID := fmt.Sprintf("rf-%d", g)
			asst := protocol.Message{
				Role:      "assistant",
				ToolCalls: []protocol.ToolCall{{ID: tcID, Name: "read_file", Arguments: json.RawMessage(`{"path":"pkg/file.go"}`)}},
			}
			aSeq, _, err := st.AppendMessage(ctx, sid, asst, nil)
			if err != nil {
				t.Fatal(err)
			}
			tMsg := protocol.Message{Role: "tool", ToolCallID: tcID, Name: "read_file", Content: body}
			tSeq, _, err := st.AppendMessage(ctx, sid, tMsg, nil)
			if err != nil {
				t.Fatal(err)
			}
			rLive.appendAssistant(aSeq, asst, "p/m", protocol.Echo{})
			rLive.appendRow(tSeq, tMsg)
			meta := buildAssistantHistoryMeta(aSeq, asst)
			applyToolRowToHistoryMeta(&meta, tSeq, tMsg)
			*allGroups = append(*allGroups, meta)
		}

		if g%64 == 0 {
			if _, err := rLive.groups(maxCtx); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := rLive.groups(maxCtx); err != nil {
		t.Fatal(err)
	}
}

// TestR204MeasureProfile captures quantitative wall-clock time, allocation bytes, live heap bytes,
// and exact SQLite query/row counts at 2048, 2049, 6000, and 12000 groups (3,000B body per group)
// on a real SQLite database across both 10 simulated 3-candidate steps (40 deriveRepairContext calls)
// and cold SQLite Resume load.
func TestR204MeasureProfile(t *testing.T) {
	ctx := context.Background()
	dbDir := filepath.Join(t.TempDir(), "r204-profile.db")
	rawStore, err := session.Open(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rawStore.Close() }()

	const (
		sid    = "s-r204-profile"
		maxCtx = 32768
	)
	if err := rawStore.Create(ctx, session.Session{ID: sid, Goal: "R2-04 scaling profile", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := rawStore.BeginRun(ctx, sid, session.RunStart{
		RunID: "run-r204",
		Input: &protocol.Message{Role: "user", Content: "long multi-thousand-step turn"},
	}); err != nil {
		t.Fatal(err)
	}

	baseStore := realSQLiteAgentStore{s: rawStore}
	cStore := &countingAgentStore{Store: baseStore}

	rLive := &run{}
	if err := rLive.load(ctx, baseStore, sid, nil, maxCtx); err != nil {
		t.Fatal(err)
	}
	allGroups := []HistoryGroupMeta{{
		Seq: rLive.transcript[0].Seq, LastSeq: rLive.transcript[0].Seq,
		Role: "user", AnsweredSeq: -1, MutationEvidence: MutationNone,
	}}

	body := strings.Repeat("x", 3000)
	checkpoints := []int{2048, 2049, 6000, 12000}
	nextGroup := 2 // group 1 is the initial user prompt

	t.Logf("R2-04_PROFILE_HEADER\tgroups\tsingle_derive_us\tstep40_calls_ms\tstep40_alloc_bytes\tlive_heap_bytes\tstep40_tx_scans\tstep40_tx_rows\tstep40_exec_queries\tcold_resume_ms\thistory_slice_len\ttranscript_entries")

	for _, target := range checkpoints {
		seedR204RangeOnRealSQLite(t, ctx, baseStore, sid, rLive, &allGroups, nextGroup, target, body, maxCtx)
		nextGroup = target + 1

		runtime.GC()
		var msBefore, msAfter runtime.MemStats
		runtime.ReadMemStats(&msBefore)

		t0 := time.Now()
		repSingle, err := rLive.deriveRepairContext(ctx, cStore, sid)
		if err != nil {
			t.Fatal(err)
		}
		singleUs := time.Since(t0).Microseconds()

		// Measure 10 steps x 4 deriveRepairContext calls per step (Loop.Run + 3 candidate evaluations).
		cStore.resetCounts()
		runtime.ReadMemStats(&msBefore)
		tBatch := time.Now()
		for callIdx := 0; callIdx < 40; callIdx++ {
			rep, err := rLive.deriveRepairContext(ctx, cStore, sid)
			if err != nil {
				t.Fatal(err)
			}
			if rep != repSingle {
				t.Fatalf("non-deterministic deriveRepairContext at %d groups", target)
			}
		}
		batchMs := float64(time.Since(tBatch).Microseconds()) / 1000.0
		runtime.ReadMemStats(&msAfter)
		allocDelta := msAfter.TotalAlloc - msBefore.TotalAlloc

		txScans := cStore.transcriptScans.Load()
		txRows := cStore.transcriptRows.Load()
		execQueries := cStore.execListQueries.Load()

		// Measure cold SQLite load + deriveRepairContext parity.
		tCold := time.Now()
		rCold := &run{}
		if err := rCold.load(ctx, baseStore, sid, nil, maxCtx); err != nil {
			t.Fatal(err)
		}
		repCold, err := rCold.deriveRepairContext(ctx, baseStore, sid)
		if err != nil {
			t.Fatal(err)
		}
		coldMs := float64(time.Since(tCold).Microseconds()) / 1000.0
		if repCold != repSingle {
			t.Fatalf("at %d groups: coldRep != liveRep:\ncold=%+v\nlive=%+v", target, repCold, repSingle)
		}

		runtime.GC()
		var msHeap runtime.MemStats
		runtime.ReadMemStats(&msHeap)

		t.Logf("R2-04_PROFILE_ROW\t%d\t%d\t%.3f\t%d\t%d\t%d\t%d\t%d\t%.3f\t%d\t%d",
			len(allGroups), singleUs, batchMs, allocDelta, msHeap.HeapAlloc,
			txScans, txRows, execQueries, coldMs, len(rLive.historyGroups), len(rLive.transcript))
	}
}

// TestR204IncrementalBaseSMParityAndZeroPerStepSQLiteScans enforces that:
//  1. Past MaxInMemoryHistoryGroups (2049, 6000, 12000 groups), live deriveRepairContext AND post-load
//     cold deriveRepairContext execute ZERO SQLite Transcript or ListAnsweredExecutions queries per call;
//  2. In-memory HistoryGroupMeta stays strictly bounded (<= MaxInMemoryHistoryGroups = 2048);
//  3. Live incremental baseSM, cold SQLite Resume, reference full-history DeriveRepairContextFromHistory,
//     and explicit SQLite streaming reconstruction produce 100% identical RepairContext structs across
//     2047, 2048, 2049, 2050, 6000, and 12000 groups, as well as after a new external user turn post-overflow.
func TestR204IncrementalBaseSMParityAndZeroPerStepSQLiteScans(t *testing.T) {
	ctx := context.Background()
	dbDir := filepath.Join(t.TempDir(), "r204-parity.db")
	rawStore, err := session.Open(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rawStore.Close() }()

	const (
		sid    = "s-r204-parity"
		maxCtx = 16384
	)
	if err := rawStore.Create(ctx, session.Session{ID: sid, Goal: "R2-04 baseSM parity", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := rawStore.BeginRun(ctx, sid, session.RunStart{
		RunID: "run-r204",
		Input: &protocol.Message{Role: "user", Content: "initial turn"},
	}); err != nil {
		t.Fatal(err)
	}
	baseStore := realSQLiteAgentStore{s: rawStore}
	cStore := &countingAgentStore{Store: baseStore}

	rLive := &run{}
	if err := rLive.load(ctx, baseStore, sid, nil, maxCtx); err != nil {
		t.Fatal(err)
	}
	allGroups := []HistoryGroupMeta{{
		Seq: rLive.transcript[0].Seq, LastSeq: rLive.transcript[0].Seq,
		Role: "user", AnsweredSeq: -1, MutationEvidence: MutationNone,
	}}

	body := strings.Repeat("z", 1024)
	nextGroup := 2
	for _, target := range []int{2047, 2048, 2049, 2050, 6000, 12000} {
		seedR204RangeOnRealSQLite(t, ctx, baseStore, sid, rLive, &allGroups, nextGroup, target, body, maxCtx)
		nextGroup = target + 1

		if len(rLive.historyGroups) > MaxInMemoryHistoryGroups {
			t.Fatalf("at %d groups: len(historyGroups)=%d exceeds MaxInMemoryHistoryGroups=%d",
				target, len(rLive.historyGroups), MaxInMemoryHistoryGroups)
		}

		cStore.resetCounts()
		for k := 0; k < 4; k++ {
			liveRep, err := rLive.deriveRepairContext(ctx, cStore, sid)
			if err != nil {
				t.Fatal(err)
			}
			wantRep := DeriveRepairContextFromHistory(allGroups)
			if liveRep != wantRep {
				t.Fatalf("at %d groups: liveRep != reference DeriveRepairContextFromHistory:\nlive=%+v\nwant=%+v",
					target, liveRep, wantRep)
			}
		}
		if scans, execQ := cStore.transcriptScans.Load(), cStore.execListQueries.Load(); scans != 0 || execQ != 0 {
			t.Fatalf("R2-04 O(n^2) REGRESSION at %d groups: 4 deriveRepairContext calls executed %d SQLite Transcript scans (%d rows) and %d ListAnsweredExecutions queries, want 0!",
				target, scans, cStore.transcriptRows.Load(), execQ)
		}

		// Cold Resume load uses at most 1 Transcript scan + 1 ListAnsweredExecutions query, then 0 on subsequent calls.
		cStore.resetCounts()
		rCold := &run{}
		if err := rCold.load(ctx, cStore, sid, nil, maxCtx); err != nil {
			t.Fatal(err)
		}
		if loadScans := cStore.transcriptScans.Load(); loadScans != 1 {
			t.Fatalf("at %d groups: cold load Transcript scans=%d, want 1", target, loadScans)
		}
		cStore.resetCounts()
		coldRep, err := rCold.deriveRepairContext(ctx, cStore, sid)
		if err != nil {
			t.Fatal(err)
		}
		if scans, execQ := cStore.transcriptScans.Load(), cStore.execListQueries.Load(); scans != 0 || execQ != 0 {
			t.Fatalf("at %d groups: post-load cold deriveRepairContext executed %d Transcript scans and %d exec queries, want 0!",
				target, scans, execQ)
		}
		wantRep := DeriveRepairContextFromHistory(allGroups)
		if coldRep != wantRep {
			t.Fatalf("at %d groups: coldRep != reference:\ncold=%+v\nwant=%+v", target, coldRep, wantRep)
		}
		if target == 12000 {
			streamRep, err := StreamRepairContextFromStore(ctx, baseStore, sid, rLive.latestUserSeq)
			if err != nil {
				t.Fatal(err)
			}
			if streamRep != wantRep {
				t.Fatalf("at 12000 groups: StreamRepairContextFromStore != reference:\nstream=%+v\nwant=%+v", streamRep, wantRep)
			}
		}
	}

	// Verify external user message after 12,000-group overflow cleanly resets baseSM + historyGroups
	// AND stays 100% equivalent on a subsequent gate refusal in the new turn!
	uMsg := protocol.Message{Role: "user", Content: "fresh external user turn after 12000 groups"}
	uSeq, _, err := baseStore.AppendMessage(ctx, sid, uMsg, nil)
	if err != nil {
		t.Fatal(err)
	}
	rLive.appendRow(uSeq, uMsg)
	activeTurnGroups := []HistoryGroupMeta{{
		Seq: uSeq, LastSeq: uSeq, Role: "user", AnsweredSeq: -1, MutationEvidence: MutationNone,
	}}
	cStore.resetCounts()
	postResetRep, err := rLive.deriveRepairContext(ctx, cStore, sid)
	if err != nil {
		t.Fatal(err)
	}
	if postResetRep != DeriveRepairContextFromHistory(activeTurnGroups) || postResetRep.Active {
		t.Fatalf("external user turn after overflow failed to reset repair context: got %+v", postResetRep)
	}

	pMsg := protocol.Message{Role: "assistant", Content: "quick done"}
	pSeq, _, err := baseStore.AppendMessage(ctx, sid, pMsg, nil)
	if err != nil {
		t.Fatal(err)
	}
	rLive.appendAssistant(pSeq, pMsg, "p/m", protocol.Echo{})
	activeTurnGroups = append(activeTurnGroups, buildAssistantHistoryMeta(pSeq, pMsg))

	bMsg := protocol.Message{Role: "user", Origin: protocol.OriginCompletionBlocked, Content: "new turn gate check failed"}
	bSeq, _, err := baseStore.AppendMessage(ctx, sid, bMsg, nil)
	if err != nil {
		t.Fatal(err)
	}
	rLive.appendRow(bSeq, bMsg)
	activeTurnGroups = append(activeTurnGroups, HistoryGroupMeta{
		Seq: bSeq, LastSeq: bSeq, Role: "user", Origin: protocol.OriginCompletionBlocked,
		AnsweredSeq: -1, GateFeedback: bMsg.Content, MutationEvidence: MutationNone,
	})

	newTurnRep, err := rLive.deriveRepairContext(ctx, cStore, sid)
	if err != nil {
		t.Fatal(err)
	}
	rColdAfterReset := &run{}
	if err := rColdAfterReset.load(ctx, baseStore, sid, nil, maxCtx); err != nil {
		t.Fatal(err)
	}
	coldAfterResetRep, err := rColdAfterReset.deriveRepairContext(ctx, cStore, sid)
	if err != nil {
		t.Fatal(err)
	}
	wantAfterReset := DeriveRepairContextFromHistory(activeTurnGroups)
	if newTurnRep != wantAfterReset || coldAfterResetRep != wantAfterReset {
		t.Fatalf("post-reset new turn parity mismatch:\nlive=%+v\ncold=%+v\nwant=%+v",
			newTurnRep, coldAfterResetRep, wantAfterReset)
	}
}
