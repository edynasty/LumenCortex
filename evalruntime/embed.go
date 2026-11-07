package evalruntime

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"os"
	"path/filepath"
)

// Worker scripts are embedded so a Core build carries its interpreters' glue: no
// install step, no network, no writable source directory. They are materialized
// under Root/workers/<content hash>/ and only read from there.
//
//go:embed workers/py_worker.py workers/js_worker.js
var workerScripts embed.FS

const (
	pythonWorkerName     = "py_worker.py"
	javascriptWorkerName = "js_worker.js"
)

// materializeWorkers writes the embedded scripts and returns their directory.
func materializeWorkers(root string) (string, error) {
	names := []string{pythonWorkerName, javascriptWorkerName}
	sum := sha256.New()
	contents := make(map[string][]byte, len(names))
	for _, name := range names {
		data, err := workerScripts.ReadFile("workers/" + name)
		if err != nil {
			return "", err
		}
		contents[name] = data
		sum.Write(data)
	}
	digest := hex.EncodeToString(sum.Sum(nil))[:16]
	dir := filepath.Join(root, "workers", digest)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	for _, name := range names {
		path := filepath.Join(dir, name)
		existing, err := os.ReadFile(path)
		if err == nil && string(existing) == string(contents[name]) {
			continue
		}
		if err := os.WriteFile(path, contents[name], 0o644); err != nil {
			return "", err
		}
	}
	return dir, nil
}
