package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/taskstate"
	"github.com/edynasty/LumenCortex/internal/toolset"
	"github.com/edynasty/LumenCortex/protocol"
)

// toolStartTask is the delegation tool (ADR-0006 D4). It is not a mode of spawn_subagent: that tool
// is open to read-only runs, this one only to a run granted delegation.
const toolStartTask = "start_task"

// delegationGranted reports the grant a run actually holds: isolated needs a TaskStarter.
func (l *Loop) delegationGranted(delegation string) string {
	switch {
	case delegation == DelegationPropose:
		return DelegationPropose
	case delegation == DelegationIsolated && l.Tasks != nil:
		return DelegationIsolated
	}
	return DelegationNone
}

// delegationSpecs is start_task's spec under the run's grant, none without one or when the allowlist
// excludes it. agents are the catalog agents start_task may run (workspace ceiling, delegation use).
func (l *Loop) delegationSpecs(delegation string, allow []string, agents []config.AgentInfo) []protocol.ToolSpec {
	grant := l.delegationGranted(delegation)
	if grant == DelegationNone || (allow != nil && !slices.Contains(allow, toolStartTask)) {
		return nil
	}
	description := "Propose an execution task: an autonomous run in its own git worktree, based on the committed HEAD, " +
		"with file read and edit tools but no shell, that works the objective until the configured checks pass. " +
		"The call only records the proposal and returns approval_required; the user decides whether to start it. " +
		"Propose a task when the user asks for a change to be made, not to answer a question."
	if grant == DelegationIsolated {
		description = "Start an execution task: an autonomous run in its own git worktree, based on the committed HEAD, " +
			"with file read and edit tools but no shell, that works the objective until the configured checks pass. " +
			"It runs on its own; its outcome arrives in a later turn as a task_result row. " +
			"Its changes stay in its worktree until the user applies them. Start a task when the user asks for a change to be made."
	}
	properties := map[string]any{
		"objective": map[string]any{"type": "string", "description": "the complete, self-contained task: what to change and how to know it is done"},
	}
	if len(agents) > 0 {
		description += " Agents that can run it:" + toolset.AgentCatalogLines(agents)
		properties["agent"] = toolset.AgentParameter(agents, config.AgentWorker)
	}
	return []protocol.ToolSpec{{
		Name:        toolStartTask,
		Description: description,
		Permission:  "delegate",
		Parameters: map[string]any{
			"type":                 "object",
			"properties":           properties,
			"required":             []string{"objective"},
			"additionalProperties": false,
		},
	}}
}

// startTask executes start_task. The grant is checked again here, whatever the specs offered: without
// it the call is denied and nothing changes. Under propose the proposal is recorded in the run's Task
// State, committed with the tool row; under isolated the child is started first and recorded after,
// and a started task reopens the thread.
func (l *Loop) startTask(ctx context.Context, r *run, opts Options, args map[string]any, at time.Time) protocol.ToolResult {
	grant := l.delegationGranted(opts.Delegation)
	if grant == DelegationNone {
		return protocol.ToolResult{OK: false, Denied: true, Permission: "delegate", Content: toolErrorContent("start_task requires a delegation grant")}
	}
	// An agent outside the enum is refused before anything is recorded or started (AG5).
	agentID, _ := args["agent"].(string)
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		agentID = config.AgentWorker
	} else if !slices.Contains(toolset.AgentIDs(opts.TaskAgents), agentID) {
		return protocol.ToolResult{OK: false, Content: toolErrorContent(fmt.Sprintf("start_task cannot run agent %q; choose one of: %s",
			agentID, strings.Join(toolset.AgentIDs(opts.TaskAgents), ", ")))}
	}
	objective, _ := args["objective"].(string)
	objective, err := taskstate.ValidTaskObjective(objective)
	if err == nil && len(r.ts.Tasks) >= taskstate.MaxTasks {
		// Checked before a child is started, so an isolated child is never left unrecorded.
		err = taskstate.ErrTooManyTasks
	}
	if err != nil {
		return protocol.ToolResult{OK: false, Content: toolErrorContent(err.Error())}
	}
	var payload map[string]any
	if grant == DelegationPropose {
		bounds := &taskstate.TaskBounds{MaxSteps: opts.MaxSteps, MaxToolCallsPerStep: opts.MaxToolCallsPerStep,
			MaxTokens: opts.MaxTokens, ToolDenylist: slices.Clone(opts.ToolDenylist)}
		task, err := r.ts.ProposeTask(objective, agentID, bounds, at)
		if err != nil {
			return protocol.ToolResult{OK: false, Content: toolErrorContent(err.Error())}
		}
		payload = map[string]any{"status": "approval_required", "task": task,
			"message": "The proposal is recorded. The user decides whether to start it; do not propose it again."}
	} else {
		sessionID, detail, err := l.Tasks.StartTask(ctx, objective, agentID)
		if err != nil {
			return protocol.ToolResult{OK: false, Content: toolErrorContent(err.Error())}
		}
		task, err := r.ts.AddStartedTask(objective, agentID, sessionID, at)
		if err != nil {
			return protocol.ToolResult{OK: false, Content: toolErrorContent(err.Error())}
		}
		r.ts.Reopen(at)
		payload = map[string]any{"status": "started", "task": task, "child": detail,
			"message": "The task runs on its own. Its outcome arrives in a later turn as a task_result row."}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return protocol.ToolResult{OK: false, Content: toolErrorContent(err.Error())}
	}
	return protocol.ToolResult{OK: true, Content: string(raw)}
}
