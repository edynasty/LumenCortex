package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// lcRegistry is a registry with one writable LumenCortex root per scope.
func lcRegistry(t *testing.T, globalRoot, projectRoot string) *Registry {
	t.Helper()
	registry, err := NewRegistry(
		[]Root{{Source: SourceLumenCortex, Path: globalRoot, Writable: true}},
		[]Root{{Source: SourceLumenCortex, Path: projectRoot, Writable: true}},
	)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func writeSkill(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLayeredSkillsOverrideDisableAndPrompt(t *testing.T) {
	registry := lcRegistry(t, t.TempDir(), t.TempDir())

	globalContent := "---\nname: Java Backend\ndescription: Global Java conventions\n---\nUse layered services and tests."
	if err := registry.Save(ScopeGlobal, "java-backend", globalContent); err != nil {
		t.Fatal(err)
	}
	items, err := registry.List(ScopeEffective)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Name != "Java Backend" || !items[0].Enabled || items[0].Scope != ScopeGlobal || items[0].ReadOnly {
		t.Fatalf("global effective=%#v", items)
	}

	prompt, err := registry.Prompt(PromptOptions{IncludeProject: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(prompt.Included) != 1 || !strings.Contains(prompt.Text, "`java-backend` (Java Backend): Global Java conventions") {
		t.Fatalf("prompt=%q used=%#v", prompt.Text, prompt.Included)
	}

	if err := registry.SetEnabled(ScopeProject, "java-backend", false); err != nil {
		t.Fatal(err)
	}
	items, err = registry.List(ScopeEffective)
	if err != nil {
		t.Fatal(err)
	}
	if items[0].Enabled {
		t.Fatalf("project disable did not override inherited global: %#v", items[0])
	}
	prompt, err = registry.Prompt(PromptOptions{IncludeProject: true})
	if err != nil {
		t.Fatal(err)
	}
	if prompt.Text != "" || len(prompt.Included) != 0 {
		t.Fatalf("disabled prompt=%q used=%#v", prompt.Text, prompt.Included)
	}

	projectContent := "---\nname: Project Java\ndescription: Project override\n---\nPrefer project-specific conventions."
	if err := registry.Save(ScopeProject, "java-backend", projectContent); err != nil {
		t.Fatal(err)
	}
	if err := registry.SetEnabled(ScopeProject, "java-backend", true); err != nil {
		t.Fatal(err)
	}
	items, err = registry.List(ScopeEffective)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Scope != ScopeProject || !items[0].Overridden || items[0].Name != "Project Java" {
		t.Fatalf("project override=%#v", items)
	}
	content, err := registry.Content(ScopeEffective, "java-backend")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "project-specific") {
		t.Fatalf("content=%q", content)
	}

	if err := registry.Delete(ScopeProject, "java-backend"); err != nil {
		t.Fatal(err)
	}
	items, err = registry.List(ScopeEffective)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Scope != ScopeGlobal || !items[0].Enabled {
		t.Fatalf("global fallback=%#v", items)
	}
}

// The catalog carries ids and descriptions, never a skill's body, and stays bounded however many
// skills are installed.
func TestSkillPromptIsACatalogAndBounded(t *testing.T) {
	registry := lcRegistry(t, t.TempDir(), t.TempDir())
	body := strings.Repeat("BODY ", 20<<10)
	for _, id := range []string{"a", "b"} {
		if err := registry.Save(ScopeProject, id, "---\nname: "+id+"\ndescription: does "+id+"\n---\n"+body); err != nil {
			t.Fatal(err)
		}
	}
	prompt, err := registry.Prompt(PromptOptions{IncludeProject: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(prompt.Text, "BODY") || !strings.Contains(prompt.Text, "- `a`: does a\n") || !strings.Contains(prompt.Text, "read_skill") {
		t.Fatalf("catalog=%q", prompt.Text)
	}

	long := strings.Repeat("d", MaxDescriptionBytes)
	for i := range 40 {
		id := "skill-" + string(rune('a'+i/26)) + string(rune('a'+i%26))
		if err := registry.Save(ScopeGlobal, id, "---\ndescription: "+long+"\n---\nx"); err != nil {
			t.Fatal(err)
		}
	}
	prompt, err = registry.Prompt(PromptOptions{IncludeProject: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(prompt.Text) > MaxCatalogBytes+len(catalogPreamble)+128 {
		t.Fatalf("catalog too large: %d", len(prompt.Text))
	}
	if len(prompt.Included) >= 42 || !strings.Contains(prompt.Text, "more enabled skills are not listed") {
		t.Fatalf("included=%d text tail=%q", len(prompt.Included), prompt.Text[len(prompt.Text)-80:])
	}
}

func TestSkillRejectsInvalidID(t *testing.T) {
	registry := lcRegistry(t, t.TempDir(), t.TempDir())
	if err := registry.Save(ScopeProject, "../escape", "bad"); err == nil {
		t.Fatal("expected invalid id rejection")
	}
}

// A project SKILL.md is repository content: in an untrusted workspace the catalog carries the
// global scope only, and reports the project skills it withheld (ADR-0013).
func TestSkillPromptWithholdsProjectScopeWithoutTrust(t *testing.T) {
	registry := lcRegistry(t, t.TempDir(), t.TempDir())
	if err := registry.Save(ScopeGlobal, "shared", "---\nname: Shared\ndescription: global text\n---\nx"); err != nil {
		t.Fatal(err)
	}
	if err := registry.Save(ScopeProject, "shared", "---\nname: Shared\ndescription: project override text\n---\nx"); err != nil {
		t.Fatal(err)
	}
	if err := registry.Save(ScopeProject, "local", "---\nname: Local\ndescription: project only text\n---\nx"); err != nil {
		t.Fatal(err)
	}

	prompt, err := registry.Prompt(PromptOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt.Text, "global text") || strings.Contains(prompt.Text, "project") {
		t.Fatalf("untrusted prompt=%q", prompt.Text)
	}
	if len(prompt.Included) != 1 || prompt.Included[0].ID != "shared" || prompt.Included[0].Scope != ScopeGlobal {
		t.Fatalf("untrusted included=%#v", prompt.Included)
	}
	withheld := make([]string, 0, len(prompt.Withheld))
	for _, skill := range prompt.Withheld {
		withheld = append(withheld, skill.ID)
	}
	if len(withheld) != 2 || withheld[0] != "local" || withheld[1] != "shared" {
		t.Fatalf("withheld=%v", withheld)
	}

	prompt, err = registry.Prompt(PromptOptions{IncludeProject: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt.Text, "project override text") || !strings.Contains(prompt.Text, "project only text") {
		t.Fatalf("trusted prompt=%q", prompt.Text)
	}
	if len(prompt.Withheld) != 0 {
		t.Fatalf("trusted withheld=%#v", prompt.Withheld)
	}
}

// Skills installed for other tools are read in place: a linked skill directory is followed, the
// LumenCortex root wins a conflict and the loser is listed as shadowed, a broken link is reported,
// and nothing LumenCortex does (save, delete, disable) writes into another tool's directory.
func TestExternalRootsAreReadInPlace(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir()) // RealPath is resolved; macOS temp dirs are links
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	globalDir := filepath.Join(home, "config", "lumencortex")
	shared := filepath.Join(home, "repo", "skills", "linked")
	writeSkill(t, shared, "---\nname: linked\ndescription: \"Quoted: shared through a link\"\n---\nlinked body")
	agents := filepath.Join(home, ".agents", "skills")
	writeSkill(t, filepath.Join(agents, "community"), "---\nname: community\ndescription: >\n  Folded\n  description\n---\ncommunity body")
	writeSkill(t, filepath.Join(agents, "dup"), "---\ndescription: agents copy\n---\nagents dup")
	if err := os.Symlink(shared, filepath.Join(agents, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "missing"), filepath.Join(agents, "broken")); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, filepath.Join(home, ".claude", "skills", "dup"), "---\ndescription: claude copy\n---\nclaude dup")
	writeSkill(t, filepath.Join(workspace, ".agents", "skills", "repo-skill"), "---\ndescription: from the repository\n---\nrepo body")

	g, p := DefaultRoots(globalDir, home, workspace)
	registry, err := NewRegistry(g, p)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Save(ScopeGlobal, "dup", "---\ndescription: lumencortex copy\n---\nlc dup"); err != nil {
		t.Fatal(err)
	}

	global, err := registry.List(ScopeGlobal)
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]Skill{}
	for _, s := range global {
		byKey[s.ID+"@"+s.Source] = s
	}
	if s := byKey["community@agents"]; !s.ReadOnly || s.Description != "Folded description" || s.Error != "" {
		t.Fatalf("community=%#v", s)
	}
	if s := byKey["linked@agents"]; !s.Linked || s.RealPath != shared || s.Description != "Quoted: shared through a link" {
		t.Fatalf("linked=%#v", s)
	}
	if s := byKey["broken@agents"]; !strings.HasPrefix(s.Error, "broken link") {
		t.Fatalf("broken=%#v", s)
	}
	if byKey["dup@lumencortex"].ShadowedBy != "" || byKey["dup@agents"].ShadowedBy != "global:lumencortex" || byKey["dup@claude"].ShadowedBy != "global:lumencortex" {
		t.Fatalf("dup precedence=%#v %#v %#v", byKey["dup@lumencortex"], byKey["dup@agents"], byKey["dup@claude"])
	}

	effective, err := registry.List(ScopeEffective)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, s := range effective {
		ids = append(ids, s.ID+"@"+s.Scope+":"+s.Source)
	}
	if strings.Join(ids, ",") != "broken@global:agents,community@global:agents,dup@global:lumencortex,linked@global:agents,repo-skill@project:agents" {
		t.Fatalf("effective=%v", ids)
	}
	prompt, err := registry.Prompt(PromptOptions{IncludeProject: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(prompt.Text, "broken") || !strings.Contains(prompt.Text, "lumencortex copy") || strings.Contains(prompt.Text, "agents copy") {
		t.Fatalf("catalog=%q", prompt.Text)
	}

	before, _ := os.ReadFile(filepath.Join(agents, "community", "SKILL.md"))
	if err := registry.SetEnabled(ScopeGlobal, "community", false); err != nil {
		t.Fatal(err)
	}
	if err := registry.Delete(ScopeGlobal, "community"); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("delete of an external skill: %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(agents, "community", "SKILL.md"))
	if string(before) != string(after) {
		t.Fatal("an external SKILL.md changed")
	}
	entries, _ := os.ReadDir(filepath.Join(agents, "community"))
	if len(entries) != 1 {
		t.Fatalf("files were added to an external skill: %v", entries)
	}
	if _, err := os.Stat(filepath.Join(globalDir, "skills-state.json")); err != nil {
		t.Fatalf("enablement is not kept in LumenCortex's state: %v", err)
	}
	effective, _ = registry.List(ScopeEffective)
	for _, s := range effective {
		if s.ID == "community" && s.Enabled {
			t.Fatal("community still enabled")
		}
	}

	// A link inside LumenCortex's own root is still someone else's files.
	if err := os.Symlink(shared, filepath.Join(globalDir, "skills", "mine")); err != nil {
		t.Fatal(err)
	}
	if err := registry.Save(ScopeGlobal, "mine", "---\ndescription: x\n---\nx"); err == nil || !strings.Contains(err.Error(), "link") {
		t.Fatalf("save through a link: %v", err)
	}
	if err := registry.Delete(ScopeGlobal, "mine"); err == nil || !strings.Contains(err.Error(), "link") {
		t.Fatalf("delete through a link: %v", err)
	}
	if body, _ := os.ReadFile(filepath.Join(shared, "SKILL.md")); !strings.Contains(string(body), "linked body") {
		t.Fatal("the linked skill's files changed")
	}
}

func TestReadResourceStaysInsideTheSkill(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside.md")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(root, "real", "demo")
	writeSkill(t, real, "---\ndescription: demo\n---\nsee references/guide.md")
	if err := os.MkdirAll(filepath.Join(real, "references"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "references", "guide.md"), []byte("guide text"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "logo.bin"), []byte{0x89, 0, 0xff}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(real, "escape.md")); err != nil {
		t.Fatal(err)
	}
	skillsDir := filepath.Join(root, "skills")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The skill is reached through a link, as skill installers lay it out.
	if err := os.Symlink(real, filepath.Join(skillsDir, "demo")); err != nil {
		t.Fatal(err)
	}
	s := Skill{ID: "demo", Path: filepath.Join(skillsDir, "demo", "SKILL.md")}

	if r, err := ReadResource(s, ""); err != nil || !strings.Contains(r.Content, "see references/guide.md") {
		t.Fatalf("SKILL.md=%#v %v", r, err)
	}
	if r, err := ReadResource(s, "references/guide.md"); err != nil || r.Content != "guide text" {
		t.Fatalf("guide=%#v %v", r, err)
	}
	if r, err := ReadResource(s, "references"); err != nil || len(r.Entries) != 1 || r.Entries[0] != "guide.md" {
		t.Fatalf("listing=%#v %v", r, err)
	}
	for _, bad := range []string{"../../outside.md", "/etc/hosts", "escape.md", "logo.bin", "missing.md"} {
		if r, err := ReadResource(s, bad); err == nil {
			t.Fatalf("%s was read: %#v", bad, r)
		}
	}
	big := strings.Repeat("é", MaxResourceBytes)
	if err := os.WriteFile(filepath.Join(real, "big.md"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	if r, err := ReadResource(s, "big.md"); err != nil || !r.Truncated || len(r.Content) > MaxResourceBytes {
		t.Fatalf("big: truncated=%v len=%d err=%v", r.Truncated, len(r.Content), err)
	}
}
