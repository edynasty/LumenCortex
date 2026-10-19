package openai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/config"
)

// error the factory can return, even when the client build fails after resolution.
const secretValue = "sk-secret-NEVER-PRINT-246810"

type fakeResolver struct {
	secrets map[string]string
}

func (f fakeResolver) Resolve(name string) (string, error) {
	if v, ok := f.secrets[name]; ok && v != "" {
		return v, nil
	}
	return "", fmt.Errorf("%w: %s", config.ErrCredentialNotFound, name)
}

func TestFromModelResolvesSecretReferencesThroughCredentials(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()
	m := config.ResolvedModel{
		Ref: "p/m", ProviderID: "p", ModelID: "upstream", BaseURL: server.URL,
		APIKeyRef: "{secret:lumencortex.p-key}", ReasoningFormat: config.ReasoningNone, IdleTimeout: time.Minute,
	}
	client, err := FromModel(m, Options{Credentials: fakeResolver{secrets: map[string]string{"lumencortex.p-key": secretValue}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Complete(context.Background(), request()); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer "+secretValue {
		t.Fatalf("auth=%q", gotAuth)
	}
}

func TestFromModelSecretFailuresNameOnlyTheReference(t *testing.T) {
	m := config.ResolvedModel{
		Ref: "p/m", ProviderID: "p", ModelID: "upstream", BaseURL: "https://p.example",
		APIKeyRef: "{secret:lumencortex.p-key}", ReasoningFormat: config.ReasoningNone, IdleTimeout: time.Minute,
	}
	// No resolver: the reference is named, never a value.
	_, err := FromModel(m, Options{})
	if err == nil || !strings.Contains(err.Error(), "lumencortex.p-key") || strings.Contains(err.Error(), secretValue) {
		t.Fatalf("nil resolver: %v", err)
	}
	// A resolver without the name reports not-found.
	_, err = FromModel(m, Options{Credentials: fakeResolver{}})
	if !errors.Is(err, config.ErrCredentialNotFound) || strings.Contains(err.Error(), secretValue) {
		t.Fatalf("not found: %v", err)
	}
	// Resolution succeeds, the client build fails afterwards: the error still carries no value.
	m.BaseURL = ""
	_, err = FromModel(m, Options{Credentials: fakeResolver{secrets: map[string]string{"lumencortex.p-key": secretValue}}})
	if !errors.Is(err, ErrNoEndpoint) {
		t.Fatalf("no endpoint: %v", err)
	}
	if strings.Contains(err.Error(), secretValue) {
		t.Fatalf("build failure leaks the key: %v", err)
	}
}
