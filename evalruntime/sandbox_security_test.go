package evalruntime

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Exercise the actual launcher, not generated profile text. /bin/sh used to
// acquire read access to / because its grandparent was treated as a prefix.
func TestOpusPluginExecutableDoesNotGrantParentRead(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	kernel := filepath.Join(root, "kernel")
	if err := os.Mkdir(kernel, 0700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(root, "private-secret")
	if err := os.WriteFile(secret, []byte("HOST-SECRET\n"), 0600); err != nil {
		t.Fatal(err)
	}
	exe, err := trustedExecutable("/bin/sh")
	if err != nil {
		t.Fatal(err)
	}
	launcher, err := newLauncher(SandboxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	spec := launchSpec{exe: exe, args: []string{"-c", fmt.Sprintf(`if IFS= read -r value < %q; then printf 'LEAK:%%s' "$value"; exit 9; fi; printf 'KERNEL-OK' > local.txt; IFS= read -r value < local.txt || :; printf '%%s' "$value"`, secret)}, kernelDir: kernel, dir: kernel, workersDir: kernel, profilePath: filepath.Join(root, "sandbox.sb"), cpuSeconds: 5, fileBytes: 1 << 20}
	cmd, err := launcher.command(spec)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	run := exec.CommandContext(ctx, cmd.Path, cmd.Args[1:]...)
	run.Dir = kernel
	run.Env = []string{"HOME=" + kernel, "PATH=/usr/bin:/bin"}
	data, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("sandbox command: %v: %s", err, data)
	}
	if strings.Contains(string(data), "HOST-SECRET") || !strings.Contains(string(data), "KERNEL-OK") {
		t.Fatalf("confinement/usable kernel violated: %s", data)
	}
}

// startVictim starts a process outside any sandbox that only this test owns. The
// returned alive reports, after a short grace for signal delivery, whether it
// still runs.
func startVictim(t *testing.T) (pid int, alive func() bool) {
	t.Helper()
	cmd := exec.Command("/bin/sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-done
	})
	return cmd.Process.Pid, func() bool {
		select {
		case <-done:
			return false
		case <-time.After(200 * time.Millisecond):
			return true
		}
	}
}

// TestSandboxSignalsStayInsideTheSandbox exercises the actual launcher with both
// profiles: the Eval kernel's (no fork) and the program profile DAP adapters and
// command hooks run under. A sandboxed process may signal itself and its own
// children - interrupts and debuggers need that - but never a process outside
// its sandbox: the host, the user's other processes or another kernel. The
// outside process here is a sleep this test owns.
func TestSandboxSignalsStayInsideTheSandbox(t *testing.T) {
	exe, err := trustedExecutable("/bin/sh")
	if err != nil {
		t.Fatal(err)
	}
	launcher, err := newLauncher(SandboxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		program bool
		inside  string
		want    string
	}{
		{name: "eval", inside: `kill -0 $$ && echo inside=ok`, want: "inside=ok"},
		{name: "program", program: true, inside: `/bin/sleep 30 & child=$!; kill -TERM $child; wait $child; echo inside=$?`, want: "inside=143"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pid, alive := startVictim(t)
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			kernel := filepath.Join(root, "kernel")
			if err := os.Mkdir(kernel, 0700); err != nil {
				t.Fatal(err)
			}
			script := fmt.Sprintf(`%s; if kill -TERM %d 2>/dev/null; then echo foreign=sent; else echo foreign=denied; fi`, tc.inside, pid)
			spec := launchSpec{exe: exe, args: []string{"-c", script}, kernelDir: kernel, dir: kernel, workersDir: kernel, profilePath: filepath.Join(root, "sandbox.sb"), cpuSeconds: 5, fileBytes: 1 << 20, allowSubprocess: tc.program, debugProgram: tc.program}
			cmd, err := launcher.command(spec)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			run := exec.CommandContext(ctx, cmd.Path, cmd.Args[1:]...)
			run.Dir = kernel
			run.Env = []string{"HOME=" + kernel, "PATH=/usr/bin:/bin"}
			data, err := run.CombinedOutput()
			if err != nil {
				t.Fatalf("sandbox command: %v: %s", err, data)
			}
			out := string(data)
			if !strings.Contains(out, tc.want) {
				t.Fatalf("a signal inside the sandbox failed: %q", out)
			}
			if !strings.Contains(out, "foreign=denied") || !alive() {
				t.Fatalf("a sandboxed process signalled pid %d outside its sandbox: %q", pid, out)
			}
		})
	}
}

// TestCellCannotSignalAnotherKernel gives a cell the host PID of another kernel's
// worker, which the process census publishes. Signalling it must fail, and the
// other kernel must keep running with its state.
func TestCellCannotSignalAnotherKernel(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	attacker := h.create("python", "signal-owner", "signal-owner")
	target := h.create("python", "signal-owner", "signal-owner")
	before := h.expectCompleted(target, "signal-owner", "kept = 41")
	pid := 0
	for _, row := range censusRows("signal-owner") {
		if row.Owner.ObjectID == target.ID {
			pid = row.PID
		}
	}
	if pid <= 0 {
		t.Fatalf("census has no worker for %s: %+v", target.ID, censusRows("signal-owner"))
	}
	result := h.expectCompleted(attacker, "signal-owner", fmt.Sprintf("import os, signal\ntry:\n    os.kill(%d, signal.SIGKILL)\n    print('sent')\nexcept OSError as exc:\n    print('denied', type(exc).__name__)\n", pid))
	if !strings.Contains(result.Stdout, "denied") {
		t.Fatalf("a cell signalled another kernel's worker %d: %q", pid, result.Stdout)
	}
	after := h.expectCompleted(target, "signal-owner", "print(kept + 1)")
	if strings.TrimSpace(after.Stdout) != "42" || after.Generation != before.Generation {
		t.Fatalf("the other kernel lost its state: %q, generation %d -> %d", after.Stdout, before.Generation, after.Generation)
	}
}

func TestOpusRuntimeExecutableMustBeRegular(t *testing.T) {
	root := t.TempDir()
	if _, err := trustedExecutable(root); err == nil {
		t.Fatal("directory executable accepted")
	}
	file := filepath.Join(root, "plain")
	if err := os.WriteFile(file, []byte("not executable"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := trustedExecutable(file); err == nil {
		t.Fatal("nonexecutable file accepted")
	}
	link := filepath.Join(root, "shell")
	if err := os.Symlink("/bin/sh", link); err != nil {
		t.Fatal(err)
	}
	got, err := trustedExecutable(link)
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks("/bin/sh")
	if err != nil || got != want {
		t.Fatalf("resolved identity %q want %q: %v", got, want, err)
	}
}

func TestOpusSandboxDeniesUnrelatedDirectoryListing(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	s := h.create("python", "owner", "owner")
	unrelated := filepath.Join(h.root, "unrelated-private")
	if err := os.Mkdir(unrelated, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unrelated, "synthetic-entry"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	r := h.expectCompleted(s, "owner", fmt.Sprintf("import os\ntry:\n os.listdir(%q)\n print('LEAK')\nexcept OSError:\n print('DENIED')", unrelated))
	if !strings.Contains(r.Stdout, "DENIED") || strings.Contains(r.Stdout, "LEAK") {
		t.Fatalf("directory privacy: %q", r.Stdout)
	}
}

// A synthetic daemon, not Docker or SSH: no request ever reaches a real host
// service. The same command exercises both /run and its /var/run alias.
func TestOpusLinuxRunSocketIsolation(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux bubblewrap")
	}
	// The listener must live under the host's real /run. An ordinary user cannot
	// create entries in /run itself, but owns /run/user/<uid>; either location
	// is shadowed by the sandbox's private /run in exactly the same way.
	dir, err := os.MkdirTemp("/run", "lc-socket-")
	if err != nil {
		dir, err = os.MkdirTemp(fmt.Sprintf("/run/user/%d", os.Getuid()), "lc-socket-")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "daemon.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	h, _ := newPythonHarness(t, nil)
	s := h.create("python", "owner", "owner")
	for _, socket := range []string{path, "/var" + path} {
		r := h.expectCompleted(s, "owner", fmt.Sprintf("import socket\nsock=socket.socket(socket.AF_UNIX)\nsock.settimeout(1)\ntry:\n sock.connect(%q)\n print('CONNECTED')\nexcept OSError:\n print('ISOLATED')\nfinally:\n sock.close()", socket))
		if !strings.Contains(r.Stdout, "ISOLATED") || strings.Contains(r.Stdout, "CONNECTED") {
			t.Fatalf("host socket reachable: %q", r.Stdout)
		}
	}
}

func TestOpusResetClearsHomeAndTemporaryState(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	s := h.create("python", "owner", "owner")
	h.expectCompleted(s, "owner", "import os\nopen(os.environ['HOME']+'/poison','w').write('old')\nopen(os.environ['TMPDIR']+'/poison','w').write('old')")
	if err := h.m.Reset(context.Background(), s.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	r := h.expectCompleted(s, "owner", "import os\nprint(os.path.exists(os.environ['HOME']+'/poison'),os.path.exists(os.environ['TMPDIR']+'/poison'))")
	if strings.TrimSpace(r.Stdout) != "False False" {
		t.Fatalf("reset retained state: %q", r.Stdout)
	}
}

func TestOpusArtifactClosedKernelRemainsReadable(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	s := h.create("python", "owner", "owner")
	a, err := h.m.PutArtifact(context.Background(), s.ID, "owner", "kept.txt", "text/plain", []byte("durable"))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.m.Close(context.Background(), s.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	_, b, err := h.m.Artifact(context.Background(), s.ID, "owner", a.ID)
	if err != nil || string(b) != "durable" {
		t.Fatalf("closed artifact=%q %v", b, err)
	}
	if _, err := h.m.PutArtifact(context.Background(), s.ID, "owner", "late.txt", "text/plain", nil); err == nil {
		t.Fatal("closed session accepted import")
	}
}
