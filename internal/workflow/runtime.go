package workflow

import (
	"errors"
	"fmt"
)

func New(def *Definition, restored *State) (*Runtime, error) {
	definition, err := Validate(def)
	if err != nil {
		return nil, err
	}
	runtime := &Runtime{
		Definition:    definition,
		CurrentAction: definition.Entry,
		Facts:         deepMap(definition.Facts),
		FactSources:   map[string]any{},
		History:       []map[string]any{},
		Status:        "running",
	}
	if restored != nil {
		runtime.CurrentAction = restored.CurrentAction
		if runtime.CurrentAction == "" {
			runtime.CurrentAction = definition.Entry
		}
		if definition.Actions[runtime.CurrentAction] == nil {
			return nil, fmt.Errorf("persisted workflow references unknown action: %s", runtime.CurrentAction)
		}
		if restored.Facts != nil {
			runtime.Facts = deepMap(restored.Facts)
		}
		if restored.FactSources != nil {
			runtime.FactSources = deepMap(restored.FactSources)
		}
		if restored.History != nil {
			runtime.History = cloneSlice(restored.History)
			if len(runtime.History) > maxHistory {
				runtime.History = runtime.History[len(runtime.History)-maxHistory:]
			}
		}
		if restored.Status != "" {
			runtime.Status = restored.Status
		}
	} else {
		seedFactSources(runtime.FactSources, runtime.Facts, "")
		runtime.appendHistory(map[string]any{"at": nowISO(), "type": "start", "action": runtime.CurrentAction})
	}
	if err := runtime.assertRequirements(); err != nil {
		return nil, err
	}
	if _, err := runtime.advance("hydrate"); err != nil {
		return nil, err
	}
	return runtime, nil
}

func (r *Runtime) Action() *Action { return r.Definition.Actions[r.CurrentAction] }

func (r *Runtime) IsToolAllowed(name string) bool {
	allowed := r.Action().AllowedTools
	if allowed == nil {
		return true
	}
	for _, candidate := range *allowed {
		if candidate == name {
			return true
		}
	}
	return false
}

func (r *Runtime) EffectiveAllowlist(base []string) []string {
	actionTools := r.Action().AllowedTools
	if actionTools == nil {
		return append([]string(nil), base...)
	}
	if base == nil {
		return append([]string(nil), (*actionTools)...)
	}
	baseSet := map[string]bool{}
	for _, name := range base {
		baseSet[name] = true
	}
	out := []string{}
	for _, name := range *actionTools {
		if baseSet[name] {
			out = append(out, name)
		}
	}
	return out
}

func (r *Runtime) ObserveTool(obs ToolObservation) (ObserveResult, error) {
	before := r.CurrentAction
	changed := []ChangedFact{}
	if obs.Args == nil {
		obs.Args = map[string]any{}
	}
	if obs.Result == nil {
		obs.Result = map[string]any{}
	}
	ctx := EvalContext{Facts: r.Facts, Tool: obs.Tool, Args: obs.Args, Result: obs.Result, ResultData: parseResultContent(obs.Result["content"])}
	for _, outcome := range r.Action().Outcomes {
		matched, err := Evaluate(outcome.When, ctx)
		if err != nil {
			return ObserveResult{}, err
		}
		if !matched {
			continue
		}
		for path, value := range outcome.Set {
			previous, _ := getPath(r.Facts, path)
			if err := setPath(r.Facts, path, deepAny(value)); err != nil {
				return ObserveResult{}, err
			}
			r.FactSources[path] = map[string]any{
				"at": nowISO(), "source": "tool", "action": before, "outcome": outcome.ID,
				"tool": obs.Tool, "step": obs.Step, "args": compact(obs.Args), "result": compactResult(obs.Result, ctx.ResultData),
			}
			if !same(previous, value) {
				changed = append(changed, ChangedFact{Path: path, Previous: previous, Value: value})
			}
		}
	}
	paths := make([]string, 0, len(changed))
	for _, fact := range changed {
		paths = append(paths, fact.Path)
	}
	r.appendHistory(map[string]any{"at": nowISO(), "type": "tool", "action": before, "tool": obs.Tool, "step": obs.Step, "ok": truthy(obs.Result["ok"]), "facts": paths})
	transition, err := r.advance("tool:" + obs.Tool)
	if err != nil {
		return ObserveResult{}, err
	}
	return ObserveResult{ChangedFacts: changed, Transition: transition, BeforeAction: before, CurrentAction: r.CurrentAction, Status: r.Status}, nil
}

func (r *Runtime) Approve(gateID, actor string) (*Transition, error) {
	if actor == "" {
		actor = "human"
	}
	var gate *Gate
	for i := range r.Action().Gates {
		if r.Action().Gates[i].ID == gateID {
			gate = &r.Action().Gates[i]
			break
		}
	}
	if gate == nil {
		return nil, fmt.Errorf("unknown gate %s for action %s", gateID, r.CurrentAction)
	}
	if gate.Type != "human" {
		return nil, fmt.Errorf("gate %s is not a human gate", gateID)
	}
	value := any(true)
	if gate.HasEquals {
		value = gate.Equals
	}
	if err := setPath(r.Facts, gate.Fact, deepAny(value)); err != nil {
		return nil, err
	}
	r.FactSources[gate.Fact] = map[string]any{"at": nowISO(), "source": "human", "actor": actor, "action": r.CurrentAction, "gate": gate.ID}
	r.appendHistory(map[string]any{"at": nowISO(), "type": "approve", "action": r.CurrentAction, "gate": gate.ID, "actor": actor})
	return r.advance("approve:" + gate.ID)
}

func (r *Runtime) CanFinish() bool { return r.Action().Terminal && r.actionSatisfied(r.Action()) }

func (r *Runtime) WaitingHumanGates() []GateSummary {
	action := r.Action()
	if !r.baseCompletionSatisfied(action) {
		return nil
	}
	for _, gate := range action.Gates {
		if gate.Type != "human" && !r.gateSatisfied(gate) {
			return nil
		}
	}
	out := []GateSummary{}
	for _, gate := range action.Gates {
		if gate.Type == "human" && !r.gateSatisfied(gate) {
			out = append(out, gateSummary(gate))
		}
	}
	return out
}

func (r *Runtime) PendingGates() []GateSummary {
	out := []GateSummary{}
	for _, gate := range r.Action().Gates {
		if !r.gateSatisfied(gate) {
			out = append(out, gateSummary(gate))
		}
	}
	return out
}

func (r *Runtime) Summary() Summary {
	action := r.Action()
	return Summary{ID: r.Definition.ID, Title: r.Definition.Title, CurrentAction: r.CurrentAction, CurrentTitle: action.Title, Terminal: action.Terminal, Status: r.Status, AllowedTools: cloneStringPtr(action.AllowedTools), CanFinish: r.CanFinish(), PendingGates: r.PendingGates(), Facts: deepMap(r.Facts)}
}

func (r *Runtime) Snapshot() Snapshot {
	definition, _ := clone(r.Definition)
	return Snapshot{Version: 1, Definition: definition, CurrentAction: r.CurrentAction, Facts: deepMap(r.Facts), FactSources: deepMap(r.FactSources), History: cloneSlice(r.History), Status: r.Status}
}

func (r *Runtime) advance(reason string) (*Transition, error) {
	var transition *Transition
	for guard := 0; guard < maxTransitions; guard++ {
		action := r.Action()
		if len(r.WaitingHumanGates()) > 0 {
			r.Status = "waiting_gate"
			return transition, nil
		}
		if !r.actionSatisfied(action) {
			r.Status = "running"
			return transition, nil
		}
		if action.Terminal {
			r.Status = "ready_to_finish"
			return transition, nil
		}
		var route *Route
		for i := range action.Routes {
			matched, err := Evaluate(action.Routes[i].When, EvalContext{Facts: r.Facts})
			if err != nil {
				return nil, err
			}
			if matched {
				route = &action.Routes[i]
				break
			}
		}
		if route == nil {
			r.Status = "running"
			return transition, nil
		}
		target := r.Definition.Actions[route.To]
		if target.Requires != nil {
			matched, err := Evaluate(target.Requires, EvalContext{Facts: r.Facts})
			if err != nil {
				return nil, err
			}
			if !matched {
				r.Status = "blocked"
				return transition, nil
			}
		}
		from := r.CurrentAction
		r.CurrentAction = route.To
		transition = &Transition{From: from, To: route.To, Reason: reason}
		r.appendHistory(map[string]any{"at": nowISO(), "type": "transition", "from": from, "to": route.To, "reason": reason})
	}
	return nil, errors.New("workflow automatic transition limit exceeded; possible route cycle")
}

func (r *Runtime) actionSatisfied(action *Action) bool {
	if !r.baseCompletionSatisfied(action) {
		return false
	}
	for _, gate := range action.Gates {
		if !r.gateSatisfied(gate) {
			return false
		}
	}
	return true
}

func (r *Runtime) baseCompletionSatisfied(action *Action) bool {
	if action.CompleteWhen != nil {
		matched, _ := Evaluate(action.CompleteWhen, EvalContext{Facts: r.Facts})
		return matched
	}
	if action.Terminal {
		return true
	}
	for _, route := range action.Routes {
		matched, _ := Evaluate(route.When, EvalContext{Facts: r.Facts})
		if matched {
			return true
		}
	}
	return false
}

func (r *Runtime) gateSatisfied(gate Gate) bool {
	if gate.Type == "human" {
		actual, _ := getPath(r.Facts, gate.Fact)
		expected := any(true)
		if gate.HasEquals {
			expected = gate.Equals
		}
		return same(actual, expected)
	}
	matched, _ := Evaluate(gate.Condition, EvalContext{Facts: r.Facts})
	return matched
}

func (r *Runtime) assertRequirements() error {
	action := r.Action()
	if action.Requires == nil {
		return nil
	}
	matched, err := Evaluate(action.Requires, EvalContext{Facts: r.Facts})
	if err != nil {
		return err
	}
	if !matched {
		return fmt.Errorf("workflow action requirements are not satisfied for %s", r.CurrentAction)
	}
	return nil
}

func (r *Runtime) appendHistory(value map[string]any) {
	r.History = append(r.History, value)
	if len(r.History) > maxHistory {
		r.History = r.History[len(r.History)-maxHistory:]
	}
}
