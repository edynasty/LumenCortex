package toolset

import (
	"context"
	"github.com/edynasty/LumenCortex/protocol"
)

// CognitiveAccess is bound to one session/run by Engine, never by model input.
// It can propose and inspect; host approvals remain outside this interface.
type CognitiveAccess interface {
	CognitiveCall(context.Context, string, map[string]any) (protocol.ToolResult, error)
}

func (s *Set) registerCognitiveTools() {
	if s.cognitive == nil {
		return
	}
	specs := []protocol.ToolSpec{
		{Name: "recall_experiences", Description: "Find condition-bound procedures and counterexamples relevant to the task. Relevance is not correctness; only adopt_experience checks present applicability.", Permission: "read", Parameters: obj(map[string]any{"query": str()}, "query")},
		{Name: "adopt_experience", Description: "Check an experience's current preconditions and record adoption for this run. Returns a bounded strategy; adoption is not execution or success.", Permission: "read", Parameters: obj(map[string]any{"id": str(), "version": map[string]any{"type": "integer", "minimum": 1}}, "id", "version")},
		{Name: "apply_experience", Description: "Apply an adopted procedure with explicit ordinary tool actions. Core rechecks conditions, executes under the same tool policy/hooks/budget, and records real outcome feedback after the run. Never replays a previous application.", Permission: "read", MutatesWorkspace: true, Parameters: obj(map[string]any{"id": str(), "adoptionId": str(), "actions": map[string]any{"type": "array", "minItems": 1, "maxItems": 8, "items": obj(map[string]any{"tool": str(), "arguments": map[string]any{"type": "object"}}, "tool", "arguments")}}, "id", "adoptionId", "actions")},
		{Name: "propose_experience", Description: "Propose a conditional lesson from this run's observable actions. It remains pending until the run ends; Core records its real source and validates using existing host-bound checks. No model-assigned evidence, success or authority.", Permission: "read", Parameters: obj(map[string]any{"proposal": map[string]any{"type": "object"}}, "proposal")},
		{Name: "assess_cognition", Description: "Assess exact cognitive records for this task: facts, assumptions, unknowns and conflicts. Request evidence, ask, defer or stop when prerequisites are not established. No action authorization is granted.", Permission: "read", Parameters: obj(map[string]any{"query": str(), "ids": map[string]any{"type": "array", "maxItems": 16, "items": str()}, "action": map[string]any{"type": "string", "enum": []string{"continue", "gather_evidence", "ask", "defer", "stop"}}}, "query", "action")},
	}
	for _, spec := range specs {
		name := spec.Name
		s.add(spec, func(ctx context.Context, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
			return s.cognitive.CognitiveCall(ctx, name, args)
		})
	}
}
