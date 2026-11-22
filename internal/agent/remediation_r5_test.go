package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/editing"
	"github.com/edynasty/LumenCortex/internal/repository"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/toolset"
	"github.com/edynasty/LumenCortex/protocol"
)

type r5EvalBridgeTools struct {
	inner     *toolset.Set
	onEval    func(ctx context.Context, cb protocol.ToolCaller) (protocol.ToolResult, error)
	evalSpecs []protocol.ToolSpec
}

func (t *r5EvalBridgeTools) Specs(allow []string) []protocol.ToolSpec {
	return append(t.inner.Specs(allow), t.evalSpecs...)
}

func (t *r5EvalBridgeTools) Execute(ctx context.Context, name string, args map[string]any, emit func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	if name == "eval_go" {
		cb, ok := protocol.ToolCallbackFromContext(ctx)
		if !ok || cb == nil {
			return protocol.ToolResult{}, errors.New("missing tool callback")
		}
		return t.onEval(ctx, cb)
	}
	return t.inner.Execute(ctx, name, args, emit)
}

// TestR5ResultPropagationAndResumeIntegrity verifies ADR-0024 R4-02-D end-to-end across:
//  1. Top-level multi-file apply_patch with external modification + Close failure -> disk preserved,
//     bounded partial-publication JSON persisted to SQLite session_messages, OutcomeUnknown +
//     MutationUnknown (never MutationNone) identical across cold SQLite Resume;
//  2. Nested callback inside eval_go -> both direct ToolCaller.Call result AND persisted parent
//     eval_go tool row carry bounded partial-publication details without host path leakage, with
//     identical SQLite Resume;
//  3. Oversized and non-JSON error preservation bounds in preserveErrorToolContent.
func TestR5ResultPropagationAndResumeIntegrity(t *testing.T) {
	const ref = config.ModelRef("test/m")

	setupSession := func(t *testing.T, sid string) (context.Context, string, string, *session.Store, r401SettleStore, *editing.Engine, *toolset.Set, *repository.Service) {
		t.Helper()
		ctx := context.Background()
		ws := filepath.Join(t.TempDir(), "ws")
		if err := os.MkdirAll(ws, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("old-a\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ws, "b.txt"), []byte("old-b\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		initGitRepoForTest(t, ws)

		dbPath := filepath.Join(t.TempDir(), sid+".db")
		raw, err := session.Open(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = raw.Close() })
		if err := raw.Create(ctx, session.Session{ID: sid, Goal: "R5 result propagation", Status: "running"}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := raw.BeginRun(ctx, sid, session.RunStart{
			RunID: "run-seed",
			Input: &protocol.Message{Role: "user", Content: "apply changes"},
		}); err != nil {
			t.Fatal(err)
		}
		st := r401SettleStore{realSQLiteAgentStore{s: raw}}
		if _, _, err := st.AppendMessage(ctx, sid, protocol.Message{Role: "assistant", Content: "done"}, nil); err != nil {
			t.Fatal(err)
		}
		if _, _, err := st.AppendMessage(ctx, sid, protocol.Message{
			Role:    "user",
			Origin:  protocol.OriginCompletionBlocked,
			Content: "Completion refused: verify outputs.",
		}, nil); err != nil {
			t.Fatal(err)
		}
		if _, _, err := raw.BeginRun(ctx, sid, session.RunStart{RunID: "run-live"}); err != nil {
			t.Fatal(err)
		}
		eng, err := editing.New(editing.Options{Root: ws})
		if err != nil {
			t.Fatal(err)
		}
		ts, err := toolset.New(toolset.Options{
			Workspace: ws,
			Policy:    toolset.PolicyFull,
			SessionID: sid,
			Editing:   eng,
		})
		if err != nil {
			t.Fatal(err)
		}
		repo, err := repository.New(ws)
		if err != nil {
			t.Fatal(err)
		}
		return ctx, ws, dbPath, raw, st, eng, ts, repo
	}

	t.Run("top_level_multifile_partial_publication_persists_and_resumes_identically", func(t *testing.T) {
		const sid = "s-r5-top"
		ctx, ws, dbPath, raw, st, eng, ts, repo := setupSession(t, sid)
		eng.SetPublishFaultHooks(editing.PublishFaultHooks{
			AfterInPlaceWriteClose: func(dst string) error {
				if filepath.Base(dst) == "b.txt" {
					f, oerr := os.OpenFile(dst, os.O_WRONLY|os.O_APPEND, 0)
					if oerr == nil {
						_, _ = f.WriteString("FOREIGN\n")
						_ = f.Close()
					}
					return syscall.EIO
				}
				return nil
			},
		})
		patchArgs := `{"patches":[{"path":"a.txt","operation":"update","edits":[{"old_text":"old-a","new_text":"new-a"}]},{"path":"b.txt","operation":"update","edits":[{"old_text":"old-b","new_text":"new-b"}]}]}`
		prov := &scriptedProvider{responses: []protocol.ProviderResponse{
			toolStep(call("tc-r5-top-1", "apply_patch", patchArgs)),
			answerOf("observed partial publication"),
		}}
		models := map[config.ModelRef]Model{ref: testModel(ref, prov)}
		res, err := (&Loop{
			Models:    models,
			Tools:     ts,
			Store:     st,
			Snapshots: repo,
		}).Run(ctx, sid, Options{Session: ref, RunID: "run-live", Resumed: true, MaxSteps: 4})
		if err != nil || res.Status != StatusCompleted {
			t.Fatalf("loop.Run: res=%+v err=%v", res, err)
		}

		// Settlement keeps the verified prefix and any foreign append on the failed path.
		gotA, _ := os.ReadFile(filepath.Join(ws, "a.txt"))
		gotB, _ := os.ReadFile(filepath.Join(ws, "b.txt"))
		if string(gotA) != "new-a\n" || string(gotB) != "new-b\nFOREIGN\n" {
			t.Fatalf("disk mismatch: a=%q b=%q", gotA, gotB)
		}

		// 2. Persisted tool row in SQLite carries bounded partial-publication JSON.
		var toolMsg protocol.Message
		found := false
		if err := st.Transcript(ctx, sid, 0, func(_ int64, m protocol.Message) error {
			if m.Role == "tool" && m.ToolCallID == "tc-r5-top-1" {
				toolMsg = m
				found = true
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if !found {
			t.Fatalf("persisted tool row for tc-r5-top-1 not found")
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(toolMsg.Content), &payload); err != nil {
			t.Fatalf("persisted tool row not valid JSON: %v (%s)", err, toolMsg.Content)
		}
		if payload["commitState"] != string(editing.CommitStatePartiallyPublished) ||
			payload["dirtyFailure"] != true ||
			payload["partialPublication"] != true {
			t.Fatalf("persisted tool row missing partial publication classification: %+v", payload)
		}
		if !strings.Contains(toolMsg.Content, string(editing.PublishSucceededAndVerified)) || !strings.Contains(toolMsg.Content, string(editing.PublishUncertain)) {
			t.Fatalf("persisted tool row missing per-file states: %s", toolMsg.Content)
		}
		if strings.Contains(toolMsg.Content, ws) || strings.Contains(toolMsg.Content, "FOREIGN") {
			t.Fatalf("persisted tool row leaked host path or raw file content: %s", toolMsg.Content)
		}

		// Verify model's second turn received the exact same persisted tool message content.
		if len(prov.seen) < 2 {
			t.Fatalf("expected 2 provider turns, got %d", len(prov.seen))
		}
		seenMatch := false
		for _, m := range prov.seen[1].Messages {
			if m.Role == "tool" && m.ToolCallID == "tc-r5-top-1" && m.Content == toolMsg.Content {
				seenMatch = true
			}
		}
		if !seenMatch {
			t.Fatalf("provider turn 2 did not receive persisted partial publication details")
		}

		// 3. Cold SQLite Resume reconstructs identical OutcomeUnknown + MutationUnknown (never MutationNone).
		sums, err := st.ListAnsweredExecutions(ctx, sid, 0)
		if err != nil || len(sums) != 1 {
			t.Fatalf("sums=%+v err=%v", sums, err)
		}
		if sums[0].Outcome != session.OutcomeUnknown || !strings.HasSuffix(sums[0].PostRef, ";cm=unknown") {
			t.Fatalf("expected OutcomeUnknown + ;cm=unknown, got %+v", sums[0])
		}
		_ = raw.Close()

		_, coldRep := assertLiveColdStreamParity(t, ctx, dbPath, sid)
		if !coldRep.Active || coldRep.PostBlockUnknownCount != 1 || coldRep.PostBlockProvenCount != 0 {
			t.Fatalf("coldRep = %+v, want PostBlockUnknownCount=1", coldRep)
		}
		if coldRep.CausalProgress[0].MutationEvidence != MutationUnknown || coldRep.CausalProgress[0].ToolCallID != "tc-r5-top-1" {
			t.Fatalf("CausalProgress[0] = %+v", coldRep.CausalProgress[0])
		}
		// Verify reopened cold SQLite session sees exact identical tool message content.
		reopened, err := session.Open(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = reopened.Close() }()
		coldStore := r401SettleStore{realSQLiteAgentStore{s: reopened}}
		coldMatched := false
		if err := coldStore.Transcript(ctx, sid, 0, func(_ int64, m protocol.Message) error {
			if m.Role == "tool" && m.ToolCallID == "tc-r5-top-1" && m.Content == toolMsg.Content {
				coldMatched = true
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if !coldMatched {
			t.Fatalf("cold SQLite Resume did not preserve identical partial publication tool message content")
		}
	})

	t.Run("cancelled_multi_file_receipt_is_paired_without_replay", func(t *testing.T) {
		const sid = "s-r7-cancel"
		ctx, ws, dbPath, raw, st, eng, ts, repo := setupSession(t, sid)
		stopped, cancel := context.WithCancel(ctx)
		defer cancel()
		eng.SetPublishFaultHooks(editing.PublishFaultHooks{AfterInPlaceWriteClose: func(dst string) error {
			if filepath.Base(dst) == "a.txt" {
				cancel()
			}
			return nil
		}})
		args := `{"patches":[{"path":"a.txt","operation":"update","edits":[{"old_text":"old-a","new_text":"new-a"}]},{"path":"b.txt","operation":"update","edits":[{"old_text":"old-b","new_text":"new-b"}]}]}`
		prov := &scriptedProvider{responses: []protocol.ProviderResponse{toolStep(call("tc-cancel", "apply_patch", args))}}
		res, runErr := (&Loop{Models: map[config.ModelRef]Model{ref: testModel(ref, prov)}, Tools: ts, Store: st, Snapshots: repo}).Run(stopped, sid, Options{Session: ref, RunID: "run-live", Resumed: true, MaxSteps: 2})
		if res.Status != StatusInterrupted {
			t.Fatalf("status=%v err=%v", res.Status, runErr)
		}
		for path, want := range map[string]string{"a.txt": "new-a\n", "b.txt": "old-b\n"} {
			got, err := os.ReadFile(filepath.Join(ws, path))
			if err != nil || string(got) != want {
				t.Fatalf("%s=%q err=%v", path, got, err)
			}
		}
		var rows int
		if err := st.Transcript(ctx, sid, 0, func(_ int64, m protocol.Message) error {
			if m.Role == "tool" && m.ToolCallID == "tc-cancel" {
				rows++
				if m.Origin != protocol.OriginToolInterrupted || !json.Valid([]byte(m.Content)) || !strings.Contains(m.Content, "partially_published") || !strings.Contains(m.Content, "not_attempted") {
					t.Fatalf("interrupted receipt: %+v", m)
				}
			}
			return nil
		}); err != nil || rows != 1 {
			t.Fatalf("paired rows=%d err=%v", rows, err)
		}
		if err := raw.Close(); err != nil {
			t.Fatal(err)
		}
		_, cold := assertLiveColdStreamParity(t, ctx, dbPath, sid)
		if cold.PostBlockUnknownCount != 1 || cold.PostBlockProvenCount != 0 {
			t.Fatalf("cold result: %+v", cold)
		}
	})

	t.Run("nested_eval_callback_partial_publication_preserved_in_callback_and_parent_row", func(t *testing.T) {
		const sid = "s-r5-nested"
		ctx, ws, dbPath, raw, st, eng, ts, repo := setupSession(t, sid)
		eng.SetPublishFaultHooks(editing.PublishFaultHooks{
			CloseAfterPublish: func(string) error { return syscall.EIO },
		})

		var nestedRes protocol.ToolResult
		var nestedErr error
		bridge := &r5EvalBridgeTools{
			inner: ts,
			evalSpecs: []protocol.ToolSpec{{
				Name:        "eval_go",
				Description: "test eval bridge",
				Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
			}},
			onEval: func(callCtx context.Context, cb protocol.ToolCaller) (protocol.ToolResult, error) {
				nestedRes, nestedErr = cb.Call(callCtx, "write_file", map[string]any{
					"path":    "nested_pub.txt",
					"content": "nested-bytes\n",
				})
				// Simulate evalruntime discarding nestedRes.Content and surfacing only nestedErr.
				return protocol.ToolResult{}, nestedErr
			},
		}

		prov := &scriptedProvider{responses: []protocol.ProviderResponse{
			toolStep(call("tc-r5-eval-1", "eval_go", `{}`)),
			answerOf("observed nested partial publication"),
		}}
		models := map[config.ModelRef]Model{ref: testModel(ref, prov)}
		res, err := (&Loop{
			Models:    models,
			Tools:     bridge,
			Store:     st,
			Snapshots: repo,
		}).Run(ctx, sid, Options{Session: ref, RunID: "run-live", Resumed: true, MaxSteps: 4})
		if err != nil || res.Status != StatusCompleted {
			t.Fatalf("loop.Run: res=%+v err=%v", res, err)
		}

		// 1. Direct callback result preserved full partial-publication JSON despite toolErr != nil.
		if nestedErr == nil || nestedRes.OK || !strings.Contains(nestedRes.Content, `"commitState":"published_with_post_error"`) || !strings.Contains(nestedRes.Content, `"nested_pub.txt"`) {
			t.Fatalf("callback result dropped partial publication details: res=%+v err=%v", nestedRes, nestedErr)
		}
		if strings.Contains(nestedRes.Content, ws) {
			t.Fatalf("callback result leaked host path: %s", nestedRes.Content)
		}

		// 2. Parent eval_go tool row persisted in SQLite STILL surfaces nested partial publication!
		var evalRow protocol.Message
		found := false
		if err := st.Transcript(ctx, sid, 0, func(_ int64, m protocol.Message) error {
			if m.Role == "tool" && m.ToolCallID == "tc-r5-eval-1" {
				evalRow = m
				found = true
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if !found {
			t.Fatal("persisted eval_go tool row not found")
		}
		var evalPayload map[string]any
		if err := json.Unmarshal([]byte(evalRow.Content), &evalPayload); err != nil {
			t.Fatalf("parent eval_go row invalid JSON: %v (%s)", err, evalRow.Content)
		}
		nestedNotes, ok := evalPayload["nestedPartialPublications"].([]any)
		if !ok || len(nestedNotes) != 1 {
			t.Fatalf("expected nestedPartialPublications in parent eval_go row, got %+v", evalPayload)
		}
		noteStr, _ := nestedNotes[0].(string)
		if !strings.Contains(noteStr, "write_file") || !strings.Contains(noteStr, "published_with_post_error") || !strings.Contains(noteStr, "nested_pub.txt:published_post_error") {
			t.Fatalf("unexpected nested note: %q", noteStr)
		}
		if strings.Contains(evalRow.Content, ws) {
			t.Fatalf("parent eval_go row leaked host path: %s", evalRow.Content)
		}

		// 3. Nested execution settled OutcomeUnknown in SQLite; cold Resume derives MutationUnknown.
		sums, err := st.ListAnsweredExecutions(ctx, sid, 0)
		if err != nil {
			t.Fatal(err)
		}
		nestedSettled := false
		for _, ex := range sums {
			if ex.ToolName == "write_file" && ex.ParentToolCallID == "tc-r5-eval-1" && ex.Outcome == session.OutcomeUnknown {
				nestedSettled = true
			}
		}
		if !nestedSettled {
			t.Fatalf("expected nested write_file OutcomeUnknown linked to tc-r5-eval-1, got %+v", sums)
		}
		_ = raw.Close()

		_, coldRep := assertLiveColdStreamParity(t, ctx, dbPath, sid)
		if !coldRep.Active || coldRep.PostBlockUnknownCount < 1 {
			t.Fatalf("coldRep = %+v, want PostBlockUnknownCount>=1", coldRep)
		}
	})

	t.Run("preserve_error_tool_content_bounds_oversized_and_plain_details", func(t *testing.T) {
		huge := `{"error":"boom","commitState":"partially_published","dirtyFailure":true,"partialPublication":true,"pad":"` + strings.Repeat("x", maxPreservedToolErrorBytes+500) + `"}`
		got := preserveErrorToolContent(huge, errors.New("boom"))
		if len(got) > maxPreservedToolErrorBytes {
			t.Fatalf("oversized error payload not bounded: len=%d", len(got))
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(got), &parsed); err != nil {
			t.Fatalf("invalid bounded JSON: %v", err)
		}
		if parsed["commitState"] != "partially_published" || parsed["truncated"] != true {
			t.Fatalf("expected compact bounded fields, got %+v", parsed)
		}

		plain := preserveErrorToolContent("partial output: "+strings.Repeat("y", 4000), errors.New("io fail"))
		var plainObj map[string]any
		if err := json.Unmarshal([]byte(plain), &plainObj); err != nil {
			t.Fatalf("invalid plain wrapper JSON: %v", err)
		}
		if plainObj["error"] != "io fail" || len(plainObj["details"].(string)) > 2060 {
			t.Fatalf("unexpected plain wrapper: %+v", plainObj)
		}
	})
}
