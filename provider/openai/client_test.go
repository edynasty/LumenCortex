package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/edynasty/LumenCortex/protocol"
)

func TestStreamingToolCallAssembly(t *testing.T) {
	var sawAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request["model"] != "deepseek-test" || request["stream"] != true {
			t.Fatalf("request=%#v", request)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"she","arguments":"{\"command\":\""}}]},"finish_reason":null}]}`)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"ll","arguments":"go test ./...\"}"}}]},"finish_reason":"tool_calls"}]}`)
		fmt.Fprintln(w, `data: {"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":7,"total_tokens":19}}`)
		fmt.Fprintln(w, "data: [DONE]")
	}))
	defer server.Close()

	client, err := New(Config{Endpoint: server.URL, APIKey: "secret", Model: "deepseek-test", DisableRetries: true})
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Complete(context.Background(), protocol.ProviderRequest{
		Messages: []protocol.Message{{Role: "user", Content: "fix it"}},
		Tools: []protocol.ToolSpec{{Name: "shell", Parameters: map[string]any{"type": "object"}}},
		ToolChoice: "auto",
	})
	if err != nil {
		t.Fatal(err)
	}
	if sawAuth != "Bearer secret" {
		t.Fatalf("auth=%q", sawAuth)
	}
	if len(response.Message.ToolCalls) != 1 {
		t.Fatalf("tool calls=%#v", response.Message.ToolCalls)
	}
	call := response.Message.ToolCalls[0]
	if call.Name != "shell" || string(call.Arguments) != `{"command":"go test ./..."}` {
		t.Fatalf("call=%#v", call)
	}
	if response.Usage.TotalTokens != 19 || response.FinishReason != "tool_calls" {
		t.Fatalf("response=%#v", response)
	}
}

func TestNonStreamingResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`)
	}))
	defer server.Close()

	client, err := New(Config{Endpoint: server.URL, Model: "test", DisableStreaming: true, DisableRetries: true})
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Complete(context.Background(), protocol.ProviderRequest{Messages: []protocol.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Message.Content != "done" || response.Usage.TotalTokens != 4 {
		t.Fatalf("response=%#v", response)
	}
}

func TestRetries429AndHonorsBoundedAttempts(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := attempts.Add(1)
		if current < 3 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, "busy", http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{}}`)
	}))
	defer server.Close()

	client, err := New(Config{Endpoint: server.URL, Model: "test", DisableStreaming: true, MaxRetries: 2, RetryBaseDelay: 1})
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Complete(context.Background(), protocol.ProviderRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if response.Message.Content != "ok" || attempts.Load() != 3 {
		t.Fatalf("response=%#v attempts=%d", response, attempts.Load())
	}
}

func TestResponseBudget(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, strings.Repeat("x", 2048))
	}))
	defer server.Close()

	client, err := New(Config{Endpoint: server.URL, Model: "test", DisableStreaming: true, DisableRetries: true, MaxResponseBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Complete(context.Background(), protocol.ProviderRequest{})
	if err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("err=%v", err)
	}
}


func TestReasoningWireFormats(t *testing.T) {
	object := requestFromProtocol("m", protocol.ProviderRequest{ReasoningEffort: "high"}, false, "reasoning-object")
	if object.Reasoning == nil || object.Reasoning.Effort != "high" || object.ReasoningEffort != "" {
		t.Fatalf("reasoning-object=%#v", object)
	}

	groq := requestFromProtocol("m", protocol.ProviderRequest{ReasoningEffort: "max"}, false, "reasoning-effort")
	if groq.ReasoningEffort != "high" || groq.Reasoning != nil {
		t.Fatalf("reasoning-effort=%#v", groq)
	}

	deepseek := requestFromProtocol("m", protocol.ProviderRequest{ReasoningEffort: "none"}, false, "deepseek")
	if deepseek.ReasoningEffort != "none" || deepseek.Thinking == nil || deepseek.Thinking.Type != "disabled" {
		t.Fatalf("deepseek none=%#v", deepseek)
	}

	generic := requestFromProtocol("m", protocol.ProviderRequest{ReasoningEffort: "high"}, false, "")
	if generic.Reasoning != nil || generic.ReasoningEffort != "" || generic.Thinking != nil {
		t.Fatalf("generic should not inject reasoning fields: %#v", generic)
	}
}
