package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/edynasty/LumenCortex/internal/cognition"
)

func TestCategoryAndDecisionProvidersFromSharedConfig(t *testing.T) {
	config := cognition.Config{
		Decision: cognition.DecisionConfig{
			Policy: "first",
			Providers: []cognition.DecisionProviderSpec{
				{Type: "laya", BaseURL: "http://127.0.0.1:8000"},
				{Type: "jev"},
			},
		},
		Categories: map[string]cognition.CategoryConfig{
			"deep": {
				Models: []cognition.ModelSpec{
					{Provider: "groq", Model: "openai/gpt-oss-120b"},
					{Provider: "deepseek", Model: "deepseek-flash"},
				},
			},
		},
	}
	chains, err := categoryProvidersFromConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	if len(chains["deep"]) != 2 {
		t.Fatalf("deep chain=%#v", chains["deep"])
	}
	if chains["deep"][0].Name != "groq" || chains["deep"][0].Provider.Model() != "openai/gpt-oss-120b" {
		t.Fatalf("first binding=%#v model=%q", chains["deep"][0], chains["deep"][0].Provider.Model())
	}
	if chains["deep"][1].Name != "deepseek" || chains["deep"][1].Provider.Model() != "deepseek-flash" {
		t.Fatalf("second binding=%#v model=%q", chains["deep"][1], chains["deep"][1].Provider.Model())
	}

	decisions, err := decisionProvidersFromConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 2 {
		t.Fatalf("decision providers=%#v", decisions)
	}
	if decisions[0].Name() != "laya" || decisions[1].Name() != "jev" || decisions[1].Model() != "jev-latest" {
		t.Fatalf("decision providers laya=%s jev=%s/%s", decisions[0].Name(), decisions[1].Name(), decisions[1].Model())
	}
}

func TestAgentOptionsAutoEnableCognitionFromWorkspaceProfile(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".lumencortex")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{
		"decision":{
			"providers":[{"type":"laya","baseURL":"http://127.0.0.1:8000"}]
		},
		"categories":{
			"general":{
				"default":true,
				"models":[{"provider":"generic","model":"local-fast","baseURL":"http://127.0.0.1:11434/v1"}]
			}
		},
		"health":{"failureThreshold":4,"cooldownMs":12000}
	}`)
	if err := os.WriteFile(filepath.Join(dir, "cognition.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("LCX_MODEL", "fallback-model")
	t.Setenv("LCX_BASE_URL", "http://127.0.0.1:11434/v1")
	t.Setenv("LCX_COGNITION", "")
	t.Setenv("LCX_COGNITION_PROFILE", "")
	t.Setenv("LCX_WORKFLOW", "")
	t.Setenv("LCX_WORK_UNITS", "")

	opts, err := agentOptionsFromEnv(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !opts.CognitionEnabled {
		t.Fatal("workspace cognition profile should enable cognitive routing")
	}
	if len(opts.CategoryProviders["general"]) != 1 {
		t.Fatalf("general providers=%#v", opts.CategoryProviders["general"])
	}
	if opts.CategoryProviders["general"][0].Provider.Model() != "local-fast" {
		t.Fatalf("model=%q", opts.CategoryProviders["general"][0].Provider.Model())
	}
	if len(opts.DecisionProviders) != 1 || opts.DecisionProviders[0].Name() != "laya" {
		t.Fatalf("decision providers=%#v", opts.DecisionProviders)
	}
}
