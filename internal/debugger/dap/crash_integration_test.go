package dap

import (
	"context"
	"github.com/edynasty/LumenCortex/evalruntime"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRealAdapterCrashClosesTransport(t *testing.T) {
	if os.Getenv("LC_CAPABILITY_EXPERIMENT") != "1" {
		t.Skip("explicit real adapter crash matrix")
	}
	lldb, err := exec.Command("xcrun", "--find", "lldb-dap").Output()
	if err != nil {
		t.Fatal(err)
	}
	python, node := os.Getenv("LC_TEST_PYTHON"), os.Getenv("LC_TEST_NODE")
	py, js := os.Getenv("LC_TEST_DEBUGPY"), os.Getenv("LC_TEST_JS_DEBUG")
	for _, tc := range []struct {
		name, command  string
		args, reads    []string
		unix, loopback bool
	}{
		{"lldb", strings.TrimSpace(string(lldb)), nil, []string{"/Applications/Xcode.app"}, false, false},
		{"delve", os.Getenv("LC_TEST_DELVE"), []string{"dap", "--listen=unix:{socket}"}, []string{"/Library/Developer/CommandLineTools/Library/PrivateFrameworks/LLDB.framework"}, true, true},
		{"debugpy", python, []string{"-I", py}, []string{filepath.Dir(filepath.Dir(python)), filepath.Dir(filepath.Dir(py))}, false, true},
		{"node", node, []string{js, "{socket}"}, []string{filepath.Dir(filepath.Dir(node)), filepath.Dir(filepath.Dir(js))}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			private, err := os.MkdirTemp("/tmp", "lc-dap-crash-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(private)
			private, err = filepath.EvalSymlinks(private)
			if err != nil {
				t.Fatal(err)
			}
			socket := ""
			if tc.unix {
				socket = filepath.Join(private, "dap.sock")
			}
			args := append([]string(nil), tc.args...)
			for i := range args {
				args[i] = strings.ReplaceAll(args[i], "{socket}", socket)
			}
			root := t.TempDir()
			cmd, err := evalruntime.ProgramCommand(ctx, evalruntime.ProgramOptions{Debug: true, Executable: tc.command, Args: args, Workspace: root, PrivateDir: private, ReadPaths: tc.reads, Loopback: tc.loopback, Socket: socket, Env: []string{"PATH=/usr/bin:/bin", "HOME=" + root, "TMPDIR=" + root}, CPUSeconds: 20, FileBytes: 16 << 20})
			if err != nil {
				t.Fatal(err)
			}
			var p *Process
			if tc.unix {
				p, err = StartUnix(ctx, cmd, socket)
			} else {
				p, err = Start(ctx, cmd)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			if err = p.Client.Request(ctx, "initialize", map[string]any{"adapterID": tc.name, "pathFormat": "path", "linesStartAt1": true, "columnsStartAt1": true}, nil); err != nil {
				t.Fatal(err)
			}
			pid := cmd.Process.Pid
			if err = cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-p.Done():
			case <-ctx.Done():
				t.Fatal("adapter not reaped")
			}
			select {
			case <-p.Client.Done():
			case <-ctx.Done():
				t.Fatal("transport survived adapter crash")
			}
			if err = p.Client.Request(ctx, "threads", nil, nil); err == nil {
				t.Fatal("request succeeded after crash")
			}
			if err = p.Close(); err != nil {
				t.Fatal(err)
			}
			t.Logf("REAL_ADAPTER_CRASH %s pid=%d reaped=true pending_transport_closed=true", tc.name, pid)
		})
	}
}
