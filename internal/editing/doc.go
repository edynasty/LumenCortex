// Package editing is the one write path for workspace files. Every front end — whole-file writes,
// exact text patches, Hashline line edits, Go AST edits and language-server workspace edits — produces
// the same kind of plan, and Engine.Commit applies it: resolve through the caller's path guard, take the
// per-path lock, check the write lease, compare the revision, validate, write with compare-and-swap on
// the open descriptor, rehash, and publish the outcome as an edit event.
//
// Text is modeled as a revision plus lines: Rev is 40 bits of SHA-256 over the raw bytes, so any byte
// change — including a BOM, a CRLF, or a missing final newline — produces a different revision. A line
// anchor is its 1-based number, its 3-symbol line hash, and the snapshot revision it was shown at. The
// hash locates nothing on its own (36.7% of the lines in a Go tree repeat inside their file); it only
// proves that the numbered line is the one the model saw.
package editing

import (
	"bytes"
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// MaxFileBytes is the largest file the engine reads or writes, the same 4 MiB limit the file tools
// document. A plan whose result would exceed it is refused before anything is written.
const MaxFileBytes = 4 << 20

// bomUTF8 is the byte order mark the engine preserves.
const bomUTF8 = "\xef\xbb\xbf"

// b32 is the revision alphabet: lower-case letters and digits without look-alikes, unpadded, so a
// revision is 8 symbols of 40 bits and a line hash is 3 symbols of 15 bits.
var b32 = base32.NewEncoding("abcdefghijkmnpqrstuvwxyz23456789").WithPadding(base32.NoPadding)

// RevOf returns the revision of raw bytes: 8 symbols, 40 bits of SHA-256. It is the primary key of a
// snapshot: it binds the exact bytes a model was shown, BOM and line endings included.
func RevOf(raw []byte) string {
	sum := sha256.Sum256(raw)
	return b32.EncodeToString(sum[:])[:8]
}

// LineHash returns the 3-symbol hash of a line with its trailing blanks removed, so a formatter that
// only changes trailing whitespace does not invalidate an anchor.
func LineHash(line string) string {
	sum := sha256.Sum256([]byte(strings.TrimRight(line, " \t")))
	return b32.EncodeToString(sum[:])[:3]
}

// Doc is a decoded UTF-8 text file: its lines without terminators, each line's own terminator, and the
// exact bytes (BOM included) it was decoded from. Lines and terminators are kept separately so a mixed
// line-ending file keeps every line as it was while inserted lines take the dominant ending.
type Doc struct {
	// Raw is the exact byte content the revision names.
	Raw []byte
	// Rev is RevOf(Raw).
	Rev string
	// BOM reports a leading UTF-8 byte order mark.
	BOM bool
	// Lines are the line contents, terminators excluded.
	Lines []string
	// EOLs holds each line's terminator: "\n", "\r\n", or "" for a final line without one.
	EOLs []string
	// Starts holds each line's byte offset in Raw.
	Starts []int
	// EOL is the dominant terminator: "crlf" when any line ends with one, else "lf".
	EOL string
}

// DecodeDoc decodes raw bytes. It does not validate UTF-8; callers that are about to edit text check
// with utf8.Valid and refuse a non-UTF-8 file without writing it.
func DecodeDoc(raw []byte) Doc {
	d := Doc{Raw: raw, Rev: RevOf(raw)}
	body := raw
	if bytes.HasPrefix(body, []byte(bomUTF8)) {
		d.BOM = true
		body = body[len(bomUTF8):]
	}
	start := 0
	offset := len(raw) - len(body)
	for i := range body {
		if body[i] != '\n' {
			continue
		}
		line, eol := body[start:i], "\n"
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line, eol = line[:len(line)-1], "\r\n"
		}
		d.Lines = append(d.Lines, string(line))
		d.EOLs = append(d.EOLs, eol)
		d.Starts = append(d.Starts, offset)
		offset += i + 1 - start
		start = i + 1
	}
	if start < len(body) {
		d.Lines = append(d.Lines, string(body[start:]))
		d.EOLs = append(d.EOLs, "")
		d.Starts = append(d.Starts, offset)
	}
	d.EOL = "lf"
	for _, eol := range d.EOLs {
		if eol == "\r\n" {
			d.EOL = "crlf"
			break
		}
	}
	return d
}

// textEOL is the terminator inserted lines take.
func (d Doc) textEOL() string {
	if d.EOL == "crlf" {
		return "\r\n"
	}
	return "\n"
}

// Encode rebuilds the bytes: BOM, then every line with its own terminator.
func (d Doc) Encode() []byte {
	var b bytes.Buffer
	if d.BOM {
		b.WriteString(bomUTF8)
	}
	for i, line := range d.Lines {
		b.WriteString(line)
		if i < len(d.EOLs) {
			b.WriteString(d.EOLs[i])
		}
	}
	return b.Bytes()
}

// Locate maps a byte offset in Raw to a 1-based line and a 0-based column inside that line's text. An
// offset inside a terminator, or at the end of the file, clamps to the end of the line it follows.
func (d Doc) Locate(offset int) (line, column int) {
	if len(d.Lines) == 0 {
		return 1, 0
	}
	if offset < 0 {
		offset = 0
	}
	i := sort.Search(len(d.Starts), func(i int) bool { return d.Starts[i] > offset }) - 1
	if i < 0 {
		i = 0
	}
	column = offset - d.Starts[i]
	if column < 0 {
		column = 0
	}
	if column > len(d.Lines[i]) {
		column = len(d.Lines[i])
	}
	return i + 1, column
}

// Offset is the byte offset of a 1-based line and a 0-based column, clamped to the line's text.
func (d Doc) Offset(line, column int) int {
	if len(d.Lines) == 0 {
		if d.BOM {
			return len(bomUTF8)
		}
		return 0
	}
	if line < 1 {
		line = 1
	}
	if line > len(d.Lines) {
		line = len(d.Lines)
	}
	if column < 0 {
		column = 0
	}
	if column > len(d.Lines[line-1]) {
		column = len(d.Lines[line-1])
	}
	return d.Starts[line-1] + column
}

// Anchor is a line number plus the hash of the line the model was shown at that number.
type Anchor struct {
	Line int
	Hash string
}

// ParseAnchor parses "N:hhh".
func ParseAnchor(s string) (Anchor, error) {
	n, h, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok {
		return Anchor{}, fmt.Errorf("anchor %q: want N:hhh", s)
	}
	line, err := strconv.Atoi(n)
	if err != nil || line < 1 || len(h) != 3 {
		return Anchor{}, fmt.Errorf("anchor %q: want N:hhh", s)
	}
	return Anchor{Line: line, Hash: h}, nil
}

// FormatAnchor renders an anchor for a snapshot line.
func FormatAnchor(d Doc, line int) string {
	if line < 1 || line > len(d.Lines) {
		return ""
	}
	return fmt.Sprintf("%d:%s", line, LineHash(d.Lines[line-1]))
}

// Render produces the anchored view of a line range: a header naming the path, revision, line count,
// line ending and BOM state, then one "N:hhh|text" row per line. The rows are what the model may
// anchor an edit to.
func Render(path string, d Doc, from, to int) string {
	if from < 1 {
		from = 1
	}
	if to <= 0 || to > len(d.Lines) {
		to = len(d.Lines)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "@@ %s rev=%s lines=%d eol=%s bom=%t\n", path, d.Rev, len(d.Lines), d.EOL, d.BOM)
	for line := from; line <= to && line <= len(d.Lines); line++ {
		fmt.Fprintf(&b, "%d:%s|%s\n", line, LineHash(d.Lines[line-1]), d.Lines[line-1])
	}
	return b.String()
}

// Splice replaces the bytes [Start, End) of the BaseRev content with Text. Text is normalized to the
// file's dominant terminator when it spans lines.
type Splice struct {
	Start int
	End   int
	Text  string
}

// LineOp is one Hashline operation addressed by 1-based, inclusive line numbers of the snapshot its
// revision names.
type LineOp struct {
	// Kind is replace, delete, insert_after or insert_before.
	Kind string
	// Start and End are the inclusive line range; an insert addresses one line.
	Start int
	End   int
	// Text is the replacement or inserted text, already split into lines.
	Text []string
}

// Line operation kinds.
const (
	OpReplace      = "replace"
	OpDelete       = "delete"
	OpInsertAfter  = "insert_after"
	OpInsertBefore = "insert_before"
)

// IsInsert reports whether the op inserts rather than touches existing lines.
func (op LineOp) IsInsert() bool {
	return op.Kind == OpInsertAfter || op.Kind == OpInsertBefore
}

// ApplyLineOps applies ops to the doc's lines and returns the new bytes. Ops may come in any order;
// they are applied from the highest line down, so each keeps the numbering of the snapshot it was
// resolved against. Ranges are not checked against each other here; ResolveOps does that.
func ApplyLineOps(d Doc, ops []LineOp) ([]byte, error) {
	segs := make([]segment, len(d.Lines))
	for i := range d.Lines {
		segs[i] = segment{text: d.Lines[i], eol: d.EOLs[i]}
	}
	sorted := append([]LineOp(nil), ops...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Start != sorted[j].Start {
			return sorted[i].Start > sorted[j].Start
		}
		return opRank(sorted[i].Kind) > opRank(sorted[j].Kind)
	})
	eol := d.textEOL()
	for _, op := range sorted {
		n := len(segs)
		switch op.Kind {
		case OpReplace, OpDelete:
			if op.Start < 1 || op.End < op.Start || op.End > n {
				return nil, fmt.Errorf("line op %s %d-%d is outside the file (%d lines)", op.Kind, op.Start, op.End, n)
			}
			s, e := op.Start-1, op.End
			// A range that reaches the end of a file without a final newline keeps that state.
			endsFile := e == n && segs[n-1].eol == ""
			var repl []segment
			if op.Kind == OpReplace {
				repl = segmentsOf(op.Text, eol, segs[e-1].eol)
			}
			out := make([]segment, 0, n-(e-s)+len(repl))
			out = append(out, segs[:s]...)
			out = append(out, repl...)
			out = append(out, segs[e:]...)
			if endsFile && len(out) > 0 {
				out[len(out)-1].eol = ""
			}
			segs = out
		case OpInsertAfter:
			if op.Start < 1 || op.Start > n {
				return nil, fmt.Errorf("line op %s %d is outside the file (%d lines)", op.Kind, op.Start, n)
			}
			at := op.Start
			repl := segmentsOf(op.Text, eol, eol)
			if at == n && len(repl) > 0 {
				// The file's final-newline state is preserved: the last inserted line becomes final.
				segs[n-1].eol = eol
				repl[len(repl)-1].eol = ""
			}
			segs = insertSegments(segs, at, repl)
		case OpInsertBefore:
			if op.Start < 1 || op.Start > n {
				return nil, fmt.Errorf("line op %s %d is outside the file (%d lines)", op.Kind, op.Start, n)
			}
			segs = insertSegments(segs, op.Start-1, segmentsOf(op.Text, eol, eol))
		default:
			return nil, fmt.Errorf("unknown line op %q", op.Kind)
		}
	}
	var b bytes.Buffer
	if d.BOM {
		b.WriteString(bomUTF8)
	}
	for _, s := range segs {
		b.WriteString(s.text)
		b.WriteString(s.eol)
	}
	return b.Bytes(), nil
}

type segment struct {
	text string
	eol  string
}

func segmentsOf(lines []string, eol, lastEOL string) []segment {
	out := make([]segment, 0, len(lines))
	for i, line := range lines {
		se := eol
		if i == len(lines)-1 {
			se = lastEOL
		}
		out = append(out, segment{text: line, eol: se})
	}
	return out
}

func insertSegments(segs []segment, at int, repl []segment) []segment {
	out := make([]segment, 0, len(segs)+len(repl))
	out = append(out, segs[:at]...)
	out = append(out, repl...)
	out = append(out, segs[at:]...)
	return out
}

// SplitText splits edit text into lines the way a line op stores it: CRLF is normalized to LF, one
// trailing newline is dropped, and empty text is no lines at all.
func SplitText(text string) []string {
	if text == "" {
		return nil
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	return strings.Split(text, "\n")
}

// ApplySplices applies byte-range splices to the doc's raw bytes and returns the new bytes. Text that
// spans lines takes the file's dominant terminator. Splices must not overlap.
func ApplySplices(d Doc, splices []Splice) ([]byte, error) {
	sorted := append([]Splice(nil), splices...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Start > sorted[j].Start })
	out := append([]byte(nil), d.Raw...)
	for _, s := range sorted {
		if s.Start < 0 || s.End < s.Start || s.End > len(d.Raw) {
			return nil, fmt.Errorf("splice %d-%d is outside the file (%d bytes)", s.Start, s.End, len(d.Raw))
		}
		text := NormalizeText(s.Text, d.EOL)
		next := make([]byte, 0, len(out)-(s.End-s.Start)+len(text))
		next = append(next, out[:s.Start]...)
		next = append(next, text...)
		next = append(next, out[s.End:]...)
		out = next
	}
	return out, nil
}

// NormalizeText turns the LF text an edit command carries into the file's dominant terminator.
func NormalizeText(text, dominant string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if dominant == "crlf" {
		return strings.ReplaceAll(text, "\n", "\r\n")
	}
	return text
}

// ValidateSplices refuses overlapping or unordered ranges before any bytes are produced.
func ValidateSplices(splices []Splice) error {
	sorted := append([]Splice(nil), splices...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Start < sorted[j].Start })
	for i, s := range sorted {
		if s.Start < 0 || s.End < s.Start {
			return fmt.Errorf("splice %d: range %d-%d", i+1, s.Start, s.End)
		}
		if i > 0 && s.Start < sorted[i-1].End {
			return fmt.Errorf("splice %d overlaps splice %d", i+1, i)
		}
	}
	return nil
}
