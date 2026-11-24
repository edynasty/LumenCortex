package agent

import (
	"context"
	"database/sql"
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

// zzApplyTools wraps a real toolset. adopt_experience / apply_experience imitate the runtime
// (runtime/experience_execution.go) result shapes: adopt returns sourceSessions=[origin]; apply runs a
// genuine nested write_file through the real callback dispatcher and returns
// {applicationId,state,results,sourceSessions}.
type zzApplyTools struct {
	inner   *toolset.Set
	origin  string
	padding int
}

func (t *zzApplyTools) Specs(allow []string) []protocol.ToolSpec {
	return append(t.inner.Specs(allow),
		protocol.ToolSpec{Name: "adopt_experience", Description: "probe", Permission: "read", Parameters: map[string]any{"type": "object", "properties": map[string]any{}}},
		protocol.ToolSpec{Name: "apply_experience", Description: "probe", Permission: "read", MutatesWorkspace: true, Parameters: map[string]any{"type": "object", "properties": map[string]any{}}})
}

func (t *zzApplyTools) Execute(ctx context.Context, name string, args map[string]any, emit func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	switch name {
	case "adopt_experience":
		raw, _ := json.Marshal(map[string]any{"adoptionId": "use-1", "sourceSessions": []string{t.origin}})
		return protocol.ToolResult{OK: true, Content: string(raw)}, nil
	case "apply_experience":
		cb, ok := protocol.ToolCallbackFromContext(ctx)
		if !ok || cb == nil {
			return protocol.ToolResult{}, errors.New("missing tool callback")
		}
		nested, _ := cb.Call(ctx, "write_file", map[string]any{"path": "applied.txt", "content": "applied\n"})
		results := []protocol.ToolResult{nested}
		if t.padding > 0 {
			results = append(results, protocol.ToolResult{OK: true, Content: strings.Repeat("r", t.padding)})
		}
		raw, _ := json.Marshal(map[string]any{"applicationId": "app-1", "state": "unknown", "results": results, "sourceSessions": []string{t.origin}, "error": "nested action failed"})
		return protocol.ToolResult{OK: false, MutatesWorkspace: nested.MutatesWorkspace, Content: string(raw)}, nil
	}
	return t.inner.Execute(ctx, name, args, emit)
}

func TestZZR402DF1ApplyExperienceRuntimeEdges(t *testing.T) {
	cases := []struct {
		name    string
		adopt   bool
		padding int
	}{
		{"apply_only_small", false, 0},
		{"apply_only_8500pad", false, 8500},
		{"adopt_then_apply_8500pad", true, 8500},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const ref = config.ModelRef("test/m")
			ctx := context.Background()
			sid, origin := "s-zz-f1", "s-zz-origin"
			ws := filepath.Join(t.TempDir(), "ws")
			if err := os.MkdirAll(ws, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("a\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			initGitRepoForTest(t, ws)
			dbPath := filepath.Join(t.TempDir(), "f1.db")
			raw, err := session.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := raw.Create(ctx, session.Session{ID: origin, Goal: "origin", Status: "completed"}); err != nil {
				t.Fatal(err)
			}
			if err := raw.Create(ctx, session.Session{ID: sid, Goal: "consumer", Status: "running"}); err != nil {
				t.Fatal(err)
			}
			if _, _, err := raw.BeginRun(ctx, sid, session.RunStart{RunID: "run-seed", Input: &protocol.Message{Role: "user", Content: "go"}}); err != nil {
				t.Fatal(err)
			}
			st := r401SettleStore{realSQLiteAgentStore{s: raw}}
			if _, _, err := st.AppendMessage(ctx, sid, protocol.Message{Role: "assistant", Content: "done"}, nil); err != nil {
				t.Fatal(err)
			}
			if _, _, err := st.AppendMessage(ctx, sid, protocol.Message{Role: "user", Origin: protocol.OriginCompletionBlocked, Content: "Completion refused: verify outputs."}, nil); err != nil {
				t.Fatal(err)
			}
			if _, _, err := raw.BeginRun(ctx, sid, session.RunStart{RunID: "run-live"}); err != nil {
				t.Fatal(err)
			}
			eng, err := editing.New(editing.Options{Root: ws})
			if err != nil {
				t.Fatal(err)
			}
			// Genuine dirty nested publish: write succeeds, Close after publish fails.
			eng.SetPublishFaultHooks(editing.PublishFaultHooks{CloseAfterPublish: func(string) error { return syscall.EIO }})
			ts, err := toolset.New(toolset.Options{Workspace: ws, Policy: toolset.PolicyFull, SessionID: sid, Editing: eng})
			if err != nil {
				t.Fatal(err)
			}
			repo, err := repository.New(ws)
			if err != nil {
				t.Fatal(err)
			}
			var steps []protocol.ProviderResponse
			if tc.adopt {
				steps = append(steps, toolStep(call("tc-adopt", "adopt_experience", `{}`)))
			}
			steps = append(steps, toolStep(call("tc-apply", "apply_experience", `{}`)), answerOf("done"))
			prov := &scriptedProvider{responses: steps}
			res, err := (&Loop{Models: map[config.ModelRef]Model{ref: testModel(ref, prov)}, Tools: &zzApplyTools{inner: ts, origin: origin, padding: tc.padding}, Store: st, Snapshots: repo}).
				Run(ctx, sid, Options{Session: ref, RunID: "run-live", Resumed: true, MaxSteps: 6})
			t.Logf("PROBE run status=%v err=%v", res.Status, err)
			disk, _ := os.ReadFile(filepath.Join(ws, "applied.txt"))
			t.Logf("PROBE disk applied.txt=%q", disk)
			_ = raw.Close()

			// Cold: raw SQL over the persisted file.
			db, err := sql.Open("sqlite3", filepath.Join(dbPath, "lumencortex.db"))
			if err != nil {
				t.Fatal(err)
			}
			rows, err := db.Query(`SELECT kind, target_ref, source_kind, source_ref, lineage_complete, COALESCE(tool_name,''), COALESCE(outcome,'') FROM derived_origins WHERE session_id=? ORDER BY rowid`, sid)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var k, tr, sk, sr, tn, oc string
				var lc int
				_ = rows.Scan(&k, &tr, &sk, &sr, &lc, &tn, &oc)
				t.Logf("PROBE edge kind=%s target=%s source_kind=%s source_ref=%s lineage_complete=%d tool=%s outcome=%s", k, tr, sk, sr, lc, tn, oc)
			}
			rows.Close()
			var applySeq int64
			var applyContent string
			_ = db.QueryRow(`SELECT seq, json_extract(json,'$.content') FROM session_messages WHERE session_id=? AND role='tool' AND json_extract(json,'$.tool_call_id')='tc-apply'`, sid).Scan(&applySeq, &applyContent)
			_ = db.Close()
			tail := applyContent
			if len(tail) > 220 {
				tail = tail[len(tail)-220:]
			}
			t.Logf("PROBE cold apply row seq=%d bytes=%d valid_json=%v has_text_block=%v tail=%q", applySeq, len(applyContent), json.Valid([]byte(applyContent)), strings.Contains(applyContent, "[nested_partial_publication]"), tail)

			cold, err := session.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer cold.Close()
			rev, err := cold.RevokeSession(ctx, origin, "probe_revoke")
			t.Logf("PROBE revoke origin err=%v sessionCount=%d sessions=%v consumer_reached=%v", err, rev.SessionCount, rev.Sessions, func() bool {
				for _, s := range rev.Sessions {
					if s == sid {
						return true
					}
				}
				return false
			}())
			proj, err := cold.Projection(ctx, sid)
			t.Logf("PROBE consumer projection err=%v epoch=%d isolated=%q withheld=%v apply_seq_withheld=%v", err, proj.Epoch, proj.Isolated, proj.Withheld, func() bool {
				for _, s := range proj.Withheld {
					if s == applySeq {
						return true
					}
				}
				return false
			}())
		})
	}
}
