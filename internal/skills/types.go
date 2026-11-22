package skills

const (
	ScopeGlobal    = "global"
	ScopeProject   = "project"
	ScopeEffective = "effective"

	// MaxSkills bounds the skills read from one root.
	MaxSkills     = 128
	MaxSkillBytes = 128 << 10
	MaxStateBytes = 64 << 10
	// MaxDescriptionBytes is the Agent Skills limit on a description; the catalog carries it whole.
	MaxDescriptionBytes = 1024
	// CompactTriggerBytes bounds one skill's trigger snippet in a ModeCompact system catalog.
	CompactTriggerBytes = 160
	// MaxCompactCatalogBytes bounds the ModeCompact skills layer body (8 KiB).
	MaxCompactCatalogBytes = 8 << 10
	// MaxResourceBytes bounds one file read_skill returns; MaxResourceEntries one directory listing.
	MaxResourceBytes   = 128 << 10
	MaxResourceEntries = 200
)

// The sources a skill root can have. LumenCortex's own directories are writable; the others are
// the cross-agent conventions other tools install into, which LumenCortex reads in place and never
// writes (a symlinked skill stays a link to its owner's files).
const (
	SourceLumenCortex = "lumencortex"
	SourceAgents      = "agents"
	SourceClaude      = "claude"
)

// Root is one directory of skills (<root>/<id>/SKILL.md). Within a scope the roots are listed in
// precedence order: the first root that has an id provides it, the others are shadowed.
type Root struct {
	Source   string
	Path     string
	Writable bool
}

type Skill struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Scope       string `json:"scope"`
	// Source names the root the skill came from (SourceLumenCortex, SourceAgents, SourceClaude) and
	// Root is that directory.
	Source string `json:"source"`
	Root   string `json:"root"`
	Path   string `json:"path"`
	// Linked reports a skill directory that is a symbolic link; RealPath is where it resolves when
	// that differs from the path.
	Linked   bool   `json:"linked,omitempty"`
	RealPath string `json:"realPath,omitempty"`
	// ReadOnly reports a skill LumenCortex does not write: it lives in another tool's root.
	ReadOnly bool `json:"readOnly,omitempty"`
	Enabled  bool `json:"enabled"`
	// Overridden reports a project skill that replaces a global one with the same id.
	Overridden bool `json:"overridden,omitempty"`
	// ShadowedBy names the skill that wins over this one ("<scope>:<source>"): a root earlier in the
	// same scope with the same id, or the project scope over the global one. A shadowed skill is
	// listed so the conflict is visible; it never reaches a run.
	ShadowedBy string `json:"shadowedBy,omitempty"`
	Bytes      int64  `json:"bytes,omitempty"`
	Error      string `json:"error,omitempty"`
}

// SnapshottedSkill pairs an eligible skill's metadata with the SKILL.md UTF-8 body captured during
// Registry.Prompt so a run's Prompt, ListSkills, and ReadSkill share one immutable snapshot (RC33-F02).
type SnapshottedSkill struct {
	Skill
	Content string
}

type stateEntry struct {
	Enabled bool `json:"enabled"`
}

// PromptOptions selects which scopes a skills prompt reads.
type PromptOptions struct {
	// IncludeProject includes the project scope. The runtime sets it from the trust of the Engine
	// workspace, read per run: a project SKILL.md is repository content, and its instructions reach
	// the model only in a workspace the user trusts (ADR-0013).
	IncludeProject bool
}

// PromptResult is a skills prompt and the immutable run-level skill snapshot it captured.
type PromptResult struct {
	// Text is the ModeFull catalog (at most MaxCatalogBytes), "" when no skill is eligible.
	Text string
	// CompactText is the ModeCompact catalog (at most MaxCompactCatalogBytes), "" when no skill is eligible.
	CompactText string
	// Included are the skills listed inline in Text.
	Included []Skill
	// Eligible are all enabled, error-free, non-shadowed skills in the run's trusted scope, sorted by ID
	// ascending, with their snapshotted SKILL.md content.
	Eligible []SnapshottedSkill
	// Withheld are the project skills left out because the workspace is untrusted.
	Withheld []Skill
	// Warnings reports non-fatal catalog conditions such as a root exceeding MaxSkills (128).
	Warnings []string
}

// Resource is one read through read_skill: a file's text or a directory's entries.
type Resource struct {
	ID      string   `json:"id"`
	Path    string   `json:"path"`
	Content string   `json:"content,omitempty"`
	Entries []string `json:"entries,omitempty"`
	// Truncated reports a file or listing cut at its bound.
	Truncated bool `json:"truncated,omitempty"`
}

type stateFile struct {
	Skills map[string]stateEntry `json:"skills"`
}

type rawSkill struct {
	Skill
	content string
	// dir is the skill directory as found (a link when Linked).
	dir string
}
