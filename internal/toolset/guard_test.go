package toolset

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The file tools must not reach LumenCortex's state (ADR-0016) by any path, and must not get in the
// way of ordinary workspace files, including the project configuration in .lumencortex.

const stateContent = "STATE-BYTES"

type guardFixture struct {
	ws        string
	legacy    string
	insideExt string // external state placed inside the workspace (workspace = home directory)
	outsideEx string // external state outside the workspace
	set       *Set
	protected map[string]string // protected file -> its content
	n         int
}

func realDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func put(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newGuardFixture(t *testing.T) *guardFixture {
	t.Helper()
	base := realDir(t)
	f := &guardFixture{
		ws:        filepath.Join(base, "ws"),
		insideExt: filepath.Join(base, "ws", "home", ".local", "state", "lumencortex", "workspaces", "proj-1"),
		outsideEx: filepath.Join(base, "outside-state", "proj-2"),
		protected: map[string]string{},
	}
	f.legacy = filepath.Join(f.ws, ".lumencortex")
	for _, rel := range []string{"lumencortex.db", "lumencortex.db-wal", "lumencortex.db-shm", "db.lock", "bind.lock",
		"checkpoint.pending", "runs/s1.lock", "backups/b.sql", "legacy-state-123/lumencortex.db"} {
		f.protected[filepath.Join(f.legacy, filepath.FromSlash(rel))] = stateContent
	}
	for _, rel := range []string{"lumencortex.db", "db.lock", "runs/s1.lock", "backups/old.sql"} {
		f.protected[filepath.Join(f.insideExt, filepath.FromSlash(rel))] = stateContent
		f.protected[filepath.Join(f.outsideEx, filepath.FromSlash(rel))] = stateContent
	}
	for path, content := range f.protected {
		put(t, path, content)
	}
	put(t, filepath.Join(f.ws, "a.txt"), "ordinary")
	set, err := New(Options{Workspace: f.ws, Policy: PolicyFull, Protected: Protected{
		Trees:  []string{f.insideExt, f.outsideEx},
		Legacy: f.legacy,
	}})
	if err != nil {
		t.Fatal(err)
	}
	f.set = set
	return f
}

func (f *guardFixture) call(tool string, args map[string]any) error {
	_, err := f.set.Execute(context.Background(), tool, args, nil)
	return err
}

// mutations are the ways a file tool can change a path that holds stateContent.
func (f *guardFixture) mutations(path string) map[string]func() error {
	return map[string]func() error{
		"write_file": func() error { return f.call("write_file", map[string]any{"path": path, "content": "overwritten"}) },
		"replace_in_file": func() error {
			return f.call("replace_in_file", map[string]any{"path": path, "old_text": stateContent, "new_text": "overwritten"})
		},
		"apply_patch update": func() error {
			return f.call("apply_patch", map[string]any{"patches": []any{map[string]any{
				"path": path, "operation": "update", "edits": []any{map[string]any{"old_text": stateContent, "new_text": "overwritten"}}}}})
		},
		"apply_patch delete": func() error {
			return f.call("apply_patch", map[string]any{"patches": []any{map[string]any{"path": path, "operation": "delete"}}})
		},
		"read_file": func() error { return f.call("read_file", map[string]any{"path": path}) },
	}
}

func (f *guardFixture) assertStateIntact(t *testing.T, when string) {
	t.Helper()
	for path, want := range f.protected {
		raw, err := os.ReadFile(path)
		if err != nil || string(raw) != want {
			t.Fatalf("%s: protected file %s is now %q (err %v), want %q", when, path, raw, err, want)
		}
	}
}

func isProtected(err error) bool { return errors.Is(err, ErrProtectedPath) }

// spellings returns every way the test can name a protected file from inside the workspace.
func (f *guardFixture) spellings(t *testing.T, target string) map[string]string {
	t.Helper()
	f.n++
	rel, err := filepath.Rel(f.ws, target)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{"absolute": target}
	if !strings.HasPrefix(rel, "..") {
		out["relative"] = rel
		out["dot-dot"] = filepath.Join("sub", "..", rel)
		out["dot segments"] = "./" + filepath.ToSlash(rel)
	}
	hard := filepath.Join(f.ws, fmt.Sprintf("hard-%d.dat", f.n))
	if err := os.Link(target, hard); err != nil {
		t.Fatalf("hard link: %v", err)
	}
	out["hard link"] = hard
	if runtime.GOOS != "windows" {
		soft := filepath.Join(f.ws, fmt.Sprintf("soft-%d.dat", f.n))
		if err := os.Symlink(target, soft); err != nil {
			t.Fatal(err)
		}
		out["symlink to file"] = soft
		dir := filepath.Join(f.ws, fmt.Sprintf("dirlink-%d", f.n))
		if err := os.Symlink(filepath.Dir(target), dir); err != nil {
			t.Fatal(err)
		}
		out["symlink to directory"] = filepath.Join(dir, filepath.Base(target))
		chain := filepath.Join(f.ws, fmt.Sprintf("chain-%d", f.n))
		if err := os.Symlink(soft, chain); err != nil {
			t.Fatal(err)
		}
		out["symlink chain"] = chain
	}
	return out
}

// Every state file inside or reachable from the workspace is refused by every file tool under every
// spelling, and is byte-for-byte what it was afterwards. A path outside the workspace is refused
// already by the workspace boundary, so its absolute and relative spellings may fail for that reason.
func TestFileToolsRefuseEveryRouteToState(t *testing.T) {
	f := newGuardFixture(t)
	var targets []string
	for path := range f.protected {
		targets = append(targets, path)
	}
	for _, target := range targets {
		outside := !within(f.ws, target)
		for spelling, path := range f.spellings(t, target) {
			for tool, run := range f.mutations(path) {
				err := run()
				switch {
				case err == nil:
					t.Errorf("%s via %s (%s) succeeded on %s", tool, spelling, path, target)
				case outside && (spelling == "absolute" || strings.HasPrefix(spelling, "symlink")):
					// stopped by the workspace boundary before the guard
				case !isProtected(err):
					t.Errorf("%s via %s (%s) on %s: %v, want ErrProtectedPath", tool, spelling, path, target, err)
				}
			}
		}
	}
	f.assertStateIntact(t, "after the attempts")
}

func TestListDirRefusesStateDirectories(t *testing.T) {
	f := newGuardFixture(t)
	for _, dir := range []string{
		filepath.Join(f.legacy, "runs"), filepath.Join(f.legacy, "backups"), filepath.Join(f.legacy, "legacy-state-123"),
		f.insideExt, filepath.Join(f.insideExt, "runs"),
	} {
		for spelling, path := range map[string]string{"absolute": dir, "relative": mustRel(t, f.ws, dir)} {
			if err := f.call("list_dir", map[string]any{"path": path}); !isProtected(err) {
				t.Errorf("list_dir %s (%s): %v, want ErrProtectedPath", dir, spelling, err)
			}
		}
	}
	if runtime.GOOS != "windows" {
		link := filepath.Join(f.ws, "dirlink")
		if err := os.Symlink(f.insideExt, link); err != nil {
			t.Fatal(err)
		}
		if err := f.call("list_dir", map[string]any{"path": "dirlink"}); !isProtected(err) {
			t.Errorf("list_dir through a symlink: %v, want ErrProtectedPath", err)
		}
	}
}

func mustRel(t *testing.T, base, target string) string {
	t.Helper()
	rel, err := filepath.Rel(base, target)
	if err != nil {
		t.Fatal(err)
	}
	return rel
}

// A file that does not exist yet cannot be created under a protected name or in a protected
// directory, through any directory route to it.
func TestFileToolsCannotCreateInsideState(t *testing.T) {
	f := newGuardFixture(t)
	missing := []string{
		filepath.Join(f.legacy, "lumencortex.db-journal"),
		filepath.Join(f.legacy, "Lumencortex.DB"),
		filepath.Join(f.legacy, "runs", "new.lock"),
		filepath.Join(f.legacy, "backups", "deep", "new.sql"),
		filepath.Join(f.legacy, "legacy-state-999", "lumencortex.db"),
		filepath.Join(f.insideExt, "lumencortex.db-wal"),
		filepath.Join(f.insideExt, "new", "deep", "file.txt"),
	}
	for _, target := range missing {
		spellings := map[string]string{"absolute": target, "relative": mustRel(t, f.ws, target), "dot-dot": filepath.Join("sub", "..", mustRel(t, f.ws, target))}
		if _, err := os.Stat(filepath.Dir(target)); err == nil && runtime.GOOS != "windows" {
			dir := filepath.Join(f.ws, "via-"+filepath.Base(filepath.Dir(target)))
			os.Remove(dir)
			if err := os.Symlink(filepath.Dir(target), dir); err == nil {
				spellings["symlinked parent"] = filepath.Join(dir, filepath.Base(target))
			}
		}
		for spelling, path := range spellings {
			if err := f.call("write_file", map[string]any{"path": path, "content": "x"}); !isProtected(err) {
				t.Errorf("write_file %s via %s: %v, want ErrProtectedPath", target, spelling, err)
			}
			err := f.call("apply_patch", map[string]any{"patches": []any{map[string]any{"path": path, "operation": "create", "content": "x"}}})
			if !isProtected(err) {
				t.Errorf("apply_patch create %s via %s: %v, want ErrProtectedPath", target, spelling, err)
			}
			// (A spelling that differs only in case may name a state file that already exists.)
			if _, statErr := os.Lstat(target); statErr == nil && target == strings.ToLower(target) {
				t.Fatalf("%s was created via %s", target, spelling)
			}
		}
	}
	f.assertStateIntact(t, "after the attempts")
}

// Names that merely look like state, project configuration beside the database, and hard links
// between ordinary files all keep working.
func TestOrdinaryWorkspaceFilesAreNotAffected(t *testing.T) {
	f := newGuardFixture(t)
	ordinary := []string{
		"a.txt",
		"other/lumencortex.db",
		"runs/data.txt",
		"backups/data.txt",
		".lumencortex/mcp.json",
		".lumencortex/cognition.json",
		".lumencortex/skills/s/SKILL.md",
		".lumencortex/notes.md",
		"home/.local/state/other-tool/state.db",
		"home/notes.txt",
	}
	for _, rel := range ordinary {
		path := filepath.Join(f.ws, filepath.FromSlash(rel))
		if err := f.call("write_file", map[string]any{"path": rel, "content": "first"}); err != nil {
			t.Errorf("write_file %s: %v", rel, err)
		}
		if err := f.call("replace_in_file", map[string]any{"path": rel, "old_text": "first", "new_text": "second"}); err != nil {
			t.Errorf("replace_in_file %s: %v", rel, err)
		}
		if err := f.call("apply_patch", map[string]any{"patches": []any{map[string]any{
			"path": rel, "operation": "update", "edits": []any{map[string]any{"old_text": "second", "new_text": "third"}}}}}); err != nil {
			t.Errorf("apply_patch update %s: %v", rel, err)
		}
		if raw, err := os.ReadFile(path); err != nil || string(raw) != "third" {
			t.Errorf("%s = %q, %v", rel, raw, err)
		}
		if err := f.call("read_file", map[string]any{"path": rel}); err != nil {
			t.Errorf("read_file %s: %v", rel, err)
		}
	}
	if err := f.call("list_dir", map[string]any{"path": ".lumencortex"}); err != nil {
		t.Errorf("list_dir .lumencortex: %v", err)
	}
	if err := f.call("list_dir", map[string]any{"path": "."}); err != nil {
		t.Errorf("list_dir .: %v", err)
	}
	if err := os.Link(filepath.Join(f.ws, "a.txt"), filepath.Join(f.ws, "a-hard.txt")); err == nil {
		if err := f.call("write_file", map[string]any{"path": "a-hard.txt", "content": "via the link"}); err != nil {
			t.Errorf("write through a hard link between ordinary files: %v", err)
		}
	}
	if err := f.call("apply_patch", map[string]any{"patches": []any{map[string]any{"path": "other/lumencortex.db", "operation": "delete"}}}); err != nil {
		t.Errorf("deleting an ordinary file named lumencortex.db: %v", err)
	}
	f.assertStateIntact(t, "after ordinary edits")
}

// The path is valid when resolved and is swapped for state before the open: the check on the open
// descriptor refuses it, and a refused write leaves the state untouched.
func TestPathSwappedAfterResolveIsRefused(t *testing.T) {
	f := newGuardFixture(t)
	state := filepath.Join(f.legacy, "lumencortex.db")
	swaps := map[string]func(victim string){
		"hard link": func(victim string) {
			os.Remove(victim)
			if err := os.Link(state, victim); err != nil {
				t.Error(err)
			}
		},
	}
	if runtime.GOOS != "windows" {
		swaps["symlink"] = func(victim string) {
			os.Remove(victim)
			if err := os.Symlink(state, victim); err != nil {
				t.Error(err)
			}
		}
	}
	for name, swap := range swaps {
		for tool, run := range map[string]func(path string) error{
			"read_file":  func(p string) error { return f.call("read_file", map[string]any{"path": p}) },
			"write_file": func(p string) error { return f.call("write_file", map[string]any{"path": p, "content": "overwritten"}) },
			"replace_in_file": func(p string) error {
				return f.call("replace_in_file", map[string]any{"path": p, "old_text": "ordinary", "new_text": "x"})
			},
			"apply_patch": func(p string) error {
				return f.call("apply_patch", map[string]any{"patches": []any{map[string]any{
					"path": p, "operation": "update", "edits": []any{map[string]any{"old_text": "ordinary", "new_text": "x"}}}}})
			},
		} {
			victim := filepath.Join(f.ws, "victim.txt")
			put(t, victim, "ordinary")
			swapped := false
			testHookBeforeOpen = func(path string) {
				if !swapped && path == victim {
					swapped = true
					swap(victim)
				}
			}
			err := run("victim.txt")
			testHookBeforeOpen = nil
			if !swapped {
				t.Fatalf("%s/%s: the hook never ran", name, tool)
			}
			if !isProtected(err) {
				t.Errorf("%s swap, %s: %v, want ErrProtectedPath", name, tool, err)
			}
			f.assertStateIntact(t, name+" swap, "+tool)
			os.Remove(victim)
		}
	}
}

// A case or Unicode-normalization spelling that the file system folds to the same directory is
// stopped by file identity, which names cannot do.
func TestFoldedSpellingOfStateIsRefused(t *testing.T) {
	f := newGuardFixture(t)
	variant := filepath.Join(f.ws, ".LUMENCORTEX", "LUMENCORTEX.DB")
	if _, err := os.Stat(variant); err != nil {
		t.Skip("this file system distinguishes case")
	}
	for tool, run := range f.mutations(variant) {
		if err := run(); !isProtected(err) {
			t.Errorf("%s via a case variant: %v, want ErrProtectedPath", tool, err)
		}
	}
	f.assertStateIntact(t, "after the case variants")
}

// Without a Protected value the guard is off and the tools behave as before.
func TestZeroProtectedChangesNothing(t *testing.T) {
	root := realDir(t)
	set, err := New(Options{Workspace: root, Policy: PolicyFull})
	if err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(root, ".lumencortex", "lumencortex.db"), "x")
	if _, err := set.Execute(context.Background(), "write_file", map[string]any{"path": ".lumencortex/lumencortex.db", "content": "y"}, nil); err != nil {
		t.Fatalf("write without a guard: %v", err)
	}
}
