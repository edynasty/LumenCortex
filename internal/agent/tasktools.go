package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/edynasty/LumenCortex/internal/taskstate"
	"github.com/edynasty/LumenCortex/protocol"
)

// Task tools are the run's own tools over Task State: work units and reference suggestions. They
// run inside the loop, never through Tools, and change only the run's in-memory state; the loop
// commits the version with the tool row.
const (
	toolWorkUnitList     = "work_unit_list"
	toolWorkUnitCreate   = "work_unit_create"
	toolWorkUnitUpdate   = "work_unit_update"
	toolReferenceSuggest = "reference_suggest"
)

func taskToolSpecs() []protocol.ToolSpec {
	return []protocol.ToolSpec{
		{
			Name:        toolWorkUnitList,
			Description: "Inspect persistent Work Units for this Agent session.",
			Permission:  "read",
			Parameters:  map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false},
		},
		{
			Name:        toolWorkUnitCreate,
			Description: "Create a persistent Work Unit. Work Units cannot choose providers, models, Categories or reasoning effort.",
			Permission:  "read",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":                map[string]any{"type": "string"},
					"goal":              map[string]any{"type": "string"},
					"description":       map[string]any{"type": "string"},
					"risk":              map[string]any{"type": "string", "enum": []string{"low", "medium", "high", "critical"}},
					"required_evidence": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"checks": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
						"description": "Ids of configured checks this unit must pass. Completing the unit runs them; only their results count."},
					"depends_on": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				},
				"required":             []string{"goal"},
				"additionalProperties": false,
			},
		},
		{
			Name: toolWorkUnitUpdate,
			Description: "Update Work Unit status, summary and evidence. Setting status completed runs the unit's checks through the runtime evaluator; " +
				"the unit completes only if all of them pass. Check results cannot be written by this tool.",
			Permission: "read",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":       map[string]any{"type": "string"},
					"status":   map[string]any{"type": "string", "enum": []string{"pending", "active", "blocked", "verifying", "completed", "failed", "cancelled"}},
					"summary":  map[string]any{"type": "string"},
					"evidence": map[string]any{"type": "array"},
				},
				"required":             []string{"id"},
				"additionalProperties": false,
			},
		},
		{
			Name: toolReferenceSuggest,
			Description: "Suggest a file, directory or earlier thread as context for this task. The user decides whether to include it; " +
				"a target the user excluded cannot be suggested again.",
			Permission: "read",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"kind":   map[string]any{"type": "string", "enum": []string{taskstate.RefPath, taskstate.RefThread}},
					"target": map[string]any{"type": "string", "description": "workspace-relative path, or a session id for a thread"},
					"note":   map[string]any{"type": "string", "description": "why it is relevant, at most 512 bytes"},
				},
				"required":             []string{"kind", "target"},
				"additionalProperties": false,
			},
		},
	}
}

// filterTaskToolSpecs returns the task tools the allowlist admits (all of them for a nil allowlist).
func filterTaskToolSpecs(allow []string) []protocol.ToolSpec {
	specs := taskToolSpecs()
	if allow == nil {
		return specs
	}
	allowed := map[string]bool{}
	for _, name := range allow {
		allowed[name] = true
	}
	out := make([]protocol.ToolSpec, 0, len(specs))
	for _, spec := range specs {
		if allowed[spec.Name] {
			out = append(out, spec)
		}
	}
	return out
}

func isTaskTool(name string) bool {
	switch name {
	case toolWorkUnitList, toolWorkUnitCreate, toolWorkUnitUpdate, toolReferenceSuggest:
		return true
	}
	return false
}

// executeTaskTool applies one task tool to the run's state. A created unit is a new obligation and
// reopens a resolved thread; a completed unit is progress and resets the stall; a unit the model
// cancels is noted, so the evidence rule does not take it as acceptance. The ExitCause is non-nil
// only when deciding a unit's checks was cancelled or failed: the run then ends.
func executeTaskTool(ctx context.Context, checks Checks, r *run, name string, args map[string]any, at time.Time) (protocol.ToolResult, *ExitCause) {
	payload, err := runTaskTool(ctx, checks, r, name, args, at)
	var refused *toolRefusal
	switch {
	case errors.As(err, &refused):
		raw, _ := json.Marshal(refused.payload)
		return protocol.ToolResult{OK: false, Content: string(raw)}, nil
	case errors.As(err, new(*checkFailure)):
		cause := checkFailureExit(ctx, errors.Unwrap(err))
		return protocol.ToolResult{}, &cause
	case err != nil:
		return protocol.ToolResult{OK: false, Content: toolErrorContent(err.Error())}, nil
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return protocol.ToolResult{OK: false, Content: toolErrorContent(err.Error())}, nil
	}
	return protocol.ToolResult{OK: true, Content: string(raw)}, nil
}

// toolRefusal is a task tool call that did not take effect in full but changed the state: its payload
// is the tool result.
type toolRefusal struct{ payload map[string]any }

func (r *toolRefusal) Error() string { return fmt.Sprint(r.payload["error"]) }

// checkFailure is a failure to decide checks; it ends the run.
type checkFailure struct{ err error }

func (c *checkFailure) Error() string { return c.err.Error() }
func (c *checkFailure) Unwrap() error { return c.err }

func runTaskTool(ctx context.Context, checks Checks, r *run, name string, args map[string]any, at time.Time) (any, error) {
	switch name {
	case toolWorkUnitList:
		active, err := r.units.EnsureActive()
		if err != nil {
			return nil, err
		}
		r.ts.SetUnits(r.units)
		return map[string]any{"active": active, "units": r.units.List()}, nil
	case toolWorkUnitCreate:
		raw, err := json.Marshal([]any{args})
		if err != nil {
			return nil, err
		}
		units, err := taskstate.ParseWorkUnits(raw)
		if err != nil {
			return nil, err
		}
		if len(units) != 1 {
			return nil, errors.New("invalid work unit")
		}
		if err := knownChecks(checks, units[0].Checks); err != nil {
			return nil, err
		}
		unit, err := r.units.Add(units[0])
		if err != nil {
			return nil, err
		}
		r.ts.SetUnits(r.units)
		r.ts.Reopen(at)
		return map[string]any{"unit": unit, "active": r.units.Current()}, nil
	case toolWorkUnitUpdate:
		return updateWorkUnit(ctx, checks, r, args)
	case toolReferenceSuggest:
		kind, _ := args["kind"].(string)
		target, _ := args["target"].(string)
		note, _ := args["note"].(string)
		ref, added, err := r.ts.Suggest(kind, target, note, at)
		if err != nil {
			return nil, err
		}
		return map[string]any{"reference": ref, "added": added}, nil
	}
	return nil, fmt.Errorf("unknown task tool: %s", name)
}

// knownChecks refuses check ids the configuration does not define, so a unit cannot list a check that
// could never be decided.
func knownChecks(checks Checks, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	if checks == nil {
		return errors.New("no checks are configured for this run")
	}
	all, _ := checks.Configured()
	for _, id := range ids {
		if !slices.Contains(all, id) {
			return fmt.Errorf("check %q is not configured; configured checks: %s", id, strings.Join(all, ", "))
		}
	}
	return nil
}

// updateArgs are the only arguments work_unit_update takes; any other key, a check result included,
// is refused.
var updateArgs = map[string]bool{"id": true, "status": true, "summary": true, "evidence": true}

func updateWorkUnit(ctx context.Context, checks Checks, r *run, args map[string]any) (any, error) {
	for key := range args {
		if !updateArgs[key] {
			return nil, fmt.Errorf("%s cannot set %s: check results come only from the runtime evaluator", toolWorkUnitUpdate, key)
		}
	}
	id := strings.TrimSpace(fmt.Sprint(args["id"]))
	if args["id"] == nil || id == "" {
		return nil, fmt.Errorf("%s requires id", toolWorkUnitUpdate)
	}
	patch := taskstate.WorkUnitPatch{}
	if value, ok := args["status"]; ok {
		status := taskstate.WorkUnitStatus(strings.TrimSpace(fmt.Sprint(value)))
		patch.Status = &status
	}
	if value, ok := args["summary"]; ok {
		summary := fmt.Sprint(value)
		patch.Summary = &summary
	}
	if value, ok := args["evidence"]; ok {
		var items []taskstate.EvidenceRef
		if err := remarshal(value, &items); err != nil {
			return nil, err
		}
		patch.Evidence = &items
	}
	current, ok := r.units.State.Items[id]
	if !ok {
		return nil, fmt.Errorf("unknown work unit: %s", id)
	}
	// Completing a unit with checks decides them first (design §4.4). The verdicts are recorded on the
	// unit whether or not they pass, and the tool result carries them.
	var verdicts []verdictView
	if patch.Status != nil && *patch.Status == taskstate.WorkCompleted && current.Status != taskstate.WorkCompleted && len(current.Checks) > 0 {
		if checks == nil {
			return nil, fmt.Errorf("work unit %s has checks, but this run cannot decide checks", id)
		}
		decided, err := checks.Decide(ctx, current.Checks, id)
		if err != nil {
			return nil, &checkFailure{err: err}
		}
		verdicts = verdictViews(decided)
		results := unitChecks(decided)
		patch.CheckResults = &results
		if len(failing(decided)) > 0 {
			recorded := patch
			recorded.Status = nil
			unit, err := r.units.Update(id, recorded)
			if err != nil {
				return nil, err
			}
			r.ts.SetUnits(r.units)
			return nil, &toolRefusal{payload: map[string]any{
				"error":    fmt.Sprintf("work unit %s was not completed: %s", id, strings.Join(unit.FailingChecks(), ", ")),
				"unit":     unit,
				"verdicts": verdicts,
			}}
		}
	}
	unit, err := r.units.Update(id, patch)
	if err != nil {
		return nil, err
	}
	switch unit.Status {
	case taskstate.WorkCompleted:
		r.ts.Progress.ResetStall()
	case taskstate.WorkCancelled:
		r.ts.NoteCancelled(unit.ID)
	}
	active, _ := r.units.EnsureActive()
	r.ts.SetUnits(r.units)
	out := map[string]any{"unit": unit, "active": active, "remaining": r.units.Incomplete()}
	if verdicts != nil {
		out["verdicts"] = verdicts
	}
	return out, nil
}

func remarshal(value any, out any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

func workUnitCompletionCorrection(units []taskstate.WorkUnit) string {
	var parts []string
	for _, unit := range units {
		parts = append(parts, fmt.Sprintf("%s[%s]: %s", unit.ID, unit.Status, unit.Goal))
	}
	return "Work Unit completion gate rejected final completion. Remaining units: " + strings.Join(parts, "; ") +
		". Use work_unit_update to record evidence and complete them; completing a unit runs its checks."
}
