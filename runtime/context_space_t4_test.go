package runtime

import (
	"context"
	"database/sql"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/edynasty/LumenCortex/internal/agent"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/protocol"
)

// TestT4_MultiCallVetoCrashReconcileResumeAndMonotonicChain exercises ADR-0024 §6 T4 (RC34-F02 T7..T10).
func TestT4_MultiCallVetoCrashReconcileResumeAndMonotonicChain(t *testing.T) {
	// RC34-F02-T7: Multi-tool calls within the same answered_seq (per-ExecutionID receipt isolation & Unknown veto).
	t.Run("RC34_F02_T7_multi_call_isolation_and_unknown_veto", func(t *testing.T) {
		workspace := t.TempDir()
		gitInit(t, workspace)
		useGlobalConfig(t, mcpGlobalConfig(workspace))
		engine := openEngineAt(t, workspace)
		t.Cleanup(func() { agent.SetTestExecutionHooks(nil, nil) })

		runBatch := func(name string, setup func(), batch []protocol.ToolCall) (agent.MutationEvidence, int64, int64, []session.AnsweredExecutionSummary) {
			agent.SetTestExecutionHooks(nil, nil)
			if setup != nil {
				setup()
			}
			h := newTestSession(t, engine, "t7-"+name)
			prov := &stepProvider{steps: []protocol.ProviderResponse{
				toolCalls(tc("wu", "work_unit_create", `{"id":"u1","goal":"g"}`), tc("g0", "bash", `{"command":"true"}`)),
				{Message: protocol.Message{Content: "proposal"}, FinishReason: "stop"},
				toolCalls(batch...),
			}}
			_, _ = runWithB0(context.Background(), engine, h.ID, prov, AgentOptions{Policy: "full", MaxSteps: 3})
			agent.SetTestExecutionHooks(nil, nil)
			muts, rep, sums := inspectPersistedRepairState(t, engine, h.ID)
			g0Seq := sums[0].AnsweredSeq
			batchSeq := sums[len(sums)-1].AnsweredSeq
			var batchSums []session.AnsweredExecutionSummary
			for _, s := range sums {
				if s.AnsweredSeq == batchSeq {
					batchSums = append(batchSums, s)
				}
			}
			return muts[batchSeq], g0Seq, rep.MinRequiredSeq, batchSums
		}

		// ① [write_file(p1, B) Proven, write_file(p2, A) None, read_file(p3)] -> Proven (advances!)
		ev1, g01, min1, sums1 := runBatch("case1_proven_plus_none_plus_read", func() {
			_ = os.WriteFile(filepath.Join(workspace, "p1.txt"), []byte("A\n"), 0o644)
			_ = os.WriteFile(filepath.Join(workspace, "p2.txt"), []byte("A\n"), 0o644)
		}, []protocol.ToolCall{
			tc("c1", "write_file", `{"path":"p1.txt","content":"B\n"}`),
			tc("c2", "write_file", `{"path":"p2.txt","content":"A\n"}`),
			tc("c3", "read_file", `{"path":"p1.txt"}`),
		})
		if ev1 != agent.MutationProven || len(sums1) != 2 ||
			!strings.HasSuffix(sums1[0].PostRef, ";cm=proven") ||
			!strings.HasSuffix(sums1[1].PostRef, ";cm=none") ||
			min1 <= g01 {
			t.Fatalf("T7-1: ev=%s min=%d (g0=%d) sums=%+v", ev1, min1, g01, sums1)
		}

		// ② Same file twice in one step: [write_file(p1, B) Proven, write_file(p1, B) None] -> Proven!
		ev2, g02, min2, sums2 := runBatch("case2_same_file_proven_then_idempotent", func() {
			_ = os.WriteFile(filepath.Join(workspace, "p1.txt"), []byte("A\n"), 0o644)
		}, []protocol.ToolCall{
			tc("c1", "write_file", `{"path":"p1.txt","content":"B\n"}`),
			tc("c2", "write_file", `{"path":"p1.txt","content":"B\n"}`),
		})
		if ev2 != agent.MutationProven || len(sums2) != 2 ||
			!strings.HasSuffix(sums2[0].PostRef, ";cm=proven") ||
			!strings.HasSuffix(sums2[1].PostRef, ";cm=none") ||
			min2 <= g02 {
			t.Fatalf("T7-2: ev=%s min=%d (g0=%d) sums=%+v", ev2, min2, g02, sums2)
		}

		// ③ [write_file(p1, B) ;cm=proven, write_file(p2, B) where Host wrote B ;cm=unknown] -> Unknown veto!
		ev3, g03, min3, sums3 := runBatch("case3_proven_vetoed_by_host_noop_unknown", func() {
			_ = os.WriteFile(filepath.Join(workspace, "p1.txt"), []byte("A\n"), 0o644)
			_ = os.WriteFile(filepath.Join(workspace, "p2.txt"), []byte("A\n"), 0o644)
			agent.SetTestExecutionHooks(func(toolName string, targets []string) {
				if toolName == "write_file" && len(targets) == 1 && targets[0] == "p2.txt" {
					_ = os.WriteFile(filepath.Join(workspace, "p2.txt"), []byte("B\n"), 0o644)
				}
			}, nil)
		}, []protocol.ToolCall{
			tc("c1", "write_file", `{"path":"p1.txt","content":"B\n"}`),
			tc("c2", "write_file", `{"path":"p2.txt","content":"B\n"}`),
		})
		if ev3 != agent.MutationUnknown || len(sums3) != 2 ||
			!strings.HasSuffix(sums3[0].PostRef, ";cm=proven") ||
			!strings.HasSuffix(sums3[1].PostRef, ";cm=unknown") ||
			min3 != g03 {
			t.Fatalf("T7-3: ev=%s min=%d (want %d) sums=%+v", ev3, min3, g03, sums3)
		}

		// ④ [write_file(p1, B) ;cm=proven, bash("true")] and [write_file(p1, B) ;cm=proven, write_file("../escape", "x")] -> Unknown veto!
		ev4a, g04a, min4a, _ := runBatch("case4a_proven_vetoed_by_bash", func() {
			_ = os.WriteFile(filepath.Join(workspace, "p1.txt"), []byte("A\n"), 0o644)
		}, []protocol.ToolCall{
			tc("c1", "write_file", `{"path":"p1.txt","content":"B\n"}`),
			tc("c2", "bash", `{"command":"true"}`),
		})
		if ev4a != agent.MutationUnknown || min4a != g04a {
			t.Fatalf("T7-4a: ev=%s min=%d (want %d)", ev4a, min4a, g04a)
		}
		ev4b, g04b, min4b, _ := runBatch("case4b_proven_vetoed_by_escape", func() {
			_ = os.WriteFile(filepath.Join(workspace, "p1.txt"), []byte("A\n"), 0o644)
		}, []protocol.ToolCall{
			tc("c1", "write_file", `{"path":"p1.txt","content":"B\n"}`),
			tc("c2", "write_file", `{"path":"../escape.txt","content":"x"}`),
		})
		if ev4b != agent.MutationUnknown || min4b != g04b {
			t.Fatalf("T7-4b: ev=%s min=%d (want %d)", ev4b, min4b, g04b)
		}
	})

	// RC34-F02-T8: Crash AFTER Engine.Commit wrote B to disk and BEFORE CommitToolResult commits to SQLite,
	// followed by cold Reconcile + Resume (and nested settled callback followed by outer interruption).
	t.Run("RC34_F02_T8_crash_after_disk_write_before_CommitToolResult_reconciles_unknown", func(t *testing.T) {
		workspace := t.TempDir()
		gitInit(t, workspace)
		useGlobalConfig(t, mcpGlobalConfig(workspace))
		_ = os.WriteFile(filepath.Join(workspace, "crash.go"), []byte("package p // A\n"), 0o644)

		engine1, err := Open(Options{Workspace: workspace})
		if err != nil {
			t.Fatal(err)
		}
		h := newTestSession(t, engine1, "t8-crash")

		// Step 1: open work_unit + bash("true") (G0)
		// Step 2: premature proposal -> Block1
		// Step 3: write_file("crash.go", B) -> Engine.Commit writes B to disk, then panic/abort in testHookBeforeToolCommit
		// BEFORE s.store.CommitToolResult writes the tool row or settles the execution!
		var crashOnWrite atomic.Bool
		testHookBeforeToolCommit = func() {
			if crashOnWrite.Load() {
				panic("simulated crash after Engine.Commit before SQLite CommitToolResult")
			}
		}
		defer func() { testHookBeforeToolCommit = nil }()

		stepIdx := 0
		prov1 := &exitProvider{fn: func(_ context.Context, _ int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
			stepIdx++
			switch stepIdx {
			case 1:
				return toolCalls(tc("wu", "work_unit_create", `{"id":"u1","goal":"g"}`), tc("g0", "bash", `{"command":"true"}`)), nil
			case 2:
				return protocol.ProviderResponse{Message: protocol.Message{Content: "proposal"}, FinishReason: "stop"}, nil
			default:
				crashOnWrite.Store(true)
				return toolCalls(tc("wcrash", "write_file", `{"path":"crash.go","content":"package p // B\n"}`)), nil
			}
		}}
		_, _ = runWithB0(context.Background(), engine1, h.ID, prov1, AgentOptions{Policy: "full", MaxSteps: 4})
		testHookBeforeToolCommit = nil

		// Verify disk ALREADY has B while SQLite execution is still 'admitted' (unsettled)!
		diskBytes, err := os.ReadFile(filepath.Join(workspace, "crash.go"))
		if err != nil || string(diskBytes) != "package p // B\n" {
			t.Fatalf("disk content = %q err=%v, want B written before crash", string(diskBytes), err)
		}
		execsBefore, _ := readProvenance(t, engine1, h.ID)
		if len(execsBefore) != 2 || execsBefore[1].Outcome != "admitted" {
			t.Fatalf("executions before Reconcile = %+v, want second execution still 'admitted'", execsBefore)
		}
		_ = engine1.Close()

		// Cold reopen Engine2 -> Resume runs Store.Reconcile (closes gap with OriginToolInterrupted + outcome_unknown)
		// and reuses Step 3 cleanly!
		engine2 := openEngineAt(t, workspace)
		res2, err := runWithB0(context.Background(), engine2, h.ID, &stepProvider{steps: []protocol.ProviderResponse{
			// On Resume, just read_file so we can verify the crashed write did NOT advance MinRequiredSeq!
			toolCalls(tc("rcheck", "read_file", `{"path":"crash.go"}`)),
		}}, AgentOptions{Policy: "full", MaxSteps: 1})
		if err == nil || res2.Status != agent.StatusInterrupted {
			t.Fatalf("expected MaxSteps=1 stop after Resume step, got res=%+v err=%v", res2, err)
		}

		muts, rep, sums := inspectPersistedRepairState(t, engine2, h.ID)
		g0Seq := sums[0].AnsweredSeq
		crashedSum := sums[1]
		if crashedSum.Outcome != session.OutcomeUnknown {
			t.Fatalf("reconciled execution outcome = %s, want outcome_unknown", crashedSum.Outcome)
		}
		if muts[crashedSum.AnsweredSeq] != agent.MutationUnknown {
			t.Fatalf("crashed write group mutation = %s, want unknown", muts[crashedSum.AnsweredSeq])
		}
		if rep.MinRequiredSeq != g0Seq {
			t.Fatalf("MinRequiredSeq after crashed write + Resume = %d, want locked at g0Seq=%d", rep.MinRequiredSeq, g0Seq)
		}

		// Also verify H3: nested settled execution (;cm=proven via SettleExecution) inside an outer group
		// whose outer tool row is OriginToolInterrupted is STILL vetoed to MutationUnknown!
		nestedSummary := session.AnsweredExecutionSummary{
			AnsweredSeq: 99, RequestID: "req-99", ExecutionID: "ex-nested-1", ToolName: "write_file",
			PreRef:   "files:1:sha256:" + strings.Repeat("1", 64),
			PostRef:  "files:1:sha256:" + strings.Repeat("2", 64) + ";cm=proven",
			Coverage: session.CoverageComplete, Outcome: session.OutcomeComplete, FileEdgeCount: 1,
		}
		outerAssistant := protocol.Message{Role: "assistant", ToolCalls: []protocol.ToolCall{tc("app1", "apply_experience", `{}`)}}
		interruptedRow := []protocol.Message{{Role: "tool", ToolCallID: "app1", Name: "apply_experience", Origin: protocol.OriginToolInterrupted, Content: "interrupted"}}
		if got := agent.AggregateGroupMutation(outerAssistant, interruptedRow, []session.AnsweredExecutionSummary{nestedSummary}); got != agent.MutationUnknown {
			t.Fatalf("H3 nested proven with interrupted outer row classified as %s, want unknown", got)
		}
	})

	// RC34-F02-T9: Crash/Cold-Reopen AFTER CommitToolResult for ;cm=proven (A), ;cm=unknown (B), ;cm=none (D),
	// clearing in-memory SnapshotStore, plus legacy unsealed row fallback & <=2 SQLite writes per tool call.
	t.Run("RC34_F02_T9_post_commit_cold_resume_bit_identity_and_legacy_unsealed_fallback", func(t *testing.T) {
		workspace := t.TempDir()
		gitInit(t, workspace)
		useGlobalConfig(t, mcpGlobalConfig(workspace))
		_ = os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("A\n"), 0o644)
		_ = os.WriteFile(filepath.Join(workspace, "b.txt"), []byte("A\n"), 0o644)
		_ = os.WriteFile(filepath.Join(workspace, "d.txt"), []byte("D\n"), 0o644)

		engine1, err := Open(Options{Workspace: workspace})
		if err != nil {
			t.Fatal(err)
		}
		h := newTestSession(t, engine1, "t9-cold")

		// Measure SQLite data_version delta across one side-effect write_file call in isolation!
		roDB, err := sql.Open("sqlite3", "file:"+engine1.store.Path()+"?mode=ro&_busy_timeout=5000")
		if err != nil {
			t.Fatal(err)
		}
		defer roDB.Close()

		var toolCommits int
		testHookBeforeToolCommit = func() { toolCommits++ }
		agent.SetTestExecutionHooks(func(toolName string, targets []string) {
			if toolName == "write_file" && len(targets) == 1 && targets[0] == "b.txt" {
				// Scenario B: host writes B before Engine.Commit -> Wrote=false, ;cm=unknown
				_ = os.WriteFile(filepath.Join(workspace, "b.txt"), []byte("B\n"), 0o644)
			}
		}, nil)

		prov := &stepProvider{steps: []protocol.ProviderResponse{
			toolCalls(tc("wu", "work_unit_create", `{"id":"u1","goal":"g"}`), tc("g0", "bash", `{"command":"true"}`)),
			{Message: protocol.Message{Content: "proposal"}, FinishReason: "stop"},
			// Scenario B (;cm=unknown)
			toolCalls(tc("wb", "write_file", `{"path":"b.txt","content":"B\n"}`)),
			// Scenario D (;cm=none)
			toolCalls(tc("wd", "write_file", `{"path":"d.txt","content":"D\n"}`)),
			// Scenario A (;cm=proven)
			toolCalls(tc("wa", "write_file", `{"path":"a.txt","content":"A2\n"}`)),
		}}
		_, _ = runWithB0(context.Background(), engine1, h.ID, prov, AgentOptions{Policy: "full", MaxSteps: 5})
		testHookBeforeToolCommit = nil
		agent.SetTestExecutionHooks(nil, nil)

		// 5 tool calls committed (work_unit_create, bash, wb, wd, wa) -> exactly 1 CommitToolResult per call!
		if toolCommits != 5 {
			t.Fatalf("CommitToolResult count = %d, want 5 (1 commit + 1 admission = 2 writes per side-effect call)", toolCommits)
		}

		onlineMuts, onlineRep, _ := inspectPersistedRepairState(t, engine1, h.ID)
		_ = engine1.Close()

		// Cold reopen Engine2 (wipes in-memory editing.SnapshotStore & runTranscript) and compare bit-for-bit!
		engine2 := openEngineAt(t, workspace)
		coldMuts, coldRep, coldSums := inspectPersistedRepairState(t, engine2, h.ID)
		if !maps.Equal(onlineMuts, coldMuts) || onlineRep != coldRep {
			t.Fatalf("online vs cold Resume mismatch!\nonline muts=%v rep=%+v\ncold   muts=%v rep=%+v", onlineMuts, onlineRep, coldMuts, coldRep)
		}
		// Specifically assert Scenario B stayed MutationUnknown after cold reopen (never degraded to Proven)!
		if agent.ClassifyExecutionSummary(coldSums[1]) != agent.MutationUnknown ||
			agent.ClassifyExecutionSummary(coldSums[2]) != agent.MutationNone ||
			agent.ClassifyExecutionSummary(coldSums[3]) != agent.MutationProven {
			t.Fatalf("cold summaries classification = [%s, %s, %s], want [unknown, none, proven]",
				agent.ClassifyExecutionSummary(coldSums[1]),
				agent.ClassifyExecutionSummary(coldSums[2]),
				agent.ClassifyExecutionSummary(coldSums[3]))
		}

		// Legacy unsealed v0.4-rc3.4 row without ";cm=" suffix MUST conservatively classify as MutationUnknown!
		legacyRow := coldSums[3]
		legacyRow.PostRef = strings.TrimSuffix(legacyRow.PostRef, ";cm=proven")
		if got := agent.ClassifyExecutionSummary(legacyRow); got != agent.MutationUnknown {
			t.Fatalf("legacy unsealed tierFiles row classified as %s, want unknown", got)
		}
	})

	// RC34-F02-T10: Multi-block monotonic chain u -> G1(bash ignored Unknown) -> G2(write_file Proven) -> Block1 ->
	// G3(Host B + Agent write_file(B) No-op ;cm=unknown) -> Block2 -> G4(write_file C ;cm=proven) -> Block3 -> u2,
	// checking online == cold Resume at EVERY checkpoint, plus MCP removal across Resume!
	t.Run("RC34_F02_T10_multi_block_monotonic_chain_and_cold_resume_equivalence", func(t *testing.T) {
		workspace := t.TempDir()
		gitInit(t, workspace)
		_ = os.WriteFile(filepath.Join(workspace, ".gitignore"), []byte("build/\n"), 0o644)
		_ = os.MkdirAll(filepath.Join(workspace, "build"), 0o755)
		_ = os.WriteFile(filepath.Join(workspace, "chain.go"), []byte("package p // A\n"), 0o644)
		useRuntimeMCP(t, workspace, filepath.Join(workspace, "build", "mcp.log"))

		engine1, err := Open(Options{Workspace: workspace})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := engine1.MCPStart(context.Background(), "", "srv"); err != nil {
			t.Fatal(err)
		}
		h := newTestSession(t, engine1, "t10-chain")

		checkOnlineEqualsCold := func(label string) (map[int64]agent.MutationEvidence, agent.RepairContext, []session.AnsweredExecutionSummary) {
			onMuts, onRep, sums := inspectPersistedRepairState(t, engine1, h.ID)
			_ = engine1.Close()
			// Remove MCP config before final cold check when requested, or reopen cleanly
			var reopenErr error
			engine1, reopenErr = Open(Options{Workspace: workspace})
			if reopenErr != nil {
				t.Fatalf("%s reopen: %v", label, reopenErr)
			}
			coldMuts, coldRep, _ := inspectPersistedRepairState(t, engine1, h.ID)
			if !maps.Equal(onMuts, coldMuts) || onRep != coldRep {
				t.Fatalf("%s: online (%v, %+v) != cold (%v, %+v)", label, onMuts, onRep, coldMuts, coldRep)
			}
			return coldMuts, coldRep, sums
		}
		t.Cleanup(func() {
			agent.SetTestExecutionHooks(nil, nil)
			if engine1 != nil {
				_ = engine1.Close()
			}
		})

		// Run 1: u -> G1(bash modifying ignored build/gen.go + mcp__srv__echo -> Unknown) -> G2(write_file Proven) -> Proposal 1 -> Block1
		prov1 := &stepProvider{steps: []protocol.ProviderResponse{
			toolCalls(
				tc("wu", "work_unit_create", `{"id":"u1","goal":"chain"}`),
				tc("g1", "bash", `{"command":"echo x > build/gen.go"}`),
				tc("mcp1", "mcp__srv__echo", `{"text":"obs"}`),
			),
			toolCalls(tc("g2", "write_file", `{"path":"chain.go","content":"package p // A1\n"}`)),
			{Message: protocol.Message{Content: "proposal 1"}, FinishReason: "stop"},
		}}
		_, _ = runWithB0(context.Background(), engine1, h.ID, prov1, AgentOptions{Policy: "full", MaxSteps: 3})
		// Uninstall MCP config from workspace before cold reopen to prove MCP classification across Resume needs zero live MCP config!
		_ = os.RemoveAll(filepath.Join(workspace, ".lumencortex", "mcp.json"))
		_, repAtBlock1, sums1 := checkOnlineEqualsCold("Block1")
		seqG1 := sums1[0].AnsweredSeq
		if repAtBlock1.MinRequiredSeq != seqG1 {
			t.Fatalf("Checkpoint 1 (Block1): MinRequiredSeq=%d, want Seq(G1)=%d", repAtBlock1.MinRequiredSeq, seqG1)
		}

		// Run 2 (Resume): G3 (Host writes B, Agent write_file(B) No-op ;cm=unknown) -> Proposal 2 -> Block2
		agent.SetTestExecutionHooks(func(toolName string, _ []string) {
			if toolName == "write_file" {
				_ = os.WriteFile(filepath.Join(workspace, "chain.go"), []byte("package p // B\n"), 0o644)
			}
		}, nil)
		prov2 := &stepProvider{steps: []protocol.ProviderResponse{
			toolCalls(tc("g3", "write_file", `{"path":"chain.go","content":"package p // B\n"}`)),
			{Message: protocol.Message{Content: "proposal 2"}, FinishReason: "stop"},
		}}
		_, _ = runWithB0(context.Background(), engine1, h.ID, prov2, AgentOptions{Policy: "full", MaxSteps: 2})
		agent.SetTestExecutionHooks(nil, nil)
		_, repAtBlock2, _ := checkOnlineEqualsCold("Block2")
		if repAtBlock2.MinRequiredSeq != seqG1 {
			t.Fatalf("Checkpoint 2 (Block2 after false-write G3): MinRequiredSeq=%d, want STILL Seq(G1)=%d!", repAtBlock2.MinRequiredSeq, seqG1)
		}
		seqBlock2 := repAtBlock2.BlockSeq

		// Run 3 (Resume): G4 (Agent real write_file C ;cm=proven) -> stop before Block3 to verify MinRequiredSeq == Seq(Block2)!
		prov3 := &stepProvider{steps: []protocol.ProviderResponse{
			toolCalls(tc("g4", "write_file", `{"path":"chain.go","content":"package p // C\n"}`)),
		}}
		_, _ = runWithB0(context.Background(), engine1, h.ID, prov3, AgentOptions{Policy: "full", MaxSteps: 1})
		_, repAfterG4, sumsAfterG4 := checkOnlineEqualsCold("AfterG4")
		seqG4 := sumsAfterG4[len(sumsAfterG4)-1].AnsweredSeq
		if repAfterG4.MinRequiredSeq != seqBlock2 {
			t.Fatalf("Checkpoint 3 (After proven G4): MinRequiredSeq=%d, want Seq(Block2)=%d", repAfterG4.MinRequiredSeq, seqBlock2)
		}

		// Run 4 (Resume): Proposal 3 -> Block3 -> MinRequiredSeq advances to Seq(G4)!
		prov4 := &stepProvider{steps: []protocol.ProviderResponse{
			{Message: protocol.Message{Content: "proposal 3"}, FinishReason: "stop"},
		}}
		_, _ = runWithB0(context.Background(), engine1, h.ID, prov4, AgentOptions{Policy: "full", MaxSteps: 1})
		_, repAtBlock3, _ := checkOnlineEqualsCold("Block3")
		if repAtBlock3.MinRequiredSeq != seqG4 {
			t.Fatalf("Checkpoint 4 (Block3): MinRequiredSeq=%d, want Seq(G4)=%d", repAtBlock3.MinRequiredSeq, seqG4)
		}

		// Run 5: New external user input u2 resets repair context (MinRequiredSeq == -1) and completes!
		prov5 := &stepProvider{steps: []protocol.ProviderResponse{
			toolCalls(
				tc("wu-done", "work_unit_update", `{"id":"u1","status":"completed","summary":"verified"}`),
			),
			{Message: protocol.Message{Content: "final answer"}, FinishReason: "stop"},
		}}
		// Append external user turn u2 via Input: "new user instruction"
		res5, err := runWithB0(context.Background(), engine1, h.ID, prov5, AgentOptions{
			Input:    "new external user instruction u2",
			Policy:   "full",
			MaxSteps: 4,
		})
		if err != nil || res5.Status != "completed" {
			t.Fatalf("Run 5 failed: res=%+v err=%v", res5, err)
		}
		_, repAfterU2, _ := checkOnlineEqualsCold("AfterU2")
		if repAfterU2.Active || repAfterU2.MinRequiredSeq != -1 {
			t.Fatalf("Checkpoint 5 (After u2): RepairContext=%+v, want Active=false MinRequiredSeq=-1", repAfterU2)
		}

		// Verify strict monotonicity across [Block1, Block2, AfterG4, Block3] before u2 reset:
		chain := []int64{repAtBlock1.MinRequiredSeq, repAtBlock2.MinRequiredSeq, repAfterG4.MinRequiredSeq, repAtBlock3.MinRequiredSeq}
		for i := 1; i < len(chain); i++ {
			if chain[i] < chain[i-1] {
				t.Fatalf("non-monotonic MinRequiredSeq chain before u2: %v", chain)
			}
		}
	})
}
