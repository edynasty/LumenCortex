package skills

import (
	"strings"
	"testing"
)

func TestLayeredSkillsOverrideDisableAndPrompt(t *testing.T) {
	globalRoot := t.TempDir()
	projectRoot := t.TempDir()
	registry, err := NewRegistry(globalRoot, projectRoot)
	if err != nil {
		t.Fatal(err)
	}

	globalContent := "---\nname: Java Backend\ndescription: Global Java conventions\n---\nUse layered services and tests."
	if err := registry.Save(ScopeGlobal, "java-backend", globalContent); err != nil {
		t.Fatal(err)
	}
	items, err := registry.List(ScopeEffective)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Name != "Java Backend" || !items[0].Enabled || items[0].Scope != ScopeGlobal {
		t.Fatalf("global effective=%#v", items)
	}

	prompt, err := registry.Prompt(PromptOptions{IncludeProject: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(prompt.Included) != 1 || !strings.Contains(prompt.Text, "Use layered services") {
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

func TestSkillPromptIsBounded(t *testing.T) {
	registry, err := NewRegistry(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat("x", 120<<10)
	for _, id := range []string{"a", "b", "c"} {
		if err := registry.Save(ScopeProject, id, "---\nname: "+id+"\n---\n"+body); err != nil {
			t.Fatal(err)
		}
	}
	prompt, err := registry.Prompt(PromptOptions{IncludeProject: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(prompt.Text) > MaxPromptBytes+256 {
		t.Fatalf("prompt too large: %d", len(prompt.Text))
	}
	if len(prompt.Included) == 0 || len(prompt.Included) > 3 {
		t.Fatalf("used=%#v", prompt.Included)
	}
}

func TestSkillRejectsInvalidID(t *testing.T) {
	registry, err := NewRegistry(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Save(ScopeProject, "../escape", "bad"); err == nil {
		t.Fatal("expected invalid id rejection")
	}
}

// A project SKILL.md is repository content: in an untrusted workspace the prompt carries the global
// scope only, and reports the project skills it withheld (ADR-0013).
func TestSkillPromptWithholdsProjectScopeWithoutTrust(t *testing.T) {
	registry, err := NewRegistry(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Save(ScopeGlobal, "shared", "---\nname: Shared\n---\nglobal text"); err != nil {
		t.Fatal(err)
	}
	if err := registry.Save(ScopeProject, "shared", "---\nname: Shared\n---\nproject override text"); err != nil {
		t.Fatal(err)
	}
	if err := registry.Save(ScopeProject, "local", "---\nname: Local\n---\nproject only text"); err != nil {
		t.Fatal(err)
	}

	prompt, err := registry.Prompt(PromptOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt.Text, "global text") || strings.Contains(prompt.Text, "project") {
		t.Fatalf("untrusted prompt=%q", prompt.Text)
	}
	if len(prompt.Included) != 1 || prompt.Included[0].ID != "shared" {
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
