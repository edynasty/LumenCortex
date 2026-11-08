package runtime

import (
	"context"
	"encoding/json"
	"fmt"
)

type DebugThread struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}
type DebugFrame struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Line   int    `json:"line"`
	Source struct {
		Path string `json:"path"`
		Name string `json:"name"`
	} `json:"source"`
}
type DebugScope struct {
	Name               string `json:"name"`
	VariablesReference int    `json:"variablesReference"`
}
type DebugVariable struct {
	Name               string `json:"name"`
	Value              string `json:"value"`
	Type               string `json:"type"`
	VariablesReference int    `json:"variablesReference"`
}
type DebugInspection struct {
	Threads     []DebugThread   `json:"threads,omitempty"`
	StackFrames []DebugFrame    `json:"stackFrames,omitempty"`
	Scopes      []DebugScope    `json:"scopes,omitempty"`
	Variables   []DebugVariable `json:"variables,omitempty"`
	Truncated   bool            `json:"truncated"`
}

func (e *Engine) DebugView(ctx context.Context, c DebugControl, kind string, reference, start, count int) (DebugInspection, error) {
	raw, err := e.DebugInspect(ctx, c, kind, reference, start, count)
	if err != nil {
		return DebugInspection{}, err
	}
	var view DebugInspection
	if err = json.Unmarshal(raw, &view); err != nil {
		return view, fmt.Errorf("invalid adapter inspection: %w", err)
	}
	if len(view.Threads) > 100 {
		view.Threads = view.Threads[:100]
		view.Truncated = true
	}
	if len(view.StackFrames) > 20 {
		view.StackFrames = view.StackFrames[:20]
		view.Truncated = true
	}
	if len(view.Scopes) > 20 {
		view.Scopes = view.Scopes[:20]
		view.Truncated = true
	}
	if len(view.Variables) > 100 {
		view.Variables = view.Variables[:100]
		view.Truncated = true
	}
	trim := func(s string) string {
		if len(s) > 8192 {
			view.Truncated = true
			return s[:8192]
		}
		return s
	}
	for i := range view.Threads {
		view.Threads[i].Name = trim(view.Threads[i].Name)
	}
	for i := range view.StackFrames {
		f := &view.StackFrames[i]
		f.Name = trim(f.Name)
		f.Source.Path = trim(f.Source.Path)
		f.Source.Name = trim(f.Source.Name)
	}
	for i := range view.Scopes {
		view.Scopes[i].Name = trim(view.Scopes[i].Name)
	}
	for i := range view.Variables {
		v := &view.Variables[i]
		v.Name = trim(v.Name)
		v.Value = trim(v.Value)
		v.Type = trim(v.Type)
	}
	return view, nil
}

func (e *Engine) DebugGrant(ctx context.Context, id string) (DebugGrant, error) {
	if err := e.runtimeEventAccess(ctx, id); err != nil {
		return DebugGrant{}, err
	}
	e.dapMu.Lock()
	defer e.dapMu.Unlock()
	return e.dapGrants[id], nil
}
