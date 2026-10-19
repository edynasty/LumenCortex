package anthropic

import (
	"fmt"
	"net/http"

	"github.com/edynasty/LumenCortex/config"
)

// Options are the construction choices a host makes that are not part of the model's
// configuration.
type Options struct {
	// HTTPClient replaces the default client. Its Timeout is the caller's business: the factory
	// sets none, because the request and idle timeouts live in the adapter and classify as
	// availability.
	HTTPClient       *http.Client
	DisableStreaming bool
	DisableRetries   bool
	// Credentials resolves {secret:NAME} apiKey references. A nil Credentials leaves the factory
	// environment-only, the CLI's mode.
	Credentials config.CredentialResolver
}

// FromModel builds the client for one resolved catalog model. It is the only place that reads the
// API key: from the environment for {env:NAME} references and from the Credentials resolver for
// {secret:NAME} references.
//
// The model's resolved Output is the max_tokens the adapter sends when a request carries none
// (the API requires the field); with no configured limit that is config.DefaultOutput.
func FromModel(m config.ResolvedModel, o Options) (*Client, error) {
	key, err := m.ResolveAPIKey(o.Credentials)
	if err != nil {
		return nil, err
	}
	client, err := New(Config{
		Endpoint:         m.Endpoint,
		BaseURL:          m.BaseURL,
		APIKey:           key,
		Model:            m.ModelID,
		Headers:          m.Headers,
		HTTPClient:       o.HTTPClient,
		DisableStreaming: o.DisableStreaming,
		DisableRetries:   o.DisableRetries,
		MaxTokensDefault: m.Output,
		RequestTimeout:   m.RequestTimeout,
		IdleTimeout:      m.IdleTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("model %s (provider %s): %w", m.Ref, m.ProviderID, err)
	}
	return client, nil
}
