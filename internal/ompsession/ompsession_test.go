package ompsession

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/protocol"
)

// The fixture follows the record shapes of real OMP 18.12 session files: a fixed-width title slot,
// the v3 header, model/thinking changes, an abandoned branch, thinking blocks with signatures, a
// provider payload, tool calls whose ids providers reject, an error result, an image result, a
// visible async notice between a call and its results, hidden reminders, a compaction, a retried
// turn, a branch summary, a steering message, an unanswered call, session_exit, and a subagent
// directory with a nested child, a killed child, an advisor transcript, a lock and an artifact.
const (
	fixtureRoot = "testdata/sessions"
	fixtureFile = "testdata/sessions/-fixture-workspace/2026-10-01T08-00-00-000Z_01a10000-0000-7000-8000-000000000001.jsonl"
	fixtureID   = "01a10000-0000-7000-8000-000000000001"
)

// sentinels are text that only appears in hidden reasoning, provider state, summaries, hidden
// reminders, the subagent system prompt, the abandoned branch and the advisor transcript.
var sentinels = []string{
	"SECRET_THINKING_SENTINEL", "SECRET_SIGNATURE_SENTINEL", "SECRET_THINKING_SUMMARY", "SECRET_TEXTSIG_SENTINEL",
	"SECRET_PAYLOAD_SENTINEL", "SECRET_COMPACTION_SUMMARY_SENTINEL", "SECRET_SHORT_SUMMARY",
	"SECRET_BRANCH_SUMMARY_SENTINEL", "HIDDEN_REMINDER_SENTINEL", "HIDDEN_NUDGE_SENTINEL",
	"SYSTEM_PROMPT_SENTINEL", "ABANDONED_BRANCH_TEXT", "ADVISOR_TEXT",
}

func readFixture(t *testing.T) *Session {
	t.Helper()
	s, skipped, err := Read(fixtureFile, true)
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 0 {
		t.Fatalf("skipped=%d", skipped)
	}
	return s
}

func TestReadFollowsActiveBranchAndSubagentLayout(t *testing.T) {
	s := readFixture(t)
	if s.Header.ID != fixtureID || s.Header.Title != "Fix the parser bug" || s.Header.Cwd != "/fixture/workspace" {
		t.Fatalf("header %+v", s.Header)
	}
	if !s.Exited || s.PartialTail || s.Malformed != 0 {
		t.Fatalf("exited=%v partial=%v malformed=%d", s.Exited, s.PartialTail, s.Malformed)
	}
	// e0000005 is the abandoned assistant reply; e0000011 a usage marker off the branch.
	if s.OffBranch != 1 || s.Records != 20 {
		t.Fatalf("offBranch=%d records=%d", s.OffBranch, s.Records)
	}
	for _, e := range s.Entries {
		if e.ID == "e0000005" || e.ID == "e0000011" {
			t.Fatalf("off-branch entry %s on the active branch", e.ID)
		}
	}
	var keys []string
	var walk func(*Session)
	walk = func(p *Session) {
		for _, c := range p.Children {
			keys = append(keys, c.Key)
			walk(c)
		}
	}
	walk(s)
	if got := strings.Join(keys, ","); got != "Killed,Scout1,Scout1/Inner" {
		t.Fatalf("children %s", got)
	}
	if !s.Children[0].Aborted || s.Children[1].Aborted || s.Children[1].Agent != "scout" {
		t.Fatalf("killed=%v scout=%v agent=%q", s.Children[0].Aborted, s.Children[1].Aborted, s.Children[1].Agent)
	}
}

func TestConvertKeepsVisibleHistoryOnly(t *testing.T) {
	s := readFixture(t)
	conv, err := Convert(s)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(conv.Messages)
	for _, sentinel := range sentinels {
		if strings.Contains(string(raw), sentinel) {
			t.Errorf("converted transcript holds %s", sentinel)
		}
	}
	type row struct{ role, origin, head string }
	var got []row
	for _, m := range conv.Messages {
		head, _, _ := strings.Cut(m.Content, "\n")
		got = append(got, row{m.Role, m.Origin, head})
	}
	normalized := NormalizeToolCallID("call_abc|fc_0123456789abcdef0123456789abcdef0123456789abcdef0123")
	want := []row{
		{"user", "", "Fix the parser bug in src/parse.go."},
		{"assistant", "", "I will read the parser first."},
		// Results follow the turn in the source's order; the notice that arrived between them waits.
		{"tool", "", "[error]"},
		{"tool", "", "package parse"},
		{"user", protocol.OriginImportedNotice, "[OMP async-result]"},
		{"user", protocol.OriginImportedMarker, "[runtime] The source session compacted its context here (about 120000 tokens before). Its summary is not imported: the source model wrote it from reasoning that is not part of the visible history. The rows above are the complete visible history."},
		{"user", protocol.OriginImportedMarker, "[runtime] The source session returned here from another branch. That branch and its summary are not imported."},
		{"user", "", "Also update the docs."},
		{"assistant", "", "Fixed the off-by-one; tests pass."},
		{"tool", protocol.OriginToolInterrupted, "run ended before the tool result was saved; re-check workspace state before repeating side effects"},
	}
	if len(got) != len(want) {
		t.Fatalf("rows %d, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	calls := conv.Messages[1].ToolCalls
	if len(calls) != 2 || calls[0].ID != normalized || calls[1].ID != "toolu_01" {
		t.Fatalf("calls %+v", calls)
	}
	if conv.Messages[2].ToolCallID != "toolu_01" || conv.Messages[3].ToolCallID != normalized || conv.Messages[3].Name != "read" {
		t.Fatalf("results %+v %+v", conv.Messages[2], conv.Messages[3])
	}
	if !strings.Contains(conv.Messages[0].Content, "[image/png attachment not imported") ||
		!strings.Contains(conv.Messages[3].Content, "[image/jpeg attachment not imported") {
		t.Fatalf("image placeholders missing: %q / %q", conv.Messages[0].Content, conv.Messages[3].Content)
	}
	st := conv.Stats
	if st.ThinkingBlocks != 2 || st.RetriedTurns != 2 || st.HiddenMessages != 3 || st.Images != 2 ||
		st.InterruptedCalls != 1 || st.Compactions != 1 || st.BranchReturns != 1 || st.ToolCalls != 3 || st.ToolResults != 2 {
		t.Fatalf("stats %+v", st)
	}
	if !conv.OpenToolCalls || conv.Goal != "Fix the parser bug" || conv.Model != "prov/model-b" {
		t.Fatalf("open=%v goal=%q model=%q", conv.OpenToolCalls, conv.Goal, conv.Model)
	}
}

func TestConvertSubagentUsesItsAgentID(t *testing.T) {
	s := readFixture(t)
	inner := s.Children[1].Children[0]
	conv, err := Convert(inner)
	if err != nil {
		t.Fatal(err)
	}
	if conv.Goal != "Inner (scout)" || conv.LastAssistant != "tests cover only short input." {
		t.Fatalf("goal=%q last=%q", conv.Goal, conv.LastAssistant)
	}
	raw, _ := json.Marshal(conv.Messages)
	if strings.Contains(string(raw), "SYSTEM_PROMPT_SENTINEL") || strings.Contains(string(raw), "SECRET_THINKING_SENTINEL") {
		t.Fatal("child transcript holds the system prompt or reasoning")
	}
}

// A file OMP is still appending to ends in an unterminated record: it is not part of the snapshot,
// and the hash covers only the complete records.
func TestReadExcludesPartialTail(t *testing.T) {
	src, err := os.ReadFile(fixtureFile)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	complete := filepath.Join(dir, "complete.jsonl")
	partial := filepath.Join(dir, "partial.jsonl")
	if err := os.WriteFile(complete, src, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(partial, append(append([]byte(nil), src...), `{"type":"message","id":"e9","parentId":"e0000020","message":{"role":"user","con`...), 0o644); err != nil {
		t.Fatal(err)
	}
	a, _, err := Read(complete, false)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := Read(partial, false)
	if err != nil {
		t.Fatal(err)
	}
	if !b.PartialTail || a.PartialTail || a.SHA256 != b.SHA256 || a.Size != b.Size || len(a.Entries) != len(b.Entries) {
		t.Fatalf("partial=%v/%v sha equal=%v size %d/%d entries %d/%d", a.PartialTail, b.PartialTail, a.SHA256 == b.SHA256, a.Size, b.Size, len(a.Entries), len(b.Entries))
	}
}

// A malformed record in the middle is counted and skipped; a missing header is not a session.
func TestReadMalformedAndHeaderless(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.jsonl")
	content := `{"type":"session","version":3,"id":"s1","timestamp":"2026-10-01T00:00:00Z","cwd":"/w"}
{"type":"message","id":"a","parentId":null,"message":{"role":"user","content":"hi"}}
{not json
{"type":"message","id":"b","parentId":"a","message":{"role":"assistant","content":[{"type":"text","text":"hello"}]}}
`
	if err := os.WriteFile(bad, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	s, _, err := Read(bad, false)
	if err != nil {
		t.Fatal(err)
	}
	if s.Malformed != 1 || len(s.Entries) != 2 {
		t.Fatalf("malformed=%d entries=%d", s.Malformed, len(s.Entries))
	}
	none := filepath.Join(dir, "none.jsonl")
	if err := os.WriteFile(none, []byte(`{"type":"message","id":"a","parentId":null}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Read(none, false); !errors.Is(err, ErrNotSession) {
		t.Fatalf("err=%v", err)
	}
}

// OMP resolves the leaf from the last entry: its activeLeafId when present (null clears the
// branch), and a captured leaf that no longer exists falls back to the newest self-selecting entry.
func TestActivePathLeafResolution(t *testing.T) {
	idx := func(spec ...indexEntry) []indexEntry { return spec }
	e := func(id, parent string) indexEntry {
		return indexEntry{id: id, parent: parent, hasParent: parent != ""}
	}
	withLeaf := func(x indexEntry, leaf string) indexEntry {
		x.leafSet = true
		if leaf == "" {
			x.leafNull = true
		} else {
			x.leaf = leaf
		}
		return x
	}
	ids := func(index []indexEntry) string {
		var out []string
		for _, i := range activePath(index) {
			out = append(out, index[i].id)
		}
		return strings.Join(out, ",")
	}
	cases := []struct {
		name  string
		index []indexEntry
		want  string
	}{
		{"last entry", idx(e("a", ""), e("b", "a"), e("c", "a")), "a,c"},
		{"usage marker selects leaf", idx(e("a", ""), e("b", "a"), e("c", "a"), withLeaf(e("u", "c"), "b")), "a,b"},
		{"cleared", idx(e("a", ""), withLeaf(e("u", "a"), "")), ""},
		{"stale leaf falls back", idx(e("a", ""), e("b", "a"), withLeaf(e("u", "b"), "gone")), "a,b"},
		{"cycle stops", idx(indexEntry{id: "a", parent: "b", hasParent: true}, indexEntry{id: "b", parent: "a", hasParent: true}), "a,b"},
	}
	for _, c := range cases {
		if got := ids(c.index); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestNormalizeToolCallID(t *testing.T) {
	if got := NormalizeToolCallID("toolu_01-ok"); got != "toolu_01-ok" {
		t.Fatalf("valid id changed: %q", got)
	}
	a := NormalizeToolCallID("call_x|fc_y")
	if a != NormalizeToolCallID("call_x|fc_y") || a == NormalizeToolCallID("call_x|fc_z") || !toolCallIDPattern.MatchString(a) {
		t.Fatalf("normalized %q", a)
	}
}

func TestTruncateKeepsUTF8(t *testing.T) {
	s := strings.Repeat("界", 10) // 3 bytes each
	got := truncate(s, 7)
	if !strings.HasPrefix(got, "界界\n[… 24 bytes not imported]") {
		t.Fatalf("%q", got)
	}
}

func TestListAndResolvePath(t *testing.T) {
	list, err := List(fixtureRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != fixtureID || list[0].Children != 2 || list[0].Title != "Fix the parser bug" {
		t.Fatalf("list %+v", list)
	}
	if _, err := ResolvePath(fixtureRoot, fixtureFile); err != nil {
		t.Fatal(err)
	}
	child := strings.TrimSuffix(fixtureFile, ".jsonl") + "/Scout1.jsonl"
	if _, err := ResolvePath(fixtureRoot, child); !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("subagent transcript accepted: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "x.jsonl")
	if err := os.WriteFile(outside, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolvePath(fixtureRoot, outside); !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("outside file accepted: %v", err)
	}
	// A symlink inside a project directory that points outside the root is refused.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "p"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "p", "link.jsonl")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolvePath(root, link); !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("symlink escape accepted: %v", err)
	}
}

// A retried turn without calls is dropped; one with a call whose result was recorded stays, so the
// result keeps its call.
func TestConvertRetriedTurnWithRecordedCallStays(t *testing.T) {
	msg := func(m Message) Entry { return Entry{Type: "message", Message: &m} }
	s := &Session{Path: "x", Entries: []Entry{
		msg(Message{Role: "user", Content: json.RawMessage(`"go"`)}),
		msg(Message{Role: "assistant", Content: json.RawMessage(`[{"type":"text","text":"partial"}]`), RetryRecovery: json.RawMessage(`{}`)}),
		msg(Message{Role: "assistant", Content: json.RawMessage(`[{"type":"toolCall","id":"c1","name":"read","arguments":{}}]`), RetryRecovery: json.RawMessage(`{}`)}),
		msg(Message{Role: "toolResult", ToolCallID: "c1", Content: json.RawMessage(`[{"type":"text","text":"ran"}]`)}),
	}}
	conv, err := Convert(s)
	if err != nil {
		t.Fatal(err)
	}
	if conv.Stats.RetriedTurns != 1 || conv.Stats.OrphanResults != 0 || conv.Stats.ToolResults != 1 || len(conv.Messages) != 3 {
		t.Fatalf("stats %+v rows %d", conv.Stats, len(conv.Messages))
	}
}
