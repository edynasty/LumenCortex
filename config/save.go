package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/edynasty/LumenCortex/internal/runlock"
)

// filePerm is the mode of a saved file. It holds {env:NAME} references and headers, not keys, but
// headers may carry tokens, so it is private to the user.
const (
	filePerm = 0o600
	dirPerm  = 0o700
)

// saveLockWait bounds the wait for another Save of the same file.
const saveLockWait = 5 * time.Second

// Save validates cfg against the schema and writes it to path. It is the only way core writes
// lumencortex.json.
//
// expectedDigest is the Document.Digest the caller read, or "" when the file did not exist. If the
// digest of the file now on disk differs, nothing is written and ErrConflict results, so a
// concurrent writer is never silently overwritten. Literal API keys and every other schema
// violation fail with ErrInvalidConfig before anything is touched.
//
// Savers of one file are serialized by an advisory lock outside the workspace, so the digest check
// and the rename are one step among Save callers (Desktop, the CLI). The document goes to a
// temporary file in the same directory, is flushed, then renamed over path: a crash leaves the old
// or the new file, never a mix, and a failure removes the temporary file. A writer that bypasses
// Save is caught by the digest check but can still race the rename itself.
//
// It returns the digest of the written file.
func Save(path string, cfg File, expectedDigest string) (string, error) {
	if err := Validate(cfg); err != nil {
		return "", err
	}
	raw, err := marshalFile(cfg)
	if err != nil {
		return "", err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("config save: %w", err)
	}
	// Writing through a symlink replaces its target, not the link.
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	unlock, err := lockSave(path)
	if err != nil {
		return "", err
	}
	defer unlock()
	if err := checkDigest(path, expectedDigest); err != nil {
		return "", err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return "", fmt.Errorf("config save: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return "", fmt.Errorf("config save: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if err := tmp.Chmod(filePerm); err != nil {
		return "", fmt.Errorf("config save: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		return "", fmt.Errorf("config save: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return "", fmt.Errorf("config save: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("config save: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", fmt.Errorf("config save: %w", err)
	}
	committed = true
	_ = syncDir(dir)
	return Digest(raw), nil
}

// lockSave takes the per-file save lock. The lock file lives in the user's temporary directory,
// named by the hash of the target path, so it never appears in a workspace or the config directory.
func lockSave(path string) (func(), error) {
	sum := sha256.Sum256([]byte(path))
	dir := filepath.Join(os.TempDir(), "lumencortex-config-locks-"+strconv.Itoa(os.Getuid()))
	id := hex.EncodeToString(sum[:16])
	deadline := time.Now().Add(saveLockWait)
	for {
		l, err := runlock.TryAcquire(dir, id)
		if err == nil {
			return func() { _ = l.Release() }, nil
		}
		if !errors.Is(err, runlock.ErrHeld) {
			return nil, fmt.Errorf("config save: lock: %w", err)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("config save: %s is being saved by another process", path)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func checkDigest(path, expected string) error {
	current := ""
	raw, err := readBounded(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return fmt.Errorf("config save: reading current %s: %w", path, err)
	default:
		current = Digest(raw)
	}
	if current != expected {
		return fmt.Errorf("%w: %s", ErrConflict, path)
	}
	return nil
}

func marshalFile(cfg File) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(cfg); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}
	return buf.Bytes(), nil
}
