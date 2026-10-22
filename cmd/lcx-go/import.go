package main

import (
	"context"
	"errors"
	"flag"
	"io"

	lcx "github.com/edynasty/LumenCortex/runtime"
)

const importUsage = "usage: lcx-go import sources | import list <source> [--all] [--query text] [--limit n] [--root dir] | import preview <source> [--root dir] <session> | import run <source> [--root dir] <session>"

// runImportCommand lists import sources and lists, previews and imports their sessions into this
// workspace through Core's import pipeline (ADR-0011, ADR-0012).
func runImportCommand(ctx context.Context, engine *lcx.Engine, args []string) error {
	if len(args) == 0 {
		return errors.New(importUsage)
	}
	if args[0] == "sources" {
		if len(args) != 1 {
			return errors.New(importUsage)
		}
		return printJSON(engine.ImportSources())
	}
	if len(args) < 2 {
		return errors.New(importUsage)
	}
	source := args[1]
	fs := flag.NewFlagSet("import "+args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	root := fs.String("root", "", "sessions directory (default: the source's own)")
	switch args[0] {
	case "list":
		all := fs.Bool("all", false, "list sessions of every workspace")
		query := fs.String("query", "", "filter by title, id or directory")
		limit := fs.Int("limit", 0, "at most this many sessions (default 100, max 500)")
		if err := fs.Parse(args[2:]); err != nil || fs.NArg() != 0 {
			return errors.New(importUsage)
		}
		items, err := engine.ListImportSessions(ctx, source, lcx.ImportListOptions{Root: *root, All: *all, Query: *query, Limit: *limit})
		if err != nil {
			return err
		}
		return printJSON(items)
	case "preview", "run":
		if err := fs.Parse(args[2:]); err != nil || fs.NArg() != 1 {
			return errors.New(importUsage)
		}
		if args[0] == "preview" {
			preview, err := engine.PreviewImport(ctx, source, *root, fs.Arg(0))
			if err != nil {
				return err
			}
			return printJSON(preview)
		}
		result, err := engine.ImportSession(ctx, source, *root, fs.Arg(0))
		if err != nil {
			return err
		}
		return printJSON(result)
	default:
		return errors.New(importUsage)
	}
}

const pluginsUsage = "usage: lcx-go plugins [list] | plugins enable <id> | plugins disable <id>"

// runPluginsCommand shows the plugin catalog and changes a plugin's enablement (ADR-0012).
func runPluginsCommand(ctx context.Context, engine *lcx.Engine, args []string) error {
	if len(args) == 0 || (args[0] == "list" && len(args) == 1) {
		return printJSON(engine.Plugins())
	}
	if len(args) != 2 || (args[0] != "enable" && args[0] != "disable") {
		return errors.New(pluginsUsage)
	}
	if err := engine.SetPluginEnabled(ctx, args[1], args[0] == "enable"); err != nil {
		return err
	}
	return printJSON(engine.Plugins())
}
