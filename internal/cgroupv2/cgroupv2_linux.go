//go:build linux

// Package cgroupv2 gives a process tree its own cgroup v2 leaf below a
// delegated systemd user scope: the kernel then bounds it, accounts for it and
// stops all of it at once, including processes that left its session or process
// group. The eval runtime runs every worker in a leaf, the shell runs every
// command in one.
package cgroupv2

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Root is where the cgroup v2 hierarchy is mounted.
const Root = "/sys/fs/cgroup"

// cleanupWindow bounds how long Remove waits for a killed leaf to empty.
const cleanupWindow = 3 * time.Second

// keeperScript holds the scope open and is its last line of defence. It reads
// one line from the owner; end of file - what the kernel delivers when the owner
// dies, however it dies - makes it kill every other leaf of the scope, so no
// tree the owner placed in a leaf outlives the owner, even when the owner never
// ran its own cleanup. $1 is the scope's unit name: the keeper acts only from
// inside that scope (its keeper leaf, or the scope itself before it was moved
// there), so a keeper that is not, or not yet, a member never derives a scope
// from the owner's own cgroup and never touches a sibling of it.
const keeperScript = `read _ || :
while IFS= read -r line; do case $line in 0::*) self=${line#0::};; esac; done </proc/self/cgroup
case $self in
*/"$1"/keeper) scope=/sys/fs/cgroup${self%/keeper};;
*/"$1") scope=/sys/fs/cgroup$self;;
*) exit 0;;
esac
for kill in "$scope"/*/cgroup.kill; do
	[ "$kill" = "$scope/keeper/cgroup.kill" ] || echo 1 >"$kill"
done 2>/dev/null`

// ErrPopulated reports a leaf that still holds processes after it was killed
// and the cleanup window passed.
var ErrPopulated = errors.New("cgroup still holds processes")

// removeFault, when set by a test, is consulted before every cgroup rmdir. It
// is read by background cleanup goroutines, so it is swapped atomically.
var removeFault atomic.Pointer[func(dir string) error]

// rmdir removes one cgroup directory, unless an injected fault refuses it.
func rmdir(dir string) error {
	if fault := removeFault.Load(); fault != nil {
		if err := (*fault)(dir); err != nil {
			return &os.PathError{Op: "remove", Path: dir, Err: err}
		}
	}
	return os.Remove(dir)
}

// InjectRemoveFault makes every cgroup rmdir consult fault first: a non-nil
// error is returned instead of removing the directory. It returns the function
// that restores the previous behaviour. Tests only.
func InjectRemoveFault(fault func(dir string) error) (restore func()) {
	previous := removeFault.Swap(&fault)
	return func() { removeFault.Store(previous) }
}

// Options name a pool and the controllers its leaves need.
type Options struct {
	// Unit is the prefix of the transient scope's name; the owner's pid and a
	// random suffix make it unique.
	Unit string
	// LeafPrefix names the leaves: <LeafPrefix><n>.
	LeafPrefix string
	// Controllers are enabled for the leaves (for example memory and pids).
	// cgroup.kill needs none.
	Controllers []string
}

// Setting is one interface file of a leaf and the value it must hold.
type Setting struct {
	File  string
	Value string
}

// Pool is a delegated cgroup v2 subtree owned by this process: a transient
// systemd user scope created with Delegate=yes. A scope needs a member, so a
// keeper process (keeperScript) holds it; it lives in its own "keeper" leaf
// because cgroup v2 forbids processes in a cgroup that distributes controllers.
// When the owner closes the pool, or dies, the keeper exits and systemd collects
// the scope.
//
// The owner supervises the keeper: when the keeper dies while the owner lives,
// the owner starts a new one in the keeper leaf at once, so the scope always has
// a process that kills its leaves when the owner dies. When no new keeper can be
// admitted, the pool fails closed: every leaf is killed and the pool is broken
// (Err reports why), so no tree runs on without its last line of defence.
type Pool struct {
	dir    string
	unit   string
	prefix string
	busctl string
	env    []string
	seq    atomic.Uint64
	// supervised is closed when the keeper supervisor has returned.
	supervised chan struct{}

	mu       sync.Mutex
	keeper   *exec.Cmd
	keeperIn io.WriteCloser
	exited   chan struct{}
	leaves   map[string]struct{}
	closed   bool
	broken   error
	respawns int
}

// NewPool creates the delegated subtree. It fails with the exact reason when
// cgroup v2, a systemd user manager, or a requested controller is not available
// to this user, after proving once that a process can be admitted into a leaf.
func NewPool(opts Options) (*Pool, error) {
	var fs unix.Statfs_t
	if err := unix.Statfs(Root, &fs); err != nil || fs.Type != unix.CGROUP2_SUPER_MAGIC {
		return nil, errors.New("cgroup v2 is not mounted at /sys/fs/cgroup")
	}
	systemdRun, err := exec.LookPath("systemd-run")
	if err != nil {
		return nil, errors.New("systemd-run is not available to create a delegated cgroup")
	}
	busctl, err := exec.LookPath("busctl")
	if err != nil {
		return nil, errors.New("busctl is not available to admit processes into their cgroup")
	}
	var suffix [6]byte
	_, _ = rand.Read(suffix[:])
	unit := fmt.Sprintf("%s-%d-%s.scope", opts.Unit, os.Getpid(), hex.EncodeToString(suffix[:]))
	keeper := exec.Command(systemdRun, "--user", "--scope", "--quiet", "--collect",
		"-p", "Delegate=yes", "--unit="+unit, "--", "/bin/sh", "-c", keeperScript, "lc-keeper", unit)
	// The keeper is a host helper: it only needs to reach the user's systemd
	// manager.
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
	exited := waitIn(keeper)
	pool := &Pool{unit: unit, prefix: opts.LeafPrefix, busctl: busctl, env: env, keeper: keeper, keeperIn: keeperIn, exited: exited, leaves: make(map[string]struct{}), supervised: make(chan struct{})}
	fail := func(err error) (*Pool, error) {
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
		if rel, err := ProcessCgroup(keeper.Process.Pid); err == nil && strings.HasSuffix(rel, "/"+unit) {
			pool.dir = filepath.Join(Root, rel)
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
	if err := WriteFile(keeperDir, "cgroup.procs", strconv.Itoa(keeper.Process.Pid)); err != nil {
		return fail(fmt.Errorf("move keeper: %w", err))
	}
	if len(opts.Controllers) > 0 {
		var enable []string
		for _, controller := range opts.Controllers {
			enable = append(enable, "+"+controller)
		}
		if err := WriteFile(pool.dir, "cgroup.subtree_control", strings.Join(enable, " ")); err != nil {
			return fail(fmt.Errorf("controllers %v are not delegated to this user: %w", opts.Controllers, err))
		}
		enabled, err := ReadFile(pool.dir, "cgroup.subtree_control")
		if err != nil {
			return fail(err)
		}
		for _, controller := range opts.Controllers {
			if !slices.Contains(strings.Fields(enabled), controller) {
				return fail(fmt.Errorf("controller %s is not enabled in the delegated scope", controller))
			}
		}
	}
	if err := pool.probeAdmission(); err != nil {
		return fail(err)
	}
	go pool.superviseKeeper()
	return pool, nil
}

// waitIn reaps cmd in the background; the channel closes when it has exited.
func waitIn(cmd *exec.Cmd) chan struct{} {
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	return exited
}

// superviseKeeper replaces a keeper that died while the pool is open, and
// fails the pool closed when it cannot.
func (p *Pool) superviseKeeper() {
	defer close(p.supervised)
	for {
		p.mu.Lock()
		exited := p.exited
		p.mu.Unlock()
		<-exited
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return
		}
		err := p.respawnKeeperLocked()
		p.mu.Unlock()
		if err != nil {
			p.failClosed(fmt.Errorf("the keeper of %s died and no new keeper could be admitted: %w", p.unit, err))
			return
		}
	}
}

// respawnKeeperLocked starts a new keeper and admits it into the keeper leaf.
// The script acts only once it is a member of the scope, so a keeper that dies
// or is orphaned before its admission never kills anything outside the scope.
func (p *Pool) respawnKeeperLocked() error {
	keeper := exec.Command("/bin/sh", "-c", keeperScript, "lc-keeper", p.unit)
	keeper.Env = p.env
	keeper.Dir = "/"
	in, err := keeper.StdinPipe()
	if err != nil {
		return err
	}
	if err := keeper.Start(); err != nil {
		return err
	}
	exited := waitIn(keeper)
	if err := p.attach("keeper", keeper.Process.Pid); err != nil {
		_ = in.Close()
		_ = keeper.Process.Kill()
		<-exited
		return err
	}
	p.keeper, p.keeperIn, p.exited = keeper, in, exited
	p.respawns++
	return nil
}

// failClosed breaks the pool: every leaf is killed and removed, and no leaf can
// be created any more.
func (p *Pool) failClosed(cause error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.broken = cause
	dirs := p.leafDirsLocked()
	p.mu.Unlock()
	for _, dir := range dirs {
		_ = (&Leaf{pool: p, dir: dir}).Remove()
	}
	_ = rmdir(filepath.Join(p.dir, "keeper"))
}

// Err reports why the pool broke, or nil while it works (or was closed by its
// owner).
func (p *Pool) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.broken
}

// KeeperRespawns counts the keepers started after the first one died.
func (p *Pool) KeeperRespawns() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.respawns
}

// Dir is the scope's directory in the cgroup hierarchy.
func (p *Pool) Dir() string { return p.dir }

// KeeperPID is the pid of the current keeper.
func (p *Pool) KeeperPID() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.keeper.Process.Pid
}

// probeAdmission proves, once, that a gated process can be admitted into a leaf.
func (p *Pool) probeAdmission() (err error) {
	leaf, err := p.NewLeaf(nil)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, leaf.Remove()) }()
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
	if err := leaf.Admit(probe.Process.Pid); err != nil {
		return fmt.Errorf("admission into the delegated cgroup failed: %w", err)
	}
	return nil
}

// attach moves pid into the sub-cgroup sub of the scope through the user's
// systemd manager and confirms the kernel agrees.
func (p *Pool) attach(sub string, pid int) error {
	cmd := exec.Command(p.busctl, "--user", "--quiet", "call",
		"org.freedesktop.systemd1", "/org/freedesktop/systemd1", "org.freedesktop.systemd1.Manager",
		"AttachProcessesToUnit", "ssau", p.unit, "/"+sub, "1", strconv.Itoa(pid))
	cmd.Env = p.env
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("AttachProcessesToUnit: %v: %s", err, strings.TrimSpace(string(out)))
	}
	rel, err := ProcessCgroup(pid)
	if err != nil {
		return err
	}
	if want := filepath.Join(p.dir, sub); filepath.Join(Root, rel) != want {
		return fmt.Errorf("process %d is in %s, not in its cgroup %s", pid, rel, want)
	}
	return nil
}

// Leaf is the cgroup of one process tree. Its settings are written before the
// tree exists. The tree's first process is started behind a gate (blocked on a
// pipe), moved into the leaf, verified to be a member, and only then released,
// so none of its code runs outside the leaf.
//
// The move goes through the user's systemd manager (AttachProcessesToUnit): an
// unprivileged host process sits in its login session's cgroup, and cgroup v2
// lets only the manager that owns the common ancestor move it into the user's
// delegated scope. CLONE_INTO_CGROUP is refused for the same reason.
type Leaf struct {
	pool *Pool
	dir  string
	name string
}

// NewLeaf creates a leaf and writes its settings, verifying each one. The
// kernel rounds memory.max down to a page, so a lower value reads back for it.
func (p *Pool) NewLeaf(settings []Setting) (*Leaf, error) {
	p.mu.Lock()
	if p.closed {
		broken := p.broken
		p.mu.Unlock()
		if broken != nil {
			return nil, fmt.Errorf("cgroup pool is broken: %w", broken)
		}
		return nil, errors.New("cgroup pool is closed")
	}
	name := fmt.Sprintf("%s%d", p.prefix, p.seq.Add(1))
	dir := filepath.Join(p.dir, name)
	p.leaves[dir] = struct{}{}
	p.mu.Unlock()
	leaf := &Leaf{pool: p, dir: dir, name: name}
	fail := func(err error) (*Leaf, error) {
		return nil, errors.Join(err, leaf.Remove())
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return fail(err)
	}
	for _, setting := range settings {
		if err := WriteFile(dir, setting.File, setting.Value); err != nil {
			return fail(err)
		}
		got, err := ReadFile(dir, setting.File)
		if err != nil {
			return fail(err)
		}
		if setting.File == "memory.max" && setting.Value != "max" {
			want, _ := strconv.ParseInt(setting.Value, 10, 64)
			if value, convErr := strconv.ParseInt(got, 10, 64); convErr != nil || value > want || value <= 0 {
				return fail(fmt.Errorf("memory.max reads %q after writing %s", got, setting.Value))
			}
			continue
		}
		if got != setting.Value {
			return fail(fmt.Errorf("%s reads %q after writing %q", setting.File, got, setting.Value))
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "cgroup.kill")); err != nil {
		return fail(errors.New("cgroup.kill is unavailable; the tree cannot be stopped atomically"))
	}
	return leaf, nil
}

// Dir is the leaf's directory in the cgroup hierarchy.
func (l *Leaf) Dir() string { return l.dir }

// Admit moves a gated process into the leaf and confirms the kernel agrees.
func (l *Leaf) Admit(pid int) error {
	return l.pool.attach(l.name, pid)
}

// Kill stops every process of the leaf at once. cgroup.kill is atomic with
// respect to forks and addresses processes by membership, never by a pid that
// could have been reused, so it reaches processes that started a new session
// or process group too, and processes in cgroups below the leaf.
func (l *Leaf) Kill() error {
	return WriteFile(l.dir, "cgroup.kill", "1")
}

// Populated reports whether any process is still in the leaf or below it.
func (l *Leaf) Populated() bool {
	events, err := ReadFile(l.dir, "cgroup.events")
	if err != nil {
		// A leaf that cannot be read cannot be certified empty.
		return !errors.Is(err, os.ErrNotExist)
	}
	return KeyedValue(events, "populated") != 0
}

// Procs lists the host pids that are still members of the leaf.
func (l *Leaf) Procs() []int {
	text, err := ReadFile(l.dir, "cgroup.procs")
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

// Read returns one interface file of the leaf, trimmed.
func (l *Leaf) Read(name string) (string, error) {
	return ReadFile(l.dir, name)
}

// Remove kills the leaf, waits for it to empty and deletes it, together with
// any cgroup a member created below it. A removed leaf proves the tree is gone:
// rmdir fails while any process remains. A leaf that cannot be removed is
// reported with its path and the reason - ErrPopulated when processes outlived
// the kill, otherwise the error of the directory that could not be deleted -
// and stays registered, so a retry and Close still see it.
func (l *Leaf) Remove() error {
	if err := rmdir(l.dir); err == nil || errors.Is(err, os.ErrNotExist) {
		l.forget()
		return nil
	}
	_ = l.Kill()
	deadline := time.Now().Add(cleanupWindow)
	for l.Populated() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if l.Populated() {
		return fmt.Errorf("cgroup %s could not be removed: %w (%v)", l.dir, ErrPopulated, l.Procs())
	}
	if err := l.removeTree(); err != nil {
		return fmt.Errorf("cgroup %s could not be removed: %w", l.dir, err)
	}
	l.forget()
	return nil
}

// RemoveIfEmpty deletes the leaf only when no process is left in it or below
// it; it never kills. It reports removed=true when the leaf is gone; removed
// false with a nil error when processes still run in it; and an error naming
// the leaf when it holds no process but could not be deleted.
func (l *Leaf) RemoveIfEmpty() (removed bool, err error) {
	if err := rmdir(l.dir); err == nil || errors.Is(err, os.ErrNotExist) {
		l.forget()
		return true, nil
	}
	if l.Populated() {
		return false, nil
	}
	if err := l.removeTree(); err != nil {
		if l.Populated() {
			// A process arrived after the check: it is a live tree, not a failure.
			return false, nil
		}
		return false, fmt.Errorf("empty cgroup %s could not be removed: %w", l.dir, err)
	}
	l.forget()
	return true, nil
}

// removeTree deletes the leaf and every cgroup below it, deepest first. Only
// directories under the leaf are touched; cgroupfs holds no symbolic links, and
// WalkDir would not follow one.
func (l *Leaf) removeTree() error {
	var dirs []string
	walkErr := filepath.WalkDir(l.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, path)
		}
		return nil
	})
	if walkErr != nil {
		return walkErr
	}
	// Deepest first: a child path is always longer than its parent's.
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	var firstErr error
	for _, dir := range dirs {
		if err := rmdir(dir); err != nil && !errors.Is(err, os.ErrNotExist) && firstErr == nil {
			firstErr = err
		}
	}
	if _, err := os.Lstat(l.dir); err == nil {
		if firstErr == nil {
			firstErr = &os.PathError{Op: "remove", Path: l.dir, Err: syscall.EBUSY}
		}
		return firstErr
	}
	return nil
}

func (l *Leaf) forget() {
	l.pool.mu.Lock()
	delete(l.pool.leaves, l.dir)
	l.pool.mu.Unlock()
}

// Live lists the leaves that still exist.
func (p *Pool) Live() []string {
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

func (p *Pool) leafDirsLocked() []string {
	dirs := make([]string, 0, len(p.leaves))
	for dir := range p.leaves {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	return dirs
}

// Close kills and removes every remaining leaf, then releases the keeper so
// systemd collects the scope. It reports every leaf that could not be removed
// and why.
func (p *Pool) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	dirs := p.leafDirsLocked()
	keeperIn, keeper, exited := p.keeperIn, p.keeper, p.exited
	p.mu.Unlock()
	var errs []error
	for _, dir := range dirs {
		if err := (&Leaf{pool: p, dir: dir}).Remove(); err != nil {
			errs = append(errs, err)
		}
	}
	_ = keeperIn.Close()
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		_ = keeper.Process.Kill()
		<-exited
	}
	<-p.supervised
	_ = rmdir(filepath.Join(p.dir, "keeper"))
	if len(errs) > 0 {
		return fmt.Errorf("%d cgroup(s) could not be removed: %w", len(errs), errors.Join(errs...))
	}
	return nil
}

// ProcessCgroup returns the cgroup v2 path of pid relative to the hierarchy root.
func ProcessCgroup(pid int) (string, error) {
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

// WriteFile writes one cgroup interface file.
func WriteFile(dir, name, value string) error {
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

// ReadFile reads one cgroup interface file, trimmed.
func ReadFile(dir, name string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, name))
	return strings.TrimSpace(string(raw)), err
}

// KeyedValue returns the value of key in a flat-keyed cgroup file.
func KeyedValue(text, key string) int64 {
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
