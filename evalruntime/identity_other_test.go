//go:build !linux

package evalruntime

import (
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// hostProc is a worker process identified by pid plus start time. macOS runs
// workers without a PID namespace, so a sandbox pid is the host pid.
type hostProc struct {
	pid        int
	start      string
	sandboxPID int
}

func processStart(pid int) string {
	out, err := exec.Command("/bin/ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func hostProcessOf(t *testing.T, m *Manager, sessionID string, sandboxPID int) hostProc {
	t.Helper()
	start := processStart(sandboxPID)
	if start == "" {
		t.Fatalf("worker pid %d is not running", sandboxPID)
	}
	return hostProc{pid: sandboxPID, start: start, sandboxPID: sandboxPID}
}

func (p hostProc) alive() bool {
	return processAlive(p.pid) && processStart(p.pid) == p.start
}

func (p hostProc) sameProcess(other hostProc) bool {
	return p.pid == other.pid && p.start == other.start
}

func waitHostProcGone(p hostProc, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !p.alive() {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return !p.alive()
}

// workerProcessesGone has no kernel membership record off Linux; callers also
// check the specific pids they observed.
func workerProcessesGone(*Manager) bool { return true }
