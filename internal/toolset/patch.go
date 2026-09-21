package toolset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/edynasty/LumenCortex/protocol"
)

func (s *Set) applyPatch(_ context.Context, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
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
	plans := make([]plan, 0, len(patches))
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
			originalBytes, err := os.ReadFile(file)
			if err != nil {
				return protocol.ToolResult{}, err
			}
			next, err := applyExactEdits(string(originalBytes), item.Edits, relative)
			if err != nil {
				return protocol.ToolResult{}, err
			}
			if len(next) > maxEditBytes {
				return protocol.ToolResult{}, fmt.Errorf("updated file exceeds 4 MiB limit: %s", relative)
			}
			plans = append(plans, plan{operation: item.Operation, file: file, relative: relative, original: string(originalBytes), next: next})
		case "create":
			if _, err := os.Lstat(file); err == nil {
				return protocol.ToolResult{}, fmt.Errorf("create target already exists: %s", relative)
			} else if !os.IsNotExist(err) {
				return protocol.ToolResult{}, err
			}
			if len(item.Content) > maxEditBytes {
				return protocol.ToolResult{}, fmt.Errorf("create content exceeds 4 MiB limit: %s", relative)
			}
			plans = append(plans, plan{operation: item.Operation, file: file, relative: relative, next: item.Content})
		case "delete":
			info, err := os.Stat(file)
			if err != nil || !info.Mode().IsRegular() {
				return protocol.ToolResult{}, fmt.Errorf("delete target not found: %s", relative)
			}
			if info.Size() > maxEditBytes {
				return protocol.ToolResult{}, fmt.Errorf("delete target exceeds 4 MiB rollback limit: %s", relative)
			}
			originalBytes, err := os.ReadFile(file)
			if err != nil {
				return protocol.ToolResult{}, err
			}
			plans = append(plans, plan{operation: item.Operation, file: file, relative: relative, original: string(originalBytes)})
		default:
			return protocol.ToolResult{}, fmt.Errorf("unsupported patch operation at index %d: %s", index, item.Operation)
		}
	}

	applied := make([]plan, 0, len(plans))
	for _, item := range plans {
		var err error
		switch item.operation {
		case "delete":
			err = os.Remove(item.file)
		default:
			if mkdirErr := os.MkdirAll(filepath.Dir(item.file), 0o755); mkdirErr != nil {
				err = mkdirErr
			} else {
				err = os.WriteFile(item.file, []byte(item.next), 0o644)
			}
		}
		if err != nil {
			rollbackPlans(applied)
			return protocol.ToolResult{}, err
		}
		applied = append(applied, item)
	}

	result := make([]map[string]any, 0, len(plans))
	for _, item := range plans {
		size := 0
		if item.operation != "delete" {
			size = len(item.next)
		}
		result = append(result, map[string]any{"path": item.relative, "operation": item.operation, "bytes": size})
	}
	content, _ := json.Marshal(result)
	return protocol.ToolResult{OK: true, Content: string(content), MutatesWorkspace: true}, nil
}

func rollbackPlans(plans []plan) {
	for i := len(plans) - 1; i >= 0; i-- {
		item := plans[i]
		switch item.operation {
		case "create":
			_ = os.Remove(item.file)
		case "update", "delete":
			_ = os.MkdirAll(filepath.Dir(item.file), 0o755)
			_ = os.WriteFile(item.file, []byte(item.original), 0o644)
		}
	}
}

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
