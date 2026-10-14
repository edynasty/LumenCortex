package config

import (
	"fmt"
	"os"
	"slices"
	"time"
)

// Defaults the schema documents for absent model settings.
const (
	DefaultContext     = 32768
	DefaultOutput      = 4096
	DefaultIdleTimeout = 120 * time.Second
)

// ResolvedModel is the settings for one catalog model with every default applied. It holds the
// name of the API key's environment variable, never the key: ResolveAPIKey reads the variable when
// a provider client is built.
type ResolvedModel struct {
	Ref          ModelRef
	ProviderID   string
	ProviderName string
	ModelKey     string
	ModelName    string
	// ModelID is the upstream id sent in the request's model field.
	ModelID  string
	BaseURL  string
	Endpoint string
	// APIKeyRef is the configured {env:NAME} reference, "" when the provider needs no key.
	APIKeyRef string
	// Headers are sent with every request. They are configuration, not a secret store, but may
	// carry tokens, so they are kept out of JSON.
	Headers         map[string]string `json:"-"`
	Context, Output int
	ReasoningFormat string
	ReasoningEcho   string
	Efforts         []Effort
	ToolCalling     bool
	InputModalities []string
	// RequestTimeout is 0 for no whole-request cap.
	RequestTimeout time.Duration
	IdleTimeout    time.Duration
	Warnings       []Warning
	// OutputSet reports that limit.output is configured. When false, Output is DefaultOutput and
	// the runtime reserves it but sends no max_tokens (D4).
	OutputSet bool
}

// ResolvedProvider is the connection settings of one catalog provider after merge and trust. Like
// ResolvedModel it holds the name of the key's environment variable, never the key.
type ResolvedProvider struct {
	ID, Name string
	BaseURL  string
	Endpoint string
	// APIKeyRef is the configured {env:NAME} reference, "" when the provider needs no key.
	APIKeyRef string
	// Headers are sent with every request; they may carry tokens and stay out of JSON.
	Headers        map[string]string `json:"-"`
	RequestTimeout time.Duration
	IdleTimeout    time.Duration
}

// APIKeyEnv returns the environment variable that holds the key, "" when there is none.
func (p ResolvedProvider) APIKeyEnv() string {
	name, _ := EnvName(p.APIKeyRef)
	return name
}

// APIKeySet reports whether the key's variable is set and non-empty.
func (p ResolvedProvider) APIKeySet() bool {
	name := p.APIKeyEnv()
	return name != "" && os.Getenv(name) != ""
}

// ResolveAPIKey reads the key from the environment, with the semantics of ResolvedModel.ResolveAPIKey.
func (p ResolvedProvider) ResolveAPIKey() (string, error) {
	return resolveAPIKey(p.ID, "provider "+p.ID, p.APIKeyRef)
}

func resolveAPIKey(providerID, subject, ref string) (string, error) {
	if ref == "" {
		return "", nil
	}
	name, ok := EnvName(ref)
	if !ok {
		return "", fmt.Errorf("%w: apiKey of %s is not an {env:NAME} reference", ErrInvalidConfig, subject)
	}
	v := os.Getenv(name)
	if v == "" {
		return "", fmt.Errorf("provider %s: environment variable %s is not set", providerID, name)
	}
	return v, nil
}

// APIKeyEnv returns the environment variable that holds the key, "" when there is none.
func (m ResolvedModel) APIKeyEnv() string {
	name, _ := EnvName(m.APIKeyRef)
	return name
}

// APIKeySet reports whether the key's variable is set and non-empty; it is what hosts may show.
func (m ResolvedModel) APIKeySet() bool {
	name := m.APIKeyEnv()
	return name != "" && os.Getenv(name) != ""
}

// ResolveAPIKey reads the key from the environment. Call it only while building a provider client
// and keep the result out of logs, errors and events. It returns "" without error when the
// provider has no apiKey, and an error naming only the variable when it is unset.
func (m ResolvedModel) ResolveAPIKey() (string, error) {
	return resolveAPIKey(m.ProviderID, string(m.Ref), m.APIKeyRef)
}

// ModelInfo describes a catalog model for pickers. It carries no credential and no header.
type ModelInfo struct {
	Ref             ModelRef `json:"ref"`
	ProviderName    string   `json:"providerName"`
	ModelName       string   `json:"modelName"`
	ModelID         string   `json:"modelID"`
	Context         int      `json:"context"`
	Output          int      `json:"output"`
	ToolCalling     bool     `json:"toolCalling"`
	InputModalities []string `json:"inputModalities"`
	ReasoningFormat string   `json:"reasoningFormat"`
	Efforts         []Effort `json:"efforts,omitempty"`
	// APIKeyEnv names the variable; APIKeySet tells whether it is set. Never its value.
	APIKeyEnv string `json:"apiKeyEnv,omitempty"`
	APIKeySet bool   `json:"apiKeySet"`
}

// Catalog lists every model of the merged provider catalog, sorted by reference.
func (c *Config) Catalog() []ModelInfo {
	var out []ModelInfo
	for _, pid := range sortedKeys(c.providers) {
		for _, key := range sortedKeys(c.providers[pid].Models) {
			m, err := c.Resolve(ModelRef(pid + "/" + key))
			if err != nil {
				continue
			}
			out = append(out, ModelInfo{
				Ref: m.Ref, ProviderName: m.ProviderName, ModelName: m.ModelName, ModelID: m.ModelID,
				Context: m.Context, Output: m.Output, ToolCalling: m.ToolCalling,
				InputModalities: m.InputModalities, ReasoningFormat: m.ReasoningFormat, Efforts: m.Efforts,
				APIKeyEnv: m.APIKeyEnv(), APIKeySet: m.APIKeySet(),
			})
		}
	}
	return out
}

// Provider returns the settings of one catalog provider, or ErrUnknownProvider. It reads the merged
// catalog, so a workspace override that the trust rules dropped is not visible here.
func (c *Config) Provider(id string) (ResolvedProvider, error) {
	p, ok := c.providers[id]
	if !ok {
		return ResolvedProvider{}, fmt.Errorf("%w: %s", ErrUnknownProvider, id)
	}
	out := ResolvedProvider{ID: id, Name: orDefault(p.Name, id), IdleTimeout: DefaultIdleTimeout}
	if s := p.Settings; s != nil {
		out.BaseURL, out.Endpoint, out.APIKeyRef = s.BaseURL, s.Endpoint, s.APIKey
		out.Headers = cloneSettings(*s).Headers
		if s.TimeoutSeconds > 0 {
			out.RequestTimeout = time.Duration(s.TimeoutSeconds) * time.Second
		}
		if s.IdleTimeoutSeconds > 0 {
			out.IdleTimeout = time.Duration(s.IdleTimeoutSeconds) * time.Second
		}
	}
	return out, nil
}

// Resolve returns the settings of one catalog model, or ErrUnknownModel.
func (c *Config) Resolve(ref ModelRef) (ResolvedModel, error) {
	pid, key, ok := ref.Split()
	if !ok {
		return ResolvedModel{}, fmt.Errorf("%w: %q is not provider/model", ErrUnknownModel, ref)
	}
	p, ok := c.providers[pid]
	if !ok {
		return ResolvedModel{}, fmt.Errorf("%w: %s", ErrUnknownModel, ref)
	}
	m, ok := p.Models[key]
	if !ok {
		return ResolvedModel{}, fmt.Errorf("%w: %s", ErrUnknownModel, ref)
	}
	prov, err := c.Provider(pid)
	if err != nil {
		return ResolvedModel{}, err
	}

	out := ResolvedModel{
		Ref: ref, ProviderID: pid, ProviderName: prov.Name,
		ModelKey: key, ModelName: orDefault(m.Name, key), ModelID: orDefault(m.ModelID, key),
		Context: DefaultContext, Output: DefaultOutput,
		ReasoningFormat: ReasoningNone, ToolCalling: true,
		InputModalities: []string{"text"},
		BaseURL:         prov.BaseURL, Endpoint: prov.Endpoint, APIKeyRef: prov.APIKeyRef,
		Headers: prov.Headers, RequestTimeout: prov.RequestTimeout, IdleTimeout: prov.IdleTimeout,
	}
	if m.Limit != nil && m.Limit.Context > 0 {
		out.Context = m.Limit.Context
	} else {
		out.Warnings = append(out.Warnings, limitWarning(ref, "limit.context", DefaultContext))
	}
	if m.Limit != nil && m.Limit.Output > 0 {
		out.Output = m.Limit.Output
		out.OutputSet = true
	} else {
		out.Warnings = append(out.Warnings, limitWarning(ref, "limit.output", DefaultOutput))
	}
	if r := m.Reasoning; r != nil {
		if r.Format != "" {
			out.ReasoningFormat = r.Format
		}
		out.Efforts = slices.Clone(r.Efforts)
		out.ReasoningEcho = r.Echo
	}
	if out.ReasoningEcho == "" {
		out.ReasoningEcho = ReasoningEchoNone
		if out.ReasoningFormat == ReasoningDeepSeek {
			out.ReasoningEcho = ReasoningEchoContent
		}
	}
	if m.ToolCalling != nil {
		out.ToolCalling = *m.ToolCalling
	}
	if m.Modalities != nil && len(m.Modalities.Input) > 0 {
		out.InputModalities = slices.Clone(m.Modalities.Input)
	}
	return out, nil
}

func limitWarning(ref ModelRef, key string, def int) Warning {
	return Warning{
		Code: WarnDefaultLimit, Key: string(ref) + "." + key,
		Message: fmt.Sprintf("model %s sets no %s; using %d", ref, key, def),
	}
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
