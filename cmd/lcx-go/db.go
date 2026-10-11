package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	lcx "github.com/edynasty/LumenCortex/runtime"
)

const dbUsage = "usage: lcx-go db downgrade --to <N>"

// runDBCommand runs `lcx-go db <subcommand>`. It never opens an Engine: an Engine would hold the
// shared schema lock the downgrade needs exclusively, and would upgrade the schema first.
func runDBCommand(ctx context.Context, opts lcx.Options, args []string) error {
	if len(args) == 0 || args[0] != "downgrade" {
		return fmt.Errorf("db: %s", dbUsage)
	}
	to, err := parseDowngradeTarget(args[1:])
	if err != nil {
		return fmt.Errorf("db downgrade: %w", err)
	}
	from, now, err := lcx.DowngradeSchema(ctx, opts, to)
	if err != nil {
		return fmt.Errorf("db downgrade: %w", err)
	}
	return printJSON(map[string]int{"from": from, "to": now})
}

// parseDowngradeTarget reads the required `--to <N>` (or `--to=<N>`) flag.
func parseDowngradeTarget(args []string) (int, error) {
	value, seen := "", false
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--to" && i+1 < len(args) && !seen:
			value, seen = args[i+1], true
			i++
		case strings.HasPrefix(args[i], "--to=") && !seen:
			value, seen = strings.TrimPrefix(args[i], "--to="), true
		default:
			return 0, fmt.Errorf("unexpected argument %q; %s", args[i], dbUsage)
		}
	}
	if !seen {
		return 0, fmt.Errorf("--to is required; %s", dbUsage)
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("--to must be a non-negative integer, got %q", value)
	}
	return n, nil
}
