package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/edynasty/LumenCortex/evalapi"
	"github.com/edynasty/LumenCortex/internal/ipynb"
)

// Durable notebook artifacts. An execution runtime's artifact index and bytes live only as long as
// its kernel session, so a notebook that stored a bare reference would lose its images and files on
// the next start. The notebook therefore copies an artifact's bytes, at execution time, into a
// content-addressed sidecar directory inside the owner's private execution subtree — the same
// subtree Engine.forgetEvalOwner removes with the session. No second database, and the same purge.
//
// The document keeps the reference (id, mime, size, sha256) and never the bytes, so a notebook body
// stays small; the bytes are read back by digest and verified on every read.
const (
	// ExecutionNotebookArtifactQuota bounds the durable artifacts of one session.
	ExecutionNotebookArtifactQuota = 64 << 20
	// ExecutionNotebookArtifactMaxBytes bounds one durable artifact.
	ExecutionNotebookArtifactMaxBytes = 8 << 20
	// notebookArtifactSidecarBytes bounds one artifact's metadata file.
	notebookArtifactSidecarBytes = 4 << 10
)

// notebookArtifactLocks serializes the quota check and the write of one session's artifacts: two
// notebooks of one session must not both pass the quota and then both write. The map holds one mutex
// per session that has stored an artifact in this process; it is small and never grows per artifact.
var notebookArtifactLocks = struct {
	mu   sync.Mutex
	held map[string]*sync.Mutex
}{held: map[string]*sync.Mutex{}}

// lockNotebookArtifacts returns the release of one session's artifact lock.
func lockNotebookArtifacts(sessionID string) func() {
	notebookArtifactLocks.mu.Lock()
	lock := notebookArtifactLocks.held[sessionID]
	if lock == nil {
		lock = &sync.Mutex{}
		notebookArtifactLocks.held[sessionID] = lock
	}
	notebookArtifactLocks.mu.Unlock()
	lock.Lock()
	return lock.Unlock
}

// notebookArtifactMeta is the sidecar of one durable artifact: what produced it and what it is.
type notebookArtifactMeta struct {
	ArtifactID string    `json:"artifactId"`
	MIME       string    `json:"mime"`
	Size       int64     `json:"size"`
	SHA256     string    `json:"sha256"`
	CellID     string    `json:"cellId,omitempty"`
	KernelID   string    `json:"kernelId,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
}

// notebookArtifactDir is the durable artifact directory of one session. It is inside the
// owner-private execution subtree, never a path a model chose; the session id is validated by the
// caller before this is built (checkExecutionOwner), so a separator can never reach the path.
func (e *Engine) notebookArtifactDir(sessionID string) string {
	return filepath.Join(e.repoDir, "eval-workspaces", sessionID, "notebook-artifacts")
}

// checkExecutionOwner refuses a session id that could name a path. It is the same rule
// createEvalSession applies, so every filesystem path of the execution runtime takes one identity.
func checkExecutionOwner(sessionID string) error {
	if sessionID == "" || filepath.Base(sessionID) != sessionID || sessionID == "." || sessionID == ".." {
		return errors.New("invalid execution owner identity")
	}
	return nil
}

// checkArtifactID accepts only the 64-hex digest this store writes, so a reference can never name a
// path outside the directory.
func checkArtifactID(artifactID string) (string, error) {
	id := strings.TrimPrefix(strings.TrimSpace(artifactID), "sha256:")
	if len(id) != 64 {
		return "", errors.New("an artifact id is a sha256 digest")
	}
	if _, err := hex.DecodeString(id); err != nil {
		return "", errors.New("an artifact id is a sha256 digest")
	}
	return strings.ToLower(id), nil
}

// putNotebookArtifact stores one artifact's bytes and returns the document reference. It refuses an
// artifact past the per-artifact or per-session quota, and verifies the digest it writes.
func (h *executionNotebookHost) putNotebookArtifact(ctx context.Context, sessionID string, artifact evalapi.Artifact, data []byte) (ipynb.OutputLC, error) {
	if int64(len(data)) > ExecutionNotebookArtifactMaxBytes {
		return ipynb.OutputLC{}, fmt.Errorf("artifact is %d bytes, more than the %d byte limit", len(data), ExecutionNotebookArtifactMaxBytes)
	}
	if artifact.Size > 0 && artifact.Size != int64(len(data)) {
		return ipynb.OutputLC{}, errors.New("artifact size does not match its bytes")
	}
	sum := sha256.Sum256(data)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if artifact.SHA256 != "" && !strings.EqualFold(artifact.SHA256, digest) && !strings.EqualFold(artifact.SHA256, hex.EncodeToString(sum[:])) {
		return ipynb.OutputLC{}, errors.New("artifact digest does not match its bytes")
	}
	id := hex.EncodeToString(sum[:])
	// The session must be a stored, non-withdrawn LC session before anything is written, and the
	// operation is tracked so a purge waits for it (trackEval does both: it checks the fence and
	// registers the operation).
	if err := checkExecutionOwner(sessionID); err != nil {
		return ipynb.OutputLC{}, err
	}
	ctx, done, err := h.engine.trackEval(ctx, sessionID)
	if err != nil {
		return ipynb.OutputLC{}, err
	}
	defer done()
	unlock := lockNotebookArtifacts(sessionID)
	defer unlock()
	dir := h.engine.notebookArtifactDir(sessionID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ipynb.OutputLC{}, err
	}
	// The same content is stored once: an existing, verified artifact is returned as it is and is not
	// charged to the quota a second time.
	if existing, ok := readStoredArtifact(dir, id); ok && bytes.Equal(existing.data, data) {
		return ipynb.OutputLC{
			ArtifactID: existing.meta.ArtifactID, SessionID: sessionID, MIME: existing.meta.MIME,
			Size: existing.meta.Size, SHA256: existing.meta.SHA256,
		}, nil
	}
	used, err := directoryBytes(dir)
	if err != nil {
		// A quota that cannot be measured is not a quota: refuse the write instead of guessing.
		return ipynb.OutputLC{}, err
	}
	if used+int64(len(data)) > ExecutionNotebookArtifactQuota {
		return ipynb.OutputLC{}, fmt.Errorf("the session's notebook artifacts would exceed the %d byte quota", ExecutionNotebookArtifactQuota)
	}
	meta := notebookArtifactMeta{
		ArtifactID: id, MIME: artifact.MIME, Size: int64(len(data)), SHA256: digest,
		CellID: artifact.CellID, KernelID: artifact.SessionID, CreatedAt: time.Now().UTC(),
	}
	sidecar, err := json.Marshal(meta)
	if err != nil {
		return ipynb.OutputLC{}, err
	}
	if err := writeFileAtomic(ctx, filepath.Join(dir, id+".bin"), data); err != nil {
		return ipynb.OutputLC{}, err
	}
	if err := writeFileAtomic(ctx, filepath.Join(dir, id+".json"), sidecar); err != nil {
		return ipynb.OutputLC{}, err
	}
	return ipynb.OutputLC{
		ArtifactID: id, SessionID: sessionID, MIME: meta.MIME, Size: meta.Size, SHA256: meta.SHA256,
	}, nil
}

// notebookArtifact reads one durable artifact back, verifying its digest and size.
func (h *executionNotebookHost) notebookArtifact(ctx context.Context, sessionID, artifactID string) (ExecutionArtifact, error) {
	id, err := checkArtifactID(artifactID)
	if err != nil {
		return ExecutionArtifact{}, err
	}
	if err := ctx.Err(); err != nil {
		return ExecutionArtifact{}, err
	}
	if err := checkExecutionOwner(sessionID); err != nil {
		return ExecutionArtifact{}, err
	}
	ctx, done, err := h.engine.trackEval(ctx, sessionID)
	if err != nil {
		return ExecutionArtifact{}, err
	}
	defer done()
	unlock := lockNotebookArtifacts(sessionID)
	defer unlock()
	dir := h.engine.notebookArtifactDir(sessionID)
	raw, err := readFileBounded(filepath.Join(dir, id+".json"), notebookArtifactSidecarBytes)
	if err != nil {
		return ExecutionArtifact{}, fmt.Errorf("notebook artifact %s: %w", id, err)
	}
	var meta notebookArtifactMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return ExecutionArtifact{}, fmt.Errorf("notebook artifact %s: %w", id, err)
	}
	// The bytes are read through a bound, so a corrupted or replaced file cannot allocate past it.
	data, err := readFileBounded(filepath.Join(dir, id+".bin"), ExecutionArtifactMaxBytes)
	if err != nil {
		return ExecutionArtifact{}, fmt.Errorf("notebook artifact %s: %w", id, err)
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != id || (meta.Size > 0 && meta.Size != int64(len(data))) {
		return ExecutionArtifact{}, fmt.Errorf("notebook artifact %s does not match its recorded digest", id)
	}
	return ExecutionArtifact{
		ArtifactID: id, SessionID: sessionID, CellID: meta.CellID, MIME: meta.MIME,
		Size: meta.Size, SHA256: meta.SHA256, CreatedAt: meta.CreatedAt, Data: data,
	}, nil
}

// storedArtifact is one artifact read back for verification.
type storedArtifact struct {
	meta notebookArtifactMeta
	data []byte
}

// readStoredArtifact reads an artifact's sidecar and bytes when both exist and the digest matches.
// ok is false for anything that is not a verified copy.
func readStoredArtifact(dir, id string) (storedArtifact, bool) {
	raw, err := readFileBounded(filepath.Join(dir, id+".json"), notebookArtifactSidecarBytes)
	if err != nil {
		return storedArtifact{}, false
	}
	var meta notebookArtifactMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return storedArtifact{}, false
	}
	data, err := readFileBounded(filepath.Join(dir, id+".bin"), ExecutionArtifactMaxBytes)
	if err != nil {
		return storedArtifact{}, false
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != id {
		return storedArtifact{}, false
	}
	return storedArtifact{meta: meta, data: data}, true
}

// readFileBounded reads at most limit bytes, refusing a file that is larger before it allocates it.
func readFileBounded(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("%s is %d bytes, more than the %d byte limit", filepath.Base(path), info.Size(), limit)
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s is more than the %d byte limit", filepath.Base(path), limit)
	}
	return data, nil
}

// directoryBytes is the size of the artifact files a directory holds.
func directoryBytes(dir string) (int64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	var total int64
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".bin") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return total, err
		}
		total += info.Size()
	}
	return total, nil
}

// writeFileAtomic writes one file through a temporary name, so a crash leaves either the old file or
// the new one and never a half-written artifact.
func writeFileAtomic(ctx context.Context, path string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".artifact-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		os.Remove(name)
		return err
	}
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		os.Remove(name)
		return err
	}
	if err := temp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}
