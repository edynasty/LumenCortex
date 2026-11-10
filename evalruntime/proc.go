package evalruntime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/edynasty/LumenCortex/internal/procrt"
	"github.com/edynasty/LumenCortex/internal/safefs"
	"github.com/edynasty/LumenCortex/internal/streambuf"
)

// Stream/output bounds. Output is bounded while it flows: retained memory is
// O(head+tail) per stream, never the size of what the cell printed.
const (
	streamChunk    = 32 << 10
	termGrace      = 2 * time.Second
	diskPoll       = 400 * time.Millisecond
	maxScanEntries = 4096
	maxScanDepth   = 8
	trackPoll      = 150 * time.Millisecond
	cleanupWindow  = 3 * time.Second
)

// Interrupt timing. They are variables only so that tests can shorten the grace
// period; nothing else changes them.
var (
	interruptGrace = 2 * time.Second
	// interruptRepeat paces a repeated cooperative interrupt; the Python
	// worker drops repeats closer than its INTERRUPT_SPACING (200ms).
	interruptRepeat = 500 * time.Millisecond
	// memoryPoll is the supervision interval of a memory bound that the OS does
	// not enforce. The overshoot a cell can reach before the kill is at most
	// what it allocates within one interval; memoryPollMS in the enforcement
	// report names it.
	memoryPoll = 200 * time.Millisecond
)

type procSpec struct {
	cmd         *exec.Cmd
	dir         string
	env         []string
	memoryBytes int64
	cpuSeconds  int
	diskBytes   int64
	diskRoot    string
	onStdout    func([]byte)
	onStderr    func([]byte)
	onLimit     func(reason string)
	// owner and enforcement are the procrt census identity and the truthful
	// resource report for this process. An empty owner is not registered.
	owner       procrt.Owner
	enforcement procrt.Enforcement
	// cgroup is the worker's own cgroup leaf (Linux). When set, the kernel bounds
	// memory and tasks, the process is admitted into the leaf behind a start
	// gate before it executes the sandbox, and stop, kill and residual checks use
	// cgroup membership instead of ps snapshots.
	cgroup      *cgroupLeaf
	launchFiles []*os.File // trusted broker namespace/seccomp, after gate fd 5
}

// process supervises one worker: its control pipes (child fds 3 and 4), its
// stdout/stderr, its process group, its resource bound, and its descendants.
//
// On Linux the worker lives in its own cgroup leaf: memory.max is a kernel hard
// limit for the whole tree, cgroup.kill stops every member atomically, and
// membership (not a pid read earlier) defines what is owned. The interpreter runs
// in the sandbox's own session, so cooperative signals go to that group (see
// workerGroup), never to the outer bwrap.
//
// Elsewhere (macOS) descendants are tracked by walking the process table (every 150ms, and
// immediately on a kernel fork event on macOS), not only by process group: a child
// that calls setsid() leaves the group but keeps its parent link, so the host
// records its pid and can terminate it. This is observation, not containment: a
// process that detaches and whose parent exits before the host records it cannot
// be observed at all, so it cannot even be reported. The host therefore reports
// every residual pid it *can* see and never claims that all descendants were
// found or that a stop was clean.
type process struct {
	cmd *exec.Cmd

	protoIn  *os.File // host -> worker (child fd 3)
	protoOut *os.File // worker -> host (child fd 4)

	pid    int
	waitCh chan struct{}
	code   atomic.Int32
	// exitSignal is the signal that killed the worker (0 when it exited
	// normally). It is recorded by the wait goroutine, never read from
	// cmd.ProcessState, which that goroutine owns.
	exitSignal atomic.Int32
	exited     atomic.Bool

	killOnce sync.Once
	stopMon  chan struct{}
	limitHit atomic.Value // string
	// limitResource and limitEnforcement say which bound stopped the worker and
	// how it was applied (hard, runtime, supervised), so a result can report
	// "limit=memory enforcement=supervised" instead of only an error string.
	limitResource    atomic.Value // string
	limitEnforcement atomic.Value // string
	// rssPeak is the highest resident set this host observed for the worker's
	// group. It is the measurement a supervised bound's overshoot is computed
	// from; 0 means it was never sampled, never that it was zero.
	rssPeak atomic.Int64
	// memoryLimit and cpuSeconds are the bounds this process started with; they
	// are what an exit is attributed to.
	memoryLimit int64
	cpuSeconds  int
	// enforcement is the report registered with procrt and attached to results.
	enforcement procrt.Enforcement
	// unregister removes this process from the procrt census.
	unregister func()

	trackMu sync.Mutex
	// tracked maps a descendant pid to its start time, so a pid that was reused
	// by an unrelated process is never signalled.
	tracked   map[int]string
	stopWatch func()

	cg          *cgroupLeaf
	onLimit     func(string)
	limitOnce   sync.Once
	groupMu     sync.Mutex
	group       *workerGroup
	groupClosed bool
	// stderrTail keeps the worker's own last words, so a startup failure reports
	// why the interpreter refused to run instead of only "channel closed".
	stderrTail *streambuf.HeadTail

	closeOnce sync.Once
	closeErr  error
	// pumps tracks the stdout/stderr readers; close waits for them so a stopped
	// worker holds no host descriptor once close returns.
	pumps     sync.WaitGroup
	pumpFiles []*os.File
}

// startFault injects a failure at a named startup stage. Tests set it; it is nil
// otherwise.
var startFault func(stage string) error

func injectStartFault(stage string) error {
	if startFault == nil {
		return nil
	}
	return startFault(stage)
}

// startProcess starts the worker command. It owns spec.cgroup until it returns:
// on any failure every descriptor it opened is closed, a started process is
// killed and reaped, and the leaf is removed before the error is returned, so a
// failed start leaves nothing behind for Dispose to find. A leaf that cannot be
// removed is reported in the error, never dropped. On success the leaf passes
// to the returned process.
func startProcess(spec procSpec) (_ *process, err error) {
	var opened []*os.File
	var cmd *exec.Cmd
	running := false
	defer func() {
		if err == nil {
			return
		}
		if running {
			// Never released, or released and failed: stop the whole tree.
			if spec.cgroup != nil {
				_ = spec.cgroup.kill()
			}
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		closeAll(opened...)
		if spec.cgroup != nil {
			if removeErr := spec.cgroup.remove(); removeErr != nil {
				err = errors.Join(err, removeErr)
			}
		}
	}()
	pipe := func(stage string) (*os.File, *os.File, error) {
		if err := injectStartFault(stage); err != nil {
			return nil, nil, err
		}
		r, w, err := os.Pipe()
		if err != nil {
			return nil, nil, err
		}
		opened = append(opened, r, w)
		return r, w, nil
	}
	inR, inW, err := pipe("pipe:control-in")
	if err != nil {
		return nil, err
	}
	outR, outW, err := pipe("pipe:control-out")
	if err != nil {
		return nil, err
	}
	stdoutR, stdoutW, err := pipe("pipe:stdout")
	if err != nil {
		return nil, err
	}
	stderrR, stderrW, err := pipe("pipe:stderr")
	if err != nil {
		return nil, err
	}

	cmd = spec.cmd
	cmd.Dir = spec.dir
	cmd.Env = spec.env
	cmd.Stdin = nil
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW
	// The child inherits fd 3 (control in) and fd 4 (control out) in this order.
	cmd.ExtraFiles = []*os.File{inR, outW}
	startProcessGroup(cmd)
	var gateR, gateW *os.File
	if spec.cgroup != nil {
		// The command runs behind a gate on fd 5: a shell that execs the sandbox
		// only after the host wrote to the pipe. If the host closes the pipe
		// instead, read fails and nothing is executed. The gate fd is closed
		// before the exec, so the sandbox never inherits it.
		gateR, gateW, err = pipe("pipe:gate")
		if err != nil {
			return nil, err
		}
		cmd.Args = append([]string{"/bin/sh", "-c", `read _ <&5 || exit 125; exec 5<&-; exec "$@"`, "lc-gate", cmd.Path}, cmd.Args[1:]...)
		cmd.Path = "/bin/sh"
		cmd.ExtraFiles = append(cmd.ExtraFiles, gateR)
	}
	cmd.ExtraFiles = append(cmd.ExtraFiles, spec.launchFiles...)

	if err := injectStartFault("start"); err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	running = true
	if spec.cgroup != nil {
		_ = gateR.Close()
		admitErr := injectStartFault("admit")
		if admitErr == nil {
			admitErr = spec.cgroup.admit(cmd.Process.Pid)
		}
		if admitErr != nil {
			// Never released: closing the gate (in the rollback) makes the shell
			// exit unexecuted.
			return nil, fmt.Errorf("resource isolation unavailable: %w", admitErr)
		}
		releaseErr := injectStartFault("release")
		if releaseErr == nil {
			_, releaseErr = gateW.Write([]byte("go\n"))
		}
		_ = gateW.Close()
		if releaseErr != nil {
			return nil, releaseErr
		}
	}
	// Parent keeps only its own ends.
	_ = inR.Close()
	_ = outW.Close()
	_ = stdoutW.Close()
	_ = stderrW.Close()

	p := &process{
		cmd:         cmd,
		protoIn:     inW,
		protoOut:    outR,
		pid:         cmd.Process.Pid,
		waitCh:      make(chan struct{}),
		stopMon:     make(chan struct{}),
		tracked:     make(map[int]string),
		stderrTail:  streambuf.New(4<<10, 4<<10),
		cg:          spec.cgroup,
		onLimit:     spec.onLimit,
		memoryLimit: spec.memoryBytes,
		cpuSeconds:  spec.cpuSeconds,
		enforcement: spec.enforcement,
	}
	p.pumpFiles = []*os.File{stdoutR, stderrR}
	p.pumps.Add(2)
	go p.pump(stdoutR, spec.onStdout)
	go p.pump(stderrR, func(chunk []byte) {
		_, _ = p.stderrTail.Write(chunk)
		if spec.onStderr != nil {
			spec.onStderr(chunk)
		}
	})
	go func() {
		_ = cmd.Wait()
		code := -1
		if cmd.ProcessState != nil {
			code = cmd.ProcessState.ExitCode()
		}
		p.code.Store(int32(code))
		if cmd.ProcessState != nil {
			if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				p.exitSignal.Store(int32(status.Signal()))
			}
		}
		if p.cg != nil {
			// Record a kernel memory kill before anyone observes the exit.
			p.noteCgroupLimit()
		}
		p.exited.Store(true)
		close(p.waitCh)
	}()
	if p.cg == nil {
		go p.trackDescendants()
		p.stopWatch = startForkWatcher(p)
		if spec.memoryBytes > 0 {
			go p.monitorMemory(spec.memoryBytes, spec.onLimit)
		}
	}
	if spec.diskBytes > 0 && spec.diskRoot != "" {
		go p.monitorDisk(spec.diskBytes, spec.diskRoot, spec.onLimit)
	}
	if spec.owner.SessionID != "" || spec.owner.Component != "" {
		// The census row exists for exactly as long as the process may. Closing
		// the process removes it, so a revoked session leaves no row behind.
		p.unregister = procrt.Default().Register(spec.owner, procHandle{p})
	}
	return p, nil
}

func (p *process) pump(r *os.File, sink func([]byte)) {
	defer p.pumps.Done()
	defer r.Close()
	buf := make([]byte, streamChunk)
	for {
		n, err := r.Read(buf)
		if n > 0 && sink != nil {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			sink(chunk)
		}
		if err != nil {
			return
		}
	}
}

func (p *process) monitorMemory(limit int64, onLimit func(string)) {
	ticker := time.NewTicker(memoryPoll)
	defer ticker.Stop()
	for {
		select {
		case <-p.stopMon:
			return
		case <-p.waitCh:
			return
		case <-ticker.C:
			rss, err := groupRSSBytes(p.pid)
			if err == nil {
				p.recordRSS(rss)
			}
			var reason string
			switch {
			case err != nil:
				// Accounting fails closed, like the disk monitor: a bound that
				// cannot be measured is not reported as holding.
				reason = fmt.Sprintf("memory accounting failed (%v); the %d byte limit cannot be certified", err, limit)
			case rss > limit:
				// The poll interval is the overshoot window: the worker can
				// allocate for at most this long after crossing the bound, so
				// the observed overshoot is measured, never assumed to be zero.
				reason = fmt.Sprintf("memory limit exceeded: observed RSS %d bytes > %d byte limit (supervised, polled every %s; overshoot %d bytes)",
					rss, limit, memoryPoll, rss-limit)
			default:
				continue
			}
			p.noteLimit("memory", procrt.Supervised, reason, onLimit)
			p.killTree()
			return
		}
	}
}

// recordRSS keeps the highest resident set this host observed.
func (p *process) recordRSS(rss int64) {
	for {
		peak := p.rssPeak.Load()
		if rss <= peak || p.rssPeak.CompareAndSwap(peak, rss) {
			return
		}
	}
}

// monitorDisk bounds the kernel's total private disk use. A per-file RLIMIT_FSIZE
// (ulimit -f) cannot bound many files, so the host sums the kernel directory on a
// short interval and terminates the worker when the aggregate exceeds the quota.
// It is a monitored bound, never claimed as a kernel hard limit.
//
// Accounting fails closed: a traversal that cannot certify the total (the entry
// or depth bound was reached, or the directory could not be read) is treated as
// an exceeded quota, never as an undercount. Counting a capped subset would make
// the bound unbounded in practice.
func (p *process) monitorDisk(limit int64, root string, onLimit func(string)) {
	ticker := time.NewTicker(diskPoll)
	defer ticker.Stop()
	for {
		select {
		case <-p.stopMon:
			return
		case <-p.waitCh:
			return
		case <-ticker.C:
			total, err := directoryBytes(root)
			var reason string
			switch {
			case err == nil && total > limit:
				reason = fmt.Sprintf("disk quota exceeded: %d > %d bytes in the kernel workspace", total, limit)
			case errors.Is(err, safefs.ErrLimit):
				reason = fmt.Sprintf("disk accounting could not cover the kernel workspace within %d entries; treating it as over the %d byte quota", maxScanEntries, limit)
			case err != nil:
				reason = fmt.Sprintf("disk accounting failed (%v); the %d byte quota cannot be certified", err, limit)
			default:
				continue
			}
			p.noteLimit("disk", procrt.Supervised, reason, onLimit)
			p.killTree()
			return
		}
	}
}

// directoryBytes uses directory descriptors; exhaustion is never an undercount.
func directoryBytes(root string) (int64, error) {
	dir, err := safefs.Open(root)
	if err != nil {
		return 0, err
	}
	defer dir.Close()
	return dir.Bytes(maxScanEntries, maxScanDepth)
}

// interrupt sends SIGINT to the worker's process group: its chance to abort the
// cell and keep its kernel. On Linux that is the sandbox's own session; when it
// cannot be identified the signal is not sent anywhere else.
func (p *process) interrupt() error {
	if p.exited.Load() {
		return nil
	}
	if p.cg != nil {
		return p.signalWorker(sigINT)
	}
	return signalGroup(p.pid, sigINT)
}

// signalWorker signals the pinned sandbox worker group (Linux). The group is
// used under groupMu, so close cannot release its pidfd mid-signal (a closed
// descriptor number could otherwise be reused for an unrelated process).
func (p *process) signalWorker(sig syscall.Signal) error {
	p.groupMu.Lock()
	defer p.groupMu.Unlock()
	group, err := p.workerGroupLocked()
	if err != nil {
		return err
	}
	return group.signal(sig)
}

// workerGroup identifies the sandbox session once and pins it.
func (p *process) workerGroup() (*workerGroup, error) {
	p.groupMu.Lock()
	defer p.groupMu.Unlock()
	return p.workerGroupLocked()
}

// workerGroupLocked waits briefly for a freshly started worker to create its
// session. After close it fails instead of pinning a new group.
func (p *process) workerGroupLocked() (*workerGroup, error) {
	if p.group != nil {
		return p.group, nil
	}
	if p.groupClosed {
		return nil, errors.New("eval: the worker is closed")
	}
	deadline := time.Now().Add(time.Second)
	for {
		group, err := findWorkerGroup(p.cg, p.pid)
		if err == nil {
			p.group = group
			return group, nil
		}
		if p.exited.Load() || time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// noteCgroupLimit turns a kernel OOM kill of the worker's cgroup into the
// worker's limit reason and event, exactly once.
func (p *process) noteCgroupLimit() {
	kills := p.cg.oomKills()
	if kills == 0 {
		return
	}
	// record directly: noteLimit takes the same sync.Once, and Once.Do is not
	// re-entrant (calling it here would deadlock the wait goroutine).
	p.limitOnce.Do(func() {
		reason := fmt.Sprintf("memory limit exceeded: the worker cgroup reached memory.max=%s bytes and the kernel killed it (oom_kill=%d)", p.cg.memoryMax(), kills)
		p.recordLimit("memory", procrt.Hard, reason)
	})
}

// noteLimit records the bound that stopped the worker exactly once, so a later
// observer cannot relabel an OOM kill as a crash.
func (p *process) noteLimit(resource, enforcement, reason string, onLimit func(string)) {
	if reason == "" {
		return
	}
	fired := false
	p.limitOnce.Do(func() {
		fired = true
		p.recordLimit(resource, enforcement, reason)
	})
	if fired && onLimit != nil {
		onLimit(reason)
	}
}

// recordLimit stores the attribution and emits the event. Callers must already
// hold limitOnce.
func (p *process) recordLimit(resource, enforcement, reason string) {
	p.limitHit.Store(reason)
	p.limitResource.Store(resource)
	p.limitEnforcement.Store(enforcement)
	if p.onLimit != nil {
		p.onLimit(reason)
	}
}

// detectCrashLimit attributes an exit to a bound the host did not observe
// directly: a V8 heap exhaustion (interpreter-enforced) and SIGXCPU
// (RLIMIT_CPU, kernel-enforced). It runs after the process is reaped and before
// the result is built, so the attribution is never invented after the fact.
func (p *process) detectCrashLimit() {
	if p == nil {
		return
	}
	if p.cg != nil {
		p.noteCgroupLimit()
	}
	if reason, ok := p.limitHit.Load().(string); ok && reason != "" {
		return
	}
	if syscall.Signal(p.exitSignal.Load()) == syscall.SIGXCPU {
		p.noteLimit("cpu", procrt.Hard,
			fmt.Sprintf("cpu limit exceeded: RLIMIT_CPU killed the worker with SIGXCPU (hard, kernel-enforced; %d s)", p.cpuSeconds), p.onLimit)
		return
	}
	if p.memoryLimit > 0 && p.enforcement.MemoryRuntimeCapBytes > 0 {
		stderr := p.stderrText()
		if strings.Contains(stderr, "JavaScript heap out of memory") || strings.Contains(stderr, "Reached heap limit") {
			p.noteLimit("memory", procrt.Runtime,
				fmt.Sprintf("memory limit exceeded: V8 reached its %d byte heap cap (--max-old-space-size, interpreter-enforced) and aborted the worker", p.enforcement.MemoryRuntimeCapBytes), p.onLimit)
		}
	}
}

// limitReason is the resource limit that stopped the worker, or "".
func (p *process) limitReason() string {
	p.detectCrashLimit()
	if reason, ok := p.limitHit.Load().(string); ok {
		return reason
	}
	return ""
}

// limitResourceName is the resource ("memory", "cpu", "disk") a bound stopped
// the worker on, or "".
func (p *process) limitResourceName() string {
	p.detectCrashLimit()
	if resource, ok := p.limitResource.Load().(string); ok {
		return resource
	}
	return ""
}

// limitEnforcementLabel is how that bound was applied (hard, runtime,
// supervised), or "".
func (p *process) limitEnforcementLabel() string {
	p.detectCrashLimit()
	if label, ok := p.limitEnforcement.Load().(string); ok {
		return label
	}
	return ""
}

// memoryPeak is the worker's memory high-water mark in bytes as the kernel
// accounts it (Linux cgroup memory.peak), or 0 when it is not measured.
func (p *process) memoryPeak() int64 {
	if p.cg != nil {
		return p.cg.memoryPeak()
	}
	// On a host without cgroups this is the highest RSS the supervisor sampled.
	// 0 means "not measured", never a zero-resident-set claim.
	return p.rssPeak.Load()
}

// terminate asks the group to exit (SIGTERM), then kills the group and every
// tracked descendant. It returns the pids that survived the cleanup window; the
// caller must not report a clean stop when that list is not empty.
func (p *process) terminate(ctx context.Context) []int {
	if !p.exited.Load() {
		if p.cg == nil || p.signalWorker(sigTERM) != nil {
			// Terminating the outer bwrap also ends the sandbox: acceptable for
			// a stop, never for an interrupt.
			_ = signalGroup(p.pid, sigTERM)
		}
		select {
		case <-p.waitCh:
		case <-time.After(termGrace):
		case <-ctx.Done():
		}
	}
	return p.killTree()
}

// killTree kills the group (while its leader is alive) and every descendant seen
// by the tracker, then waits for the tree to disappear.
func (p *process) killTree() []int {
	if p.cg != nil {
		return p.killCgroup()
	}
	if !p.exited.Load() {
		p.kill()
	}
	p.killTracked()
	deadline := time.Now().Add(cleanupWindow)
	for {
		remaining := p.residual()
		if len(remaining) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return remaining
		}
		for _, pid := range remaining {
			_ = signalPID(pid, sigKILL)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// killCgroup kills every member of the worker's cgroup and waits until the
// kernel reports it empty. Survivors are reported by host pid.
func (p *process) killCgroup() []int {
	deadline := time.Now().Add(cleanupWindow)
	for {
		_ = p.cg.kill()
		if !p.cg.populated() {
			return nil
		}
		if time.Now().After(deadline) {
			return p.cg.procs()
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (p *process) kill() {
	p.killOnce.Do(func() {
		_ = signalGroup(p.pid, sigKILL)
	})
}

func (p *process) trackedSnapshot() []int {
	p.trackMu.Lock()
	defer p.trackMu.Unlock()
	out := make([]int, 0, len(p.tracked))
	for pid := range p.tracked {
		out = append(out, pid)
	}
	return out
}

// killTracked kills the recorded descendants that are still the processes that
// were recorded: a pid whose start time changed now belongs to someone else.
func (p *process) killTracked() {
	table, err := processTable()
	if err != nil {
		return
	}
	for _, pid := range p.liveTracked(table) {
		_ = signalPID(pid, sigKILL)
	}
}

// liveTracked lists tracked descendants present in table with the start time
// they were recorded with.
func (p *process) liveTracked(table []procEntry) []int {
	starts := make(map[int]string, len(table))
	for _, entry := range table {
		starts[entry.pid] = entry.start
	}
	p.trackMu.Lock()
	defer p.trackMu.Unlock()
	var out []int
	for pid, start := range p.tracked {
		if current, ok := starts[pid]; ok && current == start && pid != os.Getpid() {
			out = append(out, pid)
		}
	}
	return out
}

// trackDescendants keeps the tracked set current while the worker lives, and
// takes one final sample after it exits (a child may outlive its parent).
func (p *process) trackDescendants() {
	ticker := time.NewTicker(trackPoll)
	defer ticker.Stop()
	for {
		select {
		case <-p.stopMon:
			return
		case <-p.waitCh:
			p.scanDescendants()
			return
		case <-ticker.C:
			p.scanDescendants()
		}
	}
}

func (p *process) scanDescendants() {
	table, err := processTable()
	if err != nil {
		return
	}
	children := make(map[int][]int, len(table))
	starts := make(map[int]string, len(table))
	for _, entry := range table {
		children[entry.ppid] = append(children[entry.ppid], entry.pid)
		starts[entry.pid] = entry.start
	}
	p.trackMu.Lock()
	defer p.trackMu.Unlock()
	queue := []int{p.pid}
	seen := map[int]bool{p.pid: true}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		for _, child := range children[pid] {
			if seen[child] {
				continue
			}
			seen[child] = true
			p.tracked[child] = starts[child]
			queue = append(queue, child)
		}
	}
	for _, entry := range table {
		if entry.pgid == p.pid && entry.pid != p.pid {
			p.tracked[entry.pid] = entry.start
		}
	}
}

// residual lists tracked descendants that still exist, plus any live process of
// the worker's group.
func (p *process) residual() []int {
	table, err := processTable()
	if err != nil {
		return nil
	}
	seen := make(map[int]struct{})
	for _, pid := range p.liveTracked(table) {
		seen[pid] = struct{}{}
	}
	for _, entry := range table {
		if entry.pgid == p.pid && entry.pid != p.pid && entry.pid != os.Getpid() {
			seen[entry.pid] = struct{}{}
		}
	}
	out := make([]int, 0, len(seen))
	for pid := range seen {
		out = append(out, pid)
	}
	sort.Ints(out)
	return out
}

// stderrText returns the bounded tail of the worker's stderr.
func (p *process) stderrText() string {
	if p == nil || p.stderrTail == nil {
		return ""
	}
	snap := p.stderrTail.Snapshot()
	text := string(snap.Head) + string(snap.Tail)
	return strings.TrimSpace(text)
}

// wait blocks until the worker process is reaped or ctx ends.
func (p *process) wait(ctx context.Context) error {
	select {
	case <-p.waitCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *process) exitCode() int {
	return int(p.code.Load())
}

// close releases the process's host resources. It reports a worker cgroup that
// could not be removed; every call returns the outcome of the first.
func (p *process) close() error {
	p.closeOnce.Do(func() {
		if p.unregister != nil {
			p.unregister()
			p.unregister = nil
		}
		close(p.stopMon)
		if p.stopWatch != nil {
			p.stopWatch()
		}
		p.groupMu.Lock()
		p.group.close()
		p.group = nil
		p.groupClosed = true
		p.groupMu.Unlock()
		if p.cg != nil {
			// rmdir only succeeds on an empty leaf; an occupied one is killed.
			p.closeErr = p.cg.remove()
		}
		if p.protoIn != nil {
			_ = p.protoIn.Close()
		}
		if p.protoOut != nil {
			_ = p.protoOut.Close()
		}
		// The tree is gone, so the output pipes reach EOF at once. A writer the
		// host could not stop keeps them open; they are closed under it then.
		drained := make(chan struct{})
		go func() {
			p.pumps.Wait()
			close(drained)
		}()
		select {
		case <-drained:
		case <-time.After(cleanupWindow):
			closeAll(p.pumpFiles...)
			<-drained
		}
	})
	return p.closeErr
}

func (p *process) cpuMillis() int64 {
	if p.cg != nil {
		// The whole tree's CPU, kept by the kernel even after the exit.
		return p.cg.cpuMillis()
	}
	if p.exited.Load() {
		return 0
	}
	return processCPUMillis(p.pid)
}

func closeAll(files ...*os.File) {
	for _, f := range files {
		if f != nil {
			_ = f.Close()
		}
	}
}

// procHandle presents a supervised worker to the procrt census. It is a thin
// adapter: the supervision itself (groups, cgroup, descendant tracking, cleanup
// window) stays in this package, and procrt only decides the escalation order.
type procHandle struct{ p *process }

func (h procHandle) PID() int { return h.p.pid }

func (h procHandle) TreePIDs() []int {
	if h.p.cg != nil {
		return h.p.cg.procs()
	}
	return h.p.trackedSnapshot()
}

func (h procHandle) State() string {
	if h.p.exited.Load() {
		return procrt.StateExited
	}
	return procrt.StateRunning
}

func (h procHandle) Usage() procrt.Usage {
	return procrt.Usage{
		CPUMs:           h.p.cpuMillis(),
		MemoryPeakBytes: h.p.memoryPeak(),
	}
}

func (h procHandle) Enforcement() procrt.Enforcement { return h.p.enforcement }

func (h procHandle) Interrupt() error { return h.p.interrupt() }

func (h procHandle) Terminate(ctx context.Context) []int { return h.p.terminate(ctx) }

func (h procHandle) Kill() []int { return h.p.killTree() }

func (h procHandle) Done() <-chan struct{} { return h.p.waitCh }

type procEntry struct {
	pid  int
	ppid int
	pgid int
	// start is the process start time as ps prints it; with the pid it
	// identifies one process, so a reused pid is not mistaken for a descendant.
	start string
}

func processTable() ([]procEntry, error) {
	out, err := exec.Command("/bin/ps", "-axo", "pid=,ppid=,pgid=,lstart=").Output()
	if err != nil {
		return nil, err
	}
	entries := make([]procEntry, 0, 128)
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		pgid, err3 := strconv.Atoi(fields[2])
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		entries = append(entries, procEntry{pid: pid, ppid: ppid, pgid: pgid, start: strings.Join(fields[3:], " ")})
	}
	return entries, nil
}

// groupRSSBytes sums the resident set of the worker's process group, in bytes.
func groupRSSBytes(pgid int) (int64, error) {
	out, err := exec.Command("/bin/ps", "-axo", "pgid=,rss=").Output()
	if err != nil {
		return 0, err
	}
	var total int64
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		group, err := strconv.Atoi(fields[0])
		if err != nil || group != pgid {
			continue
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		total += kb * 1024
	}
	return total, nil
}

// processCPUMillis reads the worker's cumulative CPU time.
func processCPUMillis(pid int) int64 {
	out, err := exec.Command("/bin/ps", "-o", "time=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0
	}
	return parseCPUTime(strings.TrimSpace(string(out)))
}

func parseCPUTime(text string) int64 {
	if text == "" {
		return 0
	}
	parts := strings.Split(text, ":")
	var seconds float64
	for _, part := range parts {
		value, err := strconv.ParseFloat(part, 64)
		if err != nil {
			return 0
		}
		seconds = seconds*60 + value
	}
	return int64(seconds * 1000)
}
