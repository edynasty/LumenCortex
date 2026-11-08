//go:build linux

package evalruntime

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const cgroupRoot = "/sys/fs/cgroup"

// cgroupPool is the delegated cgroup v2 subtree this manager owns on Linux. Every
// worker gets its own leaf below it, so its memory, task count and process tree
// are bounded and observed by the kernel, not reconstructed from ps snapshots.
//
// The subtree is a transient systemd user scope created with Delegate=yes. A
// scope needs a member, so a keeper process (sh reading the host's pipe) holds
// it; it lives in its own "keeper" leaf because cgroup v2 forbids processes in a
// cgroup that distributes controllers. The keeper exits when the host closes the
// pipe or dies, and systemd then collects the scope.
type cgroupPool struct {
	dir      string
	unit     string
	busctl   string
	env      []string
	keeper   *exec.Cmd
	keeperIn io.WriteCloser
	exited   chan struct{}
	seq      atomic.Uint64

	mu     sync.Mutex
	leaves map[string]struct{}
	closed bool
}

// newCgroupPool creates the delegated subtree. It fails with the exact reason
// when cgroup v2, a systemd user manager, or the memory/pids controllers are not
// available to this user; the caller then refuses to run workers rather than
// pretending a memory limit exists.
func newCgroupPool() (*cgroupPool, error) {
	var fs unix.Statfs_t
	if err := unix.Statfs(cgroupRoot, &fs); err != nil || fs.Type != unix.CGROUP2_SUPER_MAGIC {
		return nil, errors.New("cgroup v2 is not mounted at /sys/fs/cgroup")
	}
	systemdRun, err := exec.LookPath("systemd-run")
	if err != nil {
		return nil, errors.New("systemd-run is not available to create a delegated cgroup")
	}
	busctl, err := exec.LookPath("busctl")
	if err != nil {
		return nil, errors.New("busctl is not available to admit workers into their cgroup")
	}
	var suffix [6]byte
	_, _ = rand.Read(suffix[:])
	unit := fmt.Sprintf("lc-evalruntime-%d-%s.scope", os.Getpid(), hex.EncodeToString(suffix[:]))
	keeper := exec.Command(systemdRun, "--user", "--scope", "--quiet", "--collect",
		"-p", "Delegate=yes", "--unit="+unit, "--", "/bin/sh", "-c", "read _ || :")
	// The keeper is a host helper, never a worker: it only needs to reach the
	// user's systemd manager.
	env := []string{"PATH=/usr/bin:/bin"}
	for _, name := range []string{"XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"} {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	keeper.Env = env
	var stderr bytes.Buffer
	keeper.Stderr = &stderr
	keeperIn, err := keeper.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := keeper.Start(); err != nil {
		return nil, fmt.Errorf("systemd-run: %w", err)
	}
	exited := make(chan struct{})
	go func() {
		_ = keeper.Wait()
		close(exited)
	}()
	pool := &cgroupPool{unit: unit, busctl: busctl, env: env, keeper: keeper, keeperIn: keeperIn, exited: exited, leaves: make(map[string]struct{})}
	fail := func(err error) (*cgroupPool, error) {
		_ = keeperIn.Close()
		_ = keeper.Process.Kill()
		<-exited
		return nil, err
	}
	// systemd-run moves itself into the scope before it execs the keeper, so the
	// keeper's cgroup names the unit once the scope exists.
	deadline := time.Now().Add(10 * time.Second)
	for pool.dir == "" {
		select {
		case <-exited:
			return fail(fmt.Errorf("systemd-run could not create a delegated user scope: %s", strings.TrimSpace(stderr.String())))
		default:
		}
		if rel, err := processCgroup(keeper.Process.Pid); err == nil && strings.HasSuffix(rel, "/"+unit) {
			pool.dir = filepath.Join(cgroupRoot, rel)
			break
		}
		if time.Now().After(deadline) {
			return fail(errors.New("timed out waiting for the delegated user scope"))
		}
		time.Sleep(10 * time.Millisecond)
	}
	keeperDir := filepath.Join(pool.dir, "keeper")
	if err := os.Mkdir(keeperDir, 0o700); err != nil {
		return fail(fmt.Errorf("delegated scope is not writable: %w", err))
	}
	if err := writeCgroupFile(keeperDir, "cgroup.procs", strconv.Itoa(keeper.Process.Pid)); err != nil {
		return fail(fmt.Errorf("move keeper: %w", err))
	}
	if err := writeCgroupFile(pool.dir, "cgroup.subtree_control", "+memory +pids"); err != nil {
		return fail(fmt.Errorf("memory and pids controllers are not delegated to this user: %w", err))
	}
	enabled, err := readCgroupFile(pool.dir, "cgroup.subtree_control")
	if err != nil {
		return fail(err)
	}
	for _, controller := range []string{"memory", "pids"} {
		if !containsField(enabled, controller) {
			return fail(fmt.Errorf("controller %s is not enabled in the delegated scope", controller))
		}
	}
	if err := pool.probeAdmission(); err != nil {
		return fail(err)
	}
	return pool, nil
}

// probeAdmission proves, once, that a gated process can be admitted into a leaf
// and that the leaf's limits hold for it.
func (p *cgroupPool) probeAdmission() error {
	leaf, err := p.newLeaf(64 << 20)
	if err != nil {
		return err
	}
	defer leaf.remove()
	probe := exec.Command("/bin/sh", "-c", "read _ || :")
	probe.Env = []string{"PATH=/usr/bin:/bin"}
	gate, err := probe.StdinPipe()
	if err != nil {
		return err
	}
	if err := probe.Start(); err != nil {
		return err
	}
	defer func() {
		_ = gate.Close()
		_ = probe.Process.Kill()
		_ = probe.Wait()
	}()
	if err := leaf.admit(probe.Process.Pid); err != nil {
		return fmt.Errorf("worker admission into the delegated cgroup failed: %w", err)
	}
	return nil
}

// cgroupLeaf is one worker's cgroup. Its limits are written before the worker
// exists. The worker is started behind a gate (a shell blocked on a pipe), moved
// into the leaf, verified to be a member, and only then released to exec the
// sandbox, so no instruction of the worker runs outside the bound.
//
// The move goes through the user's systemd manager (AttachProcessesToUnit): an
// unprivileged host process sits in its login session's cgroup, and cgroup v2
// lets only the manager that owns the common ancestor move it into the user's
// delegated scope. CLONE_INTO_CGROUP is refused for the same reason.
type cgroupLeaf struct {
	pool *cgroupPool
	dir  string
	name string
}

func (p *cgroupPool) newLeaf(memoryBytes int64) (*cgroupLeaf, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, errors.New("cgroup pool is closed")
	}
	name := fmt.Sprintf("w%d", p.seq.Add(1))
	dir := filepath.Join(p.dir, name)
	p.leaves[dir] = struct{}{}
	p.mu.Unlock()
	leaf := &cgroupLeaf{pool: p, dir: dir, name: name}
	fail := func(err error) (*cgroupLeaf, error) {
		leaf.remove()
		return nil, fmt.Errorf("worker cgroup: %w", err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return fail(err)
	}
	memory := "max"
	if memoryBytes > 0 {
		memory = strconv.FormatInt(memoryBytes, 10)
	}
	settings := [][2]string{
		{"memory.max", memory},
		{"memory.swap.max", "0"},
		// One OOM kill stops the whole worker: a kernel whose interpreter
		// survived while a helper was killed (or the reverse) has unknown state.
		{"memory.oom.group", "1"},
		{"pids.max", strconv.Itoa(workerTasksMax)},
	}
	for _, setting := range settings {
		if err := writeCgroupFile(dir, setting[0], setting[1]); err != nil {
			return fail(err)
		}
		got, err := readCgroupFile(dir, setting[0])
		if err != nil {
			return fail(err)
		}
		// The kernel rounds memory.max down to a page; it never raises it.
		if setting[0] == "memory.max" && memory != "max" {
			if value, convErr := strconv.ParseInt(got, 10, 64); convErr != nil || value > memoryBytes || value <= 0 {
				return fail(fmt.Errorf("memory.max reads %q after writing %s", got, memory))
			}
			continue
		}
		if got != setting[1] {
			return fail(fmt.Errorf("%s reads %q after writing %q", setting[0], got, setting[1]))
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "cgroup.kill")); err != nil {
		return fail(errors.New("cgroup.kill is unavailable; the worker tree cannot be stopped atomically"))
	}
	return leaf, nil
}

// admit moves a gated process into the leaf and confirms the kernel agrees.
func (l *cgroupLeaf) admit(pid int) error {
	cmd := exec.Command(l.pool.busctl, "--user", "--quiet", "call",
		"org.freedesktop.systemd1", "/org/freedesktop/systemd1", "org.freedesktop.systemd1.Manager",
		"AttachProcessesToUnit", "ssau", l.pool.unit, "/"+l.name, "1", strconv.Itoa(pid))
	cmd.Env = l.pool.env
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("AttachProcessesToUnit: %v: %s", err, strings.TrimSpace(string(out)))
	}
	rel, err := processCgroup(pid)
	if err != nil {
		return err
	}
	if filepath.Join(cgroupRoot, rel) != l.dir {
		return fmt.Errorf("process %d is in %s, not in its worker cgroup", pid, rel)
	}
	return nil
}

// kill stops every process of the leaf at once. cgroup.kill is atomic with
// respect to forks and addresses processes by membership, never by a pid that
// could have been reused.
func (l *cgroupLeaf) kill() error {
	return writeCgroupFile(l.dir, "cgroup.kill", "1")
}

// populated reports whether any process is still in the leaf.
func (l *cgroupLeaf) populated() bool {
	events, err := readCgroupFile(l.dir, "cgroup.events")
	if err != nil {
		// A leaf that cannot be read cannot be certified empty.
		return !errors.Is(err, os.ErrNotExist)
	}
	return keyedValue(events, "populated") != 0
}

// procs lists the host pids that are still members of the leaf.
func (l *cgroupLeaf) procs() []int {
	text, err := readCgroupFile(l.dir, "cgroup.procs")
	if err != nil {
		return nil
	}
	var out []int
	for _, field := range strings.Fields(text) {
		if pid, err := strconv.Atoi(field); err == nil {
			out = append(out, pid)
		}
	}
	sort.Ints(out)
	return out
}

func (l *cgroupLeaf) memoryPeak() int64 {
	text, err := readCgroupFile(l.dir, "memory.peak")
	if err != nil {
		return 0
	}
	value, _ := strconv.ParseInt(text, 10, 64)
	return value
}

func (l *cgroupLeaf) memoryMax() string {
	text, _ := readCgroupFile(l.dir, "memory.max")
	return text
}

// oomKills is the kernel's count of processes killed because the leaf reached
// memory.max.
func (l *cgroupLeaf) oomKills() int64 {
	events, err := readCgroupFile(l.dir, "memory.events")
	if err != nil {
		return 0
	}
	return keyedValue(events, "oom_kill")
}

func (l *cgroupLeaf) cpuMillis() int64 {
	stat, err := readCgroupFile(l.dir, "cpu.stat")
	if err != nil {
		return 0
	}
	return keyedValue(stat, "usage_usec") / 1000
}

// remove deletes the leaf. An occupied leaf is killed first; rmdir fails while
// any process remains, so a removed leaf proves the tree is gone.
func (l *cgroupLeaf) remove() {
	if err := os.Remove(l.dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		_ = l.kill()
		deadline := time.Now().Add(cleanupWindow)
		for l.populated() && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if err := os.Remove(l.dir); err != nil && !errors.Is(err, os.ErrNotExist) {
			return // keep it registered: close() retries and reports it
		}
	}
	l.pool.mu.Lock()
	delete(l.pool.leaves, l.dir)
	l.pool.mu.Unlock()
}

// liveLeaves lists the leaves that still exist (tests and Dispose use it).
func (p *cgroupPool) liveLeaves() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.leaves))
	for dir := range p.leaves {
		if _, err := os.Stat(dir); err == nil {
			out = append(out, dir)
		}
	}
	sort.Strings(out)
	return out
}

// close kills and removes every remaining leaf, then releases the keeper so
// systemd collects the scope. It reports leaves that could not be removed.
func (p *cgroupPool) close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	dirs := make([]string, 0, len(p.leaves))
	for dir := range p.leaves {
		dirs = append(dirs, dir)
	}
	p.mu.Unlock()
	var stuck []string
	for _, dir := range dirs {
		(&cgroupLeaf{pool: p, dir: dir}).remove()
		if _, err := os.Stat(dir); err == nil {
			stuck = append(stuck, dir)
		}
	}
	_ = p.keeperIn.Close()
	select {
	case <-p.exited:
	case <-time.After(2 * time.Second):
		_ = p.keeper.Process.Kill()
		<-p.exited
	}
	_ = os.Remove(filepath.Join(p.dir, "keeper"))
	if len(stuck) > 0 {
		return fmt.Errorf("%d worker cgroup(s) still hold processes: %v", len(stuck), stuck)
	}
	return nil
}

// processCgroup returns the cgroup v2 path of pid relative to the hierarchy root.
func processCgroup(pid int) (string, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if rest, ok := strings.CutPrefix(line, "0::"); ok {
			return rest, nil
		}
	}
	return "", errors.New("no cgroup v2 membership")
}

func writeCgroupFile(dir, name, value string) error {
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, err = f.WriteString(value)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

func readCgroupFile(dir, name string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, name))
	return strings.TrimSpace(string(raw)), err
}

func keyedValue(text, key string) int64 {
	scanner := bufio.NewScanner(strings.NewReader(text))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 && fields[0] == key {
			value, _ := strconv.ParseInt(fields[1], 10, 64)
			return value
		}
	}
	return 0
}

func containsField(text, want string) bool {
	for _, field := range strings.Fields(text) {
		if field == want {
			return true
		}
	}
	return false
}

// hostIdentity is a process as the host sees it: its pid plus its start time in
// clock ticks since boot. The pair never repeats, so a check against it cannot
// be fooled by pid reuse.
type hostIdentity struct {
	pid   int
	start uint64
}

// procStat reads ppid, process group and start time from /proc/<pid>/stat.
func procStat(pid int) (ppid, pgrp int, start uint64, err error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, 0, 0, err
	}
	// comm may contain spaces and parentheses; fields resume after the last ')'.
	end := bytes.LastIndexByte(raw, ')')
	if end < 0 {
		return 0, 0, 0, errors.New("malformed stat")
	}
	fields := strings.Fields(string(raw[end+1:]))
	// fields[0] is state (field 3); ppid is field 4, pgrp 5, starttime 22.
	if len(fields) < 20 {
		return 0, 0, 0, errors.New("short stat")
	}
	ppid, _ = strconv.Atoi(fields[1])
	pgrp, _ = strconv.Atoi(fields[2])
	start, _ = strconv.ParseUint(fields[19], 10, 64)
	return ppid, pgrp, start, nil
}

// nsPIDs returns the NSpid chain of pid: the host pid first, the pid in the
// innermost namespace last.
func nsPIDs(pid int) ([]int, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if rest, ok := strings.CutPrefix(line, "NSpid:"); ok {
			var out []int
			for _, field := range strings.Fields(rest) {
				value, err := strconv.Atoi(field)
				if err != nil {
					return nil, err
				}
				out = append(out, value)
			}
			return out, nil
		}
	}
	return nil, errors.New("no NSpid")
}

func identityOf(pid int) (hostIdentity, error) {
	_, _, start, err := procStat(pid)
	if err != nil {
		return hostIdentity{}, err
	}
	return hostIdentity{pid: pid, start: start}, nil
}

// alive reports whether exactly this process (not a reused pid) still exists.
// A zombie counts as gone: it runs no code and holds no memory.
func (id hostIdentity) alive() bool {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", id.pid))
	if err != nil {
		return false
	}
	end := bytes.LastIndexByte(raw, ')')
	if end < 0 {
		return false
	}
	fields := strings.Fields(string(raw[end+1:]))
	if len(fields) < 20 || fields[0] == "Z" || fields[0] == "X" {
		return false
	}
	start, _ := strconv.ParseUint(fields[19], 10, 64)
	return start == id.start
}

// workerGroup is the sandbox's own session: bubblewrap's --new-session makes the
// in-namespace init (pid 1 inside, a child of the outer bwrap on the host) lead a
// new session and process group, and the interpreter runs in that group. Signals
// meant for the worker go there, never to the outer bwrap's group, whose death
// would tear the whole sandbox down.
type workerGroup struct {
	id    hostIdentity
	pidfd int
}

// findWorkerGroup locates the in-namespace init among the leaf's members and
// pins it with a pidfd. It never guesses: a candidate must be a member of this
// worker's cgroup, a child of the outer bwrap, pid 1 of its namespace and its
// own group leader, and it must be the same process after the pidfd is opened.
func findWorkerGroup(leaf *cgroupLeaf, outer int) (*workerGroup, error) {
	for _, pid := range leaf.procs() {
		ppid, pgrp, start, err := procStat(pid)
		if err != nil || ppid != outer || pgrp != pid {
			continue
		}
		chain, err := nsPIDs(pid)
		if err != nil || len(chain) < 2 || chain[len(chain)-1] != 1 {
			continue
		}
		fd, err := unix.PidfdOpen(pid, 0)
		if err != nil {
			continue
		}
		// The pidfd refers to whatever owned the pid at open time: confirm it is
		// still the process that was validated above.
		_, pgrpAfter, startAfter, err := procStat(pid)
		if err != nil || startAfter != start || pgrpAfter != pid {
			_ = unix.Close(fd)
			continue
		}
		return &workerGroup{id: hostIdentity{pid: pid, start: start}, pidfd: fd}, nil
	}
	return nil, errors.New("eval: the sandboxed worker's process group could not be identified")
}

// signal delivers sig to the worker's process group. The pidfd proves the group
// leader is still the pinned process, and a process group id cannot be reused
// while its leader lives, so the group signalled is this worker's.
func (g *workerGroup) signal(sig syscall.Signal) error {
	if err := unix.PidfdSendSignal(g.pidfd, 0, nil, 0); err != nil {
		return fmt.Errorf("eval: worker group leader is gone: %w", err)
	}
	return syscall.Kill(-g.id.pid, sig)
}

func (g *workerGroup) close() {
	if g != nil && g.pidfd >= 0 {
		_ = unix.Close(g.pidfd)
		g.pidfd = -1
	}
}

// cgroupRequired: Linux workers never start without their own cgroup.
const cgroupRequired = true
