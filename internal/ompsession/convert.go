package ompsession

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/window"
	"github.com/edynasty/LumenCortex/protocol"
)

// MaxRowBytes bounds the content of one imported row; longer content is cut on a UTF-8 boundary
// and says how much was left out.
const MaxRowBytes = 256 << 10

// maxGoalRunes bounds a session goal derived from the source.
const maxGoalRunes = 200

// Stats counts what a conversion copied, changed and left out.
type Stats struct {
	// Rows is the number of LumenCortex transcript rows written.
	Rows              int `json:"rows"`
	UserMessages      int `json:"userMessages"`
	AssistantMessages int `json:"assistantMessages"`
	ToolCalls         int `json:"toolCalls"`
	ToolResults       int `json:"toolResults"`
	// Notices are visible OMP notices copied as imported_notice rows; Markers are compaction and
	// branch-return markers.
	Notices int `json:"notices"`
	Markers int `json:"markers"`
	// ThinkingBlocks are hidden-reasoning blocks left out; HiddenMessages are developer/system
	// reminders and notices OMP did not display; RetriedTurns are assistant turns OMP replaced by a
	// retry or that carried no visible text or call.
	ThinkingBlocks int `json:"thinkingBlocks"`
	HiddenMessages int `json:"hiddenMessages"`
	RetriedTurns   int `json:"retriedTurns"`
	// Images are image blocks replaced by a text placeholder.
	Images int `json:"images"`
	// Compactions and BranchReturns are the source events written as markers (their summaries are
	// not copied).
	Compactions   int `json:"compactions"`
	BranchReturns int `json:"branchReturns"`
	// InterruptedCalls got a tool_interrupted result because the source recorded none;
	// OrphanResults answered no open call and were left out; DuplicateCalls repeated an id in one
	// turn and were left out.
	InterruptedCalls int `json:"interruptedCalls"`
	OrphanResults    int `json:"orphanResults"`
	DuplicateCalls   int `json:"duplicateCalls"`
	TruncatedRows    int `json:"truncatedRows"`
}

// Conversion is a session's visible history as LumenCortex rows.
type Conversion struct {
	Messages []protocol.Message
	Stats    Stats
	Goal     string
	// FirstUser and LastAssistant are short excerpts for a preview.
	FirstUser     string
	LastAssistant string
	// Model is the provider/model of the last assistant turn, for display.
	Model string
	// OpenToolCalls is true when the branch ended with calls the source has no result for.
	OpenToolCalls bool
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

var toolCallIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// NormalizeToolCallID keeps an id providers accept (^[A-Za-z0-9_-]{1,64}$) and replaces any other
// with a stable one derived from it, so a call and its result still match.
func NormalizeToolCallID(id string) string {
	if toolCallIDPattern.MatchString(id) {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return "omp_" + hex.EncodeToString(sum[:20])
}

// Convert turns the active branch of s into a transcript that window.Groups accepts: user rows,
// assistant rows with their calls, and exactly one tool row per call directly after its turn. It
// ends with the OriginImported row. Nothing is executed: a call the source never answered gets the
// tool_interrupted row reconciliation writes.
func Convert(s *Session) (Conversion, error) {
	c := converter{}
	for i := range s.Entries {
		c.entry(&s.Entries[i])
	}
	c.out.OpenToolCalls = len(c.open) > len(c.answered)
	c.closeTurn()
	c.out.Messages = append(c.out.Messages, protocol.Message{
		Role: "user", Content: protocol.ImportedContent, Origin: protocol.OriginImported,
	})
	c.out.Stats.Rows = len(c.out.Messages)

	c.out.CreatedAt = parseTime(s.Header.Timestamp, s.ModTime)
	c.out.UpdatedAt = c.out.CreatedAt
	if n := len(s.Entries); n > 0 {
		c.out.UpdatedAt = parseTime(s.Entries[n-1].Timestamp, s.ModTime)
	}
	if c.out.UpdatedAt.Before(c.out.CreatedAt) {
		c.out.UpdatedAt = c.out.CreatedAt
	}
	switch {
	case s.Key != "":
		// A subagent's task opens with OMP's generic assignment preamble; the id its parent gave it
		// names it better.
		name := s.Key[strings.LastIndexByte(s.Key, '/')+1:]
		if s.Agent != "" {
			name += " (" + s.Agent + ")"
		}
		c.out.Goal = goalLine(name)
	case strings.TrimSpace(s.Header.Title) != "":
		c.out.Goal = goalLine(s.Header.Title)
	case c.out.FirstUser != "":
		c.out.Goal = goalLine(c.out.FirstUser)
	default:
		c.out.Goal = "Imported OMP session"
	}

	entries := make([]window.Entry, len(c.out.Messages))
	for i, m := range c.out.Messages {
		entries[i] = window.Entry{Seq: int64(i), Msg: m}
	}
	if _, err := window.Groups(entries); err != nil {
		return Conversion{}, fmt.Errorf("convert %s: %w", s.Path, err)
	}
	return c.out, nil
}

type converter struct {
	out Conversion
	// open are the calls of the current assistant turn without a result yet, in call order;
	// answered marks those answered. deferred holds notices that arrived inside the turn.
	open     []protocol.ToolCall
	answered map[string]bool
	deferred []protocol.Message
}

func (c *converter) entry(e *Entry) {
	switch e.Type {
	case "message":
		if e.Message != nil {
			c.message(e.Message)
		}
	case "custom_message":
		if e.Display == nil || !*e.Display {
			c.out.Stats.HiddenMessages++
			return
		}
		c.notice(e.CustomType, e.Content)
	case "compaction":
		c.out.Stats.Compactions++
		text := "[runtime] The source session compacted its context here"
		if e.TokensBefore > 0 {
			text += fmt.Sprintf(" (about %d tokens before)", e.TokensBefore)
		}
		text += ". Its summary is not imported: the source model wrote it from reasoning that is not part of the visible history. The rows above are the complete visible history."
		c.marker(text)
	case "branch_summary":
		c.out.Stats.BranchReturns++
		c.marker("[runtime] The source session returned here from another branch. That branch and its summary are not imported.")
	}
}

func (c *converter) message(m *Message) {
	switch m.Role {
	case "user":
		text := c.text(m.Content)
		if strings.TrimSpace(text) == "" {
			return
		}
		c.closeTurn()
		c.out.Stats.UserMessages++
		if c.out.FirstUser == "" {
			c.out.FirstUser = excerpt(text)
		}
		c.append(protocol.Message{Role: "user", Content: text})
	case "assistant":
		c.assistant(m)
	case "toolResult":
		c.result(m)
	case "custom", "hookMessage":
		if m.Display == nil || !*m.Display {
			c.out.Stats.HiddenMessages++
			return
		}
		c.notice(m.CustomType, m.Content)
	default:
		// developer/system rows are harness reminders the user did not see.
		c.out.Stats.HiddenMessages++
	}
}

func (c *converter) assistant(m *Message) {
	blocks := decodeBlocks(m.Content)
	var text []string
	var calls []protocol.ToolCall
	seen := map[string]bool{}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) != "" {
				text = append(text, b.Text)
			}
		case "toolCall":
			id := NormalizeToolCallID(b.ID)
			if b.ID == "" || seen[id] {
				c.out.Stats.DuplicateCalls++
				continue
			}
			seen[id] = true
			args := bytes.TrimSpace(b.Arguments)
			if len(args) == 0 || !json.Valid(args) {
				args = []byte("{}")
			}
			calls = append(calls, protocol.ToolCall{ID: id, Name: b.Name, Arguments: json.RawMessage(args)})
		case "thinking", "redactedThinking", "redacted_thinking":
			c.out.Stats.ThinkingBlocks++
		case "image":
			c.out.Stats.Images++
			text = append(text, imagePlaceholder(b.MimeType))
		}
	}
	content := strings.Join(text, "\n")
	if len(m.RetryRecovery) > 0 || (strings.TrimSpace(content) == "" && len(calls) == 0) {
		c.out.Stats.RetriedTurns++
		return
	}
	c.closeTurn()
	c.out.Stats.AssistantMessages++
	c.out.Stats.ToolCalls += len(calls)
	if strings.TrimSpace(content) != "" {
		c.out.LastAssistant = excerpt(content)
	}
	if m.Provider != "" && m.Model != "" {
		c.out.Model = m.Provider + "/" + m.Model
	}
	c.append(protocol.Message{Role: "assistant", Content: content, ToolCalls: calls})
	if len(calls) > 0 {
		c.open = calls
		c.answered = make(map[string]bool, len(calls))
	}
}

func (c *converter) result(m *Message) {
	id := NormalizeToolCallID(m.ToolCallID)
	name := ""
	for _, call := range c.open {
		if call.ID == id {
			name = call.Name
			break
		}
	}
	if name == "" || c.answered[id] {
		c.out.Stats.OrphanResults++
		return
	}
	c.answered[id] = true
	text := c.text(m.Content)
	if m.IsError {
		text = "[error]\n" + text
	}
	c.out.Stats.ToolResults++
	c.append(protocol.Message{Role: "tool", ToolCallID: id, Name: name, Content: text})
	if len(c.answered) == len(c.open) {
		c.finishTurn()
	}
}

func (c *converter) notice(kind string, raw json.RawMessage) {
	text := c.text(raw)
	if strings.TrimSpace(text) == "" {
		return
	}
	if kind == "" {
		kind = "notice"
	}
	c.out.Stats.Notices++
	c.queue(protocol.Message{Role: "user", Content: "[OMP " + kind + "]\n" + text, Origin: protocol.OriginImportedNotice})
}

func (c *converter) marker(text string) {
	c.out.Stats.Markers++
	c.queue(protocol.Message{Role: "user", Content: text, Origin: protocol.OriginImportedMarker})
}

// queue appends a runtime row, or holds it until the open calls are answered so it never splits an
// assistant turn from its results.
func (c *converter) queue(m protocol.Message) {
	if len(c.open) > 0 {
		c.deferred = append(c.deferred, m)
		return
	}
	c.append(m)
}

// closeTurn answers every open call with the interrupted result, then releases deferred rows.
func (c *converter) closeTurn() {
	if len(c.open) == 0 {
		return
	}
	for _, call := range c.open {
		if c.answered[call.ID] {
			continue
		}
		c.out.Stats.InterruptedCalls++
		c.append(protocol.Message{
			Role: "tool", ToolCallID: call.ID, Name: call.Name,
			Content: session.InterruptedToolResult, Origin: protocol.OriginToolInterrupted,
		})
	}
	c.finishTurn()
}

func (c *converter) finishTurn() {
	c.open, c.answered = nil, nil
	pending := c.deferred
	c.deferred = nil
	for _, m := range pending {
		c.append(m)
	}
}

func (c *converter) append(m protocol.Message) {
	if len(m.Content) > MaxRowBytes {
		m.Content = truncate(m.Content, MaxRowBytes)
		c.out.Stats.TruncatedRows++
	}
	c.out.Messages = append(c.out.Messages, m)
}

// text renders message content (a string or blocks) as text: text blocks joined by newlines, images
// as placeholders, hidden reasoning left out.
func (c *converter) text(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if raw[0] == '"' {
		var s string
		_ = json.Unmarshal(raw, &s)
		return s
	}
	var parts []string
	for _, b := range decodeBlocks(raw) {
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		case "image":
			c.out.Stats.Images++
			parts = append(parts, imagePlaceholder(b.MimeType))
		case "thinking", "redactedThinking", "redacted_thinking":
			c.out.Stats.ThinkingBlocks++
		}
	}
	return strings.Join(parts, "\n")
}

func decodeBlocks(raw json.RawMessage) []Block {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '[' {
		return nil
	}
	var blocks []Block
	if json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	return blocks
}

func imagePlaceholder(mime string) string {
	if mime == "" {
		mime = "image"
	}
	return "[" + mime + " attachment not imported: LumenCortex transcripts carry text only]"
}

func truncate(s string, limit int) string {
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("\n[… %d bytes not imported]", len(s)-cut)
}

func excerpt(s string) string {
	s = strings.TrimSpace(s)
	const max = 400
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max]) + "…"
}

func goalLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if utf8.RuneCountInString(s) > maxGoalRunes {
		s = string([]rune(s)[:maxGoalRunes]) + "…"
	}
	if s == "" {
		return "Imported OMP session"
	}
	return s
}

func parseTime(value string, fallback time.Time) time.Time {
	if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return t.UTC()
	}
	return fallback.UTC()
}
