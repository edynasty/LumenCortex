package evalruntime

import (
	"context"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/evalapi"
)

func TestSandboxFailsClosedWithoutABackend(t *testing.T) {
	_, err := newLauncher(SandboxOptions{Backend: "docker-desktop-maybe"})
	if !errors.Is(err, evalapi.ErrSandboxUnsupported) {
		t.Fatalf("unknown backend = %v", err)
	}
	_, err = New(Options{Root: t.TempDir(), Sandbox: SandboxOptions{Backend: "none"}})
	if !errors.Is(err, evalapi.ErrSandboxUnsupported) {
		t.Fatalf("New with an unsupported backend = %v", err)
	}
}

func TestSeatbeltProfileDeniesHostContentsAndAllowsTheKernel(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("seatbelt profile is macOS-only")
	}
	spec := launchSpec{
		exe:         "/opt/miniconda3/envs/x/bin/python3",
		kernelDir:   "/tmp/ws/kernel-0",
		profilePath: "/tmp/ws/kernel-0/sandbox.sb",
		workersDir:  "/tmp/state/workers/abc",
	}
	profile := seatbeltProfile(spec)
	for _, required := range []string{
		"(deny default)",
		"(deny network*)",
		`(allow file-read-metadata)`,
		`(allow file-read-data (vnode-type DIRECTORY))`,
		`(subpath "/System")`,
		`(subpath "/usr")`,
		`(subpath (param "PREFIX"))`,
		`(subpath (param "WORKERS"))`,
		`(param "KERNEL")`,
		`(subpath "/private/etc/ssh")`,
		`(literal "/bin/bash")`,
	} {
		if !strings.Contains(profile, required) {
			t.Fatalf("profile is missing %q:\n%s", required, profile)
		}
	}
	// Contents are deny-by-default: there is no broad file-read allow.
	if strings.Contains(profile, "(allow file-read*)\n") {
		t.Fatalf("profile grants unrestricted file reads:\n%s", profile)
	}
	// Fork is denied by default: the owned tree is exactly one process.
	if strings.Contains(profile, "(allow process*)") {
		t.Fatalf("default profile must not allow arbitrary forking:\n%s", profile)
	}
	// No ancestor of the kernel may be denied: that breaks process.cwd().
	for _, ancestor := range []string{`(subpath "/Users")`, `(subpath "/private/tmp")`, `(subpath "/private/var/folders")`} {
		if strings.Contains(profile, ancestor) {
			t.Fatalf("profile denies an ancestor %q, which breaks process.cwd():\n%s", ancestor, profile)
		}
	}
	// The kernel is writable, and the kernel allow comes after the deny rules.
	if !strings.Contains(profile, `(allow file-read* file-write* (subpath (param "KERNEL")))`) {
		t.Fatalf("kernel is not writable:\n%s", profile)
	}
	// Linux relies on a private PID namespace, so it keeps fork.
	permissive := seatbeltProfile(launchSpec{allowSubprocess: true})
	if !strings.Contains(permissive, "(allow process*)") {
		t.Fatalf("a subprocess-capable host must allow forking:\n%s", permissive)
	}
}

func TestSeatbeltProfileIsWrittenPerKernel(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("seatbelt profile is macOS-only")
	}
	h, _ := newFixtureHarness(t)
	session := h.create("fixture", "owner", "owner")
	profile := session.Workspace + "/sandbox.sb"
	data, err := os.ReadFile(profile)
	if err != nil {
		t.Fatalf("profile was not written: %v", err)
	}
	// The profile names its kernel and the worker scripts through -D parameters so
	// paths with spaces or symlinks cannot break the rules.
	if !strings.Contains(string(data), `(param "KERNEL")`) {
		t.Fatalf("profile does not name its kernel:\n%s", data)
	}
	if !strings.Contains(string(data), `(param "WORKERS")`) {
		t.Fatalf("profile does not allow the worker scripts:\n%s", data)
	}
}

func TestWorkerScriptsAreMaterialized(t *testing.T) {
	h := newHarness(t, nil)
	for _, name := range []string{pythonWorkerName, javascriptWorkerName} {
		if _, err := os.ReadFile(h.m.workersDir + "/" + name); err != nil {
			t.Fatalf("worker script %s: %v", name, err)
		}
	}
	// Materialization is idempotent and content-addressed.
	again, err := materializeWorkers(h.m.root)
	if err != nil {
		t.Fatalf("materializeWorkers: %v", err)
	}
	if again != h.m.workersDir {
		t.Fatalf("worker dir changed: %s -> %s", h.m.workersDir, again)
	}
}

func TestManifestLimitsDescribeRealEnforcement(t *testing.T) {
	h := newHarness(t, nil)
	manifests, err := h.m.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(manifests) == 0 {
		t.Skip("no runtime available in this environment")
	}
	for _, manifest := range manifests {
		if manifest.Limits.MemoryEnforcement == "" {
			t.Fatalf("%s does not declare how memory is enforced", manifest.ID)
		}
		if manifest.Limits.MemoryEnforcement == evalapi.EnforcementHard && runtime.GOOS == "darwin" {
			t.Fatalf("%s claims a hard memory limit macOS cannot provide", manifest.ID)
		}
		if manifest.Limits.CPUEnforcement != evalapi.EnforcementHard {
			t.Fatalf("%s does not enforce CPU", manifest.ID)
		}
	}
}
