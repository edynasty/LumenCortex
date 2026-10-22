package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/protocol"
)

// The fixture is the real-format OMP session of internal/ompsession (see its test for what it holds).
const (
	ompFixtureRoot = "../internal/ompsession/testdata/sessions"
	ompFixtureRel  = "-fixture-workspace/2026-10-01T08-00-00-000Z_01a10000-0000-7000-8000-000000000001.jsonl"
	ompFixtureID   = "01a10000-0000-7000-8000-000000000001"
)

var ompSentinels = []string{
	"SECRET_THINKING_SENTINEL", "SECRET_SIGNATURE_SENTINEL", "SECRET_THINKING_SUMMARY", "SECRET_TEXTSIG_SENTINEL",
	"SECRET_PAYLOAD_SENTINEL", "SECRET_COMPACTION_SUMMARY_SENTINEL", "SECRET_SHORT_SUMMARY",
	"SECRET_BRANCH_SUMMARY_SENTINEL", "HIDDEN_REMINDER_SENTINEL", "HIDDEN_NUDGE_SENTINEL",
	"SYSTEM_PROMPT_SENTINEL", "ABANDONED_BRANCH_TEXT", "ADVISOR_TEXT",
}

// copyOMPFixture copies the fixture tree to a temporary sessions root.
func copyOMPFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	err := filepath.WalkDir(ompFixtureRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(ompFixtureRoot, path)
		target := filepath.Join(root, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, raw, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// treeDigest hashes every file name, mode and content under root.
func treeDigest(t *testing.T, root string) string {
	t.Helper()
	h := sha256.New()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		h.Write([]byte(path + info.Mode().String() + info.ModTime().String()))
		if !d.IsDir() {
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			h.Write(raw)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// transcriptRows reads a session's whole stored transcript.
func transcriptRows(t *testing.T, engine *Engine, sessionID string) []protocol.Message {
	t.Helper()
	var out []protocol.Message
	err := engine.store.EachMessageFrom(context.Background(), sessionID, 0, func(m session.Message) error {
		var msg protocol.Message
		if err := json.Unmarshal(m.JSON, &msg); err != nil {
			return err
		}
		out = append(out, msg)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func openImportEngine(t *testing.T) *Engine {
	t.Helper()
	useGlobalConfig(t, modelConfig(""))
	engine, err := Open(Options{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	return engine
}

func TestOMPImportIsIdempotentAndLeavesSourceUntouched(t *testing.T) {
	engine := openImportEngine(t)
	ctx := context.Background()
	root := copyOMPFixture(t)
	file := filepath.Join(root, ompFixtureRel)
	before := treeDigest(t, root)

	preview, err := engine.PreviewOMPSession(ctx, root, file)
	if err != nil {
		t.Fatal(err)
	}
	if listed, _ := engine.ListSessions(ctx, 10, 0); len(listed) != 0 {
		t.Fatalf("preview wrote %d sessions", len(listed))
	}
	wantID := ImportedOMPSessionID(ompFixtureID)
	if preview.SessionID != wantID || len(preview.Children) != 3 || preview.OffBranch != 1 || preview.Source.Imported != nil {
		t.Fatalf("preview %+v", preview)
	}
	codes := map[string]bool{}
	for _, w := range preview.Warnings {
		codes[w.Code] = true
	}
	if !codes[OMPWarnWorkspaceMismatch] || !codes[OMPWarnOpenToolCalls] || codes[OMPWarnMaybeRunning] {
		t.Fatalf("warnings %+v", preview.Warnings)
	}

	first, err := engine.ImportOMPSession(ctx, root, file)
	if err != nil {
		t.Fatal(err)
	}
	if first.AlreadyImported || first.Sessions != 4 || first.SessionID != wantID {
		t.Fatalf("first import %+v", first)
	}
	second, err := engine.ImportOMPSession(ctx, root, file)
	if err != nil {
		t.Fatal(err)
	}
	if !second.AlreadyImported || second.SourceChanged || second.Sessions != 0 {
		t.Fatalf("second import %+v", second)
	}
	if listed, _ := engine.ListSessions(ctx, 10, 0); len(listed) != 4 {
		t.Fatalf("sessions after two imports: %d", len(listed))
	}
	if after := treeDigest(t, root); after != before {
		t.Fatal("import changed the source tree")
	}

	// The imported tree is the subagent tree of the parent, nested as in OMP.
	sup, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := sup.SubagentTree(ctx, wantID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree) != 2 {
		t.Fatalf("tree %+v", tree)
	}
	_, info, err := engine.Session(ctx, wantID)
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != "completed" || info.Goal != "Fix the parser bug" {
		t.Fatalf("info %+v", info)
	}
	var meta struct {
		Import ompProvenance  `json:"import"`
		UI     map[string]any `json:"ui"`
	}
	decodeJSON(t, info.Metadata, &meta)
	if meta.Import.Source != ImportSourceOMP || meta.Import.SessionID != ompFixtureID || meta.Import.SHA256 != preview.SHA256 || meta.UI["title"] != "Fix the parser bug" {
		t.Fatalf("provenance %+v ui %+v", meta.Import, meta.UI)
	}

	// The list reports the import; a later change of the source is reported, never applied.
	listed, err := engine.OMPSessions(ctx, OMPListOptions{Root: root, All: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Imported == nil || listed[0].Imported.SourceChanged {
		t.Fatalf("listed %+v", listed)
	}
	if none, _ := engine.OMPSessions(ctx, OMPListOptions{Root: root}); len(none) != 0 {
		t.Fatalf("workspace filter kept %+v", none)
	}
	f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"type":"message","id":"e9","parentId":"e0000020","timestamp":"2026-10-01T09:00:00Z","message":{"role":"user","content":"more"}}` + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	third, err := engine.ImportOMPSession(ctx, root, file)
	if err != nil {
		t.Fatal(err)
	}
	if !third.AlreadyImported || !third.SourceChanged {
		t.Fatalf("changed source %+v", third)
	}
	rows := transcriptRows(t, engine, wantID)
	for _, m := range rows {
		if m.Content == "more" {
			t.Fatal("a re-import changed the imported transcript")
		}
	}
}

// A subagent transcript and a file outside the sessions root are refused before anything is read.
func TestOMPImportRefusesPathsOutsideTopLevel(t *testing.T) {
	engine := openImportEngine(t)
	root := copyOMPFixture(t)
	child := filepath.Join(root, strings.TrimSuffix(ompFixtureRel, ".jsonl"), "Scout1.jsonl")
	if _, err := engine.ImportOMPSession(context.Background(), root, child); err == nil {
		t.Fatal("a subagent transcript was imported on its own")
	}
	if _, err := engine.ImportOMPSession(context.Background(), root, filepath.Join(ompFixtureRoot, ompFixtureRel)); err == nil {
		t.Fatal("a file outside the root was imported")
	}
}

// An imported session continues like a native one: the next request carries the imported history,
// the import notice and the new input; no historical call is executed and no hidden text is stored
// or sent.
func TestImportedOMPSessionContinues(t *testing.T) {
	engine := openImportEngine(t)
	ctx := context.Background()
	root := copyOMPFixture(t)
	result, err := engine.ImportOMPSession(ctx, root, filepath.Join(root, ompFixtureRel))
	if err != nil {
		t.Fatal(err)
	}
	imported := transcriptRows(t, engine, result.SessionID)
	p := &answering{final: "continuing"}
	bindRef(engine, "a", p)
	if r := run(t, engine, result.SessionID, AgentOptions{Input: "What is left?"}); r.Status != "completed" || r.Final != "continuing" {
		t.Fatalf("continuation %+v", r)
	}
	if p.calls.Load() != 1 {
		t.Fatalf("provider calls %d", p.calls.Load())
	}
	req := p.last.Load().(protocol.ProviderRequest)
	var sent []protocol.Message
	for _, m := range req.Messages {
		if m.Role != "system" {
			sent = append(sent, m)
		}
	}
	if len(sent) < 3 || sent[len(sent)-1].Content != "What is left?" || sent[len(sent)-2].Content != protocol.ImportedContent {
		t.Fatalf("request tail %+v", sent[max(0, len(sent)-3):])
	}
	// After the Task State message the request replays the imported rows in order.
	history := sent[1 : len(sent)-1]
	if len(history) != len(imported) || history[0].Content != imported[0].Content || history[1].ToolCalls[0].ID != imported[1].ToolCalls[0].ID {
		t.Fatalf("request history %d rows, imported %d", len(history), len(imported))
	}
	raw, _ := json.Marshal(req.Messages)
	for _, s := range ompSentinels {
		if strings.Contains(string(raw), s) {
			t.Errorf("request holds %s", s)
		}
	}
	after := transcriptRows(t, engine, result.SessionID)
	if len(after) != len(imported)+2 || after[len(after)-2].Role != "user" || after[len(after)-1].Role != "assistant" {
		t.Fatalf("rows %d -> %d", len(imported), len(after))
	}
	for _, m := range after[len(imported):] {
		if m.Role == "tool" {
			t.Fatal("a historical tool call was executed")
		}
	}
	path := engine.store.Path()
	engine.Close()
	assertNoTextAtRest(t, path, ompSentinels)
}

// Host metadata cannot forge or remove import provenance; deleting the imported parent deletes its
// imported subagent sessions with it.
func TestImportedOMPSessionMetadataAndDelete(t *testing.T) {
	engine := openImportEngine(t)
	ctx := context.Background()
	root := copyOMPFixture(t)
	result, err := engine.ImportOMPSession(ctx, root, filepath.Join(root, ompFixtureRel))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.NewSession(ctx, SessionOptions{Goal: "x", Metadata: map[string]any{"import": map[string]any{"source": "omp"}}}); err == nil {
		t.Fatal("a host session set the import key")
	}
	if err := engine.store.SetHostMetadataKey(ctx, result.SessionID, session.MetadataKeyImport, json.RawMessage(`{}`)); err == nil {
		t.Fatal("a host overwrote the import key")
	}
	deleted, err := engine.DeleteSession(ctx, result.SessionID, WorktreeCleanup{})
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted.DeletedChildren) != 3 {
		t.Fatalf("deleted children %v", deleted.DeletedChildren)
	}
	if listed, _ := engine.ListSessions(ctx, 10, 0); len(listed) != 0 {
		t.Fatalf("sessions left %d", len(listed))
	}
	// After a delete the source can be imported again.
	again, err := engine.ImportOMPSession(ctx, root, filepath.Join(root, ompFixtureRel))
	if err != nil || again.AlreadyImported || again.Sessions != 4 {
		t.Fatalf("re-import after delete %+v %v", again, err)
	}
}
