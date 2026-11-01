package lsp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/protocol"
)

// fakeServerPath is the built testdata language server. The go tool does not build testdata with
// ./..., so TestMain builds it once for the package.
var fakeServerPath string

func TestMain(m *testing.M) {
	// A helper process of client_test.go speaks the protocol itself; it needs no binary.
	if os.Getenv("LCX_LSP_HELPER") == "1" {
		os.Exit(m.Run())
	}
	dir, err := os.MkdirTemp("", "lc-fake-lsp-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	path := filepath.Join(dir, "fake-lsp")
	_, file, _, _ := runtime.Caller(0)
	build := exec.Command("go", "build", "-o", path, "./testdata/fakeserver")
	build.Dir = filepath.Dir(file)
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build testdata/fakeserver: %v\n%s", err, out)
		_ = os.RemoveAll(dir)
		os.Exit(1)
	}
	fakeServerPath = path
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func newTestManager(t *testing.T, opts Options) *Manager {
	t.Helper()
	if opts.Workspace == "" {
		opts.Workspace = t.TempDir()
	}
	if opts.IdleTimeout == 0 {
		opts.IdleTimeout = time.Minute
	}
	manager, err := NewManager(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	return manager
}

func writeWorkspaceFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func goServer(args ...string) LanguageServer {
	return LanguageServer{Language: "go", Name: "fake", Command: fakeServerPath, Args: args, Extensions: []string{".go"}}
}

func pythonServer(args ...string) LanguageServer {
	return LanguageServer{Language: "python", Name: "fake", Command: fakeServerPath, Args: args, Extensions: []string{".py"}}
}

func logLines(path string) []string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

func pidFromStartLog(t *testing.T, path string) int {
	t.Helper()
	lines := logLines(path)
	if len(lines) == 0 {
		t.Fatalf("no start line in %s", path)
	}
	fields := strings.Fields(lines[0])
	pid, err := strconv.Atoi(fields[0])
	if err != nil {
		t.Fatalf("start line %q: %v", lines[0], err)
	}
	return pid
}

func processGone(pid int) bool {
	if pid <= 0 {
		return true
	}
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

// waitGone fails when pid is still a live (or unreaped) process after timeout.
func waitGone(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if processGone(pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %d still exists after %s", pid, timeout)
}

func waitState(t *testing.T, manager *Manager, language, want string, timeout time.Duration) ServerStatus {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last ServerStatus
	for time.Now().Before(deadline) {
		for _, status := range manager.Servers() {
			if status.Language != language {
				continue
			}
			last = status
			if status.State == want {
				return status
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s never reached %q; last %+v", language, want, last)
	return last
}

func TestManagerStartsOnFirstRequestAndReusesTheServer(t *testing.T) {
	workspace := t.TempDir()
	writeWorkspaceFile(t, workspace, "main.go", "package main\n")
	startLog := filepath.Join(t.TempDir(), "starts.log")
	manager := newTestManager(t, Options{
		Workspace: workspace,
		Servers:   []LanguageServer{goServer("-start-log", startLog)},
	})
	if !manager.Available() {
		t.Fatal("a server with a command and a .go file must be available")
	}
	if status := manager.Servers(); len(status) != 1 || status[0].State != StateStopped || status[0].PID != 0 {
		t.Fatalf("statuses before the first request: %+v", status)
	}
	if lines := logLines(startLog); len(lines) != 0 {
		t.Fatalf("a manager started a server before a request: %v", lines)
	}

	started := time.Now()
	raw, err := manager.Hover(context.Background(), "main.go", 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	firstRequest := time.Since(started)
	if !strings.Contains(string(raw), "hover-ok") {
		t.Fatalf("hover=%s", raw)
	}
	status := waitState(t, manager, "go", StateIdle, time.Second)
	if status.PID == 0 || status.StartedAt == nil || status.LastUsedAt == nil || status.Root != workspace {
		t.Fatalf("status=%+v", status)
	}
	pid := status.PID

	if _, err := manager.Hover(context.Background(), "main.go", 0, 1); err != nil {
		t.Fatal(err)
	}
	if got := manager.Servers()[0].PID; got != pid {
		t.Fatalf("second request started another server: %d -> %d", pid, got)
	}
	if lines := logLines(startLog); len(lines) != 1 {
		t.Fatalf("starts=%v, want one", lines)
	}
	t.Logf("first request started the server in %s (pid %d)", firstRequest.Round(time.Millisecond), pid)

	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	waitGone(t, pid, 5*time.Second)
}

// ADR-0013: a run in an untrusted workspace starts no language server and uses only one the host
// started; a server a trusted run started is a run's, and an untrusted run does not use it either.
func TestManagerForRunStartsOnlyWhenTrusted(t *testing.T) {
	workspace := t.TempDir()
	writeWorkspaceFile(t, workspace, "main.go", "package main\n")
	startLog := filepath.Join(t.TempDir(), "starts.log")
	manager := newTestManager(t, Options{
		Workspace: workspace,
		Servers:   []LanguageServer{goServer("-start-log", startLog)},
	})
	ctx := context.Background()
	untrusted, trusted := manager.ForRun(false), manager.ForRun(true)

	if untrusted.Available() || !trusted.Available() {
		t.Fatalf("nothing running: untrusted available=%v trusted available=%v", untrusted.Available(), trusted.Available())
	}
	if _, err := untrusted.Hover(ctx, "main.go", 0, 1); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("untrusted hover with nothing running: %v, want ErrNotRunning", err)
	}
	if lines := logLines(startLog); len(lines) != 0 {
		t.Fatalf("an untrusted run started a server: %v", lines)
	}

	// A trusted run starts the server; it is a run's, so an untrusted run still may not use it.
	if _, err := trusted.Hover(ctx, "main.go", 0, 1); err != nil {
		t.Fatal(err)
	}
	if untrusted.Available() {
		t.Fatal("a server a run started is offered to an untrusted run")
	}
	if _, err := untrusted.Hover(ctx, "main.go", 0, 1); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("untrusted hover on a run's server: %v, want ErrNotRunning", err)
	}

	// The host starts the server: an untrusted run uses that process and starts no other.
	status, err := manager.Start(ctx, Config{Command: fakeServerPath, Args: []string{"-start-log", startLog}, LanguageID: "go"})
	if err != nil {
		t.Fatal(err)
	}
	if !untrusted.Available() {
		t.Fatal("a host-started server is not offered to an untrusted run")
	}
	raw, err := untrusted.Hover(ctx, "main.go", 0, 1)
	if err != nil || !strings.Contains(string(raw), "hover-ok") {
		t.Fatalf("untrusted hover on the host's server: %s %v", raw, err)
	}
	if got := manager.Servers()[0].PID; got != status.PID {
		t.Fatalf("the untrusted run's call replaced the host's server: %d -> %d", status.PID, got)
	}
	if lines := logLines(startLog); len(lines) != 2 {
		t.Fatalf("starts=%v, want the trusted run's and the host's", lines)
	}
}

func TestManagerReclaimsAnIdleServer(t *testing.T) {
	workspace := t.TempDir()
	writeWorkspaceFile(t, workspace, "main.go", "package main\n")
	manager := newTestManager(t, Options{
		Workspace:   workspace,
		Servers:     []LanguageServer{goServer()},
		IdleTimeout: 150 * time.Millisecond,
	})
	if _, err := manager.Hover(context.Background(), "main.go", 0, 1); err != nil {
		t.Fatal(err)
	}
	pid := manager.Servers()[0].PID

	reclaimStart := time.Now()
	waitState(t, manager, "go", StateStopped, 5*time.Second)
	reclaimed := time.Since(reclaimStart)
	waitGone(t, pid, 5*time.Second)
	t.Logf("idle reclaim %s after the last use (idle timeout 150ms)", reclaimed.Round(time.Millisecond))

	raw, err := manager.Hover(context.Background(), "main.go", 0, 1)
	if err != nil {
		t.Fatalf("a reclaimed server must start again: %v", err)
	}
	if !strings.Contains(string(raw), "hover-ok") {
		t.Fatalf("hover=%s", raw)
	}
	if status := manager.Servers()[0]; status.PID == 0 || status.PID == pid {
		t.Fatalf("status after restart=%+v, old pid %d", status, pid)
	}
}

func TestManagerEvictsTheLeastRecentlyUsedIdleServer(t *testing.T) {
	workspace := t.TempDir()
	writeWorkspaceFile(t, workspace, "main.go", "package main\n")
	writeWorkspaceFile(t, workspace, "script.py", "print(1)\n")
	manager := newTestManager(t, Options{
		Workspace:  workspace,
		Servers:    []LanguageServer{goServer(), pythonServer()},
		MaxServers: 1,
	})
	if _, err := manager.Hover(context.Background(), "main.go", 0, 1); err != nil {
		t.Fatal(err)
	}
	goPID := waitState(t, manager, "go", StateIdle, time.Second).PID
	if _, err := manager.Hover(context.Background(), "script.py", 0, 1); err != nil {
		t.Fatal(err)
	}
	if status := waitState(t, manager, "python", StateIdle, time.Second); status.PID == 0 || status.PID == goPID {
		t.Fatalf("python status=%+v, go pid %d", status, goPID)
	}
	waitGone(t, goPID, 5*time.Second)
	if state := manager.Servers()[0].State; state != StateStopped {
		t.Fatalf("evicted go server state=%s", state)
	}
}

func TestManagerRefusesToEvictABusyServer(t *testing.T) {
	workspace := t.TempDir()
	writeWorkspaceFile(t, workspace, "main.go", "package main\n")
	writeWorkspaceFile(t, workspace, "script.py", "print(1)\n")
	manager := newTestManager(t, Options{
		Workspace:      workspace,
		Servers:        []LanguageServer{goServer("-slow", "400"), pythonServer()},
		MaxServers:     1,
		RequestTimeout: 5 * time.Second,
	})
	done := make(chan error, 1)
	go func() {
		_, err := manager.Hover(context.Background(), "main.go", 0, 1)
		done <- err
	}()
	waitState(t, manager, "go", StateRunning, 5*time.Second)
	if _, err := manager.Hover(context.Background(), "script.py", 0, 1); !errors.Is(err, ErrServerLimit) {
		t.Fatalf("err=%v, want ErrServerLimit", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("the busy server's request failed: %v", err)
	}
	if _, err := manager.Hover(context.Background(), "script.py", 0, 1); err != nil {
		t.Fatalf("the idle server must be evictable once it is free: %v", err)
	}
}

func TestManagerRestartsACrashedServer(t *testing.T) {
	workspace := t.TempDir()
	writeWorkspaceFile(t, workspace, "main.go", "package main\n")
	startLog := filepath.Join(t.TempDir(), "starts.log")
	crashMarker := writeWorkspaceFile(t, t.TempDir(), "crash-once", "")
	manager := newTestManager(t, Options{
		Workspace: workspace,
		Servers: []LanguageServer{goServer("-crash", "textDocument/hover", "-crash-once", crashMarker,
			"-start-log", startLog)},
		BackoffBase: 20 * time.Millisecond,
	})
	if _, err := manager.Hover(context.Background(), "main.go", 0, 1); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err=%v, want ErrUnavailable", err)
	}
	status := waitState(t, manager, "go", StateUnavailable, 2*time.Second)
	if status.LastError == "" || status.PID != 0 {
		t.Fatalf("status after the crash=%+v", status)
	}
	crashedPID := pidFromStartLog(t, startLog)
	waitGone(t, crashedPID, 5*time.Second)

	if _, err := manager.Hover(context.Background(), "main.go", 0, 1); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a request inside the backoff must not start a server: %v", err)
	}
	time.Sleep(40 * time.Millisecond)
	if _, err := manager.Hover(context.Background(), "main.go", 0, 1); err != nil {
		t.Fatalf("the server must restart after its backoff: %v", err)
	}
	restarted := waitState(t, manager, "go", StateIdle, time.Second)
	if restarted.PID == 0 || restarted.PID == crashedPID {
		t.Fatalf("restarted status=%+v, crashed pid %d", restarted, crashedPID)
	}
	if lines := logLines(startLog); len(lines) != 2 {
		t.Fatalf("starts=%v, want two", lines)
	}
}

func TestManagerCloseLeavesNoProcess(t *testing.T) {
	workspace := t.TempDir()
	writeWorkspaceFile(t, workspace, "main.go", "package main\n")
	writeWorkspaceFile(t, workspace, "script.py", "print(1)\n")
	manager := newTestManager(t, Options{
		Workspace: workspace,
		Servers:   []LanguageServer{goServer(), pythonServer()},
	})
	if _, err := manager.Hover(context.Background(), "main.go", 0, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Hover(context.Background(), "script.py", 0, 1); err != nil {
		t.Fatal(err)
	}
	pids := make([]int, 0, 2)
	for _, status := range manager.Servers() {
		pids = append(pids, status.PID)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	for _, pid := range pids {
		waitGone(t, pid, 5*time.Second)
	}
	for _, status := range manager.Servers() {
		if status.State != StateStopped || status.PID != 0 {
			t.Fatalf("status after Close=%+v", status)
		}
	}
	if err := manager.Close(); err != nil {
		t.Fatalf("Close is not idempotent: %v", err)
	}
}

func TestManagerRequestTimeoutAndCancellation(t *testing.T) {
	workspace := t.TempDir()
	writeWorkspaceFile(t, workspace, "main.go", "package main\n")
	cancelLog := filepath.Join(t.TempDir(), "cancels.log")
	manager := newTestManager(t, Options{
		Workspace:      workspace,
		Servers:        []LanguageServer{goServer("-slow", "300", "-cancel-log", cancelLog)},
		RequestTimeout: 80 * time.Millisecond,
	})

	started := time.Now()
	if _, err := manager.Hover(context.Background(), "main.go", 0, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, want a request timeout", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("the request timeout took %s", elapsed)
	}

	// A cancelled context returns at once and tells the server to drop the request.
	manager.Configure(Options{RequestTimeout: 10 * time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	started = time.Now()
	if _, err := manager.Hover(ctx, "main.go", 0, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want a cancellation", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("the cancellation took %s", elapsed)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(logLines(cancelLog)) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if lines := logLines(cancelLog); len(lines) == 0 {
		t.Fatal("the cancelled request was not reported to the server with $/cancelRequest")
	}

	// The run's tool deadline bounds a request that has no timeout of its own.
	manager.Configure(Options{RequestTimeout: 10 * time.Second})
	toolCtx := protocol.WithToolDeadline(context.Background(), time.Now().Add(60*time.Millisecond))
	started = time.Now()
	if _, err := manager.Hover(toolCtx, "main.go", 0, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, want the tool deadline", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("the tool deadline took %s", elapsed)
	}
}

func TestManagerStartTimeoutMarksTheServerUnavailable(t *testing.T) {
	workspace := t.TempDir()
	writeWorkspaceFile(t, workspace, "main.go", "package main\n")
	manager := newTestManager(t, Options{
		Workspace:    workspace,
		Servers:      []LanguageServer{goServer("-slow", "2000")},
		StartTimeout: 120 * time.Millisecond,
		BackoffBase:  10 * time.Millisecond,
	})
	started := time.Now()
	if _, err := manager.Hover(context.Background(), "main.go", 0, 1); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err=%v, want ErrUnavailable", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("the start timeout took %s", elapsed)
	}
	status := waitState(t, manager, "go", StateUnavailable, 2*time.Second)
	if status.LastError == "" {
		t.Fatalf("status=%+v", status)
	}
}

func TestManagerAvailability(t *testing.T) {
	t.Run("command missing", func(t *testing.T) {
		workspace := t.TempDir()
		writeWorkspaceFile(t, workspace, "main.go", "package main\n")
		manager := newTestManager(t, Options{Workspace: workspace, Servers: []LanguageServer{
			{Language: "go", Command: "lcx-no-such-language-server", Extensions: []string{".go"}},
		}})
		if manager.Available() {
			t.Fatal("a server whose command is not on PATH must not be available")
		}
		status := manager.Servers()
		if len(status) != 1 || status[0].State != StateUnavailable || !strings.Contains(status[0].LastError, "not found") {
			t.Fatalf("status=%+v", status)
		}
	})

	t.Run("no file of the language", func(t *testing.T) {
		workspace := t.TempDir()
		writeWorkspaceFile(t, workspace, "notes.txt", "hello\n")
		manager := newTestManager(t, Options{Workspace: workspace, Servers: []LanguageServer{goServer()}})
		if manager.Available() {
			t.Fatal("a workspace with no .go file and no marker must not be available")
		}
	})

	t.Run("marker only", func(t *testing.T) {
		workspace := t.TempDir()
		writeWorkspaceFile(t, workspace, "go.mod", "module example.com/x\n")
		manager := newTestManager(t, Options{Workspace: workspace, Servers: []LanguageServer{goServer()}})
		if !manager.Available() {
			t.Fatal("a go.mod marker must make the Go server available")
		}
		if languages := manager.AvailableLanguages(); len(languages) != 1 || languages[0] != "go" {
			t.Fatalf("languages=%v", languages)
		}
	})

	t.Run("default catalog", func(t *testing.T) {
		manager := newTestManager(t, Options{Workspace: t.TempDir()})
		status := manager.Servers()
		if len(status) != len(DefaultServers()) {
			t.Fatalf("default catalog has %d servers, status reports %d", len(DefaultServers()), len(status))
		}
	})
}

func TestManagerConcurrentRequestsShareOneServer(t *testing.T) {
	workspace := t.TempDir()
	writeWorkspaceFile(t, workspace, "main.go", "package main\n")
	startLog := filepath.Join(t.TempDir(), "starts.log")
	manager := newTestManager(t, Options{
		Workspace: workspace,
		Servers:   []LanguageServer{goServer("-start-log", startLog)},
	})
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := manager.Hover(context.Background(), "main.go", 0, 1); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("hover: %v", err)
	}
	if lines := logLines(startLog); len(lines) != 1 {
		t.Fatalf("starts=%v, want one", lines)
	}
	if status := manager.Servers()[0]; status.State != StateIdle || status.PID == 0 {
		t.Fatalf("status=%+v", status)
	}
}
