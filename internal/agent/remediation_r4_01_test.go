package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/repository"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/toolset"
	"github.com/edynasty/LumenCortex/protocol"
)

type r401SettleStore struct {
	realSQLiteAgentStore
}

func (s r401SettleStore) SettleExecution(ctx context.Context, sid string, x session.ExecutionResult, change *TaskStateChange, edges ...session.DerivedEdge) (int64, error) {
	w, err := encodeTaskStateWrite(change)
	if err != nil {
		return 0, err
	}
	return s.s.SettleExecution(ctx, sid, x, w, edges...)
}

type r401EvalDelegatorTools struct {
	base *toolset.Set
	mu   sync.Mutex
	plan map[string][]r401NestedOp
}

type r401NestedOp struct {
	tool string
	args map[string]any
}

func (t *r401EvalDelegatorTools) Specs(allowlist []string) []protocol.ToolSpec {
	specs := t.base.Specs(allowlist)
	specs = append(specs, protocol.ToolSpec{
		Name:        "eval",
		Description: "sandboxed kernel execution with callback bridge",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"tag": map[string]any{"type": "string"},
			},
			"required": []string{"tag"},
		},
		Permission: "exec",
	})
	return specs
}

func (t *r401EvalDelegatorTools) Execute(ctx context.Context, name string, args map[string]any, onOut func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	if name != "eval" {
		return t.base.Execute(ctx, name, args, onOut)
	}
	caller, ok := protocol.ToolCallbackFromContext(ctx)
	if !ok {
		return protocol.ToolResult{OK: false, Content: "missing tool callback"}, nil
	}
	tag, _ := args["tag"].(string)
	t.mu.Lock()
	ops := append([]r401NestedOp(nil), t.plan[tag]...)
	t.mu.Unlock()
	var execIDs []string
	mutated := false
	for _, op := range ops {
		res, err := caller.Call(ctx, op.tool, op.args)
		if err != nil {
			return protocol.ToolResult{OK: false, Content: err.Error(), MutatesWorkspace: mutated || res.MutatesWorkspace}, nil
		}
		if res.MutatesWorkspace {
			mutated = true
		}
		if res.ExecutionID != "" {
			execIDs = append(execIDs, res.ExecutionID)
		}
	}
	return protocol.ToolResult{
		OK:               true,
		Content:          "nested=" + strings.Join(execIDs, ","),
		MutatesWorkspace: mutated,
	}, nil
}

func replayLiveTranscriptFromStore(t *testing.T, ctx context.Context, st Store, sid string) *run {
	t.Helper()
	type row struct {
		seq int64
		msg protocol.Message
	}
	var rows []row
	if err := st.Transcript(ctx, sid, 0, func(seq int64, m protocol.Message) error {
		rows = append(rows, row{seq: seq, msg: m})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rLive := &run{}
	pending := int64(-1)
	for _, rw := range rows {
		if rw.msg.Role != "tool" && pending >= 0 {
			if err := rLive.refreshGroupMutation(ctx, st, sid, pending); err != nil {
				t.Fatal(err)
			}
			pending = -1
		}
		rLive.appendRow(rw.seq, rw.msg)
		if rw.msg.Role == "assistant" && len(rw.msg.ToolCalls) > 0 {
			pending = rw.seq
		}
	}
	if pending >= 0 {
		if err := rLive.refreshGroupMutation(ctx, st, sid, pending); err != nil {
			t.Fatal(err)
		}
	}
	return rLive
}

func assertLiveColdStreamParity(t *testing.T, ctx context.Context, dbDir, sid string) (*run, RepairContext) {
	t.Helper()
	reopened, err := session.Open(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	st := r401SettleStore{realSQLiteAgentStore{s: reopened}}

	rCold := &run{}
	if err := rCold.load(ctx, st, sid, nil, 32768); err != nil {
		t.Fatal(err)
	}
	coldRep, err := rCold.deriveRepairContext(ctx, st, sid)
	if err != nil {
		t.Fatal(err)
	}

	rLive := replayLiveTranscriptFromStore(t, ctx, st, sid)
	liveRep, err := rLive.deriveRepairContext(ctx, st, sid)
	if err != nil {
		t.Fatal(err)
	}

	streamRep, err := StreamRepairContextFromStore(ctx, st, sid, 0)
	if err != nil {
		t.Fatal(err)
	}

	if liveRep != coldRep || streamRep != coldRep {
		t.Fatalf("R4-01 live/cold/stream RepairContext divergence:\nlive  =%+v\ncold  =%+v\nstream=%+v",
			liveRep, coldRep, streamRep)
	}
	if !reflect.DeepEqual(rLive.historyGroups, rCold.historyGroups) {
		t.Fatalf("R4-01 live vs cold HistoryGroupMeta mapping mismatch:\nlive=%+v\ncold=%+v",
			rLive.historyGroups, rCold.historyGroups)
	}
	return rCold, coldRep
}

// TestR401LiveLoopAndColdResumeExactIdentityCases covers all R4-01 requirements on the real Agent Loop,
// real Git workspace snapshots, real toolset.Set, and real SQLite persistence (including cold Resume):
//  1. Case A: invalid first call followed by valid same-name call;
//  2. Two valid same-name calls (no-op + proven, and proven + proven);
//  3. Top-level eval with single nested write_file;
//  4. Case B: nested write_file followed by top-level write_file;
//  5. Case C: read_file followed by eval with nested write_file;
//  6. Multiple nested executions inside one eval call;
//  7. Mixed successful and failed executions;
//  8. Unknown plus Proven (nested Unknown vetoing top-level Proven);
//  9. Historical/imported SQLite records without ToolCallID and unmatched ToolCallIDs;
//  10. Cold SQLite Resume continuation + live/cold/stream parity across every case.
func TestR401LiveLoopAndColdResumeExactIdentityCases(t *testing.T) {
	setup := func(t *testing.T, sid string, nestedPlan map[string][]r401NestedOp) (context.Context, string, string, *session.Store, r401SettleStore, *r401EvalDelegatorTools, *repository.Service) {
		t.Helper()
		ctx := context.Background()
		ws := filepath.Join(t.TempDir(), "ws")
		if err := os.MkdirAll(ws, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, content := range map[string]string{
			"a.txt":       "orig-a\n",
			"b.txt":       "orig-b\n",
			"nested.txt":  "orig-nested\n",
			"nested2.txt": "orig-nested2\n",
		} {
			if err := os.WriteFile(filepath.Join(ws, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		initGitRepoForTest(t, ws)
		dbDir := filepath.Join(t.TempDir(), sid+".db")
		raw, err := session.Open(dbDir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = raw.Close() })
		if err := raw.Create(ctx, session.Session{ID: sid, Goal: "R4-01 identity provenance", Status: "running"}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := raw.BeginRun(ctx, sid, session.RunStart{
			RunID: "run-seed",
			Input: &protocol.Message{Role: "user", Content: "fix failing checks"},
		}); err != nil {
			t.Fatal(err)
		}
		seed := r401SettleStore{realSQLiteAgentStore{s: raw}}
		if _, _, err := seed.AppendMessage(ctx, sid, protocol.Message{Role: "assistant", Content: "done"}, nil); err != nil {
			t.Fatal(err)
		}
		if _, _, err := seed.AppendMessage(ctx, sid, protocol.Message{
			Role:    "user",
			Origin:  protocol.OriginCompletionBlocked,
			Content: "Completion refused: unit tests failed.",
		}, nil); err != nil {
			t.Fatal(err)
		}
		if _, _, err := raw.BeginRun(ctx, sid, session.RunStart{RunID: "run-live"}); err != nil {
			t.Fatal(err)
		}
		baseTools, err := toolset.New(toolset.Options{Workspace: ws, Policy: toolset.PolicyFull, SessionID: sid})
		if err != nil {
			t.Fatal(err)
		}
		tools := &r401EvalDelegatorTools{base: baseTools, plan: nestedPlan}
		repo, err := repository.New(ws)
		if err != nil {
			t.Fatal(err)
		}
		return ctx, ws, dbDir, raw, seed, tools, repo
	}

	t.Run("case_A_invalid_first_call_then_valid_same_name_call", func(t *testing.T) {
		ctx, _, dbDir, raw, st, tools, repo := setup(t, "s-r401-a", nil)
		const ref = config.ModelRef("test/m")
		prov := &scriptedProvider{responses: []protocol.ProviderResponse{
			toolStep(
				call("c1-bad", "write_file", `not valid json`),
				call("c2-good", "write_file", `{"path":"a.txt","content":"fixed-a\n"}`),
			),
			answerOf("fixed"),
		}}
		res, err := (&Loop{
			Models:    map[config.ModelRef]Model{ref: testModel(ref, prov)},
			Store:     st,
			Tools:     tools,
			Snapshots: repo,
		}).Run(ctx, "s-r401-a", Options{Session: ref, RunID: "run-live", Resumed: true, MaxSteps: 4})
		if err != nil || res.Status != StatusCompleted {
			t.Fatalf("run failed: status=%s err=%v", res.Status, err)
		}
		execs, err := st.ListAnsweredExecutions(ctx, "s-r401-a", 0)
		if err != nil || len(execs) != 1 || execs[0].ToolCallID != "c2-good" || execs[0].ParentToolCallID != "" {
			t.Fatalf("execs=%+v err=%v, want 1 execution bound to c2-good", execs, err)
		}
		exID := execs[0].ExecutionID
		_ = raw.Close()

		rCold, coldRep := assertLiveColdStreamParity(t, ctx, dbDir, "s-r401-a")
		if coldRep.CausalProgressLen != 1 {
			t.Fatalf("CausalProgressLen=%d, want 1 (%+v)", coldRep.CausalProgressLen, coldRep)
		}
		got := coldRep.CausalProgress[0]
		if got.ToolCallID != "c2-good" || got.ParentToolCallID != "" || got.ExecutionID != exID || got.ToolName != "write_file" || got.MutationEvidence != MutationProven {
			t.Fatalf("R4-01 Case A misassociation: got ref=%+v, want ToolCallID=c2-good ExecutionID=%s", got, exID)
		}
		for _, g := range rCold.historyGroups {
			if g.HasToolCalls {
				if g.ToolCalls[0].ExecutionID != "" || g.ToolCalls[1].ExecutionID != exID {
					t.Fatalf("per-call binding wrong: %+v", g.ToolCalls)
				}
			}
		}
	})

	t.Run("two_valid_same_name_calls_stay_distinguishable", func(t *testing.T) {
		ctx, _, dbDir, raw, st, tools, repo := setup(t, "s-r401-2same", nil)
		const ref = config.ModelRef("test/m")
		// c1 writes identical content (MutationNone), c2 writes changed content (MutationProven).
		prov := &scriptedProvider{responses: []protocol.ProviderResponse{
			toolStep(
				call("c1-noop", "write_file", `{"path":"a.txt","content":"orig-a\n"}`),
				call("c2-mut", "write_file", `{"path":"b.txt","content":"mutated-b\n"}`),
			),
			answerOf("fixed"),
		}}
		res, err := (&Loop{
			Models:    map[config.ModelRef]Model{ref: testModel(ref, prov)},
			Store:     st,
			Tools:     tools,
			Snapshots: repo,
		}).Run(ctx, "s-r401-2same", Options{Session: ref, RunID: "run-live", Resumed: true, MaxSteps: 4})
		if err != nil || res.Status != StatusCompleted {
			t.Fatalf("run failed: status=%s err=%v", res.Status, err)
		}
		execs, err := st.ListAnsweredExecutions(ctx, "s-r401-2same", 0)
		if err != nil || len(execs) != 2 {
			t.Fatalf("execs=%+v err=%v", execs, err)
		}
		_ = raw.Close()

		rCold, coldRep := assertLiveColdStreamParity(t, ctx, dbDir, "s-r401-2same")
		got := coldRep.CausalProgress[0]
		if got.ToolCallID != "c2-mut" || got.ExecutionID != execs[1].ExecutionID || got.PostRef != execs[1].PostRef || got.MutationEvidence != MutationProven {
			t.Fatalf("got ref=%+v, want c2-mut / %s", got, execs[1].ExecutionID)
		}
		for _, g := range rCold.historyGroups {
			if g.HasToolCalls {
				if g.ToolCalls[0].ExecutionID != execs[0].ExecutionID || g.ToolCalls[0].ExecEvidence != MutationNone ||
					g.ToolCalls[1].ExecutionID != execs[1].ExecutionID || g.ToolCalls[1].ExecEvidence != MutationProven {
					t.Fatalf("same-name calls confused: %+v", g.ToolCalls)
				}
			}
		}
	})

	t.Run("top_level_eval_with_single_and_multiple_nested_executions", func(t *testing.T) {
		ctx, _, dbDir, raw, st, tools, repo := setup(t, "s-r401-multi-nested", map[string][]r401NestedOp{
			"multi": {
				{tool: "read_file", args: map[string]any{"path": "a.txt"}},
				{tool: "write_file", args: map[string]any{"path": "nested.txt", "content": "n1\n"}},
				{tool: "write_file", args: map[string]any{"path": "nested2.txt", "content": "n2\n"}},
			},
		})
		const ref = config.ModelRef("test/m")
		prov := &scriptedProvider{responses: []protocol.ProviderResponse{
			toolStep(call("c-eval", "eval", `{"tag":"multi"}`)),
			answerOf("fixed"),
		}}
		res, err := (&Loop{
			Models:    map[config.ModelRef]Model{ref: testModel(ref, prov)},
			Store:     st,
			Tools:     tools,
			Snapshots: repo,
		}).Run(ctx, "s-r401-multi-nested", Options{Session: ref, RunID: "run-live", Resumed: true, MaxSteps: 4})
		if err != nil || res.Status != StatusCompleted {
			t.Fatalf("run failed: status=%s err=%v", res.Status, err)
		}
		execs, err := st.ListAnsweredExecutions(ctx, "s-r401-multi-nested", 0)
		if err != nil || len(execs) != 2 {
			t.Fatalf("execs=%+v err=%v, want 2 nested write executions", execs, err)
		}
		if execs[0].ToolCallID != "c-eval:cb2" || execs[0].ParentToolCallID != "c-eval" ||
			execs[1].ToolCallID != "c-eval:cb3" || execs[1].ParentToolCallID != "c-eval" {
			t.Fatalf("nested parent/call IDs wrong: %+v", execs)
		}
		_ = raw.Close()

		rCold, coldRep := assertLiveColdStreamParity(t, ctx, dbDir, "s-r401-multi-nested")
		got := coldRep.CausalProgress[0]
		if got.ToolCallID != "c-eval:cb2" || got.ParentToolCallID != "c-eval" ||
			got.ToolName != "write_file" || got.ExecutionID != execs[0].ExecutionID || got.MutationEvidence != MutationProven {
			t.Fatalf("unexpected nested causal ref: %+v", got)
		}
		for _, g := range rCold.historyGroups {
			if g.HasToolCalls {
				if g.ToolCalls[0].ExecutionID != "" || len(g.ToolCalls[0].Nested) != 2 {
					t.Fatalf("expected top-level eval ExecutionID='' and 2 Nested entries, got %+v", g.ToolCalls[0])
				}
			}
		}
	})

	t.Run("case_B_eval_nested_write_then_top_level_write", func(t *testing.T) {
		ctx, _, dbDir, raw, st, tools, repo := setup(t, "s-r401-b", map[string][]r401NestedOp{
			"k1": {{tool: "write_file", args: map[string]any{"path": "nested.txt", "content": "from-eval\n"}}},
		})
		const ref = config.ModelRef("test/m")
		prov := &scriptedProvider{responses: []protocol.ProviderResponse{
			toolStep(
				call("c1-eval", "eval", `{"tag":"k1"}`),
				call("c2-top", "write_file", `{"path":"b.txt","content":"from-top\n"}`),
			),
			answerOf("fixed"),
		}}
		res, err := (&Loop{
			Models:    map[config.ModelRef]Model{ref: testModel(ref, prov)},
			Store:     st,
			Tools:     tools,
			Snapshots: repo,
		}).Run(ctx, "s-r401-b", Options{Session: ref, RunID: "run-live", Resumed: true, MaxSteps: 4})
		if err != nil || res.Status != StatusCompleted {
			t.Fatalf("run failed: status=%s err=%v", res.Status, err)
		}
		execs, err := st.ListAnsweredExecutions(ctx, "s-r401-b", 0)
		if err != nil || len(execs) != 2 {
			t.Fatalf("execs=%+v err=%v, want 2 executions (nested + top-level)", execs, err)
		}
		nestedExecID := execs[0].ExecutionID
		topExecID := execs[1].ExecutionID
		_ = raw.Close()

		rCold, coldRep := assertLiveColdStreamParity(t, ctx, dbDir, "s-r401-b")
		var groupMeta *HistoryGroupMeta
		for i := range rCold.historyGroups {
			if rCold.historyGroups[i].HasToolCalls {
				groupMeta = &rCold.historyGroups[i]
				break
			}
		}
		if groupMeta == nil || len(groupMeta.ToolCalls) != 2 {
			t.Fatalf("missing tool group meta: %+v", groupMeta)
		}
		if groupMeta.ToolCalls[0].ExecutionID != "" || len(groupMeta.ToolCalls[0].Nested) != 1 || groupMeta.ToolCalls[0].Nested[0].ExecutionID != nestedExecID {
			t.Fatalf("c1-eval nested binding wrong: %+v", groupMeta.ToolCalls[0])
		}
		if groupMeta.ToolCalls[1].ExecutionID != topExecID {
			t.Fatalf("R4-01 Case B misassociation: c2-top bound to ExecutionID=%q (nested=%s), want topExecID=%s",
				groupMeta.ToolCalls[1].ExecutionID, nestedExecID, topExecID)
		}
		got := coldRep.CausalProgress[0]
		if got.ToolCallID != "c1-eval:cb1" || got.ParentToolCallID != "c1-eval" || got.ExecutionID != nestedExecID || got.ToolName != "write_file" {
			t.Fatalf("unexpected primary ref for Case B: %+v", got)
		}
	})

	t.Run("case_C_read_file_then_eval_nested_write_and_cold_resume_run", func(t *testing.T) {
		ctx, ws, dbDir, raw, st, tools, repo := setup(t, "s-r401-c", map[string][]r401NestedOp{
			"k2": {{tool: "write_file", args: map[string]any{"path": "nested.txt", "content": "nested-only\n"}}},
		})
		const ref = config.ModelRef("test/m")
		// MaxSteps: 1 stops after the tool step so we also exercise a real cold Loop.Run(Resumed: true)!
		prov1 := &scriptedProvider{responses: []protocol.ProviderResponse{
			toolStep(
				call("c0-read", "read_file", `{"path":"a.txt"}`),
				call("c1-eval", "eval", `{"tag":"k2"}`),
			),
		}}
		res1, err := (&Loop{
			Models:    map[config.ModelRef]Model{ref: testModel(ref, prov1)},
			Store:     st,
			Tools:     tools,
			Snapshots: repo,
		}).Run(ctx, "s-r401-c", Options{Session: ref, RunID: "run-live", Resumed: true, MaxSteps: 1})
		if res1.Status != StatusInterrupted {
			t.Fatalf("part1 status=%s err=%v, want interrupted at max steps", res1.Status, err)
		}
		execs, err := st.ListAnsweredExecutions(ctx, "s-r401-c", 0)
		if err != nil || len(execs) != 1 {
			t.Fatalf("execs=%+v err=%v, want 1 nested execution", execs, err)
		}
		nestedExecID := execs[0].ExecutionID
		_ = raw.Close()

		_, coldRep := assertLiveColdStreamParity(t, ctx, dbDir, "s-r401-c")
		got := coldRep.CausalProgress[0]
		if got.ToolCallID != "c1-eval:cb1" || got.ParentToolCallID != "c1-eval" ||
			got.ToolName != "write_file" || got.ExecutionID != nestedExecID || got.MutationEvidence != MutationProven {
			t.Fatalf("R4-01 Case C misassociation: got ref=%+v, want c1-eval:cb1 (parent c1-eval) exec=%s", got, nestedExecID)
		}
		rendered := appendPinnedGateAndRelaxationNotice("", coldRep, coldRep.MinRequiredSeq, "", coldRep.MinRequiredSeq, -1)
		if !strings.Contains(rendered, "tool=write_file exec="+nestedExecID+" evidence=proven call=c1-eval:cb1 parent=c1-eval") {
			t.Fatalf("rendered ledger missing nested provenance:\n%s", rendered)
		}

		// Execute a real cold Loop.Run(Resumed: true) on the reopened SQLite DB!
		reopened, err := session.Open(dbDir)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = reopened.Close() }()
		if _, err := reopened.Reconcile(ctx, "s-r401-c"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := reopened.BeginRun(ctx, "s-r401-c", session.RunStart{RunID: "run-resume-2"}); err != nil {
			t.Fatal(err)
		}
		st2 := r401SettleStore{realSQLiteAgentStore{s: reopened}}
		baseTools2, err := toolset.New(toolset.Options{Workspace: ws, Policy: toolset.PolicyFull, SessionID: "s-r401-c"})
		if err != nil {
			t.Fatal(err)
		}
		repo2, err := repository.New(ws)
		if err != nil {
			t.Fatal(err)
		}
		res2, err := (&Loop{
			Models:    map[config.ModelRef]Model{ref: testModel(ref, &scriptedProvider{responses: []protocol.ProviderResponse{answerOf("completed after cold resume")}})},
			Store:     st2,
			Tools:     baseTools2,
			Snapshots: repo2,
		}).Run(ctx, "s-r401-c", Options{Session: ref, RunID: "run-resume-2", Resumed: true, MaxSteps: 2})
		if err != nil || res2.Status != StatusCompleted {
			t.Fatalf("cold Resume run failed: status=%s err=%v", res2.Status, err)
		}
	})

	t.Run("mixed_failed_and_proven_plus_unknown_veto_over_proven", func(t *testing.T) {
		ctx, _, dbDir, raw, st, tools, repo := setup(t, "s-r401-mixed", map[string][]r401NestedOp{
			// replace_in_file on missing old_text fails cleanly in editing.Engine -> OutcomeFailed -> MutationUnknown!
			"fail-nested": {{tool: "replace_in_file", args: map[string]any{"path": "nested.txt", "old_text": "NO_SUCH_TEXT", "new_text": "x"}}},
		})
		const ref = config.ModelRef("test/m")
		prov := &scriptedProvider{responses: []protocol.ProviderResponse{
			// Step 1: c1 replace_in_file fails (OutcomeFailed -> Unknown), c2 write_file succeeds (Proven) -> Unknown wins, bound to c1!
			toolStep(
				call("c1-fail", "replace_in_file", `{"path":"a.txt","old_text":"MISSING","new_text":"x"}`),
				call("c2-ok", "write_file", `{"path":"b.txt","content":"b-v2\n"}`),
			),
			// Step 2: c3 write_file succeeds (Proven), c4 eval has nested failed replace_in_file (Unknown) -> Unknown wins, bound to c4:cb1 (parent c4)!
			toolStep(
				call("c3-prov", "write_file", `{"path":"a.txt","content":"a-v2\n"}`),
				call("c4-eval", "eval", `{"tag":"fail-nested"}`),
			),
			answerOf("done"),
		}}
		res, err := (&Loop{
			Models:    map[config.ModelRef]Model{ref: testModel(ref, prov)},
			Store:     st,
			Tools:     tools,
			Snapshots: repo,
		}).Run(ctx, "s-r401-mixed", Options{Session: ref, RunID: "run-live", Resumed: true, MaxSteps: 5})
		if err != nil || res.Status != StatusCompleted {
			t.Fatalf("run failed: status=%s err=%v", res.Status, err)
		}
		_ = raw.Close()

		_, coldRep := assertLiveColdStreamParity(t, ctx, dbDir, "s-r401-mixed")
		if coldRep.CausalProgressLen != 2 || coldRep.PostBlockUnknownCount != 2 || coldRep.PostBlockProvenCount != 0 {
			t.Fatalf("unexpected mixed/unknown causal counts: %+v", coldRep)
		}
		if coldRep.CausalProgress[0].ToolCallID != "c1-fail" || coldRep.CausalProgress[0].MutationEvidence != MutationUnknown {
			t.Fatalf("step 1 ref = %+v, want c1-fail Unknown", coldRep.CausalProgress[0])
		}
		if coldRep.CausalProgress[1].ToolCallID != "c4-eval:cb1" || coldRep.CausalProgress[1].ParentToolCallID != "c4-eval" ||
			coldRep.CausalProgress[1].ToolName != "replace_in_file" || coldRep.CausalProgress[1].MutationEvidence != MutationUnknown {
			t.Fatalf("step 2 ref = %+v, want c4-eval:cb1 (parent c4-eval) Unknown", coldRep.CausalProgress[1])
		}
	})

	t.Run("legacy_and_unmatched_records_on_real_sqlite_stay_unbound_unknown", func(t *testing.T) {
		ctx := context.Background()
		dbDir := filepath.Join(t.TempDir(), "legacy.db")
		raw, err := session.Open(dbDir)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = raw.Close() }()
		const sid = "s-r401-legacy"
		if err := raw.Create(ctx, session.Session{ID: sid, Goal: "legacy null derived_from", Status: "running"}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := raw.BeginRun(ctx, sid, session.RunStart{
			RunID: "run-leg",
			Input: &protocol.Message{Role: "user", Content: "repair"},
		}); err != nil {
			t.Fatal(err)
		}
		st := r401SettleStore{realSQLiteAgentStore{s: raw}}
		_, _, _ = st.AppendMessage(ctx, sid, protocol.Message{Role: "assistant", Content: "done"}, nil)
		bSeq, _, _ := st.AppendMessage(ctx, sid, protocol.Message{
			Role: "user", Origin: protocol.OriginCompletionBlocked, Content: "gate refused",
		}, nil)

		f0, l0 := int64(0), bSeq
		_, err = st.AdmitRequest(ctx, session.RequestAdmission{
			RequestID: "req-leg", SessionID: sid, RunID: "run-leg", Purpose: session.PurposeMain,
			Step: 2, FirstSeq: &f0, LastSeq: &l0, ProjectionVersion: 2,
			Refs: []string{"model:p/m", "proj:mode:compact", "ts:v0"},
		})
		if err != nil {
			t.Fatal(err)
		}
		asst := protocol.Message{
			Role: "assistant",
			ToolCalls: []protocol.ToolCall{
				{ID: "c-leg-1", Name: "write_file", Arguments: json.RawMessage(`{"path":"x.txt","content":"1"}`)},
				{ID: "c-leg-2", Name: "write_file", Arguments: json.RawMessage(`{"path":"y.txt","content":"2"}`)},
			},
		}
		if _, err := st.AppendAnswered(ctx, sid, "req-leg", asst); err != nil {
			t.Fatal(err)
		}
		_, _, _ = st.CommitToolResult(ctx, ToolResultCommit{
			SessionID: sid, RunID: "run-leg",
			Message: protocol.Message{Role: "tool", ToolCallID: "c-leg-1", Name: "write_file", Content: "invalid args"},
		})
		// Historical admission WITHOUT ToolCallID (derived_from IS NULL in SQLite)!
		const shaA = "1111111111111111111111111111111111111111111111111111111111111111"
		const shaB = "2222222222222222222222222222222222222222222222222222222222222222"
		if err := st.AdmitExecution(ctx, session.ExecutionAdmission{
			ExecutionID: "ex-legacy-sql", SessionID: sid, RunID: "run-leg", RequestID: "req-leg",
			ToolName: "write_file", ArgsDigest: "sha256:" + shaA, PreRef: "files:1:sha256:" + shaA,
		}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := st.CommitToolResult(ctx, ToolResultCommit{
			SessionID: sid, RunID: "run-leg",
			Message: protocol.Message{Role: "tool", ToolCallID: "c-leg-2", Name: "write_file", Content: "ok"},
			Execution: &session.ExecutionResult{
				ExecutionID: "ex-legacy-sql", Outcome: session.OutcomeComplete, Coverage: session.CoverageComplete,
				PostRef: "files:1:sha256:" + shaB + ";cm=proven",
			},
		}); err != nil {
			t.Fatal(err)
		}
		_ = raw.Close()

		rCold, coldRep := assertLiveColdStreamParity(t, ctx, dbDir, sid)
		if coldRep.CausalProgressLen != 1 || coldRep.Stage != RepairStageUnknownMutated {
			t.Fatalf("legacy record must conservatively remain Unknown, got %+v", coldRep)
		}
		ref := coldRep.CausalProgress[0]
		if ref.ToolCallID != "" || ref.ParentToolCallID != "" || ref.ExecutionID != "ex-legacy-sql" || ref.MutationEvidence != MutationUnknown {
			t.Fatalf("legacy ref must keep ToolCallID='' and ExecutionID=ex-legacy-sql with Unknown evidence, got %+v", ref)
		}
		for _, g := range rCold.historyGroups {
			if g.HasToolCalls {
				if g.ToolCalls[0].ExecutionID != "" || g.ToolCalls[1].ExecutionID != "" {
					t.Fatalf("legacy execution falsely inferred onto a call: %+v", g.ToolCalls)
				}
			}
		}

		// Also verify an execution carrying a ghost/unmatched ToolCallID never falls back to another call.
		metaGhost := buildAssistantHistoryMeta(99, asst)
		applyToolRowToHistoryMeta(&metaGhost, 100, protocol.Message{Role: "tool", ToolCallID: "c-leg-1", Content: "ok"})
		applyToolRowToHistoryMeta(&metaGhost, 101, protocol.Message{Role: "tool", ToolCallID: "c-leg-2", Content: "ok"})
		applyExecutionSummariesToMeta(&metaGhost, []session.AnsweredExecutionSummary{{
			AnsweredSeq: 99, RequestID: "req-g", ExecutionID: "ex-ghost",
			ToolCallID: "c-does-not-exist", ToolName: "write_file",
			PreRef: "files:1:sha256:" + shaA, PostRef: "files:1:sha256:" + shaB + ";cm=proven",
			Outcome: session.OutcomeComplete, Coverage: session.CoverageComplete,
		}})
		if metaGhost.ToolCalls[0].ExecutionID != "" || metaGhost.ToolCalls[1].ExecutionID != "" ||
			metaGhost.ToolCallID != "" || metaGhost.MutationEvidence != MutationUnknown {
			t.Fatalf("unmatched ToolCallID fell back to another call: %+v", metaGhost)
		}
	})
}
