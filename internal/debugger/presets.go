package debugger

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// PresetProber resolves the host commands of the built-in adapter presets. The
// defaults probe this host; tests replace them with deterministic fakes.
type PresetProber struct {
	GOOS     string
	LookPath func(string) (string, error)
	Run      func(ctx context.Context, name string, args ...string) ([]byte, error)
}

// DefaultProber probes the running host.
func DefaultProber() PresetProber {
	return PresetProber{GOOS: runtime.GOOS, LookPath: exec.LookPath, Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, name, args...).Output()
	}}
}

// lldbCandidates are the PATH names lldb-dap ships under on Linux packages and
// some macOS installs; the versioned names come first after the plain one.
var lldbCandidates = []string{"lldb-dap", "lldb-dap-21", "lldb-dap-20", "lldb-dap-19", "lldb-dap-18", "lldb-dap-17", "lldb-dap-16", "lldb-dap-15", "lldb-dap-14"}

// FindLLDB resolves lldb-dap on this host: xcrun on macOS, PATH elsewhere.
func FindLLDB() (string, error) {
	return DefaultProber().findLLDB()
}

func (p PresetProber) findLLDB() (string, error) {
	if p.GOOS == "darwin" {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		out, err := p.Run(ctx, "xcrun", "--find", "lldb-dap")
		if err == nil {
			if path := strings.TrimSpace(string(out)); path != "" {
				return path, nil
			}
		}
	}
	for _, name := range lldbCandidates {
		if path, err := p.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", errors.New("lldb-dap not found (xcrun --find lldb-dap or PATH)")
}

// output returns the whole bounded probe output.
func (p PresetProber) output(name string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	out, err := p.Run(ctx, name, args...)
	if err != nil {
		return ""
	}
	return clip(string(out), 2048)
}

func (p PresetProber) version(name string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	out, err := p.Run(ctx, name, args...)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return clip(line, 200)
		}
	}
	return ""
}

// ProbePresets reports the built-in adapter presets. An entry with Available
// true carries a usable Command and default launch template; an unavailable
// entry carries the reason this host cannot run it. Nothing is installed.
func ProbePresets(ctx context.Context, prober PresetProber) []Adapter {
	if err := ctx.Err(); err != nil {
		return nil
	}
	out := []Adapter{
		probeLLDB(prober),
		probeDelve(prober),
		probeDebugpy(prober),
		probeJSDebug(prober),
	}
	for i := range out {
		out[i].Preset = true
	}
	return out
}

func probeLLDB(p PresetProber) Adapter {
	adapter := Adapter{ID: "lldb", Language: "c", Preset: true, Launch: map[string]any{}}
	path, err := p.findLLDB()
	if err != nil {
		adapter.Reason = err.Error()
		return adapter
	}
	adapter.Command = path
	adapter.Available = true
	adapter.Version = p.version(path, "--version")
	if p.GOOS == "darwin" {
		adapter.Launch["disableASLR"] = false
		if root := xcodeRoot(path); root != "" {
			adapter.ReadPaths = []string{root}
		}
	}
	return adapter
}

// xcodeRoot returns the Xcode or CommandLineTools prefix that contains an
// adapter path, so the sandbox can read LLDB's frameworks.
func xcodeRoot(path string) string {
	for _, marker := range []string{"/Contents/Developer/", "/usr/bin/", "/usr/lib/"} {
		if i := strings.Index(path, marker); i > 0 {
			return path[:i]
		}
	}
	return ""
}

func probeDelve(p PresetProber) Adapter {
	adapter := Adapter{ID: "delve", Language: "go", Preset: true, Transport: "unix", Loopback: true, Launch: map[string]any{"mode": "exec"}, Args: []string{"dap", "--listen=unix:{socket}"}}
	if p.GOOS == "darwin" {
		if _, err := os.Stat("/Library/Developer/CommandLineTools/Library/PrivateFrameworks/LLDB.framework"); err == nil {
			adapter.ReadPaths = []string{"/Library/Developer/CommandLineTools/Library/PrivateFrameworks/LLDB.framework"}
		} else if _, err := os.Stat("/Applications/Xcode.app"); err == nil {
			adapter.ReadPaths = []string{"/Applications/Xcode.app"}
		}
	}
	path, err := p.LookPath("dlv")
	if err != nil {
		adapter.Reason = "dlv not found on PATH"
		return adapter
	}
	adapter.Command = path
	adapter.Available = true
	adapter.Version = delveVersion(p.output(path, "version"))
	return adapter
}

// delveVersion extracts the version from `dlv version` output. p.version only
// returns the first line, so ask the probe for the full output instead.
func delveVersion(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if _, value, ok := strings.Cut(line, "Version:"); ok {
			return clip(strings.TrimSpace(value), 100)
		}
	}
	return clip(strings.TrimSpace(text), 100)
}

func probeDebugpy(p PresetProber) Adapter {
	adapter := Adapter{ID: "debugpy", Language: "python", Preset: true, Loopback: true, Launch: map[string]any{"justMyCode": true}}
	python, err := p.LookPath("python3")
	if err != nil {
		adapter.Reason = "python3 not found on PATH"
		return adapter
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := p.Run(ctx, python, "-c", "import debugpy, debugpy.adapter, os; print(debugpy.__version__); print(debugpy.adapter.__file__)")
	if err != nil {
		adapter.Reason = "debugpy is not installed for " + python
		return adapter
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		adapter.Reason = "debugpy probe returned no adapter path"
		return adapter
	}
	adapter.Command = python
	adapter.Available = true
	adapter.Version = clip(strings.TrimSpace(lines[0]), 100)
	adapter.Args = []string{"-I", strings.TrimSpace(lines[1])}
	adapter.Launch["python"] = []string{python}
	seen := map[string]bool{}
	for _, path := range []string{python, strings.TrimSpace(lines[1])} {
		if dir := filepath.Dir(filepath.Dir(path)); dir != "" && !seen[dir] {
			seen[dir] = true
			adapter.ReadPaths = append(adapter.ReadPaths, dir)
		}
	}
	return adapter
}

func probeJSDebug(p PresetProber) Adapter {
	adapter := Adapter{ID: "js-debug", Language: "javascript", Preset: true, Transport: "unix", Loopback: true, Launch: map[string]any{"type": "pwa-node", "request": "launch", "sourceMaps": false, "outputCapture": "std"}}
	node, err := p.LookPath("node")
	if err != nil {
		adapter.Reason = "node not found on PATH"
		return adapter
	}
	// js-debug is a VS Code extension, not a PATH program: resolve only an
	// explicit host override or an installed `js-debug` executable.
	if path, err := p.LookPath("js-debug"); err == nil {
		adapter.Command = node
		adapter.Args = []string{path, "{socket}"}
		adapter.Available = true
		adapter.Version = p.version(node, "--version")
		adapter.ReadPaths = []string{filepath.Dir(filepath.Dir(node)), filepath.Dir(filepath.Dir(path))}
		return adapter
	}
	adapter.Reason = "js-debug adapter is not installed; configure Debug.Adapters with its dapDebugServer.js path"
	return adapter
}

// discover registers the presets this host can actually run and reports the
// rest with their reason. Configured adapters always win over a preset.
func (m *Manager) discover(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	for _, candidate := range ProbePresets(ctx, m.prober) {
		m.mu.Lock()
		existing, configured := m.adapters[candidate.ID]
		if configured && existing.Version == "" && candidate.Available {
			// A host-configured adapter keeps its own command, but discovery
			// can still report the version it probed.
			existing.Version = candidate.Version
			m.adapters[candidate.ID] = existing
		}
		m.mu.Unlock()
		if configured {
			continue
		}
		if candidate.Available {
			prepared, err := prepareAdapter(candidate, m.opts.MCP)
			if err != nil {
				candidate.Available = false
				candidate.Reason = fmt.Sprintf("adapter unusable: %v", err)
				candidate.Command = ""
				m.mu.Lock()
				m.presets = append(m.presets, candidate)
				m.mu.Unlock()
				continue
			}
			candidate = prepared
			m.mu.Lock()
			m.adapters[candidate.ID] = candidate
			m.mu.Unlock()
		}
		status := candidate
		status.Command = ""
		m.mu.Lock()
		m.presets = append(m.presets, status)
		m.mu.Unlock()
	}
}
