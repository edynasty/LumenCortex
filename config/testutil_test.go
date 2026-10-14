package config

import (
	"os"
	"path/filepath"
	"testing"
)

// env isolates a test from the developer's real configuration: HOME and XDG_CONFIG_HOME point into
// the test's temporary directory (AppData/USERPROFILE too on Windows). It returns the home and the
// GlobalDir it implies.
func env(t *testing.T) (home, globalDir string) {
	t.Helper()
	home = canon(t, t.TempDir())
	xdg := filepath.Join(home, "xdg")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("AppData", filepath.Join(home, "AppData"))
	t.Setenv("XDG_CONFIG_HOME", xdg)
	globalDir = filepath.Join(xdg, "lumencortex")
	if got, err := GlobalDir(); err != nil || got != globalDir {
		t.Fatalf("GlobalDir=%q,%v want %q", got, err, globalDir)
	}
	return home, globalDir
}

func canon(t *testing.T, p string) string {
	t.Helper()
	c, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// globalJSON is the shared global catalog: deepseek (own key, no headers) and openrouter (headers).
const globalJSON = `{
  "model": "deepseek/chat",
  "failover": ["openrouter/sonnet"],
  "subagentModel": "deepseek/coder",
  "effort": {"default": "medium", "escalation": {"sameFailure": 2, "noProgressSteps": 6, "max": "high"}},
  "providers": {
    "deepseek": {
      "name": "DeepSeek",
      "settings": {"baseURL": "https://api.deepseek.com", "apiKey": "{env:DEEPSEEK_API_KEY}"},
      "models": {
        "chat": {"modelID": "deepseek-chat", "limit": {"context": 128000, "output": 8192}},
        "coder": {"modelID": "deepseek-coder", "limit": {"context": 128000, "output": 8192}}
      }
    },
    "openrouter": {
      "settings": {
        "baseURL": "https://openrouter.ai/api/v1",
        "apiKey": "{env:OPENROUTER_API_KEY}",
        "headers": {"X-Org-Token": "org-secret-header", "HTTP-Referer": "https://example.test"}
      },
      "models": {"sonnet": {"modelID": "anthropic/claude-sonnet-4", "limit": {"context": 200000, "output": 16000}}}
    }
  }
}`

// loaded writes the global file and a workspace file, then loads the workspace. trust maps the
// canonical workspace path to the global trustedWorkspaces list (nil: trust nothing).
func loaded(t *testing.T, trust func(ws string) []string, workspaceJSON string) (*Config, string, error) {
	t.Helper()
	_, globalDir := env(t)
	ws := canon(t, t.TempDir())
	doc, err := Parse([]byte(globalJSON))
	if err != nil {
		t.Fatal(err)
	}
	if trust != nil {
		doc.TrustedWorkspaces = trust(ws)
	}
	if _, err := Save(filepath.Join(globalDir, FileName), doc, ""); err != nil {
		t.Fatal(err)
	}
	if workspaceJSON != "" {
		writeFile(t, filepath.Join(ws, FileName), workspaceJSON)
	}
	c, err := Load(ws)
	return c, ws, err
}

// trustSelf trusts exactly the workspace.
func trustSelf(ws string) []string { return []string{ws} }
