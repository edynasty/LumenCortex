//go:build linux

package shell

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/cgroupv2"
)

// openFDs counts the descriptors of this process.
func openFDs(t testing.TB) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

// liveChildren lists this process's children that are not zombies, apart from the ones in except.
func liveChildren(except ...int) []int {
	self := os.Getpid()
	entries, _ := os.ReadDir("/proc")
	var out []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || slices.Contains(except, pid) {
			continue
		}
		fields, ok := procStat(pid)
		if !ok || fields[0] == "Z" || fields[0] == "X" {
			continue
		}
		if ppid, _ := strconv.Atoi(fields[1]); ppid == self {
			out = append(out, pid)
		}
	}
	return out
}

// settled waits until cond holds, for the guards and reaper goroutines that finish a release.
func settled(within time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	return true
}

// keeperPIDs reads the keeper leaf of the scope that holds leaf.
func keeperPIDs(t *testing.T, leaf string) []int {
	t.Helper()
	text, err := cgroupv2.ReadFile(filepath.Join(filepath.Dir(leaf), "keeper"), "cgroup.procs")
	if err != nil {
		t.Fatalf("keeper of %s: %v", filepath.Dir(leaf), err)
	}
	var out []int
	for _, field := range strings.Fields(text) {
		pid, _ := strconv.Atoi(field)
		out = append(out, pid)
	}
	return out
}

// R3-SOL-P2-03: a released background tree keeps both lines of defence while the owner lives. When its
// guard, the pool's keeper or both die, the owner replaces them, the tree keeps running (the release
// contract), and the owner's death still stops all of it.
func TestReleasedBackgroundKeepsItsHelpersUntilTheOwnerDies(t *testing.T) {
	requireCgroupContainment(t)
	for _, preKill := range []string{"keeper", "guard", "both"} {
		t.Run(preKill, func(t *testing.T) {
			owner, dir := startOwner(t, "background")
			procs := commandProcs(t, dir)
			waitFor(t, filepath.Join(dir, "returned"))
			delete(procs, "tool")
			leaf := procs["child"].cgroup(t)
			for name, p := range procs {
				if got := p.cgroup(t); got != leaf {
					t.Fatalf("%s is in %s, not in the released leaf %s", name, got, leaf)
				}
			}
			guard := procs["child"].pgid()
			keepers := keeperPIDs(t, leaf)
			if preKill == "guard" || preKill == "both" {
				if err := syscall.Kill(guard, syscall.SIGKILL); err != nil {
					t.Fatalf("kill guard %d: %v", guard, err)
				}
			}
			if preKill == "keeper" || preKill == "both" {
				for _, pid := range keepers {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
				// The owner admits a new keeper into the keeper leaf.
				if !settled(3*time.Second, func() bool {
					now := keeperPIDs(t, leaf)
					return len(now) > 0 && !slices.Equal(now, keepers)
				}) {
					t.Fatalf("no new keeper after the keeper died: %v", keeperPIDs(t, leaf))
				}
			}
			time.Sleep(300 * time.Millisecond)
			running(t, procs)
			if err := owner.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			_ = owner.Wait()
			gone(t, 3*time.Second, procs)
		})
	}
}

// TestStartupFailureHelper runs commands under a real, lowered RLIMIT_NOFILE and records, for each
// limit, what Run returned and whether descriptors, children or leaves leaked. A panic fails the
// helper process.
func TestStartupFailureHelper(t *testing.T) {
	dir := os.Getenv("LC_EMFILE_DIR")
	if dir == "" {
		t.Skip("helper process of TestRealEMFILEDuringStartupIsAnError")
	}
	r := New(dir)
	if result, err := r.Run(context.Background(), "true", nil); err != nil || result.ExitCode != 0 {
		t.Fatalf("warm-up: %+v %v", result, err)
	}
	pool, err := shellPool()
	if err != nil {
		t.Fatal(err)
	}
	var keeper []int
	liveBefore := 0
	if pool != nil {
		keeper = []int{pool.KeeperPID()}
		liveBefore = len(pool.Live())
	}
	var original syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &original); err != nil {
		t.Fatal(err)
	}
	baseFDs := openFDs(t)
	type record struct {
		Limit    uint64 `json:"limit"`
		Err      string `json:"err"`
		Exit     int    `json:"exit"`
		Stdout   string `json:"stdout"`
		LeakFDs  int    `json:"leakFDs"`
		Children []int  `json:"children"`
		Leaves   int    `json:"leaves"`
	}
	var records []record
	for extra := uint64(0); extra <= 24; extra++ {
		limit := uint64(baseFDs) + extra
		lowered := syscall.Rlimit{Cur: limit, Max: original.Max}
		if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lowered); err != nil {
			t.Fatal(err)
		}
		result, runErr := r.RunTimeout(context.Background(), "echo ok", 10*time.Second, nil)
		if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &original); err != nil {
			t.Fatal(err)
		}
		rec := record{Limit: limit, Exit: result.ExitCode, Stdout: strings.TrimSpace(string(result.Stdout.Head))}
		if runErr != nil {
			rec.Err = runErr.Error()
		}
		settled(3*time.Second, func() bool { return openFDs(t) <= baseFDs && len(liveChildren(keeper...)) == 0 })
		rec.LeakFDs = openFDs(t) - baseFDs
		rec.Children = liveChildren(keeper...)
		if pool != nil {
			rec.Leaves = len(pool.Live()) - liveBefore
		}
		records = append(records, rec)
	}
	data, _ := json.Marshal(records)
	if err := os.WriteFile(filepath.Join(dir, "records.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// R3-SOL-P1-01: a real EMFILE at any step of a command's startup is returned as an error: no panic,
// and nothing acquired before the failure - descriptors, guard processes, cgroup leaves - is left.
func TestRealEMFILEDuringStartupIsAnError(t *testing.T) {
	dir := t.TempDir()
	helper := exec.Command(os.Args[0], "-test.run=^TestStartupFailureHelper$", "-test.v")
	helper.Env = append(os.Environ(), "LC_EMFILE_DIR="+dir)
	out, err := helper.CombinedOutput()
	if err != nil || strings.Contains(string(out), "panic") {
		t.Fatalf("helper failed: %v\n%s", err, out)
	}
	data, err := os.ReadFile(filepath.Join(dir, "records.json"))
	if err != nil {
		t.Fatalf("helper wrote no records: %v\n%s", err, out)
	}
	var records []struct {
		Limit    uint64
		Err      string
		Exit     int
		Stdout   string
		LeakFDs  int
		Children []int
		Leaves   int
	}
	if err := json.Unmarshal(data, &records); err != nil {
		t.Fatal(err)
	}
	failed, succeeded := 0, 0
	for _, rec := range records {
		t.Logf("limit %d: exit=%d stdout=%q err=%q leakFDs=%d children=%v leaves=%d", rec.Limit, rec.Exit, rec.Stdout, rec.Err, rec.LeakFDs, rec.Children, rec.Leaves)
		if rec.LeakFDs > 0 || len(rec.Children) > 0 || rec.Leaves != 0 {
			t.Errorf("limit %d leaked: fds=%d children=%v leaves=%d", rec.Limit, rec.LeakFDs, rec.Children, rec.Leaves)
		}
		switch {
		case rec.Err == "" && rec.Exit == 0 && rec.Stdout == "ok":
			succeeded++
		case rec.Err != "":
			failed++
			if !strings.Contains(rec.Err, "too many open files") {
				t.Errorf("limit %d: error %q does not name EMFILE", rec.Limit, rec.Err)
			}
		default:
			t.Errorf("limit %d: neither an error nor a successful command", rec.Limit)
		}
	}
	if failed == 0 || succeeded == 0 {
		t.Fatalf("the sweep did not cross the startup: %d failed, %d succeeded", failed, succeeded)
	}
}

// Every resource acquisition of a command's startup can fail; each failure is returned and rolls back
// exactly what was acquired before it.
func TestInjectedStartupFailuresRollBack(t *testing.T) {
	requireCgroupContainment(t)
	r := New(t.TempDir())
	if _, err := r.Run(context.Background(), "true", nil); err != nil {
		t.Fatal(err)
	}
	pool, err := shellPool()
	if err != nil || pool == nil {
		t.Fatalf("pool: %v", err)
	}
	keeper := pool.KeeperPID()
	baseFDs, baseLeaves := openFDs(t), len(pool.Live())
	injected := errors.New("injected")
	realPipe, realStart, realLeaf := osPipe, startProcess, newLeaf
	t.Cleanup(func() { osPipe, startProcess, newLeaf = realPipe, realStart, realLeaf })
	cases := []struct {
		name string
		arm  func(n int)
		max  int
	}{
		{"leaf", func(n int) {
			newLeaf = func(p *cgroupv2.Pool) (*cgroupv2.Leaf, error) {
				return nil, injected
			}
		}, 1},
		{"pipe", func(n int) {
			calls := 0
			osPipe = func() (*os.File, *os.File, error) {
				calls++
				if calls == n {
					return nil, nil, injected
				}
				return realPipe()
			}
		}, 2},
		{"start", func(n int) {
			calls := 0
			startProcess = func(cmd *exec.Cmd) error {
				calls++
				if calls == n {
					return injected
				}
				return realStart(cmd)
			}
		}, 1},
	}
	for _, tc := range cases {
		for n := 1; n <= tc.max; n++ {
			t.Run(fmt.Sprintf("%s-%d", tc.name, n), func(t *testing.T) {
				tc.arm(n)
				defer func() { osPipe, startProcess, newLeaf = realPipe, realStart, realLeaf }()
				result, err := r.Run(context.Background(), "echo ran > ran", nil)
				if !errors.Is(err, injected) {
					t.Fatalf("Run = %+v, %v; want the injected failure", result, err)
				}
				if _, statErr := os.Stat(filepath.Join(r.Workspace, "ran")); statErr == nil {
					t.Fatal("the command ran although its containment failed")
				}
				if !settled(3*time.Second, func() bool {
					return openFDs(t) <= baseFDs && len(liveChildren(keeper)) == 0 && len(pool.Live()) == baseLeaves
				}) {
					t.Fatalf("leaked: fds %d>%d children %v leaves %v", openFDs(t), baseFDs, liveChildren(keeper), pool.Live())
				}
			})
		}
	}
}

// R3-SOL-P2-04: a leaf that cannot be removed is visible to the caller - Result.CleanupError and an
// error wrapping ErrCleanup - and the reaper retries it until it is gone. A command that leaves a
// background process without Reap is not a cleanup failure.
func TestCleanupFailureIsReportedAndRetried(t *testing.T) {
	requireCgroupContainment(t)
	r := New(t.TempDir())
	r.Reap = true
	var failing atomic.Bool
	failing.Store(true)
	leafName := regexp.MustCompile(`^c[0-9]+$`)
	restore := cgroupv2.InjectRemoveFault(func(dir string) error {
		if failing.Load() && leafName.MatchString(filepath.Base(dir)) {
			return &os.PathError{Op: "remove", Path: dir, Err: syscall.EBUSY}
		}
		return nil
	})
	t.Cleanup(restore)
	result, err := r.Run(context.Background(), "cat /proc/self/cgroup", nil)
	if !errors.Is(err, ErrCleanup) || result.CleanupError == "" {
		t.Fatalf("Run = %+v, %v; want a reported cleanup failure", result, err)
	}
	if result.ExitCode != 0 || !strings.Contains(string(result.Stdout.Head), "lc-shell-") {
		t.Fatalf("the command's own result was lost: %+v", result)
	}
	rel := strings.TrimPrefix(strings.TrimSpace(string(result.Stdout.Head)), "0::")
	leaf := filepath.Join(cgroupv2.Root, rel)
	if !slices.Contains(PendingCleanups(), leaf) {
		t.Fatalf("the failed leaf %s is not retained for retry: %v", leaf, PendingCleanups())
	}
	failing.Store(false)
	if !settled(5*time.Second, func() bool { return !slices.Contains(PendingCleanups(), leaf) }) {
		t.Fatalf("the reaper did not retry %s", leaf)
	}
	if _, err := os.Stat(leaf); !os.IsNotExist(err) {
		t.Fatalf("leaf %s is still there: %v", leaf, err)
	}

	// The real reproducer: a command creates a cgroup below its own leaf. Removal deletes it too.
	result, err = r.Run(context.Background(), `mkdir "/sys/fs/cgroup$(cut -d: -f3 /proc/self/cgroup)/nested" && cut -d: -f3 /proc/self/cgroup`, nil)
	if err != nil || result.CleanupError != "" {
		t.Fatalf("nested cgroup: %+v %v", result, err)
	}
	nested := filepath.Join(cgroupv2.Root, strings.TrimSpace(string(result.Stdout.Head)))
	if _, err := os.Stat(nested); !os.IsNotExist(err) {
		t.Fatalf("leaf %s with a nested cgroup is still there: %v", nested, err)
	}

	// Without Reap a background process is a released tree, not a failure.
	bg := New(t.TempDir())
	result, err = bg.Run(context.Background(), "sleep 1 >/dev/null 2>&1 & cut -d: -f3 /proc/self/cgroup", nil)
	if err != nil || result.CleanupError != "" {
		t.Fatalf("released background reported as a failure: %+v %v", result, err)
	}
	released := filepath.Join(cgroupv2.Root, strings.TrimSpace(string(result.Stdout.Head)))
	if !slices.Contains(PendingCleanups(), released) {
		t.Fatalf("the released leaf %s is not owned by the reaper: %v", released, PendingCleanups())
	}
	if !settled(5*time.Second, func() bool { _, err := os.Stat(released); return os.IsNotExist(err) }) {
		t.Fatalf("the released leaf %s was not removed once its tree ended", released)
	}
}
