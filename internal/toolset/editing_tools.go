package toolset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/edynasty/LumenCortex/internal/editing"
	"github.com/edynasty/LumenCortex/internal/editing/goedit"
	"github.com/edynasty/LumenCortex/protocol"
)

// editSpecs is the static list of the structured editing tools. registerEditingTools binds each spec to
// its handler; nothing else defines them.
func editSpecs() []protocol.ToolSpec {
	opSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"op":    map[string]any{"type": "string", "enum": []string{editing.OpReplace, editing.OpDelete, editing.OpInsertAfter, editing.OpInsertBefore}},
			"start": str(),
			"end":   str(),
			"text":  str(),
		},
		"required": []string{"op", "start"}, "additionalProperties": false,
	}
	return []protocol.ToolSpec{
		{
			Name: "edit_lines",
			Description: "Edit a file by line, against the revision and anchors read_file showed. ops address lines of that " +
				"revision: replace, delete, insert_before, insert_after. Every anchor must be a line this session was " +
				"shown; if the file changed since, the edit is refused as stale (or relocated when the change is " +
				"outside the edited lines and their neighbours), so no concurrent change is lost. Returns the new " +
				"revision and fresh anchors around what changed.",
			Permission:       "write",
			MutatesWorkspace: true,
			Parameters: obj(map[string]any{
				"path": str(),
				"rev":  map[string]any{"type": "string", "description": "The rev= value of the read_file header the anchors came from."},
				"ops":  map[string]any{"type": "array", "minItems": 1, "items": opSchema},
			}, "path", "rev", "ops"),
		},
		{
			Name: "edit_go",
			Description: "Edit a Go package structurally. The AST locates the target and the edit lands as a byte splice, so " +
				"comments and formatting outside it are preserved. ops: replace_body, replace_decl, add_param, " +
				"remove_param, rename_param, rename, add_member, remove_member, replace_member, add_import, " +
				"remove_import, find. A symbol is pkg.Name, (*T).M, T.M, or Func.param. A rename is scope-aware " +
				"and refuses a name it would capture; a package-level rename reaches every file of the package. " +
				"Syntax and types are checked before anything is written; preview returns a diff and writes nothing.",
			Permission:       "write",
			MutatesWorkspace: true,
			Parameters: obj(map[string]any{
				"path":    str(),
				"preview": map[string]any{"type": "boolean"},
				"ops": map[string]any{
					"type": "array", "minItems": 1,
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"op":          map[string]any{"type": "string", "enum": []string{goedit.OpFind, goedit.OpReplaceBody, goedit.OpReplaceDecl, goedit.OpAddParam, goedit.OpRemoveParam, goedit.OpRenameParam, goedit.OpRename, goedit.OpAddMember, goedit.OpRemoveMember, goedit.OpReplaceMember, goedit.OpAddImport, goedit.OpRemoveImport}},
							"symbol":      str(),
							"name":        str(),
							"new_name":    str(),
							"text":        str(),
							"import_path": str(),
						},
						"required": []string{"op"}, "additionalProperties": false,
					},
				},
			}, "path", "ops"),
		},
	}
}

// registerEditingTools binds the structured editing tools. They are write tools, so the read-only policy
// withholds them like every other write tool.
func (s *Set) registerEditingTools() {
	handlers := map[string]func(context.Context, map[string]any, func(protocol.ToolOutput)) (protocol.ToolResult, error){
		"edit_lines": s.editLines,
		"edit_go":    s.editGo,
	}
	for _, spec := range editSpecs() {
		s.add(spec, handlers[spec.Name])
	}
}

// editLines applies anchored line operations from one snapshot.
func (s *Set) editLines(ctx context.Context, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	file, rel, err := s.resolve(stringArg(args, "path"), false)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	rev := strings.TrimSpace(stringArg(args, "rev"))
	if rev == "" {
		return protocol.ToolResult{}, errors.New("rev is required: use the rev= value of the read_file header")
	}
	ops, err := decodeAnchorOps(args["ops"])
	if err != nil {
		return protocol.ToolResult{}, err
	}
	snap, err := s.edit.SnapshotFor(s.actor(), file, rev)
	if err != nil {
		return editOutcome(err)
	}
	lineOps, err := editing.ParseOps(snap, ops)
	if err != nil {
		return editOutcome(err)
	}
	result, err := s.edit.Commit(ctx, editing.Plan{
		Actor:    s.actor(),
		Tool:     "edit_lines",
		Changes:  []editing.FileChange{{Path: file, BaseRev: rev, LineOps: lineOps}},
		Validate: validatorsFor(rel),
	})
	if err != nil {
		return editOutcome(err)
	}
	file1 := result.Files[0]
	extra := map[string]any{"path": rel, "rev": file1.NewRev, "applied": len(lineOps)}
	if file1.Changed {
		// Fresh anchors around what changed, so the next edit needs no re-read.
		if fresh, snapErr := s.edit.SnapshotFor(s.actor(), file, file1.NewRev); snapErr == nil {
			extra["anchors"] = editing.RenderWindows(rel, fresh.Doc, anchorWindows(file1.Landed, len(fresh.Doc.Lines)))
		}
	}
	if note := typeCheckNote(result.Diagnostics); note != "" {
		extra["typeCheck"] = note
	}
	return editResult(result, extra), nil
}

// anchorWindows turns the landed line ranges into the windows a caller shows: each edit plus two lines
// of context on each side.
func anchorWindows(landed []editing.LandedOp, total int) [][2]int {
	out := make([][2]int, 0, len(landed))
	for _, op := range landed {
		from, to := op.Start-2, op.End+2
		if op.Start > op.End {
			from, to = op.Start-2, op.Start+2
		}
		if from < 1 {
			from = 1
		}
		if to > total {
			to = total
		}
		if from > to {
			continue
		}
		out = append(out, [2]int{from, to})
	}
	return out
}

// decodeAnchorOps reads the ops argument. The wire shape is JSON, but a caller may hand the tool a
// decoded slice already (an eval callback, a test), so both are accepted.
func decodeAnchorOps(value any) ([]editing.AnchorOp, error) {
	if value == nil {
		return nil, errors.New("ops is required")
	}
	if typed, ok := value.([]editing.AnchorOp); ok {
		return typed, nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var ops []editing.AnchorOp
	if err := json.Unmarshal(raw, &ops); err != nil {
		return nil, fmt.Errorf("ops must be an array of {op, start, end, text}: %w", err)
	}
	if len(ops) == 0 {
		return nil, errors.New("ops must contain at least one operation")
	}
	return ops, nil
}

// validatorsFor runs the Go validators on a .go file and nothing on any other file.
func validatorsFor(relative string) []editing.Validator {
	if !strings.HasSuffix(relative, ".go") {
		return nil
	}
	return []editing.Validator{editing.GoSyntax(), editing.GoTypes()}
}

// editGo plans and commits a structural Go edit.
func (s *Set) editGo(ctx context.Context, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	file, rel, err := s.resolve(stringArg(args, "path"), false)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	if !strings.HasSuffix(rel, ".go") {
		return protocol.ToolResult{}, errors.New("edit_go edits Go files; use lsp_rename or edit_lines for other languages")
	}
	ops, err := decodeGoOps(args["ops"])
	if err != nil {
		return protocol.ToolResult{}, err
	}
	plan, err := goedit.Plan(s, file, ops)
	if err != nil {
		return editOutcome(err)
	}
	preview := boolArg(args, "preview")
	result, err := s.edit.Commit(ctx, editing.Plan{
		Actor:    s.actor(),
		Tool:     "edit_go",
		Changes:  plan.Changes,
		Validate: s.goValidators(),
		Preview:  preview,
	})
	if err != nil {
		return editOutcome(err)
	}
	extra := map[string]any{"path": rel, "symbols": plan.Symbols, "notes": plan.Notes, "preview": preview}
	if note := typeCheckNote(result.Diagnostics); note != "" {
		extra["typeCheck"] = note
	}
	if preview {
		payload := map[string]any{"path": rel, "preview": true, "files": result.Files, "diffs": result.Diffs}
		for key, value := range extra {
			payload[key] = value
		}
		content, err := json.Marshal(payload)
		if err != nil {
			return protocol.ToolResult{}, err
		}
		return protocol.ToolResult{OK: true, Content: string(content)}, nil
	}
	return editResult(result, extra), nil
}

// decodeGoOps reads the ops argument, accepting a decoded slice as well as JSON.
func decodeGoOps(value any) ([]goedit.Op, error) {
	if value == nil {
		return nil, errors.New("ops is required")
	}
	if typed, ok := value.([]goedit.Op); ok {
		return typed, nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var ops []goedit.Op
	if err := json.Unmarshal(raw, &ops); err != nil {
		return nil, fmt.Errorf("ops must be an array of structural edits: %w", err)
	}
	if len(ops) == 0 {
		return nil, errors.New("ops must contain at least one operation")
	}
	return ops, nil
}

// Read reads a file for goedit through the same guard as every other read.
func (s *Set) Read(path string) ([]byte, error) {
	file, _, err := s.resolve(path, false)
	if err != nil {
		return nil, err
	}
	return s.readChecked(file)
}

// List lists a directory's Go files for goedit through the same guard.
func (s *Set) List(dir string) ([]string, error) {
	absolute, _, err := s.resolve(dir, false)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(absolute)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		out = append(out, filepath.Join(absolute, name))
	}
	return out, nil
}
