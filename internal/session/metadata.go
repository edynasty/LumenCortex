package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// Core-owned top-level keys of sessions.metadata_json. Every other key belongs
// to the host. Hosts cannot write these keys; core writes them only through
// SetCoreMetadataKey and RemoveCoreMetadataKey.
const (
	// MetadataKeyRuntime is the worktree binding written by AttachWorktree and RemoveSessionWorktree.
	MetadataKeyRuntime = "runtime"
	// MetadataKeySubagent is written once when a child session is created.
	MetadataKeySubagent = "subagent"
)

var (
	// ErrInvalidMetadataKey reports a key outside ^[A-Za-z0-9_]{1,64}$.
	ErrInvalidMetadataKey = errors.New("invalid metadata key")
	// ErrCoreMetadataKey reports a host write to a core-owned key.
	ErrCoreMetadataKey = errors.New("metadata key is owned by core")
	// ErrInvalidMetadataValue reports a value that is not valid JSON.
	ErrInvalidMetadataValue = errors.New("metadata value is not valid JSON")
)

var metadataKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)

// IsCoreMetadataKey reports whether key is one of the core-owned metadata keys.
func IsCoreMetadataKey(key string) bool {
	return key == MetadataKeyRuntime || key == MetadataKeySubagent
}

// CheckHostMetadata rejects session metadata that sets a core-owned key. It
// accepts an empty value, JSON null and any JSON object.
func CheckHostMetadata(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return fmt.Errorf("session metadata must be a JSON object: %w", err)
	}
	for key := range object {
		if IsCoreMetadataKey(key) {
			return fmt.Errorf("%w: %q", ErrCoreMetadataKey, key)
		}
	}
	return nil
}

// SetHostMetadataKey sets one top-level host-owned metadata key. value must be valid JSON.
func (s *Store) SetHostMetadataKey(ctx context.Context, sessionID, key string, value json.RawMessage) error {
	if err := checkMetadataKey(key); err != nil {
		return err
	}
	if IsCoreMetadataKey(key) {
		return fmt.Errorf("%w: %q", ErrCoreMetadataKey, key)
	}
	return s.setMetadataKey(ctx, sessionID, key, value)
}

// RemoveHostMetadataKey removes one top-level host-owned metadata key. A missing key is not an error.
func (s *Store) RemoveHostMetadataKey(ctx context.Context, sessionID, key string) error {
	if err := checkMetadataKey(key); err != nil {
		return err
	}
	if IsCoreMetadataKey(key) {
		return fmt.Errorf("%w: %q", ErrCoreMetadataKey, key)
	}
	return s.removeMetadataKey(ctx, sessionID, key)
}

// SetCoreMetadataKey sets one core-owned metadata key (runtime or subagent).
func (s *Store) SetCoreMetadataKey(ctx context.Context, sessionID, key string, value json.RawMessage) error {
	if !IsCoreMetadataKey(key) {
		return fmt.Errorf("%w: %q is not a core key", ErrInvalidMetadataKey, key)
	}
	return s.setMetadataKey(ctx, sessionID, key, value)
}

// RemoveCoreMetadataKey removes one core-owned metadata key (runtime or subagent).
func (s *Store) RemoveCoreMetadataKey(ctx context.Context, sessionID, key string) error {
	if !IsCoreMetadataKey(key) {
		return fmt.Errorf("%w: %q is not a core key", ErrInvalidMetadataKey, key)
	}
	return s.removeMetadataKey(ctx, sessionID, key)
}

func checkMetadataKey(key string) error {
	if !metadataKeyPattern.MatchString(key) {
		return fmt.Errorf("%w: %q", ErrInvalidMetadataKey, key)
	}
	return nil
}

// The key matches ^[A-Za-z0-9_]{1,64}$, so the quoted JSON path needs no escaping.
// Each patch is one UPDATE: SQLite serializes it against every other writer, and
// it never reads the object into Go and writes it back.
func (s *Store) setMetadataKey(ctx context.Context, sessionID, key string, value json.RawMessage) error {
	if !json.Valid(value) {
		return fmt.Errorf("%w: key %q", ErrInvalidMetadataValue, key)
	}
	return s.execMetadata(ctx, sessionID, `
UPDATE sessions
SET metadata_json = json_set(metadata_json, '$."' || ? || '"', json(?)), updated_at = ?
WHERE id = ?
`, key, string(value))
}

func (s *Store) removeMetadataKey(ctx context.Context, sessionID, key string) error {
	return s.execMetadata(ctx, sessionID, `
UPDATE sessions
SET metadata_json = json_remove(metadata_json, '$."' || ? || '"'), updated_at = ?
WHERE id = ?
`, key)
}

// execMetadata runs a metadata UPDATE whose arguments are args, then the new
// updated_at and the session id.
func (s *Store) execMetadata(ctx context.Context, sessionID, query string, args ...any) error {
	args = append(args, time.Now().UTC().Format(time.RFC3339Nano), sessionID)
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
