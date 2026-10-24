package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/edynasty/LumenCortex/internal/session"
)

// ErrStateBindingMismatch reports that the workspace database is bound to a different workspace, or
// that a database of a schema that always carries a binding has none (ADR-0016). Open ran the schema
// migration first and wrote nothing after it.
var ErrStateBindingMismatch = session.ErrStateBindingMismatch

// ErrRepositoryCarriedState reports that <workspace>/.lumencortex/lumencortex.db is tracked by Git,
// so it came with a clone rather than from this machine's runs and is not trusted as state
// (ADR-0016). Open wrote nothing; the file is neither read as history nor deleted.
var ErrRepositoryCarriedState = errors.New("the workspace database is tracked by Git and is not trusted as state")

// StateMode says where an Engine's runtime state lives.
type StateMode string

const (
	// StateExternal: <state root>/lumencortex/workspaces/<key>, outside the workspace, decided by Core.
	StateExternal StateMode = "external"
	// StateLegacy: <workspace>/.lumencortex, an earlier build's database opened in place.
	StateLegacy StateMode = "legacy"
)

// StateInfo reports which state an Engine opened.
type StateInfo struct {
	Mode StateMode `json:"mode"`
	// Path is the directory that holds lumencortex.db, db.lock and runs/.
	Path string `json:"path"`
	// Workspace is the canonical workspace path the state is bound to.
	Workspace string `json:"workspace"`
	// LegacyResidue is the legacy state directory when its lumencortex.db still exists but the
	// external state is in use. It is never read, moved or deleted by T0-S1.
	LegacyResidue string `json:"legacyResidue,omitempty"`
}

const (
	stateDBFile   = "lumencortex.db"
	legacyDirName = ".lumencortex"
	keyNameLimit  = 40
	keyHashBytes  = 8 // 16 hex digits
)

// stateRoot is the directory that holds one state directory per workspace: $XDG_STATE_HOME/
// lumencortex/workspaces when XDG_STATE_HOME is an absolute path, else ~/.local/state/lumencortex/
// workspaces. It does not create the directory. The convention is the one config.GlobalDir uses.
func stateRoot() (string, error) {
	if dir := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(dir) {
		return filepath.Join(dir, "lumencortex", "workspaces"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("state root: %w", err)
	}
	return filepath.Join(home, ".local", "state", "lumencortex", "workspaces"), nil
}

// canonicalPath is the one spelling of a directory that every alias of it maps to: absolute, symlinks
// resolved, and every component spelled as the file system stores it, so case variants and Unicode
// NFC/NFD variants of one directory agree. A component whose parent cannot be listed keeps the
// spelling it was given.
func canonicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	sep := string(filepath.Separator)
	volume := filepath.VolumeName(real)
	current := volume + sep
	for _, segment := range strings.Split(strings.TrimPrefix(real[len(volume):], sep), sep) {
		if segment == "" {
			continue
		}
		current = filepath.Join(current, onDiskName(current, segment))
	}
	return current, nil
}

// onDiskName returns the spelling of the directory dir/segment that dir stores. An exact entry wins;
// otherwise the entry that is the same directory (the file system folded case or normalization) does.
func onDiskName(dir, segment string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return segment
	}
	for _, entry := range entries {
		if entry.Name() == segment {
			return segment
		}
	}
	target, err := os.Lstat(filepath.Join(dir, segment))
	if err != nil {
		return segment
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if info, err := os.Lstat(filepath.Join(dir, entry.Name())); err == nil && os.SameFile(info, target) {
			return entry.Name()
		}
	}
	return segment
}

// workspaceKey is the state directory name of a canonical workspace path: its sanitized final
// component, "-", and the first 16 hex digits of the SHA-256 of the whole path.
func workspaceKey(canonical string) string {
	sum := sha256.Sum256([]byte(canonical))
	return sanitizeKeyName(filepath.Base(canonical)) + "-" + hex.EncodeToString(sum[:keyHashBytes])
}

func sanitizeKeyName(name string) string {
	var b strings.Builder
	count := 0
	for _, r := range name {
		if count == keyNameLimit {
			break
		}
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
		count++
	}
	if out := strings.Trim(b.String(), "._"); out != "" {
		return out
	}
	return "workspace"
}

// externalStateDir is where the state of the canonical workspace lives.
func externalStateDir(canonical string) (string, error) {
	root, err := stateRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, workspaceKey(canonical)), nil
}

// stateLocation is the decision of where an Engine's state lives, made without creating anything.
type stateLocation struct {
	StateInfo
	// external is where this workspace's external state is or would be, whichever mode was chosen.
	external string
	// legacy is <workspace>/.lumencortex.
	legacy string
}

// databaseExists reports whether dir holds lumencortex.db. Any failure other than absence is an
// error: an unreadable directory must not be taken for an empty one.
func databaseExists(dir string) (bool, error) {
	_, err := os.Lstat(filepath.Join(dir, stateDBFile))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("check for %s in %s: %w", stateDBFile, dir, err)
	}
}

// locateState returns the absolute workspace and decides which directory holds its state:
//
//   - Options.RepoDir, when set, is used as given (tests and self-checks);
//   - otherwise the external state when its database exists;
//   - otherwise <workspace>/.lumencortex when it holds lumencortex.db (legacy, opened in place);
//   - otherwise the external directory, created by Open.
//
// A legacy or RepoDir database that Git tracks is refused. It writes nothing.
func locateState(ctx context.Context, opts Options) (string, stateLocation, error) {
	workspace := opts.Workspace
	if workspace == "" {
		var err error
		if workspace, err = os.Getwd(); err != nil {
			return "", stateLocation{}, err
		}
	}
	workspace, err := filepath.Abs(workspace)
	if err != nil {
		return "", stateLocation{}, err
	}
	canonical, err := canonicalPath(workspace)
	if err != nil {
		return "", stateLocation{}, fmt.Errorf("workspace %s: %w", workspace, err)
	}
	external, err := externalStateDir(canonical)
	if err != nil {
		return "", stateLocation{}, err
	}
	loc := stateLocation{
		StateInfo: StateInfo{Mode: StateExternal, Path: external, Workspace: canonical},
		external:  external,
		legacy:    filepath.Join(workspace, legacyDirName),
	}

	if opts.RepoDir != "" {
		if loc.Path, err = filepath.Abs(opts.RepoDir); err != nil {
			return "", stateLocation{}, err
		}
		if sameDirectory(loc.Path, loc.legacy) {
			loc.Mode = StateLegacy
		}
		return workspace, loc, loc.refuseCarried(ctx)
	}

	externalExists, err := databaseExists(external)
	if err != nil {
		return "", stateLocation{}, err
	}
	legacyExists, err := databaseExists(loc.legacy)
	if err != nil {
		return "", stateLocation{}, err
	}
	switch {
	case externalExists:
		if legacyExists {
			loc.LegacyResidue = loc.legacy
		}
	case legacyExists:
		loc.Mode, loc.Path = StateLegacy, loc.legacy
		return workspace, loc, loc.refuseCarried(ctx)
	}
	return workspace, loc, nil
}

// refuseCarried returns ErrRepositoryCarriedState when the chosen database is tracked by Git.
func (l stateLocation) refuseCarried(ctx context.Context) error {
	exists, err := databaseExists(l.Path)
	if err != nil || !exists {
		return err
	}
	tracked, err := trackedByGit(ctx, filepath.Join(l.Path, stateDBFile))
	if err != nil {
		return err
	}
	if tracked {
		return fmt.Errorf("%w: %s", ErrRepositoryCarriedState, filepath.Join(l.Path, stateDBFile))
	}
	return nil
}

// sameDirectory reports whether a and b are one directory, by identity when both exist.
func sameDirectory(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	return err == nil && os.SameFile(ai, bi)
}

// gitProbeTimeout bounds the one git process that checks whether a database is tracked.
const gitProbeTimeout = 10 * time.Second

// trackedByGit reports whether the file is tracked by the Git repository that contains it. A file
// outside any repository, or a machine without git, is untracked. A repository in which git fails
// for any other reason is an error: the file is then neither adopted nor assumed safe.
func trackedByGit(ctx context.Context, file string) (bool, error) {
	dir := filepath.Dir(file)
	if !insideRepository(dir) {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, gitProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "ls-files", "--error-unmatch", "--", filepath.Base(file))
	cmd.Env = gitProbeEnv()
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, exec.ErrNotFound):
		return false, nil
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		return false, nil // pathspec did not match: not tracked
	default:
		return false, fmt.Errorf("cannot tell whether %s is tracked by Git, so it is not opened: %w: %s", file, err, strings.TrimSpace(string(out)))
	}
}

// insideRepository reports whether dir or an ancestor has a .git entry.
func insideRepository(dir string) bool {
	for {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}

// gitProbeEnv is the environment without variables that would point git at another repository.
func gitProbeEnv() []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		switch name, _, _ := strings.Cut(kv, "="); name {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR", "GIT_CEILING_DIRECTORIES":
		default:
			env = append(env, kv)
		}
	}
	return append(env, "LC_ALL=C")
}

// StateInfo reports which state this Engine opened: its mode, directory, the workspace it is bound
// to, and a legacy database that still exists beside the external state.
func (e *Engine) StateInfo() StateInfo { return e.state }
