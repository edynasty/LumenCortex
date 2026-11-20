package toolset

import (
	"context"
	"fmt"
	"os"

	"github.com/edynasty/LumenCortex/internal/editing"
	"github.com/edynasty/LumenCortex/internal/lsp"
)

// applyWorkspaceEdit writes the text edits of a language-server workspace edit. It is the file tools'
// path in every respect: each path is resolved with the same guard as write_file and apply_patch
// (workspace confinement, protected state, symlink escape), each file is read through the checked
// reader, and the whole batch is validated before the first byte is written. Preflight refuses the
// rename before any publication; a later failure keeps the already published prefix and is reported
// through the same CommitReceipt as every other file tool (R7 does not roll it back).
func (s *Set) applyWorkspaceEdit(ctx context.Context, edit lsp.WorkspaceEdit) (applied []map[string]any, result editing.Result, receipt editing.CommitReceipt, err error) {
	files, err := edit.FileEdits()
	if err != nil {
		return nil, editing.Result{}, editing.CommitReceipt{}, err
	}
	changes := make([]editing.FileChange, 0, len(files))
	seen := make(map[string]bool, len(files))
	for _, file := range files {
		target, rel, err := s.resolve(file.Path, false)
		if err != nil {
			return nil, editing.Result{}, editing.CommitReceipt{}, err
		}
		// Two paths that lead to one file (a symlink beside its target) would apply two edits to the
		// same bytes, the second computed from content the first replaced.
		real := realOf(target)
		if seen[real] {
			return nil, editing.Result{}, editing.CommitReceipt{}, fmt.Errorf("rename edits the same file twice: %s", rel)
		}
		seen[real] = true

		info, err := os.Stat(target)
		if err != nil || !info.Mode().IsRegular() {
			return nil, editing.Result{}, editing.CommitReceipt{}, fmt.Errorf("rename target is not a workspace file: %s", rel)
		}
		if info.Size() > maxEditBytes {
			return nil, editing.Result{}, editing.CommitReceipt{}, fmt.Errorf("rename target exceeds 4 MiB limit: %s", rel)
		}
		original, err := s.readChecked(target)
		if err != nil {
			return nil, editing.Result{}, editing.CommitReceipt{}, err
		}
		next, err := lsp.ApplyTextEdits(string(original), file.Edits)
		if err != nil {
			return nil, editing.Result{}, editing.CommitReceipt{}, fmt.Errorf("%s: %w", rel, err)
		}
		if len(next) > maxEditBytes {
			return nil, editing.Result{}, editing.CommitReceipt{}, fmt.Errorf("renamed file exceeds 4 MiB limit: %s", rel)
		}
		if next == string(original) {
			continue
		}
		content := []byte(next)
		changes = append(changes, editing.FileChange{Path: target, BaseRev: editing.RevOf(original), Content: &content})
	}
	if len(changes) == 0 {
		return nil, editing.Result{}, editing.CommitReceipt{}, nil
	}
	result, receipt, err = s.commitPlan(ctx, editing.Plan{Host: s, Actor: s.actor(), Tool: "lsp_rename", Changes: changes})
	if err != nil {
		return nil, result, receipt, err
	}
	applied = make([]map[string]any, 0, len(result.Files))
	for _, file := range result.Files {
		applied = append(applied, map[string]any{"path": file.Path, "operation": "update"})
	}
	return applied, result, receipt, nil
}
