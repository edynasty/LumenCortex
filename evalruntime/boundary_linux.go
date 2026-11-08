//go:build linux

package evalruntime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/edynasty/LumenCortex/evalapi"
	"golang.org/x/sys/unix"
)

// Inspection never executes a command in the sandbox. /proc observations are
// bracketed by pidfd/starttime checks and pinned to the manager's worker leaf.
func (m *Manager) inspectBoundary(ctx context.Context, w *worker, binding evalapi.BoundaryContext) evalapi.BoundaryReport {
	r := newBoundaryReport(binding)
	set := func(name string, ok bool, evidence string) {
		status := evalapi.BoundaryFail
		if ok {
			status = evalapi.BoundaryPass
		}
		r.Checks[name] = evalapi.BoundaryCheck{Status: status, Evidence: evidence}
	}
	if ctx.Err() != nil {
		r.Decide()
		return r
	}
	p := w.proc
	if p == nil || p.cg == nil {
		set("cgroup", false, "worker has no managed cgroup")
		r.Decide()
		return r
	}
	p.groupMu.Lock()
	defer p.groupMu.Unlock()
	group, err := p.workerGroupLocked()
	if err != nil {
		set("identity", false, "owned process group unavailable")
		r.Decide()
		return r
	}
	// Select the actual interpreter, not the outer bwrap or the namespace init.
	exe, err := os.Stat(w.s.manifest.Executable)
	if err != nil {
		r.Decide()
		return r
	}
	var id hostIdentity
	for _, pid := range p.cg.procs() {
		if pid == p.pid || pid == group.id.pid {
			continue
		}
		_, pgid, start, e := procStat(pid)
		info, statErr := os.Stat(fmt.Sprintf("/proc/%d/exe", pid))
		if e == nil && statErr == nil && pgid == group.id.pid && os.SameFile(exe, info) {
			if id.pid != 0 {
				set("identity", false, "multiple interpreter candidates")
				r.Decide()
				return r
			}
			id = hostIdentity{pid: pid, start: start}
		}
	}
	if id.pid == 0 {
		set("identity", false, "owned interpreter not found")
		r.Decide()
		return r
	}
	fd, err := unix.PidfdOpen(id.pid, 0)
	if err != nil {
		r.Decide()
		return r
	}
	defer unix.Close(fd)
	if !id.alive() || unix.PidfdSendSignal(fd, 0, nil, 0) != nil {
		set("identity", false, "interpreter exited")
		r.Decide()
		return r
	}
	root := fmt.Sprintf("/proc/%d", id.pid)
	r.Process = evalapi.BoundaryProcess{HostPID: id.pid, Starttime: id.start, Generation: w.generation, UID: -1, GID: -1, Namespaces: make(map[string]string)}
	set("identity", true, "host-selected interpreter; pidfd and starttime match")
	w.s.mu.Lock()
	same := w.s.worker == w && w.s.generation == w.generation && !w.s.closed
	w.s.mu.Unlock()
	set("generation", same, "manager worker/generation comparison")
	cg, err := boundaryRead(root + "/cgroup")
	if err == nil {
		expected := "0::" + strings.TrimPrefix(p.cg.Dir(), "/sys/fs/cgroup")
		r.Process.Cgroup = p.cg.Dir()
		good := strings.TrimSpace(cg) == expected
		for _, setting := range []struct{ name, value string }{{"memory.max", strconv.FormatInt(w.s.budget.MemoryBytes, 10)}, {"memory.swap.max", "0"}, {"memory.oom.group", "1"}, {"cpu.max", "100000 100000"}, {"pids.max", strconv.Itoa(workerTasksMax)}} {
			actual, e := p.cg.Read(setting.name)
			good = good && e == nil && actual == setting.value
		}
		set("cgroup", good, "membership and memory/swap/OOM/CPU/pids limits read from owned leaf")
	}
	status, err := boundaryRead(root + "/status")
	if err == nil {
		fields := boundaryFields(status)
		uid, uerr := strconv.Atoi(firstBoundaryField(fields["Uid"]))
		gid, gerr := strconv.Atoi(firstBoundaryField(fields["Gid"]))
		r.Process.UID, r.Process.GID = uid, gid
		set("credentials", uerr == nil && gerr == nil && uid != 0 && fields["NoNewPrivs"] == "1" && fields["CapEff"] == "0000000000000000", "host UID/GID, NoNewPrivs and effective capabilities observed")
		// A path/group restriction alone cannot distinguish an untrusted host shell
		// that carries the same identity. This is deliberately not a broker PASS.
		set("callerIsolation", uerr == nil && uid != os.Geteuid(), "worker must not carry the trusted host UID")
	}
	for _, name := range []string{"user", "pid", "mnt", "net", "ipc", "uts"} {
		observed, e := os.Readlink(root + "/ns/" + name)
		host, he := os.Readlink("/proc/self/ns/" + name)
		if e == nil && he == nil {
			r.Process.Namespaces[name] = observed
			set(name, observed != host, "worker="+observed+" host="+host)
		}
	}
	set("networkPolicy", !m.opts.Sandbox.AllowNetwork && r.Checks["net"].Status == evalapi.BoundaryPass, "deny-network policy requires distinct network namespace")
	if mounts, e := boundaryRead(root + "/mountinfo"); e == nil {
		err = checkBoundaryMounts(m, w, root, mounts)
		evidence := "mount destinations, access modes and host source identities match launch policy"
		if err != nil {
			evidence = err.Error()
		}
		set("mounts", err == nil, evidence)
	}
	if sockets, e := boundaryRead(root + "/net/unix"); e == nil {
		// All pathname sockets seen in this namespace must remain below the kernel's
		// private data root. Abstract sockets are private to the verified netns.
		good := true
		for i, line := range strings.Split(sockets, "\n") {
			if i == 0 {
				continue
			}
			f := strings.Fields(line)
			if len(f) < 8 {
				continue
			}
			path := f[7]
			if !strings.HasPrefix(path, "@") && !boundaryUnder(path, w.s.kernel) {
				good = false
			}
		}
		set("sockets", good && r.Checks["mounts"].Status == evalapi.BoundaryPass, "private network namespace and mounted pathname socket exposure")
	}
	if label, e := boundaryRead(root + "/attr/current"); e == nil {
		set("apparmor", strings.TrimSpace(label) != "" && strings.TrimSpace(label) != "unconfined", strings.TrimSpace(label))
	}
	if actual, e := os.Stat(fmt.Sprintf("/proc/%d/exe", p.pid)); e == nil {
		configured, se := os.Stat(m.launcher.exe)
		trusted := checkTrustedExecutable(m.launcher.exe)
		set("bubblewrap", se == nil && os.SameFile(actual, configured) && trusted == nil, "outer executable inode and trusted root-owned path checked")
	}
	if e := checkCallerIsolation(m.launcher.exe); e != nil {
		set("callerIsolation", false, e.Error())
	}
	if ctx.Err() != nil || !id.alive() || unix.PidfdSendSignal(fd, 0, nil, 0) != nil || !slices.Contains(p.cg.procs(), id.pid) {
		set("identity", false, "process changed or observation cancelled")
	}
	r.Decide()
	return r
}

func boundaryRead(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if len(b) > 1<<20 {
		return "", errors.New("boundary observation exceeds limit")
	}
	return strings.TrimSpace(string(b)), err
}
func boundaryFields(text string) map[string]string {
	out := make(map[string]string)
	for _, line := range strings.Split(text, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if ok {
			out[k] = strings.TrimSpace(v)
		}
	}
	return out
}
func firstBoundaryField(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}
func boundaryUnder(path, root string) bool { return path == root || strings.HasPrefix(path, root+"/") }
func boundaryMountPath(s string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(s)
}

func checkBoundaryMounts(m *Manager, w *worker, procRoot, text string) error {
	// A bind is accepted only at an exact host-approved destination. Every source
	// inode is checked through /proc/PID/root, never trusted from mountinfo text.
	binds := map[string]bool{w.s.kernel: false, m.workersDir: true, w.s.manifest.Executable: true}
	for _, path := range []string{"/usr", "/bin", "/sbin", "/lib", "/lib64", "/etc/alternatives", "/etc/matplotlibrc"} {
		binds[path] = true
	}
	for _, path := range m.opts.Sandbox.ExtraReadPaths {
		binds[path] = true
	}
	for _, path := range w.s.readPaths {
		binds[path] = true
	}
	seen := make(map[string]bool)
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		sep := slices.Index(f, "-")
		if len(f) < 7 || sep < 6 || len(f) <= sep+2 {
			return errors.New("malformed mount observation")
		}
		path, fs := boundaryMountPath(f[4]), f[sep+1]
		if readOnly, ok := binds[path]; ok {
			if readOnly && !slices.Contains(strings.Split(f[5], ","), "ro") {
				return fmt.Errorf("writable read-only bind: %s", path)
			}
			actual, e := os.Stat(procRoot + "/root" + path)
			source, se := os.Stat(path)
			if e != nil || se != nil || !os.SameFile(actual, source) {
				return fmt.Errorf("bind source identity unavailable or changed: %s", path)
			}
			// A socket bind gives access even when mounted read-only.
			if actual.Mode()&os.ModeSocket != 0 {
				return fmt.Errorf("host socket bind: %s", path)
			}
			seen[path] = true
			continue
		}
		if fs == "tmpfs" && slices.Contains([]string{"/", "/tmp", "/run", "/dev", "/dev/shm"}, path) {
			continue
		}
		if path == "/proc" && fs == "proc" {
			continue
		}
		if path == "/dev/pts" && fs == "devpts" {
			continue
		}
		if slices.Contains([]string{"/dev/null", "/dev/zero", "/dev/full", "/dev/random", "/dev/urandom", "/dev/tty"}, path) {
			info, e := os.Stat(procRoot + "/root" + path)
			host, he := os.Stat(path)
			if e == nil && he == nil && info.Mode()&os.ModeDevice != 0 {
				a, aok := info.Sys().(*syscall.Stat_t)
				b, bok := host.Sys().(*syscall.Stat_t)
				if aok && bok && a.Rdev == b.Rdev {
					continue
				}
			}
		}
		return fmt.Errorf("unexpected mount: %s (%s)", path, fs)
	}
	if !seen[w.s.kernel] || !seen[m.workersDir] {
		return errors.New("required private workspace or worker mount absent")
	}
	// Extra directory binds may contain AF_UNIX sockets despite a read-only mount.
	// Bound the walk: inability to fully observe is refusal, never a guessed PASS.
	for _, path := range append(slices.Clone(m.opts.Sandbox.ExtraReadPaths), w.s.readPaths...) {
		count := 0
		if err := filepath.WalkDir(path, func(_ string, entry os.DirEntry, e error) error {
			if e != nil {
				return e
			}
			count++
			if count > 16384 {
				return errors.New("bind socket inspection exceeds limit")
			}
			if entry.Type()&os.ModeSocket != 0 {
				return errors.New("host socket below runtime bind")
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}
