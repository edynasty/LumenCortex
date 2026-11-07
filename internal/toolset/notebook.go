package toolset

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/edynasty/LumenCortex/protocol"
)

// PermissionState is the permission of a tool that writes only the run's own durable state, never a
// workspace file, a command or another session: notebook_update is the only one (ADR-0020). Every
// policy admits it, the read-only one included, because a read-only run may still keep notes for
// itself; it cannot change anything else. "Its own durable state" is the run's working notebooks: an
// execution notebook — the cells a user runs and the outputs they produced — is never read or written
// by these tools under any policy, so a prompt cannot talk a run into planting code or forging
// results.
const PermissionState = "state"

// NotebookStore is the working notebook of one run (ADR-0020), bound by the runtime to the run's
// session and run id. The run lists and reads its own notebooks and those of its direct children, and
// writes only its own. Every method returns a JSON-marshalable value.
type NotebookStore interface {
	// List returns the notebooks of sessionID; "" means the run's own session.
	List(ctx context.Context, sessionID string) (any, error)
	// Read returns one notebook of sessionID, or one section of it, by title or id.
	Read(ctx context.Context, sessionID, notebook, section string) (any, error)
	// Update writes the run's own notebook and returns its new version and size.
	Update(ctx context.Context, update NotebookUpdate) (any, error)
}

// NotebookUpdate is one write of the run's own notebook.
type NotebookUpdate struct {
	// Notebook is the notebook's title or id.
	Notebook string
	// Op is create, replace, append or section.
	Op string
	// Content is the new body (create, replace), the text to add (append) or the section's new text.
	Content string
	// Section names the section a section write replaces or creates.
	Section string
	// Expected is the version the write requires, -1 when the write applies to the current version.
	Expected int64
}

// NotebookProblem is a notebook operation that did not happen for a reason the model can act on (an
// unknown notebook, a session the run may not read, a body past the limit). The tool reports it as an
// ok=false result with the reason; it is not a failed call.
type NotebookProblem struct{ Reason string }

func (p *NotebookProblem) Error() string { return p.Reason }

// NotebookConflict reports a write that expected another version. Nothing was written; the model
// re-reads the notebook and retries with the version it now has.
type NotebookConflict struct{ CurrentVersion int64 }

func (c *NotebookConflict) Error() string {
	return "the notebook was written by someone else: re-read it and retry with the current version"
}

func (s *Set) registerNotebookTools() {
	if s.notebooks == nil {
		return
	}
	s.add(protocol.ToolSpec{
		Name: "notebook_list",
		Description: "List your working notebooks: title, version, size, when and by whom they were last written, and their " +
			"'## ' section headings. Add session_id to list a direct child subagent's notebooks.",
		Permission: PermissionState,
		Parameters: obj(map[string]any{
			"session_id": map[string]any{"type": "string", "description": "a direct child subagent's session; omit for your own"},
		}),
	}, s.listNotebooks)
	s.add(protocol.ToolSpec{
		Name: "notebook_read",
		Description: "Read one of your working notebooks by title or id, or one '## ' section of it. Read the Handoff section " +
			"before continuing earlier work; add session_id to read a direct child subagent's notebook.",
		Permission: PermissionState,
		Parameters: obj(map[string]any{
			"notebook":   str(),
			"section":    map[string]any{"type": "string", "description": "read only this '## ' section"},
			"session_id": map[string]any{"type": "string", "description": "a direct child subagent's session; omit for your own"},
		}, "notebook"),
	}, s.readNotebook)
	s.add(protocol.ToolSpec{
		Name: "notebook_update",
		Description: "Write your working notebook. op create needs a new title and no version; replace needs the whole new body " +
			"and expected_version; append adds content at the end; section replaces one '## ' section, creating it at the end " +
			"when absent. A conflict returns the current version: re-read, then retry. Returns the new version and size.",
		Permission: PermissionState,
		Parameters: obj(map[string]any{
			"notebook":         map[string]any{"type": "string", "description": "the new title for create, else the title or id of an existing notebook"},
			"op":               map[string]any{"type": "string", "enum": []string{"create", "replace", "append", "section"}},
			"content":          str(),
			"section":          map[string]any{"type": "string", "description": "the section a section write replaces or creates"},
			"expected_version": map[string]any{"type": "integer", "minimum": 1, "description": "the version the write requires; replace requires it"},
		}, "notebook", "op", "content"),
	}, s.updateNotebook)
}

func (s *Set) listNotebooks(ctx context.Context, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	result, err := s.notebooks.List(ctx, strings.TrimSpace(stringArg(args, "session_id")))
	if err != nil {
		return notebookFailure(err)
	}
	return jsonResult(result)
}

func (s *Set) readNotebook(ctx context.Context, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	notebook := strings.TrimSpace(stringArg(args, "notebook"))
	if notebook == "" {
		return protocol.ToolResult{}, errors.New("notebook is required")
	}
	result, err := s.notebooks.Read(ctx, strings.TrimSpace(stringArg(args, "session_id")), notebook, strings.TrimSpace(stringArg(args, "section")))
	if err != nil {
		return notebookFailure(err)
	}
	return jsonResult(result)
}

func (s *Set) updateNotebook(ctx context.Context, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	update := NotebookUpdate{
		Notebook: strings.TrimSpace(stringArg(args, "notebook")),
		Op:       strings.TrimSpace(stringArg(args, "op")),
		Content:  stringArg(args, "content"),
		Section:  strings.TrimSpace(stringArg(args, "section")),
		Expected: -1,
	}
	switch v := args["expected_version"].(type) {
	case nil:
	case float64:
		if v < 1 || v != float64(int64(v)) {
			return protocol.ToolResult{}, errors.New("expected_version must be a positive integer")
		}
		update.Expected = int64(v)
	case int:
		if v < 1 {
			return protocol.ToolResult{}, errors.New("expected_version must be a positive integer")
		}
		update.Expected = int64(v)
	case int64:
		if v < 1 {
			return protocol.ToolResult{}, errors.New("expected_version must be a positive integer")
		}
		update.Expected = v
	default:
		return protocol.ToolResult{}, errors.New("expected_version must be a positive integer")
	}
	switch {
	case update.Notebook == "":
		return protocol.ToolResult{}, errors.New("notebook is required")
	case update.Op != "create" && update.Op != "replace" && update.Op != "append" && update.Op != "section":
		return protocol.ToolResult{}, errors.New("op must be create, replace, append or section")
	case update.Op == "replace" && update.Expected < 0:
		return protocol.ToolResult{}, errors.New("replace needs expected_version: read the notebook first")
	case update.Op == "section" && update.Section == "":
		return protocol.ToolResult{}, errors.New("section is required for op section")
	}
	result, err := s.notebooks.Update(ctx, update)
	if err != nil {
		return notebookFailure(err)
	}
	return jsonResult(result)
}

// notebookFailure turns a notebook problem the model can act on into its result, and leaves every
// other error to the loop as a failed call.
func notebookFailure(err error) (protocol.ToolResult, error) {
	var problem *NotebookProblem
	if errors.As(err, &problem) {
		content, marshalErr := json.Marshal(map[string]any{"error": problem.Reason})
		if marshalErr != nil {
			return protocol.ToolResult{}, marshalErr
		}
		return protocol.ToolResult{OK: false, Content: string(content)}, nil
	}
	var conflict *NotebookConflict
	if errors.As(err, &conflict) {
		content, marshalErr := json.Marshal(map[string]any{
			"error":          err.Error(),
			"currentVersion": conflict.CurrentVersion,
		})
		if marshalErr != nil {
			return protocol.ToolResult{}, marshalErr
		}
		return protocol.ToolResult{OK: false, Content: string(content)}, nil
	}
	return protocol.ToolResult{}, err
}
