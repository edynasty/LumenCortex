package toolset

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/edynasty/LumenCortex/internal/shell"
	"github.com/edynasty/LumenCortex/protocol"
)

const (
	PolicyReadOnly  = "read-only"
	PolicyWorkspace = "workspace"
	PolicyFull      = "full"
	maxReadBytes    = 128 << 10
	maxEditBytes    = 4 << 20
	maxListEntries  = 500
)

type Options struct {
	Workspace string
	Policy    string
	Shell     *shell.Runner
}
type Set struct {
	workspace     string
	realWorkspace string
	policy        string
	shell         *shell.Runner
	tools         map[string]tool
}
type tool struct {
	spec    protocol.ToolSpec
	execute func(context.Context, map[string]any, func(protocol.ToolOutput)) (protocol.ToolResult, error)
}

type patch struct {
	Path      string `json:"path"`
	Operation string `json:"operation"`
	Edits     []edit `json:"edits"`
	Content   string `json:"content"`
}
type edit struct {
	OldText string `json:"old_text"`
	NewText string `json:"new_text"`
}
type plan struct{ operation, file, relative, original, next string }

func New(opts Options) (*Set, error) {
	if opts.Workspace == "" {
		return nil, errors.New("workspace is required")
	}
	root, err := filepath.Abs(opts.Workspace)
	if err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	policy := opts.Policy
	if policy == "" {
		policy = PolicyReadOnly
	}
	if policy != PolicyReadOnly && policy != PolicyWorkspace && policy != PolicyFull {
		return nil, fmt.Errorf("unknown policy: %s", policy)
	}
	runner := opts.Shell
	if runner == nil {
		runner = shell.New(root)
	}
	s := &Set{workspace: root, realWorkspace: real, policy: policy, shell: runner, tools: map[string]tool{}}
	s.registerBuiltins()
	return s, nil
}

func (s *Set) Specs(allow []string) []protocol.ToolSpec {
	allowed := map[string]bool{}
	if allow != nil {
		for _, n := range allow {
			allowed[n] = true
		}
	}
	names := make([]string, 0, len(s.tools))
	for name, t := range s.tools {
		if allow != nil && !allowed[name] {
			continue
		}
		if !s.policyAllows(t.spec) {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]protocol.ToolSpec, 0, len(names))
	for _, n := range names {
		out = append(out, s.tools[n].spec)
	}
	return out
}
func (s *Set) Execute(ctx context.Context, name string, args map[string]any, onOutput func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	t, ok := s.tools[name]
	if !ok {
		return protocol.ToolResult{}, fmt.Errorf("unknown tool: %s", name)
	}
	if !s.policyAllows(t.spec) {
		return protocol.ToolResult{OK: false, Denied: true, Permission: t.spec.Permission, Content: `{"error":"tool denied by policy"}`}, nil
	}
	return t.execute(ctx, args, onOutput)
}
func (s *Set) policyAllows(spec protocol.ToolSpec) bool {
	if s.policy == PolicyFull {
		return true
	}
	if s.policy == PolicyReadOnly {
		return spec.Permission == "read"
	}
	return spec.Permission == "read" || spec.Permission == "write"
}
func (s *Set) add(spec protocol.ToolSpec, fn func(context.Context, map[string]any, func(protocol.ToolOutput)) (protocol.ToolResult, error)) {
	s.tools[spec.Name] = tool{spec: spec, execute: fn}
}

func (s *Set) registerBuiltins() {
	s.add(protocol.ToolSpec{Name: "read_file", Description: "Read a bounded UTF-8 file inside the workspace.", Permission: "read", Parameters: obj(map[string]any{"path": str()}, "path")}, s.readFile)
	s.add(protocol.ToolSpec{Name: "list_dir", Description: "List a bounded number of entries in a workspace directory.", Permission: "read", Parameters: obj(map[string]any{"path": str()}, "path")}, s.listDir)
	s.add(protocol.ToolSpec{Name: "write_file", Description: "Create or replace a UTF-8 file inside the workspace.", Permission: "write", MutatesWorkspace: true, Parameters: obj(map[string]any{"path": str(), "content": str()}, "path", "content")}, s.writeFile)
	s.add(protocol.ToolSpec{Name: "replace_in_file", Description: "Replace one exact text occurrence in a UTF-8 workspace file.", Permission: "write", MutatesWorkspace: true, Parameters: obj(map[string]any{"path": str(), "old_text": str(), "new_text": str()}, "path", "old_text", "new_text")}, s.replaceFile)
	s.add(protocol.ToolSpec{Name: "apply_patch", Description: "Apply a validated atomic batch of exact text edits, creates or deletes inside the workspace.", Permission: "write", MutatesWorkspace: true, Parameters: map[string]any{
		"type": "object",
		"properties": map[string]any{"patches": map[string]any{
			"type": "array", "minItems": 1,
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":      str(),
					"operation": map[string]any{"type": "string", "enum": []string{"update", "create", "delete"}},
					"edits":     map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{"old_text": str(), "new_text": str()}, "required": []string{"old_text", "new_text"}, "additionalProperties": false}},
					"content":   str(),
				},
				"required": []string{"path", "operation"}, "additionalProperties": false,
			},
		}},
		"required": []string{"patches"}, "additionalProperties": false,
	}}, s.applyPatch)
	s.add(protocol.ToolSpec{Name: "shell", Description: "Run a shell command in the workspace with bounded retained output.", Permission: "exec", MutatesWorkspace: true, Parameters: obj(map[string]any{"command": str()}, "command")}, s.runShell)
}
func str() map[string]any { return map[string]any{"type": "string"} }
func obj(props map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
}
