package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	lcx "github.com/edynasty/LumenCortex/runtime"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "lcx-go:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()
	workspace, err := os.Getwd()
	if err != nil {
		return err
	}
	engine, err := lcx.Open(lcx.Options{Workspace: workspace})
	if err != nil {
		return err
	}
	defer engine.Close()

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
	default:
		return fmt.Errorf("unknown command %q (preview commands: health, session-new, sessions, shell)", args[0])
	}
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
