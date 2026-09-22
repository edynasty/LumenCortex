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

	prompt, used, err := registry.Prompt()
	if err != nil {
		t.Fatal(err)
	}
	if len(used) != 1 || !strings.Contains(prompt, "Use layered services") {
		t.Fatalf("prompt=%q used=%#v", prompt, used)
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
	prompt, used, err = registry.Prompt()
	if err != nil {
		t.Fatal(err)
	}
	if prompt != "" || len(used) != 0 {
		t.Fatalf("disabled prompt=%q used=%#v", prompt, used)
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
	prompt, used, err := registry.Prompt()
	if err != nil {
		t.Fatal(err)
	}
	if len(prompt) > MaxPromptBytes+256 {
		t.Fatalf("prompt too large: %d", len(prompt))
	}
	if len(used) == 0 || len(used) > 3 {
		t.Fatalf("used=%#v", used)
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
