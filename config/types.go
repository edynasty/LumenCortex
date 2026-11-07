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
	"slices"
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

// The effort levels, lowest first: the OpenAI reasoning_effort ladder (none, minimal … xhigh,
// max), which also covers Anthropic adaptive thinking (low … max). No single endpoint accepts every
// level: a model's reasoning.efforts lists the ones it accepts, ReasoningEfforts bounds that list
// per reasoning.format, and ClampEffort fits a request to it.
const (
	EffortNone    Effort = "none"
	EffortMinimal Effort = "minimal"
	EffortLow     Effort = "low"
	EffortMedium  Effort = "medium"
	EffortHigh    Effort = "high"
	EffortXHigh   Effort = "xhigh"
	EffortMax     Effort = "max"
)

var effortLevels = []Effort{EffortNone, EffortMinimal, EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax}

// EffortLevels returns the concrete effort levels, lowest first. The caller owns the copy.
func EffortLevels() []Effort { return slices.Clone(effortLevels) }

// EffortAuto asks the runtime to classify a user turn's difficulty and resolve one of the
// levels above before the first request. It is a request value, not a level: EffortFor and
// ClampEffort never see it.
const EffortAuto Effort = "auto"

// IsLevel reports whether e is one of the concrete effort levels. "" and "auto" are not.
func (e Effort) IsLevel() bool { return slices.Contains(effortLevels, e) }

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
	// ReasoningAdaptive asks for adaptive thinking (anthropic-messages): the model decides how much
	// to think, and effort is sent as output_config.effort. Only the anthropic-messages package
	// accepts it.
	ReasoningAdaptive = "adaptive"

	ReasoningEchoNone    = "none"
	ReasoningEchoContent = "reasoning_content"
)

// Check kinds.
const (
	CheckTest   = "test"
	CheckBuild  = "build"
	CheckSchema = "schema"
	// CheckSemantic is a host check that decides the semantic verification level (spec §24-25). It
	// runs like a test or build command; its result never counts as execution evidence alone.
	CheckSemantic = "semantic"
)

// File is one lumencortex.json document. A nil slice or map, a nil pointer and a zero number mean
// "absent". Failover and ProviderSettings.Headers are the exceptions: a non-nil empty value is
// written as [] or {} and replaces the global list or header set with none.
type File struct {
	Schema            string              `json:"$schema,omitempty"`
	Model             ModelRef            `json:"model,omitempty"`
	Failover          []ModelRef          `json:"failover,omitempty"`
	Effort            *EffortPolicy       `json:"effort,omitempty"`
	Agents            map[string]Agent    `json:"agents,omitempty"`
	Concurrency       *Concurrency        `json:"concurrency,omitempty"`
	Execution         *Execution          `json:"execution,omitempty"`
	LSP               *LSP                `json:"lsp,omitempty"`
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

// Concurrency bounds the agent runs of one process. It is read from the global file only. A zero
// field means "absent": the default applies.
type Concurrency struct {
	MaxAgents            int `json:"maxAgents,omitempty"`
	MaxChildrenPerParent int `json:"maxChildrenPerParent,omitempty"`
}

// Concurrency defaults and schema bounds. Parent plus a full set of children fits the default
// process limit, so one thread can use all of its child slots on its own.
const (
	DefaultMaxAgents            = 4
	DefaultMaxChildrenPerParent = 3
	MaxMaxAgents                = 16
	MaxMaxChildrenPerParent     = 8
)

// Limits returns c with its defaults applied; a nil c is all defaults. Values outside the schema
// bounds never reach here: Parse and Save validate the file.
func (c *Concurrency) Limits() Concurrency {
	out := Concurrency{MaxAgents: DefaultMaxAgents, MaxChildrenPerParent: DefaultMaxChildrenPerParent}
	if c == nil {
		return out
	}
	if c.MaxAgents > 0 {
		out.MaxAgents = c.MaxAgents
	}
	if c.MaxChildrenPerParent > 0 {
		out.MaxChildrenPerParent = c.MaxChildrenPerParent
	}
	return out
}

// Execution bounds the runtime's tool execution of one process: the foreground shell timeout, the
// background job queue and the diagnostics log. Like Concurrency it is read from the global file only,
// at every run start and job submission, and a zero field means "absent": the default applies.
type Execution struct {
	// ShellTimeoutSeconds is the timeout of a foreground shell call that names none. Absent: the
	// default, 120 s. It is clamped to ShellMaxTimeoutSeconds.
	ShellTimeoutSeconds int `json:"shellTimeoutSeconds,omitempty"`
	// ShellMaxTimeoutSeconds bounds every foreground shell call, including one whose timeout_sec names
	// more. Absent: the default, 1800 s. A lower value also lowers an absent or higher default.
	ShellMaxTimeoutSeconds int `json:"shellMaxTimeoutSeconds,omitempty"`
	// MaxConcurrentJobs is the background jobs running at once in this process.
	MaxConcurrentJobs int `json:"maxConcurrentJobs,omitempty"`
	// MaxQueuedJobs is the jobs waiting to run; a submission beyond it is refused, never dropped.
	MaxQueuedJobs int `json:"maxQueuedJobs,omitempty"`
	// JobOutputBytes is the output kept per job and stream (stdout, stderr): head and tail, middle cut.
	JobOutputBytes int `json:"jobOutputBytes,omitempty"`
	// JobRetentionDays is how long a finished job's row and output are kept.
	JobRetentionDays int `json:"jobRetentionDays,omitempty"`
	// LogLevel is the diagnostics log's level: error, warn, info or debug.
	LogLevel string `json:"logLevel,omitempty"`
	// Budget bounds every agent run of this process (ADR-0019 §6); a run's own budget overrides it field
	// by field. Absent: no limit.
	Budget *Budget `json:"budget,omitempty"`
	// Retry is how a run recovers from provider availability failures. Absent: the defaults.
	Retry *Retry `json:"retry,omitempty"`
}

// Retry bounds availability recovery within one response step. The first candidate sweep and one
// capacity reduction per candidate are separate; retries after that sweep share MaxRetries, never
// multiplying it by the model count. Deadline, cancellation and an explicit outage window also apply.
// Zero fields mean absent, not unlimited. This is not a whole-run request, token or cost budget.
type Retry struct {
	// Disabled ends the run at the first availability failure no failover candidate answered.
	Disabled bool `json:"disabled,omitempty"`
	// MaxRetries bounds additional recovery requests after the initial failover sweep. Absent: 6.
	MaxRetries int `json:"maxRetries,omitempty"`
	// BaseDelayMs is the first backoff; each further one doubles. Absent: 1000.
	BaseDelayMs int `json:"baseDelayMs,omitempty"`
	// MaxDelayMs caps the computed backoff (a provider's Retry-After may ask for more). Absent: 30000.
	MaxDelayMs int `json:"maxDelayMs,omitempty"`
	// MaxOutageSeconds optionally bounds one continuous outage. Absent: no additional time window;
	// the finite retry count and the run's deadline/cancellation still apply.
	MaxOutageSeconds int `json:"maxOutageSeconds,omitempty"`
}

// DefaultMaxSteps leaves response/tool cycles per Start unlimited. Positive host values bound steps;
// zero means omitted. Provider attempts within one response step do not consume additional steps.
const DefaultMaxSteps = -1

// Retry defaults allow about a minute of backoff without imposing a task-wide request quota.
const (
	DefaultRetryMaxRetries  = 6
	DefaultRetryBaseDelayMs = 1000
	DefaultRetryMaxDelayMs  = 30000
)

// Over returns r with every set field of o in place of its own.
func (r Retry) Over(o Retry) Retry {
	if o.Disabled {
		r.Disabled = true
	}
	if o.MaxRetries > 0 {
		r.MaxRetries = o.MaxRetries
	}
	if o.BaseDelayMs > 0 {
		r.BaseDelayMs = o.BaseDelayMs
	}
	if o.MaxDelayMs > 0 {
		r.MaxDelayMs = o.MaxDelayMs
	}
	if o.MaxOutageSeconds > 0 {
		r.MaxOutageSeconds = o.MaxOutageSeconds
	}
	return r
}

// Budget bounds one agent run (ADR-0019 §6). A zero field is no limit. The run's deadline is its
// runtime limit. Limits are checked before each new model request or tool call is admitted; a request
// already sent is settled whatever it costs, so the last request may overshoot TokenBudget and
// CostLimitUSD by its reply.
type Budget struct {
	MaxToolCalls     int   `json:"maxToolCalls,omitempty"`
	MaxModelRequests int   `json:"maxModelRequests,omitempty"`
	TokenBudget      int64 `json:"tokenBudget,omitempty"`
	// CostWarningUSD reports once (event budget.warning) when the run's known and estimated cost reaches
	// it, or when a request goes to a model whose price is unknown.
	CostWarningUSD float64 `json:"costWarningUsd,omitempty"`
	// CostLimitUSD refuses a request whose input would take the run's known and estimated cost past it,
	// and every request to a model with no configured price: an unknown cost is never taken as zero.
	CostLimitUSD float64 `json:"costLimitUsd,omitempty"`
}

// Over returns b with every non-zero field of o in place of its own.
func (b Budget) Over(o Budget) Budget {
	if o.MaxToolCalls > 0 {
		b.MaxToolCalls = o.MaxToolCalls
	}
	if o.MaxModelRequests > 0 {
		b.MaxModelRequests = o.MaxModelRequests
	}
	if o.TokenBudget > 0 {
		b.TokenBudget = o.TokenBudget
	}
	if o.CostWarningUSD > 0 {
		b.CostWarningUSD = o.CostWarningUSD
	}
	if o.CostLimitUSD > 0 {
		b.CostLimitUSD = o.CostLimitUSD
	}
	return b
}

// Execution defaults and schema bounds.
const (
	DefaultMaxConcurrentJobs      = 2
	DefaultMaxQueuedJobs          = 32
	DefaultJobOutputBytes         = 4 << 20
	DefaultJobRetentionDays       = 7
	DefaultLogLevel               = "info"
	DefaultShellTimeoutSeconds    = 120
	DefaultShellMaxTimeoutSeconds = 1800
	MaxShellTimeoutSeconds        = 86400
	MaxMaxConcurrentJobs          = 16
	MaxMaxQueuedJobs              = 1024
	MinJobOutputBytes             = 64 << 10
	MaxJobOutputBytes             = 64 << 20
	MaxJobRetentionDays           = 365
)

// Limits returns e with its defaults applied; a nil e is all defaults. Values outside the schema bounds
// never reach here: Parse and Save validate the file.
func (e *Execution) Limits() Execution {
	out := Execution{
		ShellTimeoutSeconds:    DefaultShellTimeoutSeconds,
		ShellMaxTimeoutSeconds: DefaultShellMaxTimeoutSeconds,
		MaxConcurrentJobs:      DefaultMaxConcurrentJobs,
		MaxQueuedJobs:          DefaultMaxQueuedJobs,
		JobOutputBytes:         DefaultJobOutputBytes,
		JobRetentionDays:       DefaultJobRetentionDays,
		LogLevel:               DefaultLogLevel,
	}
	if e == nil {
		return out
	}
	if e.ShellMaxTimeoutSeconds > 0 {
		out.ShellMaxTimeoutSeconds = e.ShellMaxTimeoutSeconds
	}
	if e.ShellTimeoutSeconds > 0 {
		out.ShellTimeoutSeconds = e.ShellTimeoutSeconds
	}
	if out.ShellTimeoutSeconds > out.ShellMaxTimeoutSeconds {
		out.ShellTimeoutSeconds = out.ShellMaxTimeoutSeconds
	}
	if e.MaxConcurrentJobs > 0 {
		out.MaxConcurrentJobs = e.MaxConcurrentJobs
	}
	if e.MaxQueuedJobs > 0 {
		out.MaxQueuedJobs = e.MaxQueuedJobs
	}
	if e.JobOutputBytes > 0 {
		out.JobOutputBytes = e.JobOutputBytes
	}
	if e.JobRetentionDays > 0 {
		out.JobRetentionDays = e.JobRetentionDays
	}
	if e.LogLevel != "" {
		out.LogLevel = e.LogLevel
	}
	if e.Budget != nil {
		b := *e.Budget
		out.Budget = &b
	}
	if e.Retry != nil {
		r := *e.Retry
		out.Retry = &r
	}
	return out
}

// LSP bounds the language servers one process starts and says which servers it may start. Like
// Concurrency and Execution it is read from the global file only: a language server is a process on
// this machine, and a project file must not name a command that gets executed. A zero field means
// "absent": the default applies.
//
// Nothing is bundled: a server runs only when its command is on PATH and the workspace holds a file
// or a marker of its language. A workspace with no usable server gets no lsp_* tools.
type LSP struct {
	// IdleTimeoutSeconds is how long a server may stay unused before it is stopped; the next request
	// starts it again.
	IdleTimeoutSeconds int `json:"idleTimeoutSeconds,omitempty"`
	// MaxServers is how many language servers may run at once. Starting another stops the least
	// recently used idle one; when every server is busy the call is refused, never queued.
	MaxServers int `json:"maxServers,omitempty"`
	// StartTimeoutSeconds bounds one start, from the spawn to the initialize answer.
	StartTimeoutSeconds int `json:"startTimeoutSeconds,omitempty"`
	// RequestTimeoutSeconds bounds one language-server request; a run's tool deadline bounds it too.
	RequestTimeoutSeconds int `json:"requestTimeoutSeconds,omitempty"`
	// Servers overrides or extends the built-in language server catalog, keyed by language id.
	Servers map[string]LSPServer `json:"servers,omitempty"`
}

// LSPServer is one language server: the command to run and the files it serves. Language, Extensions
// and Markers are absent for a built-in language, whose values are kept.
type LSPServer struct {
	Command    string   `json:"command"`
	Args       []string `json:"args,omitempty"`
	Extensions []string `json:"extensions,omitempty"`
	Markers    []string `json:"markers,omitempty"`
}

// LSP defaults and schema bounds.
const (
	DefaultLSPIdleTimeoutSeconds    = 600
	DefaultMaxLSPServers            = 3
	DefaultLSPStartTimeoutSeconds   = 20
	DefaultLSPRequestTimeoutSeconds = 20
	MaxLSPIdleTimeoutSeconds        = 86400
	MaxMaxLSPServers                = 8
	MaxLSPStartTimeoutSeconds       = 300
	MaxLSPRequestTimeoutSeconds     = 300
)

// Limits returns l with its defaults applied; a nil l is all defaults. Values outside the schema
// bounds never reach here: Parse and Save validate the file.
func (l *LSP) Limits() LSP {
	out := LSP{
		IdleTimeoutSeconds:    DefaultLSPIdleTimeoutSeconds,
		MaxServers:            DefaultMaxLSPServers,
		StartTimeoutSeconds:   DefaultLSPStartTimeoutSeconds,
		RequestTimeoutSeconds: DefaultLSPRequestTimeoutSeconds,
	}
	if l == nil {
		return out
	}
	if l.IdleTimeoutSeconds > 0 {
		out.IdleTimeoutSeconds = l.IdleTimeoutSeconds
	}
	if l.MaxServers > 0 {
		out.MaxServers = l.MaxServers
	}
	if l.StartTimeoutSeconds > 0 {
		out.StartTimeoutSeconds = l.StartTimeoutSeconds
	}
	if l.RequestTimeoutSeconds > 0 {
		out.RequestTimeoutSeconds = l.RequestTimeoutSeconds
	}
	if len(l.Servers) > 0 {
		out.Servers = make(map[string]LSPServer, len(l.Servers))
		for language, server := range l.Servers {
			out.Servers[language] = server
		}
	}
	return out
}

// EffortPolicy is the reasoning effort policy. Default may be "auto": each Start then resolves a
// concrete level from Auto's judge (or the heuristic fallback) before its first request.
type EffortPolicy struct {
	Default Effort      `json:"default,omitempty"`
	Auto    *AutoEffort `json:"auto,omitempty"`
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
	Cost        *Cost       `json:"cost,omitempty"`
	// Profile is the model's execution profile: how its tool catalog and failure feedback are
	// presented, and the run limits its sessions default to. It never changes policy, permissions,
	// checks or tool authority.
	Profile *ModelProfile `json:"profile,omitempty"`
}

// Model profile presentation variants. A profile may only choose a presentation; the authority a
// tool carries (its permission, parameters and mutating flag) is never profile-dependent.
const (
	ProfileCompact  = "compact"
	ProfileDetailed = "detailed"
)

// ModelProfile is a model's execution profile (spec §26).
type ModelProfile struct {
	// ToolDescriptions: compact trims each tool description to its first sentence; detailed (or
	// absent) sends the full text. The schema, permission and mutating flag are unchanged.
	ToolDescriptions string `json:"toolDescriptions,omitempty"`
	// Feedback: compact shortens a blocked completion's feedback to one line per item; detailed (or
	// absent) keeps diagnostics and output tails.
	Feedback string `json:"feedback,omitempty"`
	// MaxSteps and MaxToolCallsPerStep are the run limits a session on this model starts with when
	// the host sets none.
	MaxSteps            int `json:"maxSteps,omitempty"`
	MaxToolCallsPerStep int `json:"maxToolCallsPerStep,omitempty"`
	// Budget is this model's default run budget, overridden field by field by a run's own budget.
	Budget *Budget `json:"budget,omitempty"`
}

// ValidPresentation reports whether v is "", compact or detailed.
func ValidPresentation(v string) bool {
	return v == "" || v == ProfileCompact || v == ProfileDetailed
}

// Cost is a model's list price in US dollars per million tokens (ADR-0019 §6). It prices a run's
// usage at list price: provider cache discounts are not modelled. A model with none has an unknown
// cost.
type Cost struct {
	Input  float64 `json:"input"`
	Output float64 `json:"output"`
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
	Kind    string `json:"kind"`
	Command string `json:"command,omitempty"`
	Schema  string `json:"schema,omitempty"`
	// Files are the workspace files the definition depends on. A schema check names the files it
	// validates. A semantic check must name the expected-truth files its command reads (a committed
	// oracle, a fixture, a data table), including one the program opens from inside itself: only the
	// host knows what a program reads, so an oracle is never inferred from the command text. A
	// semantic command that carries its own expectation (grep -q '42' answer.txt, a self-contained
	// script) declares it explicitly as "files": [].
	Files          []string `json:"files,omitempty"`
	TimeoutSeconds int      `json:"timeoutSeconds,omitempty"`
	Required       *bool    `json:"required,omitempty"`
	// Source is the file the effective check came from, CheckSourceGlobal or CheckSourceWorkspace. Merge
	// sets it; it is never read from or written to a file.
	Source string `json:"-"`
}

// MarshalJSON preserves an explicit empty oracle list: missing files means undeclared.
func (c Check) MarshalJSON() ([]byte, error) {
	type plain Check
	out := struct {
		plain
		Files *[]string `json:"files,omitempty"`
	}{plain: plain(c)}
	if c.Files != nil {
		out.Files = &c.Files
	}
	return json.Marshal(out)
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
