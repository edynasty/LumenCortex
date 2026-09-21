package toolset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/edynasty/LumenCortex/protocol"
)

func (s *Set) readFile(_ context.Context, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	file, rel, err := s.resolve(fmt.Sprint(args["path"]), false)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	f, err := os.Open(file)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return protocol.ToolResult{}, err
	}
	if !info.Mode().IsRegular() {
		return protocol.ToolResult{}, errors.New("read_file target is not a regular file")
	}
	buf, err := io.ReadAll(io.LimitReader(f, maxReadBytes+1))
	if err != nil {
		return protocol.ToolResult{}, err
	}
	truncated := len(buf) > maxReadBytes
	if truncated {
		buf = buf[:maxReadBytes]
	}
	raw, _ := json.Marshal(map[string]any{"path": rel, "content": string(buf), "bytes": info.Size(), "truncated": truncated})
	return protocol.ToolResult{OK: true, Content: string(raw)}, nil
}
func (s *Set) listDir(_ context.Context, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	dir, rel, err := s.resolve(fmt.Sprint(args["path"]), false)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	truncated := len(entries) > maxListEntries
	if truncated {
		entries = entries[:maxListEntries]
	}
	items := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		items = append(items, map[string]any{"name": e.Name(), "dir": e.IsDir()})
	}
	raw, _ := json.Marshal(map[string]any{"path": rel, "entries": items, "truncated": truncated})
	return protocol.ToolResult{OK: true, Content: string(raw)}, nil
}
func (s *Set) writeFile(_ context.Context, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	content := fmt.Sprint(args["content"])
	if len(content) > maxEditBytes {
		return protocol.ToolResult{}, errors.New("write_file content exceeds 4 MiB limit")
	}
	file, rel, err := s.resolve(fmt.Sprint(args["path"]), true)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return protocol.ToolResult{}, err
	}
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		return protocol.ToolResult{}, err
	}
	raw, _ := json.Marshal(map[string]any{"path": rel, "bytes": len(content)})
	return protocol.ToolResult{OK: true, Content: string(raw), MutatesWorkspace: true}, nil
}
func (s *Set) replaceFile(_ context.Context, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	file, rel, err := s.resolve(fmt.Sprint(args["path"]), false)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	if len(raw) > maxEditBytes {
		return protocol.ToolResult{}, errors.New("replace target exceeds 4 MiB limit")
	}
	old := fmt.Sprint(args["old_text"])
	next := fmt.Sprint(args["new_text"])
	if old == "" {
		return protocol.ToolResult{}, errors.New("old_text is required")
	}
	source := string(raw)
	first := strings.Index(source, old)
	if first < 0 {
		return protocol.ToolResult{}, errors.New("old_text not found")
	}
	if strings.Index(source[first+len(old):], old) >= 0 {
		return protocol.ToolResult{}, errors.New("old_text is ambiguous")
	}
	result := source[:first] + next + source[first+len(old):]
	if err := os.WriteFile(file, []byte(result), 0o644); err != nil {
		return protocol.ToolResult{}, err
	}
	out, _ := json.Marshal(map[string]any{"path": rel, "replaced": true})
	return protocol.ToolResult{OK: true, Content: string(out), MutatesWorkspace: true}, nil
}
