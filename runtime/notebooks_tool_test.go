package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/toolset"
	"github.com/edynasty/LumenCortex/protocol"
)

// notebookEngine opens an Engine with one session, bound to nothing that needs a model.
func notebookEngine(t *testing.T) (*Engine, string) {
	t.Helper()
	engine, err := Open(Options{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	handle, err := engine.NewSession(context.Background(), SessionOptions{Goal: "notebook"})
	if err != nil {
		t.Fatal(err)
	}
	return engine, handle.ID
}

func TestNotebookAPIWritesReadsAndExports(t *testing.T) {
	engine, sessionID := notebookEngine(t)
	ctx := context.Background()
	body := "# Plan\n\n## Checklist\n\n- [ ] first step\n\n## Handoff\n\nread internal/session/notebooks.go\n"
	doc, err := engine.CreateNotebook(ctx, sessionID, "plan", body)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Version != 1 || doc.UpdatedBy != "host" || doc.Bytes != len(body) {
		t.Fatalf("created = %#v", doc)
	}

	infos, err := engine.Notebooks(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].Title != "plan" || strings.Join(infos[0].Sections, ",") != "Checklist,Handoff" {
		t.Fatalf("list = %#v", infos)
	}

	got, err := engine.Notebook(ctx, sessionID, doc.ID)
	if err != nil || got.Body != body {
		t.Fatalf("read = %#v, %v", got, err)
	}

	saved, err := engine.SaveNotebook(ctx, sessionID, doc.ID, body+"\n## Verification\n\nnot run\n", 1)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Version != 2 || saved.UpdatedBy != "host" {
		t.Fatalf("saved = %#v", saved)
	}
	_, err = engine.SaveNotebook(ctx, sessionID, doc.ID, "stale", 1)
	var conflict *NotebookConflict
	if !errors.As(err, &conflict) || !errors.Is(err, ErrNotebookConflict) || conflict.CurrentVersion != 2 {
		t.Fatalf("stale save = %v", err)
	}

	revisions, err := engine.NotebookRevisions(ctx, sessionID, doc.ID)
	if err != nil || len(revisions) != 2 {
		t.Fatalf("revisions = %#v, %v", revisions, err)
	}
	first, err := engine.NotebookRevision(ctx, sessionID, doc.ID, 1)
	if err != nil || first.Body != body {
		t.Fatalf("revision 1 = %#v, %v", first, err)
	}

	markdown, filename, err := engine.ExportNotebook(ctx, sessionID, doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if markdown != saved.Body || filename != "plan.md" {
		t.Fatalf("export = %q, %q", markdown, filename)
	}

	if err := engine.DeleteNotebook(ctx, sessionID, doc.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Notebook(ctx, sessionID, doc.ID); !errors.Is(err, ErrNotebookNotFound) {
		t.Fatalf("deleted notebook = %v", err)
	}
	if infos, err := engine.Notebooks(ctx, sessionID); err != nil || len(infos) != 0 {
		t.Fatalf("list after delete = %#v, %v", infos, err)
	}
}

// TestReadOnlyRunUpdatesItsNotebookButNotFiles: a read-only run gets the notebook tools (permission
// "state") and can use them, while its file tools stay read-only.
func TestReadOnlyRunUpdatesItsNotebookButNotFiles(t *testing.T) {
	engine, sessionID := notebookEngine(t)
	ctx := context.Background()
	access := newNotebookAccess(engine, sessionID, "run1")

	readOnly, err := toolset.New(toolset.Options{
		Workspace: engine.workspace,
		Policy:    toolset.PolicyReadOnly,
		Notebook:  access,
	})
	if err != nil {
		t.Fatal(err)
	}
	specs := readOnly.Specs(nil)
	for _, name := range []string{"notebook_list", "notebook_read", "notebook_update"} {
		spec, ok := findToolSpec(specs, name)
		if !ok {
			t.Fatalf("read-only run has no %s", name)
		}
		if spec.Permission != toolset.PermissionState {
			t.Fatalf("%s permission = %q", name, spec.Permission)
		}
	}

	before, err := engine.TaskState(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := readOnly.Execute(ctx, "notebook_update", map[string]any{
		"notebook": "plan", "op": "create", "content": "# Plan\n\n## Handoff\nstart\n",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A notebook write is not a workspace change: it opens no completion obligation and does not
	// touch the Task State, which is the boundary the notebook must not cross (ADR-0020 §2).
	if result.MutatesWorkspace {
		t.Fatal("a notebook write reported a workspace change")
	}
	var created struct {
		Version int64 `json:"version"`
		Bytes   int   `json:"bytes"`
	}
	if !result.OK {
		t.Fatalf("create = %#v", result)
	}
	decodeJSON(t, []byte(result.Content), &created)
	if created.Version != 1 || created.Bytes == 0 {
		t.Fatalf("create = %s", result.Content)
	}

	// The section write extends the version and returns the new one.
	result, err = readOnly.Execute(ctx, "notebook_update", map[string]any{
		"notebook": "plan", "op": "section", "section": "Findings", "content": "one",
	}, nil)
	if err != nil || !result.OK {
		t.Fatalf("section write = %#v, %v", result, err)
	}
	var updated struct {
		Version int64 `json:"version"`
	}
	decodeJSON(t, []byte(result.Content), &updated)
	if updated.Version != 2 {
		t.Fatalf("section write = %s", result.Content)
	}

	result, err = readOnly.Execute(ctx, "notebook_read", map[string]any{"notebook": "plan", "section": "Findings"}, nil)
	if err != nil || !result.OK {
		t.Fatalf("read = %#v, %v", result, err)
	}
	var read struct {
		Body    string `json:"body"`
		Section string `json:"section"`
	}
	decodeJSON(t, []byte(result.Content), &read)
	if read.Section != "Findings" || strings.TrimSpace(read.Body) != "one" {
		t.Fatalf("section read = %s", result.Content)
	}

	// A stale replace is a result with the current version, not a Go error.
	result, err = readOnly.Execute(ctx, "notebook_update", map[string]any{
		"notebook": "plan", "op": "replace", "content": "x", "expected_version": 1,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var conflict struct {
		Error          string `json:"error"`
		CurrentVersion int64  `json:"currentVersion"`
	}
	decodeJSON(t, []byte(result.Content), &conflict)
	if result.OK || conflict.CurrentVersion != 2 || conflict.Error == "" {
		t.Fatalf("conflict = %#v %s", result, result.Content)
	}

	// replace without a version is refused before anything is written.
	if _, err := readOnly.Execute(ctx, "notebook_update", map[string]any{
		"notebook": "plan", "op": "replace", "content": "x",
	}, nil); err == nil {
		t.Fatal("replace without expected_version was accepted")
	}

	after, err := engine.TaskState(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Version != before.Version {
		t.Fatalf("a notebook write advanced the Task State %d -> %d", before.Version, after.Version)
	}

	// The file tools of the same run are still read-only.
	if result, err := readOnly.Execute(ctx, "write_file", map[string]any{"path": "a.txt", "content": "x"}, nil); err != nil || !result.Denied {
		t.Fatalf("write_file under read-only = %#v, %v", result, err)
	}
}

// TestNotebookToolsOnlyWriteTheRunOwnSession: a run may read its direct children's notebooks but not
// a sibling's or a stranger's, and writes only its own session.
func TestNotebookToolsOnlyWriteTheRunOwnSession(t *testing.T) {
	engine, parent := notebookEngine(t)
	ctx := context.Background()
	child, err := engine.NewSession(ctx, SessionOptions{Goal: "child"})
	if err != nil {
		t.Fatal(err)
	}
	sibling, err := engine.NewSession(ctx, SessionOptions{Goal: "sibling"})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.store.AddRelation(ctx, session.Relation{ParentSessionID: parent, ChildSessionID: child.ID, Kind: "subagent"}); err != nil {
		t.Fatal(err)
	}
	childDoc, err := engine.CreateNotebook(ctx, child.ID, "research", "# Research\n\n## Findings\nchild found it\n")
	if err != nil {
		t.Fatal(err)
	}
	parentDoc, err := engine.CreateNotebook(ctx, parent, "plan", "# Plan\n\n## Handoff\nparent only\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.CreateNotebook(ctx, sibling.ID, "plan", "# Sibling\n"); err != nil {
		t.Fatal(err)
	}

	set, err := toolset.New(toolset.Options{
		Workspace: engine.workspace, Policy: toolset.PolicyReadOnly,
		Notebook: newNotebookAccess(engine, parent, "run1"),
	})
	if err != nil {
		t.Fatal(err)
	}

	// The parent reads the child's notebook through session_id.
	result, err := set.Execute(ctx, "notebook_read", map[string]any{"notebook": "research", "session_id": child.ID}, nil)
	if err != nil || !result.OK {
		t.Fatalf("parent read of the child = %#v, %v", result, err)
	}
	if !strings.Contains(result.Content, "child found it") {
		t.Fatalf("child read = %s", result.Content)
	}
	// And it lists the child's notebooks.
	result, err = set.Execute(ctx, "notebook_list", map[string]any{"session_id": child.ID}, nil)
	if err != nil || !result.OK || !strings.Contains(result.Content, `"title":"research"`) {
		t.Fatalf("parent list of the child = %#v, %v", result, err)
	}
	// A sibling is not reachable.
	result, err = set.Execute(ctx, "notebook_read", map[string]any{"notebook": "plan", "session_id": sibling.ID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.OK || !strings.Contains(result.Content, "direct child") {
		t.Fatalf("sibling read = %#v", result)
	}

	// The child run reads and writes only its own session.
	childSet, err := toolset.New(toolset.Options{
		Workspace: engine.workspace, Policy: toolset.PolicyReadOnly,
		Notebook: newNotebookAccess(engine, child.ID, "run2"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := childSet.Execute(ctx, "notebook_read", map[string]any{"notebook": "research"}, nil); err != nil || !result.OK {
		t.Fatalf("child reading its own notebooks = %#v, %v", result, err)
	}
	result, err = childSet.Execute(ctx, "notebook_read", map[string]any{"notebook": "plan", "session_id": parent}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.OK || !strings.Contains(result.Content, "direct child") {
		t.Fatalf("child read of the parent = %#v", result)
	}
	// A write names no session at all: the child writes its own notebook only.
	if _, err := childSet.Execute(ctx, "notebook_update", map[string]any{
		"notebook": "research", "op": "append", "content": "more\n",
	}, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := engine.Notebook(ctx, child.ID, childDoc.ID); err != nil || got.Version != 2 {
		t.Fatalf("child notebook = %#v, %v", got, err)
	}
	if got, err := engine.Notebook(ctx, parent, parentDoc.ID); err != nil || got.Version != 1 {
		t.Fatalf("parent notebook changed: %#v, %v", got, err)
	}
}

// notebookSentinel is the content of a child's notebook no read exit may return once the child is
// withdrawn as a source.
const notebookSentinel = "NOTEBOOK-SENTINEL-4b1f"

// childWithNotebook is a subagent child of parent with one notebook, "research", at version 2, whose
// body (both versions) holds notebookSentinel.
func childWithNotebook(t *testing.T, engine *Engine, parent string) (string, NotebookDoc) {
	t.Helper()
	ctx := context.Background()
	child, err := engine.NewSession(ctx, SessionOptions{Goal: "child"})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.store.AddRelation(ctx, session.Relation{ParentSessionID: parent, ChildSessionID: child.ID, Kind: RelationSubagent}); err != nil {
		t.Fatal(err)
	}
	doc, err := engine.CreateNotebook(ctx, child.ID, "research", "# Research\n\n## Findings\n"+notebookSentinel+"\n")
	if err != nil {
		t.Fatal(err)
	}
	if doc, err = engine.SaveNotebook(ctx, child.ID, doc.ID, doc.Body+"\n## Handoff\n"+notebookSentinel+" again\n", doc.Version); err != nil {
		t.Fatal(err)
	}
	return child.ID, doc
}

// Plan T3-S3 for the notebook (RT-state-3): once a child is revoked, every notebook read of it returns
// the child withdrawn, as read_thread does - the agent's notebook_list and notebook_read results mark
// it withdrawn and carry no title, heading or body; the host's Notebooks, Notebook, NotebookRevisions
// and NotebookRevision return the same entries marked withdrawn with no content; ExportNotebook is
// refused. The parent's own notebooks stay readable.
func TestNotebookReadsOfARevokedChildAreWithdrawn(t *testing.T) {
	engine, parent := notebookEngine(t)
	ctx := context.Background()
	child, childDoc := childWithNotebook(t, engine, parent)
	if _, err := engine.CreateNotebook(ctx, parent, "plan", "# Plan\n\n## Handoff\nparent only\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.RevokeSource(ctx, SourceSession, child, "user_revoked"); err != nil {
		t.Fatal(err)
	}

	set, err := toolset.New(toolset.Options{
		Workspace: engine.workspace, Policy: toolset.PolicyReadOnly,
		Notebook: newNotebookAccess(engine, parent, "run1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range []struct {
		name string
		args map[string]any
	}{
		{"notebook_read", map[string]any{"notebook": "research", "session_id": child}},
		{"notebook_read", map[string]any{"notebook": childDoc.ID, "section": "Findings", "session_id": child}},
		{"notebook_list", map[string]any{"session_id": child}},
	} {
		result, err := set.Execute(ctx, call.name, call.args, nil)
		if err != nil || !result.OK {
			t.Fatalf("%s %v = %#v, %v", call.name, call.args, result, err)
		}
		var out struct {
			Session   string `json:"session"`
			Withdrawn bool   `json:"withdrawn"`
		}
		decodeJSON(t, []byte(result.Content), &out)
		if out.Session != child || !out.Withdrawn || strings.Contains(result.Content, notebookSentinel) ||
			strings.Contains(result.Content, "Findings") || strings.Contains(result.Content, "Handoff") {
			t.Fatalf("%s %v of the revoked child = %s", call.name, call.args, result.Content)
		}
	}
	if result, err := set.Execute(ctx, "notebook_read", map[string]any{"notebook": "plan"}, nil); err != nil || !result.OK ||
		!strings.Contains(result.Content, "parent only") || strings.Contains(result.Content, `"withdrawn"`) {
		t.Fatalf("the parent's own notebook = %#v, %v", result, err)
	}

	withdrawn := func(what string, v any) {
		t.Helper()
		text := jsonOf(t, v)
		if strings.Contains(text, notebookSentinel) || strings.Contains(text, "research") || strings.Contains(text, "Findings") {
			t.Fatalf("host %s of the revoked child carries its content: %s", what, text)
		}
		if !strings.Contains(text, `"withdrawn":true`) {
			t.Fatalf("host %s of the revoked child is not marked withdrawn: %s", what, text)
		}
	}
	infos, err := engine.Notebooks(ctx, child)
	if err != nil || len(infos) != 1 || infos[0].ID != childDoc.ID || infos[0].Version != 2 {
		t.Fatalf("host list %+v %v", infos, err)
	}
	withdrawn("list", infos)
	doc, err := engine.Notebook(ctx, child, "research")
	if err != nil || doc.ID != childDoc.ID || doc.Version != 2 {
		t.Fatalf("host read %+v %v", doc, err)
	}
	withdrawn("read", doc)
	revisions, err := engine.NotebookRevisions(ctx, child, childDoc.ID)
	if err != nil || len(revisions) != 2 {
		t.Fatalf("host revisions %+v %v", revisions, err)
	}
	withdrawn("revisions", revisions)
	revision, err := engine.NotebookRevision(ctx, child, childDoc.ID, 1)
	if err != nil || revision.Version != 1 {
		t.Fatalf("host revision %+v %v", revision, err)
	}
	withdrawn("revision", revision)
	if markdown, filename, err := engine.ExportNotebook(ctx, child, childDoc.ID); !errors.Is(err, ErrNotebookWithdrawn) || markdown != "" || filename != "" {
		t.Fatalf("host export of the revoked child = %q, %q, %v", markdown, filename, err)
	}
	if got, err := engine.Notebook(ctx, parent, "plan"); err != nil || !strings.Contains(got.Body, "parent only") {
		t.Fatalf("the parent's own notebook on the host = %+v, %v", got, err)
	}
}

// A host write of a session withdrawn as a source is refused: its reads show the version but not the
// body, so a compare-and-set on that version would replace content the host never saw, and its
// revisions are hidden too. The refusal holds for a version read after the revocation and for one read
// before it (the revocation committed between the host's read and its write), and for a reader the
// revocation fenced, not only the revoked child. Nothing is written; the agent's own notebook writes
// are untouched.
func TestSaveNotebookOfAWithdrawnSessionIsRefused(t *testing.T) {
	engine, parent := notebookEngine(t)
	ctx := context.Background()
	child, childDoc := childWithNotebook(t, engine, parent)
	plan, err := engine.CreateNotebook(ctx, parent, "plan", "# Plan\n\n## Handoff\nparent only\n")
	if err != nil {
		t.Fatal(err)
	}
	reader := &notebookProvider{responses: []protocol.ProviderResponse{
		toolCallResponse("nb1", "notebook_read", `{"notebook":"research","session_id":"`+child+`"}`),
		finalResponse("read"),
	}}
	if r, err := engine.runAgentWith(ctx, parent, reader, AgentOptions{Input: "read the child's notebook", Policy: "read-only", MaxSteps: 3}); err != nil || r.Status != "completed" {
		t.Fatalf("reader run %+v %v", r, err)
	}
	// The host read the child's version while it was still a source.
	before, err := engine.Notebook(ctx, child, childDoc.ID)
	if err != nil || before.Withdrawn {
		t.Fatalf("host read before the revocation %+v %v", before, err)
	}
	if _, err := engine.RevokeSource(ctx, SourceSession, child, "user_revoked"); err != nil {
		t.Fatal(err)
	}
	after, err := engine.Notebook(ctx, child, childDoc.ID)
	if err != nil || !after.Withdrawn {
		t.Fatalf("host read after the revocation %+v %v", after, err)
	}
	shown, err := engine.Notebook(ctx, parent, plan.ID)
	if err != nil || !shown.Withdrawn {
		t.Fatalf("host read of the fenced reader %+v %v", shown, err)
	}
	for _, w := range []struct {
		what, session, id string
		version           int64
	}{
		{"the revoked child, version read after the revocation", child, childDoc.ID, after.Version},
		{"the revoked child, version read before the revocation", child, childDoc.ID, before.Version},
		{"the fenced reader", parent, plan.ID, shown.Version},
	} {
		if doc, err := engine.SaveNotebook(ctx, w.session, w.id, "", w.version); !errors.Is(err, ErrNotebookWithdrawn) {
			t.Fatalf("SaveNotebook of %s = %+v, %v; want ErrNotebookWithdrawn", w.what, doc, err)
		}
	}
	if got, err := engine.store.GetNotebook(ctx, child, childDoc.ID); err != nil || got.Version != childDoc.Version || got.Body != childDoc.Body {
		t.Fatalf("the child's notebook changed: %+v %v", got, err)
	}
	if got, err := engine.store.GetNotebook(ctx, parent, plan.ID); err != nil || got.Version != plan.Version || got.Body != plan.Body {
		t.Fatalf("the reader's notebook changed: %+v %v", got, err)
	}
	// The run's own writes of its notebook are not host writes: a fenced reader that continues keeps
	// its notebook.
	if _, err := newNotebookAccess(engine, parent, "run2").Update(ctx, toolset.NotebookUpdate{
		Notebook: "plan", Op: "append", Content: "more", Expected: -1,
	}); err != nil {
		t.Fatalf("the fenced reader's own append: %v", err)
	}
}

func TestNotebookToolSpecsAreSmall(t *testing.T) {
	engine, sessionID := notebookEngine(t)
	set, err := toolset.New(toolset.Options{
		Workspace: engine.workspace, Policy: toolset.PolicyReadOnly,
		Notebook: newNotebookAccess(engine, sessionID, "run1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	specs := set.Specs(nil)
	total, wire := 0, 0
	for _, name := range []string{"notebook_list", "notebook_read", "notebook_update"} {
		spec, ok := findToolSpec(specs, name)
		if !ok {
			t.Fatalf("no %s", name)
		}
		raw, err := json.Marshal(spec)
		if err != nil {
			t.Fatal(err)
		}
		// The wire form the OpenAI-compatible adapter sends: {"type":"function","function":{...}}.
		sent, err := json.Marshal(map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": spec.Name, "description": spec.Description, "parameters": spec.Parameters,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		total += len(raw)
		wire += len(sent)
		t.Logf("%s: spec %d bytes, sent %d bytes", name, len(raw), len(sent))
	}
	t.Logf("three notebook specs: %d bytes as ToolSpec JSON, %d bytes as sent", total, wire)
}

func findToolSpec(specs []protocol.ToolSpec, name string) (protocol.ToolSpec, bool) {
	for _, spec := range specs {
		if spec.Name == name {
			return spec, true
		}
	}
	return protocol.ToolSpec{}, false
}
