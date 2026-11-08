//go:build linux

package evalruntime

import (
	"testing"
	"time"
)

// hostProc is a worker-owned process as the host sees it: host pid plus start
// time. A pid printed inside the sandbox is a PID-namespace number (the
// interpreter is usually pid 2 in every kernel), so it is never signalled or
// compared on the host directly; it is translated through the worker's cgroup
// and the NSpid chain while the process lives.
type hostProc struct {
	hostIdentity
	sandboxPID int
}

// hostProcessOf maps a pid reported from inside session's sandbox to the host
// process with that innermost NSpid among the members of the session's current
// worker cgroup. It fails the test when no such member exists.
func hostProcessOf(t *testing.T, m *Manager, sessionID string, sandboxPID int) hostProc {
	t.Helper()
	s, err := m.session(sessionID)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	w := installedWorker(s)
	if w == nil || w.proc == nil || w.proc.cg == nil {
		t.Fatalf("session %s has no worker cgroup", sessionID)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, pid := range w.proc.cg.procs() {
			chain, err := nsPIDs(pid)
			if err != nil || len(chain) < 2 || chain[len(chain)-1] != sandboxPID {
				continue
			}
			id, err := identityOf(pid)
			if err != nil {
				continue
			}
			return hostProc{hostIdentity: id, sandboxPID: sandboxPID}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no member of %s's worker cgroup has sandbox pid %d", sessionID, sandboxPID)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (p hostProc) alive() bool { return p.hostIdentity.alive() }

// sameProcess reports whether two observations are one host process.
func (p hostProc) sameProcess(other hostProc) bool { return p.hostIdentity == other.hostIdentity }

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

// workerProcessesGone reports whether the manager has no worker cgroup left:
// a leaf is only removed once the kernel reports it empty, so no worker process
// and no descendant of one survives.
func workerProcessesGone(m *Manager) bool {
	return m.cgroups == nil || len(m.cgroups.liveLeaves()) == 0
}
