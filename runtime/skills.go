package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/edynasty/LumenCortex/internal/skills"
	"github.com/edynasty/LumenCortex/internal/toolset"
)

type Skill = skills.Skill

// skillReader serves read_skill (both ReadSkill and ListSkills) from an immutable per-run snapshot
// of eligible skills (RC33-F02). It holds no *skills.Registry reference: mid-run disk or state edits
// cannot leak new skills into an active run or tear SKILL.md reads.
type skillReader struct {
	ordered []skills.SnapshottedSkill
	byID    map[string]int
}

// newSkillReader returns nil when no skill is eligible: such a run is not offered read_skill.
func newSkillReader(eligible []skills.SnapshottedSkill) toolset.SkillReader {
	if len(eligible) == 0 {
		return nil
	}
	ordered := make([]skills.SnapshottedSkill, len(eligible))
	copy(ordered, eligible)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	byID := make(map[string]int, len(ordered))
	for i, s := range ordered {
		byID[s.ID] = i
	}
	return &skillReader{ordered: ordered, byID: byID}
}

func (r *skillReader) ReadSkill(_ context.Context, id, path string) (any, error) {
	idx, ok := r.byID[strings.TrimSpace(id)]
	if !ok {
		return nil, fmt.Errorf("unknown skill %q: read_skill reads the skills the catalog lists", id)
	}
	entry := r.ordered[idx]
	rel := strings.TrimSpace(path)
	if rel == "" || filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel))) == "SKILL.md" {
		out := skills.Resource{ID: entry.ID, Path: "SKILL.md"}
		raw := []byte(entry.Content)
		if len(raw) > skills.MaxResourceBytes {
			raw, out.Truncated = raw[:skills.MaxResourceBytes], true
			for len(raw) > 0 && !utf8.Valid(raw) {
				raw = raw[:len(raw)-1]
			}
		}
		out.Content = string(raw)
		return out, nil
	}
	return skills.ReadResource(entry.Skill, path)
}

func (r *skillReader) ListSkills(_ context.Context, query string, offset int) (toolset.SkillCatalogPage, error) {
	if offset < 0 {
		return toolset.SkillCatalogPage{}, errors.New("offset must not be negative")
	}
	q := strings.ToLower(strings.TrimSpace(query))
	matched := make([]skills.SnapshottedSkill, 0, len(r.ordered))
	for _, s := range r.ordered {
		if q == "" ||
			strings.Contains(strings.ToLower(s.ID), q) ||
			strings.Contains(strings.ToLower(s.Name), q) ||
			strings.Contains(strings.ToLower(s.Description), q) {
			matched = append(matched, s)
		}
	}
	page := toolset.SkillCatalogPage{
		TotalEligible: len(r.ordered),
		Matched:       len(matched),
		Offset:        offset,
		Skills:        []toolset.SkillCatalogEntry{},
	}
	if offset >= len(matched) {
		return page, nil
	}
	for k := 0; offset+k < len(matched) && k < toolset.MaxSkillCatalogPage; k++ {
		s := matched[offset+k]
		name := s.Name
		if name == s.ID {
			name = ""
		}
		entry := toolset.SkillCatalogEntry{
			ID:          s.ID,
			Name:        name,
			Scope:       s.Scope,
			Description: s.Description,
		}
		candidate := page
		candidate.Skills = append(append([]toolset.SkillCatalogEntry(nil), page.Skills...), entry)
		if offset+len(candidate.Skills) < len(matched) {
			candidate.HasMore = true
			candidate.NextOffset = offset + len(candidate.Skills)
		} else {
			candidate.HasMore = false
			candidate.NextOffset = 0
		}
		raw, err := json.Marshal(candidate)
		if err != nil {
			return toolset.SkillCatalogPage{}, err
		}
		if len(raw) <= toolset.MaxSkillCatalogResponseBytes {
			page = candidate
			continue
		}
		if k > 0 {
			break
		}
		// k == 0: guarantee progress of at least 1 entry by truncating Description (then Name if needed)
		// at UTF-8 rune boundaries while preserving ID and Scope.
		page = fitSingleCatalogEntry(page, entry, len(matched))
		break
	}
	return page, nil
}

func fitSingleCatalogEntry(base toolset.SkillCatalogPage, entry toolset.SkillCatalogEntry, matchedCount int) toolset.SkillCatalogPage {
	entry.DescriptionTruncated = true
	makePage := func(e toolset.SkillCatalogEntry) toolset.SkillCatalogPage {
		p := base
		p.Skills = []toolset.SkillCatalogEntry{e}
		if base.Offset+1 < matchedCount {
			p.HasMore = true
			p.NextOffset = base.Offset + 1
		}
		return p
	}
	descRunes := []rune(entry.Description)
	lo, hi := 0, len(descRunes)
	bestDesc := ""
	found := false
	for lo <= hi {
		mid := (lo + hi) / 2
		trial := entry
		if mid < len(descRunes) && mid > 0 {
			trial.Description = string(descRunes[:mid]) + "…"
		} else {
			trial.Description = string(descRunes[:mid])
		}
		if raw, err := json.Marshal(makePage(trial)); err == nil && len(raw) <= toolset.MaxSkillCatalogResponseBytes {
			found = true
			bestDesc = trial.Description
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	entry.Description = bestDesc
	if found {
		return makePage(entry)
	}
	// Description alone at "" still exceeded 16 KiB (e.g. oversized Name): shrink Name on rune boundary.
	nameRunes := []rune(entry.Name)
	lo, hi = 0, len(nameRunes)
	bestName := ""
	for lo <= hi {
		mid := (lo + hi) / 2
		trial := entry
		trial.Name = string(nameRunes[:mid])
		if raw, err := json.Marshal(makePage(trial)); err == nil && len(raw) <= toolset.MaxSkillCatalogResponseBytes {
			bestName = trial.Name
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	entry.Name = bestName
	return makePage(entry)
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
