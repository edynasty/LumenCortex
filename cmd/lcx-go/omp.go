package main

import (
	"context"
	"errors"
	"flag"
	"io"

	lcx "github.com/edynasty/LumenCortex/runtime"
)

const ompUsage = "usage: lcx-go omp list [--all] [--query text] [--limit n] [--root dir] | omp preview [--root dir] <session.jsonl> | omp import [--root dir] <session.jsonl>"

// runOMPCommand lists, previews and imports OMP sessions into this workspace (ADR-0011).
func runOMPCommand(ctx context.Context, engine *lcx.Engine, args []string) error {
	if len(args) == 0 {
		return errors.New(ompUsage)
	}
	fs := flag.NewFlagSet("omp "+args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	root := fs.String("root", "", "OMP sessions directory (default: the OMP agent directory's sessions)")
	switch args[0] {
	case "list":
		all := fs.Bool("all", false, "list sessions of every workspace")
		query := fs.String("query", "", "filter by title, id or directory")
		limit := fs.Int("limit", 0, "at most this many sessions (default 100, max 500)")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
			return errors.New(ompUsage)
		}
		items, err := engine.OMPSessions(ctx, lcx.OMPListOptions{Root: *root, All: *all, Query: *query, Limit: *limit})
		if err != nil {
			return err
		}
		return printJSON(items)
	case "preview", "import":
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 1 {
			return errors.New(ompUsage)
		}
		if args[0] == "preview" {
			preview, err := engine.PreviewOMPSession(ctx, *root, fs.Arg(0))
			if err != nil {
				return err
			}
			return printJSON(preview)
		}
		result, err := engine.ImportOMPSession(ctx, *root, fs.Arg(0))
		if err != nil {
			return err
		}
		return printJSON(result)
	default:
		return errors.New(ompUsage)
	}
}
