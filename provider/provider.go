// Package provider selects the wire adapter of one resolved catalog model by its provider's
// package: openai-compatible (the legacy package ids included) builds the chat-completions
// adapter, anthropic-messages builds the Anthropic Messages adapter. It is the runtime's default
// ProviderFactory, so a host that injects no factory gets every supported protocol.
package provider

import (
	"fmt"
	"net/http"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/protocol"
	"github.com/edynasty/LumenCortex/provider/anthropic"
	"github.com/edynasty/LumenCortex/provider/openai"
)

// Options are the construction choices a host makes that are not part of the model's
// configuration. They are forwarded unchanged to the adapter the package selects.
type Options struct {
	// HTTPClient replaces the default client of the selected adapter. Its Timeout is the caller's
	// business: the adapters classify their own request and idle timeouts.
	HTTPClient       *http.Client
	DisableStreaming bool
	DisableRetries   bool
	// Credentials resolves {secret:NAME} apiKey references. A nil Credentials leaves every adapter
	// environment-only, the CLI's mode; a host with a secret store passes its resolver here.
	Credentials config.CredentialResolver
}

// FromModel builds the provider client of one resolved catalog model, selecting the adapter by the
// provider's package. An empty package selects the chat-completions adapter, which is what every
// configuration without a package means.
func FromModel(m config.ResolvedModel, o Options) (protocol.Provider, error) {
	switch m.Package {
	case "", config.PackageOpenAICompatible:
		return openai.FromModel(m, openai.Options{
			HTTPClient:       o.HTTPClient,
			DisableStreaming: o.DisableStreaming,
			DisableRetries:   o.DisableRetries,
			Credentials:      o.Credentials,
		})
	case config.PackageAnthropicMessages:
		return anthropic.FromModel(m, anthropic.Options{
			HTTPClient:       o.HTTPClient,
			DisableStreaming: o.DisableStreaming,
			DisableRetries:   o.DisableRetries,
			Credentials:      o.Credentials,
		})
	default:
		return nil, fmt.Errorf("model %s (provider %s): package %q is not one of %s, %s",
			m.Ref, m.ProviderID, m.Package, config.PackageOpenAICompatible, config.PackageAnthropicMessages)
	}
}
