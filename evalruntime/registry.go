package evalruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/edynasty/LumenCortex/evalapi"
)

// provider is one registered runtime. Registration is a claim; availability is
// decided by the host (platform, executable, sandbox backend, dependencies).
type provider struct {
	manifest  evalapi.Manifest
	builtin   bool
	available bool
	reason    string
	readPaths []string
}

type registry struct {
	mu        sync.RWMutex
	providers map[string]*provider
}

func newRegistry() *registry {
	return &registry{providers: make(map[string]*provider)}
}

// validateManifest enforces the parts of the SPI a plugin cannot negotiate away.
func validateManifest(m evalapi.Manifest) error {
	if strings.TrimSpace(m.ID) == "" {
		return fmt.Errorf("%w: manifest id is required", evalapi.ErrInvalid)
	}
	if strings.ContainsAny(m.ID, "/\\ \t\n") {
		return fmt.Errorf("%w: manifest id %q contains separators or spaces", evalapi.ErrInvalid, m.ID)
	}
	if strings.TrimSpace(m.Version) == "" {
		return fmt.Errorf("%w: manifest %q has no version", evalapi.ErrInvalid, m.ID)
	}
	if strings.TrimSpace(m.SPIVersion) == "" {
		return fmt.Errorf("%w: manifest %q declares no SPI version", evalapi.ErrInvalid, m.ID)
	}
	if major(m.SPIVersion) != major(evalapi.SPIVersion) {
		return fmt.Errorf("%w: manifest %q speaks SPI %s, host speaks %s", evalapi.ErrUnsupportedCapability, m.ID, m.SPIVersion, evalapi.SPIVersion)
	}
	if strings.TrimSpace(m.Language) == "" {
		return fmt.Errorf("%w: manifest %q declares no language", evalapi.ErrInvalid, m.ID)
	}
	if strings.TrimSpace(m.Executable) == "" {
		return fmt.Errorf("%w: manifest %q declares no executable", evalapi.ErrInvalid, m.ID)
	}
	for _, c := range m.Capabilities {
		if strings.TrimSpace(c.Name) == "" {
			return fmt.Errorf("%w: manifest %q has a capability with no name", evalapi.ErrInvalid, m.ID)
		}
	}
	return nil
}

func major(version string) string {
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	if i := strings.IndexByte(version, '.'); i >= 0 {
		return version[:i]
	}
	return version
}

func (r *registry) register(m evalapi.Manifest, builtin bool, available bool, reason string) error {
	if err := validateManifest(m); err != nil {
		return err
	}
	if available {
		resolved, err := trustedExecutable(m.Executable)
		if err != nil {
			return err
		}
		m.Executable = resolved
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.providers[m.ID]; exists {
		return fmt.Errorf("%w: runtime %q is already registered", evalapi.ErrInvalid, m.ID)
	}
	r.providers[m.ID] = &provider{manifest: m, builtin: builtin, available: available, reason: reason}
	return nil
}

func (r *registry) get(id string) (*provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[id]
	return p, ok
}

// discover returns the available manifests, sorted by id. An unavailable runtime
// is not offered: discovery never implies a capability the host cannot deliver.
func (r *registry) discover() []evalapi.Manifest {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]evalapi.Manifest, 0, len(r.providers))
	for _, p := range r.providers {
		if !p.available {
			continue
		}
		out = append(out, p.manifest)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// pythonCapabilities probes the interpreter once. Discovery may only find what is
// already installed; it never installs anything.
func pythonCapabilities(exe string) map[string]bool {
	found := map[string]bool{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	script := `import importlib.util as u
mods = ["numpy", "scipy", "pandas", "matplotlib"]
print(" ".join(m for m in mods if u.find_spec(m) is not None))`
	cmd := exec.CommandContext(ctx, exe, "-I", "-c", script)
	out, err := cmd.Output()
	if err != nil {
		return found
	}
	for _, name := range strings.Fields(string(out)) {
		found[name] = true
	}
	return found
}

func pythonManifest(exe, worker string, caps map[string]bool, limits evalapi.Limits, allowSubprocess bool) evalapi.Manifest {
	capabilities := []evalapi.Capability{
		{Name: evalapi.CapPersistent, Supported: true},
		{Name: evalapi.CapModules, Supported: true},
		{Name: evalapi.CapFileIO, Supported: true},
		{Name: evalapi.CapArtifact, Supported: true},
		{Name: evalapi.CapDisplay, Supported: true},
		{Name: evalapi.CapToolCallback, Supported: true},
		{Name: evalapi.CapInterrupt, Supported: true, Notes: "SIGINT to the process group; a C extension that ignores it is escalated to group termination and the kernel is lost"},
		{Name: evalapi.CapSubprocess, Supported: allowSubprocess, Notes: subprocessNote(allowSubprocess)},
		{Name: evalapi.CapNotebook, Supported: true},
		{Name: evalapi.CapInspect, Supported: true, Notes: "names and types of live globals; values are never rendered"},
		{Name: evalapi.CapScientific, Supported: caps["numpy"] && caps["scipy"], Notes: dependencyProbe(caps, "numpy", "scipy", "pandas")},
		{Name: evalapi.CapPlotting, Supported: caps["matplotlib"], Notes: dependencyProbe(caps, "matplotlib")},
	}
	return evalapi.Manifest{
		ID:             "python",
		Version:        "1.0.0",
		SPIVersion:     evalapi.SPIVersion,
		Language:       "python",
		RuntimeVersion: "",
		Executable:     exe,
		Args:           []string{worker},
		Capabilities:   capabilities,
		Dependencies:   []string{"python3"},
		Platforms:      []string{"darwin", "linux"},
		Sandbox:        evalapi.SandboxRequirement{Required: true, Backends: []string{evalapi.SandboxSeatbelt, evalapi.SandboxBubblewrap}, Network: false},
		Limits:         limits,
		ArtifactTypes:  []string{"text/plain", "text/csv", "application/json", "image/png", "image/svg+xml", "application/octet-stream"},
		HealthCheck:    "initialize",
		Description:    "Persistent CPython kernel with the scientific stack when it is already installed.",
	}
}

func javascriptManifest(exe, worker string, limits evalapi.Limits, allowSubprocess bool) evalapi.Manifest {
	return evalapi.Manifest{
		ID:         "javascript",
		Version:    "1.0.0",
		SPIVersion: evalapi.SPIVersion,
		Language:   "javascript",
		Executable: exe,
		Args:       []string{worker},
		Capabilities: []evalapi.Capability{
			{Name: evalapi.CapPersistent, Supported: true},
			{Name: evalapi.CapModules, Supported: true, Notes: "require() and await import() in the kernel workspace"},
			{Name: evalapi.CapFileIO, Supported: true},
			{Name: evalapi.CapArtifact, Supported: true},
			{Name: evalapi.CapDisplay, Supported: true},
			{Name: evalapi.CapToolCallback, Supported: true},
			{Name: evalapi.CapInterrupt, Supported: false, Notes: "synchronous V8 execution cannot be preempted in-process; the host terminates the worker group and the kernel state is lost"},
			{Name: evalapi.CapSubprocess, Supported: allowSubprocess, Notes: subprocessNote(allowSubprocess)},
			{Name: evalapi.CapNotebook, Supported: true},
			{Name: evalapi.CapInspect, Supported: true, Notes: "names and types of the kernel state; values are never rendered"},
		},
		Dependencies:  []string{"node"},
		Platforms:     []string{"darwin", "linux"},
		Sandbox:       evalapi.SandboxRequirement{Required: true, Backends: []string{evalapi.SandboxSeatbelt, evalapi.SandboxBubblewrap}, Network: false},
		Limits:        limits,
		ArtifactTypes: []string{"text/plain", "text/csv", "application/json", "image/png", "application/octet-stream"},
		HealthCheck:   "initialize",
		Description:   "Persistent Node.js kernel with a vm context per session.",
	}
}

func dependencyProbe(caps map[string]bool, names ...string) string {
	var present, missing []string
	for _, name := range names {
		if caps[name] {
			present = append(present, name)
		} else {
			missing = append(missing, name)
		}
	}
	note := ""
	if len(present) > 0 {
		note = "installed: " + strings.Join(present, ", ")
	}
	if len(missing) > 0 {
		if note != "" {
			note += "; "
		}
		note += "not installed: " + strings.Join(missing, ", ")
	}
	return note
}

// subprocessNote states the containment trade-off exactly.
func subprocessNote(allow bool) string {
	if !allow {
		return "disabled: the sandbox denies fork, so the owned process tree is exactly one process and cleanup is complete; delegate external operations through host tool callbacks"
	}
	return "enabled: every descendant, detached or not, inherits the worker's cgroup and is stopped with it by cgroup.kill; pids.max bounds the task count"
}

// platformLimits reports how this host actually enforces each bound. memory is
// the enforcement the host verified it can apply (hard: Linux cgroup memory.max;
// supervised: macOS RSS polling — darwin rejects RLIMIT_AS and RLIMIT_DATA with
// EINVAL; runtime: only when the interpreter has its own heap cap; none: no
// bound is applied).
func platformLimits(backend string, budget evalapi.Budget, allowSubprocess bool, memory string) evalapi.Limits {
	limits := evalapi.Limits{
		WallSeconds:       budget.WallSeconds,
		CPUSeconds:        budget.CPUSeconds,
		MemoryBytes:       budget.MemoryBytes,
		OutputBytes:       budget.OutputBytes,
		ArtifactBytes:     budget.ArtifactBytes,
		DiskBytes:         budget.DiskBytes,
		MemoryEnforcement: memory,
		// RLIMIT_CPU (ulimit -t) is enforced by the kernel on every backend.
		CPUEnforcement:  evalapi.EnforcementHard,
		DiskEnforcement: evalapi.EnforcementHard,
	}
	limits.Notes = []string{
		"wall clock: host timer, then SIGINT and SIGKILL to the process group",
		"cpu: RLIMIT_CPU via ulimit -t (kernel-enforced)",
		memoryNote(memory),
		"disk: RLIMIT_FSIZE via ulimit -f bounds one file (hard); the kernel workspace's total size is host-supervised every 400ms against DiskBytes and the worker is terminated on exceed (supervised, not a kernel hard limit); artifacts also have an explicit quota",
		"output: bounded while streaming (head+tail), never buffered in full",
		processTreeNote(allowSubprocess),
	}
	if backend == evalapi.SandboxBubblewrap && memory == evalapi.EnforcementHard {
		limits.Notes = append(limits.Notes, "cpu: cgroup v2 cpu.max=100000 100000 limits each complete worker tree to one CPU, in addition to RLIMIT_CPU")
	}
	if backend == "" {
		limits.MemoryEnforcement = evalapi.EnforcementNone
		limits.CPUEnforcement = evalapi.EnforcementNone
		limits.DiskEnforcement = evalapi.EnforcementNone
	}
	return limits
}

// memoryNote states how memory is bounded on this host.
func memoryNote(memory string) string {
	switch memory {
	case evalapi.EnforcementHard:
		return "memory: kernel hard limit per worker: cgroup v2 memory.max=MemoryBytes with memory.swap.max=0 and memory.oom.group=1, applied before the worker exists and covering every descendant; exceeding it OOM-kills the whole worker and the kernel is lost. The total is at most MaxWorkers x MemoryBytes and is not enforced as an aggregate"
	case evalapi.EnforcementSupervised:
		return "memory: per-worker process-group RSS, host-supervised every 200ms and killed on exceed or when it cannot be measured; not a kernel rlimit because macOS rejects RLIMIT_AS/RLIMIT_DATA with EINVAL, so the process may overshoot the bound by what it allocates within one poll interval. The total is at most MaxWorkers x MemoryBytes and is not enforced as an aggregate"
	}
	return "memory: not bounded on this host"
}

// workerTasksMax bounds the processes and threads of one Linux worker
// (pids.max). It stops a fork bomb inside the sandbox while leaving room for
// thread pools (BLAS, libuv) and ordinary subprocess use.
const workerTasksMax = 512

// processTreeNote describes the actual tree guarantee of this host.
func processTreeNote(allowSubprocess bool) string {
	if !allowSubprocess {
		return "process tree: fork is denied by the sandbox, so the kernel owns exactly one process; stop, reset and dispose terminate it and no descendant can exist"
	}
	return fmt.Sprintf("process tree: the worker is admitted into its own cgroup v2 leaf before it executes (started behind a gate, moved by the user's systemd manager, membership verified), inside a private PID namespace, so every descendant is a member; stop, reset, timeout and dispose write cgroup.kill and wait until the kernel reports the leaf empty, then remove it; pids.max=%d. Surviving members are reported by host pid and the stop is then not clean", workerTasksMax)
}

// loadPluginDir registers manifest JSON files a host placed under Root/plugins.
// A malformed or incompatible file is skipped silently — it is not reported as an
// unavailable runtime, because a file that does not parse has no id to report —
// and it cannot disable the built-in runtimes. A plugin that parses but whose
// executable is missing is registered as unavailable and named in Discover's
// absence plus Create's error.
func (r *registry) loadPluginDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var m evalapi.Manifest
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		available, reason := pluginAvailable(m)
		_ = r.register(m, false, available, reason)
	}
	return nil
}

func pluginAvailable(m evalapi.Manifest) (bool, string) {
	if !m.SupportsPlatform(platformName()) {
		return false, fmt.Sprintf("platform %s is not supported by the plugin", platformName())
	}
	if _, err := trustedExecutable(m.Executable); err != nil {
		return false, err.Error()
	}
	return true, ""
}
