package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/edynasty/LumenCortex/internal/cognition"
	"github.com/edynasty/LumenCortex/protocol"
)

type ProviderBinding struct {
	Name     string            `json:"name,omitempty"`
	Provider protocol.Provider `json:"-"`
}

type providerAttempt struct {
	Response protocol.ProviderResponse
	Binding  ProviderBinding
	Index    int
	Elapsed  time.Duration
}

func (l *Loop) providerCandidates(category string) []ProviderBinding {
	chain := l.ProviderChains[category]
	usable := make([]ProviderBinding, 0, len(chain))
	for _, binding := range chain {
		if binding.Provider == nil {
			continue
		}
		if strings.TrimSpace(binding.Name) == "" {
			binding.Name = "current"
		}
		if l.ProviderHealth != nil && !l.ProviderHealth.Available(providerHealthKey(binding)) {
			continue
		}
		usable = append(usable, binding)
	}
	if len(usable) > 0 {
		return usable
	}

	fallback := ProviderBinding{Name: l.ProviderName, Provider: l.Provider}
	if strings.TrimSpace(fallback.Name) == "" {
		fallback.Name = "current"
	}
	if fallback.Provider == nil {
		return nil
	}
	if l.ProviderHealth != nil && !l.ProviderHealth.Available(providerHealthKey(fallback)) {
		return nil
	}
	return []ProviderBinding{fallback}
}

func (l *Loop) completeWithProviderChain(
	ctx context.Context,
	sessionID string,
	step int64,
	category string,
	req protocol.ProviderRequest,
	messageCount int,
	tools []protocol.ToolSpec,
) (providerAttempt, error) {
	candidates := l.providerCandidates(category)
	if len(candidates) == 0 {
		return providerAttempt{}, errors.New("no generative provider is available for the selected category")
	}

	var lastErr error
	for index, binding := range candidates {
		started := time.Now()
		l.emit("llm.request", sessionID, map[string]any{
			"step": step,
			"category": category,
			"provider": binding.Name,
			"model": binding.Provider.Model(),
			"messages": messageCount,
			"tools": toolNames(tools),
			"reasoningEffort": req.ReasoningEffort,
			"providerIndex": index,
		})

		response, err := binding.Provider.Complete(ctx, req)
		elapsed := time.Since(started)
		if err == nil {
			l.ProviderHealth?.Success(providerHealthKey(binding))
			l.emit("llm.complete", sessionID, map[string]any{
				"step": step,
				"category": category,
				"provider": binding.Name,
				"model": binding.Provider.Model(),
				"elapsedMs": elapsed.Milliseconds(),
				"providerIndex": index,
			})
			return providerAttempt{
				Response: response,
				Binding: binding,
				Index: index,
				Elapsed: elapsed,
			}, nil
		}

		lastErr = err
		if ctx.Err() != nil {
			return providerAttempt{}, ctx.Err()
		}
		l.ProviderHealth?.Failure(providerHealthKey(binding), err)
		next := ""
		nextModel := ""
		if index+1 < len(candidates) {
			next = candidates[index+1].Name
			nextModel = candidates[index+1].Provider.Model()
		}
		l.emit("provider.failover", sessionID, map[string]any{
			"step": step,
			"category": category,
			"provider": binding.Name,
			"model": binding.Provider.Model(),
			"error": err.Error(),
			"elapsedMs": elapsed.Milliseconds(),
			"nextProvider": next,
			"nextModel": nextModel,
		})
	}
	if lastErr != nil {
		return providerAttempt{}, lastErr
	}
	return providerAttempt{}, fmt.Errorf("category provider chain %q failed", category)
}

func providerHealthKey(binding ProviderBinding) string {
	name := strings.TrimSpace(binding.Name)
	if name == "" {
		name = "current"
	}
	model := "default"
	if binding.Provider != nil && strings.TrimSpace(binding.Provider.Model()) != "" {
		model = binding.Provider.Model()
	}
	return "model:" + name + ":" + model
}
