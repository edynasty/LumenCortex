package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/protocol"
)

// T1-S2 against the real Engine and SQLite: the envelope rows describe the requests the provider
// received, an answer lands in the same transaction as its answered state, and no hidden reasoning
// reaches the database.

type envRow struct {
	ID, State, Purpose, RunID, Digest, Refs string
	Reason                                  sql.NullString
	Step, Attempt, Lineage                  int
	First, Last, Answered                   sql.NullInt64
}

func envelopeRows(t *testing.T, engine *Engine, sessionID string) []envRow {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+engine.store.Path()+"?mode=ro&_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT request_id, state, purpose, COALESCE(run_id, ''), COALESCE(tool_specs_digest, ''), refs_json, reason,
		step, attempt, lineage_complete, first_seq, last_seq, answered_seq
		FROM request_envelopes WHERE session_id = ? ORDER BY created_at, rowid`, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []envRow
	for rows.Next() {
		var r envRow
		if err := rows.Scan(&r.ID, &r.State, &r.Purpose, &r.RunID, &r.Digest, &r.Refs, &r.Reason, &r.Step, &r.Attempt, &r.Lineage, &r.First, &r.Last, &r.Answered); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// specsDigest recomputes the tool digest from the tools the provider received, independently of the
// loop's code: name, description and schema in the order sent.
func specsDigest(t *testing.T, tools []protocol.ToolSpec) string {
	t.Helper()
	type visible struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	}
	list := make([]visible, len(tools))
	for i, s := range tools {
		list[i] = visible{s.Name, s.Description, s.Parameters}
	}
	raw, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestRealEngineEnvelopesMatchTheRequestsSent(t *testing.T) {
	engine := openTestEngine(t)
	handle := newTestSession(t, engine, "write a file")
	var requests []protocol.ProviderRequest
	provider := &exitProvider{fn: func(_ context.Context, n int, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		requests = append(requests, req)
		if n == 1 {
			return protocol.ProviderResponse{Message: protocol.Message{ToolCalls: []protocol.ToolCall{
				{ID: "w1", Name: "write_file", Arguments: json.RawMessage(`{"path":"a.txt","content":"x"}`)},
			}}, FinishReason: "tool_calls"}, nil
		}
		return protocol.ProviderResponse{Message: protocol.Message{Content: "done"}}, nil
	}}
	if _, err := engine.runAgentWith(context.Background(), handle.ID, provider, AgentOptions{Policy: "workspace", MaxSteps: 4}); err != nil {
		t.Fatal(err)
	}
	envs := envelopeRows(t, engine, handle.ID)
	if len(envs) != len(requests) || len(requests) != 2 {
		t.Fatalf("envelopes=%d requests=%d, want 2 each", len(envs), len(requests))
	}
	stored := transcript(t, handle)
	for i, env := range envs {
		req := requests[i]
		if env.State != "answered" || env.Purpose != "main" || !env.Answered.Valid || env.Lineage != 1 {
			t.Fatalf("envelope %d = %+v", i, env)
		}
		if env.RunID == "" || env.RunID != envs[0].RunID || env.Step != i+1 || env.Attempt != 0 {
			t.Errorf("envelope %d identity = %+v", i, env)
		}
		if env.Digest != specsDigest(t, req.Tools) {
			t.Errorf("envelope %d: digest %s, tools sent hash to %s", i, env.Digest, specsDigest(t, req.Tools))
		}
		// The transcript rows of the request are exactly the stored rows [first, last].
		n := int(env.Last.Int64 - env.First.Int64 + 1)
		sent := req.Messages[len(req.Messages)-n:]
		for k, m := range sent {
			want := stored[int(env.First.Int64)+k]
			if m.Role != want.Role || m.Content != want.Content || m.ToolCallID != want.ToolCallID {
				t.Fatalf("envelope %d row %d: request carried %+v, database holds %+v", i, k, m, want)
			}
		}
		if before := req.Messages[len(req.Messages)-n-1]; before.Role != "user" || !strings.Contains(before.Content, "Objective") {
			t.Errorf("envelope %d: the message before the window is %q, want the Task State block", i, before.Role)
		}
		if stored[env.Answered.Int64].Role != "assistant" {
			t.Errorf("envelope %d answered_seq %d is a %s row", i, env.Answered.Int64, stored[env.Answered.Int64].Role)
		}
		if !strings.Contains(env.Refs, "ts:v") || !strings.Contains(env.Refs, "sys:prompt:") {
			t.Errorf("envelope %d refs = %s", i, env.Refs)
		}
	}
	if envs[1].Last.Int64 <= envs[0].Last.Int64 {
		t.Errorf("windows %d and %d: the second request must see the rows between", envs[0].Last.Int64, envs[1].Last.Int64)
	}
}

// Provider reasoning is run memory: neither the answer row nor an envelope nor any journal entry may
// hold it, in the database file or its WAL.
func TestHiddenReasoningNeverReachesTheDatabaseThroughEnvelopes(t *testing.T) {
	const sentinel = "HIDDEN-REASONING-SENTINEL-T1S2"
	engine := openTestEngine(t)
	handle := newTestSession(t, engine, "think")
	provider := &exitProvider{fn: func(_ context.Context, n int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		resp := protocol.ProviderResponse{Message: protocol.Message{Content: "answer"}, FinishReason: "stop"}
		resp.Echo = protocol.NewEcho("reasoning_content", []byte(sentinel), 8)
		return resp, nil
	}}
	if _, err := engine.runAgentWith(context.Background(), handle.ID, provider, AgentOptions{Policy: "workspace", MaxSteps: 2}); err != nil {
		t.Fatal(err)
	}
	if len(envelopeRows(t, engine, handle.ID)) != 1 {
		t.Fatal("no envelope was written")
	}
	for _, name := range []string{"lumencortex.db", "lumencortex.db-wal"} {
		raw, err := os.ReadFile(filepath.Join(filepath.Dir(engine.store.Path()), name))
		if err != nil {
			continue
		}
		if bytes.Contains(raw, []byte(sentinel)) {
			t.Errorf("%s holds the hidden reasoning sentinel", name)
		}
	}
}
