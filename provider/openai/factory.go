package openai

import (
	"fmt"
	"net/http"

	"github.com/edynasty/LumenCortex/config"
)

// Options are the construction choices a host makes that are not part of the model's configuration.
type Options struct {
	// HTTPClient replaces the default client. Its Timeout is the caller's business: the factory sets
	// none, because the request and idle timeouts live in the adapter and classify as availability.
	HTTPClient       *http.Client
	DisableStreaming bool
	DisableRetries   bool
}

// FromModel builds the client for one resolved catalog model. It is the only place that reads the
// API key from the environment.
func FromModel(m config.ResolvedModel, o Options) (*Client, error) {
	key, err := m.ResolveAPIKey()
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
		ReasoningFormat:  m.ReasoningFormat,
		ReasoningEcho:    m.ReasoningEcho,
		DisableStreaming: o.DisableStreaming,
		DisableRetries:   o.DisableRetries,
		RequestTimeout:   m.RequestTimeout,
		IdleTimeout:      m.IdleTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("model %s (provider %s): %w", m.Ref, m.ProviderID, err)
	}
	return client, nil
}
