package toolset

import (
	"fmt"
	"os"

	"github.com/edynasty/LumenCortex/internal/lsp"
)

// applyWorkspaceEdit writes the text edits of a language-server workspace edit. It is the file tools'
// path in every respect: each path is resolved with the same guard as write_file and apply_patch
// (workspace confinement, protected state, symlink escape), each file is read through the checked
// reader, and the whole batch is validated before the first byte is written, so a refused path leaves
// every file as it was. wrote reports whether any file was written, also when the write failed and the
// batch was rolled back.
func (s *Set) applyWorkspaceEdit(edit lsp.WorkspaceEdit) (applied []map[string]any, wrote bool, err error) {
	files, err := edit.FileEdits()
	if err != nil {
		return nil, false, err
	}
	plans := make([]plan, 0, len(files))
	seen := make(map[string]bool, len(files))
	for _, file := range files {
		target, relative, err := s.resolve(file.Path, false)
		if err != nil {
			return nil, false, err
		}
		// Two paths that lead to one file (a symlink beside its target) would apply two edits to the
		// same bytes, the second computed from content the first replaced.
		real := realOf(target)
		if seen[real] {
			return nil, false, fmt.Errorf("rename edits the same file twice: %s", relative)
		}
		seen[real] = true

		info, err := os.Stat(target)
		if err != nil || !info.Mode().IsRegular() {
			return nil, false, fmt.Errorf("rename target is not a workspace file: %s", relative)
		}
		if info.Size() > maxEditBytes {
			return nil, false, fmt.Errorf("rename target exceeds 4 MiB limit: %s", relative)
		}
		original, err := s.readChecked(target)
		if err != nil {
			return nil, false, err
		}
		next, err := lsp.ApplyTextEdits(string(original), file.Edits)
		if err != nil {
			return nil, false, fmt.Errorf("%s: %w", relative, err)
		}
		if len(next) > maxEditBytes {
			return nil, false, fmt.Errorf("renamed file exceeds 4 MiB limit: %s", relative)
		}
		if next == string(original) {
			continue
		}
		plans = append(plans, plan{operation: "update", file: target, relative: relative,
			original: string(original), next: next})
	}
	if len(plans) == 0 {
		return nil, false, nil
	}
	written := make([]plan, 0, len(plans))
	err = s.applyPlans(plans, &written)
	applied = make([]map[string]any, 0, len(written))
	for _, item := range written {
		applied = append(applied, map[string]any{"path": item.relative, "operation": "update", "bytes": len(item.next)})
	}
	if err != nil {
		return nil, len(written) > 0, err
	}
	return applied, true, nil
}
