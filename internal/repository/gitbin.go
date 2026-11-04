package repository

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// macGitShim is the git that macOS installs in /usr/bin: a stub that asks xcrun for the active
// developer directory on every call and then runs that directory's git. The lookup costs about 10 ms
// per process on top of git itself (T1 G1 measurement), which a side-effect tool pays two to four
// times per call for its snapshots.
const macGitShim = "/usr/bin/git"

var (
	shimMu     sync.Mutex
	shimTarget = map[string]string{} // DEVELOPER_DIR -> resolved git, "" when the lookup failed
	// lookPath and findGit are replaced in tests.
	lookPath = exec.LookPath
	findGit  = func() (string, error) {
		out, err := exec.Command("/usr/bin/xcrun", "--find", "git").Output()
		return strings.TrimSpace(string(out)), err
	}
)

// snapshotGit names the executable a read-only snapshot or blob command runs: "git", resolved from
// PATH at the call as before, except that when PATH selects the macOS shim it is the binary the shim
// would run, found once per DEVELOPER_DIR value. The same git binary runs either way; only the
// per-call developer-directory lookup is skipped. Any failure keeps "git".
func snapshotGit() string {
	if runtime.GOOS != "darwin" {
		return "git"
	}
	if path, err := lookPath("git"); err != nil || path != macGitShim {
		return "git"
	}
	key := os.Getenv("DEVELOPER_DIR")
	shimMu.Lock()
	defer shimMu.Unlock()
	target, ok := shimTarget[key]
	if !ok {
		target = ""
		if found, err := findGit(); err == nil && filepath.IsAbs(found) && found != macGitShim {
			if info, err := os.Stat(found); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
				target = found
			}
		}
		shimTarget[key] = target
	}
	if target == "" {
		return "git"
	}
	return target
}
