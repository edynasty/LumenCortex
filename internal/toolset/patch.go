package toolset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/edynasty/LumenCortex/internal/editing"
	"github.com/edynasty/LumenCortex/protocol"
)

// applyPatch applies a validated atomic batch of exact text edits, creates and deletes. Every patch is
// resolved and validated before anything is written, and the batch is committed through the editing
// engine: one file's failure rolls back the files already written.
func (s *Set) applyPatch(ctx context.Context, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	raw, err := json.Marshal(args["patches"])
	if err != nil {
		return protocol.ToolResult{}, err
	}
	var patches []patch
	if err := json.Unmarshal(raw, &patches); err != nil {
		return protocol.ToolResult{}, errors.New("patches must be an array")
	}
	if len(patches) == 0 {
		return protocol.ToolResult{}, errors.New("patches must contain at least one patch")
	}

	seen := map[string]bool{}
	changes := make([]editing.FileChange, 0, len(patches))
	reported := make([]map[string]any, 0, len(patches))
	for index, item := range patches {
		file, relative, err := s.resolve(item.Path, item.Operation == "create")
		if err != nil {
			return protocol.ToolResult{}, err
		}
		if seen[file] {
			return protocol.ToolResult{}, fmt.Errorf("duplicate patch path: %s", relative)
		}
		seen[file] = true

		switch item.Operation {
		case "update":
			info, err := os.Stat(file)
			if err != nil || !info.Mode().IsRegular() {
				return protocol.ToolResult{}, fmt.Errorf("update target not found: %s", relative)
			}
			if info.Size() > maxEditBytes {
				return protocol.ToolResult{}, fmt.Errorf("update target exceeds 4 MiB limit: %s", relative)
			}
			if len(item.Edits) == 0 {
				return protocol.ToolResult{}, fmt.Errorf("update requires edits: %s", relative)
			}
			original, err := s.readChecked(file)
			if err != nil {
				return protocol.ToolResult{}, err
			}
			next, err := applyExactEdits(string(original), item.Edits, relative)
			if err != nil {
				return protocol.ToolResult{}, err
			}
			if len(next) > maxEditBytes {
				return protocol.ToolResult{}, fmt.Errorf("updated file exceeds 4 MiB limit: %s", relative)
			}
			edits := make([]editing.TextEdit, 0, len(item.Edits))
			for _, edit := range item.Edits {
				edits = append(edits, editing.TextEdit{Old: edit.OldText, New: edit.NewText})
			}
			changes = append(changes, editing.FileChange{Path: file, BaseRev: editing.RevOf(original), Edits: edits})
			reported = append(reported, map[string]any{"path": relative, "operation": item.Operation, "bytes": len(next)})
		case "create":
			if _, err := os.Lstat(file); err == nil {
				return protocol.ToolResult{}, fmt.Errorf("create target already exists: %s", relative)
			} else if !os.IsNotExist(err) {
				return protocol.ToolResult{}, err
			}
			if len(item.Content) > maxEditBytes {
				return protocol.ToolResult{}, fmt.Errorf("create content exceeds 4 MiB limit: %s", relative)
			}
			content := []byte(item.Content)
			changes = append(changes, editing.FileChange{Path: file, Content: &content, Create: true})
			reported = append(reported, map[string]any{"path": relative, "operation": item.Operation, "bytes": len(content)})
		case "delete":
			info, err := os.Stat(file)
			if err != nil || !info.Mode().IsRegular() {
				return protocol.ToolResult{}, fmt.Errorf("delete target not found: %s", relative)
			}
			if info.Size() > maxEditBytes {
				return protocol.ToolResult{}, fmt.Errorf("delete target exceeds 4 MiB rollback limit: %s", relative)
			}
			changes = append(changes, editing.FileChange{Path: file, Delete: true})
			reported = append(reported, map[string]any{"path": relative, "operation": item.Operation, "bytes": 0})
		default:
			return protocol.ToolResult{}, fmt.Errorf("unsupported patch operation at index %d: %s", index, item.Operation)
		}
	}

	if _, err := s.edit.Commit(ctx, editing.Plan{Host: s, Actor: s.actor(), Tool: "apply_patch", Changes: changes}); err != nil {
		return editOutcome(err)
	}
	content, _ := json.Marshal(reported)
	return protocol.ToolResult{OK: true, Content: string(content), MutatesWorkspace: true}, nil
}

// applyExactEdits validates a patch's edits against the current content, applying them in order the way
// the engine will: each old_text must occur exactly once.
func applyExactEdits(source string, edits []edit, relative string) (string, error) {
	next := source
	for index, item := range edits {
		if item.OldText == "" {
			return "", fmt.Errorf("patch edit %d has empty old_text: %s", index+1, relative)
		}
		first := strings.Index(next, item.OldText)
		if first < 0 {
			return "", fmt.Errorf("patch edit %d old_text not found: %s", index+1, relative)
		}
		if strings.Index(next[first+len(item.OldText):], item.OldText) >= 0 {
			return "", fmt.Errorf("patch edit %d old_text is ambiguous: %s", index+1, relative)
		}
		next = next[:first] + item.NewText + next[first+len(item.OldText):]
	}
	return next, nil
}
