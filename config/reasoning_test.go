package config

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// reasoningDoc is a global document with provider p (the package given) and model m whose reasoning
// object is built from format (omitted when "") and efforts.
func reasoningDoc(pkg, format string, efforts ...string) string {
	quoted := make([]string, len(efforts))
	for i, e := range efforts {
		quoted[i] = `"` + e + `"`
	}
	reasoning := `{"efforts": [` + strings.Join(quoted, ", ") + `]}`
	if format != "" {
		reasoning = `{"format": "` + format + `", "efforts": [` + strings.Join(quoted, ", ") + `]}`
	}
	return `{"model": "p/m", "providers": {"p": {"package": "` + pkg +
		`", "settings": {"baseURL": "https://example.test"}, "models": {"m": {"reasoning": ` + reasoning + `}}}}}`
}

// loadReasoning writes doc as the global file and loads it with no workspace.
func loadReasoning(t *testing.T, doc string) (*Config, error) {
	t.Helper()
	_, globalDir := env(t)
	writeFile(t, filepath.Join(globalDir, FileName), doc)
	return Load("")
}

// A reasoning.format is accepted only by the package that implements its encoding, and adaptive
// thinking accepts only the levels it can express.
func TestReasoningFormatIsValidatedPerPackage(t *testing.T) {
	for name, doc := range map[string]string{
		"anthropic adaptive":             reasoningDoc(PackageAnthropicMessages, ReasoningAdaptive, "low", "medium", "high", "max"),
		"anthropic adaptive, no efforts": reasoningDoc(PackageAnthropicMessages, ReasoningAdaptive),
		"anthropic default, none listed": reasoningDoc(PackageAnthropicMessages, "", "none", "low"),
		"anthropic absent reasoning":     `{"model": "p/m", "providers": {"p": {"package": "anthropic-messages", "settings": {"baseURL": "https://example.test"}, "models": {"m": {}}}}}`,
		"openai reasoning-effort":        reasoningDoc(PackageOpenAICompatible, ReasoningEffortField, "low", "medium", "high"),
		"openai deepseek":                reasoningDoc(PackageOpenAICompatible, ReasoningDeepSeek, "none", "low", "high"),
		"openai none":                    reasoningDoc(PackageOpenAICompatible, ReasoningNone),
		"openai absent package":          reasoningDoc("", ReasoningObject, "max"),
		"explicit empty format":          strings.Replace(reasoningDoc(PackageAnthropicMessages, ""), `{"efforts"`, `{"format": "", "efforts"`, 1),
		"legacy package id":              reasoningDoc("@ai-sdk/openai-compatible", ReasoningEffortField, "high"),
	} {
		c, err := loadReasoning(t, doc)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if _, err := c.Resolve("p/m"); err != nil {
			t.Errorf("%s: resolve: %v", name, err)
		}
	}

	for name, c := range map[string]struct{ doc, key string }{
		"adaptive on openai-compatible": {
			reasoningDoc(PackageOpenAICompatible, ReasoningAdaptive, "low"),
			"providers.p.models.m.reasoning.format",
		},
		"deepseek on anthropic": {
			reasoningDoc(PackageAnthropicMessages, ReasoningDeepSeek),
			"providers.p.models.m.reasoning.format",
		},
		"reasoning-effort on anthropic": {
			reasoningDoc(PackageAnthropicMessages, ReasoningEffortField, "high"),
			"providers.p.models.m.reasoning.format",
		},
		// A value the schema does not list at all is refused there, as a JSON pointer.
		"unknown format": {
			reasoningDoc(PackageOpenAICompatible, "thinking", "high"),
			"/providers/p/models/m/reasoning/format",
		},
		"none in adaptive efforts": {
			reasoningDoc(PackageAnthropicMessages, ReasoningAdaptive, "none", "low"),
			"providers.p.models.m.reasoning.efforts[0]",
		},
	} {
		_, err := loadReasoning(t, c.doc)
		if !errors.Is(err, ErrInvalidConfig) || !strings.Contains(err.Error(), c.key) {
			t.Errorf("%s: err=%v, want ErrInvalidConfig naming %s", name, err, c.key)
		}
	}
	// The adaptive error says why the level cannot be kept.
	_, err := loadReasoning(t, reasoningDoc(PackageAnthropicMessages, ReasoningAdaptive, "none"))
	if err == nil || !strings.Contains(err.Error(), "adaptive thinking cannot be turned off") {
		t.Errorf("adaptive none error: %v", err)
	}
}

// loadedDoc writes global as the global file and workspace as the workspace file, then loads the
// workspace. trust maps the canonical workspace path to the global trustedWorkspaces (nil: trust
// nothing).
func loadedDoc(t *testing.T, trust func(ws string) []string, global, workspace string) (*Config, error) {
	t.Helper()
	_, globalDir := env(t)
	ws := canon(t, t.TempDir())
	doc, err := Parse([]byte(global))
	if err != nil {
		t.Fatal(err)
	}
	if trust != nil {
		doc.TrustedWorkspaces = trust(ws)
	}
	if _, err := Save(filepath.Join(globalDir, FileName), doc, ""); err != nil {
		t.Fatal(err)
	}
	if workspace != "" {
		writeFile(t, filepath.Join(ws, FileName), workspace)
	}
	return Load(ws)
}

// The format is checked on the merged catalog: a trusted workspace may change the package a global
// model's format is checked against, and an untrusted workspace's providers are dropped before the
// check, so the global format stands.
func TestReasoningFormatIsCheckedOnTheMergedCatalog(t *testing.T) {
	global := reasoningDoc(PackageAnthropicMessages, ReasoningAdaptive, "low", "high")
	wsDoc := `{"providers": {"p": {"package": "` + PackageOpenAICompatible + `"}}}`

	_, err := loadedDoc(t, trustSelf, global, wsDoc)
	if !errors.Is(err, ErrInvalidConfig) || !strings.Contains(err.Error(), "providers.p.models.m.reasoning.format") {
		t.Fatalf("trusted package change: err=%v", err)
	}

	c, err := loadedDoc(t, nil, global, wsDoc)
	if err != nil {
		t.Fatalf("untrusted package change: %v", err)
	}
	model, err := c.Resolve("p/m")
	if err != nil {
		t.Fatal(err)
	}
	if model.Package != PackageAnthropicMessages || model.ReasoningFormat != ReasoningAdaptive {
		t.Fatalf("package=%q format=%q, want the global anthropic/adaptive", model.Package, model.ReasoningFormat)
	}
}

// A model that declares no format resolves to its package's default, and ReasoningFormats lists
// exactly the values its package accepts, the default first.
func TestReasoningFormatsPerPackage(t *testing.T) {
	want := map[string][]string{
		PackageOpenAICompatible:     {"", ReasoningNone, ReasoningObject, ReasoningEffortField, ReasoningDeepSeek},
		"":                          {"", ReasoningNone, ReasoningObject, ReasoningEffortField, ReasoningDeepSeek},
		"@ai-sdk/openai-compatible": {"", ReasoningNone, ReasoningObject, ReasoningEffortField, ReasoningDeepSeek},
		PackageAnthropicMessages:    {"", ReasoningAdaptive},
	}
	for pkg, wantFormats := range want {
		if got := ReasoningFormats(pkg); !slices.Equal(got, wantFormats) {
			t.Errorf("ReasoningFormats(%q)=%v want %v", pkg, got, wantFormats)
		}
	}
	if got := ReasoningFormats("grpc"); got != nil {
		t.Errorf("unknown package: %v", got)
	}
	// The caller owns the copy.
	first := ReasoningFormats(PackageAnthropicMessages)
	first[0] = "mutated"
	if ReasoningFormats(PackageAnthropicMessages)[0] != "" {
		t.Fatal("ReasoningFormats returned its own slice")
	}

	for _, c := range []struct {
		doc  string
		want string
	}{
		{reasoningDoc(PackageAnthropicMessages, "", "low"), ""},
		{reasoningDoc(PackageAnthropicMessages, ReasoningAdaptive, "low"), ReasoningAdaptive},
		{reasoningDoc(PackageOpenAICompatible, "", "low"), ReasoningNone},
		{reasoningDoc(PackageOpenAICompatible, ReasoningDeepSeek, "low"), ReasoningDeepSeek},
	} {
		cfg, err := loadReasoning(t, c.doc)
		if err != nil {
			t.Fatalf("%s: %v", c.doc, err)
		}
		m, err := cfg.Resolve("p/m")
		if err != nil {
			t.Fatal(err)
		}
		if m.ReasoningFormat != c.want {
			t.Errorf("package %q format %q: resolved %q, want %q", m.Package, c.doc, m.ReasoningFormat, c.want)
		}
	}
}
