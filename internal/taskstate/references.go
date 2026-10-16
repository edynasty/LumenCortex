package taskstate

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
	"unicode/utf8"
)

// Reference kinds, states and authors (ADR-0006 D1′).
const (
	RefPath   = "path"
	RefThread = "thread"

	RefIncluded  = "included"
	RefExcluded  = "excluded"
	RefSuggested = "suggested"
	// RefRemoved is an edit state only: the user deletes the entry, exclusion included.
	RefRemoved = "removed"

	ByAgent = "agent"
)

// MaxReferences bounds the list the agent can grow; the user's own edits are always applied.
const MaxReferences = 64

const (
	maxTargetBytes = 1024
	maxNoteBytes   = 512
)

var (
	// ErrReferenceExcluded: the user excluded the target; the agent may not suggest it again.
	ErrReferenceExcluded = errors.New("the user excluded this reference")
	// ErrTooManyReferences: the agent may not grow the list beyond MaxReferences.
	ErrTooManyReferences = errors.New("too many references")
)

// Reference is a context attachment of the thread. Exclusion is context curation, not access control:
// tools may still read an excluded file when the task needs it.
type Reference struct {
	ID     string    `json:"id"`
	Kind   string    `json:"kind"`
	Target string    `json:"target"`
	Note   string    `json:"note,omitempty"`
	State  string    `json:"state"`
	By     string    `json:"by"`
	At     time.Time `json:"at"`
}

// ReferenceID is the stable id of a (kind, target) pair.
func ReferenceID(kind, target string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + target))
	return "ref_" + hex.EncodeToString(sum[:6])
}

// NormalizeTarget validates a reference target and returns its canonical form: a cleaned slash path
// relative to the workspace for paths, the session id for threads.
func NormalizeTarget(kind, target string) (string, error) {
	target = strings.TrimSpace(target)
	if target == "" || len(target) > maxTargetBytes || !utf8.ValidString(target) || strings.ContainsRune(target, 0) {
		return "", fmt.Errorf("reference target must be 1-%d bytes of text", maxTargetBytes)
	}
	switch kind {
	case RefPath:
		clean := path.Clean(strings.ReplaceAll(target, "\\", "/"))
		if clean == "." {
			return "", errors.New("reference path must name a file or directory")
		}
		return clean, nil
	case RefThread:
		return target, nil
	default:
		return "", fmt.Errorf("reference kind must be %q or %q", RefPath, RefThread)
	}
}

func normalizeNote(note string) (string, error) {
	note = strings.TrimSpace(note)
	if len(note) > maxNoteBytes || !utf8.ValidString(note) {
		return "", fmt.Errorf("reference note must be at most %d bytes of text", maxNoteBytes)
	}
	return note, nil
}

func (s State) referenceIndex(id string) int {
	for i, r := range s.References {
		if r.ID == id {
			return i
		}
	}
	return -1
}

// Suggest adds an agent suggestion. It never changes an entry that exists, whoever set it, and refuses
// a target the user excluded. It returns the entry as stored and whether s changed.
func (s *State) Suggest(kind, target, note string, at time.Time) (Reference, bool, error) {
	target, err := NormalizeTarget(kind, target)
	if err != nil {
		return Reference{}, false, err
	}
	if note, err = normalizeNote(note); err != nil {
		return Reference{}, false, err
	}
	id := ReferenceID(kind, target)
	if i := s.referenceIndex(id); i >= 0 {
		existing := s.References[i]
		if existing.State == RefExcluded {
			return existing, false, ErrReferenceExcluded
		}
		return existing, false, nil
	}
	if len(s.References) >= MaxReferences {
		return Reference{}, false, ErrTooManyReferences
	}
	ref := Reference{ID: id, Kind: kind, Target: target, Note: note, State: RefSuggested, By: ByAgent, At: at}
	s.References = append(s.References, ref)
	return ref, true, nil
}

// setReference applies a user edit: include or exclude (creating the entry when needed, and taking it
// over from the agent), or remove it.
func (s *State) setReference(e ReferenceEdit, at time.Time) {
	target, err := NormalizeTarget(e.Kind, e.Target)
	if err != nil {
		return
	}
	id := ReferenceID(e.Kind, target)
	i := s.referenceIndex(id)
	if e.State == RefRemoved {
		if i >= 0 {
			s.References = append(s.References[:i:i], s.References[i+1:]...)
		}
		return
	}
	ref := Reference{ID: id, Kind: e.Kind, Target: target, State: e.State, By: ByUser, At: at}
	if i >= 0 {
		ref.Note = s.References[i].Note
	}
	if e.Note != nil {
		ref.Note, _ = normalizeNote(*e.Note)
	}
	if i >= 0 {
		s.References[i] = ref
		return
	}
	s.References = append(s.References, ref)
}
