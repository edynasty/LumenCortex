package main

import (
	"context"
	"encoding/json"
	"errors"
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
		parsed, err := parseAgentCommandArgs(args[1:])
		if err != nil {
			return fmt.Errorf("session-new: %w", err)
		}
		h, err := engine.NewSession(ctx, lcx.SessionOptions{Goal: parsed.Goal})
		if err != nil {
			return err
		}
		if parsed.Worktree {
			identity, err := engine.AttachWorktree(ctx, h.ID, parsed.Base)
			if err != nil {
				return err
			}
			return printJSON(map[string]any{"session": h, "runtime": identity})
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
		parsed, err := parseAgentCommandArgs(args[1:])
		if err != nil {
			return fmt.Errorf("agent: %w", err)
		}
		provider, err := providerFromEnv()
		if err != nil {
			return err
		}
		h, err := engine.NewSession(ctx, lcx.SessionOptions{Goal: parsed.Goal, Provider: "openai-compatible", Model: provider.Model()})
		if err != nil {
			return err
		}
		if parsed.Worktree {
			if _, err := engine.AttachWorktree(ctx, h.ID, parsed.Base); err != nil {
				return err
			}
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
	case "governor":
		if len(args) < 2 {
			return fmt.Errorf("usage: lcx-go governor <analyze|plan|validate> [plan.json]")
		}
		profile, err := cognition.LoadConfig(workspace, strings.TrimSpace(os.Getenv("LCX_COGNITION_PROFILE")))
		if err != nil {
			return err
		}
		switch args[1] {
		case "analyze":
			analysis, err := engine.GovernorAnalyze(ctx, cognition.GovernorAnalyzeOptions{})
			if err != nil {
				return err
			}
			return printJSON(analysis)
		case "plan":
			curator, err := governorCuratorFromConfig(profile)
			if err != nil {
				return err
			}
			result, err := engine.GovernorPlan(ctx, curator, cognition.GovernorAnalyzeOptions{})
			if err != nil {
				return err
			}
			return printJSON(result)
		case "validate":
			if len(args) != 3 {
				return fmt.Errorf("usage: lcx-go governor validate <plan.json>")
			}
			raw, err := os.ReadFile(args[2])
			if err != nil {
				return err
			}
			var plan cognition.GovernorPlan
			if err := json.Unmarshal(raw, &plan); err != nil {
				return fmt.Errorf("decode Governor plan: %w", err)
			}
			validation, err := engine.GovernorValidate(ctx, plan)
			if err != nil {
				return err
			}
			return printJSON(validation)
		case "apply":
			return fmt.Errorf("lcx-go Governor is read-only; use the Node reference runtime for validated graph mutation/apply")
		default:
			return fmt.Errorf("unknown governor command %q (supported: analyze, plan, validate)", args[1])
		}
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
	case "worktree":
		return runWorktreeCommand(ctx, engine, args[1:])
	case "skills":
		return runSkillsCommand(engine, args[1:])
	default:
		return fmt.Errorf("unknown command %q (preview commands: health, session-new, sessions, shell, agent, resume, governor, approve, worktree, skills)", args[0])
	}
}

type agentCommandArgs struct {
	Goal     string
	Worktree bool
	Base     string
}

func parseAgentCommandArgs(args []string) (agentCommandArgs, error) {
	out := agentCommandArgs{
		Worktree: envBool("LCX_AGENT_WORKTREE"),
		Base:     strings.TrimSpace(os.Getenv("LCX_WORKTREE_BASE")),
	}
	if out.Base == "" {
		out.Base = "HEAD"
	}

	positionals := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		value := args[index]
		switch value {
		case "--worktree":
			out.Worktree = true
		case "--no-worktree":
			out.Worktree = false
		case "--base":
			if index+1 >= len(args) {
				return agentCommandArgs{}, fmt.Errorf("--base requires a Git ref")
			}
			index++
			out.Base = strings.TrimSpace(args[index])
			if out.Base == "" {
				return agentCommandArgs{}, fmt.Errorf("--base requires a non-empty Git ref")
			}
			out.Worktree = true
		default:
			if strings.HasPrefix(value, "--") {
				return agentCommandArgs{}, fmt.Errorf("unknown option %q", value)
			}
			positionals = append(positionals, value)
		}
	}
	out.Goal = strings.TrimSpace(strings.Join(positionals, " "))
	if out.Goal == "" {
		return agentCommandArgs{}, fmt.Errorf("usage: <command> [--worktree] [--base REF] <goal>")
	}
	return out, nil
}

func runWorktreeCommand(ctx context.Context, engine *lcx.Engine, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: lcx-go worktree <status|attach|plan|apply|remove|conflicts> ...")
	}
	switch args[0] {
	case "status":
		if len(args) != 2 {
			return fmt.Errorf("usage: lcx-go worktree status <session-id>")
		}
		result, err := engine.SessionRuntime(ctx, args[1])
		if err != nil {
			return err
		}
		return printJSON(result)
	case "attach":
		if len(args) < 2 || len(args) > 3 {
			return fmt.Errorf("usage: lcx-go worktree attach <session-id> [base]")
		}
		base := "HEAD"
		if len(args) == 3 {
			base = args[2]
		}
		result, err := engine.AttachWorktree(ctx, args[1], base)
		if err != nil {
			return err
		}
		return printJSON(result)
	case "plan":
		if len(args) != 2 {
			return fmt.Errorf("usage: lcx-go worktree plan <session-id>")
		}
		result, err := engine.WorktreeHandoffPlan(ctx, args[1])
		if err != nil {
			return err
		}
		return printJSON(result)
	case "apply":
		if len(args) < 2 || len(args) > 3 || (len(args) == 3 && args[2] != "--yes") {
			return fmt.Errorf("usage: lcx-go worktree apply <session-id> --yes")
		}
		if len(args) != 3 || args[2] != "--yes" {
			return fmt.Errorf("worktree apply requires --yes")
		}
		result, err := engine.ApplySessionWorktree(ctx, args[1])
		if err != nil {
			return err
		}
		return printJSON(result)
	case "remove":
		if len(args) < 2 || len(args) > 3 || (len(args) == 3 && args[2] != "--force") {
			return fmt.Errorf("usage: lcx-go worktree remove <session-id> [--force]")
		}
		force := len(args) == 3 && args[2] == "--force"
		if err := engine.RemoveSessionWorktree(ctx, args[1], force); err != nil {
			return err
		}
		return printJSON(map[string]any{"removed": true, "sessionId": args[1], "force": force})
	case "conflicts":
		if len(args) != 1 {
			return fmt.Errorf("usage: lcx-go worktree conflicts")
		}
		result, err := engine.WorktreeConflicts(ctx)
		if err != nil {
			return err
		}
		return printJSON(result)
	default:
		return fmt.Errorf("unknown worktree command %q (supported: status, attach, plan, apply, remove, conflicts)", args[0])
	}
}

func runSkillsCommand(engine *lcx.Engine, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: lcx-go skills <list|show|save|enable|disable|delete> ...")
	}
	switch args[0] {
	case "list":
		if len(args) > 2 {
			return fmt.Errorf("usage: lcx-go skills list [effective|global|project]")
		}
		scope := "effective"
		if len(args) == 2 {
			scope = args[1]
		}
		items, err := engine.Skills(scope)
		if err != nil {
			return err
		}
		return printJSON(items)
	case "show":
		if len(args) != 3 {
			return fmt.Errorf("usage: lcx-go skills show <global|project> <id>")
		}
		content, err := engine.SkillContent(args[1], args[2])
		if err != nil {
			return err
		}
		return printJSON(map[string]any{"scope": args[1], "id": args[2], "content": content})
	case "save":
		if len(args) != 4 {
			return fmt.Errorf("usage: lcx-go skills save <global|project> <id> <skill.md>")
		}
		raw, err := os.ReadFile(args[3])
		if err != nil {
			return err
		}
		if err := engine.SaveSkill(args[1], args[2], string(raw)); err != nil {
			return err
		}
		return printJSON(map[string]any{"saved": true, "scope": args[1], "id": args[2]})
	case "enable", "disable":
		if len(args) != 3 {
			return fmt.Errorf("usage: lcx-go skills %s <global|project> <id>", args[0])
		}
		enabled := args[0] == "enable"
		if err := engine.SetSkillEnabled(args[1], args[2], enabled); err != nil {
			return err
		}
		return printJSON(map[string]any{"scope": args[1], "id": args[2], "enabled": enabled})
	case "delete":
		if len(args) != 3 {
			return fmt.Errorf("usage: lcx-go skills delete <global|project> <id>")
		}
		if err := engine.DeleteSkill(args[1], args[2]); err != nil {
			return err
		}
		return printJSON(map[string]any{"deleted": true, "scope": args[1], "id": args[2]})
	default:
		return fmt.Errorf("unknown skills command %q (supported: list, show, save, enable, disable, delete)", args[0])
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
	if kind == "generative" || kind == "llm" || kind == "model" {
		providerName := strings.TrimSpace(spec.Provider)
		if providerName == "" || providerName == "generative" || providerName == "llm" || providerName == "model" {
			return nil, errors.New("generative decision provider requires an execution provider name")
		}
		binding, err := providerBindingFromSpec(cognition.ModelSpec{
			Provider: providerName,
			Model: spec.Model,
			BaseURL: spec.BaseURL,
			APIKey: spec.APIKey,
			APIKeyEnv: spec.APIKeyEnv,
			TimeoutMS: spec.TimeoutMS,
			Headers: spec.Headers,
		})
		if err != nil {
			return nil, err
		}
		temperature := 0.0
		if spec.Temperature != nil {
			temperature = *spec.Temperature
		}
		return cognition.NewGenerativeDecisionProvider(cognition.GenerativeDecisionConfig{
			Name: spec.Name,
			Provider: binding.Provider,
			MaxTokens: spec.MaxTokens,
			ReasoningEffort: spec.ReasoningEffort,
			Temperature: temperature,
			ConfidenceScale: spec.ConfidenceScale,
			ConfidenceCap: spec.ConfidenceCap,
			ScoreScale: spec.ScoreScale,
		})
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


func governorCuratorFromConfig(config cognition.Config) (*cognition.LLMGovernorCurator, error) {
	governor := config.Governor
	if governor == nil || !governor.Enabled {
		return nil, nil
	}
	if strings.TrimSpace(governor.Provider) == "" || strings.TrimSpace(governor.Model) == "" {
		return nil, fmt.Errorf("enabled Governor requires provider and model")
	}
	binding, err := providerBindingFromSpec(cognition.ModelSpec{
		Provider: governor.Provider,
		Model: governor.Model,
		BaseURL: governor.BaseURL,
		TimeoutMS: governor.TimeoutMS,
		Headers: governor.Headers,
	})
	if err != nil {
		return nil, err
	}
	maxTokens := governor.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 6000
	}
	effort := strings.TrimSpace(governor.ReasoningEffort)
	if effort == "" {
		effort = "high"
	}
	return &cognition.LLMGovernorCurator{
		Provider: binding.Provider,
		MaxTokens: maxTokens,
		ReasoningEffort: effort,
	}, nil
}
