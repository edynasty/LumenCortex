package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/protocol"
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

// providerFactory is the Engine's ProviderFactory for the CLI: the OpenAI-compatible adapter, with
// the two debugging switches the CLI has always offered. It is the only place that reads an API
// key from the environment (through ResolvedModel.ResolveAPIKey).
func providerFactory(m config.ResolvedModel) (protocol.Provider, error) {
	return openai.FromModel(m, openai.Options{
		DisableStreaming: envBool("LCX_DISABLE_STREAMING"),
		DisableRetries:   envBool("LCX_DISABLE_RETRIES"),
	})
}

// preflight reads the configuration the way the Engine will at Start, prints its warnings to warn
// (dropped workspace entries with the trustedWorkspaces entry that would enable them, legacy
// files, removed environment variables with their replacement keys, a leftover cognition.json),
// and, when newSession is true, checks that the configured default model can start: it is set, in
// the catalog, tool-capable, and its client can be built. Nothing is sent and nothing is written,
// so a refusal leaves no session behind. A resumed session runs on its pinned model, which only
// the Engine can read, so resume skips the model check and the Engine refuses at Start.
func preflight(workspace string, warn io.Writer, newSession bool) error {
	var set []string
	for _, v := range removedProviderEnv {
		if os.Getenv(v.name) != "" {
			set = append(set, v.name)
			fmt.Fprintf(warn, "lcx-go: warning: %s is no longer read; configure %s\n", v.name, v.replacement)
		}
	}
	cfg, err := config.Load(workspace)
	if err != nil {
		return err
	}
	eff := cfg.Effective()
	for _, w := range eff.Warnings {
		fmt.Fprintf(warn, "lcx-go: warning: %s\n", w.Message)
	}
	if !newSession {
		return nil
	}
	if eff.Model == "" {
		msg := fmt.Sprintf(`no model is configured: set "model" (provider/model) and the provider in %s`,
			filepath.Join(eff.GlobalDir, config.FileName))
		if len(set) > 0 {
			msg += fmt.Sprintf("; the environment variables %v are no longer read", set)
		}
		return fmt.Errorf("%s", msg)
	}
	model, err := cfg.Resolve(eff.Model)
	if err != nil {
		return err
	}
	for _, w := range model.Warnings {
		fmt.Fprintf(warn, "lcx-go: warning: %s\n", w.Message)
	}
	if !model.ToolCalling {
		return fmt.Errorf("model %s does not support tool calling", model.Ref)
	}
	_, err = providerFactory(model)
	return err
}
