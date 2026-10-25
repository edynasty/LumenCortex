package runtime

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/edynasty/LumenCortex/internal/migrate"
	"github.com/edynasty/LumenCortex/internal/runlock"
	"github.com/edynasty/LumenCortex/protocol"
)

// ADR-0016 at the Engine: where state lives, what it is bound to, and what is refused. Every test
// runs against the private home and XDG_STATE_HOME that TestMain installs.

func newWorkspace(t *testing.T) string {
	t.Helper()
	return physical(t, t.TempDir())
}

// isolatedState gives the test an empty state root, so the state directories it counts are its own.
func isolatedState(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", physical(t, t.TempDir()))
}

func openWorkspace(t *testing.T, workspace string) *Engine {
	t.Helper()
	engine, err := Open(Options{Workspace: workspace})
	if err != nil {
		t.Fatalf("Open(%s): %v", workspace, err)
	}
	t.Cleanup(func() { engine.Close() })
	return engine
}

func sessionGoals(t *testing.T, engine *Engine) []string {
	t.Helper()
	infos, err := engine.ListSessions(context.Background(), 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	var goals []string
	for _, info := range infos {
		goals = append(goals, info.Goal)
	}
	return goals
}

// createdState opens workspace with an Engine, records one session, closes it, and returns the
// state directory the Engine chose.
func createdState(t *testing.T, workspace, goal string) string {
	t.Helper()
	engine, err := Open(Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.NewSession(context.Background(), SessionOptions{Goal: goal}); err != nil {
		t.Fatal(err)
	}
	dir := engine.StateInfo().Path
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

// legacyState writes what an earlier build left: <workspace>/.lumencortex/lumencortex.db with one
// session, a schema before 6 and no workspace binding. It opens through RepoDir, downgrades the
// database to schema 5 (the last one an earlier build wrote) and removes the binding row.
func legacyState(t *testing.T, workspace, goal string) string {
	t.Helper()
	dir := filepath.Join(workspace, ".lumencortex")
	engine, err := Open(Options{Workspace: workspace, RepoDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.NewSession(context.Background(), SessionOptions{Goal: goal}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	set, err := migrate.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := migrate.Downgrade(context.Background(), dir, set, 5); err != nil {
		t.Fatal(err)
	}
	execSQL(t, dir, `DELETE FROM metadata WHERE key = 'workspace_binding'`)
	return dir
}

func execSQL(t *testing.T, stateDir, statement string) {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+filepath.ToSlash(filepath.Join(stateDir, "lumencortex.db")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(statement); err != nil {
		t.Fatal(err)
	}
}

func queryString(t *testing.T, stateDir, query string) string {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+filepath.ToSlash(filepath.Join(stateDir, "lumencortex.db")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var out sql.NullString
	if err := db.QueryRow(query).Scan(&out); err != nil {
		t.Fatal(err)
	}
	return out.String
}

func boundPath(t *testing.T, stateDir string) string {
	t.Helper()
	var record struct {
		CanonicalPath string `json:"canonicalPath"`
	}
	raw := queryString(t, stateDir, `SELECT value FROM metadata WHERE key = 'workspace_binding'`)
	if err := json.Unmarshal([]byte(raw), &record); err != nil {
		t.Fatalf("binding %q: %v", raw, err)
	}
	return record.CanonicalPath
}

// fingerprint hashes every regular file under dir (names and bytes), so a refused Open is shown to
// leave a legacy directory exactly as it was. Lock files are excluded: they hold a pid.
func fingerprint(t *testing.T, dir string) [32]byte {
	t.Helper()
	h := sha256.New()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(path, ".lock") {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		h.Write([]byte(rel))
		h.Write(raw)
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return [32]byte(h.Sum(nil))
}

func stateEntries(t *testing.T) []string {
	t.Helper()
	root, err := stateRoot()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// A new workspace keeps its state outside it, in the directory its canonical path names, and the
// workspace gains no database and not even a state directory.
func TestNewWorkspaceStateIsExternal(t *testing.T) {
	isolatedState(t)
	workspace := newWorkspace(t)
	engine := openWorkspace(t, workspace)
	info := engine.StateInfo()

	root, _ := stateRoot()
	canonical, _ := canonicalPath(workspace)
	if info.Mode != StateExternal || info.Path != filepath.Join(root, workspaceKey(canonical)) || info.Workspace != canonical {
		t.Fatalf("StateInfo = %+v, want external state %s bound to %s", info, filepath.Join(root, workspaceKey(canonical)), canonical)
	}
	if _, err := os.Stat(filepath.Join(info.Path, "lumencortex.db")); err != nil {
		t.Fatalf("external database missing: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(workspace, ".lumencortex")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the workspace gained .lumencortex (lstat: %v)", err)
	}
	if got := boundPath(t, info.Path); got != canonical {
		t.Errorf("binding = %q, want %q", got, canonical)
	}
	if engine.Health().Database != filepath.Join(info.Path, "lumencortex.db") {
		t.Errorf("Health().Database = %q", engine.Health().Database)
	}
}

// Every spelling of one directory is one workspace and one state; two directories never share one.
func TestEquivalentWorkspacePathsShareOneState(t *testing.T) {
	isolatedState(t)
	root := newWorkspace(t)
	real := filepath.Join(root, "proj")
	if err := os.MkdirAll(filepath.Join(real, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	dir := createdState(t, real, "shared history")

	aliases := []string{link, filepath.Join(real, "sub", ".."), real + string(filepath.Separator), filepath.Join(link, "sub", "..")}
	if upper := filepath.Join(root, "PROJ"); sameDirectory(upper, real) {
		aliases = append(aliases, upper)
	}
	for _, alias := range aliases {
		engine, err := Open(Options{Workspace: alias})
		if err != nil {
			t.Fatalf("Open(%s): %v", alias, err)
		}
		if got := engine.StateInfo().Path; got != dir {
			t.Errorf("Open(%s) uses state %s, want %s", alias, got, dir)
		}
		if goals := sessionGoals(t, engine); len(goals) != 1 || goals[0] != "shared history" {
			t.Errorf("Open(%s) sees sessions %v", alias, goals)
		}
		engine.Close()
	}
	if entries := stateEntries(t); len(entries) != 1 {
		t.Errorf("aliases produced %d state directories: %v", len(entries), entries)
	}
}

func TestDifferentWorkspacesDoNotShareState(t *testing.T) {
	isolatedState(t)
	a, b := newWorkspace(t), newWorkspace(t)
	dirA := createdState(t, a, "only in a")
	engine := openWorkspace(t, b)
	if engine.StateInfo().Path == dirA {
		t.Fatal("two workspaces share a state directory")
	}
	if goals := sessionGoals(t, engine); len(goals) != 0 {
		t.Errorf("workspace b sees %v", goals)
	}
}

// A database left by an earlier build opens in place, nothing is moved, and no external state is
// created beside it. The binding it never had is adopted.
func TestLegacyStateOpensInPlace(t *testing.T) {
	isolatedState(t)
	workspace := newWorkspace(t)
	legacy := legacyState(t, workspace, "old history")
	if queryString(t, legacy, `SELECT count(*) FROM metadata WHERE key = 'workspace_binding'`) != "0" {
		t.Fatal("fixture: legacy database already has a binding")
	}
	entriesBefore := stateEntries(t)

	engine := openWorkspace(t, workspace)
	info := engine.StateInfo()
	if info.Mode != StateLegacy || info.Path != legacy {
		t.Fatalf("StateInfo = %+v, want legacy at %s", info, legacy)
	}
	if goals := sessionGoals(t, engine); len(goals) != 1 || goals[0] != "old history" {
		t.Fatalf("legacy sessions = %v", goals)
	}
	canonical, _ := canonicalPath(workspace)
	if got := boundPath(t, legacy); got != canonical {
		t.Errorf("legacy binding = %q, want %q", got, canonical)
	}
	if after := stateEntries(t); len(after) != len(entriesBefore) {
		t.Errorf("opening a legacy workspace created external state: %v -> %v", entriesBefore, after)
	}
	// The legacy directory is still where the Engine keeps its run locks.
	if _, err := os.Stat(filepath.Join(legacy, "db.lock")); err != nil {
		t.Errorf("db.lock not in the legacy directory: %v", err)
	}
}

// When both exist the external state wins, the legacy files are untouched, and the residue is reported.
func TestExternalStateWinsOverLegacyResidue(t *testing.T) {
	workspace := newWorkspace(t)
	external := createdState(t, workspace, "in the external state")
	legacy := legacyState(t, workspace, "in the legacy state")
	before := fingerprint(t, legacy)

	engine := openWorkspace(t, workspace)
	info := engine.StateInfo()
	if info.Mode != StateExternal || info.Path != external || info.LegacyResidue != legacy {
		t.Fatalf("StateInfo = %+v, want external %s with residue %s", info, external, legacy)
	}
	if goals := sessionGoals(t, engine); len(goals) != 1 || goals[0] != "in the external state" {
		t.Errorf("sessions = %v, want only the external one", goals)
	}
	engine.Close()
	if fingerprint(t, legacy) != before {
		t.Error("the legacy files changed while the external state was in use")
	}
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
}

// A legacy database that Git tracks came with a clone. It is refused, not adopted as history, and
// no empty external state is created in its place.
func TestGitTrackedLegacyDatabaseIsRefused(t *testing.T) {
	isolatedState(t)
	requireGit(t)
	workspace := newWorkspace(t)
	legacy := legacyState(t, workspace, "carried by the repository")
	initGitRepo(t, workspace)
	gitIn(t, workspace, "add", "-f", ".lumencortex/lumencortex.db")
	before := fingerprint(t, legacy)

	_, err := Open(Options{Workspace: workspace})
	if !errors.Is(err, ErrRepositoryCarriedState) {
		t.Fatalf("Open = %v, want ErrRepositoryCarriedState", err)
	}
	if entries := stateEntries(t); len(entries) != 0 {
		t.Errorf("a refused Open created state: %v", entries)
	}
	if fingerprint(t, legacy) != before {
		t.Error("the refused Open changed the legacy files")
	}

	// The same database, untracked, is ordinary legacy state.
	gitIn(t, workspace, "rm", "--cached", "-q", ".lumencortex/lumencortex.db")
	engine := openWorkspace(t, workspace)
	if engine.StateInfo().Mode != StateLegacy {
		t.Errorf("untracked legacy database: %+v", engine.StateInfo())
	}
}

// Git failing inside a repository is not "untracked": the legacy database is refused, not adopted.
func TestGitProbeFailureRefusesLegacyDatabase(t *testing.T) {
	isolatedState(t)
	requireGit(t)
	workspace := newWorkspace(t)
	legacy := legacyState(t, workspace, "history")
	if err := os.Mkdir(filepath.Join(workspace, ".git"), 0o755); err != nil { // a repository git cannot read
		t.Fatal(err)
	}
	before := fingerprint(t, legacy)

	_, err := Open(Options{Workspace: workspace})
	if err == nil || errors.Is(err, ErrRepositoryCarriedState) || !strings.Contains(err.Error(), "tracked by Git") {
		t.Fatalf("Open = %v, want a probe failure", err)
	}
	if entries := stateEntries(t); len(entries) != 0 {
		t.Errorf("a refused Open created state: %v", entries)
	}
	if fingerprint(t, legacy) != before {
		t.Error("the refused Open changed the legacy files")
	}
}

func copyTree(t *testing.T, from, to string) {
	t.Helper()
	if err := os.MkdirAll(to, 0o755); err != nil {
		t.Fatal(err)
	}
	err := filepath.WalkDir(from, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(path, ".lock") {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, path)
		return os.WriteFile(filepath.Join(to, rel), raw, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A database moved or copied to another workspace is refused. It is not opened, not rebound, and no
// empty state replaces it.
func TestStateBoundToAnotherWorkspaceIsRefused(t *testing.T) {
	isolatedState(t)
	a, b := newWorkspace(t), newWorkspace(t)
	dirA := createdState(t, a, "belongs to a")
	canonicalA, _ := canonicalPath(a)

	// External: a's state planted where b's would be.
	canonicalB, _ := canonicalPath(b)
	dirB, err := externalStateDir(canonicalB)
	if err != nil {
		t.Fatal(err)
	}
	copyTree(t, dirA, dirB)
	if _, err := Open(Options{Workspace: b}); !errors.Is(err, ErrStateBindingMismatch) {
		t.Fatalf("external: Open = %v, want ErrStateBindingMismatch", err)
	}
	if got := boundPath(t, dirB); got != canonicalA {
		t.Errorf("the refused Open rebound the copy to %q", got)
	}
	if got := queryString(t, dirB, `SELECT goal FROM sessions`); got != "belongs to a" {
		t.Errorf("the copy's history changed: %q", got)
	}
	if entries := stateEntries(t); len(entries) != 2 {
		t.Errorf("state directories = %v, want only a's and the planted copy", entries)
	}

	// Legacy: a's database copied into b/.lumencortex.
	c := newWorkspace(t)
	copyTree(t, dirA, filepath.Join(c, ".lumencortex"))
	if _, err := Open(Options{Workspace: c}); !errors.Is(err, ErrStateBindingMismatch) {
		t.Fatalf("legacy: Open = %v, want ErrStateBindingMismatch", err)
	}
	if got := boundPath(t, filepath.Join(c, ".lumencortex")); got != canonicalA {
		t.Errorf("the refused legacy Open rebound the copy to %q", got)
	}

	// An explicit RepoDir is checked too.
	if _, err := Open(Options{Workspace: c, RepoDir: dirA}); !errors.Is(err, ErrStateBindingMismatch) {
		t.Fatalf("RepoDir: Open = %v, want ErrStateBindingMismatch", err)
	}
}

func TestUnreadableBindingIsRefusedNotReplaced(t *testing.T) {
	workspace := newWorkspace(t)
	dir := createdState(t, workspace, "history")
	execSQL(t, dir, `UPDATE metadata SET value = 'not json' WHERE key = 'workspace_binding'`)
	if _, err := Open(Options{Workspace: workspace}); !errors.Is(err, ErrStateBindingMismatch) {
		t.Fatalf("Open = %v, want ErrStateBindingMismatch", err)
	}
	if got := queryString(t, dir, `SELECT value FROM metadata WHERE key = 'workspace_binding'`); got != "not json" {
		t.Errorf("the refused Open rewrote the binding: %q", got)
	}
}

// The state directory cannot be created: Open fails and nothing stands in for it.
func TestUnwritableStateRootFailsOpen(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("a file where the state root must go"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", blocker)
	workspace := newWorkspace(t)
	if _, err := Open(Options{Workspace: workspace}); err == nil {
		t.Fatal("Open succeeded although the state directory cannot be created")
	}
	if _, err := os.Lstat(filepath.Join(workspace, ".lumencortex")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Open fell back to the workspace (lstat: %v)", err)
	}
}

// A cancelled Open leaves no database and the next Open starts clean.
func TestCancelledOpenLeavesNoState(t *testing.T) {
	workspace := newWorkspace(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := OpenContext(ctx, Options{Workspace: workspace}); err == nil {
		t.Fatal("OpenContext with a cancelled context succeeded")
	}
	canonical, _ := canonicalPath(workspace)
	dir, _ := externalStateDir(canonical)
	if _, err := os.Stat(filepath.Join(dir, "lumencortex.db")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a cancelled Open left a database (stat: %v)", err)
	}
	engine := openWorkspace(t, workspace)
	if engine.StateInfo().Path != dir {
		t.Errorf("state after the cancelled Open = %s", engine.StateInfo().Path)
	}
}

// bind.lock serializes the schema migration and the first binding: while another process holds it,
// Open waits for it and does not open the database.
func TestOpenWaitsForTheBindingLock(t *testing.T) {
	workspace := newWorkspace(t)
	dir := createdState(t, workspace, "history")
	lock := mustLock(t, dir, "bind")
	defer lock.Release()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := OpenContext(ctx, Options{Workspace: workspace}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Open while bind.lock is held: %v, want context.DeadlineExceeded", err)
	}
	lock.Release()
	openWorkspace(t, workspace)
}

// Project configuration stays in <workspace>/.lumencortex in both modes; only the database moved.
func TestProjectConfigIsReadFromTheWorkspaceInBothModes(t *testing.T) {
	servers := map[string]any{"project": map[string]any{"command": "/bin/true"}}
	for _, mode := range []StateMode{StateExternal, StateLegacy} {
		workspace := newWorkspace(t)
		if mode == StateLegacy {
			legacyState(t, workspace, "old")
		}
		writeEngineMCP(t, workspace, servers)
		engine := openWorkspace(t, workspace)
		if engine.StateInfo().Mode != mode {
			t.Fatalf("%s: opened as %+v", mode, engine.StateInfo())
		}
		configs := engine.MCPConfigs()
		if len(configs) != 1 || configs[0].Scope != "project" || configs[0].ID != "project" {
			t.Errorf("%s: project mcp.json not read from the workspace: %#v", mode, configs)
		}
		if mode == StateExternal {
			if _, err := os.Stat(filepath.Join(engine.StateInfo().Path, "mcp.json")); err == nil {
				t.Error("external mode read or wrote mcp.json in the state directory")
			}
		}
	}
}

const (
	helperWorkspaceEnv = "LCX_STATE_HELPER_WORKSPACE"
	helperGoEnv        = "LCX_STATE_HELPER_GO"
)

// TestHelperOpenWorkspace is the child process of TestConcurrentFirstOpenMakesOneStateAndBinding:
// it waits for the go file, opens the workspace, and reports the state it got.
func TestHelperOpenWorkspace(t *testing.T) {
	workspace := os.Getenv(helperWorkspaceEnv)
	if workspace == "" {
		t.Skip("helper process only")
	}
	os.Stdout.WriteString("READY\n")
	goFile := os.Getenv(helperGoEnv)
	for deadline := time.Now().Add(30 * time.Second); ; {
		if _, err := os.Stat(goFile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			os.Stdout.WriteString("RESULT error: no go signal\n")
			os.Exit(3)
		}
		time.Sleep(time.Millisecond)
	}
	engine, err := Open(Options{Workspace: workspace})
	if err != nil {
		os.Stdout.WriteString("RESULT error: " + err.Error() + "\n")
		os.Exit(3)
	}
	defer engine.Close()
	raw, _ := json.Marshal(engine.StateInfo())
	os.Stdout.WriteString("RESULT " + string(raw) + "\n")
}

// Several processes opening one new workspace at the same moment produce one state directory, one
// database and one binding, and every one of them opens it.
func TestConcurrentFirstOpenMakesOneStateAndBinding(t *testing.T) {
	isolatedState(t)
	const processes, rounds = 4, 3
	for round := range rounds {
		workspace := newWorkspace(t)
		before := len(stateEntries(t))
		goFile := filepath.Join(t.TempDir(), "go")

		type child struct {
			cmd *exec.Cmd
			out *bytes.Buffer
		}
		var children []child
		var ready sync.WaitGroup
		for range processes {
			cmd := exec.Command(os.Args[0], "-test.run=^TestHelperOpenWorkspace$", "-test.count=1")
			cmd.Env = append(os.Environ(), helperWorkspaceEnv+"="+workspace, helperGoEnv+"="+goFile)
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			buf := &bytes.Buffer{}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			children = append(children, child{cmd, buf})
			ready.Add(1)
			go func() {
				scanner := bufio.NewScanner(stdout)
				signalled := false
				for scanner.Scan() {
					buf.WriteString(scanner.Text() + "\n")
					if !signalled && strings.HasPrefix(scanner.Text(), "READY") {
						signalled = true
						ready.Done()
					}
				}
				if !signalled {
					ready.Done()
				}
			}()
		}
		ready.Wait()
		if err := os.WriteFile(goFile, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		var paths []string
		var workspaces []string
		for _, c := range children {
			if err := c.cmd.Wait(); err != nil {
				t.Fatalf("round %d: helper failed: %v\n%s", round, err, c.out.String())
			}
			var info StateInfo
			found := false
			for _, line := range strings.Split(c.out.String(), "\n") {
				if rest, ok := strings.CutPrefix(line, "RESULT "); ok {
					if err := json.Unmarshal([]byte(rest), &info); err != nil {
						t.Fatalf("round %d: helper said %q", round, line)
					}
					found = true
				}
			}
			if !found {
				t.Fatalf("round %d: helper reported no state:\n%s", round, c.out.String())
			}
			paths = append(paths, info.Path)
			workspaces = append(workspaces, info.Workspace)
		}
		for i := range paths {
			if paths[i] != paths[0] || workspaces[i] != workspaces[0] {
				t.Fatalf("round %d: processes disagree: %v", round, paths)
			}
		}
		if got := len(stateEntries(t)) - before; got != 1 {
			t.Errorf("round %d: %d new state directories, want 1", round, got)
		}
		if n := queryString(t, paths[0], `SELECT count(*) FROM metadata WHERE key = 'workspace_binding'`); n != "1" {
			t.Errorf("round %d: %s binding rows, want 1", round, n)
		}
		if got := boundPath(t, paths[0]); got != workspaces[0] {
			t.Errorf("round %d: binding %q, want %q", round, got, workspaces[0])
		}
	}
}

func mustLock(t *testing.T, dir, id string) *runlock.Lock {
	t.Helper()
	lock, err := runlock.TryAcquire(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	return lock
}

// The Engine hands its state to the tool set of every run: a model that asks write_file for the
// database through a symlink or a hard link is refused, in both modes, and an ordinary file in the
// same step is written.
func TestAgentFileToolsCannotReachTheEnginesState(t *testing.T) {
	isolatedState(t)
	for _, mode := range []StateMode{StateExternal, StateLegacy} {
		workspace := newWorkspace(t)
		if mode == StateLegacy {
			legacyState(t, workspace, "old")
		}
		engine := openWorkspace(t, workspace)
		if engine.StateInfo().Mode != mode {
			t.Fatalf("%s: opened as %+v", mode, engine.StateInfo())
		}
		database := filepath.Join(engine.StateInfo().Path, "lumencortex.db")
		if err := os.Symlink(database, filepath.Join(workspace, "soft.db")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if err := os.Link(database, filepath.Join(workspace, "hard.db")); err != nil {
			t.Fatal(err)
		}
		handle := newTestSession(t, engine, "reach for the database")
		call := func(id, path, content string) protocol.ToolCall {
			args, _ := json.Marshal(map[string]string{"path": path, "content": content})
			return protocol.ToolCall{ID: id, Name: "write_file", Arguments: args}
		}
		provider := &exitProvider{fn: func(_ context.Context, n int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
			if n == 1 {
				return protocol.ProviderResponse{Message: protocol.Message{ToolCalls: []protocol.ToolCall{
					call("soft", "soft.db", "clobbered"), call("hard", "hard.db", "clobbered"),
					call("abs", database, "clobbered"), call("ok", "ok.txt", "fine"),
				}}, FinishReason: "tool_calls"}, nil
			}
			return protocol.ProviderResponse{Message: protocol.Message{Content: "done"}}, nil
		}}
		if _, err := engine.runAgentWith(context.Background(), handle.ID, provider, AgentOptions{Policy: "workspace", MaxSteps: 3}); err != nil {
			t.Fatalf("%s: RunAgent: %v", mode, err)
		}
		results := map[string]string{}
		for _, row := range transcript(t, handle) {
			if row.Role == "tool" {
				results[row.ToolCallID] = row.Content
			}
		}
		for _, id := range []string{"soft", "hard", "abs"} {
			if !strings.Contains(results[id], "LumenCortex state") && !strings.Contains(results[id], "escapes workspace") {
				t.Errorf("%s: write_file %s was not refused: %q", mode, id, results[id])
			}
		}
		if strings.Contains(results["ok"], "error") {
			t.Errorf("%s: the ordinary write failed: %q", mode, results["ok"])
		}
		if raw, err := os.ReadFile(filepath.Join(workspace, "ok.txt")); err != nil || string(raw) != "fine" {
			t.Errorf("%s: ok.txt = %q, %v", mode, raw, err)
		}
		head, err := os.ReadFile(database)
		if err != nil || !bytes.HasPrefix(head, []byte("SQLite format 3\x00")) {
			t.Errorf("%s: the database was overwritten: %.20q, %v", mode, head, err)
		}
	}
}
