package instructions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func byScope(t *testing.T, files []File, scope string) File {
	t.Helper()
	for _, file := range files {
		if file.Scope == scope {
			return file
		}
	}
	t.Fatalf("no %s layer in %#v", scope, files)
	return File{}
}

// The global file is the user's and is read in every workspace; the project file is repository
// content and is read only when the workspace is trusted (ADR-0013).
func TestLoadGatesProjectFileOnTrust(t *testing.T) {
	globalDir, workspace := t.TempDir(), t.TempDir()
	write(t, filepath.Join(globalDir, FileName), "global rules")
	write(t, filepath.Join(workspace, FileName), "project rules")

	trusted := Load(globalDir, workspace, true)
	global, project := byScope(t, trusted, ScopeGlobal), byScope(t, trusted, ScopeProject)
	if !global.Included || global.Text != "global rules" || global.Source != filepath.Join(globalDir, FileName) {
		t.Fatalf("global=%+v", global)
	}
	if !project.Included || project.Text != "project rules" || project.Kind != KindProject {
		t.Fatalf("project=%+v", project)
	}
	if project.Warning != "" || project.Reason != "" {
		t.Fatalf("project=%+v", project)
	}

	untrusted := Load(globalDir, workspace, false)
	global, project = byScope(t, untrusted, ScopeGlobal), byScope(t, untrusted, ScopeProject)
	if !global.Included {
		t.Fatalf("global layer was withheld: %+v", global)
	}
	if project.Included || project.Reason != ReasonUntrusted || project.Text != "" {
		t.Fatalf("project layer=%+v", project)
	}
	if project.Source != filepath.Join(workspace, FileName) {
		t.Fatalf("project source=%q", project.Source)
	}
	if !strings.Contains(project.Warning, "not trusted") {
		t.Fatalf("project warning=%q", project.Warning)
	}
}

// The workspace's AGENTS.md wins; without one, its CLAUDE.md is read; never both.
func TestLoadFallsBackToClaudeMd(t *testing.T) {
	globalDir, workspace := t.TempDir(), t.TempDir()
	write(t, filepath.Join(workspace, FallbackFileName), "claude rules")
	project := byScope(t, Load(globalDir, workspace, true), ScopeProject)
	if !project.Included || project.Text != "claude rules" || project.Source != filepath.Join(workspace, FallbackFileName) {
		t.Fatalf("fallback=%+v", project)
	}

	write(t, filepath.Join(workspace, FileName), "agents rules")
	project = byScope(t, Load(globalDir, workspace, true), ScopeProject)
	if !project.Included || project.Text != "agents rules" {
		t.Fatalf("agents.md did not win: %+v", project)
	}
}

// A project file that is a symlink out of the workspace is refused: an instruction file may not be a
// door to a file outside the workspace.
func TestLoadRefusesSymlinkOutsideWorkspace(t *testing.T) {
	globalDir, workspace, outside := t.TempDir(), t.TempDir(), t.TempDir()
	secret := filepath.Join(outside, FileName)
	write(t, secret, "outside instructions")
	if err := os.Symlink(secret, filepath.Join(workspace, FileName)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	project := byScope(t, Load(globalDir, workspace, true), ScopeProject)
	if project.Included || project.Text != "" {
		t.Fatalf("symlinked file was read: %+v", project)
	}
	if project.Reason != ReasonOutsideWorkspace || project.Source != filepath.Join(workspace, FileName) {
		t.Fatalf("project=%+v", project)
	}
	if project.Warning == "" {
		t.Fatal("a refused instruction file has no warning")
	}
}

// A file larger than the cap contributes its beginning, with a visible warning; the run still gets
// instructions instead of failing.
func TestLoadTruncatesLargeFile(t *testing.T) {
	globalDir, workspace := t.TempDir(), t.TempDir()
	body := strings.Repeat("a", MaxFileBytes+4096)
	write(t, filepath.Join(workspace, FileName), body)

	project := byScope(t, Load(globalDir, workspace, true), ScopeProject)
	if !project.Included || !project.Truncated {
		t.Fatalf("project=%+v", project)
	}
	if len(project.Text) != MaxFileBytes || project.Bytes != MaxFileBytes {
		t.Fatalf("text=%d bytes=%d", len(project.Text), project.Bytes)
	}
	if !strings.Contains(project.Warning, "truncated") {
		t.Fatalf("warning=%q", project.Warning)
	}
}

// A missing file is a layer with a reason, never an error: a workspace without instructions is
// normal.
func TestLoadReportsMissingFiles(t *testing.T) {
	globalDir, workspace := t.TempDir(), t.TempDir()
	files := Load(globalDir, workspace, true)
	if len(files) != 2 {
		t.Fatalf("layers=%#v", files)
	}
	for _, file := range files {
		if file.Included || file.Reason != ReasonMissing || file.Source != "" {
			t.Fatalf("%s layer=%+v", file.Scope, file)
		}
	}
}

// The global file is not confined to the global directory: the user owns it and may point it at a
// dotfiles checkout.
func TestLoadReadsGlobalFileThroughSymlink(t *testing.T) {
	globalDir, workspace, elsewhere := t.TempDir(), t.TempDir(), t.TempDir()
	target := filepath.Join(elsewhere, FileName)
	write(t, target, "dotfiles rules")
	if err := os.Symlink(target, filepath.Join(globalDir, FileName)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	global := byScope(t, Load(globalDir, workspace, false), ScopeGlobal)
	if !global.Included || global.Text != "dotfiles rules" {
		t.Fatalf("global=%+v", global)
	}
}
