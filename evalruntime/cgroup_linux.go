//go:build linux

package evalruntime

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/edynasty/LumenCortex/internal/cgroupv2"
)

// cgroupPool is the delegated cgroup v2 subtree this manager owns on Linux. Every
// worker gets its own leaf below it, so its memory, task count and process tree
// are bounded and observed by the kernel, not reconstructed from ps snapshots.
// When the host dies without closing the pool, the pool's keeper kills every
// worker leaf.
type cgroupPool struct{ *cgroupv2.Pool }

// newCgroupPool creates the delegated subtree. It fails with the exact reason
// when cgroup v2, a systemd user manager, or the memory/pids controllers are not
// available to this user; the caller then refuses to run workers rather than
// pretending a memory limit exists.
func newCgroupPool() (*cgroupPool, error) {
	pool, err := cgroupv2.NewPool(cgroupv2.Options{Unit: "lc-evalruntime", LeafPrefix: "w", Controllers: []string{"memory", "pids"}})
	if err != nil {
		return nil, err
	}
	return &cgroupPool{pool}, nil
}

// cgroupLeaf is one worker's cgroup. Its limits are written before the worker
// exists; startProcess admits the gated worker into it before releasing it, so
// no instruction of the worker runs outside the bound.
type cgroupLeaf struct{ *cgroupv2.Leaf }

func (p *cgroupPool) newLeaf(memoryBytes int64) (*cgroupLeaf, error) {
	memory := "max"
	if memoryBytes > 0 {
		memory = strconv.FormatInt(memoryBytes, 10)
	}
	leaf, err := p.NewLeaf([]cgroupv2.Setting{
		{File: "memory.max", Value: memory},
		{File: "memory.swap.max", Value: "0"},
		// One OOM kill stops the whole worker: a kernel whose interpreter
		// survived while a helper was killed (or the reverse) has unknown state.
		{File: "memory.oom.group", Value: "1"},
		{File: "pids.max", Value: strconv.Itoa(workerTasksMax)},
	})
	if err != nil {
		return nil, fmt.Errorf("worker cgroup: %w", err)
	}
	if err := injectStartFault("cgroup"); err != nil {
		return nil, errors.Join(fmt.Errorf("worker cgroup: %w", err), leaf.Remove())
	}
	return &cgroupLeaf{leaf}, nil
}

func (p *cgroupPool) liveLeaves() []string { return p.Live() }
func (p *cgroupPool) close() error         { return p.Close() }

func (l *cgroupLeaf) admit(pid int) error { return l.Admit(pid) }
func (l *cgroupLeaf) kill() error         { return l.Kill() }
func (l *cgroupLeaf) populated() bool     { return l.Populated() }
func (l *cgroupLeaf) procs() []int        { return l.Procs() }
func (l *cgroupLeaf) remove() error       { return l.Remove() }

func (l *cgroupLeaf) memoryPeak() int64 {
	text, err := l.Read("memory.peak")
	if err != nil {
		return 0
	}
	value, _ := strconv.ParseInt(text, 10, 64)
	return value
}

func (l *cgroupLeaf) memoryMax() string {
	text, _ := l.Read("memory.max")
	return text
}

// oomKills is the kernel's count of processes killed because the leaf reached
// memory.max.
func (l *cgroupLeaf) oomKills() int64 {
	events, err := l.Read("memory.events")
	if err != nil {
		return 0
	}
	return cgroupv2.KeyedValue(events, "oom_kill")
}

func (l *cgroupLeaf) cpuMillis() int64 {
	stat, err := l.Read("cpu.stat")
	if err != nil {
		return 0
	}
	return cgroupv2.KeyedValue(stat, "usage_usec") / 1000
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
	leaf  *cgroupLeaf
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
		return &workerGroup{id: hostIdentity{pid: pid, start: start}, pidfd: fd, leaf: leaf}, nil
	}
	return nil, errors.New("eval: the sandboxed worker's process group could not be identified")
}

// signal pins each current member before delivery. No signal uses a numeric
// PGID after an identity check: exit/reuse between check and send is harmless.
// Forks may miss one cooperative signal; the host repeats it, then cgroup.kill
// atomically stops the complete tree if cooperation fails.
func (g *workerGroup) signal(sig syscall.Signal) error {
	if err := unix.PidfdSendSignal(g.pidfd, 0, nil, 0); err != nil {
		return fmt.Errorf("eval: worker group leader is gone: %w", err)
	}
	var errs []error
	for _, pid := range g.leaf.procs() {
		_, pgid, start, err := procStat(pid)
		if err != nil || pgid != g.id.pid {
			continue
		}
		fd, err := unix.PidfdOpen(pid, 0)
		if err != nil {
			if !errors.Is(err, syscall.ESRCH) {
				errs = append(errs, err)
			}
			continue
		}
		_, afterGroup, afterStart, checkErr := procStat(pid)
		rel, memberErr := cgroupv2.ProcessCgroup(pid)
		if checkErr == nil && memberErr == nil && afterStart == start && afterGroup == g.id.pid && cgroupv2.Root+rel == g.leaf.Dir() {
			if err := unix.PidfdSendSignal(fd, unix.Signal(sig), nil, 0); err != nil && !errors.Is(err, syscall.ESRCH) {
				errs = append(errs, err)
			}
		}
		_ = unix.Close(fd)
	}
	return errors.Join(errs...)
}

func (g *workerGroup) close() {
	if g != nil && g.pidfd >= 0 {
		_ = unix.Close(g.pidfd)
		g.pidfd = -1
	}
}

// cgroupRequired: Linux workers never start without their own cgroup.
const cgroupRequired = true
