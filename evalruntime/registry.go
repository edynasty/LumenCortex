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
	return "enabled by host opt-in: descendants are recorded from fork events and a 150ms scan, survivors are reported, and completeness is not claimed (a process that detaches and whose parent exits before observation cannot be reported)"
}

// platformLimits reports how this host actually enforces each bound.
func platformLimits(backend string, budget evalapi.Budget, allowSubprocess bool) evalapi.Limits {
	limits := evalapi.Limits{
		WallSeconds:       budget.WallSeconds,
		CPUSeconds:        budget.CPUSeconds,
		MemoryBytes:       budget.MemoryBytes,
		OutputBytes:       budget.OutputBytes,
		ArtifactBytes:     budget.ArtifactBytes,
		DiskBytes:         budget.DiskBytes,
		MemoryEnforcement: evalapi.EnforcementMonitored,
		CPUEnforcement:    evalapi.EnforcementHard,
		DiskEnforcement:   evalapi.EnforcementHard,
	}
	limits.Notes = []string{
		"wall clock: host timer, then SIGINT and SIGKILL to the process group",
		"cpu: RLIMIT_CPU via ulimit -t (kernel-enforced)",
		"memory: per-worker process-group RSS, host-monitored every 200ms and killed on exceed; not a kernel rlimit because macOS rejects RLIMIT_AS/RLIMIT_DATA. The total is at most MaxWorkers x MemoryBytes and is not enforced as an aggregate",
		"disk: RLIMIT_FSIZE via ulimit -f bounds one file; the kernel workspace's total size is host-monitored every 400ms against DiskBytes and the worker is terminated on exceed (monitored, not a kernel hard limit); artifacts also have an explicit quota",
		"output: bounded while streaming (head+tail), never buffered in full",
		processTreeNote(allowSubprocess),
	}
	if backend == "" {
		limits.MemoryEnforcement = evalapi.EnforcementNone
		limits.CPUEnforcement = evalapi.EnforcementNone
		limits.DiskEnforcement = evalapi.EnforcementNone
	}
	return limits
}

// processTreeNote describes the actual tree guarantee of this host.
func processTreeNote(allowSubprocess bool) string {
	if !allowSubprocess {
		return "process tree: fork is denied by the sandbox, so the kernel owns exactly one process; stop, reset and dispose terminate it and no descendant can exist"
	}
	return "process tree: the worker leads a process group; descendants are recorded from kernel fork events (macOS kqueue NOTE_FORK) plus a 150ms parent-link scan. At teardown the group and every recorded descendant are SIGKILLed and surviving pids are reported. A descendant that detaches and whose parent exits before observation cannot be reported, so the host never claims that every descendant was found or that a stop was clean"
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
	exe := m.Executable
	if !filepath.IsAbs(exe) {
		resolved, err := exec.LookPath(exe)
		if err != nil {
			return false, fmt.Sprintf("executable %q is not installed", exe)
		}
		exe = resolved
	}
	if _, err := os.Stat(exe); err != nil {
		return false, fmt.Sprintf("executable %q is missing", exe)
	}
	return true, ""
}
