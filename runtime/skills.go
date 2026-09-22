package runtime

import (
	"errors"

	"github.com/edynasty/LumenCortex/internal/skills"
)

type Skill = skills.Skill

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
