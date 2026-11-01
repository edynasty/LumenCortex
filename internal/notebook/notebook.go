// Package notebook holds the Markdown shape of an agent's working notebook (ADR-0020): the split into
// "## " sections and the edits the notebook tools apply to a body. It holds no storage and no policy,
// so the store, the tools, the context index and the host export all read one implementation.
//
// The shape is deliberately small: a body is free text, and the first "## " headings that are not
// inside a fenced code block name its sections. Everything before the first heading (usually a "# "
// title line and an introduction) is kept verbatim as the preamble. A "## " heading that repeats an
// earlier one is kept in the document, but every lookup and every section write uses the first one.
// CRLF input is read as LF, and every write this package returns uses LF.
package notebook

import (
	"strings"
)

// Item is one checklist entry of a body: a "- [ ]" or "- [x]" line and the section it appears in
// ("" before the first "## " heading).
type Item struct {
	Section string `json:"section"`
	Done    bool   `json:"done"`
	Text    string `json:"text"`
}

// Section is one "## " section: its heading text and the raw text after the heading line up to the
// next "## " heading, so Parse and Render round-trip a body.
type Section struct {
	Title string
	Body  string
}

// Doc is a parsed body: the preamble and the sections, in document order.
type Doc struct {
	Preamble string
	Sections []Section
}

// heading is the byte range of one "## " heading line in the normalized text.
type heading struct {
	start int // offset of the line
	next  int // offset of the line after it (the section body's start)
	title string
}

// Parse splits body into its preamble and its "## " sections. Lines inside a fenced code block (three
// or more backticks or tildes, closed by the same character at the same or greater length) are not
// headings.
func Parse(body string) Doc {
	text := normalize(body)
	heads := headings(text)
	if len(heads) == 0 {
		return Doc{Preamble: text}
	}
	doc := Doc{Preamble: text[:heads[0].start]}
	doc.Sections = make([]Section, 0, len(heads))
	for i, h := range heads {
		end := len(text)
		if i+1 < len(heads) {
			end = heads[i+1].start
		}
		doc.Sections = append(doc.Sections, Section{Title: h.title, Body: text[h.next:end]})
	}
	return doc
}

// Render writes a parsed document back: the preamble, then every section heading and body as parsed.
func (d Doc) Render() string {
	var b strings.Builder
	b.WriteString(d.Preamble)
	for _, s := range d.Sections {
		b.WriteString("## ")
		b.WriteString(s.Title)
		b.WriteByte('\n')
		b.WriteString(s.Body)
	}
	return b.String()
}

// Headings returns the titles of the "## " sections in document order, duplicates included.
func Headings(body string) []string {
	heads := headings(normalize(body))
	out := make([]string, 0, len(heads))
	for _, h := range heads {
		out = append(out, h.title)
	}
	return out
}

// SectionText returns the text of the first section named title, without its heading line and with
// the blank lines around it trimmed. ok is false when the body has no such section.
func SectionText(body, title string) (string, bool) {
	text := normalize(body)
	title = strings.TrimSpace(title)
	heads := headings(text)
	for i, h := range heads {
		if h.title != title {
			continue
		}
		end := len(text)
		if i+1 < len(heads) {
			end = heads[i+1].start
		}
		return strings.Trim(text[h.next:end], "\n"), true
	}
	return "", false
}

// UpsertSection returns body with the first section named title replaced by content, or with a new
// section appended at the end when the body has none. content is trimmed of trailing newlines; an
// empty content leaves the heading with no text. A section that is not the last keeps one blank line
// before the next heading.
func UpsertSection(body, title, content string) string {
	title = strings.TrimSpace(title)
	content = strings.Trim(content, "\n")
	text := normalize(body)
	heads := headings(text)
	for i, h := range heads {
		if h.title != title {
			continue
		}
		end := len(text)
		last := i+1 == len(heads)
		if !last {
			end = heads[i+1].start
		}
		var raw string
		switch {
		case content == "" && last:
			raw = ""
		case content == "":
			raw = "\n"
		case last:
			raw = "\n" + content + "\n"
		default:
			raw = "\n" + content + "\n\n"
		}
		return text[:h.next] + raw + text[end:]
	}
	if content == "" {
		return appendBlock(text, "## "+title+"\n")
	}
	return appendBlock(text, "## "+title+"\n\n"+content+"\n")
}

// Append returns body with content added as one block at the end, separated from the text above it by
// one blank line. An empty content changes nothing.
func Append(body, content string) string {
	content = strings.TrimRight(content, "\n")
	if strings.TrimSpace(content) == "" {
		return normalize(body)
	}
	return appendBlock(normalize(body), content+"\n")
}

// Checklist returns the checkbox items of body in document order: a line "- [ ]" or "- [x]" (also
// with "*" or "+") with the title of the section it appears in. Lines inside a fenced code block are
// not items.
func Checklist(body string) []Item {
	text := normalize(body)
	out := []Item{}
	section := ""
	inFence, fenceChar, fenceLen := false, byte(0), 0
	for i := 0; i < len(text); {
		end := strings.IndexByte(text[i:], '\n')
		var line string
		next := len(text)
		if end < 0 {
			line = text[i:]
		} else {
			line, next = text[i:i+end], i+end+1
		}
		switch {
		case inFence:
			if closesFence(line, fenceChar, fenceLen) {
				inFence = false
			}
		default:
			if ch, n, ok := opensFence(line); ok {
				inFence, fenceChar, fenceLen = true, ch, n
			} else if title, ok := sectionTitle(line); ok {
				section = title
			} else if done, text, ok := checkbox(line); ok {
				out = append(out, Item{Section: section, Done: done, Text: text})
			}
		}
		i = next
	}
	return out
}

// appendBlock adds block to text, ensuring one blank line between them.
func appendBlock(text, block string) string {
	switch {
	case text == "":
		return block
	case strings.HasSuffix(text, "\n\n"):
		return text + block
	case strings.HasSuffix(text, "\n"):
		return text + "\n" + block
	default:
		return text + "\n\n" + block
	}
}

// normalize reads CRLF and lone CR as LF.
func normalize(body string) string {
	if !strings.ContainsRune(body, '\r') {
		return body
	}
	return strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\r", "\n")
}

// headings finds the "## " heading lines of text outside fenced code blocks.
func headings(text string) []heading {
	var out []heading
	inFence, fenceChar, fenceLen := false, byte(0), 0
	for i := 0; i < len(text); {
		end := strings.IndexByte(text[i:], '\n')
		var line string
		next := len(text)
		if end < 0 {
			line = text[i:]
		} else {
			line, next = text[i:i+end], i+end+1
		}
		switch {
		case inFence:
			if closesFence(line, fenceChar, fenceLen) {
				inFence = false
			}
		default:
			if ch, n, ok := opensFence(line); ok {
				inFence, fenceChar, fenceLen = true, ch, n
			} else if title, ok := sectionTitle(line); ok {
				out = append(out, heading{start: i, next: next, title: title})
			}
		}
		i = next
	}
	return out
}

// sectionTitle returns the title of a "## " heading line: exactly two hashes, a space and a non-empty
// title. A line of three or more hashes is a deeper heading and stays in the section body.
func sectionTitle(line string) (string, bool) {
	if !strings.HasPrefix(line, "## ") {
		return "", false
	}
	title := strings.TrimSpace(line[3:])
	if title == "" {
		return "", false
	}
	return title, true
}

// opensFence reports whether line opens a fenced code block: at most three leading spaces, then three
// or more backticks or tildes.
func opensFence(line string) (byte, int, bool) {
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) > 3 || len(trimmed) < 3 {
		return 0, 0, false
	}
	ch := trimmed[0]
	if ch != '`' && ch != '~' {
		return 0, 0, false
	}
	n := 0
	for n < len(trimmed) && trimmed[n] == ch {
		n++
	}
	if n < 3 {
		return 0, 0, false
	}
	return ch, n, true
}

// closesFence reports whether line closes a fence opened with ch repeated length times: the same
// character at the same or greater length, with nothing else on the line.
func closesFence(line string, ch byte, length int) bool {
	trimmed := strings.TrimSpace(line)
	n := 0
	for n < len(trimmed) && trimmed[n] == ch {
		n++
	}
	return n >= length && n == len(trimmed)
}

// checkbox parses a checklist line: "- [ ]", "- [x]" or "- [X]" with an optional leading indent of up
// to three spaces and any of the "-", "*" or "+" bullets.
func checkbox(line string) (done bool, text string, ok bool) {
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) > 3 || len(trimmed) < 6 {
		return false, "", false
	}
	if trimmed[0] != '-' && trimmed[0] != '*' && trimmed[0] != '+' {
		return false, "", false
	}
	if trimmed[1] != ' ' || trimmed[2] != '[' || trimmed[4] != ']' || trimmed[5] != ' ' {
		return false, "", false
	}
	switch trimmed[3] {
	case ' ':
		done = false
	case 'x', 'X':
		done = true
	default:
		return false, "", false
	}
	text = strings.TrimSpace(trimmed[6:])
	if text == "" {
		return false, "", false
	}
	return done, text, true
}
