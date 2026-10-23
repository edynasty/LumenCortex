package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/edynasty/LumenCortex/internal/skills"
	"github.com/edynasty/LumenCortex/internal/toolset"
)

type Skill = skills.Skill

// skillReader serves read_skill for one run: the skills its catalog lists, by id. A skill the
// catalog does not list (disabled, shadowed, withheld in an untrusted workspace, or unknown) is not
// readable, so the tool never widens what the prompt layer decided.
type skillReader map[string]skills.Skill

// newSkillReader is nil when the catalog lists nothing: such a run is not offered read_skill.
func newSkillReader(included []skills.Skill) toolset.SkillReader {
	if len(included) == 0 {
		return nil
	}
	reader := make(skillReader, len(included))
	for _, s := range included {
		reader[s.ID] = s
	}
	return reader
}

func (r skillReader) ReadSkill(_ context.Context, id, path string) (any, error) {
	s, ok := r[strings.TrimSpace(id)]
	if !ok {
		return nil, fmt.Errorf("unknown skill %q: read_skill reads the skills the catalog lists", id)
	}
	return skills.ReadResource(s, path)
}

func (e *Engine) Skills(scope string) ([]Skill, error) {
	if e.skillRegistry == nil {
		return []Skill{}, nil
	}
	return e.skillRegistry.List(scope)
}

func (e *Engine) SkillContent(scope, id string) (string, error) {
	if e.skillRegistry == nil {
		return "", errors.New("skill registry is unavailable")
	}
	return e.skillRegistry.Content(scope, id)
}

func (e *Engine) SaveSkill(scope, id, content string) error {
	if e.skillRegistry == nil {
		return errors.New("skill registry is unavailable")
	}
	return e.skillRegistry.Save(scope, id, content)
}

func (e *Engine) DeleteSkill(scope, id string) error {
	if e.skillRegistry == nil {
		return errors.New("skill registry is unavailable")
	}
	return e.skillRegistry.Delete(scope, id)
}

func (e *Engine) SetSkillEnabled(scope, id string, enabled bool) error {
	if e.skillRegistry == nil {
		return errors.New("skill registry is unavailable")
	}
	return e.skillRegistry.SetEnabled(scope, id, enabled)
}
