package skills

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

var skillIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// Registry reads skills from the roots of two scopes. Within a scope the roots are in precedence
// order; across scopes the project scope overrides the global one. Only a scope's LumenCortex root
// is written; the enablement of every skill of a scope, wherever it lives, is kept in that scope's
// state file beside its LumenCortex root, so another tool's files are never changed.
type Registry struct {
	global  []Root
	project []Root
}

// DefaultRoots are the roots LumenCortex reads: its own directory, then the cross-agent
// conventions other tools install into (.agents/skills, .claude/skills), for the user (globalDir,
// home) and for the workspace. An empty argument leaves its roots out.
func DefaultRoots(globalDir, home, workspace string) (global, project []Root) {
	if globalDir != "" {
		global = append(global, Root{Source: SourceLumenCortex, Path: filepath.Join(globalDir, "skills"), Writable: true})
	}
	if home != "" {
		global = append(global,
			Root{Source: SourceAgents, Path: filepath.Join(home, ".agents", "skills")},
			Root{Source: SourceClaude, Path: filepath.Join(home, ".claude", "skills")},
		)
	}
	if workspace != "" {
		project = append(project,
			Root{Source: SourceLumenCortex, Path: filepath.Join(workspace, ".lumencortex", "skills"), Writable: true},
			Root{Source: SourceAgents, Path: filepath.Join(workspace, ".agents", "skills")},
			Root{Source: SourceClaude, Path: filepath.Join(workspace, ".claude", "skills")},
		)
	}
	return global, project
}

// NewRegistry takes the roots of each scope in precedence order. A scope has at most one writable
// root, and a root path appears once (a home directory that is also the workspace would otherwise
// list the same skills in both scopes).
func NewRegistry(global, project []Root) (*Registry, error) {
	seen := map[string]bool{}
	normalize := func(roots []Root) ([]Root, error) {
		out := make([]Root, 0, len(roots))
		writable := false
		for _, root := range roots {
			if root.Path == "" {
				continue
			}
			abs, err := filepath.Abs(root.Path)
			if err != nil {
				return nil, err
			}
			if seen[abs] {
				continue
			}
			seen[abs] = true
			if root.Writable {
				if writable {
					return nil, errors.New("a skill scope has at most one writable root")
				}
				writable = true
			}
			root.Path = abs
			out = append(out, root)
		}
		return out, nil
	}
	g, err := normalize(global)
	if err != nil {
		return nil, err
	}
	p, err := normalize(project)
	if err != nil {
		return nil, err
	}
	if len(g) == 0 && len(p) == 0 {
		return nil, errors.New("at least one skill root is required")
	}
	return &Registry{global: g, project: p}, nil
}

// Roots returns a scope's roots in precedence order.
func (r *Registry) Roots(scope string) []Root {
	switch scope {
	case ScopeGlobal:
		return append([]Root(nil), r.global...)
	case ScopeProject:
		return append([]Root(nil), r.project...)
	}
	return nil
}

// List returns a scope's skills. The global and project lists include the skills a higher root of
// the same scope shadows (ShadowedBy set), so a conflict is visible; the effective list holds the
// winners only, which is what a run may use.
func (r *Registry) List(scope string) ([]Skill, error) {
	switch scope {
	case ScopeGlobal, ScopeProject:
		all, err := r.scanScope(scope)
		if err != nil {
			return nil, err
		}
		state, err := loadState(r.statePath(scope))
		if err != nil {
			return nil, err
		}
		out := make([]Skill, 0, len(all))
		for _, item := range all {
			item.Enabled = enabledIn(state, item.ID, true)
			out = append(out, item.Skill)
		}
		return out, nil
	case ScopeEffective, "":
		items, err := r.rawEffective(true)
		if err != nil {
			return nil, err
		}
		return sortedSkills(items), nil
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

// Save writes a skill into the scope's LumenCortex root. A skill of the same id in another tool's
// root stays where it is; the saved copy shadows it.
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
	root, err := r.writableRoot(scope)
	if err != nil {
		return err
	}
	dir := filepath.Join(root.Path, id)
	if info, err := os.Lstat(dir); err == nil && info.Mode()&os.ModeSymlink != 0 {
		// A linked skill belongs to whatever the link points at; writing through it would change
		// another tool's or repository's files.
		return fmt.Errorf("skill %s is a link to %s: edit it where it lives", id, resolvedOr(dir))
	}
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

// Delete removes a skill from the scope's LumenCortex root and forgets its enablement. A skill that
// lives only in another tool's root is refused: LumenCortex does not delete files it did not write.
func (r *Registry) Delete(scope, id string) error {
	id = strings.TrimSpace(id)
	if !skillIDPattern.MatchString(id) {
		return errors.New("invalid skill id")
	}
	root, err := r.writableRoot(scope)
	if err != nil {
		return err
	}
	dir := filepath.Join(root.Path, id)
	info, err := os.Lstat(dir)
	switch {
	case os.IsNotExist(err):
		all, scanErr := r.scanScope(scope)
		if scanErr != nil {
			return scanErr
		}
		for _, item := range all {
			if item.ID == id {
				return fmt.Errorf("skill %s is read-only here (%s): remove it with the tool that installed it, or disable it", id, item.Root)
			}
		}
		return os.ErrNotExist
	case err != nil:
		return err
	case info.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("skill %s is a link to %s: remove the link yourself or disable it", id, resolvedOr(dir))
	}
	if err := os.Remove(filepath.Join(dir, "SKILL.md")); err != nil && !os.IsNotExist(err) {
		return err
	}
	_ = os.Remove(dir) // only when empty: other files a user put there stay
	state, err := loadState(r.statePath(scope))
	if err != nil {
		return err
	}
	delete(state.Skills, id)
	return saveState(r.statePath(scope), state)
}

// SetEnabled records whether a skill is used, in the scope's state file. It works for every skill,
// including one in another tool's root, because it never touches the skill's files. A project
// setting overrides the global one for that workspace.
func (r *Registry) SetEnabled(scope, id string, enabled bool) error {
	id = strings.TrimSpace(id)
	if !skillIDPattern.MatchString(id) {
		return errors.New("invalid skill id")
	}
	if scope != ScopeGlobal && scope != ScopeProject {
		return errors.New("skill enable scope must be global or project")
	}
	if _, err := r.writableRoot(scope); err != nil {
		return err
	}
	effective, err := r.rawEffective(true)
	if err != nil {
		return err
	}
	if _, ok := effective[id]; !ok {
		return fmt.Errorf("unknown skill: %s", id)
	}
	if scope == ScopeGlobal {
		global, err := r.winners(ScopeGlobal)
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

// Prompt returns the skills catalog of a run and the skills it offers. The catalog names each
// enabled skill and its description, nothing more: a skill's instructions reach the model only when
// it loads them with read_skill (progressive disclosure, as the Agent Skills format intends), so
// installing many skills costs a line each, not their whole text. A project-scope skill is offered
// only when opts.IncludeProject says the workspace is trusted; the project skills withheld for that
// reason are returned so the caller can report them.
func (r *Registry) Prompt(opts PromptOptions) (PromptResult, error) {
	items, err := r.rawEffective(opts.IncludeProject)
	if err != nil {
		return PromptResult{}, err
	}
	result := PromptResult{}
	if !opts.IncludeProject {
		project, err := r.winners(ScopeProject)
		if err != nil {
			return PromptResult{}, err
		}
		state, err := loadState(r.statePath(ScopeProject))
		if err != nil {
			return PromptResult{}, err
		}
		for _, item := range sortedRaw(project) {
			item.Enabled = enabledIn(state, item.ID, true)
			result.Withheld = append(result.Withheld, item.Skill)
		}
	}
	var lines strings.Builder
	omitted := 0
	for _, item := range sortedRaw(items) {
		if !item.Enabled || item.Error != "" {
			continue
		}
		line := catalogLine(item.Skill)
		if lines.Len()+len(line) > MaxCatalogBytes {
			omitted++
			continue
		}
		lines.WriteString(line)
		result.Included = append(result.Included, item.Skill)
	}
	if len(result.Included) == 0 {
		return result, nil
	}
	text := catalogPreamble + lines.String()
	if omitted > 0 {
		text += fmt.Sprintf("\n(%d more enabled skills are not listed: the catalog is full.)\n", omitted)
	}
	result.Text = text
	return result, nil
}

// MaxCatalogBytes bounds the skills catalog in the system prompt.
const MaxCatalogBytes = 32 << 10

const catalogPreamble = "Skills are instructions the user installed for particular kinds of work. Only each skill's id and description are listed here. " +
	"When a task matches a description, call read_skill with that id before you act and follow what it returns, unless it conflicts with higher-priority instructions. " +
	"A skill may name other files by a path relative to its directory (references, scripts, templates): read them with read_skill(id, path) when you need them. " +
	"Do not load a skill that does not apply.\n\n"

func catalogLine(s Skill) string {
	label := "`" + s.ID + "`"
	if s.Name != "" && s.Name != s.ID {
		label += " (" + s.Name + ")"
	}
	description := strings.Join(strings.Fields(s.Description), " ")
	if description == "" {
		description = "no description"
	}
	return "- " + label + ": " + description + "\n"
}

// ReadResource reads one file or directory of a skill: "" (or "SKILL.md") is its instructions,
// another path is relative to the skill directory. The path must stay inside the directory the
// skill resolves to, also through links, and files are text only and bounded.
func ReadResource(s Skill, rel string) (Resource, error) {
	dir := filepath.Dir(s.Path)
	base, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return Resource{}, fmt.Errorf("skill %s is unavailable: %w", s.ID, err)
	}
	rel = strings.TrimSpace(rel)
	if rel == "" {
		rel = "SKILL.md"
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return Resource{}, fmt.Errorf("path %q is outside skill %s", rel, s.ID)
	}
	target, err := filepath.EvalSymlinks(filepath.Join(base, clean))
	if err != nil {
		return Resource{}, fmt.Errorf("skill %s has no %s", s.ID, filepath.ToSlash(clean))
	}
	if target != base && !strings.HasPrefix(target, base+string(filepath.Separator)) {
		return Resource{}, fmt.Errorf("path %q is outside skill %s", rel, s.ID)
	}
	out := Resource{ID: s.ID, Path: filepath.ToSlash(clean)}
	info, err := os.Stat(target)
	if err != nil {
		return Resource{}, err
	}
	if info.IsDir() {
		entries, err := os.ReadDir(target)
		if err != nil {
			return Resource{}, err
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			if len(out.Entries) == MaxResourceEntries {
				out.Truncated = true
				break
			}
			name := entry.Name()
			if entry.IsDir() {
				name += "/"
			}
			out.Entries = append(out.Entries, name)
		}
		return out, nil
	}
	f, err := os.Open(target)
	if err != nil {
		return Resource{}, err
	}
	defer f.Close()
	buf := make([]byte, MaxResourceBytes+1)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return Resource{}, err
	}
	buf = buf[:n]
	if len(buf) > MaxResourceBytes {
		buf, out.Truncated = buf[:MaxResourceBytes], true
		for len(buf) > 0 && !utf8.Valid(buf) {
			buf = buf[:len(buf)-1] // do not end inside a rune
		}
	}
	if !utf8.Valid(buf) || strings.IndexByte(string(buf), 0) >= 0 {
		return Resource{}, fmt.Errorf("%s in skill %s is not a text file", out.Path, s.ID)
	}
	out.Content = string(buf)
	return out, nil
}

func (r *Registry) rawScope(scope string) (map[string]rawSkill, error) {
	switch scope {
	case ScopeGlobal, ScopeProject:
		items, err := r.winners(scope)
		if err != nil {
			return nil, err
		}
		state, err := loadState(r.statePath(scope))
		if err != nil {
			return nil, err
		}
		for id, item := range items {
			item.Enabled = enabledIn(state, id, true)
			items[id] = item
		}
		return items, nil
	case ScopeEffective, "":
		return r.rawEffective(true)
	default:
		return nil, errors.New("unknown skill scope")
	}
}

// rawEffective merges the scopes by id, project overriding global. includeProject false merges the
// global scope only: it is the prompt of a run in an untrusted workspace (ADR-0013).
func (r *Registry) rawEffective(includeProject bool) (map[string]rawSkill, error) {
	global, err := r.winners(ScopeGlobal)
	if err != nil {
		return nil, err
	}
	project := map[string]rawSkill{}
	if includeProject {
		if project, err = r.winners(ScopeProject); err != nil {
			return nil, err
		}
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
		item.Enabled = enabledIn(projectState, id, enabledIn(globalState, id, true))
		out[id] = item
	}
	for id, item := range project {
		item.Enabled = enabledIn(projectState, id, true)
		if _, ok := global[id]; ok {
			item.Overridden = true
		}
		out[id] = item
	}
	return out, nil
}

// winners is a scope's skills by id, the first root that has an id providing it.
func (r *Registry) winners(scope string) (map[string]rawSkill, error) {
	all, err := r.scanScope(scope)
	if err != nil {
		return nil, err
	}
	out := make(map[string]rawSkill, len(all))
	for _, item := range all {
		if item.ShadowedBy == "" {
			out[item.ID] = item
		}
	}
	return out, nil
}

// scanScope lists every skill of a scope's roots, sorted by id then precedence, marking the ones a
// higher root shadows.
func (r *Registry) scanScope(scope string) ([]rawSkill, error) {
	roots := r.global
	if scope == ScopeProject {
		roots = r.project
	}
	winner := map[string]string{}
	var out []rawSkill
	for _, root := range roots {
		items, err := scanRoot(root, scope)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			if owner, ok := winner[item.ID]; ok {
				item.ShadowedBy = owner
			} else {
				winner[item.ID] = scope + ":" + root.Source
			}
			out = append(out, item)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// scanRoot reads <root>/<id>/SKILL.md. A skill directory may be a symbolic link (how skill
// installers share one copy between tools); it is followed one level and reported as linked. A
// root that does not exist has no skills.
func scanRoot(root Root, scope string) ([]rawSkill, error) {
	entries, err := os.ReadDir(root.Path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		if root.Writable {
			return nil, err
		}
		// Another tool's directory that cannot be read hides its skills; it does not stop runs.
		return nil, nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	var out []rawSkill
	for _, entry := range entries {
		if len(out) >= MaxSkills {
			break
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") || !skillIDPattern.MatchString(name) {
			continue
		}
		dir := filepath.Join(root.Path, name)
		linked := entry.Type()&os.ModeSymlink != 0
		if !entry.IsDir() && !linked {
			continue
		}
		item := rawSkill{dir: dir, Skill: Skill{
			ID: name, Name: name, Scope: scope, Source: root.Source, Root: root.Path,
			Path: filepath.Join(dir, "SKILL.md"), Linked: linked, ReadOnly: !root.Writable || linked, Enabled: true,
		}}
		if linked {
			real, err := filepath.EvalSymlinks(dir)
			if err != nil {
				item.Error = "broken link: " + linkTarget(dir)
				out = append(out, item)
				continue
			}
			item.RealPath = real
			if info, err := os.Stat(real); err != nil || !info.IsDir() {
				continue
			}
		}
		info, err := os.Stat(item.Path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			item.Error = err.Error()
			out = append(out, item)
			continue
		}
		item.Bytes = info.Size()
		if info.Size() > MaxSkillBytes {
			item.Error = "SKILL.md exceeds 128 KiB limit"
			out = append(out, item)
			continue
		}
		raw, err := os.ReadFile(item.Path)
		if err != nil {
			item.Error = err.Error()
			out = append(out, item)
			continue
		}
		item.content = string(raw)
		item.Name, item.Description = parseMetadata(item.ID, item.content)
		out = append(out, item)
	}
	return out, nil
}

func (r *Registry) writableRoot(scope string) (Root, error) {
	roots := r.global
	switch scope {
	case ScopeGlobal:
	case ScopeProject:
		roots = r.project
	default:
		return Root{}, errors.New("skill write scope must be global or project")
	}
	for _, root := range roots {
		if root.Writable {
			return root, nil
		}
	}
	return Root{}, fmt.Errorf("%s skill root is unavailable", scope)
}

func (r *Registry) statePath(scope string) string {
	root, err := r.writableRoot(scope)
	if err != nil {
		return ""
	}
	return filepath.Join(filepath.Dir(root.Path), "skills-state.json")
}

func enabledIn(state stateFile, id string, fallback bool) bool {
	if entry, ok := state.Skills[id]; ok {
		return entry.Enabled
	}
	return fallback
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

func sortedRaw(items map[string]rawSkill) []rawSkill {
	out := make([]rawSkill, 0, len(items))
	for _, item := range items {
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func sortedSkills(items map[string]rawSkill) []Skill {
	raw := sortedRaw(items)
	out := make([]Skill, len(raw))
	for i, item := range raw {
		out[i] = item.Skill
	}
	return out
}

func linkTarget(path string) string {
	target, err := os.Readlink(path)
	if err != nil {
		return path
	}
	return target
}

func resolvedOr(path string) string {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	return linkTarget(path)
}

// parseMetadata reads name and description from the YAML frontmatter of a SKILL.md (quoted,
// folded and block scalars all work); without a description, the first prose line stands in.
func parseMetadata(id, content string) (string, string) {
	name, description := id, ""
	normalized := strings.ReplaceAll(strings.TrimPrefix(content, "\ufeff"), "\r\n", "\n")
	body := normalized
	if strings.HasPrefix(normalized, "---\n") {
		rest := normalized[4:]
		end := strings.Index(rest, "\n---")
		if strings.HasPrefix(rest, "---") {
			end = -1
			body = rest[3:]
		}
		if end >= 0 {
			front := rest[:end]
			body = rest[end+4:]
			var meta struct {
				Name        any `yaml:"name"`
				Description any `yaml:"description"`
			}
			if yaml.Unmarshal([]byte(front), &meta) == nil {
				if v, ok := meta.Name.(string); ok && strings.TrimSpace(v) != "" {
					name = strings.TrimSpace(v)
				}
				if v, ok := meta.Description.(string); ok {
					description = strings.TrimSpace(v)
				}
			} else {
				name, description = lineMetadata(id, front)
			}
		}
	}
	if description == "" {
		for _, line := range strings.Split(body, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") || line == "---" {
				continue
			}
			description = line
			break
		}
	}
	return truncateRunes(name, 128), truncateRunes(description, MaxDescriptionBytes)
}

// lineMetadata is the fallback for frontmatter YAML cannot parse: single-line key: value pairs.
func lineMetadata(id, front string) (string, string) {
	name, description := id, ""
	for _, line := range strings.Split(front, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), "`'\"")
		switch strings.TrimSpace(key) {
		case "name":
			if value != "" {
				name = value
			}
		case "description":
			description = value
		}
	}
	return name, description
}

// truncateRunes cuts s to at most n bytes without splitting a rune.
func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
