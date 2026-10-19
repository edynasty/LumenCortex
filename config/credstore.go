package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/edynasty/LumenCortex/internal/runlock"
)

// CredentialStoreFileName is the credential store file inside GlobalDir. It holds {secret:NAME}
// values only: lumencortex.json keeps the references (I12).
const CredentialStoreFileName = "credentials.json"

// maxSecretBytes bounds one stored secret; real API keys are a few hundred bytes.
const maxSecretBytes = 8 << 10

// DefaultCredentialStorePath is the default store location, GlobalDir/credentials.json, using the
// same directory resolution as every other global file (XDG-aware, config/dir.go). It does not
// create anything.
func DefaultCredentialStorePath() (string, error) {
	dir, err := GlobalDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, CredentialStoreFileName), nil
}

// FileCredentialStore is the file-backed CredentialResolver for {secret:NAME} references: one JSON
// object of name to value in one private file (0600 in a 0700 directory), shared by every Core host
// (the CLI and Desktop resolve through the same default path).
//
// It is not a secrets manager: the file is not encrypted at rest. It is protected by file
// permissions and the user account, the same trust level as an environment variable, and it is
// never exported, logged or sent to a frontend. The only APIs are per-name; nothing returns every
// value.
//
// Concurrency: writers are serialized by an advisory run lock (like Save), and a write replaces
// the whole file through a temporary file, fsync and rename, so a reader always sees the complete
// old or new file, and a crash leaves one of the two, never a mix.
type FileCredentialStore struct {
	// path is the only field: the values never live in the struct, so no fmt %v of the store, no
	// JSON of it and no reflection dump can carry one.
	path string
}

// NewFileCredentialStore returns the store backed by path. No file is read or created until an
// operation runs.
func NewFileCredentialStore(path string) *FileCredentialStore {
	return &FileCredentialStore{path: path}
}

// Path is the file the store reads and writes.
func (s *FileCredentialStore) Path() string { return s.path }

// Resolve is Get, so the store satisfies CredentialResolver.
func (s *FileCredentialStore) Resolve(name string) (string, error) { return s.Get(name) }

// Get returns the secret stored under name, or an error wrapping ErrCredentialNotFound. A name
// outside the SecretName rules can never be stored, so it is refused outright.
func (s *FileCredentialStore) Get(name string) (string, error) {
	if !validSecretName(name) {
		return "", invalid("credential name %q is not a {secret:NAME} name", name)
	}
	secrets, err := s.load()
	if err != nil {
		return "", err
	}
	v, ok := secrets[name]
	if !ok || v == "" {
		return "", fmt.Errorf("credential store %s: %w: %s", s.path, ErrCredentialNotFound, name)
	}
	return v, nil
}

// Has reports whether name is stored. Any read failure counts as absent.
func (s *FileCredentialStore) Has(name string) bool {
	if !validSecretName(name) {
		return false
	}
	secrets, err := s.load()
	if err != nil {
		return false
	}
	v, ok := secrets[name]
	return ok && v != ""
}

// Names lists the stored names, sorted. It carries names only, never values.
func (s *FileCredentialStore) Names() ([]string, error) {
	secrets, err := s.load()
	if err != nil {
		return nil, err
	}
	return sortedKeys(secrets), nil
}

// Set stores secret under name, replacing any previous value. The name must fit the {secret:NAME}
// rules and the value must be non-empty and at most maxSecretBytes; both failures are errors that
// name the name (never the value) before anything is touched.
func (s *FileCredentialStore) Set(name, secret string) error {
	if !validSecretName(name) {
		return invalid("credential name %q is not a {secret:NAME} name", name)
	}
	if secret == "" {
		return invalid("credential %s: a secret value is empty", name)
	}
	if len(secret) > maxSecretBytes {
		return invalid("credential %s: value is larger than %d bytes", name, maxSecretBytes)
	}
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	secrets, err := s.load()
	if err != nil {
		return err
	}
	secrets[name] = secret
	return s.store(secrets)
}

// Delete removes name. Removing a name that is not stored is not an error.
func (s *FileCredentialStore) Delete(name string) error {
	if !validSecretName(name) {
		return invalid("credential name %q is not a {secret:NAME} name", name)
	}
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	secrets, err := s.load()
	if err != nil {
		return err
	}
	delete(secrets, name)
	return s.store(secrets)
}

// validSecretName applies the {secret:NAME} rules to a bare name.
func validSecretName(name string) bool {
	_, ok := SecretName("{secret:" + name + "}")
	return ok
}

// load reads the store file. A missing file is an empty store; anything else is verified before it
// is parsed: a mode other than 0600 fails closed, naming only the path and the mode. A corrupt
// file is an error that carries no bytes of it.
func (s *FileCredentialStore) load() (map[string]string, error) {
	path := storePath(s.path)

	raw, err := readBounded(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("credential store: reading %s: %w", s.path, err)
	}
	if err := checkStoreFileMode(path); err != nil {
		return nil, err
	}
	var secrets map[string]string
	if err := json.Unmarshal(raw, &secrets); err != nil {
		return nil, fmt.Errorf("credential store %s is not a valid credentials file", s.path)
	}
	if secrets == nil {
		return nil, fmt.Errorf("credential store %s is not a valid credentials file", s.path)
	}
	return secrets, nil
}

// store writes the whole store atomically: a temporary file in the same directory, chmod 0600,
// fsync, rename, directory fsync. A failure removes the temporary file and leaves the previous
// file untouched.
func (s *FileCredentialStore) store(secrets map[string]string) error {
	path := storePath(s.path)
	raw, err := json.Marshal(secrets)
	if err != nil {
		return fmt.Errorf("credential store %s: %w", s.path, err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("credential store: %w", err)
	}
	if err := tightenStoreDir(dir); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+CredentialStoreFileName+".tmp-*")
	if err != nil {
		return fmt.Errorf("credential store: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if err := tmp.Chmod(filePerm); err != nil {
		return fmt.Errorf("credential store: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		return fmt.Errorf("credential store: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("credential store: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("credential store: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("credential store: %w", err)
	}
	committed = true
	_ = syncDir(dir)
	return nil
}

// checkStoreFileMode fails closed on any mode but 0600: a loosened file may already have been read
// by someone else, and a tightened one is not the file this package writes. The error names the
// path and the modes, never content. Windows has no file modes to check.
func checkStoreFileMode(path string) error {
	if !storePermChecks {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("credential store: reading %s: %w", path, err)
	}
	if perm := info.Mode().Perm(); perm != filePerm {
		return fmt.Errorf("credential store %s has mode %04o, want %04o: refusing to read it; run chmod %04o %s", path, perm, filePerm, filePerm, path)
	}
	return nil
}

// tightenStoreDir strips group and other bits from the store directory (a repair of a loosened
// 0755). It never widens the owner bits: a directory the user made non-writable stays that way,
// and the write then fails on it.
func tightenStoreDir(dir string) error {
	if !storePermChecks {
		return nil
	}
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("credential store: %w", err)
	}
	perm := info.Mode().Perm()
	if perm&0077 == 0 {
		return nil
	}
	if err := os.Chmod(dir, perm&0700); err != nil {
		return fmt.Errorf("credential store: %w", err)
	}
	return nil
}

// storePermChecks is false on Windows, where the mode bits are not the access control.
var storePermChecks = runtime.GOOS != "windows"

// credStoreLockWait bounds the wait for another writer of the same store file.
const credStoreLockWait = 5 * time.Second

// lock takes the per-file store lock, like Save's: an advisory run lock in the user's cache
// directory, named by the hash of the resolved path, waiting at most credStoreLockWait. The run
// lock belongs to the open file description, so two goroutines of one process conflict like two
// processes, and the kernel drops it if the holder dies.
func (s *FileCredentialStore) lock() (func(), error) {
	path := storePath(s.path)

	cache, err := os.UserCacheDir()
	if err != nil {
		return nil, fmt.Errorf("credential store: lock: %w", err)
	}
	sum := sha256.Sum256([]byte("credstore:" + path))
	dir := filepath.Join(cache, appDirName, "credstore-locks")
	id := hex.EncodeToString(sum[:16])
	deadline := time.Now().Add(credStoreLockWait)
	for {
		l, err := runlock.TryAcquire(dir, id)
		if err == nil {
			return func() { _ = l.Release() }, nil
		}
		if !errors.Is(err, runlock.ErrHeld) {
			return nil, fmt.Errorf("credential store: lock: %w", err)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("credential store: %s is being written by another process", s.path)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// storePath resolves the store location like Save does: absolute, symlinks resolved when the file
// exists, and otherwise the directory resolved (macOS temporary directories are symlinked), so the
// lock id and the rename target are stable whether or not the file exists yet.
func storePath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	dir, base := filepath.Split(abs)
	if rd, err := filepath.EvalSymlinks(filepath.Clean(dir)); err == nil {
		return filepath.Join(rd, base)
	}
	return abs
}
