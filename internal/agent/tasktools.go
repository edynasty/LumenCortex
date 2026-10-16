package agent

import (
	"encoding/json"
	"errors"
	"fmt"
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
					"verification":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"depends_on":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				},
				"required":             []string{"goal"},
				"additionalProperties": false,
			},
		},
		{
			Name:        toolWorkUnitUpdate,
			Description: "Update Work Unit status, evidence and verification. Completion is gated by required evidence and passed checks.",
			Permission:  "read",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":                   map[string]any{"type": "string"},
					"status":               map[string]any{"type": "string", "enum": []string{"pending", "active", "blocked", "verifying", "completed", "failed", "cancelled"}},
					"summary":              map[string]any{"type": "string"},
					"evidence":             map[string]any{"type": "array"},
					"verification_results": map[string]any{"type": "array"},
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
// cancels is noted, so the evidence rule does not take it as acceptance.
func executeTaskTool(r *run, name string, args map[string]any, at time.Time) protocol.ToolResult {
	payload, err := runTaskTool(r, name, args, at)
	if err != nil {
		return protocol.ToolResult{OK: false, Content: toolErrorContent(err.Error())}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return protocol.ToolResult{OK: false, Content: toolErrorContent(err.Error())}
	}
	return protocol.ToolResult{OK: true, Content: string(raw)}
}

func runTaskTool(r *run, name string, args map[string]any, at time.Time) (any, error) {
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
		unit, err := r.units.Add(units[0])
		if err != nil {
			return nil, err
		}
		r.ts.SetUnits(r.units)
		r.ts.Reopen(at)
		return map[string]any{"unit": unit, "active": r.units.Current()}, nil
	case toolWorkUnitUpdate:
		return updateWorkUnit(r, args)
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

func updateWorkUnit(r *run, args map[string]any) (any, error) {
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
	if value, ok := args["verification_results"]; ok {
		var items []taskstate.VerificationResult
		if err := remarshal(value, &items); err != nil {
			return nil, err
		}
		patch.VerificationResults = &items
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
	return map[string]any{"unit": unit, "active": active, "remaining": r.units.Incomplete()}, nil
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
		". Use work_unit_update to satisfy evidence/verification and complete them before finishing."
}
