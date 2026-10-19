package anthropic_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/runtime"
)

// thinkingSentinel marks the hidden reasoning the fake model returns; nothing at rest may hold it.
const thinkingSentinel = "SENTINEL-THINKING-4c1d"

// fakeAnthropic is a scripted Messages API: it answers one request per script entry, as SSE.
type fakeAnthropic struct {
	mu     sync.Mutex
	script []string // each entry is one full SSE response
	bodies []string
	heads  []http.Header
}

func (f *fakeAnthropic) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var body map[string]any
		raw, _ := io_ReadAll(r)
		_ = json.Unmarshal(raw, &body)
		f.bodies = append(f.bodies, string(raw))
		f.heads = append(f.heads, r.Header.Clone())
		if len(f.script) == 0 {
			t.Error("fake anthropic: request beyond the script")
			http.Error(w, "beyond script", http.StatusBadRequest)
			return
		}
		answer := f.script[0]
		f.script = f.script[1:]
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, answer)
	}
}

func io_ReadAll(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	buf := &strings.Builder{}
	if _, err := copyInto(buf, r); err != nil {
		return nil, err
	}
	return []byte(buf.String()), nil
}

func copyInto(buf *strings.Builder, r *http.Request) (int64, error) {
	p := make([]byte, 4096)
	var total int64
	for {
		n, err := r.Body.Read(p)
		if n > 0 {
			buf.Write(p[:n])
			total += int64(n)
		}
		if err != nil {
			return total, nil
		}
	}
}

// The full agent run through the real Engine and the default provider factory (the package
// dispatcher): a fake Messages server answers a tool_use turn, the engine runs the tool, and the
// next request carries the tool_result and the thinking echo; a later run (a new turn) carries
// neither. Nothing at rest holds the thinking text (AGENTS.md reasoning boundary).
func TestEngineRunThroughFakeAnthropic(t *testing.T) {
	server := &fakeAnthropic{script: []string{
		// Turn 1: thinking + one read_file tool call.
		sseResponse(
			`{"type":"message_start","message":{"usage":{"input_tokens":42}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"`+thinkingSentinel+`"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-1"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"reading it"}}`,
			`{"type":"content_block_stop","index":1}`,
			`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"read_file"}}`,
			`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"README.md\"}"}}`,
			`{"type":"content_block_stop","index":2}`,
			`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}`,
			`{"type":"message_stop"}`,
		),
		// Turn 2: the final answer.
		sseResponse(
			`{"type":"message_start","message":{"usage":{"input_tokens":80}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"the readme says hello"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`,
			`{"type":"message_stop"}`,
		),
		// A later run's answer (a new turn, no thinking echo).
		sseResponse(
			`{"type":"message_start","message":{"usage":{"input_tokens":30}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"again done"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
			`{"type":"message_stop"}`,
		),
	}}
	api := httptest.NewServer(server.handler(t))
	defer api.Close()

	// The configuration the Engine reads: one anthropic-messages provider whose baseURL is the
	// fake, one model with a configured output limit, the key in an environment variable.
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("FAKE_ANTHROPIC_KEY", "fake-key-value")
	if err := os.MkdirAll(filepath.Join(xdg, "lumencortex"), 0o755); err != nil {
		t.Fatal(err)
	}
	doc := fmt.Sprintf(`{
		"model": "fake/claude",
		"providers": {"fake": {"package": "anthropic-messages",
			"settings": {"baseURL": %q, "apiKey": "{env:FAKE_ANTHROPIC_KEY}"},
			"models": {"claude": {"modelID": "claude-fake", "limit": {"context": 32000, "output": 8192}}}}}
	}`, api.URL)
	if err := os.WriteFile(filepath.Join(xdg, "lumencortex", config.FileName), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}

	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	engine, err := runtime.Open(runtime.Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	events, unsubscribe := engine.Events(4096)
	handle, err := engine.NewSession(context.Background(), runtime.SessionOptions{Goal: "read the readme and tell me what it says"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := engine.RunAgent(context.Background(), handle.ID, runtime.AgentOptions{Effort: "high", MaxSteps: 5})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "completed" || result.Final != "the readme says hello" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	result2, err := engine.RunAgent(context.Background(), handle.ID, runtime.AgentOptions{Input: "again", MaxSteps: 5})
	if err != nil || result2.Status != "completed" || result2.Final != "again done" {
		t.Fatalf("second result=%+v err=%v", result2, err)
	}

	server.mu.Lock()
	bodies, heads := append([]string(nil), server.bodies...), append([]http.Header(nil), server.heads...)
	server.mu.Unlock()
	if len(bodies) != 3 {
		t.Fatalf("requests=%d", len(bodies))
	}
	// Every request authenticated the Messages way and carried the tool schema of the run.
	for i, head := range heads {
		if head.Get("x-api-key") != "fake-key-value" || head.Get("anthropic-version") == "" {
			t.Fatalf("request %d headers: %v", i, head)
		}
	}
	first := bodies[0]
	if !strings.Contains(first, `"system"`) || !strings.Contains(first, `"read_file"`) {
		t.Fatalf("first request: %s", first)
	}
	if !strings.Contains(first, `"thinking":{"type":"enabled","budget_tokens":7168}`) {
		t.Fatalf("first request thinking: %s", first)
	}
	// The second request carries the tool_result of the executed read_file and the thinking echo,
	// byte for byte, inside the same active tool flow.
	second := bodies[1]
	if !strings.Contains(second, `"tool_use_id":"toolu_1"`) || !strings.Contains(second, "hello") {
		t.Fatalf("second request tool result: %s", second)
	}
	if !strings.Contains(second, `"thinking":"`+thinkingSentinel+`"`) || !strings.Contains(second, `"signature":"sig-1"`) {
		t.Fatalf("second request thinking echo: %s", second)
	}
	// The later run is a new turn: no thinking block is sent.
	third := bodies[2]
	if strings.Contains(third, thinkingSentinel) || strings.Contains(third, `"signature"`) {
		t.Fatalf("a new turn replayed thinking: %s", third)
	}

	// Nothing published holds the sentinel.
	unsubscribe()
	store := engine.Health().Database
	var published []string
	for len(events) > 0 {
		raw, _ := json.Marshal(<-events)
		published = append(published, string(raw))
	}
	if strings.Contains(strings.Join(published, "\n"), thinkingSentinel) {
		t.Fatal("the thinking text reached the event stream")
	}
	// Nothing at rest holds it: the database, its WAL and shared memory.
	engine.Close()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		raw, err := os.ReadFile(store + suffix)
		if err != nil {
			continue
		}
		if strings.Contains(string(raw), thinkingSentinel) {
			t.Fatalf("the thinking text is in %s", filepath.Base(store+suffix))
		}
	}
}

func sseResponse(events ...string) string {
	var out strings.Builder
	for _, event := range events {
		out.WriteString("data: ")
		out.WriteString(event)
		out.WriteString("\n\n")
	}
	return out.String()
}
