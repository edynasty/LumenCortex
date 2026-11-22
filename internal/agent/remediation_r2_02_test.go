package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/repository"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/toolset"
	"github.com/edynasty/LumenCortex/internal/window"
	"github.com/edynasty/LumenCortex/protocol"
)

func countGateOccurrencesInRequest(req protocol.ProviderRequest, needle string) (inState int, inTranscript int) {
	seenState := false
	for _, m := range req.Messages {
		if !seenState && m.Role == "system" {
			continue
		}
		if !seenState {
			seenState = true
			if strings.Contains(m.Content, needle) {
				inState++
			}
			continue
		}
		if m.Origin == protocol.OriginCompletionBlocked && strings.Contains(m.Content, needle) {
			inTranscript++
		}
	}
	return inState, inTranscript
}

// TestR202LiveGatedRepairPhase40StepsSurvivesOnRealSQLite solidifies Opus 5.5's live post-block
// probe (zz_probe_live_gated_repair_phase_test.go) on a real SQLite database: 5 pre-block bash
// steps, 1 completion-gate refusal, then 40 post-block bash repair steps (~44K tokens > 32K)
// before final completion on a single 32,768/4,096 model.
func TestR202LiveGatedRepairPhase40StepsSurvivesOnRealSQLite(t *testing.T) {
	for _, tc := range []struct{ before, after int }{{5, 10}, {5, 40}} {
		t.Run(fmt.Sprintf("%d_pre_refusal_then_%d_post_repair_steps", tc.before, tc.after), func(t *testing.T) {
			ctx := context.Background()
			dbDir := filepath.Join(t.TempDir(), "db")
			rawStore, err := session.Open(dbDir)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = rawStore.Close() }()

			sid := fmt.Sprintf("s-r202-live-%d", tc.after)
			if err := rawStore.Create(ctx, session.Session{ID: sid, Goal: "survive long post-gate repair", Status: "running"}); err != nil {
				t.Fatal(err)
			}
			if _, _, err := rawStore.BeginRun(ctx, sid, session.RunStart{
				RunID: "run-r202",
				Input: &protocol.Message{Role: "user", Content: "run all package tests and fix"},
			}); err != nil {
				t.Fatal(err)
			}
			var rawSteps []json.RawMessage
			store := realSQLiteAgentStore{s: rawStore, steps: &rawSteps}

			jobs := &scriptedJobs{answers: [][]ActiveJob{{{ID: "job_bg_1", State: "running", Command: "make test"}}, nil}}
			var postGateRequests []protocol.ProviderRequest
			prov := &funcProvider{fn: func(_ context.Context, n int, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
				if n > tc.before+1 {
					postGateRequests = append(postGateRequests, req)
				}
				switch {
				case n <= tc.before:
					return protocol.ProviderResponse{Message: protocol.Message{ToolCalls: []protocol.ToolCall{
						call(fmt.Sprintf("b%d", n), "bash", `{"command":"go test ./..."}`)}}}, nil
				case n == tc.before+1:
					return answer("All done."), nil
				case n <= tc.before+1+tc.after:
					return protocol.ProviderResponse{Message: protocol.Message{ToolCalls: []protocol.ToolCall{
						call(fmt.Sprintf("r%d", n), "bash", `{"command":"go test ./..."}`)}}}, nil
				default:
					return answer("done after the gate's correction"), nil
				}
			}}
			tools := f03BigShell{out: strings.Repeat("ok   github.com/acme/pkg/sub 0.12s\n", 100)}
			want := tc.before + 1 + tc.after + 1
			res, err := (&Loop{Models: testModels(prov), Store: store, Tools: tools, Jobs: jobs}).Run(
				ctx, sid, Options{Session: testRef, MaxSteps: want + 4, RunID: "run-r202"},
			)
			if err != nil || res.Status != StatusCompleted {
				t.Fatalf("R2-02 RUN DIED DURING REPAIR PHASE after %d of %d repair steps: status=%s err=%v",
					prov.n-tc.before-1, tc.after, res.Status, err)
			}
			if prov.n != want {
				t.Fatalf("answered=%d, want %d", prov.n, want)
			}
			assertAllStepsVerify(t, rawSteps)

			// Verify zero Gate Feedback loss AND zero duplication between State and Messages on EVERY post-gate request!
			for idx, req := range postGateRequests {
				inState, inTx := countGateOccurrencesInRequest(req, "job_bg_1")
				if inState+inTx != 1 {
					t.Fatalf("post-gate request #%d: gate feedback occurrences inState=%d inTranscript=%d, want exact total 1 (zero loss, zero duplication)",
						idx+1, inState, inTx)
				}
			}
			if tc.after == 40 {
				lastReq := postGateRequests[len(postGateRequests)-1]
				inState, inTx := countGateOccurrencesInRequest(lastReq, "job_bg_1")
				if inState != 1 || inTx != 0 {
					t.Fatalf("expected 40th post-gate step to pin Gate State in State (inState=%d inTx=%d)", inState, inTx)
				}
			}
		})
	}
}

// TestR202FiveWindowConfigsPostBlockLongRepairAndColdResume verifies all 5 mandatory window
// configurations (32768/4096, 64000/8192, 128000/8192, 200000/16384, 600000/32768) well past
// Opus 5.5's R2-02 failure cliffs (17, 33, 19, 30, 37 steps) on real SQLite across both live
// continuation and cold SQLite Resume, checking full RepairContext + Decision parity.
func TestR202FiveWindowConfigsPostBlockLongRepairAndColdResume(t *testing.T) {
	configs := []struct {
		name            string
		context, output int
		outBytes        int
		postSteps       int // strictly greater than R2-02 death cliffs (17, 33, 19, 30, 37)!
	}{
		{"32768_4096", 32768, 4096, 3500, 30},
		{"64000_8192", 64000, 8192, 3500, 50},
		{"128000_8192", 128000, 8192, 14000, 32},
		{"200000_16384", 200000, 16384, 14000, 45},
		{"600000_32768", 600000, 32768, 35000, 50},
	}

	const line = "ok   github.com/acme/pkg/sub 0.12s\n"
	for _, cfg := range configs {
		t.Run(cfg.name, func(t *testing.T) {
			ctx := context.Background()
			dbDir := filepath.Join(t.TempDir(), "sqlite")
			rawStore, err := session.Open(dbDir)
			if err != nil {
				t.Fatal(err)
			}
			sid := "s-5cfg-" + cfg.name
			if err := rawStore.Create(ctx, session.Session{ID: sid, Goal: "5-config post-block survival", Status: "running"}); err != nil {
				t.Fatal(err)
			}
			if _, _, err := rawStore.BeginRun(ctx, sid, session.RunStart{
				RunID: "run-seed",
				Input: &protocol.Message{Role: "user", Content: "stabilize long suite"},
			}); err != nil {
				t.Fatal(err)
			}
			st := realSQLiteAgentStore{s: rawStore}
			liveRun := &run{}
			if err := liveRun.load(ctx, st, sid, nil, cfg.context); err != nil {
				t.Fatal(err)
			}

			output := strings.Repeat(line, cfg.outBytes/len(line))
			ref := config.ModelRef("test/m-" + cfg.name)
			last := int64(0)
			appendShell := func(tag string, idx int) {
				f, l := int64(0), last
				req, err := st.AdmitRequest(ctx, session.RequestAdmission{
					RequestID: fmt.Sprintf("rq-%s-%s-%d", cfg.name, tag, idx),
					SessionID: sid, RunID: "run-seed", Purpose: session.PurposeMain, Step: idx + 1,
					FirstSeq: &f, LastSeq: &l, ProjectionVersion: 2,
					Refs: []string{"model:" + string(ref), "proj:mode:compact", "ts:v0"},
				})
				if err != nil {
					t.Fatal(err)
				}
				cid := fmt.Sprintf("%s%d", tag, idx)
				aMsg := protocol.Message{Role: "assistant", ToolCalls: []protocol.ToolCall{call(cid, "bash", `{"command":"go test ./..."}`)}}
				aSeq, err := st.AppendAnswered(ctx, sid, req.RequestID, aMsg)
				if err != nil {
					t.Fatal(err)
				}
				exID := fmt.Sprintf("ex-%s-%s-%d", cfg.name, tag, idx)
				if err := st.AdmitExecution(ctx, session.ExecutionAdmission{
					SessionID: sid, RunID: "run-seed", ExecutionID: exID, RequestID: req.RequestID,
					ToolName: "bash", ArgsDigest: "sha256:" + strings.Repeat("a", 64), PreUnavailable: true,
				}); err != nil {
					t.Fatal(err)
				}
				tMsg := protocol.Message{Role: "tool", ToolCallID: cid, Name: "bash", Content: output}
				tSeq, _, err := st.CommitToolResult(ctx, ToolResultCommit{
					SessionID: sid, RunID: "run-seed", Message: tMsg,
					Execution: &session.ExecutionResult{ExecutionID: exID, Outcome: session.OutcomeComplete, Coverage: session.CoverageUnavailable},
				})
				if err != nil {
					t.Fatal(err)
				}
				last = tSeq
				liveRun.appendAssistant(aSeq, aMsg, ref, protocol.Echo{})
				liveRun.appendRow(tSeq, tMsg)
				if err := liveRun.refreshGroupMutation(ctx, st, sid, aSeq); err != nil {
					t.Fatal(err)
				}
				if _, err := liveRun.groups(cfg.context); err != nil {
					t.Fatal(err)
				}
			}

			for i := 0; i < 5; i++ {
				appendShell("pre", i)
			}
			propMsg := protocol.Message{Role: "assistant", Content: "All done."}
			pSeq, _, err := st.AppendMessage(ctx, sid, propMsg, nil)
			if err != nil {
				t.Fatal(err)
			}
			liveRun.appendAssistant(pSeq, propMsg, ref, protocol.Echo{})
			blkMsg := protocol.Message{
				Role:    "user",
				Origin:  protocol.OriginCompletionBlocked,
				Content: "Completion refused: required check `go test ./pkg/core` failed with exit code 1.",
			}
			bSeq, _, err := st.AppendMessage(ctx, sid, blkMsg, nil)
			if err != nil {
				t.Fatal(err)
			}
			liveRun.appendRow(bSeq, blkMsg)

			for i := 0; i < cfg.postSteps; i++ {
				appendShell("post", i)
			}

			liveRepair, err := liveRun.deriveRepairContext(ctx, st, sid)
			if err != nil {
				t.Fatal(err)
			}
			if err := rawStore.Close(); err != nil {
				t.Fatal(err)
			}

			// Reopen SQLite cold and verify full RepairContext DeepEqual + Loop.Run Resume survival!
			reopened, err := session.Open(dbDir)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = reopened.Close() }()
			if _, err := reopened.Reconcile(ctx, sid); err != nil {
				t.Fatal(err)
			}
			if _, _, err := reopened.BeginRun(ctx, sid, session.RunStart{RunID: "run-resume"}); err != nil {
				t.Fatal(err)
			}
			var resumeSteps []json.RawMessage
			resumeStore := realSQLiteAgentStore{s: reopened, steps: &resumeSteps}

			coldRun := &run{}
			if err := coldRun.load(ctx, resumeStore, sid, nil, cfg.context); err != nil {
				t.Fatal(err)
			}
			coldRepair, err := coldRun.deriveRepairContext(ctx, resumeStore, sid)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(liveRepair, coldRepair) {
				t.Fatalf("live vs cold RepairContext mismatch:\nlive=%+v\ncold=%+v", liveRepair, coldRepair)
			}

			var sentReq protocol.ProviderRequest
			prov := &funcProvider{fn: func(_ context.Context, _ int, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
				sentReq = req
				return answer("repaired across long post-gate phase"), nil
			}}
			m := testModel(ref, prov)
			m.Context, m.Output = cfg.context, cfg.output
			res, err := (&Loop{Models: map[config.ModelRef]Model{ref: m}, Store: resumeStore, Tools: fakeTools{}}).Run(
				ctx, sid, Options{Session: ref, RunID: "run-resume", Resumed: true, MaxSteps: 2},
			)
			if err != nil || res.Status != StatusCompleted {
				t.Fatalf("%s died after %d post-block steps: status=%s err=%v", cfg.name, cfg.postSteps, res.Status, err)
			}
			assertAllStepsVerify(t, resumeSteps)
			inState, inTx := countGateOccurrencesInRequest(sentReq, "go test ./pkg/core")
			if inState != 1 || inTx != 0 {
				t.Fatalf("%s expected Gate State pinned in State without transcript duplication, got inState=%d inTx=%d",
					cfg.name, inState, inTx)
			}
		})
	}
}

// TestR202PostBlockStageR2RetainsProvenAnchorAndFailoverFirst exercises:
//  1. Failover-First on post-block overflow ([32K, 200K] -> fails over to 200K with zero relaxation);
//  2. Stage R2 selective rolling: 30 large read_file (MutationNone) steps after BlockSeq followed by
//     a real write_file (MutationProven) on 32K -> drops read_file bodies, pins Gate State in State,
//     and retains the MutationProven edit group in Messages;
//  3. Controlled KindWindowOverflow when Pinned Gate State + newest atomic group exceeds largest candidate.
func TestR202PostBlockStageR2RetainsProvenAnchorAndFailoverFirst(t *testing.T) {
	t.Run("post_block_overflow_fails_over_from_32k_to_200k_without_relaxation", func(t *testing.T) {
		const (
			ref32k  = config.ModelRef("test/m-32k")
			ref200k = config.ModelRef("test/m-200k")
		)
		store := envStore("r202-fo")
		output := strings.Repeat("ok   github.com/acme/pkg/sub 0.12s\n", 100)
		_, _, _ = store.AppendMessage(context.Background(), "r202-fo", protocol.Message{Role: "assistant", Content: "done"}, nil)
		_, _, _ = store.AppendMessage(context.Background(), "r202-fo", protocol.Message{
			Role: "user", Origin: protocol.OriginCompletionBlocked, Content: "gate refused",
		}, nil)
		last := int64(2)
		for i := 0; i < 25; i++ {
			f, l := int64(0), last
			req, _ := store.AdmitRequest(context.Background(), session.RequestAdmission{
				RequestID: fmt.Sprintf("rqfo-%d", i), SessionID: "r202-fo", Purpose: session.PurposeMain,
				FirstSeq: &f, LastSeq: &l, ProjectionVersion: 2, LineageComplete: true,
			})
			cid := fmt.Sprintf("b%d", i)
			_, _ = store.AppendAnswered(context.Background(), "r202-fo", req.RequestID, protocol.Message{
				Role: "assistant", ToolCalls: []protocol.ToolCall{call(cid, "bash", `{"command":"true"}`)},
			})
			exID := fmt.Sprintf("exfo-%d", i)
			_ = store.AdmitExecution(context.Background(), session.ExecutionAdmission{
				SessionID: "r202-fo", ExecutionID: exID, RequestID: req.RequestID,
				ToolName: "bash", ArgsDigest: "sha256:" + strings.Repeat("c", 64), PreUnavailable: true,
			})
			seq, _, _ := store.CommitToolResult(context.Background(), ToolResultCommit{
				SessionID: "r202-fo",
				Message:   protocol.Message{Role: "tool", ToolCallID: cid, Name: "bash", Content: output},
				Execution: &session.ExecutionResult{ExecutionID: exID, Outcome: session.OutcomeComplete, Coverage: session.CoverageUnavailable},
			})
			last = seq
		}

		p32 := &scriptedProvider{responses: []protocol.ProviderResponse{answerOf("wrong-32k")}}
		p200 := &scriptedProvider{responses: []protocol.ProviderResponse{answerOf("answered-200k")}}
		m32 := testModel(ref32k, p32)
		m32.Context, m32.Output = 32768, 4096
		m200 := testModel(ref200k, p200)
		m200.Context, m200.Output = 200000, 4096

		res, err := (&Loop{Models: map[config.ModelRef]Model{ref32k: m32, ref200k: m200}, Store: store, Tools: fakeTools{}}).Run(
			context.Background(), "r202-fo", Options{Session: ref32k, Failover: []config.ModelRef{ref200k}, MaxSteps: 2},
		)
		if err != nil || res.Status != StatusCompleted || res.Final != "answered-200k" {
			t.Fatalf("expected failover to 200k, got status=%s final=%q err=%v", res.Status, res.Final, err)
		}
		if len(p32.seen) != 0 || len(p200.seen) != 1 {
			t.Fatalf("32k=%d 200k=%d, want 0 and 1", len(p32.seen), len(p200.seen))
		}
	})

	t.Run("stage_r2_drops_post_block_reads_while_preserving_proven_edit_and_pinned_gate", func(t *testing.T) {
		ws := t.TempDir()
		initGitRepoForTest(t, ws)
		repo, err := repository.New(ws)
		if err != nil {
			t.Fatal(err)
		}
		set, err := toolset.New(toolset.Options{Workspace: ws, Policy: toolset.PolicyFull, SessionID: "s-r2-proven"})
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		dbDir := filepath.Join(t.TempDir(), "state")
		rawStore, err := session.Open(dbDir)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rawStore.Close() }()
		const sid = "s-r2-proven"
		_ = rawStore.Create(ctx, session.Session{ID: sid, Goal: "stage r2 proven retention", Status: "running"})
		_, _, _ = rawStore.BeginRun(ctx, sid, session.RunStart{
			RunID: "run-r2", Input: &protocol.Message{Role: "user", Content: "fix main.go"},
		})
		var rawSteps []json.RawMessage
		store := realSQLiteAgentStore{s: rawStore, steps: &rawSteps}

		// Gate refusal at seq 2, then 30 large read_file (MutationNone) groups (~35K > 32K!), then real write_file!
		_, _, _ = store.AppendMessage(ctx, sid, protocol.Message{Role: "assistant", Content: "premature"}, nil)
		_, _, _ = store.AppendMessage(ctx, sid, protocol.Message{
			Role: "user", Origin: protocol.OriginCompletionBlocked, Content: "Gate check failed: missing Fixed()",
		}, nil)
		bigRead := strings.Repeat("// read-only context line 0123456789abcdef\n", 90)
		for i := 0; i < 30; i++ {
			cid := fmt.Sprintf("rd-%d", i)
			_, _, _ = store.AppendMessage(ctx, sid, protocol.Message{
				Role: "assistant", ToolCalls: []protocol.ToolCall{call(cid, "read_file", `{"path":"main.go"}`)},
			}, nil)
			_, _, _ = store.AppendMessage(ctx, sid, protocol.Message{
				Role: "tool", ToolCallID: cid, Name: "read_file", Content: bigRead,
			}, nil)
		}

		const ref = config.ModelRef("test/m-32k")
		var postEditReq protocol.ProviderRequest
		prov := &funcProvider{fn: func(_ context.Context, n int, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
			switch n {
			case 1:
				return protocol.ProviderResponse{Message: protocol.Message{
					Role:      "assistant",
					ToolCalls: []protocol.ToolCall{call("edit-proven", "write_file", `{"path":"main.go","content":"package main\nfunc Fixed() {}\n"}`)},
				}}, nil
			case 2:
				// One read after the proven edit so edit-proven is NOT the newest group — Stage R2 must STILL retain edit-proven!
				return protocol.ProviderResponse{Message: protocol.Message{
					Role:      "assistant",
					ToolCalls: []protocol.ToolCall{call("read-after", "read_file", `{"path":"main.go"}`)},
				}}, nil
			default:
				postEditReq = req
				return answer("verified Fixed()"), nil
			}
		}}
		m32 := testModel(ref, prov)
		m32.Context, m32.Output = 32768, 4096
		res, err := (&Loop{Models: map[config.ModelRef]Model{ref: m32}, Store: store, Tools: set, Snapshots: repo}).Run(
			ctx, sid, Options{Session: ref, RunID: "run-r2", MaxSteps: 5},
		)
		if err != nil || res.Status != StatusCompleted {
			t.Fatalf("Stage R2 run failed: status=%s err=%v", res.Status, err)
		}
		assertAllStepsVerify(t, rawSteps)

		sawProvenEdit := false
		for _, m := range postEditReq.Messages {
			for _, tc := range m.ToolCalls {
				if tc.ID == "edit-proven" {
					sawProvenEdit = true
				}
			}
		}
		if !sawProvenEdit {
			t.Fatalf("Stage R2 dropped post-block MutationProven edit 'edit-proven' instead of retaining PostBlockCausalFloorSeq!")
		}
		inState, inTx := countGateOccurrencesInRequest(postEditReq, "missing Fixed()")
		if inState != 1 || inTx != 0 {
			t.Fatalf("expected Gate State pinned once in State (inState=%d inTx=%d)", inState, inTx)
		}
	})

	t.Run("controlled_overflow_when_pinned_gate_and_newest_group_exceed_largest_candidate", func(t *testing.T) {
		const ref64k = config.ModelRef("test/m-64k")
		store := envStore("r202-huge-newest")
		_, _, _ = store.AppendMessage(context.Background(), "r202-huge-newest", protocol.Message{Role: "assistant", Content: "done"}, nil)
		_, _, _ = store.AppendMessage(context.Background(), "r202-huge-newest", protocol.Message{
			Role: "user", Origin: protocol.OriginCompletionBlocked, Content: "gate refused",
		}, nil)
		// Several normal post-block steps followed by an 80K newest atomic group (> 64K)!
		for i := 0; i < 4; i++ {
			cid := fmt.Sprintf("r%d", i)
			_, _, _ = store.AppendMessage(context.Background(), "r202-huge-newest", protocol.Message{
				Role: "assistant", ToolCalls: []protocol.ToolCall{call(cid, "read_file", `{"path":"a.go"}`)},
			}, nil)
			_, _, _ = store.AppendMessage(context.Background(), "r202-huge-newest", protocol.Message{
				Role: "tool", ToolCallID: cid, Name: "read_file", Content: strings.Repeat("line ", 200),
			}, nil)
		}
		_, _, _ = store.AppendMessage(context.Background(), "r202-huge-newest", protocol.Message{
			Role: "assistant", ToolCalls: []protocol.ToolCall{call("r-80k", "read_file", `{"path":"huge.log"}`)},
		}, nil)
		_, _, _ = store.AppendMessage(context.Background(), "r202-huge-newest", protocol.Message{
			Role: "tool", ToolCallID: "r-80k", Name: "read_file", Content: strings.Repeat("abcd ", 48000),
		}, nil)

		p64 := &scriptedProvider{responses: []protocol.ProviderResponse{answerOf("never")}}
		m64 := testModel(ref64k, p64)
		m64.Context, m64.Output = 64000, 4096
		res, err := (&Loop{Models: map[config.ModelRef]Model{ref64k: m64}, Store: store, Tools: fakeTools{}}).Run(
			context.Background(), "r202-huge-newest", Options{Session: ref64k, MaxSteps: 2},
		)
		if err == nil || res.Status != StatusFailed || !errors.Is(err, window.ErrOverflow) {
			t.Fatalf("expected controlled StatusFailed / ErrOverflow, got res=%+v err=%v", res, err)
		}
		if len(p64.seen) != 0 {
			t.Fatalf("expected 0 provider requests on unrecoverable overflow, got %d", len(p64.seen))
		}
	})

	t.Run("three_way_sqlite_resume_parity_unknown_veto_and_post_resume_repair", func(t *testing.T) {
		// Exercises real SQLite across:
		//   Path 1: Live run executes 5 pre-block bash -> Block1 -> 22 post-block bash -> Proven edit -> Unknown bash (veto!) -> pauses at MaxSteps;
		//   Path 2: Process exit (Close) -> Cold reopen load -> RepairContext == live RepairContext (all 16 value fields + CausalProgress!);
		//   Path 3: Cold Resume continues with 10 more post-block steps -> Block2 -> Proven fix -> completes, with every step passing VerifyStep!
		ws := t.TempDir()
		initGitRepoForTest(t, ws)
		repo, err := repository.New(ws)
		if err != nil {
			t.Fatal(err)
		}
		set, err := toolset.New(toolset.Options{Workspace: ws, Policy: toolset.PolicyFull, SessionID: "s-3way"})
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		dbDir := filepath.Join(t.TempDir(), "db-3way")
		raw1, err := session.Open(dbDir)
		if err != nil {
			t.Fatal(err)
		}
		const sid = "s-3way"
		_ = raw1.Create(ctx, session.Session{ID: sid, Goal: "three-way parity and post-resume repair", Status: "running"})
		_, _, _ = raw1.BeginRun(ctx, sid, session.RunStart{
			RunID: "run-part1", Input: &protocol.Message{Role: "user", Content: "fix and test"},
		})
		var part1Steps []json.RawMessage
		store1 := realSQLiteAgentStore{s: raw1, steps: &part1Steps}

		// Two consecutive gate refusals across part1 and part2!
		jobs1 := &scriptedJobs{answers: [][]ActiveJob{{{ID: "job_a", State: "running", Command: "go test"}}}}
		const ref = config.ModelRef("test/m-32k")
		bigNote := strings.Repeat("diag-line-0123456789abcdef\n", 110)
		prov1 := &funcProvider{fn: func(_ context.Context, n int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
			switch {
			case n <= 3:
				return protocol.ProviderResponse{Message: protocol.Message{
					Role: "assistant", ToolCalls: []protocol.ToolCall{call(fmt.Sprintf("pre-%d", n), "bash", `{"command":"echo pre"}`)},
				}}, nil
			case n == 4:
				return answer("premature #1"), nil // -> Block1
			case n <= 26:
				// 22 post-block read/bash steps pushing past 32K!
				return protocol.ProviderResponse{Message: protocol.Message{
					Role: "assistant", ToolCalls: []protocol.ToolCall{call(fmt.Sprintf("pb-%d", n), "write_file", fmt.Sprintf(`{"path":"tmp.log","content":%q}`, bigNote))},
				}}, nil
			case n == 27:
				// Proven edit on main.go
				return protocol.ProviderResponse{Message: protocol.Message{
					Role: "assistant", ToolCalls: []protocol.ToolCall{call("prov-1", "write_file", `{"path":"main.go","content":"package main\nfunc V1() {}\n"}`)},
				}}, nil
			default:
				// Trailing Unknown bash after Proven -> Stage must become RepairStageUnknownMutated!
				return protocol.ProviderResponse{Message: protocol.Message{
					Role: "assistant", ToolCalls: []protocol.ToolCall{call("unk-tail", "bash", `{"command":"true"}`)},
				}}, nil
			}
		}}
		m1 := testModel(ref, prov1)
		m1.Context, m1.Output = 32768, 4096
		// MaxSteps: 28 -> stops right after step 28 (unk-tail) with StatusInterrupted!
		res1, err1 := (&Loop{Models: map[config.ModelRef]Model{ref: m1}, Store: store1, Tools: set, Snapshots: repo, Jobs: jobs1}).Run(
			ctx, sid, Options{Session: ref, RunID: "run-part1", MaxSteps: 28},
		)
		if err1 == nil || res1.Status != StatusInterrupted {
			t.Fatalf("part1 expected StatusInterrupted at MaxSteps=28, got status=%s err=%v", res1.Status, err1)
		}
		assertAllStepsVerify(t, part1Steps)

		// Capture state via fresh load before closing raw1, then close and reopen cold!
		liveCheck := &run{}
		if err := liveCheck.load(ctx, store1, sid, nil, 32768); err != nil {
			t.Fatal(err)
		}
		repBeforeClose, err := liveCheck.deriveRepairContext(ctx, store1, sid)
		if err != nil {
			t.Fatal(err)
		}
		if repBeforeClose.Stage != RepairStageUnknownMutated || repBeforeClose.PostBlockProvenCount == 0 || repBeforeClose.PostBlockUnknownCount == 0 {
			t.Fatalf("expected Stage=unknown_mutated with both proven & unknown counts > 0, got %+v", repBeforeClose)
		}
		if err := raw1.Close(); err != nil {
			t.Fatal(err)
		}

		// Cold reopen + Reconcile + BeginRun -> verify exact struct == equality AND continue through Block2 + Proven fix!
		raw2, err := session.Open(dbDir)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = raw2.Close() }()
		if _, err := raw2.Reconcile(ctx, sid); err != nil {
			t.Fatal(err)
		}
		if _, _, err := raw2.BeginRun(ctx, sid, session.RunStart{RunID: "run-part2"}); err != nil {
			t.Fatal(err)
		}
		var part2Steps []json.RawMessage
		store2 := realSQLiteAgentStore{s: raw2, steps: &part2Steps}

		coldCheck := &run{}
		if err := coldCheck.load(ctx, store2, sid, nil, 32768); err != nil {
			t.Fatal(err)
		}
		repCold, err := coldCheck.deriveRepairContext(ctx, store2, sid)
		if err != nil {
			t.Fatal(err)
		}
		if repCold != repBeforeClose {
			t.Fatalf("cold reopened RepairContext != pre-close RepairContext:\ncold=%+v\nlive=%+v", repCold, repBeforeClose)
		}

		set2, err := toolset.New(toolset.Options{Workspace: ws, Policy: toolset.PolicyFull, SessionID: sid})
		if err != nil {
			t.Fatal(err)
		}
		jobs2 := &scriptedJobs{answers: [][]ActiveJob{{{ID: "job_b", State: "running", Command: "go test"}}, nil}}
		prov2 := &funcProvider{fn: func(_ context.Context, n int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
			switch n {
			case 1:
				return answer("premature #2"), nil // -> Block2!
			case 2:
				return protocol.ProviderResponse{Message: protocol.Message{
					Role: "assistant", ToolCalls: []protocol.ToolCall{call("prov-2", "write_file", `{"path":"main.go","content":"package main\nfunc V2Final() {}\n"}`)},
				}}, nil
			default:
				return answer("all checks green after cold resume"), nil
			}
		}}
		m2 := testModel(ref, prov2)
		m2.Context, m2.Output = 32768, 4096
		res2, err2 := (&Loop{Models: map[config.ModelRef]Model{ref: m2}, Store: store2, Tools: set2, Snapshots: repo, Jobs: jobs2}).Run(
			ctx, sid, Options{Session: ref, RunID: "run-part2", Resumed: true, MaxSteps: 6},
		)
		if err2 != nil || res2.Status != StatusCompleted || res2.Final != "all checks green after cold resume" {
			t.Fatalf("part2 post-Resume repair failed: status=%s final=%q err=%v", res2.Status, res2.Final, err2)
		}
		assertAllStepsVerify(t, part2Steps)
	})
}
