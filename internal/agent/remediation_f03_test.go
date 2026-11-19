package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/repository"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/toolset"
	"github.com/edynasty/LumenCortex/internal/window"
	"github.com/edynasty/LumenCortex/protocol"
)

type f03BigShell struct{ out string }

func (f03BigShell) Specs([]string) []protocol.ToolSpec {
	return []protocol.ToolSpec{{Name: "bash", Permission: "exec", MutatesWorkspace: true}}
}

func (z f03BigShell) Execute(context.Context, string, map[string]any, func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	return protocol.ToolResult{OK: true, Content: z.out, MutatesWorkspace: true}, nil
}

func assertAllStepsVerify(t *testing.T, rawSteps []json.RawMessage) {
	t.Helper()
	if len(rawSteps) == 0 {
		t.Fatalf("expected recorded steps to verify")
	}
	for idx, raw := range rawSteps {
		if err := VerifyStep(raw); err != nil {
			t.Fatalf("VerifyStep[%d] failed: %v\nraw=%s", idx, err, raw)
		}
	}
}

func findFloorRelaxedDecisions(t *testing.T, rawSteps []json.RawMessage) []Decision {
	t.Helper()
	var found []Decision
	for _, raw := range rawSteps {
		var s struct {
			Decisions []Decision `json:"decisions"`
		}
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Fatal(err)
		}
		for _, d := range s.Decisions {
			if d.Rule == "window.floor_relaxed" {
				found = append(found, d)
			}
		}
	}
	return found
}

// TestF03LiveLongGatedTurnSurvivesViaFloorRelaxation solidifies Opus 5.5's live long-gated-turn probe:
// 10 and 40 shell steps (~1K tokens each) followed by a completion-gate refusal on a single 32K model.
// For 10 steps, strict MinRequiredSeq fits without relaxation; for 40 steps, strict MinRequiredSeq
// overflows 32K and enters controlled Floor Relaxation with an explicit workspace re-inspection notice
// and a replayable window.floor_relaxed decision.
func TestF03LiveLongGatedTurnSurvivesViaFloorRelaxation(t *testing.T) {
	for _, steps := range []int{10, 40} {
		t.Run(fmt.Sprintf("%d_shell_steps_then_one_gate_refusal", steps), func(t *testing.T) {
			store := envStore(fmt.Sprintf("f03live%d", steps))
			jobs := &scriptedJobs{answers: [][]ActiveJob{{{ID: "job_1", State: "running", Command: "make test"}}, nil}}
			var postGateReq protocol.ProviderRequest
			provider := &funcProvider{fn: func(_ context.Context, n int, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
				switch {
				case n <= steps:
					return protocol.ProviderResponse{Message: protocol.Message{ToolCalls: []protocol.ToolCall{
						call(fmt.Sprintf("b%d", n), "bash", `{"command":"go test ./..."}`)}}}, nil
				case n == steps+1:
					return answer("All done."), nil
				default:
					postGateReq = req
					return answer("done after the gate's correction"), nil
				}
			}}
			tools := f03BigShell{out: strings.Repeat("ok   github.com/acme/pkg/sub 0.12s\n", 100)}
			res, err := (&Loop{Models: testModels(provider), Store: store, Tools: tools, Jobs: jobs}).Run(
				context.Background(),
				store.state.ID,
				Options{Session: testRef, MaxSteps: steps + 6, RunID: "run-f03-live"},
			)
			if err != nil || res.Status != StatusCompleted {
				t.Fatalf("live gated turn (%d steps) died: status=%s err=%v", steps, res.Status, err)
			}

			var rawSteps []json.RawMessage
			for _, st := range store.steps {
				b, _ := json.Marshal(st)
				rawSteps = append(rawSteps, b)
			}
			assertAllStepsVerify(t, rawSteps)
			relaxed := findFloorRelaxedDecisions(t, rawSteps)
			if steps == 10 && len(relaxed) != 0 {
				t.Fatalf("10-step turn fits 32K strictly and must NOT trigger Floor Relaxation, got %d", len(relaxed))
			}
			if steps == 40 {
				if len(relaxed) != 1 {
					t.Fatalf("40-step turn on single 32K model must log exactly 1 window.floor_relaxed decision, got %d", len(relaxed))
				}
				stateText := ""
				hasBlocked := false
				for _, m := range postGateReq.Messages {
					if m.Role == "user" && strings.Contains(m.Content, "[Context Window Notice — Floor Relaxation]") {
						stateText = m.Content
					}
					if m.Origin == protocol.OriginCompletionBlocked {
						hasBlocked = true
					}
				}
				if stateText == "" || !strings.Contains(stateText, "re-inspect") {
					t.Fatalf("expected explicit Floor Relaxation workspace re-inspection notice in State, got %q", stateText)
				}
				if !hasBlocked {
					t.Fatalf("expected active OriginCompletionBlocked gate feedback to be retained in post-gate request")
				}
			}
		})
	}
}

// TestF03ColdResumeLongGatedTurnSurvivesOnRealSQLite solidifies Opus 5.5's cold-Resume long-gated-turn
// probe on a real SQLite database (session.Open): 10 and 40 bash steps + 1 gate block, reopened cold.
func TestF03ColdResumeLongGatedTurnSurvivesOnRealSQLite(t *testing.T) {
	for _, groups := range []int{10, 40} {
		t.Run(fmt.Sprintf("%d_bash_steps_then_cold_sqlite_resume", groups), func(t *testing.T) {
			ctx := context.Background()
			dbDir := filepath.Join(t.TempDir(), fmt.Sprintf("sqlite-%d", groups))
			rawStore, err := session.Open(dbDir)
			if err != nil {
				t.Fatal(err)
			}
			const sid = "s-f03-cold"
			if err := rawStore.Create(ctx, session.Session{ID: sid, Goal: "long autonomous repair", Status: "running"}); err != nil {
				t.Fatal(err)
			}
			if _, _, err := rawStore.BeginRun(ctx, sid, session.RunStart{
				RunID: "run-seed",
				Input: &protocol.Message{Role: "user", Content: "fix tests"},
			}); err != nil {
				t.Fatal(err)
			}
			seedStore := realSQLiteAgentStore{s: rawStore}
			last := int64(0)
			output := strings.Repeat("ok   github.com/acme/pkg/sub 0.12s\n", 100)
			firstBashSeq := int64(-1)
			for i := 0; i < groups; i++ {
				first, lastSeq := int64(0), last
				req, err := seedStore.AdmitRequest(ctx, session.RequestAdmission{
					RequestID: fmt.Sprintf("rq-seed-%d", i), SessionID: sid, RunID: "run-seed",
					Purpose: session.PurposeMain, Step: i + 1,
					FirstSeq: &first, LastSeq: &lastSeq, ProjectionVersion: 2,
					Refs: []string{"model:test/m-32k", "proj:mode:compact", "ts:v0"},
				})
				if err != nil {
					t.Fatal(err)
				}
				callID := fmt.Sprintf("b%d", i)
				aSeq, err := seedStore.AppendAnswered(ctx, sid, req.RequestID, protocol.Message{
					Role: "assistant", ToolCalls: []protocol.ToolCall{call(callID, "bash", `{"command":"go test ./..."}`)},
				})
				if err != nil {
					t.Fatal(err)
				}
				if i == 0 {
					firstBashSeq = aSeq
				}
				exID := fmt.Sprintf("ex-seed-%d", i)
				if err := seedStore.AdmitExecution(ctx, session.ExecutionAdmission{
					SessionID: sid, RunID: "run-seed", ExecutionID: exID, RequestID: req.RequestID,
					ToolName: "bash", ArgsDigest: "sha256:" + strings.Repeat("b", 64), PreUnavailable: true,
				}); err != nil {
					t.Fatal(err)
				}
				seq, _, err := seedStore.CommitToolResult(ctx, ToolResultCommit{
					SessionID: sid, RunID: "run-seed",
					Message:   protocol.Message{Role: "tool", ToolCallID: callID, Name: "bash", Content: output},
					Execution: &session.ExecutionResult{ExecutionID: exID, Outcome: session.OutcomeComplete, Coverage: session.CoverageUnavailable},
				})
				if err != nil {
					t.Fatal(err)
				}
				last = seq
			}
			if _, _, err := seedStore.AppendMessage(ctx, sid, protocol.Message{Role: "assistant", Content: "All done."}, nil); err != nil {
				t.Fatal(err)
			}
			blockSeq, _, err := seedStore.AppendMessage(ctx, sid, protocol.Message{
				Role:    "user",
				Origin:  protocol.OriginCompletionBlocked,
				Content: "Completion refused: required check `go test ./...` failed.",
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := rawStore.Close(); err != nil {
				t.Fatal(err)
			}

			// Reopen SQLite cold and run Resume on a single 32K model!
			reopened, err := session.Open(dbDir)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = reopened.Close() }()
			if _, err := reopened.Reconcile(ctx, sid); err != nil {
				t.Fatal(err)
			}
			if _, _, err := reopened.BeginRun(ctx, sid, session.RunStart{RunID: "run-cold-resume"}); err != nil {
				t.Fatal(err)
			}
			var rawSteps []json.RawMessage
			resumeStore := realSQLiteAgentStore{s: reopened, steps: &rawSteps}

			const ref = config.ModelRef("test/m-32k")
			p := &scriptedProvider{responses: []protocol.ProviderResponse{answerOf("repaired")}}
			m := testModel(ref, p)
			m.Context, m.Output = 32768, 4096
			loop := Loop{Models: map[config.ModelRef]Model{ref: m}, Store: resumeStore, Tools: fakeTools{}}
			res, err := loop.Run(ctx, sid, Options{Session: ref, RunID: "run-cold-resume", Resumed: true, MaxSteps: 2})
			if err != nil || res.Status != StatusCompleted {
				t.Fatalf("cold SQLite Resume (%d groups) failed: status=%s err=%v", groups, res.Status, err)
			}

			latestRaw, err := reopened.LatestStep(ctx, sid)
			if err != nil || len(latestRaw) == 0 {
				t.Fatalf("LatestStep from SQLite failed: %v", err)
			}
			assertAllStepsVerify(t, rawSteps)
			relaxed := findFloorRelaxedDecisions(t, rawSteps)
			if groups == 40 {
				if len(relaxed) != 1 {
					t.Fatalf("expected 1 window.floor_relaxed decision on 40-group cold Resume, got %d", len(relaxed))
				}
				var out struct {
					Relaxed                bool   `json:"relaxed"`
					Reason                 string `json:"reason"`
					OriginalMinRequiredSeq int64  `json:"originalMinRequiredSeq"`
					SentFirstSeq           int64  `json:"sentFirstSeq"`
				}
				if err := json.Unmarshal(relaxed[0].Output, &out); err != nil {
					t.Fatal(err)
				}
				if !out.Relaxed || out.OriginalMinRequiredSeq != firstBashSeq || out.SentFirstSeq <= firstBashSeq || out.SentFirstSeq > blockSeq {
					t.Fatalf("unexpected floor_relaxed output: %+v (firstBashSeq=%d blockSeq=%d)", out, firstBashSeq, blockSeq)
				}
			}
		})
	}
}

// TestF03FailoverFirstGuardAndAllScenarios exercises all mandatory F-03 edge cases:
//  1. 32K -> 200K failover guard (small model NEVER triggers Floor Relaxation when 200K is available);
//  2. [16K, 32K, 200K] with 200K unavailable (503) -> falls back to largest available 32K (not 16K) and relaxes;
//  3. Two consecutive gate refusals + post-relaxation MutationProven edit clearing Floor Relaxation and passing verification;
//  4. Controlled KindWindowOverflow when even the largest available candidate cannot fit active gate feedback + newest group.
func TestF03FailoverFirstGuardAndAllScenarios(t *testing.T) {
	t.Run("32k_fails_over_to_200k_without_floor_relaxation", func(t *testing.T) {
		const (
			ref32k  = config.ModelRef("test/m-32k")
			ref200k = config.ModelRef("test/m-200k")
		)
		store := envStore("f03-32-200")
		output := strings.Repeat("ok   github.com/acme/pkg/sub 0.12s\n", 100)
		last := int64(0)
		for i := 0; i < 40; i++ {
			f, l := int64(0), last
			req, _ := store.AdmitRequest(context.Background(), session.RequestAdmission{
				RequestID: fmt.Sprintf("rq-%d", i), SessionID: "f03-32-200", Purpose: session.PurposeMain,
				FirstSeq: &f, LastSeq: &l, ProjectionVersion: 2, LineageComplete: true,
			})
			cid := fmt.Sprintf("b%d", i)
			_, _ = store.AppendAnswered(context.Background(), "f03-32-200", req.RequestID, protocol.Message{
				Role: "assistant", ToolCalls: []protocol.ToolCall{call(cid, "bash", `{"command":"true"}`)},
			})
			exID := fmt.Sprintf("ex-%d", i)
			_ = store.AdmitExecution(context.Background(), session.ExecutionAdmission{
				SessionID: "f03-32-200", ExecutionID: exID, RequestID: req.RequestID,
				ToolName: "bash", ArgsDigest: "sha256:" + strings.Repeat("a", 64), PreUnavailable: true,
			})
			seq, _, _ := store.CommitToolResult(context.Background(), ToolResultCommit{
				SessionID: "f03-32-200",
				Message:   protocol.Message{Role: "tool", ToolCallID: cid, Name: "bash", Content: output},
				Execution: &session.ExecutionResult{ExecutionID: exID, Outcome: session.OutcomeComplete, Coverage: session.CoverageUnavailable},
			})
			last = seq
		}
		_, _, _ = store.AppendMessage(context.Background(), "f03-32-200", protocol.Message{Role: "assistant", Content: "done"}, nil)
		_, _, _ = store.AppendMessage(context.Background(), "f03-32-200", protocol.Message{
			Role: "user", Origin: protocol.OriginCompletionBlocked, Content: "gate refused",
		}, nil)

		p32 := &scriptedProvider{responses: []protocol.ProviderResponse{answerOf("wrong-32k")}}
		p200 := &scriptedProvider{responses: []protocol.ProviderResponse{answerOf("answered-by-200k")}}
		m32 := testModel(ref32k, p32)
		m32.Context, m32.Output = 32768, 4096
		m200 := testModel(ref200k, p200)
		m200.Context, m200.Output = 200000, 4096

		loop := Loop{Models: map[config.ModelRef]Model{ref32k: m32, ref200k: m200}, Store: store, Tools: fakeTools{}}
		res, err := loop.Run(context.Background(), "f03-32-200", Options{
			Session:  ref32k,
			Failover: []config.ModelRef{ref200k},
			MaxSteps: 2,
		})
		if err != nil || res.Status != StatusCompleted || res.Final != "answered-by-200k" {
			t.Fatalf("expected failover to 200k, got status=%s final=%q err=%v", res.Status, res.Final, err)
		}
		if len(p32.seen) != 0 || len(p200.seen) != 1 {
			t.Fatalf("32k calls=%d 200k calls=%d, want 0 and 1", len(p32.seen), len(p200.seen))
		}
		var rawSteps []json.RawMessage
		for _, st := range store.steps {
			b, _ := json.Marshal(st)
			rawSteps = append(rawSteps, b)
		}
		assertAllStepsVerify(t, rawSteps)
		if relaxed := findFloorRelaxedDecisions(t, rawSteps); len(relaxed) != 0 {
			t.Fatalf("small model MUST NOT trigger Floor Relaxation when 200K is available; got %+v", relaxed)
		}
	})

	t.Run("200k_unavailable_503_falls_back_to_largest_available_32k_not_16k", func(t *testing.T) {
		const (
			ref16k  = config.ModelRef("test/m-16k")
			ref32k  = config.ModelRef("test/m-32k")
			ref200k = config.ModelRef("test/m-200k")
		)
		store := envStore("f03-503")
		output := strings.Repeat("ok   github.com/acme/pkg/sub 0.12s\n", 100)
		last := int64(0)
		for i := 0; i < 40; i++ {
			f, l := int64(0), last
			req, _ := store.AdmitRequest(context.Background(), session.RequestAdmission{
				RequestID: fmt.Sprintf("rq503-%d", i), SessionID: "f03-503", Purpose: session.PurposeMain,
				FirstSeq: &f, LastSeq: &l, ProjectionVersion: 2, LineageComplete: true,
			})
			cid := fmt.Sprintf("b%d", i)
			_, _ = store.AppendAnswered(context.Background(), "f03-503", req.RequestID, protocol.Message{
				Role: "assistant", ToolCalls: []protocol.ToolCall{call(cid, "bash", `{"command":"true"}`)},
			})
			exID := fmt.Sprintf("ex503-%d", i)
			_ = store.AdmitExecution(context.Background(), session.ExecutionAdmission{
				SessionID: "f03-503", ExecutionID: exID, RequestID: req.RequestID,
				ToolName: "bash", ArgsDigest: "sha256:" + strings.Repeat("c", 64), PreUnavailable: true,
			})
			seq, _, _ := store.CommitToolResult(context.Background(), ToolResultCommit{
				SessionID: "f03-503",
				Message:   protocol.Message{Role: "tool", ToolCallID: cid, Name: "bash", Content: output},
				Execution: &session.ExecutionResult{ExecutionID: exID, Outcome: session.OutcomeComplete, Coverage: session.CoverageUnavailable},
			})
			last = seq
		}
		_, _, _ = store.AppendMessage(context.Background(), "f03-503", protocol.Message{Role: "assistant", Content: "done"}, nil)
		_, _, _ = store.AppendMessage(context.Background(), "f03-503", protocol.Message{
			Role: "user", Origin: protocol.OriginCompletionBlocked, Content: "gate refused",
		}, nil)

		p16 := &scriptedProvider{responses: []protocol.ProviderResponse{answerOf("wrong-16k")}}
		p32 := &scriptedProvider{responses: []protocol.ProviderResponse{answerOf("repaired-on-32k")}}
		p200 := &funcProvider{fn: func(context.Context, int, protocol.ProviderRequest) (protocol.ProviderResponse, error) {
			return protocol.ProviderResponse{}, &protocol.ProviderError{Class: protocol.ClassAvailability, Status: 503, Err: errors.New("upstream 503")}
		}}
		m16 := testModel(ref16k, p16)
		m16.Context, m16.Output = 16384, 2048
		m32 := testModel(ref32k, p32)
		m32.Context, m32.Output = 32768, 4096
		m200 := testModel(ref200k, p200)
		m200.Context, m200.Output = 200000, 4096

		loop := Loop{
			Models: map[config.ModelRef]Model{ref16k: m16, ref32k: m32, ref200k: m200},
			Store:  store,
			Tools:  fakeTools{},
		}
		res, err := loop.Run(context.Background(), "f03-503", Options{
			Session:  ref16k,
			Failover: []config.ModelRef{ref32k, ref200k},
			MaxSteps: 2,
		})
		if err != nil || res.Status != StatusCompleted || res.Final != "repaired-on-32k" {
			t.Fatalf("expected fallback to largest available 32k, got status=%s final=%q err=%v", res.Status, res.Final, err)
		}
		if len(p16.seen) != 0 || len(p32.seen) != 1 || p200.n != 1 {
			t.Fatalf("calls 16k=%d 32k=%d 200k=%d, want 0, 1, 1", len(p16.seen), len(p32.seen), p200.n)
		}
		var rawSteps []json.RawMessage
		for _, st := range store.steps {
			b, _ := json.Marshal(st)
			rawSteps = append(rawSteps, b)
		}
		assertAllStepsVerify(t, rawSteps)
		if relaxed := findFloorRelaxedDecisions(t, rawSteps); len(relaxed) != 1 {
			t.Fatalf("expected 1 window.floor_relaxed decision on 32k fallback, got %d", len(relaxed))
		}
	})

	t.Run("two_consecutive_gate_refusals_then_proven_edit_clears_relaxation_and_verifies", func(t *testing.T) {
		ws := t.TempDir()
		initGitRepoForTest(t, ws)
		repo, err := repository.New(ws)
		if err != nil {
			t.Fatal(err)
		}
		set, err := toolset.New(toolset.Options{Workspace: ws, Policy: toolset.PolicyFull, SessionID: "s-f03-2gate"})
		if err != nil {
			t.Fatal(err)
		}

		ctx := context.Background()
		dbDir := filepath.Join(t.TempDir(), "state-2gate")
		rawStore, err := session.Open(dbDir)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rawStore.Close() }()
		const sid = "s-f03-2gate"
		if err := rawStore.Create(ctx, session.Session{ID: sid, Goal: "two gates then proven fix", Status: "running"}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := rawStore.BeginRun(ctx, sid, session.RunStart{
			RunID: "run-2gate",
			Input: &protocol.Message{Role: "user", Content: "fix main.go"},
		}); err != nil {
			t.Fatal(err)
		}
		var rawSteps []json.RawMessage
		store := realSQLiteAgentStore{s: rawStore, steps: &rawSteps}

		// Seed 38 bash groups (~40K tokens > 32K) before first gate refusal.
		output := strings.Repeat("diagnostic test failure line 1234567890\n", 90)
		last := int64(0)
		for i := 0; i < 38; i++ {
			f, l := int64(0), last
			req, err := store.AdmitRequest(ctx, session.RequestAdmission{
				RequestID: fmt.Sprintf("rq2g-%d", i), SessionID: sid, RunID: "run-2gate",
				Purpose: session.PurposeMain, Step: i + 1,
				FirstSeq: &f, LastSeq: &l, ProjectionVersion: 2,
				Refs: []string{"model:test/m-32k", "proj:mode:compact", "ts:v0"},
			})
			if err != nil {
				t.Fatal(err)
			}
			cid := fmt.Sprintf("bg%d", i)
			_, err = store.AppendAnswered(ctx, sid, req.RequestID, protocol.Message{
				Role: "assistant", ToolCalls: []protocol.ToolCall{call(cid, "bash", `{"command":"true"}`)},
			})
			if err != nil {
				t.Fatal(err)
			}
			exID := fmt.Sprintf("ex2g-%d", i)
			_ = store.AdmitExecution(ctx, session.ExecutionAdmission{
				SessionID: sid, RunID: "run-2gate", ExecutionID: exID, RequestID: req.RequestID,
				ToolName: "bash", ArgsDigest: "sha256:" + strings.Repeat("d", 64), PreUnavailable: true,
			})
			seq, _, err := store.CommitToolResult(ctx, ToolResultCommit{
				SessionID: sid, RunID: "run-2gate",
				Message:   protocol.Message{Role: "tool", ToolCallID: cid, Name: "bash", Content: output},
				Execution: &session.ExecutionResult{ExecutionID: exID, Outcome: session.OutcomeComplete, Coverage: session.CoverageUnavailable},
			})
			if err != nil {
				t.Fatal(err)
			}
			last = seq
		}

		// Two consecutive gate refusals driven by active background jobs on proposals 1 & 2, then cleared on proposal 3!
		jobs := &scriptedJobs{answers: [][]ActiveJob{
			{{ID: "job_1", State: "running", Command: "go test"}}, // blocks proposal #1
			{{ID: "job_2", State: "running", Command: "go test"}}, // blocks proposal #2
			nil, // allows proposal #3 after proven edit!
		}}
		const ref = config.ModelRef("test/m-32k")
		prov := &funcProvider{fn: func(_ context.Context, n int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
			switch n {
			case 1:
				return answer("first premature proposal"), nil // -> Gate Refusal #1
			case 2:
				// Under Floor Relaxation #1: re-inspect workspace via read_file
				return protocol.ProviderResponse{Message: protocol.Message{
					Role:      "assistant",
					ToolCalls: []protocol.ToolCall{call("insp-1", "read_file", `{"path":"main.go"}`)},
				}}, nil
			case 3:
				return answer("second premature proposal"), nil // -> Gate Refusal #2 (still relaxed!)
			case 4:
				// Under Floor Relaxation #2: real write_file (;cm=proven -> MutationProven!)
				return protocol.ProviderResponse{Message: protocol.Message{
					Role:      "assistant",
					ToolCalls: []protocol.ToolCall{call("fix-1", "write_file", `{"path":"main.go","content":"package main\nfunc Fixed() {}\n"}`)},
				}}, nil
			default:
				// Step 5: MinRequiredSeq has now advanced to Block2, so NO Floor Relaxation is needed!
				return answer("verified and fixed"), nil
			}
		}}
		m32 := testModel(ref, prov)
		m32.Context, m32.Output = 32768, 4096
		loop := Loop{Models: map[config.ModelRef]Model{ref: m32}, Store: store, Tools: set, Snapshots: repo, Jobs: jobs}
		res, err := loop.Run(ctx, sid, Options{Session: ref, RunID: "run-2gate", MaxSteps: 10})
		if err != nil || res.Status != StatusCompleted || res.Final != "verified and fixed" {
			t.Fatalf("2-gate + proven edit run failed: status=%s final=%q err=%v", res.Status, res.Final, err)
		}

		if _, err := rawStore.LatestStep(ctx, sid); err != nil {
			t.Fatal(err)
		}
		assertAllStepsVerify(t, rawSteps)
		// Steps 2, 3, and 4 are after Block1/Block2 before proven fix -> relaxed; final Step 5 after proven fix -> NOT relaxed!
		lastStepRelaxed := findFloorRelaxedDecisions(t, rawSteps[len(rawSteps)-1:])
		if len(lastStepRelaxed) != 0 {
			t.Fatalf("expected final step after MutationProven edit to pass strict floor without relaxation, got %+v", lastStepRelaxed)
		}
		allRelaxed := findFloorRelaxedDecisions(t, rawSteps)
		if len(allRelaxed) < 2 {
			t.Fatalf("expected Floor Relaxation across both consecutive gate refusals before proven edit, got %d", len(allRelaxed))
		}
	})

	t.Run("controlled_KindWindowOverflow_when_largest_cannot_fit_gate_and_newest_group", func(t *testing.T) {
		const ref32k = config.ModelRef("test/m-32k")
		store := envStore("f03-unfit")
		_, _, _ = store.AppendMessage(context.Background(), "f03-unfit", protocol.Message{Role: "assistant", Content: "done"}, nil)
		_, _, _ = store.AppendMessage(context.Background(), "f03-unfit", protocol.Message{
			Role: "user", Origin: protocol.OriginCompletionBlocked, Content: "gate refused",
		}, nil)
		// Post-gate newest atomic group alone is ~45K tokens (> 32K)! Neither strict nor Floor Relaxation may send without it!
		huge45k := strings.Repeat("abcd ", 27000)
		_, _, _ = store.AppendMessage(context.Background(), "f03-unfit", protocol.Message{
			Role: "assistant", ToolCalls: []protocol.ToolCall{call("r-huge", "read_file", `{"path":"big.log"}`)},
		}, nil)
		_, _, _ = store.AppendMessage(context.Background(), "f03-unfit", protocol.Message{
			Role: "tool", ToolCallID: "r-huge", Name: "read_file", Content: huge45k,
		}, nil)

		p32 := &scriptedProvider{responses: []protocol.ProviderResponse{answerOf("never")}}
		m32 := testModel(ref32k, p32)
		m32.Context, m32.Output = 32768, 4096
		loop := Loop{Models: map[config.ModelRef]Model{ref32k: m32}, Store: store, Tools: fakeTools{}}
		res, err := loop.Run(context.Background(), "f03-unfit", Options{Session: ref32k, MaxSteps: 2})
		if err == nil || res.Status != StatusFailed || !errors.Is(err, window.ErrOverflow) {
			t.Fatalf("expected controlled StatusFailed / ErrOverflow when newest group + gate cannot fit, got res=%+v err=%v", res, err)
		}
		if len(p32.seen) != 0 {
			t.Fatalf("expected 0 requests sent on unrecoverable overflow, got %d", len(p32.seen))
		}
	})

	t.Run("resume_after_floor_relaxation_on_real_sqlite", func(t *testing.T) {
		// Run 1 enters Floor Relaxation (executes a read-only inspection step under Floor Relaxation and stops at MaxSteps),
		// then SQLite is closed, reopened cold, and Run 2 (Resumed: true) continues under Floor Relaxation, completes, and verifies.
		ctx := context.Background()
		dbDir := filepath.Join(t.TempDir(), "state-post-relax-resume")
		rawStore, err := session.Open(dbDir)
		if err != nil {
			t.Fatal(err)
		}
		const sid = "s-f03-post-relax-resume"
		if err := rawStore.Create(ctx, session.Session{ID: sid, Goal: "resume after floor relaxation", Status: "running"}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := rawStore.BeginRun(ctx, sid, session.RunStart{
			RunID: "run-1",
			Input: &protocol.Message{Role: "user", Content: "repair long session"},
		}); err != nil {
			t.Fatal(err)
		}
		var run1Steps []json.RawMessage
		store1 := realSQLiteAgentStore{s: rawStore, steps: &run1Steps}
		output := strings.Repeat("ok   github.com/acme/pkg/sub 0.12s\n", 100)
		last := int64(0)
		for i := 0; i < 40; i++ {
			f, l := int64(0), last
			req, _ := store1.AdmitRequest(ctx, session.RequestAdmission{
				RequestID: fmt.Sprintf("rqr-%d", i), SessionID: sid, RunID: "run-1",
				Purpose: session.PurposeMain, Step: i + 1,
				FirstSeq: &f, LastSeq: &l, ProjectionVersion: 2,
				Refs: []string{"model:test/m-32k", "proj:mode:compact", "ts:v0"},
			})
			cid := fmt.Sprintf("br%d", i)
			_, _ = store1.AppendAnswered(ctx, sid, req.RequestID, protocol.Message{
				Role: "assistant", ToolCalls: []protocol.ToolCall{call(cid, "bash", `{"command":"true"}`)},
			})
			exID := fmt.Sprintf("exr-%d", i)
			_ = store1.AdmitExecution(ctx, session.ExecutionAdmission{
				SessionID: sid, RunID: "run-1", ExecutionID: exID, RequestID: req.RequestID,
				ToolName: "bash", ArgsDigest: "sha256:" + strings.Repeat("e", 64), PreUnavailable: true,
			})
			seq, _, _ := store1.CommitToolResult(ctx, ToolResultCommit{
				SessionID: sid, RunID: "run-1",
				Message:   protocol.Message{Role: "tool", ToolCallID: cid, Name: "bash", Content: output},
				Execution: &session.ExecutionResult{ExecutionID: exID, Outcome: session.OutcomeComplete, Coverage: session.CoverageUnavailable},
			})
			last = seq
		}
		_, _, _ = store1.AppendMessage(ctx, sid, protocol.Message{Role: "assistant", Content: "done"}, nil)
		_, _, _ = store1.AppendMessage(ctx, sid, protocol.Message{
			Role: "user", Origin: protocol.OriginCompletionBlocked, Content: "gate refused",
		}, nil)

		const ref = config.ModelRef("test/m-32k")
		p1 := &scriptedProvider{responses: []protocol.ProviderResponse{{
			Message: protocol.Message{
				Role:      "assistant",
				ToolCalls: []protocol.ToolCall{call("read-1", "read_file", `{"path":"main.go"}`)},
			},
		}}}
		m1 := testModel(ref, p1)
		m1.Context, m1.Output = 32768, 4096
		// MaxSteps: 1 -> executes 1 Floor-Relaxed step (read_file) and stops with StatusInterrupted (KindMaxSteps)!
		res1, err := (&Loop{Models: map[config.ModelRef]Model{ref: m1}, Store: store1, Tools: fakeTools{}}).Run(
			ctx, sid, Options{Session: ref, RunID: "run-1", MaxSteps: 1},
		)
		if err == nil || res1.Status != StatusInterrupted {
			t.Fatalf("run-1 expected StatusInterrupted after 1 relaxed step, got status=%s err=%v", res1.Status, err)
		}
		assertAllStepsVerify(t, run1Steps)
		if len(findFloorRelaxedDecisions(t, run1Steps)) != 1 {
			t.Fatalf("expected 1 relaxed decision in run-1")
		}
		if err := rawStore.Close(); err != nil {
			t.Fatal(err)
		}

		// Reopen SQLite cold and Resume!
		reopened, err := session.Open(dbDir)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = reopened.Close() }()
		if _, err := reopened.Reconcile(ctx, sid); err != nil {
			t.Fatal(err)
		}
		if _, _, err := reopened.BeginRun(ctx, sid, session.RunStart{RunID: "run-2-resume"}); err != nil {
			t.Fatal(err)
		}
		var run2Steps []json.RawMessage
		store2 := realSQLiteAgentStore{s: reopened, steps: &run2Steps}
		p2 := &scriptedProvider{responses: []protocol.ProviderResponse{answerOf("completed after cold resume")}}
		m2 := testModel(ref, p2)
		m2.Context, m2.Output = 32768, 4096
		res2, err := (&Loop{Models: map[config.ModelRef]Model{ref: m2}, Store: store2, Tools: fakeTools{}}).Run(
			ctx, sid, Options{Session: ref, RunID: "run-2-resume", Resumed: true, MaxSteps: 2},
		)
		if err != nil || res2.Status != StatusCompleted || res2.Final != "completed after cold resume" {
			t.Fatalf("run-2 cold Resume after Floor Relaxation failed: status=%s final=%q err=%v", res2.Status, res2.Final, err)
		}
		assertAllStepsVerify(t, run2Steps)
		if len(findFloorRelaxedDecisions(t, run2Steps)) != 1 {
			t.Fatalf("expected cold Resume after Floor Relaxation to deterministically re-enter Floor Relaxation")
		}
	})
}
