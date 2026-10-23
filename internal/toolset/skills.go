package toolset

import (
	"context"

	"github.com/edynasty/LumenCortex/protocol"
)

// SkillReader loads the skills a run's catalog offers. The runtime builds it from the skills the
// run's system prompt lists, so read_skill reads exactly those and nothing a disabled, withheld or
// shadowed skill provides.
type SkillReader interface {
	ReadSkill(ctx context.Context, id, path string) (any, error)
}

func (s *Set) registerSkillTools() {
	if s.skills == nil {
		return
	}
	s.add(protocol.ToolSpec{
		Name: "read_skill",
		Description: "Load a skill from the Skills catalog in your instructions: with only an id it returns the skill's instructions (SKILL.md); " +
			"with a path relative to the skill directory it returns that file, or the entries of that directory. " +
			"Call it before acting on a task a skill's description matches.",
		Permission: "read",
		Parameters: obj(map[string]any{
			"id":   map[string]any{"type": "string", "description": "the skill id from the catalog"},
			"path": map[string]any{"type": "string", "description": "optional file or directory inside the skill, e.g. references/api.md"},
		}, "id"),
	}, s.readSkill)
}

func (s *Set) readSkill(ctx context.Context, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	result, err := s.skills.ReadSkill(ctx, stringArg(args, "id"), stringArg(args, "path"))
	if err != nil {
		return protocol.ToolResult{}, err
	}
	return jsonResult(result)
}
