package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/docs"
)

// S2: every reference example is valid and every bad case fails at the reference keyword and path.
func TestSchemaValidFixtures(t *testing.T) {
	files, _ := filepath.Glob("testdata/valid/*.json")
	if len(files) != 6 {
		t.Fatalf("valid fixtures=%v", files)
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := Parse(raw)
		if err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		// The typed document must carry everything the file says: re-encoding it validates and
		// equals the original structurally.
		if err := Validate(parsed); err != nil {
			t.Errorf("%s: typed document invalid: %v", f, err)
		}
		again, err := json.Marshal(parsed)
		if err != nil {
			t.Fatal(err)
		}
		if !jsonEqual(t, raw, again) {
			t.Errorf("%s: typed round trip lost data\n got %s", f, again)
		}
	}
}

func TestSchemaInvalidFixtures(t *testing.T) {
	// Keyword and JSON pointer of the violation, as in the reference validate.mjs expectations.
	// For propertyNames the pattern failure sits at the offending property (ajv reports its parent
	// plus a propertyNames error; this validator reports the property).
	want := map[string][2]string{
		"literal-api-key":             {"pattern", "/providers/p/settings/apiKey"},
		"secret-ref-bad-shape":        {"pattern", "/providers/p/settings/apiKey"},
		"model-ref-no-slash":          {"pattern", "/model"},
		"unknown-root-key":            {"additionalProperties", ""},
		"baseurl-camelcase":           {"additionalProperties", "/providers/p/settings"},
		"check-test-without-command":  {"required", "/checks/unit"},
		"effort-unknown":              {"enum", "/effort/default"},
		"provider-id-with-slash":      {"pattern", "/providers/a~1b"},
		"failover-duplicate":          {"uniqueItems", "/failover"},
		"header-non-string":           {"type", "/providers/p/settings/headers/X-Retries"},
		"nested-model-key-with-slash": {"pattern", "/providers/p/models/org~1model"},
		"escalation-without-default":  {"dependentRequired", "/effort"},
		"escalation-incomplete":       {"required", "/effort/escalation"},
		"package-unknown":             {"enum", "/providers/p/package"},
		"auto-max-unknown":            {"enum", "/effort/auto/max"},
		"auto-timeout-out-of-range":   {"maximum", "/effort/auto/timeoutSeconds"},
		"auto-model-no-slash":         {"pattern", "/effort/auto/model"},
		"agent-id-pattern":            {"pattern", "/agents/Bad_ID"},
		"agent-models-empty":          {"minItems", "/agents/x/models"},
		"agent-use-unknown":           {"enum", "/agents/x/use"},
		"agent-unknown-field":         {"additionalProperties", "/agents/x"},
	}
	files, _ := filepath.Glob("testdata/invalid/*.json")
	if len(files) != len(want) {
		t.Fatalf("invalid fixtures=%d want %d", len(files), len(want))
	}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".json")
		exp, ok := want[name]
		if !ok {
			t.Errorf("%s: no expectation", name)
			continue
		}
		raw, _ := os.ReadFile(f)
		_, err := Parse(raw)
		if !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("%s: err=%v want ErrInvalidConfig", name, err)
			continue
		}
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Errorf("%s: not a ValidationError: %v", name, err)
			continue
		}
		found := false
		for _, v := range ve.Violations {
			if v.Keyword == exp[0] && v.Path == exp[1] {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: want %s at %q, got %+v", name, exp[0], exp[1], ve.Violations)
		}
	}
}

func TestSchemaFileIsTheEmbeddedOne(t *testing.T) {
	// docs/lumencortex.schema.json is the file Desktop copies; the embedded bytes are the file.
	disk, err := os.ReadFile(filepath.Join("..", "docs", "lumencortex.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(disk, docs.ConfigSchema) {
		t.Fatal("embedded schema differs from docs/lumencortex.schema.json")
	}
}

// Unknown keys are errors at every object level, not only the root.
func TestUnknownKeysRejectedAtEveryLevel(t *testing.T) {
	cases := map[string]string{
		"root":        `{"models": {}}`,
		"effort":      `{"effort": {"default": "low", "extra": 1}}`,
		"effort-auto": `{"effort": {"auto": {"model": "a/b", "budget": 1}}}`,
		"escalation":  `{"effort": {"default": "low", "escalation": {"sameFailure": 1, "noProgressSteps": 1, "max": "high", "x": 1}}}`,
		"provider":    `{"providers": {"p": {"endpoint": "https://x"}}}`,
		"settings":    `{"providers": {"p": {"settings": {"apikey": "{env:K}"}}}}`,
		"model":       `{"providers": {"p": {"models": {"m": {"id": "x"}}}}}`,
		"limit":       `{"providers": {"p": {"models": {"m": {"limit": {"window": 1}}}}}}`,
		"reasoning":   `{"providers": {"p": {"models": {"m": {"reasoning": {"mode": "x"}}}}}}`,
		"modalities":  `{"providers": {"p": {"models": {"m": {"modalities": {"output": ["text"]}}}}}}`,
		"check":       `{"checks": {"c": {"kind": "test", "command": "x", "cwd": "."}}}`,
		"trailing":    `{} {}`,
		"not-object":  `[]`,
		"empty":       ``,
		"bad-package": `{"providers": {"p": {"package": "anthropic"}}}`,
	}
	for name, doc := range cases {
		if _, err := Parse([]byte(doc)); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("%s: err=%v want ErrInvalidConfig", name, err)
		}
	}
}

// Go-side and schema-side validation enforce the same constraints: whatever Validate (typed, used
// by Save) accepts, Parse (schema on bytes) accepts, and vice versa, for the security-relevant
// fields.
func TestGoValidationMatchesSchema(t *testing.T) {
	bad := []File{
		{Providers: map[string]Provider{"p": {Settings: &ProviderSettings{APIKey: "sk-live-123"}}}},
		{Providers: map[string]Provider{"p": {Settings: &ProviderSettings{APIKey: "{env:MY-KEY}"}}}},
		{Providers: map[string]Provider{"p": {Settings: &ProviderSettings{APIKey: "$OPENAI_API_KEY"}}}},
		{Providers: map[string]Provider{"p": {Settings: &ProviderSettings{BaseURL: "api.example.com/v1"}}}},
		{Providers: map[string]Provider{"p": {Settings: &ProviderSettings{Endpoint: "file:///etc/passwd"}}}},
		{Providers: map[string]Provider{"a/b": {}}},
		{Providers: map[string]Provider{"my provider": {}}},
		{Model: "a/b/c"},
		{Model: "noslash"},
		{Failover: []ModelRef{"a/b", "a/b"}},
		{TrustedWorkspaces: []string{"relative/path"}},
		{Checks: map[string]Check{"c": {Kind: CheckTest}}},
		{Checks: map[string]Check{"C": {Kind: CheckTest, Command: "x"}}},
		{Effort: &EffortPolicy{Escalation: &Escalation{SameFailure: 1, NoProgressSteps: 1, Max: EffortHigh}}},
		{Effort: &EffortPolicy{Default: "extreme"}},
		{Effort: &EffortPolicy{Auto: &AutoEffort{Max: "ultra"}}},
		{Effort: &EffortPolicy{Auto: &AutoEffort{TimeoutSeconds: 61}}},
		{Effort: &EffortPolicy{Auto: &AutoEffort{Model: "noslash"}}},
	}
	for i, f := range bad {
		verr := Validate(f)
		raw, _ := json.Marshal(f)
		_, perr := Parse(raw)
		if !errors.Is(verr, ErrInvalidConfig) || !errors.Is(perr, ErrInvalidConfig) {
			t.Errorf("case %d (%s): Validate=%v Parse=%v", i, raw, verr, perr)
		}
	}
}

// A validation error never repeats a value from the document: a literal key must not reach a log.
func TestValidationErrorsDoNotLeakValues(t *testing.T) {
	const secret = "sk-live-SUPERSECRET-123"
	docs := []string{
		`{"providers":{"p":{"settings":{"apiKey":"` + secret + `"}}}}`,
		`{"providers":{"p":{"settings":{"headers":{"Authorization":42},"apiKey":"` + secret + `"}}}}`,
		`{"providers":{"p":{"settings":{"apiKey":"` + secret + `"}}},"x`,
		`{"providers":{"p":{"settings":{"timeoutSeconds":"` + secret + `"}}}}`,
	}
	for _, d := range docs {
		_, err := Parse([]byte(d))
		if err == nil {
			t.Fatalf("accepted %s", d)
		}
		if strings.Contains(err.Error(), "SUPERSECRET") {
			t.Errorf("error leaks the value: %v", err)
		}
	}
}

func TestParseToleratesBOM(t *testing.T) {
	f, err := Parse(append([]byte("\xef\xbb\xbf"), `{"model":"a/b"}`...))
	if err != nil || f.Model != "a/b" {
		t.Fatalf("model=%q err=%v", f.Model, err)
	}
}

func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatal(err)
	}
	xa, _ := json.Marshal(x)
	yb, _ := json.Marshal(y)
	return bytes.Equal(xa, yb)
}
