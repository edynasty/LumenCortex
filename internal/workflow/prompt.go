package workflow

import (
	"encoding/json"
	"fmt"
	"strings"
)

func (r *Runtime) BlockReason() string {
	pending := r.PendingGates()
	if len(pending) > 0 {
		ids := make([]string, 0, len(pending))
		for _, gate := range pending {
			ids = append(ids, gate.ID)
		}
		return fmt.Sprintf("workflow action %s has pending gate(s): %s", r.CurrentAction, strings.Join(ids, ", "))
	}
	if !r.baseCompletionSatisfied(r.Action()) {
		return fmt.Sprintf("workflow action %s has not satisfied its completion evidence", r.CurrentAction)
	}
	if !r.Action().Terminal {
		return fmt.Sprintf("workflow is not at a terminal action; current action is %s", r.CurrentAction)
	}
	return "workflow completion is not yet proven"
}

func (r *Runtime) Prompt() string {
	action := r.Action()
	gates := make([]map[string]any, 0, len(action.Gates))
	for _, gate := range action.Gates {
		gates = append(gates, map[string]any{
			"id":        gate.ID,
			"type":      gate.Type,
			"title":     gate.Title,
			"satisfied": r.gateSatisfied(gate),
		})
	}
	payload := map[string]any{
		"workflow": map[string]any{
			"id": r.Definition.ID, "title": r.Definition.Title, "status": r.Status,
		},
		"currentAction": map[string]any{
			"id": r.CurrentAction, "title": action.Title, "description": action.Description,
			"terminal": action.Terminal, "allowedTools": action.AllowedTools,
			"completeWhen": action.CompleteWhen, "gates": gates,
		},
		"facts": r.Facts,
	}
	raw, _ := json.MarshalIndent(payload, "", "  ")
	if len(raw) > 12000 {
		raw = raw[:12000]
	}
	return "LumenCortex Workflow Contract is ACTIVE and binding.\n\n" +
		"Reason freely inside the current action, but obey its tool boundary and completion evidence.\n\n" +
		"Do not claim completion until the runtime reaches a satisfied terminal action.\n\n" + string(raw)
}
