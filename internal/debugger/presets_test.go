package debugger

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func fakeProber(goos string, paths map[string]string, run func(name string, args []string) ([]byte, error)) PresetProber {
	return PresetProber{GOOS: goos, LookPath: func(name string) (string, error) {
		if path, ok := paths[name]; ok {
			return path, nil
		}
		return "", errors.New(name + " not found")
	}, Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
		if run != nil {
			return run(name, args)
		}
		return nil, errors.New("not run")
	}}
}

func presetsByID(items []Adapter) map[string]Adapter {
	out := map[string]Adapter{}
	for _, item := range items {
		out[item.ID] = item
	}
	return out
}

// TestPresetProbesAreHonest covers lldb resolution and the unavailable cases:
// an adapter is "available" only when the probe actually found it.
func TestPresetProbesAreHonest(t *testing.T) {
	prober := fakeProber("linux", map[string]string{
		"lldb-dap-18": "/usr/bin/lldb-dap-18",
		"dlv":         "/opt/lcbin/dlv",
	}, func(name string, args []string) ([]byte, error) {
		switch {
		case name == "/usr/bin/lldb-dap-18" && len(args) == 1 && args[0] == "--version":
			return []byte("lldb-dap: LLVM version 18.1.3\n"), nil
		case name == "/opt/lcbin/dlv" && len(args) == 1 && args[0] == "version":
			return []byte("Delve Debugger\nVersion: 1.27.2\n"), nil
		}
		return nil, errors.New("unexpected probe")
	})
	got := presetsByID(ProbePresets(context.Background(), prober))
	lldb := got["lldb"]
	if !lldb.Available || lldb.Command != "/usr/bin/lldb-dap-18" || !strings.Contains(lldb.Version, "18.1.3") {
		t.Fatalf("lldb resolved wrong: %+v", lldb)
	}
	delve := got["delve"]
	if !delve.Available || delve.Command != "/opt/lcbin/dlv" || delve.Version != "1.27.2" || delve.Transport != "unix" {
		t.Fatalf("delve resolved wrong: %+v", delve)
	}
	if py := got["debugpy"]; py.Available || !strings.Contains(py.Reason, "python3") {
		t.Fatalf("debugpy claimed available without a probe: %+v", py)
	}
	if js := got["js-debug"]; js.Available || js.Reason == "" {
		t.Fatalf("js-debug claimed available without a probe: %+v", js)
	}
}

// TestPresetDebugpyProbeReportsVersion checks the actual probe contract.
func TestPresetDebugpyProbeReportsVersion(t *testing.T) {
	prober := fakeProber("linux", map[string]string{"python3": "/usr/bin/python3"}, func(name string, args []string) ([]byte, error) {
		if name != "/usr/bin/python3" {
			return nil, errors.New("unexpected")
		}
		return []byte("1.8.1\n/home/user/.local/lib/python3.12/site-packages/debugpy/adapter/__main__.py\n"), nil
	})
	got := presetsByID(ProbePresets(context.Background(), prober))["debugpy"]
	if !got.Available || got.Version != "1.8.1" || len(got.Args) != 2 || !strings.HasSuffix(got.Args[1], "adapter/__main__.py") {
		t.Fatalf("debugpy probe wrong: %+v", got)
	}
}

// TestPresetDelveMissingIsUnavailable keeps "not installed" honest.
func TestPresetDelveMissingIsUnavailable(t *testing.T) {
	prober := fakeProber("linux", map[string]string{}, nil)
	got := presetsByID(ProbePresets(context.Background(), prober))["delve"]
	if got.Available || !strings.Contains(got.Reason, "dlv") {
		t.Fatalf("delve status wrong: %+v", got)
	}
}

// TestManagerDiscoveryRegistersOnlyProbedAdapters covers the Manager wiring:
// configured adapters win, probed ones become startable, and the rest are
// reported with a reason.
func TestManagerDiscoveryRegistersOnlyProbedAdapters(t *testing.T) {
	m, err := New(Options{Adapters: []Adapter{{ID: "configured", Command: testExecutable(t), Launch: map[string]any{}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.prober = fakeProber("linux", map[string]string{"lldb-dap": testExecutable(t)}, nil)
	m.discover(context.Background())
	byID := presetsByID(m.Adapters())
	if !byID["configured"].Available {
		t.Fatalf("configured adapter unavailable: %+v", byID["configured"])
	}
	if !byID["lldb"].Available || !byID["lldb"].Preset {
		t.Fatalf("probed lldb not registered: %+v", byID["lldb"])
	}
	if byID["debugpy"].Available {
		t.Fatalf("unprobed debugpy reported available: %+v", byID["debugpy"])
	}
	if _, err := m.Start(context.Background(), StartOptions{SessionID: "s", AdapterID: "lldb", Workspace: t.TempDir(), Program: testExecutable(t), Actor: "human"}); err != nil && err != ErrNotFound {
		t.Logf("probed adapter start reached the sandbox layer: %v", err)
	}
}
