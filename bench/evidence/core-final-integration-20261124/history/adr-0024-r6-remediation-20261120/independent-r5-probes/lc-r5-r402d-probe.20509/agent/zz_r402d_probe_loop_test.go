package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/editing"
	"github.com/edynasty/LumenCortex/internal/repository"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/toolset"
	"github.com/edynasty/LumenCortex/protocol"
)

// Probe: real toolset edit_lines on a .go file that fails GoSyntax validation, through the real
// Loop + SQLite store, then cold reopen. Reports whether the persisted session_messages row
// contains the absolute workspace path and its byte size.
func TestZZR402DProbeGoDiagnosticsAbsPathPersisted(t *testing.T) {
	for _, nBad := range []int{1, 120} {
		t.Run(fmt.Sprintf("bad_lines_%d", nBad), func(t *testing.T) {
			const ref = config.ModelRef("test/m")
			ctx := context.Background()
			sid := fmt.Sprintf("s-zz-r402d-%d", nBad)
			ws := filepath.Join(t.TempDir(), "ws")
			if err := os.MkdirAll(ws, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(ws, "p.go"), []byte("package p\n\nfunc f() {\n}\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			initGitRepoForTest(t, ws)
			realWS, _ := filepath.EvalSymlinks(ws)
			dbPath := filepath.Join(t.TempDir(), sid+".db")
			raw, err := session.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := raw.Create(ctx, session.Session{ID: sid, Goal: "probe", Status: "running"}); err != nil {
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
			ts, err := toolset.New(toolset.Options{Workspace: ws, Policy: toolset.PolicyFull, SessionID: sid, Editing: eng})
			if err != nil {
				t.Fatal(err)
			}
			repo, err := repository.New(ws)
			if err != nil {
				t.Fatal(err)
			}
			rd, rdErr := ts.Execute(ctx, "read_file", map[string]any{"path": "p.go", "anchors": true}, nil)
			if rdErr != nil {
				t.Fatal(rdErr)
			}
			rev := regexp.MustCompile(`rev=(\S+)`).FindStringSubmatch(rd.Content)[1]
			anchor := regexp.MustCompile(`(?m)^(3:[^|]+)\|`).FindStringSubmatch(rd.Content)[1]
			bad := strings.Repeat("func f( {\n", nBad)
			args, _ := json.Marshal(map[string]any{"path": "p.go", "rev": rev, "ops": []map[string]any{{"op": "replace", "start": anchor, "text": bad}}})
			prov := &scriptedProvider{responses: []protocol.ProviderResponse{
				toolStep(call("tc-zz-1", "edit_lines", string(args))),
				answerOf("ok"),
			}}
			res, err := (&Loop{Models: map[config.ModelRef]Model{ref: testModel(ref, prov)}, Tools: ts, Store: st, Snapshots: repo}).
				Run(ctx, sid, Options{Session: ref, RunID: "run-live", Resumed: true, MaxSteps: 4})
			if err != nil || res.Status != StatusCompleted {
				t.Fatalf("run: %+v %v", res, err)
			}
			_ = raw.Close()
			cold, err := session.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer cold.Close()
			cs := r401SettleStore{realSQLiteAgentStore{s: cold}}
			var content string
			_ = cs.Transcript(ctx, sid, 0, func(_ int64, m protocol.Message) error {
				if m.Role == "tool" && m.ToolCallID == "tc-zz-1" {
					content = m.Content
				}
				return nil
			})
			var obj map[string]any
			jsonErr := json.Unmarshal([]byte(content), &obj)
			diags, _ := obj["diagnostics"].([]any)
			t.Logf("PROBE ws=%s realWS=%s", ws, realWS)
			t.Logf("PROBE cold_row_bytes=%d valid_json=%v code=%v path=%v diagnostics=%d", len(content), jsonErr == nil, obj["code"], obj["path"], len(diags))
			t.Logf("PROBE contains_ws=%v contains_realWS=%v", strings.Contains(content, ws), strings.Contains(content, realWS))
			head := content
			if len(head) > 700 {
				head = head[:700]
			}
			t.Logf("PROBE row_head=%s", head)
		})
	}
}
