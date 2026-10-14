package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/provider/openai"
)

// removedProviderEnv maps the provider and model environment variables the CLI used before M2.2 to
// the configuration key that replaces each (design §8.3). They are not read any more.
var removedProviderEnv = []struct{ name, replacement string }{
	{"LCX_MODEL", `"model": "<provider>/<key>" and providers.<provider>.models.<key>.modelID`},
	{"LCX_ENDPOINT", `providers.<provider>.settings.endpoint`},
	{"LCX_BASE_URL", `providers.<provider>.settings.baseURL`},
	{"LCX_API_KEY", `providers.<provider>.settings.apiKey, set to "{env:NAME}" naming the variable that holds the key`},
	{"LCX_REASONING_FORMAT", `providers.<provider>.models.<key>.reasoning.format`},
}

// providerFromConfig resolves the session model from the unified configuration (global file,
// then <workspace>/lumencortex.json under the trust rules) and builds its client. Warnings go to
// warn: dropped workspace entries with the trustedWorkspaces entry that would enable them, legacy
// global files, and removed environment variables with their replacement keys.
//
// The API key is read from its {env:NAME} variable here and goes only into the client.
func providerFromConfig(workspace string, warn io.Writer) (*openai.Client, config.ResolvedModel, error) {
	var set []string
	for _, v := range removedProviderEnv {
		if os.Getenv(v.name) != "" {
			set = append(set, v.name)
			fmt.Fprintf(warn, "lcx-go: warning: %s is no longer read; configure %s\n", v.name, v.replacement)
		}
	}
	cfg, err := config.Load(workspace)
	if err != nil {
		return nil, config.ResolvedModel{}, err
	}
	eff := cfg.Effective()
	for _, w := range eff.Warnings {
		fmt.Fprintf(warn, "lcx-go: warning: %s\n", w.Message)
	}
	if eff.Model == "" {
		msg := fmt.Sprintf(`no model is configured: set "model" (provider/model) and the provider in %s`,
			filepath.Join(eff.GlobalDir, config.FileName))
		if len(set) > 0 {
			msg += fmt.Sprintf("; the environment variables %v are no longer read", set)
		}
		return nil, config.ResolvedModel{}, fmt.Errorf("%s", msg)
	}
	model, err := cfg.Resolve(eff.Model)
	if err != nil {
		return nil, config.ResolvedModel{}, err
	}
	for _, w := range model.Warnings {
		fmt.Fprintf(warn, "lcx-go: warning: %s\n", w.Message)
	}
	client, err := newProvider(model)
	if err != nil {
		return nil, config.ResolvedModel{}, err
	}
	return client, model, nil
}

// newProvider builds the OpenAI-compatible client for one resolved model. It is the only place
// that resolves the API key from the environment.
func newProvider(m config.ResolvedModel) (*openai.Client, error) {
	key, err := m.ResolveAPIKey()
	if err != nil {
		return nil, err
	}
	var httpClient *http.Client
	if m.RequestTimeout > 0 {
		httpClient = &http.Client{Timeout: m.RequestTimeout}
	}
	return openai.New(openai.Config{
		Endpoint:         m.Endpoint,
		BaseURL:          m.BaseURL,
		APIKey:           key,
		Model:            m.ModelID,
		Headers:          m.Headers,
		HTTPClient:       httpClient,
		ReasoningFormat:  m.ReasoningFormat,
		DisableStreaming: envBool("LCX_DISABLE_STREAMING"),
		DisableRetries:   envBool("LCX_DISABLE_RETRIES"),
	})
}
