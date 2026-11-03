package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/edynasty/LumenCortex/protocol"
)

// Reduced fixtures from BENCH-004 Tengo and Testem: only the final visible
// content and finish_reason, never private reasoning or the original prompts.
func TestMalformedDSMLStopIsRejected(t *testing.T) {
	fixtures := []struct{ name, content string }{
		{"tengo", "I still need to edit script.go.\n</parameter>\n</invoke>\n</｜｜DSML｜｜ parameter>\n</｜｜DSML｜｜ invoke>\n</｜｜DSML｜｜ calls>"},
		{"testem", "  </｜｜DSML｜｜ parameter>\n</｜｜DSML｜｜ invoke>\n</｜｜DSML｜｜ calls>"},
	}
	for _, fixture := range fixtures {
		for _, stream := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/stream=%v", fixture.name, stream), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					choice := map[string]any{"finish_reason": "stop"}
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						choice["delta"] = map[string]any{"content": fixture.content}
						data, _ := json.Marshal(map[string]any{"choices": []any{choice}})
						fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n", data)
						return
					}
					choice["message"] = map[string]any{"role": "assistant", "content": fixture.content}
					_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{choice}})
				}))
				defer server.Close()
				client, err := New(Config{Endpoint: server.URL, Model: "fixture", DisableStreaming: !stream})
				if err != nil {
					t.Fatal(err)
				}
				response, err := client.Complete(context.Background(), protocol.ProviderRequest{Messages: []protocol.Message{{Role: "user", Content: "fix code"}}, Tools: []protocol.ToolSpec{{Name: "bash"}}})
				var pe *protocol.ProviderError
				if !errors.As(err, &pe) || pe.Class != protocol.ClassAvailability {
					t.Fatalf("malformed provider tool output accepted: response status=%s toolCalls=%d error=%v", response.FinishReason, len(response.Message.ToolCalls), err)
				}
				if strings.Contains(err.Error(), fixture.content) {
					t.Fatal("error must not embed raw model content")
				}
			})
		}
	}
}

func TestMalformedDSMLGuardDoesNotBlockOrdinaryOutputs(t *testing.T) {
	tail := "  </｜｜DSML｜｜ parameter>\n</｜｜DSML｜｜ invoke>\n</｜｜DSML｜｜ calls>"
	request := protocol.ProviderRequest{Tools: []protocol.ToolSpec{{Name: "bash"}}}
	cases := []struct {
		name      string
		content   string
		finish    string
		calls     []protocol.ToolCall
		tools     bool
		malformed bool
	}{
		{name: "normal final answer", content: "Implemented and verified.", finish: "stop", tools: true},
		{name: "DSML described without invalid closure", content: "The marker </｜｜DSML｜｜ invoke> is a tool-format delimiter.", finish: "stop", tools: true},
		{name: "partial closure does not trigger false claim", content: "</｜｜DSML｜｜ calls>", finish: "stop", tools: true},
		{name: "no tool request", content: tail, finish: "stop", tools: false},
		{name: "actual structured tool call", content: tail, finish: "tool_calls", tools: true, calls: []protocol.ToolCall{{ID: "call1", Name: "bash", Arguments: json.RawMessage(`{"command":"echo ok"}`)}}},
		{name: "length finish is a separate interruption", content: tail, finish: "length", tools: true},
		{name: "full malformed stop", content: tail, finish: "stop", tools: true, malformed: true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			req := request
			if !tt.tools {
				req.Tools = nil
			}
			resp := protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", Content: tt.content, ToolCalls: tt.calls}, FinishReason: tt.finish}
			if got := malformedDSMLToolStop(resp, req); got != tt.malformed {
				t.Fatalf("detector=%t want=%t", got, tt.malformed)
			}
		})
	}
}

// Leaked tool-call delimiters are one availability/malformed_response attempt: the loop decides whether
// to send the step again, and nothing in the text is ever executed.
func TestMalformedDSMLStopIsOneMalformedAttempt(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"</｜｜DSML｜｜ parameter>\n</｜｜DSML｜｜ invoke>\n</｜｜DSML｜｜ calls>"},"finish_reason":"stop"}]}`)
		fmt.Fprintln(w, "data: [DONE]")
	}))
	defer server.Close()
	client, err := New(Config{Endpoint: server.URL, Model: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Complete(context.Background(), protocol.ProviderRequest{Tools: []protocol.ToolSpec{{Name: "bash"}}})
	var pe *protocol.ProviderError
	if !errors.As(err, &pe) || pe.Class != protocol.ClassAvailability || pe.Reason != protocol.ReasonMalformedResponse {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("got %d requests, want 1", got)
	}
}
