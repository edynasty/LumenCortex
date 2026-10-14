package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

// F9, M2.2: the Engine reads and writes the global mcp.json and skills/ under config.GlobalDir
// only. Copies in the directories earlier builds used are neither read nor touched.
func TestEngineUsesGlobalDirForMCPAndSkills(t *testing.T) {
	home := t.TempDir()
	xdg := filepath.Join(home, "xdg")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("AppData", filepath.Join(home, "AppData"))
	t.Setenv("XDG_CONFIG_HOME", xdg)
	globalDir := filepath.Join(xdg, "lumencortex")

	// Legacy copies: Desktop's ~/.config/lumencortex and core's os.UserConfigDir()/lumencortex.
	legacyDirs := []string{filepath.Join(home, ".config", "lumencortex")}
	if d, err := os.UserConfigDir(); err == nil {
		if c := filepath.Join(d, "lumencortex"); c != globalDir && c != legacyDirs[0] {
			legacyDirs = append(legacyDirs, c)
		}
	}
	legacyMCP := `{"servers":{"legacy":{"command":"/bin/true"}}}`
	for _, d := range legacyDirs {
		for p, c := range map[string]string{
			filepath.Join(d, "mcp.json"):                  legacyMCP,
			filepath.Join(d, "skills", "old", "SKILL.md"): "# old\nlegacy skill",
		} {
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	engine, err := Open(Options{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()

	if cfgs, err := engine.MCPConfigsScope("global"); err != nil || len(cfgs) != 0 {
		t.Fatalf("legacy mcp.json was read: %v %#v", err, cfgs)
	}
	if items, err := engine.Skills("global"); err != nil || len(items) != 0 {
		t.Fatalf("legacy skills were read: %v %#v", err, items)
	}

	if err := engine.MCPUpsertConfigScope("global", MCPConfig{ID: "new", Name: "New", Command: "/bin/true"}); err != nil {
		t.Fatal(err)
	}
	if err := engine.SaveSkill("global", "fresh", "# fresh\nnew skill"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(globalDir, "mcp.json"), filepath.Join(globalDir, "skills", "fresh", "SKILL.md")} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("global write did not land in GlobalDir: %v", err)
		}
	}
	// The legacy files are byte-identical afterwards.
	for _, d := range legacyDirs {
		if b, err := os.ReadFile(filepath.Join(d, "mcp.json")); err != nil || string(b) != legacyMCP {
			t.Errorf("%s/mcp.json changed: %v", d, err)
		}
		if _, err := os.Stat(filepath.Join(d, "skills", "fresh")); err == nil {
			t.Errorf("new skill written into legacy %s", d)
		}
	}
}
