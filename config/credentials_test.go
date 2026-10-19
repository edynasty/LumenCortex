package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// secretValue is planted in a fake resolver; every redaction test asserts it appears in no error,
// warning, JSON blob or fmt dump the package can produce.
const secretValue = "sk-secret-NEVER-PRINT-9876543210"

// fakeResolver is the test-only CredentialResolver; Core ships no implementation.
type fakeResolver struct {
	secrets map[string]string
	missing map[string]bool // names that report ErrCredentialNotFound
	failing map[string]bool // names whose store reports an unrelated error
	calls   []string
}

func (f *fakeResolver) Resolve(name string) (string, error) {
	f.calls = append(f.calls, name)
	if f.failing[name] {
		// A misbehaving store whose error text contains the value: resolveAPIKey must drop it.
		return "", fmt.Errorf("keychain unlocked the secret %s but the item is damaged", secretValue)
	}
	if f.missing[name] {
		return "", fmt.Errorf("%w: %s", ErrCredentialNotFound, name)
	}
	return f.secrets[name], nil
}

func secretLoaded(t *testing.T, apiKey string) (*Config, ResolvedModel, ResolvedProvider) {
	t.Helper()
	_, globalDir := env(t)
	writeFile(t, filepath.Join(globalDir, FileName), `{"providers": {"sp": {"settings": {"baseURL": "https://sp.example", "apiKey": "`+apiKey+`"}, "models": {"m": {}}}}}`)
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	m, err := c.Resolve("sp/m")
	if err != nil {
		t.Fatal(err)
	}
	p, err := c.Provider("sp")
	if err != nil {
		t.Fatal(err)
	}
	return c, m, p
}

func TestSecretReferenceResolvesThroughTheCredentialResolver(t *testing.T) {
	c, m, p := secretLoaded(t, "{secret:lumencortex.sp-key}")
	r := &fakeResolver{secrets: map[string]string{"lumencortex.sp-key": secretValue}}
	if m.APIKeyRef != "{secret:lumencortex.sp-key}" {
		t.Fatalf("ref=%q", m.APIKeyRef)
	}
	key, err := m.ResolveAPIKey(r)
	if err != nil || key != secretValue {
		t.Fatalf("model key=%q err=%v", key, err)
	}
	pkey, err := p.ResolveAPIKey(r)
	if err != nil || pkey != secretValue {
		t.Fatalf("provider key=%q err=%v", pkey, err)
	}
	if !m.APIKeySet(r) || !p.APIKeySet(r) {
		t.Fatal("resolvable secret reported as unset")
	}
	// A {secret:NAME} reference reads exactly one source: the resolver, never the environment.
	t.Setenv("lumencortex.sp-key", "env-value-must-not-win")
	if key, _ := m.ResolveAPIKey(r); key != secretValue {
		t.Fatalf("secret ref read the environment: %q", key)
	}
	r2 := &fakeResolver{missing: map[string]bool{"lumencortex.sp-key": true}}
	if _, err := m.ResolveAPIKey(r2); !errors.Is(err, ErrCredentialNotFound) {
		t.Fatalf("missing secret: %v", err)
	}
	_ = c
}

func TestEachReferencePicksExactlyOneSource(t *testing.T) {
	t.Setenv("SHARED_KEY", "the-env-value")
	r := &fakeResolver{secrets: map[string]string{"SHARED_KEY": "the-secret-value"}}
	envModel := ResolvedModel{Ref: "p/m", ProviderID: "p", APIKeyRef: "{env:SHARED_KEY}"}
	if v, err := envModel.ResolveAPIKey(r); err != nil || v != "the-env-value" {
		t.Fatalf("env ref: %q %v", v, err)
	}
	if len(r.calls) != 0 {
		t.Fatalf("env ref consulted the resolver: %v", r.calls)
	}
	secretModel := ResolvedModel{Ref: "p/m", ProviderID: "p", APIKeyRef: "{secret:SHARED_KEY}"}
	if v, err := secretModel.ResolveAPIKey(r); err != nil || v != "the-secret-value" {
		t.Fatalf("secret ref: %q %v", v, err)
	}
	if len(r.calls) != 1 || r.calls[0] != "SHARED_KEY" {
		t.Fatalf("secret ref resolver calls: %v", r.calls)
	}
}

func TestNilResolverWithSecretRefNamesOnlyTheReference(t *testing.T) {
	_, m, p := secretLoaded(t, "{secret:lumencortex.sp-key}")
	for _, res := range []struct {
		name string
		err  error
	}{
		{"model", func() error { _, err := m.ResolveAPIKey(nil); return err }()},
		{"provider", func() error { _, err := p.ResolveAPIKey(nil); return err }()},
	} {
		if res.err == nil {
			t.Fatalf("%s: nil resolver accepted a secret ref", res.name)
		}
		if !strings.Contains(res.err.Error(), "lumencortex.sp-key") || strings.Contains(res.err.Error(), secretValue) {
			t.Fatalf("%s: %v", res.name, res.err)
		}
	}
	if m.APIKeySet(nil) || p.APIKeySet(nil) {
		t.Fatal("nil resolver confirmed a secret it cannot read")
	}
}

func TestResolverErrorsNeverCarryTheSecretValue(t *testing.T) {
	_, m, p := secretLoaded(t, "{secret:lumencortex.sp-key}")
	cases := map[string]error{
		"not-found": func() error {
			_, err := m.ResolveAPIKey(&fakeResolver{missing: map[string]bool{"lumencortex.sp-key": true}})
			return err
		}(),
		"other-error": func() error {
			_, err := p.ResolveAPIKey(&fakeResolver{failing: map[string]bool{"lumencortex.sp-key": true}})
			return err
		}(),
		"empty-secret": func() error {
			_, err := m.ResolveAPIKey(&fakeResolver{secrets: map[string]string{"lumencortex.sp-key": ""}})
			return err
		}(),
		"nil-resolver": func() error {
			_, err := m.ResolveAPIKey(nil)
			return err
		}(),
	}
	for name, err := range cases {
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
		if strings.Contains(err.Error(), secretValue) {
			t.Errorf("%s: error leaks the secret: %v", name, err)
		}
		if !strings.Contains(err.Error(), "lumencortex.sp-key") {
			t.Errorf("%s: error does not name the reference: %v", name, err)
		}
	}
	if !errors.Is(cases["not-found"], ErrCredentialNotFound) {
		t.Errorf("not-found: %v", cases["not-found"])
	}
}

func TestSecretValueExposure(t *testing.T) {
	c, m, p := secretLoaded(t, "{secret:lumencortex.sp-key}")
	r := &fakeResolver{secrets: map[string]string{"lumencortex.sp-key": secretValue}}
	if _, err := m.ResolveAPIKey(r); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ResolveAPIKey(r); err != nil {
		t.Fatal(err)
	}
	// Everything a host can serialize: catalog, resolved model/provider, effective file, warnings.
	j, _ := json.Marshal(c.Catalog())
	blobs := []string{
		string(j), fmt.Sprintf("%+v", c.Catalog()),
		fmt.Sprintf("%+v", c.Effective()),
	}
	j, _ = json.Marshal(m)
	blobs = append(blobs, string(j), fmt.Sprintf("%+v", m))
	j, _ = json.Marshal(p)
	blobs = append(blobs, string(j), fmt.Sprintf("%+v", p))
	for _, w := range m.Warnings {
		blobs = append(blobs, w.Message)
	}
	for _, b := range blobs {
		if strings.Contains(b, secretValue) {
			t.Errorf("secret value exposed: %.200s", b)
		}
	}
	// The catalog keeps env-only knowledge: a secret ref reports no env var and no confirmation.
	info := c.Catalog()[0]
	if info.APIKeyEnv != "" || info.APIKeySet {
		t.Fatalf("catalog info=%+v", info)
	}
}

func TestSaveRoundTripsASecretReference(t *testing.T) {
	_, globalDir := env(t)
	target := filepath.Join(globalDir, FileName)
	f := File{Providers: map[string]Provider{"p": {Settings: &ProviderSettings{BaseURL: "https://p.example", APIKey: "{secret:lumencortex.sp-key}"}}}}
	if _, err := Save(target, f, ""); err != nil {
		t.Fatalf("save: %v", err)
	}
	parsed, err := ReadDocument(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.File.Providers["p"].Settings.APIKey; got != "{secret:lumencortex.sp-key}" {
		t.Fatalf("round trip=%q", got)
	}
}

func TestCredentialRedirectTreatsSecretReferencesLikeEnvReferences(t *testing.T) {
	global := &ProviderSettings{BaseURL: "https://g.example", APIKey: "{env:G_KEY}", Headers: map[string]string{"X-Old": "1"}}
	// A redirect with the workspace's own secret reference is allowed and drops the global headers.
	merged, err := MergeProviderSettings(global, ProviderSettings{BaseURL: "https://w.example", APIKey: "{secret:w-key}"}, true)
	if err != nil {
		t.Fatalf("own secret ref: %v", err)
	}
	if merged.APIKey != "{secret:w-key}" || merged.BaseURL != "https://w.example" || merged.Headers != nil {
		t.Fatalf("merged=%+v", merged)
	}
	// The same secret reference as the global one still redirects the credential.
	if _, err := MergeProviderSettings(&ProviderSettings{BaseURL: "https://g.example", APIKey: "{secret:g-key}"},
		ProviderSettings{BaseURL: "https://w.example", APIKey: "{secret:g-key}"}, true); !errors.Is(err, ErrCredentialRedirect) {
		t.Fatalf("same secret ref: %v", err)
	}
	// A redirect without any key is refused the same way.
	if _, err := MergeProviderSettings(global, ProviderSettings{BaseURL: "https://w.example"}, true); !errors.Is(err, ErrCredentialRedirect) {
		t.Fatalf("no key: %v", err)
	}
	// End to end through Load: a trusted workspace redirects with its own secret ref and the merged
	// model carries it; the entry-level path exercises the same MergeProviderSettings code.
	_, globalDir := env(t)
	ws := canon(t, t.TempDir())
	writeFile(t, filepath.Join(globalDir, FileName), fmt.Sprintf(`{"trustedWorkspaces": [%q], "providers": {"p": {"settings": {"baseURL": "https://g.example", "apiKey": "{env:G_KEY}"}, "models": {"m": {}}}}}`, ws))
	writeFile(t, filepath.Join(ws, FileName), `{"providers": {"p": {"settings": {"baseURL": "https://w.example", "apiKey": "{secret:w-key}"}}}}`)
	c, err := Load(ws)
	if err != nil {
		t.Fatal(err)
	}
	m, err := c.Resolve("p/m")
	if err != nil {
		t.Fatal(err)
	}
	if m.APIKeyRef != "{secret:w-key}" || m.BaseURL != "https://w.example" || m.Headers != nil {
		t.Fatalf("merged model=%+v", m)
	}
}
