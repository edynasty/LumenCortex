//go:build linux

package evalruntime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/evalapi"
)

func TestLinuxBoundaryObservesOwnedWorkerAndRejectsSameUID(t *testing.T) {
	binding := evalapi.BoundaryContext{CohortID: "local", JobID: "boundary", TrialID: "offline", ExecutionID: "first", RequestID: "inspect"}
	h, _ := newPythonHarness(t, nil)
	s := h.create("python", "owner", "owner")
	report, err := h.m.AssertBoundary(context.Background(), s.ID, "owner", binding)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(report)
	t.Log(string(raw))
	if report.Process.HostPID == 0 || report.Process.Starttime == 0 || report.Checks["identity"].Status != evalapi.BoundaryPass {
		t.Fatalf("real interpreter identity unobserved: %+v", report)
	}
	for _, name := range []string{"generation", "cgroup", "user", "pid", "mnt", "net", "ipc", "uts", "networkPolicy", "bubblewrap"} {
		if report.Checks[name].Status != evalapi.BoundaryPass {
			t.Errorf("%s: %+v", name, report.Checks[name])
		}
	}
	if report.Checks["callerIsolation"].Status == evalapi.BoundaryPass || report.RequirePass(binding) == nil {
		t.Fatal("same-UID host was accepted as independent broker")
	}
	if _, err = h.m.AssertBoundary(context.Background(), s.ID, "foreign", binding); !errors.Is(err, evalapi.ErrDenied) {
		t.Fatalf("foreign observation: %v", err)
	}
	if err = h.m.Reset(context.Background(), s.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	h.expectCompleted(s, "owner", "40+2")
	fresh, err := h.m.AssertBoundary(context.Background(), s.ID, "owner", binding)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Process.Generation <= report.Process.Generation || (fresh.Process.HostPID == report.Process.HostPID && fresh.Process.Starttime == report.Process.Starttime) {
		t.Fatal("reset reused old proof identity")
	}
}

func TestLinuxBoundaryRefusesCellBeforeSideEffects(t *testing.T) {
	binding := evalapi.BoundaryContext{CohortID: "local", JobID: "boundary", TrialID: "offline", ExecutionID: "denied"}
	h, _ := newPythonHarness(t, func(o *Options) { o.Boundary = &binding })
	s := h.create("python", "owner", "owner")
	_, err := h.m.Execute(context.Background(), evalapi.Request{ID: "must-not-run", SessionID: s.ID, AgentID: "owner", Code: "open('forbidden.txt','w').write('escaped')"}, nil)
	if err == nil {
		t.Fatal("unverified boundary ran a cell")
	}
	if _, err = os.Stat(filepath.Join(s.Workspace, "work", "forbidden.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cell side effect: %v", err)
	}
}

func TestLinuxBoundaryProcControlMounts(t *testing.T) {
	root := t.TempDir()
	m := &Manager{workersDir: root}
	w := &worker{s: &session{kernel: root}}
	base := "10 1 0:90 / /proc rw,nosuid,nodev,noexec - proc proc rw\n" +
		"11 1 0:91 / " + root + " ro - tmpfs tmpfs rw\n"
	for _, tc := range []struct {
		name, mount string
		wantErr     bool
	}{
		{"readonly-self", "12 10 0:90 /irq /proc/irq ro - proc proc rw", false},
		{"writable", "12 10 0:90 /irq /proc/irq rw - proc proc rw", true},
		{"foreign-device", "12 10 0:92 /irq /proc/irq ro - proc proc rw", true},
		{"foreign-parent", "12 9 0:90 /irq /proc/irq ro - proc proc rw", true},
		{"wrong-source", "12 10 0:90 /sys /proc/irq ro - proc proc rw", true},
		{"unexpected-control", "12 10 0:90 /kcore /proc/kcore ro - proc proc rw", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkBoundaryMounts(m, w, "/proc/self", base+tc.mount)
			if (err != nil) != tc.wantErr {
				t.Fatalf("mount admission: %v", err)
			}
			if tc.wantErr && !strings.Contains(err.Error(), "unexpected mount") {
				t.Fatalf("unrelated failure: %v", err)
			}
		})
	}
}
