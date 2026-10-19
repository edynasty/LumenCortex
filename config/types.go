// Package config is the one reader and writer of lumencortex.json (design §4.8).
//
// The global file lives in GlobalDir, a workspace may add <workspace>/lumencortex.json, and both
// follow docs/lumencortex.schema.json. Load merges them under the workspace trust rules (A8);
// Save is the only way core writes either file. Secrets are never stored here: an API key is an
// {env:NAME} reference read from the process environment or a {secret:NAME} reference resolved
// through the host's CredentialResolver when a provider client is built (I12).
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// ModelRef names a catalog model as "provider/model": exactly one '/'.
type ModelRef string

// Split returns the provider id and the model key.
func (r ModelRef) Split() (provider, model string, ok bool) {
	provider, model, ok = strings.Cut(string(r), "/")
	if !ok || provider == "" || model == "" || strings.Contains(model, "/") {
		return "", "", false
	}
	return provider, model, true
}

// Provider returns the provider id of the reference, or "" when it is malformed.
func (r ModelRef) Provider() string { p, _, _ := r.Split(); return p }

// Effort is the provider-neutral reasoning effort level.
type Effort string

// The effort levels, lowest first.
const (
	EffortNone   Effort = "none"
	EffortLow    Effort = "low"
	EffortMedium Effort = "medium"
	EffortHigh   Effort = "high"
	EffortMax    Effort = "max"
)

// EffortAuto asks the runtime to classify a user turn's difficulty and resolve one of the five
// levels above before the first request. It is a request value, not a level: EffortFor and
// ClampEffort never see it.
const EffortAuto Effort = "auto"

// IsLevel reports whether e is one of the five concrete effort levels. "" and "auto" are not.
func (e Effort) IsLevel() bool {
	switch e {
	case EffortNone, EffortLow, EffortMedium, EffortHigh, EffortMax:
		return true
	}
	return false
}

// PackageOpenAICompatible is the wire protocol every accepted Provider.Package value selects.
const PackageOpenAICompatible = "openai-compatible"

// PackageAnthropicMessages selects the Anthropic Messages wire protocol.
const PackageAnthropicMessages = "anthropic-messages"

// legacyPackages are the package ids earlier builds accepted; they select the openai-compatible
// client, as the schema documents.
var legacyPackages = map[string]bool{
	"@opencode/ai/providers/openai-compatible": true,
	"@ai-sdk/openai-compatible":                true,
}

// NormalizePackage maps a Provider.Package value to the wire protocol it selects: "" and the
// legacy ids become openai-compatible, anthropic-messages stays itself. Any other value is
// ErrInvalidConfig naming the accepted values.
func NormalizePackage(pkg string) (string, error) {
	switch {
	case pkg == "" || pkg == PackageOpenAICompatible:
		return PackageOpenAICompatible, nil
	case legacyPackages[pkg]:
		return PackageOpenAICompatible, nil
	case pkg == PackageAnthropicMessages:
		return PackageAnthropicMessages, nil
	default:
		return "", fmt.Errorf("%w: package %q is not one of %s, %s", ErrInvalidConfig, pkg, PackageOpenAICompatible, PackageAnthropicMessages)
	}
}

// Reasoning formats and echo modes of a model.
const (
	ReasoningNone        = "none"
	ReasoningObject      = "reasoning-object"
	ReasoningEffortField = "reasoning-effort"
	ReasoningDeepSeek    = "deepseek"

	ReasoningEchoNone    = "none"
	ReasoningEchoContent = "reasoning_content"
)

// Check kinds.
const (
	CheckTest   = "test"
	CheckBuild  = "build"
	CheckSchema = "schema"
)

// File is one lumencortex.json document. A nil slice or map, a nil pointer and a zero number mean
// "absent". Failover and ProviderSettings.Headers are the exceptions: a non-nil empty value is
// written as [] or {} and replaces the global list or header set with none.
type File struct {
	Schema            string              `json:"$schema,omitempty"`
	Model             ModelRef            `json:"model,omitempty"`
	Failover          []ModelRef          `json:"failover,omitempty"`
	SubagentModel     ModelRef            `json:"subagentModel,omitempty"`
	Effort            *EffortPolicy       `json:"effort,omitempty"`
	Providers         map[string]Provider `json:"providers,omitempty"`
	Checks            map[string]Check    `json:"checks,omitempty"`
	TrustedWorkspaces []string            `json:"trustedWorkspaces,omitempty"`
}

// MarshalJSON keeps an explicit empty failover list, which omitempty would drop.
func (f File) MarshalJSON() ([]byte, error) {
	type plain File
	out := struct {
		plain
		Failover *[]ModelRef `json:"failover,omitempty"`
	}{plain: plain(f)}
	if f.Failover != nil {
		out.Failover = &f.Failover
	}
	return json.Marshal(out)
}

// EffortPolicy is the reasoning effort policy. Default may be "auto": each Start then resolves a
// concrete level from Auto's judge (or the heuristic fallback) before its first request.
type EffortPolicy struct {
	Default    Effort      `json:"default,omitempty"`
	Auto       *AutoEffort `json:"auto,omitempty"`
	Escalation *Escalation `json:"escalation,omitempty"`
}

// AutoEffort configures the auto effort resolver. Absent Model means the deterministic heuristic
// classifier only; absent Max means "high"; absent TimeoutSeconds means 8.
type AutoEffort struct {
	// Model is the judge model ref (provider/model). It classifies each user turn; a judge failure,
	// timeout or unusable answer falls back to the heuristic.
	Model ModelRef `json:"model,omitempty"`
	// Max is the ceiling: the classifier's level is never resolved above it.
	Max Effort `json:"max,omitempty"`
	// TimeoutSeconds bounds one judge request.
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`
}

// Auto effort defaults.
const (
	DefaultAutoMax     = EffortHigh
	DefaultAutoTimeout = 8
)

// AutoEffortPolicy is effort.auto with its defaults applied.
type AutoEffortPolicy struct {
	Model ModelRef
	Max   Effort
	// Timeout bounds one judge request.
	Timeout time.Duration
}

// Escalation raises effort while a run is stuck.
type Escalation struct {
	SameFailure     int    `json:"sameFailure"`
	NoProgressSteps int    `json:"noProgressSteps"`
	Max             Effort `json:"max"`
}

// Provider is one catalog provider.
type Provider struct {
	Name     string            `json:"name,omitempty"`
	Package  string            `json:"package,omitempty"`
	Settings *ProviderSettings `json:"settings,omitempty"`
	Models   map[string]Model  `json:"models,omitempty"`
}

// ProviderSettings are the connection settings of a provider client.
type ProviderSettings struct {
	BaseURL            string            `json:"baseURL,omitempty"`
	Endpoint           string            `json:"endpoint,omitempty"`
	APIKey             string            `json:"apiKey,omitempty"` // {env:NAME} or {secret:NAME}, never a literal key
	Headers            map[string]string `json:"headers,omitempty"`
	TimeoutSeconds     int               `json:"timeoutSeconds,omitempty"`
	IdleTimeoutSeconds int               `json:"idleTimeoutSeconds,omitempty"`
}

// MarshalJSON keeps an explicit empty headers set, which omitempty would drop.
func (s ProviderSettings) MarshalJSON() ([]byte, error) {
	type plain ProviderSettings
	out := struct {
		plain
		Headers *map[string]string `json:"headers,omitempty"`
	}{plain: plain(s)}
	if s.Headers != nil {
		out.Headers = &s.Headers
	}
	return json.Marshal(out)
}

// Model is one catalog model.
type Model struct {
	Name        string      `json:"name,omitempty"`
	ModelID     string      `json:"modelID,omitempty"`
	Limit       *Limit      `json:"limit,omitempty"`
	Reasoning   *Reasoning  `json:"reasoning,omitempty"`
	ToolCalling *bool       `json:"toolCalling,omitempty"`
	Modalities  *Modalities `json:"modalities,omitempty"`
}

// Limit holds the token limits used by the context window builder.
type Limit struct {
	Context int `json:"context,omitempty"`
	Output  int `json:"output,omitempty"`
}

// Reasoning describes how effort is sent and how reasoning output is handled.
type Reasoning struct {
	Format  string   `json:"format,omitempty"`
	Efforts []Effort `json:"efforts,omitempty"`
	Echo    string   `json:"echo,omitempty"`
}

// Modalities lists the accepted input modalities.
type Modalities struct {
	Input []string `json:"input,omitempty"`
}

// Check is an evaluator check (design §4.7); internal/evaluator runs it. Absent TimeoutSeconds means
// DefaultCheckTimeout, absent Required means true.
type Check struct {
	Kind           string   `json:"kind"`
	Command        string   `json:"command,omitempty"`
	Schema         string   `json:"schema,omitempty"`
	Files          []string `json:"files,omitempty"`
	TimeoutSeconds int      `json:"timeoutSeconds,omitempty"`
	Required       *bool    `json:"required,omitempty"`
	// Source is the file the effective check came from, CheckSourceGlobal or CheckSourceWorkspace. Merge
	// sets it; it is never read from or written to a file.
	Source string `json:"-"`
}

// Check sources and defaults.
const (
	CheckSourceGlobal    = "global"
	CheckSourceWorkspace = "workspace"
	// DefaultCheckTimeout applies when a check sets no timeoutSeconds.
	DefaultCheckTimeout = 600
)

// IsRequired reports whether a failing result of the check blocks completion; absent means true.
func (c Check) IsRequired() bool { return c.Required == nil || *c.Required }

// Errors returned by this package; match them with errors.Is.
var (
	// ErrInvalidConfig wraps every failure of a file to be valid configuration.
	ErrInvalidConfig = errors.New("invalid configuration")
	// ErrUntrustedOverride reports that a workspace that is not listed in the global
	// trustedWorkspaces defined or changed something only a trusted workspace may. Load drops the
	// entry and carries the error in the Warning.
	ErrUntrustedOverride = errors.New("untrusted workspace may not define or change providers, checks or trust")
	// ErrCredentialRedirect reports a workspace that changes a global provider's baseURL or
	// endpoint without its own apiKey, which would send the global credential to a URL of the
	// workspace's choosing. It makes the configuration invalid.
	ErrCredentialRedirect = errors.New("workspace changes a global provider's baseURL/endpoint without its own apiKey")
	// ErrConflict reports that Save found the file different from the one the caller read.
	ErrConflict = errors.New("config file changed since it was read")
	// ErrUnknownModel reports a model reference missing from the provider catalog.
	ErrUnknownModel = errors.New("model is not in the provider catalog")
	// ErrUnknownProvider reports a provider id missing from the provider catalog.
	ErrUnknownProvider = errors.New("provider is not in the catalog")
)

var envRef = regexp.MustCompile(`^\{env:([A-Za-z_][A-Za-z0-9_]*)\}$`)

// EnvName returns NAME for an {env:NAME} reference.
func EnvName(ref string) (string, bool) {
	m := envRef.FindStringSubmatch(ref)
	if m == nil {
		return "", false
	}
	return m[1], true
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidConfig, fmt.Sprintf(format, args...))
}
