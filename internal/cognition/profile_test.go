package cognition

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigMergesBuiltinsAndParsesSharedProfile(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".lumencortex")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "cognition.json")
	raw := []byte(`{
		"decision": {
			"policy": "all",
			"providers": [
				{"type":"laya","baseURL":"http://127.0.0.1:8000","timeoutMs":1200},
				"jev"
			]
		},
		"categories": {
			"general": {
				"default": true,
				"models": [
					{"provider":"openrouter","model":"openrouter/free"}
				]
			},
			"deep": {
				"default": true,
				"models": [
					"groq:openai/gpt-oss-120b",
					{"provider":"deepseek","model":"deepseek-flash","timeoutMs":9000}
				]
			},
			"custom": {
				"description":"custom work",
				"models":["generic:local-model"]
			}
		},
		"telemetry":{"enabled":false},
		"health":{"failureThreshold":5,"cooldownMs":45000},
		"retrieval":{"embeddings":{
			"enabled":true,
			"provider":"generic",
			"model":"embed-local",
			"baseURL":"http://127.0.0.1:11434/v1",
			"batchSize":7,
			"candidateLimit":80,
			"rrfK":42,
			"lexicalWeight":0,
			"semanticWeight":1.4
		}},
		"governor":{"enabled":true,"provider":"openrouter","model":"governor-model"}
	}`)
	if err := os.WriteFile(file, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	config, err := LoadConfig(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if config.Source != file {
		t.Fatalf("source=%q", config.Source)
	}
	if config.Decision.Policy != "all" || len(config.Decision.Providers) != 2 {
		t.Fatalf("decision=%#v", config.Decision)
	}
	if config.Decision.Providers[1].Type != "jev" {
		t.Fatalf("decision provider=%#v", config.Decision.Providers[1])
	}
	if config.Telemetry {
		t.Fatal("expected telemetry disabled")
	}
	if config.Health.FailureThreshold != 5 || config.Health.CooldownMS != 45000 {
		t.Fatalf("health=%#v", config.Health)
	}
	if !config.Categories["general"].Default || config.Categories["deep"].Default {
		t.Fatalf("default categories general=%v deep=%v", config.Categories["general"].Default, config.Categories["deep"].Default)
	}
	if config.Categories["writing"].Description == "" {
		t.Fatal("built-in writing category should remain")
	}
	deep := config.Categories["deep"].Models
	if len(deep) != 2 {
		t.Fatalf("deep models=%#v", deep)
	}
	if deep[0].Provider != "groq" || deep[0].Model != "openai/gpt-oss-120b" {
		t.Fatalf("string model spec=%#v", deep[0])
	}
	if deep[1].Provider != "deepseek" || deep[1].TimeoutMS != 9000 {
		t.Fatalf("object model spec=%#v", deep[1])
	}
	custom := config.Categories["custom"].Models
	if len(custom) != 1 || custom[0].Provider != "generic" || custom[0].Model != "local-model" {
		t.Fatalf("custom models=%#v", custom)
	}
	if config.Governor == nil || !config.Governor.Enabled || config.Governor.Model != "governor-model" {
		t.Fatalf("governor=%#v", config.Governor)
	}
	if config.Retrieval.Embeddings == nil || !config.Retrieval.Embeddings.Enabled {
		t.Fatalf("embeddings=%#v", config.Retrieval.Embeddings)
	}
	if config.Retrieval.Embeddings.Model != "embed-local" ||
		config.Retrieval.Embeddings.BatchSize != 7 ||
		config.Retrieval.Embeddings.CandidateLimit != 80 ||
		config.Retrieval.Embeddings.RRFK != 42 {
		t.Fatalf("embeddings=%#v", config.Retrieval.Embeddings)
	}
	if config.Retrieval.Embeddings.LexicalWeight != 0 ||
		config.Retrieval.Embeddings.SemanticWeight != 1.4 {
		t.Fatalf("embedding weights=%#v", config.Retrieval.Embeddings)
	}
}

func TestLoadConfigFallsBackToBuiltinsWhenFileMissing(t *testing.T) {
	config, err := LoadConfig(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if config.Source != "built-in" {
		t.Fatalf("source=%q", config.Source)
	}
	if !config.Categories["general"].Default {
		t.Fatal("general category must remain default")
	}
	if config.Health.FailureThreshold != 3 || config.Health.CooldownMS != 30000 {
		t.Fatalf("health=%#v", config.Health)
	}
}
