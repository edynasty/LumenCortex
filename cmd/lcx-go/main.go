package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/edynasty/LumenCortex/internal/cognition"
	"github.com/edynasty/LumenCortex/provider/openai"
	lcx "github.com/edynasty/LumenCortex/runtime"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "lcx-go:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	workspace, err := os.Getwd()
	if err != nil {
		return err
	}
	engine, err := lcx.Open(lcx.Options{Workspace: workspace})
	if err != nil {
		return err
	}
	defer engine.Close()

	var stopEvents func()
	if envBool("LCX_EVENTS") {
		stopEvents = streamEvents(engine)
		defer stopEvents()
	}

	args := os.Args[1:]
	if len(args) == 0 || args[0] == "health" {
		return printJSON(engine.Health())
	}
	switch args[0] {
	case "session-new":
		if len(args) < 2 {
			return fmt.Errorf("usage: lcx-go session-new <goal>")
		}
		h, err := engine.NewSession(ctx, lcx.SessionOptions{Goal: strings.Join(args[1:], " ")})
		if err != nil {
			return err
		}
		return printJSON(h)
	case "sessions":
		items, err := engine.ListSessions(ctx, 50, 0)
		if err != nil {
			return err
		}
		return printJSON(items)
	case "shell":
		if len(args) < 3 {
			return fmt.Errorf("usage: lcx-go shell <session-id> <command>")
		}
		h, _, err := engine.Session(ctx, args[1])
		if err != nil {
			return err
		}
		result, err := h.RunShell(ctx, strings.Join(args[2:], " "))
		if err != nil {
			return err
		}
		return printJSON(result)
	case "agent":
		if len(args) < 2 {
			return fmt.Errorf("usage: lcx-go agent <goal>")
		}
		provider, err := providerFromEnv()
		if err != nil {
			return err
		}
		h, err := engine.NewSession(ctx, lcx.SessionOptions{Goal: strings.Join(args[1:], " "), Provider: "openai-compatible", Model: provider.Model()})
		if err != nil {
			return err
		}
		agentOpts, err := agentOptionsFromEnv(workspace, engine)
		if err != nil {
			return err
		}
		result, err := engine.RunAgent(ctx, h.ID, provider, agentOpts)
		if err != nil {
			return err
		}
		return printJSON(result)
	case "resume":
		if len(args) != 2 {
			return fmt.Errorf("usage: lcx-go resume <session-id>")
		}
		provider, err := providerFromEnv()
		if err != nil {
			return err
		}
		agentOpts, err := agentOptionsFromEnv(workspace, engine)
		if err != nil {
			return err
		}
		result, err := engine.RunAgent(ctx, args[1], provider, agentOpts)
		if err != nil {
			return err
		}
		return printJSON(result)
	case "approve":
		if len(args) < 3 || len(args) > 4 {
			return fmt.Errorf("usage: lcx-go approve <session-id> <gate-id> [actor]")
		}
		actor := "human"
		if len(args) == 4 {
			actor = args[3]
		}
		result, err := engine.ApproveWorkflowGate(ctx, args[1], args[2], actor)
		if err != nil {
			return err
		}
		return printJSON(result)
	default:
		return fmt.Errorf("unknown command %q (preview commands: health, session-new, sessions, shell, agent, resume, approve)", args[0])
	}
}

func providerFromEnv() (*openai.Client, error) {
	model := strings.TrimSpace(os.Getenv("LCX_MODEL"))
	if model == "" {
		return nil, fmt.Errorf("LCX_MODEL is required for agent/resume")
	}
	return openai.New(openai.Config{
		Endpoint:         strings.TrimSpace(os.Getenv("LCX_ENDPOINT")),
		BaseURL:          strings.TrimSpace(os.Getenv("LCX_BASE_URL")),
		APIKey:           os.Getenv("LCX_API_KEY"),
		Model:            model,
		ReasoningFormat:  strings.TrimSpace(os.Getenv("LCX_REASONING_FORMAT")),
		DisableStreaming: envBool("LCX_DISABLE_STREAMING"),
		DisableRetries:   envBool("LCX_DISABLE_RETRIES"),
	})
}

func agentOptionsFromEnv(workspace string, engine *lcx.Engine) (lcx.AgentOptions, error) {
	policy := strings.TrimSpace(os.Getenv("LCX_POLICY"))
	if policy == "" {
		policy = "read-only"
	}
	maxSteps := envInt("LCX_MAX_STEPS", 24)
	maxTokens := envInt("LCX_MAX_TOKENS", 0)
	recentMessages := envInt("LCX_RECENT_MESSAGES", 8)
	maxToolCalls := envInt("LCX_MAX_TOOL_CALLS_PER_STEP", 8)
	var workflow []byte
	if path := strings.TrimSpace(os.Getenv("LCX_WORKFLOW")); path != "" {
		var err error
		workflow, err = os.ReadFile(path)
		if err != nil {
			return lcx.AgentOptions{}, fmt.Errorf("read LCX_WORKFLOW %q: %w", path, err)
		}
	}
	var workUnits []byte
	if path := strings.TrimSpace(os.Getenv("LCX_WORK_UNITS")); path != "" {
		var err error
		workUnits, err = os.ReadFile(path)
		if err != nil {
			return lcx.AgentOptions{}, fmt.Errorf("read LCX_WORK_UNITS %q: %w", path, err)
		}
	}
	opts := lcx.AgentOptions{
		ProviderName: "openai-compatible",
		Policy: policy,
		MaxSteps: maxSteps,
		MaxTokens: maxTokens,
		RecentMessages: recentMessages,
		MaxToolCallsPerStep: maxToolCalls,
		Workflow: workflow,
		WorkUnits: workUnits,
	}

	profilePath := strings.TrimSpace(os.Getenv("LCX_COGNITION_PROFILE"))
	defaultProfile := filepath.Join(workspace, ".lumencortex", "cognition.json")
	_, defaultProfileErr := os.Stat(defaultProfile)
	opts.CognitionEnabled = envBool("LCX_COGNITION") || profilePath != "" || defaultProfileErr == nil
	if !opts.CognitionEnabled {
		return opts, nil
	}

	config, err := cognition.LoadConfig(workspace, profilePath)
	if err != nil {
		return lcx.AgentOptions{}, fmt.Errorf("load cognition profile: %w", err)
	}
	if engine != nil {
		engine.ConfigureCognitionHealth(lcx.CognitionHealthOptions{
			FailureThreshold: config.Health.FailureThreshold,
			CooldownMS: config.Health.CooldownMS,
		})
	}

	categoryProviders, err := categoryProvidersFromConfig(config)
	if err != nil {
		return lcx.AgentOptions{}, err
	}
	decisionProviders, err := decisionProvidersFromConfig(config)
	if err != nil {
		return lcx.AgentOptions{}, err
	}
	opts.CategoryProviders = categoryProviders
	opts.DecisionProviders = decisionProviders
	opts.DecisionPolicy = config.Decision.Policy
	return opts, nil
}

func streamEvents(engine *lcx.Engine) func() {
	events, stop := engine.Events(256)
	go func() {
		enc := json.NewEncoder(os.Stderr)
		for event := range events {
			_ = enc.Encode(event)
		}
	}()
	return stop
}

func envBool(name string) bool {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return false
	}
	parsed, err := strconv.ParseBool(value)
	return err == nil && parsed
}

func envInt(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		return fallback
	}
	return parsed
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}


type providerPreset struct {
	BaseURL         string
	APIKeyEnv       string
	DefaultModel    string
	ReasoningFormat string
	Headers         map[string]string
}

var cognitiveProviderPresets = map[string]providerPreset{
	"openrouter": {
		BaseURL: "https://openrouter.ai/api/v1",
		APIKeyEnv: "OPENROUTER_API_KEY",
		DefaultModel: "openrouter/free",
		ReasoningFormat: "reasoning-object",
		Headers: map[string]string{
			"HTTP-Referer": "https://github.com/edynasty/LumenCortex",
			"X-Title": "LumenCortex",
		},
	},
	"openrouter-deepseek-free": {
		BaseURL: "https://openrouter.ai/api/v1",
		APIKeyEnv: "OPENROUTER_API_KEY",
		DefaultModel: "deepseek/deepseek-v4-flash-0731:free",
		ReasoningFormat: "reasoning-object",
		Headers: map[string]string{
			"HTTP-Referer": "https://github.com/edynasty/LumenCortex",
			"X-Title": "LumenCortex DeepSeek Free",
		},
	},
	"groq": {
		BaseURL: "https://api.groq.com/openai/v1",
		APIKeyEnv: "GROQ_API_KEY",
		DefaultModel: "openai/gpt-oss-120b",
		ReasoningFormat: "reasoning-effort",
	},
	"deepseek": {
		BaseURL: "https://api.deepseek.com",
		APIKeyEnv: "DEEPSEEK_API_KEY",
		DefaultModel: "deepseek-flash",
		ReasoningFormat: "deepseek",
	},
	"generic": {},
}

func categoryProvidersFromConfig(config cognition.Config) (map[string][]lcx.ProviderBinding, error) {
	out := map[string][]lcx.ProviderBinding{}
	for category, entry := range config.Categories {
		for _, spec := range entry.Models {
			binding, err := providerBindingFromSpec(spec)
			if err != nil {
				return nil, fmt.Errorf("category %s: %w", category, err)
			}
			out[category] = append(out[category], binding)
		}
	}
	return out, nil
}

func providerBindingFromSpec(spec cognition.ModelSpec) (lcx.ProviderBinding, error) {
	name := strings.TrimSpace(spec.Provider)
	if name == "" {
		name = "generic"
	}
	preset, ok := cognitiveProviderPresets[name]
	if !ok {
		return lcx.ProviderBinding{}, fmt.Errorf("unknown provider %q", name)
	}
	model := strings.TrimSpace(spec.Model)
	if model == "" {
		model = preset.DefaultModel
	}
	if model == "" {
		model = strings.TrimSpace(os.Getenv("LCX_MODEL"))
	}
	if model == "" {
		return lcx.ProviderBinding{}, fmt.Errorf("provider %s model is required", name)
	}
	baseURL := strings.TrimSpace(spec.BaseURL)
	if baseURL == "" {
		baseURL = preset.BaseURL
	}
	if baseURL == "" {
		baseURL = strings.TrimSpace(os.Getenv("LCX_BASE_URL"))
	}
	apiKey := spec.APIKey
	apiKeyEnv := strings.TrimSpace(spec.APIKeyEnv)
	if apiKeyEnv == "" {
		apiKeyEnv = preset.APIKeyEnv
	}
	if apiKey == "" && apiKeyEnv != "" {
		apiKey = os.Getenv(apiKeyEnv)
	}
	headers := map[string]string{}
	for key, value := range preset.Headers {
		headers[key] = value
	}
	for key, value := range spec.Headers {
		headers[key] = value
	}
	var httpClient *http.Client
	if spec.TimeoutMS > 0 {
		httpClient = &http.Client{Timeout: time.Duration(spec.TimeoutMS) * time.Millisecond}
	}
	provider, err := openai.New(openai.Config{
		BaseURL: baseURL,
		APIKey: apiKey,
		Model: model,
		Headers: headers,
		HTTPClient: httpClient,
		ReasoningFormat: preset.ReasoningFormat,
		DisableStreaming: envBool("LCX_DISABLE_STREAMING"),
		DisableRetries: envBool("LCX_DISABLE_RETRIES"),
	})
	if err != nil {
		return lcx.ProviderBinding{}, err
	}
	return lcx.ProviderBinding{Name: name, Provider: provider}, nil
}

func decisionProvidersFromConfig(config cognition.Config) ([]lcx.DecisionProvider, error) {
	out := []lcx.DecisionProvider{}
	for _, spec := range config.Decision.Providers {
		provider, err := decisionProviderFromSpec(spec)
		if err != nil {
			return nil, err
		}
		if provider != nil {
			out = append(out, provider)
		}
	}
	return out, nil
}

func decisionProviderFromSpec(spec cognition.DecisionProviderSpec) (lcx.DecisionProvider, error) {
	kind := strings.ToLower(strings.TrimSpace(spec.Type))
	if kind == "" {
		kind = "systemone"
	}
	if kind == "algorithm" {
		return nil, nil
	}
	apiKey := spec.APIKey
	apiKeyEnv := strings.TrimSpace(spec.APIKeyEnv)
	if apiKeyEnv == "" {
		switch kind {
		case "jev":
			apiKeyEnv = "TYPESAFE_API_KEY"
		case "laya":
			apiKeyEnv = "LAYA_API_KEY"
		}
	}
	if apiKey == "" && apiKeyEnv != "" {
		apiKey = os.Getenv(apiKeyEnv)
	}
	cfg := cognition.SystemOneConfig{
		Name: spec.Name,
		BaseURL: spec.BaseURL,
		APIKey: apiKey,
		Model: spec.Model,
		Headers: spec.Headers,
	}
	if spec.TimeoutMS > 0 {
		cfg.Timeout = time.Duration(spec.TimeoutMS) * time.Millisecond
	}
	switch kind {
	case "jev":
		return cognition.NewJevDecisionProvider(cfg)
	case "laya":
		return cognition.NewLayaDecisionProvider(cfg)
	case "systemone":
		return cognition.NewSystemOneProvider(cfg)
	default:
		return nil, fmt.Errorf("unknown decision provider type %q", kind)
	}
}
