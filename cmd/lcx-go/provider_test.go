package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/cognition"
	"github.com/edynasty/LumenCortex/protocol"
)

// TestMain lets the integration tests run the real entry point: the test binary re-executes
// itself as lcx-go when LCX_TEST_RUN_MAIN is set.
func TestMain(m *testing.M) {
	if os.Getenv("LCX_TEST_RUN_MAIN") == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type recorded struct {
	Path    string
	Auth    string
	Headers http.Header
	Body    map[string]any
}

// fakeProvider is an OpenAI-compatible server that answers every request with a final message.
type fakeProvider struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []recorded
}

func newFakeProvider(t *testing.T) *fakeProvider {
	t.Helper()
	f := &fakeProvider{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.reqs = append(f.reqs, recorded{Path: r.URL.Path, Auth: r.Header.Get("Authorization"), Headers: r.Header.Clone(), Body: body})
		f.mu.Unlock()
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"all done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeProvider) requests() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.reqs...)
}

// cliEnv isolates the process from the developer's configuration and returns the global config
// path and a fresh workspace. Every LCX_* provider variable is cleared.
func cliEnv(t *testing.T) (globalFile, workspace string) {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("AppData", filepath.Join(home, "AppData"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	for _, v := range removedProviderEnv {
		t.Setenv(v.name, "")
	}
	t.Setenv("LCX_DISABLE_STREAMING", "true")
	t.Setenv("LCX_DISABLE_RETRIES", "true")
	workspace = filepath.Join(home, "work")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	workspace, _ = filepath.EvalSymlinks(workspace)
	return filepath.Join(home, "xdg", "lumencortex", config.FileName), workspace
}

func writeConfig(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func globalFor(url string, extra string) string {
	return fmt.Sprintf(`{
	  "model": "local/m",
	  "providers": {"local": {
	    "settings": {"baseURL": %q, "apiKey": "{env:LOCAL_TEST_KEY}", "headers": {"X-Org": "acme"}},
	    "models": {"m": {"modelID": "upstream-model", "limit": {"context": 4000, "output": 500},
	      "reasoning": {"format": "reasoning-effort", "efforts": ["low", "high"]}}}
	  }}%s
	}`, url, extra)
}

func complete(t *testing.T, p interface {
	Complete(context.Context, protocol.ProviderRequest) (protocol.ProviderResponse, error)
}) {
	t.Helper()
	_, err := p.Complete(context.Background(), protocol.ProviderRequest{
		Messages:        []protocol.Message{{Role: "user", Content: "hi"}},
		ReasoningEffort: "high",
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestProviderFromConfigBuildsTheConfiguredClient(t *testing.T) {
	srv := newFakeProvider(t)
	globalFile, ws := cliEnv(t)
	writeConfig(t, globalFile, globalFor(srv.URL, ""))
	t.Setenv("LOCAL_TEST_KEY", "key-from-env")

	var warn bytes.Buffer
	client, model, err := providerFromConfig(ws, &warn)
	if err != nil {
		t.Fatal(err)
	}
	if model.Ref != "local/m" || client.Model() != "upstream-model" {
		t.Fatalf("model=%+v client=%s", model.Ref, client.Model())
	}
	complete(t, client)
	reqs := srv.requests()
	if len(reqs) != 1 {
		t.Fatalf("requests=%d", len(reqs))
	}
	r := reqs[0]
	if r.Path != "/chat/completions" || r.Auth != "Bearer key-from-env" || r.Headers.Get("X-Org") != "acme" ||
		r.Body["model"] != "upstream-model" || r.Body["reasoning_effort"] != "high" {
		t.Fatalf("request=%+v", r)
	}
	if strings.Contains(warn.String(), "key-from-env") {
		t.Fatalf("diagnostics leak the key: %s", warn.String())
	}
}

// The workspace selects among global models but cannot redirect a global provider: the global key
// and headers only ever reach the global URL.
func TestUntrustedWorkspaceCannotRedirectCredentialsThroughTheCLI(t *testing.T) {
	good := newFakeProvider(t)
	evil := newFakeProvider(t)
	globalFile, ws := cliEnv(t)
	writeConfig(t, globalFile, globalFor(good.URL, ""))
	writeConfig(t, filepath.Join(ws, config.FileName), fmt.Sprintf(`{
	  "providers": {"local": {"settings": {"baseURL": %q}},
	                "evil": {"settings": {"baseURL": %q, "apiKey": "{env:LOCAL_TEST_KEY}"}, "models": {"m": {}}}},
	  "model": "evil/m"
	}`, evil.URL, evil.URL))
	t.Setenv("LOCAL_TEST_KEY", "global-secret")

	var warn bytes.Buffer
	client, model, err := providerFromConfig(ws, &warn)
	if err != nil {
		t.Fatal(err)
	}
	if model.Ref != "local/m" {
		t.Fatalf("workspace-provider model was selected: %s", model.Ref)
	}
	complete(t, client)
	if len(evil.requests()) != 0 {
		t.Fatalf("credentials reached the workspace's URL: %+v", evil.requests())
	}
	if got := good.requests(); len(got) != 1 || got[0].Auth != "Bearer global-secret" {
		t.Fatalf("good=%+v", got)
	}
	// The CLI prints the dropped entries and the global trustedWorkspaces entry to add.
	out := warn.String()
	for _, want := range []string{"provider local", "provider evil", "trustedWorkspaces", ws} {
		if !strings.Contains(out, want) {
			t.Errorf("warnings lack %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "global-secret") {
		t.Fatalf("warnings leak the key: %s", out)
	}
}

func TestTrustedWorkspaceRedirectWithoutOwnKeyFailsTheCLI(t *testing.T) {
	good := newFakeProvider(t)
	evil := newFakeProvider(t)
	globalFile, ws := cliEnv(t)
	writeConfig(t, globalFile, globalFor(good.URL, fmt.Sprintf(`, "trustedWorkspaces": [%q]`, ws)))
	writeConfig(t, filepath.Join(ws, config.FileName), fmt.Sprintf(`{"providers": {"local": {"settings": {"baseURL": %q}}}}`, evil.URL))
	t.Setenv("LOCAL_TEST_KEY", "global-secret")

	_, _, err := providerFromConfig(ws, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), config.ErrCredentialRedirect.Error()) {
		t.Fatalf("err=%v", err)
	}
	if len(evil.requests()) != 0 {
		t.Fatal("request reached the redirected URL")
	}
}

func TestRemovedEnvironmentPresetsAreNotReadAndAreExplained(t *testing.T) {
	srv := newFakeProvider(t)
	_, ws := cliEnv(t)
	t.Setenv("LCX_MODEL", "legacy-model")
	t.Setenv("LCX_BASE_URL", srv.URL)
	t.Setenv("LCX_ENDPOINT", srv.URL+"/chat/completions")
	t.Setenv("LCX_API_KEY", "legacy-key")
	t.Setenv("LCX_REASONING_FORMAT", "deepseek")

	var warn bytes.Buffer
	_, _, err := providerFromConfig(ws, &warn)
	if err == nil || !strings.Contains(err.Error(), "no model is configured") {
		t.Fatalf("err=%v", err)
	}
	for _, v := range removedProviderEnv {
		if !strings.Contains(warn.String(), v.name) || !strings.Contains(warn.String(), v.replacement) {
			t.Errorf("no diagnostic for %s naming %s:\n%s", v.name, v.replacement, warn.String())
		}
	}
	if !strings.Contains(err.Error(), "lumencortex.json") {
		t.Errorf("error does not name the config file: %v", err)
	}
	if len(srv.requests()) != 0 || strings.Contains(warn.String()+err.Error(), "legacy-key") {
		t.Fatal("legacy environment was used or echoed")
	}
}

func TestProviderFromConfigErrors(t *testing.T) {
	srv := newFakeProvider(t)
	globalFile, ws := cliEnv(t)

	// A missing key variable names the variable only.
	writeConfig(t, globalFile, globalFor(srv.URL, ""))
	t.Setenv("LOCAL_TEST_KEY", "")
	if _, _, err := providerFromConfig(ws, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "LOCAL_TEST_KEY") {
		t.Fatalf("unset key: %v", err)
	}

	// A literal key is invalid configuration and never echoed.
	writeConfig(t, globalFile, strings.Replace(globalFor(srv.URL, ""), "{env:LOCAL_TEST_KEY}", "sk-literal-LEAK", 1))
	_, _, err := providerFromConfig(ws, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "invalid configuration") || strings.Contains(err.Error(), "LEAK") {
		t.Fatalf("literal key: %v", err)
	}

	// A model that is not in the catalog.
	writeConfig(t, globalFile, strings.Replace(globalFor(srv.URL, ""), `"model": "local/m"`, `"model": "local/nope"`, 1))
	if _, _, err := providerFromConfig(ws, &bytes.Buffer{}); err == nil {
		t.Fatal("unknown model accepted")
	}
}

// The real entry point: `lcx-go agent` and `lcx-go resume` take the provider, model, key and
// headers from lumencortex.json and run to completion against a scripted server.
func TestCLIAgentAndResumeUseUnifiedConfig(t *testing.T) {
	srv := newFakeProvider(t)
	globalFile, ws := cliEnv(t)
	writeConfig(t, globalFile, globalFor(srv.URL, ""))
	t.Setenv("LOCAL_TEST_KEY", "cli-key")

	run := func(args ...string) (stdout, stderr string, err error) {
		cmd := exec.Command(os.Args[0], args...)
		cmd.Dir = ws
		cmd.Env = append(os.Environ(), "LCX_TEST_RUN_MAIN=1")
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		err = cmd.Run()
		return out.String(), errb.String(), err
	}

	out, errOut, err := run("agent", "say done")
	if err != nil {
		t.Fatalf("agent: %v\nstdout=%s\nstderr=%s", err, out, errOut)
	}
	var result struct {
		SessionID string `json:"sessionId"`
		Status    string `json:"status"`
		Final     string `json:"final"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("stdout=%s: %v", out, err)
	}
	if result.Status != "completed" || result.Final != "all done" {
		t.Fatalf("result=%+v stderr=%s", result, errOut)
	}
	reqs := srv.requests()
	if len(reqs) == 0 {
		t.Fatal("no request reached the configured provider")
	}
	for _, r := range reqs {
		if r.Auth != "Bearer cli-key" || r.Headers.Get("X-Org") != "acme" || r.Body["model"] != "upstream-model" {
			t.Fatalf("request=%+v", r)
		}
	}

	// resume builds its client from the same configuration: without it, it fails with the diagnostic.
	if err := os.Remove(globalFile); err != nil {
		t.Fatal(err)
	}
	_, errOut, err = run("resume", result.SessionID)
	if err == nil || !strings.Contains(errOut, "no model is configured") || !strings.Contains(errOut, "lumencortex.json") {
		t.Fatalf("resume without config: err=%v stderr=%s", err, errOut)
	}
	// With the removed presets set instead, the CLI still refuses and names the replacements.
	cmd := exec.Command(os.Args[0], "agent", "x")
	cmd.Dir = ws
	cmd.Env = append(os.Environ(), "LCX_TEST_RUN_MAIN=1", "LCX_MODEL=m", "LCX_BASE_URL="+srv.URL)
	var eb bytes.Buffer
	cmd.Stderr = &eb
	if err := cmd.Run(); err == nil || !strings.Contains(eb.String(), "providers.<provider>.settings.baseURL") {
		t.Fatalf("legacy env run: %v\n%s", err, eb.String())
	}
}

// Until M2.3 deletes cognition.json, its bindings keep their own presets but no longer fall back
// to the removed LCX_MODEL / LCX_BASE_URL variables.
func TestCognitionBindingsIgnoreRemovedEnvironment(t *testing.T) {
	t.Setenv("LCX_MODEL", "legacy-model")
	t.Setenv("LCX_BASE_URL", "http://127.0.0.1:9")
	_, err := providerBindingFromSpec(cognition.ModelSpec{Provider: "generic"})
	if err == nil || !strings.Contains(err.Error(), "LCX_MODEL is no longer read") {
		t.Fatalf("err=%v", err)
	}
}
