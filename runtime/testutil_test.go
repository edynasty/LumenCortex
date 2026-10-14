package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/protocol"
)

// boundModels is how many models the fixture catalog holds: one per provider a test binds to an Engine.
const boundModels = 64

// TestMain points the whole package at a private configuration directory, so no test reads the
// developer's lumencortex.json, mcp.json or skills, and installs a global catalog of boundModels
// models, test/b0 ... test/b63, each with a baseURL that is never contacted: the Engine's
// ProviderFactory returns the bound fake instead.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "lcx-runtime-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		home = resolved
	}
	for _, name := range []string{"HOME", "USERPROFILE"} {
		os.Setenv(name, home)
	}
	os.Setenv("AppData", filepath.Join(home, "AppData"))
	os.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	for _, name := range []string{"LCX_MODEL", "LCX_ENDPOINT", "LCX_BASE_URL", "LCX_API_KEY", "LCX_REASONING_FORMAT"} {
		os.Unsetenv(name)
	}
	if err := os.MkdirAll(filepath.Join(home, "xdg", "lumencortex"), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.WriteFile(filepath.Join(home, "xdg", "lumencortex", config.FileName), []byte(boundCatalog("test/b0")), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

func boundCatalog(model string) string {
	var models []string
	for i := 0; i < boundModels; i++ {
		models = append(models, fmt.Sprintf(`"b%d": {"modelID": "b%d"}`, i, i))
	}
	return fmt.Sprintf(`{"model": %q, "providers": {"test": {"settings": {"baseURL": "http://127.0.0.1:9/v1"}, "models": {%s}}}}`,
		model, strings.Join(models, ","))
}

// binder maps the catalog models of one Engine to the fakes tests bound to them.
type binder struct {
	mu        sync.Mutex
	next      int
	providers map[string]protocol.Provider
}

var binders sync.Map // *Engine -> *binder

// bind attaches p to a fresh catalog model of e and returns its reference. The Engine builds p for
// that reference through its ProviderFactory, which is how a host injects a client.
func bind(e *Engine, p protocol.Provider) config.ModelRef {
	b := binderFor(e)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.next >= boundModels-reservedModels {
		panic("test bound more providers than the fixture catalog has models")
	}
	id := fmt.Sprintf("b%d", b.next)
	b.next++
	b.providers[id] = p
	return config.ModelRef("test/" + id)
}

// reservedModels is how many of the highest catalog models bindRef hands out by name.
const reservedModels = 4

// binderFor returns e's binder and installs the Engine's ProviderFactory on first use.
func binderFor(e *Engine) *binder {
	v, loaded := binders.LoadOrStore(e, &binder{providers: map[string]protocol.Provider{}})
	b := v.(*binder)
	if !loaded {
		e.providerFactory = func(m config.ResolvedModel) (protocol.Provider, error) {
			b.mu.Lock()
			defer b.mu.Unlock()
			if p, ok := b.providers[m.ModelID]; ok {
				return p, nil
			}
			return nil, fmt.Errorf("no provider bound to model %s", m.Ref)
		}
	}
	return b
}

// runAgentWith runs RunAgent with p as the session model's client.
func (e *Engine) runAgentWith(ctx context.Context, sessionID string, p protocol.Provider, opts AgentOptions) (AgentResult, error) {
	opts.ModelRef = bind(e, p)
	return e.RunAgent(ctx, sessionID, opts)
}

// startWith runs Start with p as the session model's client.
func (s *RunSupervisor) startWith(ctx context.Context, sessionID string, p protocol.Provider, opts AgentOptions) (SessionInfo, error) {
	opts.ModelRef = bind(s.engine, p)
	return s.Start(ctx, sessionID, opts)
}

// useGlobalConfig replaces the fixture's global configuration for the test.
func useGlobalConfig(t *testing.T, content string) {
	t.Helper()
	xdg := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(xdg); err == nil {
		xdg = resolved
	}
	dir := filepath.Join(xdg, "lumencortex")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, config.FileName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", xdg)
}

func decodeJSON(t *testing.T, raw []byte, into any) {
	t.Helper()
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
}

// bindRef attaches p to the catalog model test/<id> of e (one of the last reservedModels ids), for
// tests that name the model in the configuration (subagentModel, failover).
func bindRef(e *Engine, id string, p protocol.Provider) config.ModelRef {
	b := binderFor(e)
	b.mu.Lock()
	b.providers[id] = p
	b.mu.Unlock()
	return config.ModelRef("test/" + id)
}
