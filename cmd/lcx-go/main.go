package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

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
		agentOpts, err := agentOptionsFromEnv()
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
		agentOpts, err := agentOptionsFromEnv()
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

func agentOptionsFromEnv() (lcx.AgentOptions, error) {
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
	return lcx.AgentOptions{
		ProviderName: "openai-compatible",
		Policy: policy,
		MaxSteps: maxSteps,
		MaxTokens: maxTokens,
		RecentMessages: recentMessages,
		MaxToolCallsPerStep: maxToolCalls,
		Workflow: workflow,
		CognitionEnabled: envBool("LCX_COGNITION"),
	}, nil
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
