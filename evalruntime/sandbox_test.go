package evalruntime

import (
	"context"
	"errors"
	"os"
	"runtime"
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
