package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/agent"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/window"
	"github.com/edynasty/LumenCortex/protocol"
)

// inspectPersistedRepairState loads all messages and answered executions from the session's SQLite
// store and returns (mutations map[answeredSeq]MutationEvidence, RepairContext, summaries).
func inspectPersistedRepairState(t *testing.T, engine *Engine, sessionID string) (map[int64]agent.MutationEvidence, agent.RepairContext, []session.AnsweredExecutionSummary) {
	t.Helper()
	ctx := context.Background()
	as := agentStore{store: engine.store}
	ts, err := as.TaskState(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	var entries []window.Entry
	if err := as.Transcript(ctx, sessionID, ts.WindowStartSeq, func(seq int64, msg protocol.Message) error {
		entries = append(entries, window.Entry{Seq: seq, Msg: msg})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	summaries, err := as.ListAnsweredExecutions(ctx, sessionID, ts.WindowStartSeq)
	if err != nil {
		t.Fatal(err)
	}
	bySeq := map[int64][]session.AnsweredExecutionSummary{}
	for _, s := range summaries {
		bySeq[s.AnsweredSeq] = append(bySeq[s.AnsweredSeq], s)
	}
	mutations := map[int64]agent.MutationEvidence{}
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
		mutations[e.Seq] = agent.AggregateGroupMutation(e.Msg, toolRows, bySeq[e.Seq])
		i = j
	}
	return mutations, agent.DeriveRepairContext(entries, mutations), summaries
}

func runWithB0(ctx context.Context, engine *Engine, sessionID string, p protocol.Provider, opts AgentOptions) (AgentResult, error) {
	bindRef(engine, "b0", p)
	opts.ModelRef = "test/b0"
	return engine.RunAgent(ctx, sessionID, opts)
}

func stepPrefixModeAndReason(t *testing.T, entry stepEntry) []struct{ Mode, Reason string } {
	t.Helper()
	var out []struct{ Mode, Reason string }
	for _, d := range entry.Decisions {
		if d.Rule != agent.RuleWindowPrefix {
			continue
		}
		var item struct {
			Mode   string `json:"mode"`
			Reason string `json:"reason"`
		}
		decodeJSON(t, d.Output, &item)
		out = append(out, struct{ Mode, Reason string }{Mode: item.Mode, Reason: item.Reason})
	}
	return out
}

// TestT11_CandidateProfileIsolationAndGovernanceInvariants exercises ADR-0024 §6 T11 (Subsets A & B).
func TestT11_CandidateProfileIsolationAndGovernanceInvariants(t *testing.T) {
	t.Run("SubsetA_compact_failover_to_detailed_preserves_run_governance_caps", func(t *testing.T) {
		cfg := `{
			"model": "test/m-compact",
			"failover": ["test/m-detailed"],
			"providers": {"test": {"settings": {"baseURL": "http://127.0.0.1:9/v1"}, "models": {
				"m-compact": {
					"modelID": "m-compact",
					"limit": {"context": 128000, "output": 4096},
					"cost": {"input": 1.0, "output": 2.0},
					"profile": {"toolDescriptions": "compact", "feedback": "compact"}
				},
				"m-detailed": {
					"modelID": "m-detailed",
					"limit": {"context": 128000, "output": 4096},
					"cost": {"input": 1.0, "output": 2.0},
					"profile": {"toolDescriptions": "detailed", "feedback": "detailed"}
				}
			}}}
		}`
		useGlobalConfig(t, cfg)
		engine := openTestEngine(t)

		var seenCompact, seenDetailed protocol.ProviderRequest
		pCompact := &exitProvider{fn: func(_ context.Context, _ int, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
			seenCompact = req
			return protocol.ProviderResponse{}, classed(protocol.ClassAvailability)
		}}
		var detailCalls atomic.Int32
		pDetailed := &exitProvider{fn: func(_ context.Context, _ int, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
			n := detailCalls.Add(1)
			seenDetailed = req
			// Keep issuing read_file calls beyond step 2 so MaxSteps=2 (not any hypothetical 50) stops the run!
			return toolCalls(tc(fmt.Sprintf("r-%d", n), "read_file", `{"path":"missing.txt"}`)), nil
		}}
		bindRef(engine, "m-compact", pCompact)
		bindRef(engine, "m-detailed", pDetailed)

		handle := newTestSession(t, engine, "t11-a")
		res, err := engine.RunAgent(context.Background(), handle.ID, AgentOptions{
			MaxSteps: 2,
			Budget:   &config.Budget{CostLimitUSD: 1.0},
		})
		if err == nil || res.Status != agent.StatusInterrupted {
			t.Fatalf("expected MaxSteps=2 interruption, got res=%+v err=%v", res, err)
		}
		_, cause := sessionState(t, engine, handle.ID)
		if cause == nil || cause.Kind != agent.KindMaxSteps || !strings.Contains(cause.Message, "2") {
			t.Fatalf("run-level MaxSteps=2 was altered across failover: cause=%+v", cause)
		}

		// Check step 1 prefix decisions: m-compact used ModeCompact (candidate_profile), m-detailed used ModeFull (fits_full)!
		allSteps := steps(t, engine, handle.ID)
		if len(allSteps) != 2 {
			t.Fatalf("steps=%d, want 2 (run MaxSteps=2)", len(allSteps))
		}
		verifyTrace(t, allSteps)
		gotPrefixes := stepPrefixModeAndReason(t, allSteps[0])
		if len(gotPrefixes) != 2 ||
			gotPrefixes[0].Mode != string(agent.ModeCompact) || gotPrefixes[0].Reason != agent.PrefixReasonCandidateProfile ||
			gotPrefixes[1].Mode != string(agent.ModeFull) || gotPrefixes[1].Reason != agent.PrefixReasonFitsFull {
			t.Fatalf("Step 1 candidate prefix decisions = %+v, want [(compact,candidate_profile), (full,fits_full)]", gotPrefixes)
		}

		// Verify tool description bytes actually differed between m-compact and m-detailed!
		findDesc := func(req protocol.ProviderRequest, name string) string {
			for _, s := range req.Tools {
				if s.Name == name {
					return s.Description
				}
			}
			return ""
		}
		compactEval := findDesc(seenCompact, "read_file")
		detailedEval := findDesc(seenDetailed, "read_file")
		if compactEval == "" || detailedEval == "" || len(compactEval) >= len(detailedEval) {
			t.Fatalf("expected m-compact read_file desc (%dB) < m-detailed read_file desc (%dB)", len(compactEval), len(detailedEval))
		}
	})

	t.Run("SubsetB_64k_detailed_honors_preference_while_64k_auto_triggers_fixed_prefix_budget", func(t *testing.T) {
		workspace := t.TempDir()
		// Create a 30 KiB trusted AGENTS.md (~10K tokens) + 40 project skills (~10.5K tokens) + ~3K builtin tools
		// so fullFixed (~24.5K) > usableFull/2 (~23.1K) while fullFixed + requiredTranscript (~24.6K) <= usableFull (~46.2K)!
		if err := os.WriteFile(filepath.Join(workspace, "AGENTS.md"), []byte("# Guide\n"+strings.Repeat("Detailed workspace guide line for context testing.\n", 600)), 0o644); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 40; i++ {
			sdir := filepath.Join(workspace, ".lumencortex", "skills", fmt.Sprintf("skill-%02d", i))
			if err := os.MkdirAll(sdir, 0o755); err != nil {
				t.Fatal(err)
			}
			sbody := fmt.Sprintf("---\nname: Skill %02d\ndescription: %s\n---\nBody %02d\n", i, strings.Repeat("specialized workflow capability description ", 16), i)
			if err := os.WriteFile(filepath.Join(sdir, "SKILL.md"), []byte(sbody), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		cfg := fmt.Sprintf(`{
			"model": "test/m-auto",
			"trustedWorkspaces": [%q],
			"providers": {"test": {"settings": {"baseURL": "http://127.0.0.1:9/v1"}, "models": {
				"m-auto": {
					"modelID": "m-auto",
					"limit": {"context": 64000, "output": 4096}
				},
				"m-detailed": {
					"modelID": "m-detailed",
					"limit": {"context": 64000, "output": 4096},
					"profile": {"toolDescriptions": "detailed"}
				}
			}}}
		}`, workspace)
		useGlobalConfig(t, cfg)
		engine := openEngineAt(t, workspace)
		pAuto := &answering{final: "auto-ok"}
		pDet := &answering{final: "detailed-ok"}
		bindRef(engine, "m-auto", pAuto)
		bindRef(engine, "m-detailed", pDet)

		hAuto := newTestSession(t, engine, "t11-b-auto")
		if r := run(t, engine, hAuto.ID, AgentOptions{ModelRef: "test/m-auto"}); r.Status != "completed" {
			t.Fatalf("m-auto failed: %+v", r)
		}
		hDet := newTestSession(t, engine, "t11-b-det")
		if r := run(t, engine, hDet.ID, AgentOptions{ModelRef: "test/m-detailed"}); r.Status != "completed" {
			t.Fatalf("m-detailed failed: %+v", r)
		}

		autoSteps := steps(t, engine, hAuto.ID)
		detSteps := steps(t, engine, hDet.ID)
		verifyTrace(t, autoSteps)
		verifyTrace(t, detSteps)
		autoPrefix := stepPrefixModeAndReason(t, autoSteps[0])
		detPrefix := stepPrefixModeAndReason(t, detSteps[0])
		if len(autoPrefix) != 1 || autoPrefix[0].Mode != string(agent.ModeCompact) || autoPrefix[0].Reason != agent.PrefixReasonFixedPrefixBudget {
			t.Fatalf("m-auto prefix = %+v, want (compact, fixed_prefix_budget)", autoPrefix)
		}
		if len(detPrefix) != 1 || detPrefix[0].Mode != string(agent.ModeFull) || detPrefix[0].Reason != agent.PrefixReasonFitsFull {
			t.Fatalf("m-detailed prefix = %+v, want (full, fits_full)", detPrefix)
		}
	})
}

// TestT3B_CausalAuthorshipConcurrencyAndIgnoredFilesMatrix exercises ADR-0024 §6 T3 Subset B:
// RC34-F02 mandatory tests 1..6 + RC34-F01 workspace/ignored/MCP matrix 7..15 in a real Git repository.
func TestT3B_CausalAuthorshipConcurrencyAndIgnoredFilesMatrix(t *testing.T) {
	workspace := t.TempDir()
	gitInit(t, workspace)
	if err := os.WriteFile(filepath.Join(workspace, ".gitignore"), []byte("build/\n*.secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Configure a .gitattributes clean filter on *.filtered to prove raw SHA-256 sandwich + Git blob check coexist!
	for _, args := range [][]string{
		{"config", "filter.stripcr.clean", "tr -d '\\r'"},
		{"config", "filter.stripcr.smudge", "cat"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", workspace}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(workspace, ".gitattributes"), []byte("*.filtered filter=stripcr\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspace, "build"), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", workspace, "add", ".gitignore", ".gitattributes").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v %s", err, out)
	}
	if out, err := exec.Command("git", "-C", workspace, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "ignore+attr").CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v %s", err, out)
	}

	useRuntimeMCP(t, workspace, filepath.Join(workspace, "build", "mcp.secret"))
	engine := openEngineAt(t, workspace)
	if _, err := engine.MCPStart(context.Background(), "", "srv"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		agent.SetTestExecutionHooks(nil, nil)
		engine.editing.SetTestFaultHooks(nil, nil)
	})

	// Helper that sets up an active completion-gate repair session (u -> G0(Unknown) -> Proposal -> Block1)
	// using an open work_unit so the completion gate blocks on Proposal 1 without external shell checks,
	// then runs one candidate repair step `actCall`, then closes the work_unit or inspects (MutationEvidence, MinRequiredSeq)!
	runPostBlockScenario := func(t *testing.T, name string, setup func(), actCalls []protocol.ToolCall) (agent.MutationEvidence, int64, int64, []session.AnsweredExecutionSummary) {
		t.Helper()
		agent.SetTestExecutionHooks(nil, nil)
		engine.editing.SetTestFaultHooks(nil, nil)
		if setup != nil {
			setup()
		}
		h := newTestSession(t, engine, "scenario-"+name)
		// Step 1: create an open work unit + run bash("true") (Unknown, seq=G0)
		// Step 2: propose completion -> blocked by incomplete work unit (OriginCompletionBlocked at Block1)!
		// Step 3: execute `actCalls` (the post-block repair step under test!)
		// Step 4: complete the work unit and finish!
		// Wait: inspect repair state right after Step 3 by stopping at MaxSteps=3!
		prov := &stepProvider{steps: []protocol.ProviderResponse{
			toolCalls(
				tc("wu", "work_unit_create", `{"id":"u1","goal":"verify fix"}`),
				tc("g0", "bash", `{"command":"true"}`),
			),
			{Message: protocol.Message{Content: "premature completion proposal"}, FinishReason: "stop"},
			toolCalls(actCalls...),
		}}
		_, _ = runWithB0(context.Background(), engine, h.ID, prov, AgentOptions{Policy: "full", MaxSteps: 3})
		agent.SetTestExecutionHooks(nil, nil)
		engine.editing.SetTestFaultHooks(nil, nil)

		muts, rep, sums := inspectPersistedRepairState(t, engine, h.ID)
		// Find G0 seq (step 1 assistant) and actCalls seq (step 3 assistant):
		var seqs []int64
		for k := range muts {
			seqs = append(seqs, k)
		}
		// Sort ascending
		for i := 0; i < len(seqs); i++ {
			for j := i + 1; j < len(seqs); j++ {
				if seqs[j] < seqs[i] {
					seqs[i], seqs[j] = seqs[j], seqs[i]
				}
			}
		}
		if len(seqs) < 2 {
			t.Fatalf("%s: expected >=2 tool-call groups, got muts=%v", name, muts)
		}
		g0Seq := seqs[0]
		actSeq := seqs[len(seqs)-1]
		var actSums []session.AnsweredExecutionSummary
		for _, s := range sums {
			if s.AnsweredSeq == actSeq {
				actSums = append(actSums, s)
			}
		}
		return muts[actSeq], g0Seq, rep.MinRequiredSeq, actSums
	}

	// RC34-F02-T1: Initial A -> Agent write_file B (tracked, Git-ignored, and .gitattributes clean-filtered!)
	for _, target := range []struct {
		label   string
		relPath string
		before  string
		after   string
	}{
		{"tracked_file", "a.go", "package p\nconst V = 1\n", "package p\nconst V = 2\n"},
		{"git_ignored_file", "build/generated.go", "// v1\n", "// v2\n"},
		{"gitattributes_filtered_file", "doc.filtered", "line1\r\n", "line2\r\n"},
	} {
		t.Run("RC34_F02_T1_"+target.label, func(t *testing.T) {
			ev, g0Seq, minReqAfter, sums := runPostBlockScenario(t, "t1-"+target.label, func() {
				_ = os.WriteFile(filepath.Join(workspace, filepath.FromSlash(target.relPath)), []byte(target.before), 0o644)
			}, []protocol.ToolCall{
				tc("act", "write_file", fmt.Sprintf(`{"path":%q,"content":%q}`, target.relPath, target.after)),
			})
			if ev != agent.MutationProven {
				t.Fatalf("%s: ev=%s, want proven (sums=%+v)", target.label, ev, sums)
			}
			if len(sums) != 1 || !strings.HasSuffix(sums[0].PostRef, ";cm=proven") || sums[0].FileEdgeCount != 1 {
				t.Fatalf("%s: summary=%+v, want ;cm=proven and 1 file edge", target.label, sums)
			}
			if minReqAfter <= g0Seq {
				t.Fatalf("%s: MinRequiredSeq=%d did not advance past g0Seq=%d to Block1!", target.label, minReqAfter, g0Seq)
			}
		})
	}

	// RC34-F02-T2: Initial A -> Host concurrently writes B -> Agent write_file(B) (unread path -> Wrote=false, OK=true!)
	t.Run("RC34_F02_T2_host_writes_B_then_agent_noop_writes_B_is_unknown", func(t *testing.T) {
		ev, g0Seq, minReqAfter, sums := runPostBlockScenario(t, "t2", func() {
			_ = os.WriteFile(filepath.Join(workspace, "t2.go"), []byte("package p // A\n"), 0o644)
			agent.SetTestExecutionHooks(func(toolName string, _ []string) {
				if toolName == "write_file" {
					// Host modifies t2.go to B AFTER admitExecution took PreRef=Blob(A) and BEFORE Engine.Commit!
					_ = os.WriteFile(filepath.Join(workspace, "t2.go"), []byte("package p // B\n"), 0o644)
				}
			}, nil)
		}, []protocol.ToolCall{
			tc("act", "write_file", `{"path":"t2.go","content":"package p // B\n"}`),
		})
		if ev != agent.MutationUnknown {
			t.Fatalf("T2: ev=%s, want unknown (sums=%+v)", ev, sums)
		}
		if len(sums) != 1 || sums[0].Outcome != session.OutcomeComplete || !strings.HasSuffix(sums[0].PostRef, ";cm=unknown") || sums[0].FileEdgeCount != 1 {
			t.Fatalf("T2: summary=%+v, want completed + ;cm=unknown + FileEdgeCount=1", sums)
		}
		if minReqAfter != g0Seq {
			t.Fatalf("T2: MinRequiredSeq=%d, want locked at g0Seq=%d (host change must NEVER advance floor!)", minReqAfter, g0Seq)
		}
	})

	// RC34-F02-T3: C1 (A -> Host B -> Agent C != A,B), C2 (A -> Host B -> Agent A), C3 (A -> Agent C -> Host overwrites D before postFiles)
	t.Run("RC34_F02_T3_C1_C2_C3", func(t *testing.T) {
		// C1: Proven
		evC1, g0C1, minC1, sumsC1 := runPostBlockScenario(t, "t3-c1", func() {
			_ = os.WriteFile(filepath.Join(workspace, "c1.go"), []byte("package p // A\n"), 0o644)
			agent.SetTestExecutionHooks(func(toolName string, _ []string) {
				if toolName == "write_file" {
					_ = os.WriteFile(filepath.Join(workspace, "c1.go"), []byte("package p // B\n"), 0o644)
				}
			}, nil)
		}, []protocol.ToolCall{tc("act", "write_file", `{"path":"c1.go","content":"package p // C\n"}`)})
		if evC1 != agent.MutationProven || !strings.HasSuffix(sumsC1[0].PostRef, ";cm=proven") || minC1 <= g0C1 {
			t.Fatalf("C1: ev=%s min=%d (g0=%d) sums=%+v, want proven & advanced", evC1, minC1, g0C1, sumsC1)
		}

		// C2: Agent writes back original A -> PreBlob(A) == PostBlob(A) -> ;cm=unknown!
		evC2, g0C2, minC2, sumsC2 := runPostBlockScenario(t, "t3-c2", func() {
			_ = os.WriteFile(filepath.Join(workspace, "c2.go"), []byte("package p // A\n"), 0o644)
			agent.SetTestExecutionHooks(func(toolName string, _ []string) {
				if toolName == "write_file" {
					_ = os.WriteFile(filepath.Join(workspace, "c2.go"), []byte("package p // B\n"), 0o644)
				}
			}, nil)
		}, []protocol.ToolCall{tc("act", "write_file", `{"path":"c2.go","content":"package p // A\n"}`)})
		if evC2 != agent.MutationUnknown || !strings.HasSuffix(sumsC2[0].PostRef, ";cm=unknown") || minC2 != g0C2 {
			t.Fatalf("C2: ev=%s min=%d (want %d) sums=%+v, want unknown & unadvanced", evC2, minC2, g0C2, sumsC2)
		}

		// C3: Agent writes C, then Host overwrites D before postFiles sandwich check -> ;cm=unknown!
		evC3, g0C3, minC3, sumsC3 := runPostBlockScenario(t, "t3-c3", func() {
			_ = os.WriteFile(filepath.Join(workspace, "c3.go"), []byte("package p // A\n"), 0o644)
			agent.SetTestExecutionHooks(nil, func(toolName string, _ []string) {
				if toolName == "write_file" {
					_ = os.WriteFile(filepath.Join(workspace, "c3.go"), []byte("package p // D\n"), 0o644)
				}
			})
		}, []protocol.ToolCall{tc("act", "write_file", `{"path":"c3.go","content":"package p // C\n"}`)})
		if evC3 != agent.MutationUnknown || !strings.HasSuffix(sumsC3[0].PostRef, ";cm=unknown") || minC3 != g0C3 {
			t.Fatalf("C3: ev=%s min=%d (want %d) sums=%+v, want unknown & unadvanced", evC3, minC3, g0C3, sumsC3)
		}
	})

	// RC34-F02-T4: Idempotent A -> Agent write_file(A) -> ;cm=none, MutationNone, never advances!
	t.Run("RC34_F02_T4_idempotent_write_same_content_is_none", func(t *testing.T) {
		ev, g0Seq, minReqAfter, sums := runPostBlockScenario(t, "t4-noop", func() {
			_ = os.WriteFile(filepath.Join(workspace, "t4.go"), []byte("package p // same\n"), 0o644)
		}, []protocol.ToolCall{tc("act", "write_file", `{"path":"t4.go","content":"package p // same\n"}`)})
		if ev != agent.MutationNone || len(sums) != 1 || !strings.HasSuffix(sums[0].PostRef, ";cm=none") || sums[0].FileEdgeCount != 0 || minReqAfter != g0Seq {
			t.Fatalf("T4: ev=%s min=%d (want %d) sums=%+v", ev, minReqAfter, g0Seq, sums)
		}
	})

	// RC34-F02-T5: Agent Read A -> Host writes B -> Agent write_file(C) refused by RejectStaleRead (Outcome=failed, FileEdgeCount=1, ;cm=unknown)
	t.Run("RC34_F02_T5_stale_read_conflict_with_host_edge_is_unknown", func(t *testing.T) {
		_ = os.WriteFile(filepath.Join(workspace, "t5.go"), []byte("package p // A\n"), 0o644)
		h := newTestSession(t, engine, "t5-stale")
		agent.SetTestExecutionHooks(func(toolName string, _ []string) {
			if toolName == "write_file" {
				_ = os.WriteFile(filepath.Join(workspace, "t5.go"), []byte("package p // B\n"), 0o644)
			}
		}, nil)
		prov := &stepProvider{steps: []protocol.ProviderResponse{
			toolCalls(
				tc("wu", "work_unit_create", `{"id":"u1","goal":"g"}`),
				tc("rd", "read_file", `{"path":"t5.go"}`),
				tc("g0", "bash", `{"command":"true"}`),
			),
			{Message: protocol.Message{Content: "proposal"}, FinishReason: "stop"},
			toolCalls(tc("wr", "write_file", `{"path":"t5.go","content":"package p // C\n"}`)),
		}}
		_, _ = runWithB0(context.Background(), engine, h.ID, prov, AgentOptions{Policy: "full", MaxSteps: 3})
		agent.SetTestExecutionHooks(nil, nil)
		muts, rep, sums := inspectPersistedRepairState(t, engine, h.ID)
		lastSum := sums[len(sums)-1]
		if lastSum.Outcome != session.OutcomeFailed || lastSum.FileEdgeCount != 1 || !strings.HasSuffix(lastSum.PostRef, ";cm=unknown") {
			t.Fatalf("T5 summary=%+v, want failed + FileEdgeCount=1 + ;cm=unknown", lastSum)
		}
		if muts[lastSum.AnsweredSeq] != agent.MutationUnknown || rep.MinRequiredSeq != sums[0].AnsweredSeq {
			t.Fatalf("T5 mut=%s minReq=%d (want %d)", muts[lastSum.AnsweredSeq], rep.MinRequiredSeq, sums[0].AnsweredSeq)
		}
	})

	// RC34-F02-T6: DirtyFailure (① multi-file rollback ok, ② rollback failure, ③ post-Truncate I/O error) & ④ pre-write failure
	t.Run("RC34_F02_T6_dirty_and_clean_write_failures", func(t *testing.T) {
		// ① Multi-file apply_patch where file #2 fails after Truncate and rollback restores file #1
		ev1, g01, min1, sums1 := runPostBlockScenario(t, "t6-1", func() {
			_ = os.WriteFile(filepath.Join(workspace, "p1.txt"), []byte("old1\n"), 0o644)
			_ = os.WriteFile(filepath.Join(workspace, "p2.txt"), []byte("old2\n"), 0o644)
			engine.editing.SetTestFaultHooks(func(path string) error {
				if strings.HasSuffix(path, "p2.txt") {
					return errors.New("injected disk write error after truncate")
				}
				return nil
			}, nil)
		}, []protocol.ToolCall{tc("ap1", "apply_patch", `{"patches":[{"path":"p1.txt","operation":"update","edits":[{"old_text":"old1","new_text":"new1"}]},{"path":"p2.txt","operation":"update","edits":[{"old_text":"old2","new_text":"new2"}]}]}`)})
		if ev1 != agent.MutationUnknown || sums1[0].Outcome != session.OutcomeUnknown || !strings.HasSuffix(sums1[0].PostRef, ";cm=unknown") || min1 != g01 {
			t.Fatalf("T6-1: ev=%s sum=%+v min=%d (want %d)", ev1, sums1[0], min1, g01)
		}

		// ② Multi-file apply_patch where rollback ALSO fails, leaving partial write on p1.txt
		ev2, g02, min2, sums2 := runPostBlockScenario(t, "t6-2", func() {
			_ = os.WriteFile(filepath.Join(workspace, "p1.txt"), []byte("old1\n"), 0o644)
			_ = os.WriteFile(filepath.Join(workspace, "p2.txt"), []byte("old2\n"), 0o644)
			engine.editing.SetTestFaultHooks(
				func(path string) error {
					if strings.HasSuffix(path, "p2.txt") {
						return errors.New("injected p2 error")
					}
					return nil
				},
				func(_ string) error { return errors.New("injected rollback failure") },
			)
		}, []protocol.ToolCall{tc("ap2", "apply_patch", `{"patches":[{"path":"p1.txt","operation":"update","edits":[{"old_text":"old1","new_text":"new1"}]},{"path":"p2.txt","operation":"update","edits":[{"old_text":"old2","new_text":"new2"}]}]}`)})
		if ev2 != agent.MutationUnknown || sums2[0].Outcome != session.OutcomeUnknown || !strings.HasSuffix(sums2[0].PostRef, ";cm=unknown") || min2 != g02 {
			t.Fatalf("T6-2: ev=%s sum=%+v min=%d (want %d)", ev2, sums2[0], min2, g02)
		}

		// ③ Single-file write_file failing after f.Truncate(0)
		ev3, g03, min3, sums3 := runPostBlockScenario(t, "t6-3", func() {
			_ = os.WriteFile(filepath.Join(workspace, "single.txt"), []byte("orig\n"), 0o644)
			engine.editing.SetTestFaultHooks(func(_ string) error {
				return errors.New("short write after truncate")
			}, nil)
		}, []protocol.ToolCall{tc("wf3", "write_file", `{"path":"single.txt","content":"next\n"}`)})
		if ev3 != agent.MutationUnknown || sums3[0].Outcome != session.OutcomeUnknown || !strings.HasSuffix(sums3[0].PostRef, ";cm=unknown") || min3 != g03 {
			t.Fatalf("T6-3: ev=%s sum=%+v min=%d (want %d)", ev3, sums3[0], min3, g03)
		}

		// ④ Pre-write validation failures (old_text not found & path escape)
		ev4a, g04a, min4a, sums4a := runPostBlockScenario(t, "t6-4a", func() {
			_ = os.WriteFile(filepath.Join(workspace, "pre.txt"), []byte("hello\n"), 0o644)
		}, []protocol.ToolCall{tc("rf", "replace_in_file", `{"path":"pre.txt","old_text":"missing","new_text":"x"}`)})
		if ev4a != agent.MutationUnknown || sums4a[0].Outcome != session.OutcomeFailed || !strings.HasSuffix(sums4a[0].PostRef, ";cm=unknown") || min4a != g04a {
			t.Fatalf("T6-4a: ev=%s sum=%+v min=%d", ev4a, sums4a[0], min4a)
		}
		ev4b, g04b, min4b, sums4b := runPostBlockScenario(t, "t6-4b", nil,
			[]protocol.ToolCall{tc("esc", "write_file", `{"path":"../escape.txt","content":"x"}`)})
		if ev4b != agent.MutationUnknown || sums4b[0].Outcome != session.OutcomeUnknown || !sums4b[0].PreUnavailable || min4b != g04b {
			t.Fatalf("T6-4b: ev=%s sum=%+v min=%d", ev4b, sums4b[0], min4b)
		}
	})

	// RC34-F01 (7..15): tierWorkspace on Git-ignored files (PreRef==PostRef, Coverage=complete, FileEdgeCount=0),
	// touch, git commit, edit_go, MCP read/exec -> 100% MutationUnknown; pure read_file -> MutationNone!
	t.Run("RC34_F01_workspace_ignored_mcp_and_readonly_matrix", func(t *testing.T) {
		_ = os.WriteFile(filepath.Join(workspace, "m.go"), []byte("package p\nfunc F() int { return 1 }\n"), 0o644)
		for _, tcItem := range []struct {
			name string
			call protocol.ToolCall
			want agent.MutationEvidence
		}{
			{"bash_create_ignored", tc("i1", "bash", `{"command":"echo secret > app.secret && echo gen > build/gen.txt"}`), agent.MutationUnknown},
			{"bash_modify_ignored", tc("i2", "bash", `{"command":"echo secret2 > app.secret"}`), agent.MutationUnknown},
			{"bash_delete_ignored", tc("i3", "bash", `{"command":"rm -f app.secret build/gen.txt"}`), agent.MutationUnknown},
			{"bash_touch_tracked", tc("i4", "bash", `{"command":"touch .gitignore"}`), agent.MutationUnknown},
			{"edit_go_preview", tc("i5", "edit_go", `{"path":"m.go","preview":true,"ops":[{"op":"find","symbol":"F"}]}`), agent.MutationUnknown},
			{"mcp_read", tc("i6", "mcp__srv__echo", `{"text":"ping"}`), agent.MutationUnknown},
			{"mcp_exec", tc("i7", "mcp__srv__act", `{"text":"pong"}`), agent.MutationUnknown},
			{"builtin_read_file", tc("i8", "read_file", `{"path":"m.go"}`), agent.MutationNone},
		} {
			ev, g0Seq, minAfter, sums := runPostBlockScenario(t, tcItem.name, nil, []protocol.ToolCall{tcItem.call})
			if ev != tcItem.want {
				t.Fatalf("%s: ev=%s, want %s (sums=%+v)", tcItem.name, ev, tcItem.want, sums)
			}
			if minAfter != g0Seq {
				t.Fatalf("%s: MinRequiredSeq=%d, want unchanged %d", tcItem.name, minAfter, g0Seq)
			}
			if strings.HasPrefix(tcItem.name, "bash_") && strings.Contains(tcItem.name, "ignored") {
				// Confirm SnapshotWorkspace WAS blind to ignored file (PreRef == PostRef, Coverage=complete, FileEdgeCount=0)
				// and our rule STILL classified it as MutationUnknown rather than false MutationNone!
				if len(sums) != 1 || sums[0].PreRef != sums[0].PostRef || sums[0].Coverage != session.CoverageComplete || sums[0].FileEdgeCount != 0 {
					t.Fatalf("%s: expected PreRef==PostRef & CoverageComplete & FileEdgeCount=0 on ignored file, got %+v", tcItem.name, sums)
				}
			}
		}
	})
}
