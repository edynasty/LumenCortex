package main

import (
	"fmt"
	"path/filepath"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/cognition"
)

// The curator asks the model for one JSON plan; these are the limits the Governor has always used.
const (
	curatorMaxTokens = 6000
	curatorEffort    = "high"
)

// governorCurator builds the Governor's LLM curator from the configured default model through the
// CLI's provider factory (D1). Without `--curator` the plan stays deterministic and no model is
// built or called.
func governorCurator(workspace string) (*cognition.LLMGovernorCurator, error) {
	cfg, err := config.Load(workspace)
	if err != nil {
		return nil, err
	}
	eff := cfg.Effective()
	if eff.Model == "" {
		return nil, fmt.Errorf(`--curator needs a configured model: set "model" (provider/model) in %s`,
			filepath.Join(eff.GlobalDir, config.FileName))
	}
	model, err := cfg.Resolve(eff.Model)
	if err != nil {
		return nil, err
	}
	provider, err := providerFactory(model)
	if err != nil {
		return nil, err
	}
	return &cognition.LLMGovernorCurator{Provider: provider, MaxTokens: curatorMaxTokens, ReasoningEffort: curatorEffort}, nil
}
