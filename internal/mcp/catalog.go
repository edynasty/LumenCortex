package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/edynasty/LumenCortex/protocol"
)

// catalogFile is the persisted tool list of one server, written when a start discovered it. It lets a
// run offer a server's tools without starting the server, and it is keyed by a fingerprint of the
// server definition so an edited command never reuses the old list.
type catalogFile struct {
	Version     int       `json:"version"`
	ServerID    string    `json:"serverId"`
	Fingerprint string    `json:"fingerprint"`
	RecordedAt  time.Time `json:"recordedAt"`
	Tools       []Tool    `json:"tools"`
}

const catalogVersion = 1

// catalogPath is where one server's catalog lives: <stateDir>/<workspace key>/<server id>.json.
func (r *Registry) catalogPath(key string, cfg Config) string {
	r.stateMu.RLock()
	dir := r.stateDir
	r.stateMu.RUnlock()
	if dir == "" {
		return ""
	}
	workspace := key
	if index := lastIndexByte(key, 0); index >= 0 {
		workspace = key[:index]
	}
	digest := sha256.Sum256([]byte(workspace))
	return filepath.Join(dir, hex.EncodeToString(digest[:])[:16], cfg.ID+".json")
}

func lastIndexByte(value string, b byte) int {
	for i := len(value) - 1; i >= 0; i-- {
		if value[i] == b {
			return i
		}
	}
	return -1
}

// loadCatalog reads the catalog a previous start recorded, when its fingerprint still matches the
// definition in scope.
func (r *Registry) loadCatalog(key string, cfg Config, scope string) ([]Tool, bool) {
	path := r.catalogPath(key, cfg)
	if path == "" {
		return nil, false
	}
	file, ok := readCatalogFile(path)
	if !ok || file.ServerID != cfg.ID || file.Fingerprint != catalogFingerprint(cfg, scope) {
		return nil, false
	}
	return file.Tools, len(file.Tools) > 0
}

// saveCatalog persists a tool list. A write failure is not an error for the caller: the in-memory
// catalog already serves the current process.
func (r *Registry) saveCatalog(key string, cfg Config, scope string, tools []Tool) {
	path := r.catalogPath(key, cfg)
	if path == "" || len(tools) == 0 {
		return
	}
	writeCatalogFile(path, catalogFile{
		Version:     catalogVersion,
		ServerID:    cfg.ID,
		Fingerprint: catalogFingerprint(cfg, scope),
		RecordedAt:  time.Now().UTC(),
		Tools:       tools,
	})
}

// rerecordCatalogs moves the catalogs recorded for before, in every workspace, to after's fingerprint
// when the host's edit through the registry changed only what the fingerprint covers beyond the spawn
// (a project entry's exposure): the tool list a start reported is still the server's, and the host
// chose the new exposure. A catalog recorded for any other definition — one a file edit outside the
// registry produced, or an older spawn — is left as it is. scope "" is the project file, as in
// scopeLocked. The caller holds r.mu, so no other edit of the entry interleaves.
func (r *Registry) rerecordCatalogs(before, after Config, scope string) {
	if scope == "" {
		scope = ScopeProject
	}
	from, to := catalogFingerprint(before, scope), catalogFingerprint(after, scope)
	if from == to || catalogFingerprint(before, ScopeGlobal) != catalogFingerprint(after, ScopeGlobal) {
		return
	}
	for _, path := range r.recordedCatalogPaths(after.ID) {
		file, ok := readCatalogFile(path)
		if !ok || file.ServerID != after.ID || file.Fingerprint != from {
			continue
		}
		file.Fingerprint = to
		writeCatalogFile(path, file)
	}
}

// forgetCatalogs removes the catalogs recorded for a server id in every workspace. The caller holds
// r.mu, so no edit of the entry interleaves.
func (r *Registry) forgetCatalogs(id string) {
	for _, path := range r.recordedCatalogPaths(id) {
		_ = os.Remove(path)
	}
}

// recordedCatalogPaths lists the catalog files of a server id in every workspace, none without a state
// directory.
func (r *Registry) recordedCatalogPaths(id string) []string {
	r.stateMu.RLock()
	dir := r.stateDir
	r.stateMu.RUnlock()
	if dir == "" || !serverIDPattern.MatchString(id) {
		return nil
	}
	// A server id matches [A-Za-z0-9._-], so it is a literal in the pattern.
	paths, _ := filepath.Glob(filepath.Join(dir, "*", id+".json"))
	return paths
}

// readCatalogFile reads one recorded catalog within the runtime's bounds.
func readCatalogFile(path string) (catalogFile, bool) {
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) > MaxCatalogBytes {
		return catalogFile{}, false
	}
	var file catalogFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return catalogFile{}, false
	}
	if file.Version != catalogVersion || len(file.Tools) > MaxTools {
		return catalogFile{}, false
	}
	return file, true
}

// writeCatalogFile replaces one recorded catalog atomically; a failure leaves the previous file.
func writeCatalogFile(path string, file catalogFile) {
	raw, err := json.Marshal(file)
	if err != nil || len(raw) > MaxCatalogBytes {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// toolDeadline is the run's tool deadline, when ctx carries one.
func toolDeadline(ctx context.Context) (time.Time, bool) {
	return protocol.ToolDeadline(ctx)
}
