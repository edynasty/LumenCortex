package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

const literalKey = "sk-live-TOPSECRET-0123456789"

func TestLiteralAPIKeyRejectedOnLoadAndSave(t *testing.T) {
	_, globalDir := env(t)
	doc := `{"providers": {"p": {"settings": {"baseURL": "https://p.example", "apiKey": "` + literalKey + `"}, "models": {"m": {}}}}}`

	writeFile(t, filepath.Join(globalDir, FileName), doc)
	_, err := Load("")
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("global load: %v", err)
	}
	if strings.Contains(err.Error(), "TOPSECRET") {
		t.Fatalf("load error leaks the key: %v", err)
	}

	ws := canon(t, t.TempDir())
	writeFile(t, filepath.Join(ws, FileName), doc)
	if _, err = Load(ws); !errors.Is(err, ErrInvalidConfig) || strings.Contains(err.Error(), "TOPSECRET") {
		t.Fatalf("workspace load: %v", err)
	}

	f := File{Providers: map[string]Provider{"p": {Settings: &ProviderSettings{APIKey: literalKey}}}}
	target := filepath.Join(t.TempDir(), FileName)
	_, err = Save(target, f, "")
	if !errors.Is(err, ErrInvalidConfig) || strings.Contains(err.Error(), "TOPSECRET") {
		t.Fatalf("save: %v", err)
	}
	if _, statErr := ReadDocument(target); statErr != nil {
		t.Fatal(statErr)
	}
	if d, _ := ReadDocument(target); d.Digest != "" {
		t.Fatal("rejected Save created the file")
	}
	// Other shapes of "not an env reference".
	for _, k := range []string{"", "$KEY", "${KEY}", "{env:}", "{env:1A}", "{env:A B}", "{ENV:KEY}", "{env:KEY} ", "env:KEY", " {env:KEY}"} {
		f.Providers["p"] = Provider{Settings: &ProviderSettings{APIKey: k}}
		if k == "" {
			continue // absent
		}
		if _, err := Save(target, f, ""); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("apiKey %q accepted", k)
		}
	}
}

func TestEnvReferenceResolutionAndExposure(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", literalKey)
	t.Setenv("OPENROUTER_API_KEY", "")
	c, _, err := loaded(t, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	ds, _ := c.Resolve("deepseek/chat")
	if ds.APIKeyRef != "{env:DEEPSEEK_API_KEY}" || ds.APIKeyEnv() != "DEEPSEEK_API_KEY" || !ds.APIKeySet(nil) {
		t.Fatalf("deepseek=%+v", ds)
	}
	key, err := ds.ResolveAPIKey(nil)
	if err != nil || key != literalKey {
		t.Fatalf("key=%q err=%v", key, err)
	}
	// An unset variable is an error that names the variable and nothing else.
	or, _ := c.Resolve("openrouter/sonnet")
	if or.APIKeySet(nil) {
		t.Fatal("empty variable reported as set")
	}
	if _, err := or.ResolveAPIKey(nil); err == nil || !strings.Contains(err.Error(), "OPENROUTER_API_KEY") {
		t.Fatalf("unset: %v", err)
	}
	// A provider without apiKey needs none.
	if k, err := (ResolvedModel{}).ResolveAPIKey(nil); k != "" || err != nil {
		t.Fatalf("no key: %q %v", k, err)
	}

	// Nothing the host can serialize or print carries the secret or the header values.
	blobs := []string{fmt.Sprintf("%+v", c.Effective()), fmt.Sprintf("%#v", c.Catalog())}
	j, _ := json.Marshal(c.Catalog())
	blobs = append(blobs, string(j))
	j, _ = json.Marshal(ds)
	blobs = append(blobs, string(j))
	j, _ = json.Marshal(or)
	blobs = append(blobs, string(j))
	j, _ = json.Marshal(c.Effective())
	blobs = append(blobs, string(j))
	for _, b := range blobs {
		if strings.Contains(b, "TOPSECRET") || strings.Contains(b, "org-secret-header") {
			t.Errorf("secret or header value exposed: %.200s", b)
		}
	}
	info := c.Catalog()[0]
	if info.APIKeyEnv == "" || !info.APIKeySet && info.Ref == "deepseek/chat" {
		t.Fatalf("catalog info=%+v", info)
	}
}

func TestResolveDefaultsAndWarnings(t *testing.T) {
	_, globalDir := env(t)
	writeFile(t, filepath.Join(globalDir, FileName), `{"providers": {"p": {"settings": {"baseURL": "https://p.example"}, "models": {"bare": {}, "ds": {"reasoning": {"format": "deepseek"}, "limit": {"context": 1000, "output": 10}}}}}}`)
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	m, err := c.Resolve("p/bare")
	if err != nil {
		t.Fatal(err)
	}
	if m.ModelID != "bare" || m.Context != 32768 || m.Output != 4096 || !m.ToolCalling || m.ReasoningFormat != "none" ||
		m.ReasoningEcho != "none" || m.IdleTimeout.Seconds() != 120 || m.RequestTimeout != 0 || len(m.InputModalities) != 1 ||
		m.InputModalities[0] != "text" || len(m.Warnings) != 2 || m.APIKeyRef != "" {
		t.Fatalf("defaults=%+v", m)
	}
	d, _ := c.Resolve("p/ds")
	if d.ReasoningEcho != ReasoningEchoContent || len(d.Warnings) != 0 || d.Context != 1000 {
		t.Fatalf("deepseek=%+v", d)
	}
	for _, bad := range []ModelRef{"p", "p/", "/m", "p/m/x", "q/m", "p/missing"} {
		if _, err := c.Resolve(bad); !errors.Is(err, ErrUnknownModel) {
			t.Errorf("Resolve(%q)=%v", bad, err)
		}
	}
}
