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
	"unicode/utf8"

	"github.com/edynasty/LumenCortex/internal/editing"
	"github.com/edynasty/LumenCortex/protocol"
)

// maxAnchorRows bounds how many anchored rows one read_file returns; a model pages with start_line and
// end_line, and a header names the file's real line count.
const maxAnchorRows = 4000

// readFile reads a bounded slice of a workspace file. With anchors it renders the Hashline view: a
// header naming the revision, line count, line ending and BOM state, then "N:hhh|text" rows the model
// may anchor an edit to. Without anchors it keeps the JSON shape existing callers parse, and still
// records the revision so a later whole-file write can tell that the file moved on.
func (s *Set) readFile(_ context.Context, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	file, rel, err := s.resolve(fmt.Sprint(args["path"]), false)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	f, info, err := s.openRead(file)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	defer f.Close()
	if !info.Mode().IsRegular() {
		return protocol.ToolResult{}, errors.New("read_file target is not a regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, editing.MaxFileBytes+1))
	if err != nil {
		return protocol.ToolResult{}, err
	}
	complete := len(raw) <= editing.MaxFileBytes
	if !complete {
		raw = raw[:editing.MaxFileBytes]
	}
	anchors := boolArg(args, "anchors")
	startLine := intArg(args, "start_line", 0)
	endLine := intArg(args, "end_line", 0)
	if !anchors {
		truncated := len(raw) > maxReadBytes
		content := raw
		if truncated {
			content = raw[:maxReadBytes]
		}
		body := map[string]any{"path": rel, "content": string(content), "bytes": info.Size(), "truncated": truncated}
		if complete {
			// The revision is recorded without the text: enough for a whole-file write to notice the
			// file changed after this read (the E3 S7b race), without keeping the lines.
			rev := editing.RevOf(raw)
			s.edit.RegisterRead(s.actor(), file, rev)
			body["rev"] = rev
		}
		out, _ := json.Marshal(body)
		return protocol.ToolResult{OK: true, Content: string(out)}, nil
	}
	if !complete {
		return protocol.ToolResult{}, errors.New("read_file with anchors needs the whole file, which is over the 4 MiB edit limit")
	}
	if !utf8.Valid(raw) {
		return protocol.ToolResult{}, errors.New("read_file with anchors needs a UTF-8 file")
	}
	doc := editing.DecodeDoc(raw)
	total := len(doc.Lines)
	from := startLine
	if from < 1 {
		from = 1
	}
	to := endLine
	if to <= 0 || to > total {
		to = total
	}
	if total > 0 && from > total {
		return protocol.ToolResult{}, fmt.Errorf("start_line %d is past the end of %s (%d lines)", from, rel, total)
	}
	if to < from {
		return protocol.ToolResult{}, fmt.Errorf("end_line %d is before start_line %d", to, from)
	}
	shown := to
	if shown-from+1 > maxAnchorRows {
		shown = from + maxAnchorRows - 1
	}
	snap := s.edit.RegisterAnchoredRead(s.actor(), file, doc, from, shown)
	text := editing.Render(rel, snap.Doc, from, shown)
	if shown < to {
		text += fmt.Sprintf("# rows %d-%d of %d; continue with start_line=%d\n", from, shown, total, shown+1)
	}
	return protocol.ToolResult{OK: true, Content: text}, nil
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

// writeFile creates or replaces a file. It commits through the editing engine: when the session has read
// the file at an older revision and the file changed since, the write is refused instead of silently
// dropping the other writer's change. base_rev makes that precondition explicit.
func (s *Set) writeFile(ctx context.Context, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
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
	body := []byte(content)
	result, err := s.edit.Commit(ctx, editing.Plan{
		Actor: s.actor(),
		Host:  s,
		Tool:  "write_file",
		Changes: []editing.FileChange{{
			Path: file, BaseRev: stringArg(args, "base_rev"), Content: &body,
			AllowCreate: true, RejectStaleRead: true,
		}},
	})
	if err != nil {
		return editOutcome(err)
	}
	out, _ := json.Marshal(map[string]any{"path": rel, "bytes": len(content), "rev": result.Files[0].NewRev})
	return protocol.ToolResult{OK: true, Content: string(out), MutatesWorkspace: true}, nil
}

// replaceFile replaces one exact text occurrence. The replacement is computed against the bytes read
// here and committed only if the file still has that revision, so a concurrent change is never lost.
func (s *Set) replaceFile(ctx context.Context, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	file, rel, err := s.resolve(fmt.Sprint(args["path"]), false)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	raw, err := s.readChecked(file)
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
	splices := []editing.Splice{{Start: first, End: first + len(old), Text: next}}
	result, err := s.edit.Commit(ctx, editing.Plan{
		Actor: s.actor(),
		Host:  s,
		Tool:  "replace_in_file",
		Changes: []editing.FileChange{{
			Path: file, BaseRev: editing.RevOf(raw), Splices: splices,
		}},
	})
	if err != nil {
		return editOutcome(err)
	}
	out, _ := json.Marshal(map[string]any{"path": rel, "replaced": true, "rev": result.Files[0].NewRev})
	return protocol.ToolResult{OK: true, Content: string(out), MutatesWorkspace: true}, nil
}
