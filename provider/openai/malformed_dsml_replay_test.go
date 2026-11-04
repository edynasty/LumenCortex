package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/edynasty/LumenCortex/protocol"
)

// Optional evidence-based regression: LCX_BENCH004_TRACE_ROOT points at the
// local bench004 directory. No prompts, reasoning, tokens or SSE body are copied
// into source control or printed; the exact captured last reply is replayed.
func TestCapturedFinalResponsesReplay(t *testing.T) {
	root := os.Getenv("LCX_BENCH004_TRACE_ROOT")
	if root == "" {
		t.Skip("set LCX_BENCH004_TRACE_ROOT for captured local replay")
	}
	cases := []struct {
		task      string
		malformed bool
	}{
		{"tengo-callable-instance-isolation", true},
		{"testem-bail-on-test-failure", true},
		{"true-myth-iterable-collection-combinators", false},
	}
	for _, tt := range cases {
		t.Run(tt.task, func(t *testing.T) {
			dir := filepath.Join(root, tt.task, "trace")
			raw, err := os.ReadFile(filepath.Join(dir, "chat.json"))
			if err != nil {
				t.Fatal(err)
			}
			var trace struct {
				Attempts []struct {
					WireResponse struct {
						Body struct {
							Ref string `json:"ref"`
						} `json:"body"`
					} `json:"wire_response"`
				} `json:"attempts"`
			}
			if err := json.Unmarshal(raw, &trace); err != nil {
				t.Fatal(err)
			}
			if len(trace.Attempts) == 0 {
				t.Fatal("missing attempts")
			}
			ref := trace.Attempts[len(trace.Attempts)-1].WireResponse.Body.Ref
			file := filepath.Join(dir, filepath.Clean(ref))
			if filepath.Dir(file) != filepath.Join(dir, "blobs", "sha256") {
				t.Fatal("unexpected trace ref path")
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				f, err := os.Open(file)
				if err != nil {
					http.Error(w, "fixture missing", 500)
					return
				}
				defer f.Close()
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.Copy(w, f)
			}))
			defer server.Close()
			c, err := New(Config{Endpoint: server.URL, Model: "replay", MaxResponseBytes: 4 << 20})
			if err != nil {
				t.Fatal(err)
			}
			resp, err := c.Complete(context.Background(), protocol.ProviderRequest{Messages: []protocol.Message{{Role: "user", Content: "replay"}}, Tools: []protocol.ToolSpec{{Name: "bash"}}})
			var pe *protocol.ProviderError
			if tt.malformed {
				if !errors.As(err, &pe) || pe.Class != protocol.ClassAvailability {
					t.Fatalf("captured malformed reply was not isolated: %v", err)
				}
			} else if err != nil || resp.FinishReason != "stop" || len(resp.Message.ToolCalls) != 0 {
				t.Fatalf("valid protocol stop should remain a stop: err=%v finish=%s calls=%d", err, resp.FinishReason, len(resp.Message.ToolCalls))
			}
			t.Log(fmt.Sprintf("captured SSE classified: malformed=%t", tt.malformed))
		})
	}
}
