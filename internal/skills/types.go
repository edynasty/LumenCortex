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

type stateFile struct {
	Skills map[string]stateEntry `json:"skills"`
}

type rawSkill struct {
	Skill
	content string
}
