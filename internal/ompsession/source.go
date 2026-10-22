// Package ompsession reads OMP (oh-my-pi) session files and converts their visible history into
// LumenCortex transcript rows (ADR-0011).
//
// An OMP session is a JSONL file: a fixed-width title slot, a "session" header, then entries that
// form a tree through id/parentId. The file keeps every branch; the active branch is the path from
// the resolved leaf to the root. Subagent sessions live beside it in a directory named after the
// file's stem, nested the same way.
//
// The package only reads. Files are opened read-only, never locked, renamed or written, and blobs
// the session refers to are not read at all. Fields that carry hidden model reasoning or opaque
// provider state (thinking blocks, signatures, provider payloads) and summaries a model wrote from
// them (compaction and branch summaries) are not decoded into memory.
package ompsession

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Bounds of what an import reads.
const (
	// MaxLineBytes is the longest JSONL record read. A longer record fails the read.
	MaxLineBytes = 64 << 20
	// MaxDescendants bounds the subagent sessions imported with one session, as LumenCortex bounds
	// a subagent tree (runtime.MaxSubagentTreeNodes).
	MaxDescendants = 100
	// MaxDepth bounds subagent nesting below the imported session (runtime.MaxSubagentTreeDepth).
	MaxDepth = 4
)

// ErrNotSession reports a file that has no OMP session header.
var ErrNotSession = errors.New("not an OMP session file")

// Header is the session header and the title slot of a file.
type Header struct {
	ID            string `json:"id"`
	Version       int    `json:"version"`
	Timestamp     string `json:"timestamp"`
	Cwd           string `json:"cwd"`
	Title         string `json:"title"`
	ParentSession string `json:"parentSession"`
}

// Session is one OMP session file read for import: its header, the entries of its active branch in
// order, and what the read observed about the rest of the file.
type Session struct {
	Path   string
	Header Header
	// Key is the slash-joined subagent path relative to the imported session ("" for it).
	Key string
	// Entries is the active branch, root first.
	Entries []Entry
	// SHA256 and Size cover the complete records read (a trailing partial record is excluded).
	SHA256  string
	Size    int64
	ModTime time.Time
	// Records counts the entries in the file, OffBranch the message entries not on the active branch.
	Records   int
	OffBranch int
	// Malformed counts records that could not be decoded, PartialTail a trailing unterminated one.
	Malformed   int
	PartialTail bool
	// Exited is true when the file's last record is OMP's session_exit marker.
	Exited bool
	// Agent and Task come from the session_init entry of a subagent session.
	Agent string
	Task  string
	// Aborted is true when OMP left a tombstone beside a subagent transcript (killed).
	Aborted  bool
	Children []*Session
}

// Entry is one tree entry with the fields an import uses. Anything not listed here is skipped by
// the decoder, including thinking text, signatures, provider payloads and summaries.
type Entry struct {
	Type       string   `json:"type"`
	ID         string   `json:"id"`
	ParentID   *string  `json:"parentId"`
	Timestamp  string   `json:"timestamp"`
	Message    *Message `json:"message,omitempty"`
	CustomType string   `json:"customType,omitempty"`
	// Content and Display belong to custom_message entries.
	Content json.RawMessage `json:"content,omitempty"`
	Display *bool           `json:"display,omitempty"`
	// TokensBefore and Method belong to compaction entries; the summary is not decoded.
	TokensBefore int64  `json:"tokensBefore,omitempty"`
	Method       string `json:"method,omitempty"`
	// Model belongs to model_change entries.
	Model string `json:"model,omitempty"`
	// Agent and Task belong to session_init entries.
	Agent string `json:"agent,omitempty"`
	Task  string `json:"task,omitempty"`
}

// Message is the part of an OMP AgentMessage an import uses.
type Message struct {
	Role         string          `json:"role"`
	Content      json.RawMessage `json:"content"`
	ToolCallID   string          `json:"toolCallId,omitempty"`
	ToolName     string          `json:"toolName,omitempty"`
	IsError      bool            `json:"isError,omitempty"`
	Display      *bool           `json:"display,omitempty"`
	CustomType   string          `json:"customType,omitempty"`
	Provider     string          `json:"provider,omitempty"`
	Model        string          `json:"model,omitempty"`
	StopReason   string          `json:"stopReason,omitempty"`
	ErrorMessage string          `json:"errorMessage,omitempty"`
	// RetryRecovery is present on assistant turns OMP replaced by a retry.
	RetryRecovery json.RawMessage `json:"retryRecovery,omitempty"`
}

// Block is a content block. A thinking block decodes to its type alone.
type Block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	MimeType  string          `json:"mimeType,omitempty"`
}

// indexEntry locates one record of the file for the second pass.
type indexEntry struct {
	id, parent   string
	hasParent    bool
	typ          string
	leaf         string // activeLeafId
	leafSet      bool   // activeLeafId present
	leafNull     bool   // activeLeafId is null
	offset, size int64
}

type indexProbe struct {
	Type         string          `json:"type"`
	ID           string          `json:"id"`
	ParentID     *string         `json:"parentId"`
	ActiveLeafID json.RawMessage `json:"activeLeafId"`
	CustomType   string          `json:"customType"`
	Title        string          `json:"title"`
}

// ReadHeader reads only the title slot and header of an OMP session file.
func ReadHeader(path string) (Header, error) {
	f, err := os.Open(path)
	if err != nil {
		return Header{}, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64<<10)
	var header Header
	title := ""
	for range 3 {
		line, err := readLine(r, 1<<20)
		if len(line) == 0 && err != nil {
			break
		}
		var probe struct {
			Type string `json:"type"`
			Header
		}
		if json.Unmarshal(line, &probe) != nil {
			continue
		}
		switch probe.Type {
		case "title":
			title = probe.Title
		case "session":
			header = probe.Header
			if title != "" {
				header.Title = title
			}
			if header.ID == "" {
				return Header{}, ErrNotSession
			}
			return header, nil
		}
		if err != nil {
			break
		}
	}
	return Header{}, ErrNotSession
}

// Read reads one session file and, when children is true, its subagent sessions, bounded by
// MaxDescendants and MaxDepth. skipped reports subagent transcripts the bounds left out.
func Read(path string, children bool) (s *Session, skipped int, err error) {
	s, err = readFile(path)
	if err != nil {
		return nil, 0, err
	}
	if children {
		budget := MaxDescendants
		skipped, err = readChildren(s, strings.TrimSuffix(path, ".jsonl"), "", 1, &budget)
		if err != nil {
			return nil, 0, err
		}
	}
	return s, skipped, nil
}

// readChildren reads the subagent transcripts in dir as children of parent, the way OMP lists
// them: *.jsonl files sorted by name, without backups and advisor transcripts, descending only into
// real directories named after a transcript.
func readChildren(parent *Session, dir, parentKey string, depth int, budget *int) (int, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	files := map[string]bool{}
	dirs := map[string]bool{}
	var names []string
	for _, e := range entries {
		switch {
		case e.Type().IsRegular():
			files[e.Name()] = true
			names = append(names, e.Name())
		case e.IsDir():
			dirs[e.Name()] = true
		}
	}
	sort.Strings(names)
	skipped := 0
	for _, name := range names {
		if !isChildTranscript(name) {
			continue
		}
		agentID := strings.TrimSuffix(name, ".jsonl")
		key := agentID
		if parentKey != "" {
			key = parentKey + "/" + agentID
		}
		if depth > MaxDepth || *budget <= 0 {
			skipped += countTranscripts(filepath.Join(dir, name))
			continue
		}
		child, err := readFile(filepath.Join(dir, name))
		if errors.Is(err, ErrNotSession) {
			skipped++
			continue
		}
		if err != nil {
			return skipped, err
		}
		*budget--
		child.Key = key
		child.Aborted = files[name+".tombstone"]
		parent.Children = append(parent.Children, child)
		if dirs[agentID] {
			n, err := readChildren(child, filepath.Join(dir, agentID), key, depth+1, budget)
			skipped += n
			if err != nil {
				return skipped, err
			}
		}
	}
	return skipped, nil
}

// countTranscripts counts a transcript the bounds leave out and every transcript nested below it.
func countTranscripts(file string) int {
	n := 1
	dir := strings.TrimSuffix(file, ".jsonl")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return n
	}
	for _, e := range entries {
		if e.Type().IsRegular() && isChildTranscript(e.Name()) {
			n += countTranscripts(filepath.Join(dir, e.Name()))
		}
	}
	return n
}

func isChildTranscript(name string) bool {
	return strings.HasSuffix(name, ".jsonl") && !strings.HasPrefix(name, ".") &&
		!strings.Contains(name, ".bak") && !strings.HasPrefix(name, "__advisor")
}

// readFile indexes the file in one pass (hashing every complete record), resolves the active
// branch, then decodes only the branch's records from the same descriptor, so the result is one
// consistent snapshot even while OMP appends.
func readFile(path string) (*Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: not a regular file", path)
	}
	s := &Session{Path: path, ModTime: info.ModTime()}
	hash := sha256.New()
	r := bufio.NewReaderSize(f, 256<<10)
	var index []indexEntry
	var offset int64
	haveHeader := false
	title := ""
	lastCustom := ""
	for {
		line, err := r.ReadSlice('\n')
		full := line
		if errors.Is(err, bufio.ErrBufferFull) {
			buf := append([]byte(nil), line...)
			for errors.Is(err, bufio.ErrBufferFull) {
				line, err = r.ReadSlice('\n')
				buf = append(buf, line...)
				if len(buf) > MaxLineBytes {
					return nil, fmt.Errorf("%s: record at byte %d exceeds %d bytes", path, offset, MaxLineBytes)
				}
			}
			full = buf
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		size := int64(len(full))
		if errors.Is(err, io.EOF) && size > 0 && full[size-1] != '\n' {
			// A record still being written: not part of this snapshot.
			if hasPayload(full) {
				s.PartialTail = true
			}
			break
		}
		if size == 0 {
			break
		}
		hash.Write(full)
		start := offset
		offset += size
		body := bytes.TrimSpace(full)
		if len(body) == 0 {
			continue
		}
		var probe indexProbe
		if json.Unmarshal(body, &probe) != nil {
			s.Malformed++
			continue
		}
		switch probe.Type {
		case "title":
			title = probe.Title
			continue
		case "session":
			if !haveHeader {
				if err := json.Unmarshal(body, &s.Header); err != nil || s.Header.ID == "" {
					return nil, fmt.Errorf("%s: %w", path, ErrNotSession)
				}
				haveHeader = true
			}
			continue
		}
		if !haveHeader {
			return nil, fmt.Errorf("%s: %w", path, ErrNotSession)
		}
		if probe.ID == "" {
			s.Malformed++
			continue
		}
		e := indexEntry{id: probe.ID, typ: probe.Type, offset: start, size: size}
		if probe.ParentID != nil {
			e.parent, e.hasParent = *probe.ParentID, true
		}
		if probe.ActiveLeafID != nil {
			e.leafSet = true
			if string(probe.ActiveLeafID) == "null" {
				e.leafNull = true
			} else if json.Unmarshal(probe.ActiveLeafID, &e.leaf) != nil {
				e.leafSet = false
			}
		}
		lastCustom = ""
		if probe.Type == "custom" {
			lastCustom = probe.CustomType
		}
		index = append(index, e)
		if errors.Is(err, io.EOF) {
			break
		}
	}
	if !haveHeader {
		return nil, fmt.Errorf("%s: %w", path, ErrNotSession)
	}
	if title != "" {
		s.Header.Title = title
	}
	s.SHA256 = hex.EncodeToString(hash.Sum(nil))
	s.Size = offset
	s.Records = len(index)
	s.Exited = lastCustom == "session_exit"

	path2 := activePath(index)
	onPath := make(map[int]bool, len(path2))
	for _, i := range path2 {
		onPath[i] = true
	}
	for i, e := range index {
		if !onPath[i] && (e.typ == "message" || e.typ == "custom_message") {
			s.OffBranch++
		}
	}
	s.Entries = make([]Entry, 0, len(path2))
	for _, i := range path2 {
		e := index[i]
		buf := make([]byte, e.size)
		if _, err := f.ReadAt(buf, e.offset); err != nil {
			return nil, err
		}
		var entry Entry
		if err := json.Unmarshal(buf, &entry); err != nil {
			s.Malformed++
			continue
		}
		if entry.Type == "session_init" {
			s.Agent, s.Task = entry.Agent, entry.Task
		}
		s.Entries = append(s.Entries, entry)
	}
	return s, nil
}

// activePath returns the indexes of the active branch, root first, as OMP resolves it: the last
// entry selects the leaf (its activeLeafId when it carries one, else itself); a captured leaf that no
// longer exists falls back to the newest entry that selects itself. Parent cycles stop the walk.
func activePath(index []indexEntry) []int {
	if len(index) == 0 {
		return nil
	}
	byID := make(map[string]int, len(index))
	for i, e := range index {
		byID[e.id] = i
	}
	last := index[len(index)-1]
	leaf := last.id
	if last.leafSet {
		if last.leafNull {
			return nil
		}
		leaf = last.leaf
	}
	if _, ok := byID[leaf]; !ok {
		leaf = ""
		for i := len(index) - 1; i >= 0; i-- {
			e := index[i]
			if !e.leafSet || (!e.leafNull && e.leaf == e.id) {
				leaf = e.id
				break
			}
		}
		if leaf == "" {
			return nil
		}
	}
	var out []int
	seen := map[string]bool{}
	for cur, ok := byID[leaf]; ok && !seen[index[cur].id]; {
		seen[index[cur].id] = true
		out = append(out, cur)
		if !index[cur].hasParent {
			break
		}
		cur, ok = byID[index[cur].parent]
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func hasPayload(b []byte) bool {
	return len(bytes.TrimSpace(b)) > 0
}

// readLine reads one line of at most limit bytes (longer lines are cut), without the newline.
func readLine(r *bufio.Reader, limit int) ([]byte, error) {
	var out []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(out)+len(chunk) <= limit {
			out = append(out, chunk...)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return bytes.TrimRight(out, "\r\n"), err
	}
}
