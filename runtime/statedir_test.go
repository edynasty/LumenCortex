package runtime

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// physical returns dir with symlinks resolved, as the test's reference spelling.
func physical(t *testing.T, dir string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

func TestStateRootFollowsXDGStateHomeOnlyWhenAbsolute(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_STATE_HOME", xdg)
	got, err := stateRoot()
	if err != nil || got != filepath.Join(xdg, "lumencortex", "workspaces") {
		t.Fatalf("absolute XDG_STATE_HOME: got %q, %v", got, err)
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, bad := range []string{"", "relative/state", "."} {
		t.Setenv("XDG_STATE_HOME", bad)
		got, err := stateRoot()
		want := filepath.Join(home, ".local", "state", "lumencortex", "workspaces")
		if err != nil || got != want {
			t.Errorf("XDG_STATE_HOME=%q: got %q, %v; want %q", bad, got, err, want)
		}
	}
}

func TestWorkspaceKeyShape(t *testing.T) {
	key := workspaceKey("/work/My Project")
	if !regexp.MustCompile(`^My_Project-[0-9a-f]{16}$`).MatchString(key) {
		t.Fatalf("key %q: want <sanitized basename>-<16 hex>", key)
	}
	if workspaceKey("/work/My Project") != key {
		t.Error("key is not deterministic")
	}
	if workspaceKey("/other/My Project") == key {
		t.Error("two directories with one final name share a key")
	}
	for _, name := range []string{"/", "/work/...", "/work/__"} {
		if got := workspaceKey(name); !strings.HasPrefix(got, "workspace-") {
			t.Errorf("%q: key %q has no usable name", name, got)
		}
	}
	long := workspaceKey("/work/" + strings.Repeat("a", 300))
	if len(long) != keyNameLimit+1+16 {
		t.Errorf("long name key has %d bytes, want %d", len(long), keyNameLimit+1+16)
	}
	if strings.ContainsAny(workspaceKey("/work/a:b*c?d"), `:*?/\`) {
		t.Error("key keeps characters that are not safe in a directory name")
	}
}

func TestCanonicalPathAliasesAgree(t *testing.T) {
	root := physical(t, t.TempDir())
	real := filepath.Join(root, "proj")
	if err := os.MkdirAll(filepath.Join(real, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	chain := filepath.Join(root, "chain")
	if err := os.Symlink(link, chain); err != nil {
		t.Fatal(err)
	}
	want, err := canonicalPath(real)
	if err != nil {
		t.Fatal(err)
	}
	if want != real {
		t.Fatalf("canonical of a physical path = %q, want %q", want, real)
	}
	aliases := map[string]string{
		"symlink":        link,
		"symlink chain":  chain,
		"dot dot":        filepath.Join(real, "sub", ".."),
		"through link":   filepath.Join(link, "sub", ".."),
		"trailing slash": real + string(filepath.Separator),
		"dot segments":   filepath.Join(root, ".", "proj", "."),
	}
	for name, alias := range aliases {
		got, err := canonicalPath(alias)
		if err != nil || got != want {
			t.Errorf("%s: canonical(%q) = %q, %v; want %q", name, alias, got, err, want)
		}
	}

	// A relative path is made absolute against the working directory.
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(previous) })
	if got, err := canonicalPath("proj"); err != nil || got != want {
		t.Errorf("relative: got %q, %v; want %q", got, err, want)
	}
}

func TestCanonicalPathFoldsCaseAndNormalization(t *testing.T) {
	root := physical(t, t.TempDir())
	// "café" stored in composed form (NFC); the alias spells it decomposed (NFD).
	composed := "caf\u00e9-Proj"
	real := filepath.Join(root, composed)
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	want, err := canonicalPath(real)
	if err != nil || want != real {
		t.Fatalf("canonical(%q) = %q, %v", real, want, err)
	}

	variants := map[string]string{
		"case":            filepath.Join(root, strings.ToUpper(composed)),
		"decomposed":      filepath.Join(root, "cafe\u0301-Proj"),
		"case+decomposed": filepath.Join(root, "CAFE\u0301-PROJ"),
	}
	exercised := 0
	for name, alias := range variants {
		if _, err := os.Stat(alias); err != nil {
			continue // this file system tells the variant apart from the directory
		}
		exercised++
		got, err := canonicalPath(alias)
		if err != nil || got != want {
			t.Errorf("%s: canonical(%q) = %q, %v; want %q", name, alias, got, err, want)
		}
		if workspaceKey(got) != workspaceKey(want) {
			t.Errorf("%s: alias gets another state key", name)
		}
	}
	if exercised == 0 {
		t.Skip("this file system distinguishes case and Unicode normalization; nothing to fold")
	}
}

func TestCanonicalPathOfMissingDirectoryFails(t *testing.T) {
	if _, err := canonicalPath(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("a workspace that does not exist got a canonical path")
	}
}
