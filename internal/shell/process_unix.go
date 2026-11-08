//go:build !windows

package shell

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/edynasty/LumenCortex/internal/cgroupv2"
)

// StartGroup makes the command lead its own process group, so a kill reaches every process it started,
// and makes cancellation kill that group instead of the command alone. The stdio MCP client uses it: a
// command's own children (a browser under an MCP server) outlive it otherwise.
func StartGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return KillGroup(cmd) }
}

// KillGroup sends SIGKILL to the command's process group. A group that is already gone is not an error.
func KillGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return killGroup(cmd.Process.Pid, syscall.SIGKILL)
}

func killGroup(pgid int, sig syscall.Signal) error {
	if err := syscall.Kill(-pgid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// Lifetime contract of a shell command (Runner):
//
//   - While the command runs, everything it starts belongs to it - including processes that start a
//     new session or process group (setsid, double fork). Cancellation, a timeout, Reap, and the death
//     of the owning process, however it dies, stop all of it.
//   - When the command ends on its own and Reap is false, what it left running in the background
//     (`server > log &`) keeps running, but only for as long as the owning process lives.
//
// On Linux with a delegated cgroup v2 subtree (a systemd user manager) the contract is kernel-enforced:
// every command runs in its own cgroup leaf, admitted before any of its code runs, and cgroup.kill
// reaches every member no matter its session or group. For as long as anything runs in the leaf -
// the command, or what it left in the background - two independent helpers kill the leaf when the
// owner dies: the command's crash guard and the pool's keeper (see cgroupv2). The owner supervises
// both while it lives: a guard or keeper that dies is replaced at once, and when a replacement cannot
// be started the owner kills what that helper protected (fail closed). So the final responsibility
// never rests on one user-space process: while the owner lives, the owner holds it; when the owner
// dies, both helpers do. Elsewhere (macOS, a Linux without a user manager such as a container) the
// command is contained by its process group only: a process that leaves the group is not followed.
// Containment reports which one applies.

// shellPools is this process's pool of command leaves, created on first use. When the first attempt
// fails the host has no delegated cgroups and commands are contained by their process group. A pool
// that worked and then broke (its keeper could not be replaced) is replaced on the next command; if
// that fails, commands are refused rather than run with less containment than before.
var shellPools struct {
	sync.Mutex
	tried   bool
	pool    *cgroupv2.Pool
	initErr error
}

func newShellPool() (*cgroupv2.Pool, error) {
	return cgroupv2.NewPool(cgroupv2.Options{Unit: "lc-shell", LeafPrefix: "c"})
}

// shellPool returns the pool for the next command: nil without an error when the host only offers
// process-group containment.
func shellPool() (*cgroupv2.Pool, error) {
	shellPools.Lock()
	defer shellPools.Unlock()
	if !shellPools.tried {
		shellPools.tried = true
		shellPools.pool, shellPools.initErr = newShellPool()
	}
	pool := shellPools.pool
	if pool == nil {
		return nil, nil
	}
	if broken := pool.Err(); broken != nil {
		fresh, err := newShellPool()
		if err != nil {
			return nil, fmt.Errorf("the shell cgroup pool broke (%v) and could not be recreated: %w", broken, err)
		}
		shellPools.pool = fresh
		pool = fresh
	}
	return pool, nil
}

// Containment names how shell commands are contained in this process: "cgroup" when every command
// gets its own cgroup leaf, otherwise "process-group" and the reason cgroups are unavailable.
func Containment() (string, error) {
	if _, err := shellPool(); err != nil {
		return "cgroup", err
	}
	shellPools.Lock()
	defer shellPools.Unlock()
	if shellPools.pool == nil {
		return "process-group", shellPools.initErr
	}
	return "cgroup", nil
}

// Seams for fault injection in tests: every resource newGuardedGroup acquires goes through one of them.
var (
	osPipe       = os.Pipe
	startProcess = func(cmd *exec.Cmd) error { return cmd.Start() }
	newLeaf      = func(pool *cgroupv2.Pool) (*cgroupv2.Leaf, error) { return pool.NewLeaf(nil) }
)

// guardScript is the crash guard of a shell command. It reads lines from the host until "done", which
// releases it. End of file - what the kernel delivers when the host process dies, however it dies, or
// when the host ends the guard to reap - kills the command's cgroup leaf ($1), when there is one, and
// the guard's process group. The first guard leads the command's process group and is not reaped
// before the host retires it, so its pid - the group id - cannot be reused while the host may still
// signal the group. A replacement guard leads a group of its own. The group's stop signals do not stop
// it.
const guardScript = `trap '' INT TERM HUP QUIT TSTP TTIN TTOU
while read line; do [ "$line" = done ] && exit 0; done
[ -n "$1" ] && echo 1 >"$1/cgroup.kill"
kill -KILL 0`

// gateScript holds a command until the host has moved it into its cgroup leaf: it waits for one line on
// descriptor 3, closes it, and execs the command, which keeps the pid the host admitted.
const gateScript = `read _ <&3 || exit 126; exec 3<&-; exec "$@"`

// guardian is the crash guard of one command and the host's supervision of it.
type guardian struct {
	leafDir string
	pgid    int       // the command's process group: the first guard's pid
	first   *exec.Cmd // the first guard, reaped only by retire

	mu      sync.Mutex
	cur     *exec.Cmd
	ctl     *os.File // host end of the current guard's control pipe
	retired bool
	// replaced counts the guards started after one died; failed is why the last replacement could not
	// be started (the leaf was killed then).
	replaced int
	failed   error
	// supervised is closed when supervise has returned (only with a leaf).
	supervised chan struct{}
}

// spawnGuard starts one guard. Every descriptor it opened is closed again when it fails.
func spawnGuard(leafDir string) (*exec.Cmd, *os.File, error) {
	r, w, err := osPipe()
	if err != nil {
		return nil, nil, err
	}
	guard := exec.Command("/bin/sh", "-c", guardScript, "lc-shell-guard", leafDir)
	guard.Stdin = r
	guard.Dir = "/"
	guard.Env = []string{"PATH=/usr/bin:/bin"}
	guard.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	err = startProcess(guard)
	_ = r.Close()
	if err != nil {
		_ = w.Close()
		return nil, nil, err
	}
	return guard, w, nil
}

// startGuardian starts the first guard; with a leaf, the host supervises it from then on.
func startGuardian(leafDir string) (*guardian, error) {
	guard, ctl, err := spawnGuard(leafDir)
	if err != nil {
		return nil, err
	}
	gd := &guardian{leafDir: leafDir, pgid: guard.Process.Pid, first: guard, cur: guard, ctl: ctl}
	if leafDir != "" {
		gd.supervised = make(chan struct{})
		go gd.supervise()
	}
	return gd, nil
}

// supervise replaces a guard that died while the host did not retire it. When no replacement can be
// started the leaf is killed: a command does not run on with one line of defence less.
func (gd *guardian) supervise() {
	defer close(gd.supervised)
	for {
		gd.mu.Lock()
		cur := gd.cur
		gd.mu.Unlock()
		// Observe the exit without reaping: the first guard's pid stays reserved as the group id.
		if err := waitExitedNoReap(cur.Process.Pid); err != nil && !errors.Is(err, syscall.ECHILD) {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		gd.mu.Lock()
		if gd.retired {
			gd.mu.Unlock()
			return
		}
		if cur != gd.first {
			_ = cur.Wait()
		}
		_ = gd.ctl.Close()
		guard, ctl, err := spawnGuard(gd.leafDir)
		if err != nil {
			gd.failed = err
			gd.retired = true
			gd.ctl = nil
			gd.mu.Unlock()
			_ = cgroupv2.WriteFile(gd.leafDir, "cgroup.kill", "1")
			return
		}
		gd.cur, gd.ctl = guard, ctl
		gd.replaced++
		gd.mu.Unlock()
	}
}

// retire ends the guard: with msg "done\n" it exits and leaves the command's processes alone; with an
// empty msg it reads end of file and kills the leaf and its group first. retire returns once every
// guard is reaped.
func (gd *guardian) retire(msg string) {
	gd.mu.Lock()
	if gd.retired && gd.ctl == nil {
		// A replacement failed; supervise already killed the leaf.
		gd.mu.Unlock()
		gd.reap()
		return
	}
	gd.retired = true
	ctl := gd.ctl
	gd.ctl = nil
	gd.mu.Unlock()
	if ctl != nil {
		if msg != "" {
			_, _ = ctl.Write([]byte(msg))
		}
		_ = ctl.Close()
	}
	gd.reap()
}

func (gd *guardian) reap() {
	if gd.supervised != nil {
		<-gd.supervised
	}
	gd.mu.Lock()
	cur, first := gd.cur, gd.first
	gd.mu.Unlock()
	_ = cur.Wait()
	if first != cur {
		_ = first.Wait()
	}
}

// guardedGroup is the process group (and, on Linux, the cgroup leaf) of one shell command, led by a crash
// guard (guardScript).
type guardedGroup struct {
	guard *guardian
	leaf  *cgroupv2.Leaf
	gate  *os.File // host end of the admission gate, until the command is released
	gateR *os.File
}

// rollback undoes, in reverse order, what a constructor acquired before it failed.
type rollback []func() error

func (r *rollback) add(undo func() error) { *r = append(*r, undo) }

func (r rollback) run() error {
	var errs []error
	for i := len(r) - 1; i >= 0; i-- {
		if err := r[i](); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func closeFiles(files ...*os.File) func() error {
	return func() error {
		for _, f := range files {
			_ = f.Close()
		}
		return nil
	}
}

// newGuardedGroup prepares the containment of cmd before it starts: the cgroup leaf when cgroups are
// available, the admission gate, the guard, and cmd's membership in the guard's process group. With a
// leaf, cmd is wrapped in gateScript so that none of its code runs before started has admitted it. The
// guard holds no descriptor of the command; the host's ends of the control and gate pipes are
// close-on-exec, so no command inherits them.
//
// Every resource is owned by a rollback entry from the moment it exists until the group is complete:
// a failure at any step releases exactly what was acquired before it, in reverse order, and reports
// the failure together with any error of that rollback. cmd is changed only on success.
func newGuardedGroup(cmd *exec.Cmd) (*guardedGroup, error) {
	pool, err := shellPool()
	if err != nil {
		return nil, fmt.Errorf("shell command cgroup: %w", err)
	}
	var undo rollback
	var leaf *cgroupv2.Leaf
	var gate, gateR *os.File
	if pool != nil {
		if leaf, err = newLeaf(pool); err != nil {
			return nil, fmt.Errorf("shell command cgroup: %w", err)
		}
		undo.add(leaf.Remove)
		if gateR, gate, err = osPipe(); err != nil {
			return nil, errors.Join(fmt.Errorf("shell admission gate: %w", err), undo.run())
		}
		undo.add(closeFiles(gateR, gate))
	}
	leafDir := ""
	if leaf != nil {
		leafDir = leaf.Dir()
	}
	guard, err := startGuardian(leafDir)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("shell crash guard: %w", err), undo.run())
	}
	g := &guardedGroup{guard: guard, leaf: leaf, gate: gate, gateR: gateR}
	if leaf != nil {
		cmd.Args = append([]string{"/bin/sh", "-c", gateScript, "lc-shell-gate", cmd.Path}, cmd.Args[1:]...)
		cmd.Path = "/bin/sh"
		cmd.ExtraFiles = []*os.File{gateR}
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: guard.pgid}
	cmd.Cancel = g.kill
	return g, nil
}

// started admits the just-started command into its leaf and lets it run. When admission fails the
// command is killed, never released: it has run none of its code.
func (g *guardedGroup) started(cmd *exec.Cmd) error {
	if g.leaf == nil {
		return nil
	}
	_ = g.gateR.Close()
	g.gateR = nil
	err := g.leaf.Admit(cmd.Process.Pid)
	if err == nil {
		_, err = g.gate.Write([]byte("go\n"))
	}
	_ = g.gate.Close()
	g.gate = nil
	if err != nil {
		_ = g.kill()
		return fmt.Errorf("shell command cgroup admission: %w", err)
	}
	return nil
}

// term asks the group to exit; the guard ignores it.
func (g *guardedGroup) term() error { return killGroup(g.guard.pgid, syscall.SIGTERM) }

// kill stops the command: every member of its leaf, wherever it moved, and the whole group, guard
// included.
func (g *guardedGroup) kill() error {
	var err error
	if g.leaf != nil {
		err = g.leaf.Kill()
	}
	return errors.Join(err, killGroup(g.guard.pgid, syscall.SIGKILL))
}

// release ends the command's containment once the command is over (or never started). With reap
// everything the command started is killed and its leaf removed. Without it, background processes the
// command left keep running, still guarded, until they end or the owner dies; the reaper removes their
// leaf once it is empty. A leaf that cannot be removed is reported - the returned error names it and
// why - and handed to the reaper, which retries a bounded number of times and records a persistent
// failure (CleanupFailures).
func (g *guardedGroup) release(reap bool) error {
	g.closeGate()
	if g.leaf == nil {
		// Process-group containment has nothing to remove. The kill is best effort: a group whose
		// members already exited is not a failure (macOS answers EPERM for a group of zombies).
		if reap {
			_ = g.kill()
			g.guard.retire("")
		} else {
			g.guard.retire("done\n")
		}
		return nil
	}
	if reap {
		_ = g.kill() // Remove confirms the outcome
		g.guard.retire("")
		if err := g.leaf.Remove(); err != nil {
			reaper.add(&cleanupEntry{leaf: g.leaf, kill: true, err: err, attempts: 1})
			return err
		}
		return nil
	}
	removed, err := g.leaf.RemoveIfEmpty()
	switch {
	case removed:
		g.guard.retire("done\n")
		return nil
	case err == nil:
		// The command left processes behind: they stay guarded until their leaf is empty.
		reaper.add(&cleanupEntry{leaf: g.leaf, guard: g.guard})
		return nil
	default:
		g.guard.retire("done\n")
		reaper.add(&cleanupEntry{leaf: g.leaf, err: err, attempts: 1})
		return err
	}
}

// closeGate closes what is left of the admission gate: a command that never started keeps both ends.
func (g *guardedGroup) closeGate() {
	for _, f := range []*os.File{g.gate, g.gateR} {
		if f != nil {
			_ = f.Close()
		}
	}
	g.gate, g.gateR = nil, nil
}

// CleanupFailure is a command leaf the reaper could not remove after its last attempt. The leaf stays
// registered with its pool, so closing the pool retries it once more and reports it.
type CleanupFailure struct {
	Leaf     string
	Attempts int
	Err      string
}

// cleanupEntry is a leaf the reaper owns: a released background tree (guard set, still guarded) or a
// leaf whose removal failed (err set).
type cleanupEntry struct {
	leaf     *cgroupv2.Leaf
	guard    *guardian
	kill     bool // the leaf must be emptied: Remove, not RemoveIfEmpty
	attempts int
	err      error
	next     time.Time
}

const (
	reaperTick         = 200 * time.Millisecond
	maxCleanupAttempts = 6
	maxCleanupBackoff  = 5 * time.Second
)

// reaper removes released leaves once they are empty and retries failed removals with a bounded
// backoff. It runs only while it owns an entry.
var reaper = &reaperState{}

type reaperState struct {
	mu      sync.Mutex
	entries []*cleanupEntry
	failed  []CleanupFailure
	running bool
}

func (r *reaperState) add(e *cleanupEntry) {
	if e.err != nil {
		e.next = time.Now().Add(backoff(e.attempts))
	}
	r.mu.Lock()
	r.entries = append(r.entries, e)
	start := !r.running
	r.running = true
	r.mu.Unlock()
	if start {
		go r.run()
	}
}

func backoff(attempts int) time.Duration {
	d := reaperTick << attempts
	if d > maxCleanupBackoff || d <= 0 {
		d = maxCleanupBackoff
	}
	return d
}

func (r *reaperState) run() {
	for {
		time.Sleep(reaperTick)
		// The entries stay listed while they are worked on, so PendingCleanups never misses a leaf the
		// reaper still owns; add only appends, so the first n entries are the ones processed here.
		r.mu.Lock()
		entries := slices.Clone(r.entries)
		r.mu.Unlock()
		n := len(entries)
		var keep []*cleanupEntry
		var failed []CleanupFailure
		for _, e := range entries {
			if !r.step(e) {
				continue
			}
			if e.attempts >= maxCleanupAttempts {
				failed = append(failed, CleanupFailure{Leaf: e.leaf.Dir(), Attempts: e.attempts, Err: e.err.Error()})
				continue
			}
			keep = append(keep, e)
		}
		r.mu.Lock()
		r.entries = append(keep, r.entries[n:]...)
		r.failed = append(r.failed, failed...)
		if len(r.entries) == 0 {
			r.running = false
			r.mu.Unlock()
			return
		}
		r.mu.Unlock()
	}
}

// step works on one entry and reports whether the reaper still owns it.
func (r *reaperState) step(e *cleanupEntry) bool {
	if time.Now().Before(e.next) {
		return true
	}
	if e.kill {
		if err := e.leaf.Remove(); err != nil {
			e.attempts++
			e.err = err
			e.next = time.Now().Add(backoff(e.attempts))
			return true
		}
		return false
	}
	removed, err := e.leaf.RemoveIfEmpty()
	switch {
	case removed:
		if e.guard != nil {
			e.guard.retire("done\n")
		}
		return false
	case err == nil:
		// Still running in the background: not an attempt.
		return true
	default:
		// Empty but not removable: nothing is left to guard.
		if e.guard != nil {
			e.guard.retire("done\n")
			e.guard = nil
		}
		e.attempts++
		e.err = err
		e.next = time.Now().Add(backoff(e.attempts))
		return true
	}
}

// CleanupFailures lists the command leaves the reaper gave up on.
func CleanupFailures() []CleanupFailure {
	reaper.mu.Lock()
	defer reaper.mu.Unlock()
	return append([]CleanupFailure(nil), reaper.failed...)
}

// PendingCleanups lists the leaves the reaper still owns: released background trees and leaves whose
// removal it is retrying.
func PendingCleanups() []string {
	reaper.mu.Lock()
	defer reaper.mu.Unlock()
	out := make([]string, 0, len(reaper.entries))
	for _, e := range reaper.entries {
		out = append(out, e.leaf.Dir())
	}
	return out
}
