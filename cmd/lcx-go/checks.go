package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	lcx "github.com/edynasty/LumenCortex/runtime"
)

const checksUsage = "usage: lcx-go checks status | lcx-go checks approve <set-digest> [--actor <name>] [--decision-ref <ref>]"

// runChecksCommand is `lcx-go checks` (ADR-0017): status shows the workspace's effective check set,
// each definition against the newest approval, and where a change came from; approve approves the set
// whose digest status printed. Both go through the Engine's approval API, the one Desktop uses.
func runChecksCommand(ctx context.Context, engine *lcx.Engine, out io.Writer, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%s", checksUsage)
	}
	switch args[0] {
	case "status":
		if len(args) != 1 {
			return fmt.Errorf("%s", checksUsage)
		}
		state, err := engine.CheckApprovalState(ctx)
		if err != nil {
			return err
		}
		return writeJSON(out, state)
	case "approve":
		req, err := parseApproveChecks(args[1:])
		if err != nil {
			return err
		}
		state, err := engine.ApproveChecks(ctx, req)
		if err != nil {
			return err
		}
		return writeJSON(out, state)
	default:
		return fmt.Errorf("unknown checks command %q; %s", args[0], checksUsage)
	}
}

// parseApproveChecks reads `approve <set-digest> [--actor <name>] [--decision-ref <ref>]`. The actor
// defaults to the OS user; the approval is the user's (approvedBy "user").
func parseApproveChecks(args []string) (lcx.ApproveChecksRequest, error) {
	req := lcx.ApproveChecksRequest{ApprovedBy: lcx.ApprovedByUser, Actor: os.Getenv("USER")}
	if req.Actor == "" {
		req.Actor = "human"
	}
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "--actor" || arg == "--decision-ref":
			if i+1 >= len(args) {
				return lcx.ApproveChecksRequest{}, fmt.Errorf("%s needs a value; %s", arg, checksUsage)
			}
			i++
			if arg == "--actor" {
				req.Actor = args[i]
			} else {
				req.DecisionRef = args[i]
			}
		case strings.HasPrefix(arg, "-"):
			return lcx.ApproveChecksRequest{}, fmt.Errorf("unknown flag %q; %s", arg, checksUsage)
		case req.SetDigest == "":
			req.SetDigest = arg
		default:
			return lcx.ApproveChecksRequest{}, fmt.Errorf("%s", checksUsage)
		}
	}
	if req.SetDigest == "" {
		return lcx.ApproveChecksRequest{}, fmt.Errorf("approve names the set digest `lcx-go checks status` printed; %s", checksUsage)
	}
	return req, nil
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
