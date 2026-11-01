package lsp

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// FileEdit is the part of a WorkspaceEdit that changes one file's text.
type FileEdit struct {
	Path  string
	Edits []TextEdit
}

// FileEdits flattens a workspace edit into one entry per file, sorted by path. A resource operation
// (create, rename, delete) is refused: the caller applies text edits only, and a rename that would
// move or create files must not be half-applied.
func (w WorkspaceEdit) FileEdits() ([]FileEdit, error) {
	out := make([]FileEdit, 0, len(w.Changes)+len(w.DocumentChanges))
	for _, change := range w.DocumentChanges {
		if change.Kind != "" {
			return nil, fmt.Errorf("workspace edit resource operation %q is not supported", change.Kind)
		}
		if change.TextDocument == nil {
			continue
		}
		path, err := pathFromURI(change.TextDocument.URI)
		if err != nil {
			return nil, err
		}
		out = append(out, FileEdit{Path: path, Edits: change.Edits})
	}
	uris := make([]string, 0, len(w.Changes))
	for uri := range w.Changes {
		uris = append(uris, uri)
	}
	sort.Strings(uris)
	for _, uri := range uris {
		path, err := pathFromURI(uri)
		if err != nil {
			return nil, err
		}
		out = append(out, FileEdit{Path: path, Edits: w.Changes[uri]})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	merged := out[:0]
	for _, edit := range out {
		if n := len(merged); n > 0 && merged[n-1].Path == edit.Path {
			merged[n-1].Edits = append(merged[n-1].Edits, edit.Edits...)
			continue
		}
		merged = append(merged, edit)
	}
	return merged, nil
}

// pathFromURI turns a file URI into a path. Any other scheme is refused: an edit for a document that
// is not a workspace file has no path the file guard could check.
func pathFromURI(uri string) (string, error) {
	parsed, err := url.Parse(uri)
	if err != nil {
		return "", fmt.Errorf("workspace edit has an unreadable URI: %w", err)
	}
	if !strings.EqualFold(parsed.Scheme, "file") {
		return "", fmt.Errorf("workspace edit names %q, which is not a file path", uri)
	}
	path := parsed.Path
	if path == "" {
		return "", errors.New("workspace edit has an empty file path")
	}
	if runtime.GOOS == "windows" && len(path) > 2 && path[0] == '/' && path[2] == ':' {
		path = path[1:]
	}
	return filepath.FromSlash(path), nil
}

// ApplyTextEdits applies the edits of one file to its content. Positions are UTF-16 code units, as the
// protocol specifies; an edit past the end of the text, or two edits that overlap, is refused so the
// caller writes nothing.
func ApplyTextEdits(source string, edits []TextEdit) (string, error) {
	type span struct {
		start, end int
		text       string
	}
	spans := make([]span, 0, len(edits))
	for i, edit := range edits {
		start, err := offsetAt(source, edit.Range.Start)
		if err != nil {
			return "", fmt.Errorf("edit %d start: %w", i+1, err)
		}
		end, err := offsetAt(source, edit.Range.End)
		if err != nil {
			return "", fmt.Errorf("edit %d end: %w", i+1, err)
		}
		if end < start {
			return "", fmt.Errorf("edit %d ends before it starts", i+1)
		}
		spans = append(spans, span{start: start, end: end, text: edit.NewText})
	}
	sort.SliceStable(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	for i := 1; i < len(spans); i++ {
		if spans[i].start < spans[i-1].end {
			return "", errors.New("edits overlap")
		}
	}
	out := source
	for i := len(spans) - 1; i >= 0; i-- {
		out = out[:spans[i].start] + spans[i].text + out[spans[i].end:]
	}
	return out, nil
}

// offsetAt converts a protocol position into a byte offset. A position inside a surrogate pair or
// past the end of a line is refused rather than guessed at.
func offsetAt(source string, pos Position) (int, error) {
	if pos.Line < 0 || pos.Character < 0 {
		return 0, errors.New("negative position")
	}
	line, start := 0, 0
	for line < pos.Line {
		next := strings.IndexByte(source[start:], '\n')
		if next < 0 {
			return 0, fmt.Errorf("line %d is past the end of the file", pos.Line)
		}
		start += next + 1
		line++
	}
	units := 0
	for i, r := range source[start:] {
		if units >= pos.Character {
			return start + i, nil
		}
		if r == '\n' {
			return 0, fmt.Errorf("character %d is past the end of line %d", pos.Character, pos.Line)
		}
		if r > 0xFFFF {
			units += 2
		} else {
			units++
		}
	}
	if units >= pos.Character {
		return len(source), nil
	}
	return 0, fmt.Errorf("character %d is past the end of line %d", pos.Character, pos.Line)
}
