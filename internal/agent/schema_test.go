package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/lsp"
	"github.com/edynasty/LumenCortex/internal/toolset"
	"github.com/edynasty/LumenCortex/protocol"
)

type schemaSubagents struct{}

func (schemaSubagents) Spawn(context.Context, string, string) (any, error)       { return nil, nil }
func (schemaSubagents) List(context.Context) (any, error)                        { return nil, nil }
func (schemaSubagents) Wait(context.Context, string, time.Duration) (any, error) { return nil, nil }

type schemaStarter struct{}

func (schemaStarter) StartTask(context.Context, string, string) (string, any, error) {
	return "", nil, nil
}

// Every tool a request carries has a parameter schema an endpoint that validates JSON Schema accepts:
// an object schema, no null anywhere, and "required" either absent or a non-empty list of declared
// properties. A schema with "required": null made a strict endpoint reject the whole request.
func TestRequestToolSchemasAreValidJSONSchema(t *testing.T) {
	workspace := t.TempDir()
	manager, err := lsp.NewManager(workspace)
	if err != nil {
		t.Fatal(err)
	}
	// Both delegation tools carry their agent enum, so its schema is checked too.
	spawnAgents := []config.AgentInfo{{ID: config.AgentExplore, Description: "Reads."}, {ID: "audit", Description: "Audits."}}
	taskAgents := []config.AgentInfo{{ID: config.AgentWorker, Description: "Edits."}}
	tools, err := toolset.New(toolset.Options{Workspace: workspace, Policy: toolset.PolicyFull, LSP: manager, Subagents: schemaSubagents{},
		SubagentAgents: spawnAgents})
	if err != nil {
		t.Fatal(err)
	}
	for _, delegation := range []string{DelegationPropose, DelegationIsolated} {
		t.Run(delegation, func(t *testing.T) {
			provider := &scriptedProvider{responses: []protocol.ProviderResponse{answer("ok")}}
			loop := &Loop{Models: testModels(provider), Store: newState("p"), Tools: tools, Tasks: schemaStarter{}}
			opts := Options{Session: testRef, MaxSteps: 2, Interactive: true, Delegation: delegation, TaskAgents: taskAgents}
			if _, err := loop.Run(context.Background(), "p", opts); err != nil {
				t.Fatal(err)
			}
			if len(provider.seen) == 0 {
				t.Fatal("no request was sent")
			}
			specs := provider.seen[0].Tools
			names := make([]string, 0, len(specs))
			for _, spec := range specs {
				names = append(names, spec.Name)
				// The wire request carries Parameters as they marshal (provider/openai wireToolFunction).
				raw, err := json.Marshal(spec.Parameters)
				if err != nil {
					t.Fatalf("%s: %v", spec.Name, err)
				}
				var schema any
				if err := json.Unmarshal(raw, &schema); err != nil {
					t.Fatalf("%s: %v", spec.Name, err)
				}
				if err := checkObjectSchema(schema, "$"); err != nil {
					t.Errorf("%s: %v in %s", spec.Name, err, raw)
				}
			}
			for _, want := range []string{"git_status", "git_diff", "list_subagents", "spawn_subagent", "wait_subagent", "shell",
				"apply_patch", "lsp_hover", "lsp_rename", "work_unit_update", "start_task"} {
				if !slices.Contains(names, want) {
					t.Errorf("request tools lack %s: %v", want, names)
				}
			}
		})
	}
}

func checkObjectSchema(node any, path string) error {
	schema, ok := node.(map[string]any)
	if !ok || schema["type"] != "object" {
		return fmt.Errorf("%s is not an object schema", path)
	}
	return checkSchema(schema, path)
}

func checkSchema(node any, path string) error {
	schema, ok := node.(map[string]any)
	if !ok {
		return fmt.Errorf("%s is not a schema object", path)
	}
	for key, value := range schema {
		if value == nil {
			return fmt.Errorf("%s.%s is null", path, key)
		}
	}
	properties, _ := schema["properties"].(map[string]any)
	if _, present := schema["properties"]; present && properties == nil {
		return fmt.Errorf("%s.properties is not an object", path)
	}
	if required, present := schema["required"]; present {
		list, ok := required.([]any)
		if !ok || len(list) == 0 {
			return fmt.Errorf("%s.required is not a non-empty array", path)
		}
		seen := map[string]bool{}
		for _, item := range list {
			name, ok := item.(string)
			if !ok || seen[name] || properties[name] == nil {
				return fmt.Errorf("%s.required lists %v, not a unique declared property", path, item)
			}
			seen[name] = true
		}
	}
	for name, property := range properties {
		if err := checkSchema(property, path+".properties."+name); err != nil {
			return err
		}
	}
	if items, present := schema["items"]; present {
		if err := checkSchema(items, path+".items"); err != nil {
			return err
		}
	}
	return nil
}
