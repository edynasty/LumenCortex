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

// loadCatalog reads the catalog a previous start recorded, when its fingerprint still matches.
func (r *Registry) loadCatalog(key string, cfg Config) ([]Tool, bool) {
	path := r.catalogPath(key, cfg)
	if path == "" {
		return nil, false
	}
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) > MaxCatalogBytes {
		return nil, false
	}
	var file catalogFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, false
	}
	if file.Version != catalogVersion || file.ServerID != cfg.ID || file.Fingerprint != catalogFingerprint(cfg) {
		return nil, false
	}
	if len(file.Tools) > MaxTools {
		return nil, false
	}
	return file.Tools, len(file.Tools) > 0
}

// saveCatalog persists a tool list. A write failure is not an error for the caller: the in-memory
// catalog already serves the current process.
func (r *Registry) saveCatalog(key string, cfg Config, tools []Tool) {
	path := r.catalogPath(key, cfg)
	if path == "" || len(tools) == 0 {
		return
	}
	file := catalogFile{
		Version:     catalogVersion,
		ServerID:    cfg.ID,
		Fingerprint: catalogFingerprint(cfg),
		RecordedAt:  time.Now().UTC(),
		Tools:       tools,
	}
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
