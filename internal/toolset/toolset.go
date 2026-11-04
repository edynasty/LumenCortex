package toolset

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/lsp"
	"github.com/edynasty/LumenCortex/internal/mcp"
	"github.com/edynasty/LumenCortex/internal/repository"
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
	// LSP is the workspace's language servers as this run reaches them (lsp.Manager.ForRun): whether a
	// call may start a server is decided there, by the workspace's trust (ADR-0013).
	LSP       *lsp.Access
	MCP       *mcp.Registry
	Subagents SubagentController
	// SubagentAgents are the catalog agents spawn_subagent may run (ADR-0009 A3): delegation use,
	// read-only ceiling. They form the tool's agent enum; empty offers no agent parameter (explore runs).
	SubagentAgents []config.AgentInfo
	// Threads backs read_thread and recall (ADR-0010 D3); nil offers neither.
	Threads ThreadReader
	// Skills backs read_skill; nil (a run whose catalog offers no skill) does not offer it.
	Skills SkillReader
	// Notebook backs notebook_list/read/update (ADR-0020); nil offers none of them.
	Notebook NotebookStore
	// Protected is the state the file tools refuse to reach (ADR-0016). The zero value protects nothing.
	Protected Protected
	// ShellTimeout is the default timeout of a foreground shell call (execution.shellTimeoutSeconds);
	// 0 selects the default, 120 s. A call's timeout_sec replaces it; ShellMaxTimeout bounds both.
	ShellTimeout time.Duration
	// ShellMaxTimeout bounds every foreground shell call (execution.shellMaxTimeoutSeconds); 0 selects
	// the default, 1800 s, and a lower value also lowers ShellTimeout. A call's timeout_sec above it is
	// clamped to it; the run's tool deadline still bounds every call.
	ShellMaxTimeout time.Duration
	// Jobs is the background job queue of the run's session (ADR-0019 §3); nil offers no job tools and
	// no background mode.
	Jobs JobController
	// Wakeups backs wakeup_schedule and wakeup_cancel (ADR-0019 §4); nil offers neither.
	Wakeups WakeupController
	// Logger receives the tools' own log lines; nil discards them. LSP lifecycle lines come from the
	// manager, which has its own logger.
	Logger *slog.Logger
	// SessionID is the session the run belongs to. A server configured with a session binding receives
	// it instead of a model-supplied id (a computer session is bound to the run that opened it).
	SessionID string
}
type Set struct {
	workspace      string
	realWorkspace  string
	policy         string
	shell          *shell.Runner
	repository     *repository.Service
	lsp            *lsp.Access
	mcp            *mcp.Registry
	subagents      SubagentController
	subagentAgents []config.AgentInfo
	threads        ThreadReader
	skills         SkillReader
	notebooks      NotebookStore
	protected      Protected
	// shellTimeoutDefault is Options.ShellTimeout with its default applied.
	shellTimeoutDefault time.Duration
	// shellMaxTimeout is Options.ShellMaxTimeout with its default applied.
	shellMaxTimeout time.Duration
	jobs            JobController
	wakeups         WakeupController
	log             *slog.Logger
	// sessionID is Options.SessionID.
	sessionID string
	tools     map[string]tool
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
	// The bash tool runs in bash (ADR-0019 §0); a runner the host shares with checks and RunShell keeps
	// its own interpreter for those, so the tool gets a copy. An injected interpreter is kept.
	bash := *runner
	if bash.Interpreter.IsZero() {
		bash.Interpreter = shell.Bash()
	}
	runner = &bash
	repoService, err := repository.New(root)
	if err != nil {
		return nil, err
	}
	shellTimeout := opts.ShellTimeout
	if shellTimeout <= 0 {
		shellTimeout = config.DefaultShellTimeoutSeconds * time.Second
	}
	shellMaxTimeout := opts.ShellMaxTimeout
	if shellMaxTimeout <= 0 {
		shellMaxTimeout = config.DefaultShellMaxTimeoutSeconds * time.Second
	}
	if shellTimeout > shellMaxTimeout {
		shellTimeout = shellMaxTimeout
	}
	s := &Set{workspace: root, realWorkspace: real, policy: policy, shell: runner, repository: repoService, lsp: opts.LSP, mcp: opts.MCP, subagents: opts.Subagents, subagentAgents: opts.SubagentAgents, threads: opts.Threads, skills: opts.Skills, notebooks: opts.Notebook, protected: opts.Protected, shellTimeoutDefault: shellTimeout, shellMaxTimeout: shellMaxTimeout, jobs: opts.Jobs, wakeups: opts.Wakeups, log: newToolsetLogger(opts.Logger), sessionID: opts.SessionID, tools: map[string]tool{}}
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

// CatalogTool is one registered tool with its origin: "builtin", "lsp", or "mcp:<serverID>".
type CatalogTool struct {
	Spec   protocol.ToolSpec
	Source string
}

// Catalog returns every registered tool with its origin, sorted by name, before the policy and
// allowlist filters: the whole universe a step's tool list is drawn from. A prompt report uses it to
// name the tools a policy or an allowlist withholds, so the report explains the run rather than only
// listing what survived the filters.
func (s *Set) Catalog() []CatalogTool {
	servers := map[string]string{}
	if s.mcp != nil {
		if bindings, err := s.mcp.AgentTools(s.workspace); err == nil {
			for _, binding := range bindings {
				servers[binding.Name] = binding.ServerID
			}
		}
	}
	names := make([]string, 0, len(s.tools))
	for name := range s.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]CatalogTool, 0, len(names))
	for _, name := range names {
		source := "builtin"
		switch server, ok := servers[name]; {
		case ok:
			source = "mcp:" + server
		case strings.HasPrefix(name, "mcp__"):
			// Registered by an MCP server that has since stopped: its server is no longer named by
			// the registry, but the tool is still of that kind.
			source = "mcp"
		case strings.HasPrefix(name, "lsp_"):
			source = "lsp"
		}
		out = append(out, CatalogTool{Spec: s.tools[name].spec, Source: source})
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
	// A state tool writes only the run's own durable working memory (its notebook), never a workspace
	// file or a command, so every policy admits it - the read-only one included (ADR-0020).
	if spec.Permission == PermissionState {
		return true
	}
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

// builtinSpecs is the static tool list every run has, before the policy and allowlist filters. It is
// the one definition of those specs: registerBuiltins binds each to its handler, and the capability
// registry reads them from a toolset built as a run builds it. bash's description names the interpreter
// it really runs (ADR-0019 §0) and offers mode "background" only where a job queue exists (ADR-0019 §3).
// git_status and git_diff were removed (ADR-0019 §0): the agent runs git through bash; the host's Git
// API (repository.Service) is unchanged.
func builtinSpecs(interp shell.Interpreter, background bool) []protocol.ToolSpec {
	return []protocol.ToolSpec{
		{Name: "search_text", Description: "Search repository text with bounded results. Uses ripgrep when available.", Permission: "read", Parameters: obj(map[string]any{"query": str(), "path": str(), "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 1000}}, "query")},
		{Name: "find_files", Description: "Find repository files by bounded glob/name pattern.", Permission: "read", Parameters: obj(map[string]any{"pattern": str(), "path": str(), "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 1000}}, "pattern")},
		{Name: "read_file", Description: "Read a bounded UTF-8 file inside the workspace.", Permission: "read", Parameters: obj(map[string]any{"path": str()}, "path")},
		{Name: "list_dir", Description: "List a bounded number of entries in a workspace directory.", Permission: "read", Parameters: obj(map[string]any{"path": str()}, "path")},
		{Name: "write_file", Description: "Create or replace a UTF-8 file inside the workspace.", Permission: "write", MutatesWorkspace: true, Parameters: obj(map[string]any{"path": str(), "content": str()}, "path", "content")},
		{Name: "replace_in_file", Description: "Replace one exact text occurrence in a UTF-8 workspace file.", Permission: "write", MutatesWorkspace: true, Parameters: obj(map[string]any{"path": str(), "old_text": str(), "new_text": str()}, "path", "old_text", "new_text")},
		{Name: "apply_patch", Description: "Apply a validated atomic batch of exact text edits, creates or deletes inside the workspace.", Permission: "write", MutatesWorkspace: true, Parameters: map[string]any{
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
		}},
		bashSpec(interp, background),
	}
}

// bashSpec is the command tool's spec for a run whose commands run in interp, with the background mode
// when the run has a job queue.
func bashSpec(interp shell.Interpreter, background bool) protocol.ToolSpec {
	bashParams := map[string]any{
		"command":     str(),
		"timeout_sec": map[string]any{"type": "integer", "minimum": 1, "maximum": maxShellTimeoutSeconds, "description": "Seconds the command may run before its process group is terminated (SIGTERM, then SIGKILL). In the foreground it replaces the configured default and the run's deadline still bounds it."},
	}
	description := bashDescription(interp)
	if background {
		// The job queue of this run is available: the same tool can start a command in the background and
		// return at once with its job id, which job_status, job_wait, job_cancel and job_list act on.
		bashParams["mode"] = map[string]any{"type": "string", "enum": []string{"foreground", "background"}}
		description += " mode \"background\" starts it as a durable background job and returns its job id at once: use it for " +
			"long builds and test runs, then job_wait, job_status or job_cancel it. A run cannot deliver its " +
			"answer while its background jobs are active. timeout_sec bounds a background job's run time; its " +
			"output is kept head and tail only."
	}
	return protocol.ToolSpec{Name: protocol.ToolBash, Description: description, Permission: "exec", MutatesWorkspace: true, Parameters: obj(bashParams, "command")}
}

// registerBuiltins binds the static specs to their handlers.
func (s *Set) registerBuiltins() {
	handlers := map[string]func(context.Context, map[string]any, func(protocol.ToolOutput)) (protocol.ToolResult, error){
		"search_text":     s.searchText,
		"find_files":      s.findFiles,
		"read_file":       s.readFile,
		"list_dir":        s.listDir,
		"write_file":      s.writeFile,
		"replace_in_file": s.replaceFile,
		"apply_patch":     s.applyPatch,
		protocol.ToolBash: s.runShell,
	}
	for _, spec := range builtinSpecs(s.shell.Interpreter, s.jobs != nil) {
		s.add(spec, handlers[spec.Name])
	}
	s.registerLSPTools()
	if s.mcp != nil {
		s.registerMCPTools()
	}
	if s.subagents != nil {
		s.registerSubagentTools()
	}
	if s.jobs != nil {
		s.registerJobTools()
	}
	s.registerThreadTools()
	s.registerSkillTools()
	s.registerWakeupTools()
	s.registerNotebookTools()
}

func str() map[string]any { return map[string]any{"type": "string"} }

// obj builds an object schema. JSON Schema requires "required" to be an array of strings when present,
// so a schema without required properties omits the member rather than emitting null.
func obj(props map[string]any, required ...string) map[string]any {
	schema := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}
