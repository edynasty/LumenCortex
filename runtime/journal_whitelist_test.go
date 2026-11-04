package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/protocol"
)

// The sentinels of the body-leak test. Each names where its text enters the Engine, so a hit says
// which copy leaked: a tool's arguments, a tool's output, a session goal, a final answer, a shell
// command run through SessionHandle.RunShell, and a child's goal, which the parent checkpoint used
// to repeat.
const (
	journalSentinelArgs   = "SENTINEL-ARGS-3f7a1c"
	journalSentinelOutput = "SENTINEL-OUTPUT-91b2de"
	journalSentinelGoal   = "SENTINEL-GOAL-4c8e02"
	journalSentinelFinal  = "SENTINEL-FINAL-a05d77"
	journalSentinelShell  = "SENTINEL-SHELL-2d6b41"
	journalSentinelChild  = "SENTINEL-CHILD-GOAL-8e50f3"
)

// journalSentinels maps each sentinel to the path it entered through.
func journalSentinels() map[string]string {
	return map[string]string{
		journalSentinelArgs:   "tool arguments",
		journalSentinelOutput: "tool output",
		journalSentinelGoal:   "session goal",
		journalSentinelFinal:  "final answer",
		journalSentinelShell:  "shell command",
		journalSentinelChild:  "child goal",
	}
}

// journalScriptProvider answers with its scripted responses in order.
type journalScriptProvider struct {
	mu        sync.Mutex
	responses []protocol.ProviderResponse
}

func (p *journalScriptProvider) Model() string { return "journal-script" }

func (p *journalScriptProvider) Complete(context.Context, protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.responses) == 0 {
		return protocol.ProviderResponse{}, fmt.Errorf("journalScriptProvider: the script is exhausted")
	}
	response := p.responses[0]
	p.responses = p.responses[1:]
	return response, nil
}

// A journal row and a parent's child-session checkpoint record what happened, never what was said:
// after a run that puts a sentinel in tool arguments, tool output, a goal, a final answer, a shell
// command and a child's goal, no sentinel reaches either store, by SQL or by raw bytes. The
// in-memory event stream still carries the tool output, which is the Desktop's copy.
func TestJournalAndParentCheckpointHoldNoBodySentinels(t *testing.T) {
	useGlobalConfig(t, catalogWithAgents(delegationAgents))
	workspace := t.TempDir()
	engine := openEngineAt(t, workspace)
	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()
	bindRef(engine, "b63", &recordingProvider{final: "child report " + journalSentinelFinal})

	events, unsubscribe := engine.Events(1024)
	defer unsubscribe()

	parent := &journalScriptProvider{responses: []protocol.ProviderResponse{
		{Message: protocol.Message{ToolCalls: []protocol.ToolCall{
			{ID: "call-write", Name: "write_file", Arguments: json.RawMessage(`{"path":"note.txt","content":"` + journalSentinelArgs + `"}`)},
			{ID: "call-shell", Name: "bash", Arguments: json.RawMessage(`{"command":"printf '%s\n' ` + journalSentinelOutput + `"}`)},
		}}, FinishReason: "tool_calls"},
		{Message: protocol.Message{Role: "assistant", Content: "parent final " + journalSentinelFinal}, FinishReason: "stop"},
	}}
	handle, err := engine.NewSession(context.Background(), SessionOptions{Goal: "parent goal " + journalSentinelGoal})
	if err != nil {
		t.Fatal(err)
	}
	result, err := engine.runAgentWith(context.Background(), handle.ID, parent, AgentOptions{Policy: "full", MaxSteps: 4})
	if err != nil || result.Status != "completed" {
		t.Fatalf("run=%+v err=%v", result, err)
	}
	if _, err := handle.RunShell(context.Background(), "printf '%s\n' "+journalSentinelShell); err != nil {
		t.Fatal(err)
	}

	controller := &subagentController{supervisor: supervisor, parentID: handle.ID}
	spawned, err := controller.Spawn(context.Background(), "child goal "+journalSentinelChild, "audit")
	if err != nil {
		t.Fatal(err)
	}
	childID := spawned.(SubagentNode).SessionID
	waitUntilIdle(t, supervisor, childID, 10*time.Second)

	// The journal is scanned by SQL: every row, whatever its event. Every leak is reported, not
	// just the first, so the failure names each copy a projection mistake would leave behind.
	rows := readJournal(t, engine.store.Path())
	if len(rows) == 0 {
		t.Fatal("the journal is empty; the scan would prove nothing")
	}
	var leaks []string
	for _, row := range rows {
		for sentinel, source := range journalSentinels() {
			if strings.Contains(row.Payload, sentinel) {
				leaks = append(leaks, fmt.Sprintf("journal row %q carries the sentinel of the %s: %s", row.Event, source, row.Payload))
			}
		}
	}
	if len(leaks) > 0 {
		t.Fatalf("%d body leak(s):\n%s", len(leaks), strings.Join(leaks, "\n"))
	}

	// The RunShell row names the session and the outcome, and no command.
	shellRows := 0
	for _, row := range rows {
		if row.Event != "tool.shell" {
			continue
		}
		shellRows++
		var payload map[string]any
		if err := json.Unmarshal([]byte(row.Payload), &payload); err != nil {
			t.Fatalf("tool.shell payload %s: %v", row.Payload, err)
		}
		if payload["sessionId"] != handle.ID {
			t.Fatalf("tool.shell payload names %v, want the session %s", payload["sessionId"], handle.ID)
		}
		if _, ok := payload["command"]; ok {
			t.Fatalf("tool.shell payload carries a command: %s", row.Payload)
		}
		if payload["exitCode"] != float64(0) {
			t.Fatalf("tool.shell payload lost the exit code: %s", row.Payload)
		}
	}
	if shellRows != 1 {
		t.Fatalf("tool.shell rows=%d, want the one RunShell wrote", shellRows)
	}

	// The parent's checkpoints are scanned by SQL too, and the child checkpoints keep three fields.
	checkpoints, err := engine.SessionCheckpoints(context.Background(), handle.ID, 200)
	if err != nil {
		t.Fatal(err)
	}
	reasons := map[string]bool{}
	for _, checkpoint := range checkpoints {
		for sentinel, source := range journalSentinels() {
			if strings.Contains(string(checkpoint.JSON), sentinel) {
				leaks = append(leaks, fmt.Sprintf("checkpoint %q carries the sentinel of the %s: %s", checkpoint.Reason, source, checkpoint.JSON))
			}
		}
		if checkpoint.Reason != "subagent.spawn" && checkpoint.Reason != "subagent.stopped" {
			continue
		}
		reasons[checkpoint.Reason] = true
		var payload map[string]any
		if err := json.Unmarshal(checkpoint.JSON, &payload); err != nil {
			t.Fatalf("checkpoint %s: %v", checkpoint.JSON, err)
		}
		for key := range payload {
			switch key {
			case "childSessionId", "status", "agent":
			default:
				leaks = append(leaks, fmt.Sprintf("checkpoint %q carries the field %q: %s", checkpoint.Reason, key, checkpoint.JSON))
			}
		}
		if payload["childSessionId"] != childID {
			t.Fatalf("checkpoint %q names %v, want %s", checkpoint.Reason, payload["childSessionId"], childID)
		}
		if checkpoint.Reason == "subagent.stopped" && (payload["status"] != "completed" || payload["agent"] != "audit") {
			t.Fatalf("stopped checkpoint %s", checkpoint.JSON)
		}
	}
	if len(leaks) > 0 {
		t.Fatalf("%d body leak(s):\n%s", len(leaks), strings.Join(leaks, "\n"))
	}
	if !reasons["subagent.spawn"] || !reasons["subagent.stopped"] {
		t.Fatalf("checkpoints=%#v", checkpoints)
	}
	t.Logf("scanned %d journal rows and %d checkpoints of the parent", len(rows), len(checkpoints))

	// The event stream is untouched: the Desktop still receives the tool output and the child's
	// final text, which the stores above no longer hold.
	var sawToolOutput, sawSubagentStop bool
	for drained := false; !drained; {
		select {
		case event := <-events:
			data := string(event.Data)
			if event.Type == "tool.output" && strings.Contains(data, journalSentinelOutput) {
				sawToolOutput = true
			}
			if event.Type == "subagent.stopped" && strings.Contains(data, journalSentinelFinal) {
				sawSubagentStop = true
			}
		default:
			drained = true
		}
	}
	if !sawToolOutput {
		t.Fatal("the event stream lost tool.output")
	}
	if !sawSubagentStop {
		t.Fatal("the event stream lost the child's final text on subagent.stopped")
	}

	// Raw bytes: the state database and its WAL must hold the sentinels the transcript keeps —
	// otherwise the byte scan below would prove nothing — and must not hold them once every table
	// but the journal and the checkpoints is gone.
	stateBytes := readStateBytes(t, engine.store.Path())
	if !strings.Contains(stateBytes, journalSentinelOutput) || !strings.Contains(stateBytes, journalSentinelArgs) {
		t.Fatalf("the state files do not hold the transcript's sentinels; the byte scan proves nothing")
	}
	scrubbed := scrubStateForJournalScan(t, engine.store.Path())
	if scrubbedRows := readJournal(t, scrubbed); len(scrubbedRows) != len(rows) {
		t.Fatalf("the scrubbed copy kept %d journal rows, want %d", len(scrubbedRows), len(rows))
	}
	for _, file := range stateFiles(scrubbed) {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for sentinel, source := range journalSentinels() {
			if strings.Contains(string(raw), sentinel) {
				t.Fatalf("%s holds the sentinel of the %s after the transcript was removed", filepath.Base(file), source)
			}
		}
	}
}

type journalRow struct {
	Seq     int64
	Event   string
	Payload string
}

// readJournal reads every journal row of the database at path.
func readJournal(t *testing.T, path string) []journalRow {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+filepath.ToSlash(path)+"?mode=ro&_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT seq, event, payload_json FROM journal ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []journalRow
	for rows.Next() {
		var row journalRow
		if err := rows.Scan(&row.Seq, &row.Event, &row.Payload); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// stateFiles lists the database and the WAL beside it, as the byte scan sees them.
func stateFiles(path string) []string {
	out := []string{path}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); err == nil {
			out = append(out, path+suffix)
		}
	}
	return out
}

// readStateBytes reads the database and its WAL as raw bytes.
func readStateBytes(t *testing.T, path string) string {
	t.Helper()
	var out strings.Builder
	for _, file := range stateFiles(path) {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		out.Write(raw)
	}
	return out.String()
}

// scrubStateForJournalScan copies the state into a temporary directory and removes every row of
// every table but journal and session_checkpoints, then vacuums the copy. After the vacuum the file
// holds no page of a removed row, so a sentinel found in it could only come from the journal or a
// checkpoint. The copy is what makes a byte scan able to say which store leaked.
func scrubStateForJournalScan(t *testing.T, path string) string {
	t.Helper()
	dir := t.TempDir()
	copyPath := filepath.Join(dir, filepath.Base(path))
	for _, file := range stateFiles(path) {
		source, err := os.Open(file)
		if err != nil {
			t.Fatal(err)
		}
		target, err := os.Create(filepath.Join(dir, filepath.Base(file)))
		if err != nil {
			source.Close()
			t.Fatal(err)
		}
		if _, err := target.ReadFrom(source); err != nil {
			source.Close()
			target.Close()
			t.Fatal(err)
		}
		source.Close()
		if err := target.Close(); err != nil {
			t.Fatal(err)
		}
	}

	db, err := sql.Open("sqlite3", "file:"+filepath.ToSlash(copyPath)+"?_journal_mode=DELETE&_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode = DELETE`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "delete" {
		t.Fatalf("the copy's journal mode is %q, want delete", mode)
	}
	tables, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for tables.Next() {
		var name string
		if err := tables.Scan(&name); err != nil {
			t.Fatal(err)
		}
		if name != "journal" && name != "session_checkpoints" {
			names = append(names, name)
		}
	}
	if err := tables.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if _, err := db.Exec(`DELETE FROM "` + name + `"`); err != nil {
			t.Fatalf("delete %s: %v", name, err)
		}
	}
	if _, err := db.Exec(`VACUUM`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// The vacuum rewrote the database; a WAL left behind would hold pre-vacuum pages.
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(copyPath + suffix); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	return copyPath
}

// TestJournalShellHeavyJournalSize measures what the journal keeps of a shell-heavy run: the row
// count and the payload bytes, per event, for a short and for a long tool output. It asserts only
// that the journal is written; the numbers belong to the slice report, and the same test on the
// base commit produces the before column.
func TestJournalShellHeavyJournalSize(t *testing.T) {
	for _, fixture := range []struct {
		name    string
		payload int
	}{{"short-output", 120}, {"long-output", 2048}} {
		t.Run(fixture.name, func(t *testing.T) {
			// The package catalog (test/b0 ... test/b63) is enough: runAgentWith binds the
			// scripted provider to one of its models.
			engine := openTestEngine(t)
			const calls = 12
			responses := make([]protocol.ProviderResponse, 0, calls+1)
			for i := range calls {
				command := `{"command":"printf '%s\n' ` + strings.Repeat("x", fixture.payload) + `"}`
				responses = append(responses, protocol.ProviderResponse{
					Message:      protocol.Message{ToolCalls: []protocol.ToolCall{{ID: fmt.Sprintf("call-%d", i), Name: "bash", Arguments: json.RawMessage(command)}}},
					FinishReason: "tool_calls",
				})
			}
			responses = append(responses, protocol.ProviderResponse{Message: protocol.Message{Content: "done"}, FinishReason: "stop"})
			handle := newTestSession(t, engine, "shell heavy")
			result, err := engine.runAgentWith(context.Background(), handle.ID, &journalScriptProvider{responses: responses}, AgentOptions{
				Policy: "full", MaxSteps: calls + 2, MaxToolCallsPerStep: 1,
			})
			if err != nil || result.Status != "completed" {
				t.Fatalf("run=%+v err=%v", result, err)
			}

			db, err := sql.Open("sqlite3", "file:"+filepath.ToSlash(engine.store.Path())+"?mode=ro&_busy_timeout=5000")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var rows, payloadBytes int64
			if err := db.QueryRow(`SELECT count(*), coalesce(sum(length(payload_json)), 0) FROM journal`).Scan(&rows, &payloadBytes); err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: journal rows=%d payload bytes=%d", fixture.name, rows, payloadBytes)
			breakdown, err := db.Query(`SELECT event, count(*), coalesce(sum(length(payload_json)), 0) FROM journal GROUP BY event ORDER BY event`)
			if err != nil {
				t.Fatal(err)
			}
			defer breakdown.Close()
			for breakdown.Next() {
				var event string
				var count, bytes int64
				if err := breakdown.Scan(&event, &count, &bytes); err != nil {
					t.Fatal(err)
				}
				t.Logf("  %-24s rows=%3d bytes=%6d", event, count, bytes)
			}
			if err := breakdown.Err(); err != nil {
				t.Fatal(err)
			}
			if rows == 0 {
				t.Fatal("the journal is empty")
			}
		})
	}
}
