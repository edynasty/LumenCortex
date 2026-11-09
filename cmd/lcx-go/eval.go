package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/edynasty/LumenCortex/evalapi"
	"github.com/edynasty/LumenCortex/evalruntime"
	lcx "github.com/edynasty/LumenCortex/runtime"
)

func evalOptionsFromEnv() *lcx.EvalOptions {
	if !envBool("LCX_EVAL_ENABLED") {
		return nil
	}
	opts := &lcx.EvalOptions{Python: strings.TrimSpace(os.Getenv("LCX_EVAL_PYTHON")), JavaScript: strings.TrimSpace(os.Getenv("LCX_EVAL_JAVASCRIPT")), Bubblewrap: strings.TrimSpace(os.Getenv("LCX_EVAL_BUBBLEWRAP")), RequireCallerIsolation: envBool("LCX_EVAL_REQUIRE_CALLER_ISOLATION"), RunID: strings.TrimSpace(os.Getenv("LCX_EVAL_RUN_ID")), TrialID: strings.TrimSpace(os.Getenv("LCX_EVAL_TRIAL_ID"))}
	if envBool("LCX_EVAL_REQUIRE_BOUNDARY") {
		opts.Boundary = &evalapi.BoundaryContext{CohortID: os.Getenv("LCX_EVAL_COHORT_ID"), JobID: opts.RunID, TrialID: opts.TrialID, ExecutionID: os.Getenv("LCX_EVAL_EXECUTION_ID")}
	}
	if socket := strings.TrimSpace(os.Getenv("LCX_EVAL_BROKER_SOCKET")); socket != "" {
		opts.Broker = &evalruntime.BrokerOptions{Socket: socket, GrantID: os.Getenv("LCX_EVAL_BROKER_GRANT")}
		// A process-bound grant is consumed at connect; do not leave it in the
		// environment inherited by any subsequent command.
		_ = os.Unsetenv("LCX_EVAL_BROKER_GRANT")
	}
	return opts
}

// serve retains the same Engine and workers across JSONL requests. Closing stdin or
// cancelling the CLI disposes all workers; no model/provider is used by this command.
func runEvalCommand(ctx context.Context, engine *lcx.Engine, args []string) error {
	if len(args) == 1 && args[0] == "runtimes" {
		value, err := engine.EvalRuntimes(ctx)
		if err != nil {
			return err
		}
		return printJSON(value)
	}
	if len(args) != 2 || args[0] != "serve" {
		return errors.New("usage: LCX_EVAL_ENABLED=1 lcx-go eval runtimes | eval serve <lc-session-id> (JSONL on stdin)")
	}
	sessionID := args[1]
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopClose := context.AfterFunc(ctx, func() { _ = os.Stdin.Close() })
	defer stopClose()
	var pending sync.WaitGroup
	defer func() { cancel(); pending.Wait() }()
	slots := make(chan struct{}, 2)
	var outputMu sync.Mutex
	input := bufio.NewScanner(os.Stdin)
	input.Buffer(make([]byte, 4096), 1<<20)
	output := json.NewEncoder(os.Stdout)
	respond := func(id string, value any, err error) error {
		reply := map[string]any{"id": id, "result": value}
		if err != nil {
			reply = map[string]any{"id": id, "error": err.Error()}
		}
		outputMu.Lock()
		defer outputMu.Unlock()
		return output.Encode(reply)
	}
	for input.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		var call struct {
			ID              string          `json:"id"`
			Method          string          `json:"method"`
			RuntimeID       string          `json:"runtimeId"`
			EvalSessionID   string          `json:"evalSessionId"`
			Request         evalapi.Request `json:"request"`
			Action          string          `json:"action"`
			ArtifactID      string          `json:"artifactId"`
			Path            string          `json:"path"`
			TargetSessionID string          `json:"targetSessionId"`
			// RequestID and Generation name the execution an interrupt stops.
			RequestID  string `json:"requestId"`
			Generation uint64 `json:"generation"`
		}
		var value any
		var err error
		if err = json.Unmarshal(input.Bytes(), &call); err == nil {
			if call.Method == "execute" {
				select {
				case slots <- struct{}{}:
					pending.Add(1)
					go func(id string, request evalapi.Request) {
						defer pending.Done()
						defer func() { <-slots }()
						value, err := engine.ExecuteEval(ctx, sessionID, request)
						if err := respond(id, value, err); err != nil {
							cancel()
						}
					}(call.ID, call.Request)
				default:
					if err := respond(call.ID, nil, errors.New("two CLI executions are already active; cancel or await one")); err != nil {
						return err
					}
				}
				continue
			}
			switch call.Method {
			case "discover":
				value, err = engine.EvalRuntimes(ctx)
			case "create":
				value, err = engine.CreateEvalSession(ctx, sessionID, call.RuntimeID)
			case "sessions":
				value, err = engine.EvalSessions(ctx, sessionID)
			case "health":
				value, err = engine.EvalHealth(ctx, sessionID, call.EvalSessionID)
			case "results":
				value, err = engine.EvalResults(ctx, sessionID, call.EvalSessionID)
			case "artifact":
				value, err = engine.TransferEvalArtifact(ctx, sessionID, call.Action, call.EvalSessionID, call.ArtifactID, call.Path, call.TargetSessionID)
			case "interrupt":
				if strings.TrimSpace(call.RequestID) == "" {
					err = errors.New("interrupt needs requestId: the request id of the execution to stop")
					break
				}
				value, err = engine.EvalInterrupt(ctx, sessionID, call.EvalSessionID, evalapi.InterruptTarget{RequestID: call.RequestID, Generation: call.Generation})
			case "reset":
				err = engine.EvalReset(ctx, sessionID, call.EvalSessionID)
			case "close":
				err = engine.EvalClose(ctx, sessionID, call.EvalSessionID)
			default:
				err = fmt.Errorf("unsupported eval method %q", call.Method)
			}
		}
		if err := respond(call.ID, value, err); err != nil {
			return err
		}
	}
	return input.Err()
}
