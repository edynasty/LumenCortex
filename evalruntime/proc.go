package evalruntime

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/edynasty/LumenCortex/internal/streambuf"
)

// Stream/output bounds. Output is bounded while it flows: retained memory is
// O(head+tail) per stream, never the size of what the cell printed.
const (
	streamChunk    = 32 << 10
	termGrace      = 2 * time.Second
	interruptGrace = 2 * time.Second
	memoryPoll     = 200 * time.Millisecond
	diskPoll       = 400 * time.Millisecond
	maxScanEntries = 4096
	maxScanDepth   = 8
	trackPoll      = 150 * time.Millisecond
	cleanupWindow  = 3 * time.Second
)

type procSpec struct {
	cmd         *exec.Cmd
	dir         string
	env         []string
	memoryBytes int64
	diskBytes   int64
	diskRoot    string
	onStdout    func([]byte)
	onStderr    func([]byte)
	onLimit     func(reason string)
}

// process supervises one worker: its control pipes (child fds 3 and 4), its
// stdout/stderr, its process group, a polled memory bound, and a descendant set.
//
// Descendants are tracked by walking the process table (every 150ms, and
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
	exited atomic.Bool

	killOnce sync.Once
	stopMon  chan struct{}
	limitHit atomic.Value // string

	trackMu   sync.Mutex
	tracked   map[int]struct{}
	stopWatch func()
	// stderrTail keeps the worker's own last words, so a startup failure reports
	// why the interpreter refused to run instead of only "channel closed".
	stderrTail *streambuf.HeadTail

	closeOnce sync.Once
}

func startProcess(spec procSpec) (*process, error) {
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		closeAll(inR, inW)
		return nil, err
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		closeAll(inR, inW, outR, outW)
		return nil, err
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		closeAll(inR, inW, outR, outW, stdoutR, stdoutW)
		return nil, err
	}

	cmd := spec.cmd
	cmd.Dir = spec.dir
	cmd.Env = spec.env
	cmd.Stdin = nil
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW
	// The child inherits fd 3 (control in) and fd 4 (control out) in this order.
	cmd.ExtraFiles = []*os.File{inR, outW}
	startProcessGroup(cmd)

	if err := cmd.Start(); err != nil {
		closeAll(inR, inW, outR, outW, stdoutR, stdoutW, stderrR, stderrW)
		return nil, err
	}
	// Parent keeps only its own ends.
	_ = inR.Close()
	_ = outW.Close()
	_ = stdoutW.Close()
	_ = stderrW.Close()

	p := &process{
		cmd:        cmd,
		protoIn:    inW,
		protoOut:   outR,
		pid:        cmd.Process.Pid,
		waitCh:     make(chan struct{}),
		stopMon:    make(chan struct{}),
		tracked:    make(map[int]struct{}),
		stderrTail: streambuf.New(4<<10, 4<<10),
	}
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
		p.exited.Store(true)
		close(p.waitCh)
	}()
	go p.trackDescendants()
	p.stopWatch = startForkWatcher(p)
	if spec.memoryBytes > 0 {
		go p.monitorMemory(spec.memoryBytes, spec.onLimit)
	}
	if spec.diskBytes > 0 && spec.diskRoot != "" {
		go p.monitorDisk(spec.diskBytes, spec.diskRoot, spec.onLimit)
	}
	return p, nil
}

func (p *process) pump(r *os.File, sink func([]byte)) {
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
			if err != nil || rss <= limit {
				continue
			}
			reason := fmt.Sprintf("memory limit exceeded: %d > %d bytes", rss, limit)
			p.limitHit.Store(reason)
			if onLimit != nil {
				onLimit(reason)
			}
			p.killTree()
			return
		}
	}
}

// monitorDisk bounds the kernel's total private disk use. A per-file RLIMIT_FSIZE
// (ulimit -f) cannot bound many files, so the host sums the kernel directory on a
// short interval and terminates the worker when the aggregate exceeds the quota.
// It is a monitored bound, never claimed as a kernel hard limit.
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
			if err != nil || total <= limit {
				continue
			}
			reason := fmt.Sprintf("disk quota exceeded: %d > %d bytes in the kernel workspace", total, limit)
			p.limitHit.Store(reason)
			if onLimit != nil {
				onLimit(reason)
			}
			p.killTree()
			return
		}
	}
}

// directoryBytes sums regular-file sizes under root with bounded traversal: at
// most maxScanEntries entries and maxScanDepth levels, symlinks are not followed.
func directoryBytes(root string) (int64, error) {
	var total int64
	entries := 0
	var walk func(dir string, depth int) error
	walk = func(dir string, depth int) error {
		if depth > maxScanDepth {
			return nil
		}
		names, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, name := range names {
			entries++
			if entries > maxScanEntries {
				return nil
			}
			full := filepath.Join(dir, name.Name())
			info, err := os.Lstat(full)
			if err != nil {
				continue
			}
			switch {
			case info.Mode().IsRegular():
				total += info.Size()
			case info.IsDir():
				if err := walk(full, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(root, 0); err != nil {
		return total, err
	}
	return total, nil
}

// interrupt sends SIGINT to the process group: the worker's chance to abort the
// cell and keep its kernel.
func (p *process) interrupt() error {
	if p.exited.Load() {
		return nil
	}
	return signalGroup(p.pid, sigINT)
}

// terminate asks the group to exit (SIGTERM), then kills the group and every
// tracked descendant. It returns the pids that survived the cleanup window; the
// caller must not report a clean stop when that list is not empty.
func (p *process) terminate(ctx context.Context) []int {
	if !p.exited.Load() {
		_ = signalGroup(p.pid, sigTERM)
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

func (p *process) killTracked() {
	p.trackMu.Lock()
	pids := make([]int, 0, len(p.tracked))
	for pid := range p.tracked {
		pids = append(pids, pid)
	}
	p.trackMu.Unlock()
	for _, pid := range pids {
		if pid != os.Getpid() {
			_ = signalPID(pid, sigKILL)
		}
	}
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
	for _, entry := range table {
		children[entry.ppid] = append(children[entry.ppid], entry.pid)
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
			p.tracked[child] = struct{}{}
			queue = append(queue, child)
		}
	}
	for _, entry := range table {
		if entry.pgid == p.pid && entry.pid != p.pid {
			p.tracked[entry.pid] = struct{}{}
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
	alive := make(map[int]bool, len(table))
	for _, entry := range table {
		alive[entry.pid] = true
	}
	p.trackMu.Lock()
	seen := make(map[int]struct{}, len(p.tracked))
	for pid := range p.tracked {
		if alive[pid] && pid != os.Getpid() {
			seen[pid] = struct{}{}
		}
	}
	p.trackMu.Unlock()
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

func (p *process) close() {
	p.closeOnce.Do(func() {
		close(p.stopMon)
		if p.stopWatch != nil {
			p.stopWatch()
		}
		if p.protoIn != nil {
			_ = p.protoIn.Close()
		}
		if p.protoOut != nil {
			_ = p.protoOut.Close()
		}
	})
}

func (p *process) cpuMillis() int64 {
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

type procEntry struct {
	pid  int
	ppid int
	pgid int
}

func processTable() ([]procEntry, error) {
	out, err := exec.Command("/bin/ps", "-axo", "pid=,ppid=,pgid=").Output()
	if err != nil {
		return nil, err
	}
	entries := make([]procEntry, 0, 128)
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		pgid, err3 := strconv.Atoi(fields[2])
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		entries = append(entries, procEntry{pid: pid, ppid: ppid, pgid: pgid})
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
