//go:build !windows

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/editing"
	"github.com/edynasty/LumenCortex/internal/repository"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/toolset"
	"github.com/edynasty/LumenCortex/protocol"
)

type r402CapturingStore struct {
	r401SettleStore
	mu      *sync.Mutex
	commits *[]ToolResultCommit
}

func (s r402CapturingStore) CommitToolResult(ctx context.Context, c ToolResultCommit) (int64, int64, error) {
	if s.mu != nil && s.commits != nil {
		s.mu.Lock()
		copied := c
		copied.Edges = append([]session.DerivedEdge(nil), c.Edges...)
		if c.Execution != nil {
			exCopy := *c.Execution
			copied.Execution = &exCopy
		}
		*s.commits = append(*s.commits, copied)
		s.mu.Unlock()
	}
	return s.r401SettleStore.CommitToolResult(ctx, c)
}

// TestR402EndToEndPostPublicationAndUncertainResume verifies R4-02 through the real Agent Loop,
// real Git SnapshotFiles/SnapshotWorkspace, real toolset.Set + editing.Engine, and real SQLite
// persistence across live runs and cold Resume:
//  1. Post-publication Stat/Close failure after write_file publishes a file on disk;
//  2. Uncertain publication (EIO during atomic link, both with and without destination visibility)
//     preserving OutcomeUnknown + MutationUnknown (never MutationNone) across cold SQLite Resume;
//  3. Multi-file apply_patch partial publication where external append prevents rollback of file 1
//     while file 2 fails.
func TestR402EndToEndPostPublicationAndUncertainResume(t *testing.T) {
	const ref = config.ModelRef("test/m")

	setup := func(t *testing.T, sid string) (context.Context, string, string, *session.Store, r402CapturingStore, *editing.Engine, *toolset.Set, *repository.Service, *[]ToolResultCommit) {
		t.Helper()
		ctx := context.Background()
		ws := filepath.Join(t.TempDir(), "ws")
		if err := os.MkdirAll(ws, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ws, "existing.txt"), []byte("orig-existing\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		initGitRepoForTest(t, ws)

		dbDir := filepath.Join(t.TempDir(), sid+".db")
		raw, err := session.Open(dbDir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = raw.Close() })
		if err := raw.Create(ctx, session.Session{ID: sid, Goal: "R4-02 transaction provenance", Status: "running"}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := raw.BeginRun(ctx, sid, session.RunStart{
			RunID: "run-seed",
			Input: &protocol.Message{Role: "user", Content: "create output files"},
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
			Content: "Completion refused: missing required artifact.",
		}, nil); err != nil {
			t.Fatal(err)
		}
		if _, _, err := raw.BeginRun(ctx, sid, session.RunStart{RunID: "run-live"}); err != nil {
			t.Fatal(err)
		}

		editEngine, err := editing.New(editing.Options{Root: ws})
		if err != nil {
			t.Fatal(err)
		}
		tools, err := toolset.New(toolset.Options{
			Workspace: ws,
			Policy:    toolset.PolicyFull,
			SessionID: sid,
			Editing:   editEngine,
		})
		if err != nil {
			t.Fatal(err)
		}
		repo, err := repository.New(ws)
		if err != nil {
			t.Fatal(err)
		}
		var mu sync.Mutex
		var commits []ToolResultCommit
		st := r402CapturingStore{r401SettleStore: seed, mu: &mu, commits: &commits}
		return ctx, ws, dbDir, raw, st, editEngine, tools, repo, &commits
	}

	t.Run("post_publish_close_failure_preserves_disk_edge_unknown_and_resume", func(t *testing.T) {
		ctx, ws, dbDir, raw, st, editEngine, tools, repo, commits := setup(t, "s-r402-postpub")
		editEngine.SetPublishFaultHooks(editing.PublishFaultHooks{
			CloseAfterPublish: func(string) error { return syscall.EIO },
		})

		prov := &scriptedProvider{responses: []protocol.ProviderResponse{
			toolStep(call("c-pub-err", "write_file", `{"path":"published_err.txt","content":"published\n"}`)),
			answerOf("inspected after post-publish error"),
		}}
		models := map[config.ModelRef]Model{ref: testModel(ref, prov)}
		res1, _ := (&Loop{
			Models:    models,
			Tools:     tools,
			Store:     st,
			Snapshots: repo,
		}).Run(ctx, "s-r402-postpub", Options{Session: ref, RunID: "run-live", Resumed: true, MaxSteps: 1})
		if res1.Status != StatusInterrupted {
			t.Fatalf("status=%s, want interrupted at MaxSteps=1", res1.Status)
		}

		// 1. Actual disk contents must hold the published file.
		diskBytes, rerr := os.ReadFile(filepath.Join(ws, "published_err.txt"))
		if rerr != nil || string(diskBytes) != "published\n" {
			t.Fatalf("disk published_err.txt = %q err=%v", diskBytes, rerr)
		}

		// 2. ExecutionResult & DerivedEdge persisted to SQLite.
		if len(*commits) != 1 || (*commits)[0].Execution == nil {
			t.Fatalf("commits = %+v, want 1 settled execution", *commits)
		}
		execRes := (*commits)[0].Execution
		if execRes.Outcome != session.OutcomeUnknown || !strings.HasSuffix(execRes.PostRef, ";cm=unknown") {
			t.Fatalf("ExecutionResult = %+v, want OutcomeUnknown and ;cm=unknown", execRes)
		}
		edges := (*commits)[0].Edges
		if len(edges) != 1 || edges[0].Kind != "file" || !strings.HasPrefix(edges[0].TargetRef, "published_err.txt@") || edges[0].LineageComplete {
			t.Fatalf("DerivedEdges = %+v, want 1 incomplete file edge for published_err.txt", edges)
		}

		sums, err := st.ListAnsweredExecutions(ctx, "s-r402-postpub", 0)
		if err != nil || len(sums) != 1 {
			t.Fatalf("sums=%+v err=%v", sums, err)
		}
		if sums[0].ToolCallID != "c-pub-err" || sums[0].Outcome != session.OutcomeUnknown || sums[0].FileEdgeCount != 1 {
			t.Fatalf("persisted summary = %+v", sums[0])
		}
		_ = raw.Close()

		// 3. Cold SQLite Resume must reconstruct identical MutationUnknown (never MutationNone!) and run cleanly.
		_, coldRep := assertLiveColdStreamParity(t, ctx, dbDir, "s-r402-postpub")
		if !coldRep.Active || coldRep.PostBlockUnknownCount != 1 || coldRep.PostBlockProvenCount != 0 {
			t.Fatalf("coldRep = %+v, want PostBlockUnknownCount=1", coldRep)
		}
		if coldRep.CausalProgress[0].MutationEvidence != MutationUnknown ||
			coldRep.CausalProgress[0].ToolCallID != "c-pub-err" ||
			coldRep.CausalProgress[0].ExecutionID != execRes.ExecutionID {
			t.Fatalf("CausalProgress[0] = %+v", coldRep.CausalProgress[0])
		}

		reopened, err := session.Open(dbDir)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = reopened.Close() }()
		if _, err := reopened.Reconcile(ctx, "s-r402-postpub"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := reopened.BeginRun(ctx, "s-r402-postpub", session.RunStart{RunID: "run-resume"}); err != nil {
			t.Fatal(err)
		}
		editEngine.SetPublishFaultHooks(editing.PublishFaultHooks{})
		res2, err := (&Loop{
			Models:    models,
			Tools:     tools,
			Store:     r401SettleStore{realSQLiteAgentStore{s: reopened}},
			Snapshots: repo,
		}).Run(ctx, "s-r402-postpub", Options{Session: ref, RunID: "run-resume", Resumed: true, MaxSteps: 4})
		if err != nil || res2.Status != StatusCompleted {
			t.Fatalf("cold Resume failed: res=%+v err=%v", res2, err)
		}
	})

	t.Run("uncertain_publication_eio_never_becomes_mutation_none_across_resume", func(t *testing.T) {
		ctx, ws, dbDir, raw, st, editEngine, tools, repo, commits := setup(t, "s-r402-uncertain")
		editEngine.SetPublishFaultHooks(editing.PublishFaultHooks{
			PublishUncertainErr: func(string) error { return syscall.EIO },
		})

		prov := &scriptedProvider{responses: []protocol.ProviderResponse{
			toolStep(call("c-uncertain", "write_file", `{"path":"uncertain.txt","content":"maybe\n"}`)),
			answerOf("recovered after uncertain publish"),
		}}
		models := map[config.ModelRef]Model{ref: testModel(ref, prov)}
		res1, _ := (&Loop{
			Models:    models,
			Tools:     tools,
			Store:     st,
			Snapshots: repo,
		}).Run(ctx, "s-r402-uncertain", Options{Session: ref, RunID: "run-live", Resumed: true, MaxSteps: 1})
		if res1.Status != StatusInterrupted {
			t.Fatalf("status=%s, want interrupted at MaxSteps=1", res1.Status)
		}
		if _, serr := os.Stat(filepath.Join(ws, "uncertain.txt")); !os.IsNotExist(serr) {
			t.Fatalf("uncertain.txt should be absent in this subcase: %v", serr)
		}
		// Even though PreRef == PostRef and FileEdgeCount == 0 on disk, DirtyFailure MUST force
		// OutcomeUnknown + ;cm=unknown + MutationUnknown (never OutcomeFailed or MutationNone!).
		if len(*commits) != 1 || (*commits)[0].Execution == nil {
			t.Fatalf("commits = %+v", *commits)
		}
		execRes := (*commits)[0].Execution
		if execRes.Outcome != session.OutcomeUnknown || !strings.HasSuffix(execRes.PostRef, ";cm=unknown") {
			t.Fatalf("uncertain ExecutionResult = %+v, want OutcomeUnknown and ;cm=unknown", execRes)
		}
		_ = raw.Close()

		_, coldRep := assertLiveColdStreamParity(t, ctx, dbDir, "s-r402-uncertain")
		if coldRep.CausalProgress[0].MutationEvidence != MutationUnknown ||
			coldRep.CausalProgress[0].ToolCallID != "c-uncertain" ||
			coldRep.CausalProgress[0].ExecutionID != execRes.ExecutionID {
			t.Fatalf("coldRep CausalProgress[0] = %+v, want MutationUnknown", coldRep.CausalProgress[0])
		}

		reopened, err := session.Open(dbDir)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = reopened.Close() }()
		if _, err := reopened.Reconcile(ctx, "s-r402-uncertain"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := reopened.BeginRun(ctx, "s-r402-uncertain", session.RunStart{RunID: "run-resume"}); err != nil {
			t.Fatal(err)
		}
		editEngine.SetPublishFaultHooks(editing.PublishFaultHooks{})
		res2, err := (&Loop{
			Models:    models,
			Tools:     tools,
			Store:     r401SettleStore{realSQLiteAgentStore{s: reopened}},
			Snapshots: repo,
		}).Run(ctx, "s-r402-uncertain", Options{Session: ref, RunID: "run-resume", Resumed: true, MaxSteps: 4})
		if err != nil || res2.Status != StatusCompleted {
			t.Fatalf("cold Resume failed: res=%+v err=%v", res2, err)
		}
	})

	t.Run("multifile_partial_publish_external_append_records_edge_and_unknown_across_resume", func(t *testing.T) {
		ctx, ws, dbDir, raw, st, editEngine, tools, repo, commits := setup(t, "s-r402-multi")
		aPath := filepath.Join(ws, "a_multi.txt")
		existingPath := filepath.Join(ws, "existing.txt")
		editEngine.SetPublishFaultHooks(editing.PublishFaultHooks{
			AfterLinkSuccess: func(dst string) error {
				if filepath.Base(dst) == "a_multi.txt" {
					af, oerr := os.OpenFile(aPath, os.O_WRONLY|os.O_APPEND, 0)
					if oerr != nil {
						return oerr
					}
					_, _ = af.Write([]byte("external-tail\n"))
					_ = af.Close()
					_ = os.WriteFile(existingPath, []byte("raced-existing\n"), 0o644)
				}
				return nil
			},
		})

		prov := &scriptedProvider{responses: []protocol.ProviderResponse{
			toolStep(call("c-multi", "apply_patch", `{"patches":[{"path":"a_multi.txt","operation":"create","content":"agent-a\n"},{"path":"existing.txt","operation":"update","edits":[{"old_text":"orig-existing","new_text":"new-existing"}]}]}`)),
		}}
		models := map[config.ModelRef]Model{ref: testModel(ref, prov)}
		res1, _ := (&Loop{
			Models:    models,
			Tools:     tools,
			Store:     st,
			Snapshots: repo,
		}).Run(ctx, "s-r402-multi", Options{Session: ref, RunID: "run-live", Resumed: true, MaxSteps: 1})
		if res1.Status != StatusInterrupted {
			t.Fatalf("status=%s, want interrupted at MaxSteps=1", res1.Status)
		}
		gotA, rerr := os.ReadFile(aPath)
		if rerr != nil || string(gotA) != "agent-a\nexternal-tail\n" {
			t.Fatalf("a_multi.txt = %q err=%v", gotA, rerr)
		}
		if len(*commits) != 1 || (*commits)[0].Execution == nil {
			t.Fatalf("commits = %+v", *commits)
		}
		execRes := (*commits)[0].Execution
		if execRes.Outcome != session.OutcomeUnknown || !strings.HasSuffix(execRes.PostRef, ";cm=unknown") {
			t.Fatalf("ExecutionResult = %+v", execRes)
		}
		if len((*commits)[0].Edges) < 1 {
			t.Fatalf("expected DerivedEdges recorded for changed targets, got %+v", (*commits)[0].Edges)
		}
		_ = raw.Close()

		_, coldRep := assertLiveColdStreamParity(t, ctx, dbDir, "s-r402-multi")
		if coldRep.CausalProgress[0].MutationEvidence != MutationUnknown ||
			coldRep.CausalProgress[0].ToolCallID != "c-multi" ||
			coldRep.CausalProgress[0].ExecutionID != execRes.ExecutionID {
			t.Fatalf("coldRep CausalProgress[0] = %+v", coldRep.CausalProgress[0])
		}
	})
}
