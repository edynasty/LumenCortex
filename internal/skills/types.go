package skills

const (
	ScopeGlobal    = "global"
	ScopeProject   = "project"
	ScopeEffective = "effective"

	MaxSkills      = 128
	MaxSkillBytes  = 128 << 10
	MaxPromptBytes = 256 << 10
	MaxStateBytes  = 64 << 10
)

type Skill struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Scope       string `json:"scope"`
	Path        string `json:"path"`
	Enabled     bool   `json:"enabled"`
	Overridden  bool   `json:"overridden,omitempty"`
	Bytes       int64  `json:"bytes,omitempty"`
	Error       string `json:"error,omitempty"`
}

type stateEntry struct {
	Enabled bool `json:"enabled"`
}

// PromptOptions selects which scopes a skills prompt reads.
type PromptOptions struct {
	// IncludeProject includes the project scope (<workspace>/.lumencortex/skills). The runtime sets
	// it from the trust of the Engine workspace, read per run: a project SKILL.md is repository
	// content, and its instructions reach the model only in a workspace the user trusts (ADR-0013).
	IncludeProject bool
}

// PromptResult is a skills prompt and what it carries.
type PromptResult struct {
	// Text is the prompt, "" when no skill is included.
	Text string
	// Included are the skills whose text the prompt carries.
	Included []Skill
	// Withheld are the project skills left out because the workspace is untrusted.
	Withheld []Skill
}

type stateFile struct {
	Skills map[string]stateEntry `json:"skills"`
}

type rawSkill struct {
	Skill
	content string
}
