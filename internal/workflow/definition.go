package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

func LoadFile(path string) (*Definition, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(raw)
}

func Parse(raw []byte) (*Definition, error) {
	var d Definition
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, err
	}
	return Validate(&d)
}

func Validate(input *Definition) (*Definition, error) {
	if input == nil {
		return nil, errors.New("workflow definition must be an object")
	}
	d, err := clone(input)
	if err != nil {
		return nil, err
	}
	if d.Version == 0 {
		d.Version = 1
	}
	if d.Version != 1 {
		return nil, fmt.Errorf("unsupported workflow version: %d", d.Version)
	}
	if strings.TrimSpace(d.ID) == "" {
		return nil, errors.New("workflow id is required")
	}
	if len(d.Actions) == 0 {
		return nil, errors.New("workflow must define at least one action")
	}
	ids := sortedKeys(d.Actions)
	if d.Entry == "" {
		d.Entry = ids[0]
	}
	if d.Actions[d.Entry] == nil {
		return nil, fmt.Errorf("unknown workflow entry action: %s", d.Entry)
	}
	if d.Title == "" {
		d.Title = d.ID
	}
	if d.Facts == nil {
		d.Facts = map[string]any{}
	}

	for _, id := range ids {
		a := d.Actions[id]
		if a == nil {
			return nil, fmt.Errorf("workflow action %s must be an object", id)
		}
		a.ID = id
		if a.Title == "" {
			a.Title = id
		}
		if err := normalizeAllowedTools(a, id); err != nil {
			return nil, err
		}
		for i := range a.Outcomes {
			o := &a.Outcomes[i]
			if o.ID == "" {
				o.ID = fmt.Sprintf("%s.outcome.%d", id, i+1)
			}
			if o.When == nil {
				o.When = true
			}
			if err := validateCondition(o.When, fmt.Sprintf("outcome %s[%d].when", id, i)); err != nil {
				return nil, err
			}
			if o.Set == nil {
				return nil, fmt.Errorf("outcome %s[%d] requires a set object", id, i)
			}
			for path := range o.Set {
				if _, err := validatePath(path); err != nil {
					return nil, fmt.Errorf("outcome %s[%d].set: %w", id, i, err)
				}
			}
		}
		for i := range a.Routes {
			route := &a.Routes[i]
			if strings.TrimSpace(route.To) == "" {
				return nil, fmt.Errorf("route %s[%d] requires a target action", id, i)
			}
			if route.When == nil {
				route.When = true
			}
			if err := validateCondition(route.When, fmt.Sprintf("route %s[%d].when", id, i)); err != nil {
				return nil, err
			}
		}
		for i := range a.Gates {
			if err := normalizeGate(&a.Gates[i], id, i); err != nil {
				return nil, err
			}
		}
		if a.Requires != nil {
			if err := validateCondition(a.Requires, "action "+id+".requires"); err != nil {
				return nil, err
			}
		}
		if a.CompleteWhen != nil {
			if err := validateCondition(a.CompleteWhen, "action "+id+".completeWhen"); err != nil {
				return nil, err
			}
		}
	}

	for id, a := range d.Actions {
		for _, route := range a.Routes {
			if d.Actions[route.To] == nil {
				return nil, fmt.Errorf("action %s routes to unknown action: %s", id, route.To)
			}
		}
	}
	return d, nil
}

func normalizeAllowedTools(a *Action, actionID string) error {
	if a.AllowedTools == nil {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(*a.AllowedTools))
	for _, name := range *a.AllowedTools {
		name = strings.TrimSpace(name)
		if name == "" {
			return fmt.Errorf("allowedTools for %s must be an array of tool names", actionID)
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	a.AllowedTools = &out
	return nil
}

func normalizeGate(g *Gate, actionID string, index int) error {
	if g.ID == "" {
		g.ID = fmt.Sprintf("%s.gate.%d", actionID, index+1)
	}
	if g.Title == "" {
		g.Title = g.ID
	}
	if g.Type == "" {
		g.Type = "condition"
	}
	if g.Type != "condition" && g.Type != "human" {
		return fmt.Errorf("unsupported gate type %s for %s", g.Type, g.ID)
	}
	if g.Type == "human" {
		if g.Fact == "" {
			g.Fact = "gates." + g.ID
		}
		_, err := validatePath(g.Fact)
		return err
	}
	if g.Condition == nil {
		return fmt.Errorf("condition gate %s requires condition", g.ID)
	}
	return validateCondition(g.Condition, "gate "+g.ID+".condition")
}
