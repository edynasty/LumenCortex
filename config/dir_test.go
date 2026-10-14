package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestGlobalDir(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	xdg := filepath.Join(root, "xdg")
	t.Setenv("XDG_CONFIG_HOME", xdg)
	if dir, err := GlobalDir(); err != nil || dir != filepath.Join(xdg, "lumencortex") {
		t.Fatalf("absolute XDG: %q %v", dir, err)
	}
	// A relative or empty XDG_CONFIG_HOME is ignored, on every OS.
	for _, v := range []string{"relative/dir", "", "."} {
		t.Setenv("XDG_CONFIG_HOME", v)
		if dir, err := GlobalDir(); err != nil || dir != filepath.Join(home, ".config", "lumencortex") {
			t.Fatalf("XDG_CONFIG_HOME=%q: %q %v", v, dir, err)
		}
	}
}

// The directory is not created by GlobalDir, and a symlinked XDG root is read through.
func TestGlobalDirSymlinkAndMissing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows")
	}
	root := canon(t, t.TempDir())
	real := filepath.Join(root, "real")
	link := filepath.Join(root, "link")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_CONFIG_HOME", link)

	dir, err := GlobalDir()
	if err != nil || dir != filepath.Join(link, "lumencortex") {
		t.Fatalf("dir=%q err=%v", dir, err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("GlobalDir created %s", dir)
	}
	// Missing directory and files: an empty configuration, no warnings.
	c, err := Load(root)
	if err != nil || len(c.Effective().Warnings) != 0 {
		t.Fatalf("missing: %v %+v", err, c)
	}
	// The file written through the link is the one Load reads.
	writeFile(t, filepath.Join(real, "lumencortex", FileName), globalJSON)
	c, err = Load(root)
	if err != nil || c.Effective().Model != "deepseek/chat" {
		t.Fatalf("through link: %v", err)
	}
}

// F9: files in directories earlier builds used are reported with their replacement path, never
// read as active configuration, never moved.
func TestLegacyGlobalFilesAreReportedNotReadNorMoved(t *testing.T) {
	home, globalDir := env(t)
	legacyConfig := filepath.Join(home, ".config", "lumencortex") // Desktop's directory
	legacyFiles := map[string]string{
		filepath.Join(legacyConfig, FileName):                  `{"model": "legacy/m", "providers": {"legacy": {"models": {"m": {}}}}}`,
		filepath.Join(legacyConfig, MCPFileName):               `{"servers": {}}`,
		filepath.Join(legacyConfig, "skills", "s", "SKILL.md"): "# legacy skill\n",
	}
	for p, c := range legacyFiles {
		writeFile(t, p, c)
	}
	var appSupport string // core's os.UserConfigDir()/lumencortex when it differs from both
	if d, err := os.UserConfigDir(); err == nil {
		if cand := filepath.Join(d, "lumencortex"); cand != globalDir && cand != legacyConfig {
			appSupport = cand
			writeFile(t, filepath.Join(cand, MCPFileName), `{"servers": {}}`)
		}
	}

	c, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := c.Effective()
	if e.Model != "" || len(c.Catalog()) != 0 {
		t.Fatalf("a legacy lumencortex.json was read: model=%q catalog=%d", e.Model, len(c.Catalog()))
	}
	got := map[string]Warning{}
	for _, w := range e.Warnings {
		if w.Code != WarnLegacyPath {
			t.Errorf("unexpected warning %+v", w)
		}
		got[w.Path] = w
	}
	want := map[string]string{
		filepath.Join(legacyConfig, FileName):    filepath.Join(globalDir, FileName),
		filepath.Join(legacyConfig, MCPFileName): filepath.Join(globalDir, MCPFileName),
		filepath.Join(legacyConfig, "skills"):    filepath.Join(globalDir, "skills"),
	}
	if appSupport != "" {
		want[filepath.Join(appSupport, MCPFileName)] = filepath.Join(globalDir, MCPFileName)
	}
	if len(got) != len(want) {
		t.Fatalf("warnings=%+v want %v", got, want)
	}
	for old, target := range want {
		w, ok := got[old]
		if !ok || w.MoveTo != target || w.Message == "" {
			t.Errorf("%s: warning=%+v want MoveTo %s", old, w, target)
		}
	}
	// Nothing moved, nothing created in the new location, nothing rewritten.
	for p, content := range legacyFiles {
		if b, err := os.ReadFile(p); err != nil || string(b) != content {
			t.Errorf("legacy file %s changed: %v", p, err)
		}
	}
	if _, err := os.Stat(globalDir); !os.IsNotExist(err) {
		t.Errorf("Load created %s", globalDir)
	}
}

func TestNoLegacyWarningWhenGlobalDirIsTheOldDirectory(t *testing.T) {
	root := canon(t, t.TempDir())
	t.Setenv("HOME", root)
	t.Setenv("USERPROFILE", root)
	t.Setenv("XDG_CONFIG_HOME", "") // GlobalDir = ~/.config/lumencortex
	old := filepath.Join(root, ".config", "lumencortex")
	writeFile(t, filepath.Join(old, FileName), globalJSON)
	writeFile(t, filepath.Join(old, MCPFileName), `{"servers": {}}`)
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range c.Effective().Warnings {
		if w.Code == WarnLegacyPath && filepath.Dir(w.Path) == old {
			t.Errorf("file in GlobalDir reported as legacy: %+v", w)
		}
	}
	if c.Effective().Model != "deepseek/chat" {
		t.Fatalf("global file in ~/.config/lumencortex not read")
	}
}

// A legacy directory reached through a symlink to GlobalDir is the same directory.
func TestLegacySymlinkToGlobalDirIsNotReported(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows")
	}
	home, globalDir := env(t)
	writeFile(t, filepath.Join(globalDir, MCPFileName), `{"servers": {}}`)
	if err := os.MkdirAll(filepath.Join(home, ".config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(globalDir, filepath.Join(home, ".config", "lumencortex")); err != nil {
		t.Fatal(err)
	}
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range c.Effective().Warnings {
		if w.Code == WarnLegacyPath && filepath.Base(filepath.Dir(w.Path)) == "lumencortex" && w.Path != "" {
			if sameDir(filepath.Dir(w.Path), globalDir) {
				t.Errorf("symlink to GlobalDir reported: %+v", w)
			}
		}
	}
}
