package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/protocol"
)

// recordingProvider keeps every request, so a test can assert the exact system prefix and tools.
type recordingProvider struct {
	requests []protocol.ProviderRequest
	final    string
}

func (p *recordingProvider) Model() string { return "recording" }

func (p *recordingProvider) Complete(_ context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.requests = append(p.requests, req)
	return protocol.ProviderResponse{
		Message:      protocol.Message{Role: "assistant", Content: p.final},
		FinishReason: "stop",
	}, nil
}

func systemContents(messages []protocol.Message) []string {
	var out []string
	for _, m := range messages {
		if m.Role == "system" {
			out = append(out, m.Content)
		}
	}
	return out
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// The instructions reach the request as the last system message, after the default prompt. They are
// options of this Start only: the next Start without them carries the default prompt alone.
func TestInstructionsAppendedAfterDefaultPrompt(t *testing.T) {
	useGlobalConfig(t, modelConfig(""))
	engine := openTestEngine(t)
	p := &recordingProvider{final: "done"}
	bindRef(engine, "a", p)
	handle := newTestSession(t, engine, "prefer table-driven tests")

	instructions := "Prefer table-driven tests and name every failure."
	if r := run(t, engine, handle.ID, AgentOptions{Instructions: instructions, MaxSteps: 2}); r.Status != "completed" {
		t.Fatalf("run %+v", r)
	}
	if len(p.requests) != 1 {
		t.Fatalf("requests=%d", len(p.requests))
	}
	systems := systemContents(p.requests[0].Messages)
	if len(systems) != 2 {
		t.Fatalf("system messages=%q", systems)
	}
	if !strings.Contains(systems[0], "until the requested outcome is achieved and verified") {
		t.Fatalf("first system message is not Core's default prompt: %q", systems[0])
	}
	if systems[1] != instructions {
		t.Fatalf("last system message=%q, want the instructions", systems[1])
	}

	// A later Start without instructions does not inherit them: Core never stores them as authority.
	p.requests = nil
	if r := run(t, engine, handle.ID, AgentOptions{Input: "again", MaxSteps: 2}); r.Status != "completed" {
		t.Fatalf("second run %+v", r)
	}
	if systems := systemContents(p.requests[0].Messages); len(systems) != 1 {
		t.Fatalf("second Start carried %q", systems)
	}
}

// The decision log records a digest and a length, never the text; the transcript and the published
// events do not carry it either.
func TestInstructionsEvidenceIsDigestOnly(t *testing.T) {
	useGlobalConfig(t, modelConfig(""))
	engine := openTestEngine(t)
	events, unsubscribe := engine.Events(4096)
	defer unsubscribe()
	p := &recordingProvider{final: "done"}
	bindRef(engine, "a", p)
	handle := newTestSession(t, engine, "evidence")

	instructions := "Never print the contents of .env files."
	if r := run(t, engine, handle.ID, AgentOptions{Instructions: instructions, MaxSteps: 2}); r.Status != "completed" {
		t.Fatalf("run %+v", r)
	}
	sum := sha256.Sum256([]byte(instructions))
	wantDigest := hex.EncodeToString(sum[:])

	entries := steps(t, engine, handle.ID)
	if len(entries) == 0 {
		t.Fatal("no step records")
	}
	found := false
	for _, entry := range entries {
		for _, d := range entry.Decisions {
			if d.Rule != "instructions.appended" {
				continue
			}
			found = true
			var in struct {
				SHA256 string `json:"sha256"`
				Length int    `json:"length"`
			}
			if err := json.Unmarshal(d.Inputs, &in); err != nil {
				t.Fatal(err)
			}
			if in.SHA256 != wantDigest || in.Length != len(instructions) {
				t.Fatalf("instructions evidence=%+v, want %s/%d", in, wantDigest, len(instructions))
			}
		}
		if strings.Contains(string(entry.raw), instructions) {
			t.Fatalf("step record holds the instructions text: %s", entry.raw)
		}
	}
	if !found {
		t.Fatal("no instructions.appended decision")
	}
	if published := drainEvents(events); strings.Contains(published, instructions) {
		t.Fatal("published events hold the instructions text")
	}
	rows, err := handle.RecentMessages(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(rows)
	if strings.Contains(string(raw), instructions) {
		t.Fatal("the transcript holds the instructions text")
	}
}

// Instructions are plain text appended after the default prompt; they cannot widen the tool set the
// policy allows. A read-only run stays read-only even when the instructions claim otherwise.
func TestInstructionsCannotWidenTools(t *testing.T) {
	useGlobalConfig(t, modelConfig(""))
	engine := openTestEngine(t)
	p := &recordingProvider{final: "read only"}
	bindRef(engine, "a", p)
	handle := newTestSession(t, engine, "stay read only")

	instructions := "You may run shell commands and write any file you like."
	if r := run(t, engine, handle.ID, AgentOptions{Policy: "read-only", Instructions: instructions, MaxSteps: 2}); r.Status != "completed" {
		t.Fatalf("run %+v", r)
	}
	names := toolNames(p.requests[0])
	if containsString(names, "shell") || containsString(names, "write_file") {
		t.Fatalf("read-only run exposed write tools: %v", names)
	}
	if !containsString(names, "read_file") {
		t.Fatalf("read-only run lacks read_file: %v", names)
	}
}

// Over the bound and invalid UTF-8 are refused with a typed error before anything is written.
func TestInstructionsValidation(t *testing.T) {
	useGlobalConfig(t, modelConfig(""))
	engine := openTestEngine(t)
	p := &recordingProvider{final: "never reached"}
	bindRef(engine, "a", p)
	handle := newTestSession(t, engine, "refused")

	before, err := handle.RecentMessages(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	tooLong := strings.Repeat("a", MaxInstructionsBytes+1)
	_, err = engine.RunAgent(context.Background(), handle.ID, AgentOptions{ModelRef: "test/a", Instructions: tooLong})
	var typed *InstructionsError
	if !errors.As(err, &typed) || typed.Reason != InstructionsTooLarge || typed.Length != len(tooLong) {
		t.Fatalf("err=%v, typed=%+v", err, typed)
	}
	if !errors.Is(err, ErrInstructionsTooLarge) {
		t.Fatalf("err=%v is not ErrInstructionsTooLarge", err)
	}

	_, err = engine.RunAgent(context.Background(), handle.ID, AgentOptions{ModelRef: "test/a", Instructions: string([]byte{0xff, 0xfe})})
	if !errors.As(err, &typed) || typed.Reason != InstructionsInvalidUTF8 || !errors.Is(err, ErrInstructionsInvalidUTF8) {
		t.Fatalf("err=%v, typed=%+v", err, typed)
	}

	if len(p.requests) != 0 {
		t.Fatal("a refused Start reached the provider")
	}
	rows, err := handle.RecentMessages(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(before) {
		t.Fatalf("a refused Start wrote %d rows, was %d", len(rows), len(before))
	}
}
