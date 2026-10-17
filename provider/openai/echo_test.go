package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/protocol"
)

const echoSentinel = "ECHO-SENTINEL-REASONING-91c2"

// echoServer answers every request with reasoning_content and records each request body.
func echoServer(t *testing.T, stream bool, bodies *[]map[string]any) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		*bodies = append(*bodies, body)
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"role":"assistant","reasoning_content":"`+echoSentinel[:6]+`"}}]}`)
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"reasoning_content":"`+echoSentinel[6:]+`","content":"ok"},"finish_reason":"stop"}]}`)
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok","reasoning_content":"`+echoSentinel+`"},"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(server.Close)
	return server
}

func wireMessages(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, _ := json.Marshal(body["messages"])
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// reasoning.echo = reasoning_content: the response's reasoning_content becomes its Echo and never its
// Message; a request's Echo goes back as reasoning_content on exactly the aligned assistant message.
// reasoning.echo = none: no Echo is returned and none is sent, whatever the request holds.
func TestReasoningContentEcho(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, mode := range []string{"reasoning_content", "none"} {
			name := fmt.Sprintf("stream=%v/echo=%s", stream, mode)
			var bodies []map[string]any
			server := echoServer(t, stream, &bodies)
			client, err := New(Config{Endpoint: server.URL, Model: "m", DisableStreaming: !stream, DisableRetries: true, ReasoningEcho: mode})
			if err != nil {
				t.Fatal(err)
			}
			resp, err := client.Complete(context.Background(), protocol.ProviderRequest{Messages: []protocol.Message{{Role: "user", Content: "q"}}})
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if raw, _ := json.Marshal(resp.Message); strings.Contains(string(raw), echoSentinel) || strings.Contains(resp.Message.Content, echoSentinel) {
				t.Fatalf("%s: reasoning reached the message: %s", name, raw)
			}
			if mode == "none" {
				if !resp.Echo.IsZero() {
					t.Fatalf("%s: echo returned under reasoning.echo none", name)
				}
				resp.Echo = protocol.NewEcho("reasoning_content", []byte(echoSentinel), 1) // what a buggy loop might send
			} else if string(resp.Echo.Data()) != echoSentinel || resp.Echo.Kind() != "reasoning_content" || resp.Echo.Tokens() <= 0 {
				t.Fatalf("%s: echo %v", name, resp.Echo)
			}

			messages := []protocol.Message{
				{Role: "user", Content: "q"},
				{Role: "assistant", ToolCalls: []protocol.ToolCall{{ID: "c1", Name: "read", Arguments: json.RawMessage(`{}`)}}},
				{Role: "tool", ToolCallID: "c1", Content: "r"},
				{Role: "assistant", Content: "earlier turn"},
			}
			echo := []protocol.Echo{{}, resp.Echo, resp.Echo, {}} // a tool row never carries echo
			if _, err := client.Complete(context.Background(), protocol.ProviderRequest{Messages: messages, Echo: echo}); err != nil {
				t.Fatal(err)
			}
			sent := wireMessages(t, bodies[1])
			for i, m := range sent {
				got, has := m["reasoning_content"]
				want := mode == "reasoning_content" && i == 1
				if want && got != echoSentinel {
					t.Fatalf("%s: message %d reasoning_content=%v, want the echo", name, i, got)
				}
				if !want && has {
					t.Fatalf("%s: message %d carries reasoning_content %v", name, i, got)
				}
			}
			if raw, _ := json.Marshal(bodies[0]); strings.Contains(string(raw), echoSentinel) {
				t.Fatalf("%s: first request carried reasoning: %s", name, raw)
			}
		}
	}
}
