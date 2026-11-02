package runtime

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/provider/openai"
)

func TestAstraProviderErrorPersistsWithoutCredentials(t *testing.T) {
	const key = "astra-runtime-fixture-key-79ac"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintf(w, `{"error":{"message":"rejected %s"}}`, r.Header.Get("Authorization"))
	}))
	defer server.Close()
	engine := openTestEngine(t)
	handle := newTestSession(t, engine, "reject provider credentials safely")
	client, err := openai.New(openai.Config{Endpoint: server.URL, Model: "fixture", APIKey: key, DisableStreaming: true, DisableRetries: true})
	if err != nil {
		t.Fatal(err)
	}
	result, runErr := engine.runAgentWith(context.Background(), handle.ID, client, AgentOptions{Policy: "read-only", MaxSteps: 1})
	if runErr == nil || result.Status != "failed" {
		t.Fatalf("want provider failure; result=%#v err=%v", result, runErr)
	}
	row, err := engine.store.Get(context.Background(), handle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(row.Error), key) {
		t.Fatal("durable session error contains provider API key")
	}
	if strings.Contains(runErr.Error(), key) {
		t.Fatal("returned runtime error contains provider API key")
	}
}
