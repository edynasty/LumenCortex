package skills

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var skillIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

type Registry struct {
	globalRoot  string
	projectRoot string
}

func NewRegistry(globalRoot, projectRoot string) (*Registry, error) {
	var err error
	if globalRoot != "" {
		globalRoot, err = filepath.Abs(globalRoot)
		if err != nil {
			return nil, err
		}
	}
	if projectRoot != "" {
		projectRoot, err = filepath.Abs(projectRoot)
		if err != nil {
			return nil, err
		}
	}
	if globalRoot == "" && projectRoot == "" {
		return nil, errors.New("at least one skill root is required")
	}
	return &Registry{globalRoot: globalRoot, projectRoot: projectRoot}, nil
}

func (r *Registry) List(scope string) ([]Skill, error) {
	global, err := r.scanRoot(r.globalRoot, ScopeGlobal)
	if err != nil {
		return nil, err
	}
	project, err := r.scanRoot(r.projectRoot, ScopeProject)
	if err != nil {
		return nil, err
	}
	globalState, err := loadState(r.statePath(ScopeGlobal))
	if err != nil {
		return nil, err
	}
	projectState, err := loadState(r.statePath(ScopeProject))
	if err != nil {
		return nil, err
	}

	switch scope {
	case ScopeGlobal:
		return sortedSkills(applyScopeState(global, globalState)), nil
	case ScopeProject:
		return sortedSkills(applyScopeState(project, projectState)), nil
	case ScopeEffective, "":
		effective := make(map[string]rawSkill, len(global)+len(project))
		for id, item := range global {
			if state, ok := globalState.Skills[id]; ok {
				item.Enabled = state.Enabled
			} else {
				item.Enabled = true
			}
			if state, ok := projectState.Skills[id]; ok {
				item.Enabled = state.Enabled
			}
			effective[id] = item
		}
		for id, item := range project {
			item.Enabled = true
			if state, ok := projectState.Skills[id]; ok {
				item.Enabled = state.Enabled
			}
			if _, ok := global[id]; ok {
				item.Overridden = true
			}
			effective[id] = item
		}
		return sortedSkills(effective), nil
	default:
		return nil, errors.New("unknown skill scope")
	}
}

func (r *Registry) Content(scope, id string) (string, error) {
	id = strings.TrimSpace(id)
	if !skillIDPattern.MatchString(id) {
		return "", errors.New("invalid skill id")
	}
	items, err := r.rawScope(scope)
	if err != nil {
		return "", err
	}
	item, ok := items[id]
	if !ok {
		return "", os.ErrNotExist
	}
	if item.Error != "" {
		return "", errors.New(item.Error)
	}
	return item.content, nil
}

func (r *Registry) Save(scope, id, content string) error {
	id = strings.TrimSpace(id)
	if !skillIDPattern.MatchString(id) {
		return errors.New("skill id must match [A-Za-z0-9._-] and be at most 64 characters")
	}
	if strings.TrimSpace(content) == "" {
		return errors.New("skill content is required")
	}
	if len(content) > MaxSkillBytes {
		return errors.New("skill content exceeds 128 KiB limit")
	}
	root, err := r.root(scope)
	if err != nil {
		return err
	}
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, "SKILL.md")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (r *Registry) Delete(scope, id string) error {
	id = strings.TrimSpace(id)
	if !skillIDPattern.MatchString(id) {
		return errors.New("invalid skill id")
	}
	root, err := r.root(scope)
	if err != nil {
		return err
	}
	path := filepath.Join(root, id, "SKILL.md")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	state, err := loadState(r.statePath(scope))
	if err != nil {
		return err
	}
	delete(state.Skills, id)
	return saveState(r.statePath(scope), state)
}

func (r *Registry) SetEnabled(scope, id string, enabled bool) error {
	id = strings.TrimSpace(id)
	if !skillIDPattern.MatchString(id) {
		return errors.New("invalid skill id")
	}
	if scope != ScopeGlobal && scope != ScopeProject {
		return errors.New("skill enable scope must be global or project")
	}
	effective, err := r.rawScope(ScopeEffective)
	if err != nil {
		return err
	}
	if _, ok := effective[id]; !ok {
		return fmt.Errorf("unknown skill: %s", id)
	}
	if scope == ScopeGlobal {
		global, err := r.rawScope(ScopeGlobal)
		if err != nil {
			return err
		}
		if _, ok := global[id]; !ok {
			return fmt.Errorf("skill is not defined in global scope: %s", id)
		}
	}
	state, err := loadState(r.statePath(scope))
	if err != nil {
		return err
	}
	state.Skills[id] = stateEntry{Enabled: enabled}
	return saveState(r.statePath(scope), state)
}

func (r *Registry) Prompt() (string, []Skill, error) {
	items, err := r.rawEffective()
	if err != nil {
		return "", nil, err
	}
	ids := make([]string, 0, len(items))
	for id := range items {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var b strings.Builder
	used := make([]Skill, 0)
	for _, id := range ids {
		item := items[id]
		if !item.Enabled || item.Error != "" {
			continue
		}
		header := fmt.Sprintf("\n\n## Skill: %s (%s)\n", item.Name, item.ID)
		remaining := MaxPromptBytes - b.Len()
		if remaining <= len(header) {
			break
		}
		b.WriteString(header)
		remaining = MaxPromptBytes - b.Len()
		content := item.content
		if len(content) > remaining {
			content = content[:remaining]
		}
		b.WriteString(content)
		used = append(used, item.Skill)
		if b.Len() >= MaxPromptBytes {
			break
		}
	}
	if b.Len() == 0 {
		return "", used, nil
	}
	return "The following LumenCortex Skills are enabled for this workspace. Treat them as task-specific development instructions unless they conflict with higher-priority instructions." + b.String(), used, nil
}

func (r *Registry) rawScope(scope string) (map[string]rawSkill, error) {
	switch scope {
	case ScopeGlobal:
		items, err := r.scanRoot(r.globalRoot, ScopeGlobal)
		if err != nil {
			return nil, err
		}
		state, err := loadState(r.statePath(ScopeGlobal))
		if err != nil {
			return nil, err
		}
		return applyScopeState(items, state), nil
	case ScopeProject:
		items, err := r.scanRoot(r.projectRoot, ScopeProject)
		if err != nil {
			return nil, err
		}
		state, err := loadState(r.statePath(ScopeProject))
		if err != nil {
			return nil, err
		}
		return applyScopeState(items, state), nil
	case ScopeEffective, "":
		return r.rawEffective()
	default:
		return nil, errors.New("unknown skill scope")
	}
}

func (r *Registry) rawEffective() (map[string]rawSkill, error) {
	global, err := r.scanRoot(r.globalRoot, ScopeGlobal)
	if err != nil {
		return nil, err
	}
	project, err := r.scanRoot(r.projectRoot, ScopeProject)
	if err != nil {
		return nil, err
	}
	globalState, err := loadState(r.statePath(ScopeGlobal))
	if err != nil {
		return nil, err
	}
	projectState, err := loadState(r.statePath(ScopeProject))
	if err != nil {
		return nil, err
	}
	out := make(map[string]rawSkill, len(global)+len(project))
	for id, item := range global {
		item.Enabled = true
		if state, ok := globalState.Skills[id]; ok {
			item.Enabled = state.Enabled
		}
		if state, ok := projectState.Skills[id]; ok {
			item.Enabled = state.Enabled
		}
		out[id] = item
	}
	for id, item := range project {
		item.Enabled = true
		if state, ok := projectState.Skills[id]; ok {
			item.Enabled = state.Enabled
		}
		if _, ok := global[id]; ok {
			item.Overridden = true
		}
		out[id] = item
	}
	return out, nil
}

func (r *Registry) scanRoot(root, scope string) (map[string]rawSkill, error) {
	out := make(map[string]rawSkill)
	if root == "" {
		return out, nil
	}
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if len(out) >= MaxSkills {
			break
		}
		if !entry.IsDir() || !skillIDPattern.MatchString(entry.Name()) {
			continue
		}
		path := filepath.Join(root, entry.Name(), "SKILL.md")
		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		item := rawSkill{Skill: Skill{
			ID: entry.Name(),
			Name: entry.Name(),
			Scope: scope,
			Path: path,
			Enabled: true,
			Bytes: info.Size(),
		}}
		if info.Size() > MaxSkillBytes {
			item.Error = "SKILL.md exceeds 128 KiB limit"
			out[item.ID] = item
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			item.Error = err.Error()
			out[item.ID] = item
			continue
		}
		item.content = string(raw)
		item.Name, item.Description = parseMetadata(item.ID, item.content)
		out[item.ID] = item
	}
	return out, nil
}

func (r *Registry) root(scope string) (string, error) {
	switch scope {
	case ScopeGlobal:
		if r.globalRoot == "" {
			return "", errors.New("global skill root is unavailable")
		}
		return r.globalRoot, nil
	case ScopeProject:
		if r.projectRoot == "" {
			return "", errors.New("project skill root is unavailable")
		}
		return r.projectRoot, nil
	default:
		return "", errors.New("skill write scope must be global or project")
	}
}

func (r *Registry) statePath(scope string) string {
	root, err := r.root(scope)
	if err != nil {
		return ""
	}
	return filepath.Join(filepath.Dir(root), "skills-state.json")
}

func loadState(path string) (stateFile, error) {
	state := stateFile{Skills: map[string]stateEntry{}}
	if path == "" {
		return state, nil
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if len(raw) > MaxStateBytes {
		return state, errors.New("skill state exceeds runtime limit")
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		return state, err
	}
	if state.Skills == nil {
		state.Skills = map[string]stateEntry{}
	}
	return state, nil
}

func saveState(path string, state stateFile) error {
	if path == "" {
		return errors.New("skill state path is unavailable")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if len(raw) > MaxStateBytes {
		return errors.New("skill state exceeds runtime limit")
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func applyScopeState(items map[string]rawSkill, state stateFile) map[string]rawSkill {
	for id, item := range items {
		item.Enabled = true
		if value, ok := state.Skills[id]; ok {
			item.Enabled = value.Enabled
		}
		items[id] = item
	}
	return items
}

func sortedSkills(items map[string]rawSkill) []Skill {
	ids := make([]string, 0, len(items))
	for id := range items {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]Skill, 0, len(ids))
	for _, id := range ids {
		out = append(out, items[id].Skill)
	}
	return out
}

func parseMetadata(id, content string) (string, string) {
	name := id
	description := ""
	normalized := strings.ReplaceAll(strings.TrimPrefix(content, "\ufeff"), "\r\n", "\n")
	body := normalized
	if strings.HasPrefix(normalized, "---\n") {
		if end := strings.Index(normalized[4:], "\n---\n"); end >= 0 {
			front := normalized[4 : 4+end]
			body = normalized[4+end+5:]
			for _, line := range strings.Split(front, "\n") {
				key, value, ok := strings.Cut(line, ":")
				if !ok {
					continue
				}
				value = strings.Trim(strings.TrimSpace(value), "`'")
				switch strings.TrimSpace(key) {
				case "name":
					if value != "" {
						name = value
					}
				case "description":
					description = value
				}
			}
		}
	}
	if description == "" {
		for _, line := range strings.Split(body, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			description = line
			break
		}
	}
	if len(name) > 128 {
		name = name[:128]
	}
	if len(description) > 512 {
		description = description[:512]
	}
	return name, description
}
