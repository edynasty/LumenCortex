package evalruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/evalapi"
)

// TestPutArtifactLandsInKernelWorkspace proves the import path: host-owned bytes
// become an indexed artifact and a file the runtime can read by name.
func TestPutArtifactLandsInKernelWorkspace(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "owner", "owner")
	artifact, err := h.m.PutArtifact(context.Background(), session.ID, "owner", "input.csv", "text/csv", []byte("a,b\n1,2\n"))
	if err != nil {
		t.Fatalf("PutArtifact: %v", err)
	}
	if artifact.Name != "input.csv" || artifact.MIME != "text/csv" || artifact.Size != 8 || artifact.SHA256 == "" {
		t.Fatalf("artifact = %+v", artifact)
	}
	result := h.expectCompleted(session, "owner", `print(open("input.csv").read().strip())`)
	if !strings.Contains(result.Stdout, "a,b") {
		t.Fatalf("runtime could not read the imported file: %q", result.Stdout)
	}
	meta, data, err := h.m.Artifact(context.Background(), session.ID, "owner", artifact.ID)
	if err != nil {
		t.Fatalf("Artifact: %v", err)
	}
	if string(data) != "a,b\n1,2\n" || meta.SHA256 != artifact.SHA256 {
		t.Fatalf("artifact bytes/metadata mismatch")
	}
	// A different agent cannot read it.
	if _, _, err := h.m.Artifact(context.Background(), session.ID, "stranger", artifact.ID); !errors.Is(err, evalapi.ErrDenied) {
		t.Fatalf("cross-agent artifact read = %v", err)
	}
	// Nor can an observer write one.
	if _, err := h.m.PutArtifact(context.Background(), session.ID, "stranger", "x.txt", "text/plain", []byte("x")); !errors.Is(err, evalapi.ErrDenied) {
		t.Fatalf("cross-agent PutArtifact = %v", err)
	}
}

// TestCrossLanguageArtifactTransfer is the Python -> JavaScript -> Python path of
// §13: the artifact is the only channel, and provenance is explicit.
func TestCrossLanguageArtifactTransfer(t *testing.T) {
	pyExe := requirePython(t)
	nodeExe := requireNode(t)
	h := newHarness(t, func(opts *Options) {
		opts.Python = pyExe
		opts.JavaScript = nodeExe
	})
	python := h.create("python", "owner", "owner")
	javascript := h.create("javascript", "owner", "owner")

	produced := h.expectCompleted(python, "owner", `
import json
lc_artifact(json.dumps({"values": [1, 2, 3]}), name="data.json", mime="application/json")
print("written")
`)
	if len(produced.Artifacts) != 1 {
		t.Fatalf("python artifacts = %+v", produced.Artifacts)
	}
	source := produced.Artifacts[0]

	js := h.execRequest(javascript, "owner", evalapi.Request{
		ID:     "js-import-ok",
		CellID: "cell",
		Code: `
const fs = await import('node:fs/promises');
const parsed = JSON.parse(await fs.readFile('data.json', 'utf8'));
const total = parsed.values.reduce((a, b) => a + b, 0);
lc_artifact(String(total), 'total.txt', 'text/plain');
total
`,
		Inputs: []evalapi.Input{{SessionID: python.ID, ArtifactID: source.ID, Name: "data.json"}},
	}, nil)
	if !js.Succeeded() {
		t.Fatalf("javascript could not read the imported artifact: %+v", js)
	}
	if len(js.Displays) == 0 || js.Displays[0].Text != "6" {
		t.Fatalf("javascript result = %+v", js.Displays)
	}
	if len(js.Artifacts) != 1 {
		t.Fatalf("javascript artifacts = %+v", js.Artifacts)
	}
	total := js.Artifacts[0]
	meta, data, err := h.m.Artifact(context.Background(), javascript.ID, "owner", total.ID)
	if err != nil {
		t.Fatalf("Artifact: %v", err)
	}
	if meta.MIME != "text/plain" || string(data) != "6" {
		t.Fatalf("round-trip artifact = %+v %q", meta, data)
	}

	// Back to Python: the second hop proves the loop closes across runtimes.
	back := h.execRequest(python, "owner", evalapi.Request{
		ID:     "py-back",
		CellID: "cell",
		Code:   `print(open("total.txt").read())`,
		Inputs: []evalapi.Input{{SessionID: javascript.ID, ArtifactID: total.ID, Name: "total.txt"}},
	}, nil)
	if !back.Succeeded() || !strings.Contains(back.Stdout, "6") {
		t.Fatalf("python could not read the javascript artifact: %+v", back)
	}
}

func TestArtifactQuotaIsEnforced(t *testing.T) {
	h, _ := newPythonHarness(t, func(opts *Options) {
		opts.Budget.ArtifactBytes = 1024
	})
	session := h.create("python", "owner", "owner")
	result := h.exec(session, "owner", `
try:
    lc_artifact(b"x" * 4096, name="big.bin")
    print("accepted")
except ValueError as exc:
    print("rejected", exc)
`, evalapi.Budget{ArtifactBytes: 1024})
	if !result.Succeeded() {
		t.Fatalf("cell failed: %+v", result)
	}
	if strings.Contains(result.Stdout, "accepted") || len(result.Artifacts) != 0 {
		t.Fatalf("oversized artifact was accepted: %q %+v", result.Stdout, result.Artifacts)
	}
	if !strings.Contains(result.Stdout, "rejected") {
		t.Fatalf("stdout = %q", result.Stdout)
	}
	if _, err := h.m.PutArtifact(context.Background(), session.ID, "owner", "big.bin", "application/octet-stream", make([]byte, 4096)); !errors.Is(err, evalapi.ErrLimit) {
		t.Fatalf("PutArtifact over quota = %v", err)
	}
}

func TestArtifactIntegrityIsVerifiedOnRead(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "owner", "owner")
	result := h.expectCompleted(session, "owner", `lc_artifact("stable", name="note.txt", mime="text/plain")`)
	if len(result.Artifacts) != 1 {
		t.Fatalf("artifacts = %+v", result.Artifacts)
	}
	artifact := result.Artifacts[0]
	// Tamper with the stored file: the manager must report the change, not serve it.
	internal, err := h.m.session(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(internal.artifactDir, artifact.ID)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := os.WriteFile(path, []byte("tampered"), 0o600); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	if _, _, err := h.m.Artifact(context.Background(), session.ID, "owner", artifact.ID); !errors.Is(err, evalapi.ErrInvalid) {
		t.Fatalf("tampered artifact read = %v", err)
	}
}

func TestArtifactNamesRejectTraversal(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "owner", "owner")
	if _, err := h.m.PutArtifact(context.Background(), session.ID, "owner", "../../escape.txt", "text/plain", []byte("x")); !errors.Is(err, evalapi.ErrInvalid) {
		t.Fatalf("traversal accepted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.workspace, "escape.txt")); err == nil {
		t.Fatal("artifact escaped kernel")
	}
}
