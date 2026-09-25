package cognition

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type ModelSpec struct {
	Provider  string            `json:"provider,omitempty"`
	Model     string            `json:"model,omitempty"`
	BaseURL   string            `json:"baseURL,omitempty"`
	APIKey    string            `json:"apiKey,omitempty"`
	APIKeyEnv string            `json:"apiKeyEnv,omitempty"`
	TimeoutMS int               `json:"timeoutMs,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
}

func (m *ModelSpec) UnmarshalJSON(raw []byte) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return errors.New("empty model spec")
	}
	if raw[0] == '"' {
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		*m = parseModelString(value)
		return nil
	}
	type alias ModelSpec
	var value alias
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	*m = ModelSpec(value)
	return nil
}

type CategoryConfig struct {
	Description string      `json:"description,omitempty"`
	Models      []ModelSpec `json:"models,omitempty"`
	Default     bool        `json:"default,omitempty"`
}

type DecisionProviderSpec struct {
	Type      string            `json:"type,omitempty"`
	Name      string            `json:"name,omitempty"`
	BaseURL   string            `json:"baseURL,omitempty"`
	APIKey    string            `json:"apiKey,omitempty"`
	APIKeyEnv string            `json:"apiKeyEnv,omitempty"`
	Model     string            `json:"model,omitempty"`
	TimeoutMS int               `json:"timeoutMs,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
}

func (d *DecisionProviderSpec) UnmarshalJSON(raw []byte) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return errors.New("empty decision provider spec")
	}
	if raw[0] == '"' {
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		d.Type = strings.TrimSpace(value)
		return nil
	}
	type alias DecisionProviderSpec
	var value alias
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	*d = DecisionProviderSpec(value)
	return nil
}

type DecisionConfig struct {
	Policy    string                 `json:"policy,omitempty"`
	Providers []DecisionProviderSpec `json:"providers,omitempty"`
}

type TelemetryConfig struct {
	Enabled *bool `json:"enabled,omitempty"`
}

type GovernorConfig struct {
	Enabled         bool              `json:"enabled,omitempty"`
	Provider        string            `json:"provider,omitempty"`
	Model           string            `json:"model,omitempty"`
	BaseURL         string            `json:"baseURL,omitempty"`
	TimeoutMS       int               `json:"timeoutMs,omitempty"`
	ReasoningEffort string            `json:"reasoningEffort,omitempty"`
	MaxTokens       int               `json:"maxTokens,omitempty"`
	Headers         map[string]string `json:"headers,omitempty"`
}

type Config struct {
	Version    int                       `json:"version"`
	Source     string                    `json:"source"`
	Decision   DecisionConfig            `json:"decision"`
	Categories map[string]CategoryConfig `json:"categories"`
	Telemetry  bool                      `json:"telemetry"`
	Health     HealthConfig              `json:"health"`
	Governor   *GovernorConfig           `json:"governor,omitempty"`
}

type rawConfig struct {
	Decision   DecisionConfig            `json:"decision"`
	Categories map[string]CategoryConfig `json:"categories"`
	Telemetry  TelemetryConfig           `json:"telemetry"`
	Health     HealthConfig              `json:"health"`
	Governor   *GovernorConfig           `json:"governor"`
}

func LoadConfig(workspace, file string) (Config, error) {
	if strings.TrimSpace(workspace) == "" {
		workspace = "."
	}
	if strings.TrimSpace(file) == "" {
		file = filepath.Join(workspace, ".lumencortex", "cognition.json")
	} else if !filepath.IsAbs(file) {
		file = filepath.Join(workspace, file)
	}

	config := defaultConfig()
	config.Source = "built-in"
	raw, err := os.ReadFile(file)
	if err != nil {
		if os.IsNotExist(err) {
			return config, nil
		}
		return Config{}, err
	}

	var user rawConfig
	if err := json.Unmarshal(raw, &user); err != nil {
		return Config{}, err
	}
	config.Source = file
	if strings.TrimSpace(user.Decision.Policy) != "" {
		config.Decision.Policy = strings.TrimSpace(user.Decision.Policy)
	}
	if user.Decision.Providers != nil {
		config.Decision.Providers = append([]DecisionProviderSpec(nil), user.Decision.Providers...)
	}
	for name, category := range user.Categories {
		current := config.Categories[name]
		if strings.TrimSpace(category.Description) != "" {
			current.Description = category.Description
		}
		if category.Models != nil {
			current.Models = append([]ModelSpec(nil), category.Models...)
		}
		current.Default = category.Default
		config.Categories[name] = current
	}
	if user.Telemetry.Enabled != nil {
		config.Telemetry = *user.Telemetry.Enabled
	}
	if user.Health.FailureThreshold > 0 {
		config.Health.FailureThreshold = user.Health.FailureThreshold
	}
	if user.Health.CooldownMS > 0 {
		config.Health.CooldownMS = user.Health.CooldownMS
	}
	if user.Governor != nil {
		copy := *user.Governor
		config.Governor = &copy
	}

	normalizeDefaultCategory(config.Categories)
	return config, nil
}

func defaultConfig() Config {
	profile := DefaultProfile()
	categories := map[string]CategoryConfig{}
	for name, category := range profile.Categories {
		categories[name] = CategoryConfig{
			Description: category.Description,
			Default: category.Default,
		}
	}
	return Config{
		Version: 1,
		Decision: DecisionConfig{Policy: "first"},
		Categories: categories,
		Telemetry: true,
		Health: HealthConfig{
			FailureThreshold: 3,
			CooldownMS: 30000,
		},
	}
}

func normalizeDefaultCategory(categories map[string]CategoryConfig) {
	defaults := []string{}
	for name, category := range categories {
		if category.Default {
			defaults = append(defaults, name)
		}
	}
	sort.Strings(defaults)
	if len(defaults) == 0 {
		category := categories["general"]
		category.Default = true
		categories["general"] = category
		return
	}
	if len(defaults) == 1 {
		return
	}
	keep := defaults[0]
	for _, name := range defaults {
		if name == "general" {
			keep = "general"
			break
		}
	}
	for name, category := range categories {
		category.Default = name == keep
		categories[name] = category
	}
}

func parseModelString(value string) ModelSpec {
	value = strings.TrimSpace(value)
	if value == "" {
		return ModelSpec{}
	}
	if index := strings.Index(value, ":"); index > 0 {
		prefix := value[:index]
		if !strings.Contains(prefix, "/") {
			return ModelSpec{Provider: prefix, Model: value[index+1:]}
		}
	}
	if index := strings.Index(value, "/"); index > 0 {
		prefix := value[:index]
		if knownProvider(prefix) {
			return ModelSpec{Provider: prefix, Model: value[index+1:]}
		}
	}
	return ModelSpec{Model: value}
}

func knownProvider(name string) bool {
	switch name {
	case "openrouter", "openrouter-deepseek-free", "groq", "deepseek", "generic":
		return true
	default:
		return false
	}
}
