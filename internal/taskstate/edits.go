package taskstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Edit is one host edit queued in task_state_inbox (ADR-0006 D1′). Exactly one field is set. A run
// applies queued edits in inbox order at its next step boundary, before the request is built, or the
// next Start applies them; an edit is visible in the first request built after it committed.
type Edit struct {
	// Brief replaces the brief; "" restores the goal.
	Brief     *string        `json:"brief,omitempty"`
	Reference *ReferenceEdit `json:"reference,omitempty"`
	Resolve   *ResolveEdit   `json:"resolve,omitempty"`
	Note      *NoteEdit      `json:"note,omitempty"`
}

// ReferenceEdit includes, excludes or removes one reference as the user.
type ReferenceEdit struct {
	Kind   string `json:"kind"`
	Target string `json:"target"`
	// Note replaces the note when non-nil; nil keeps the stored one.
	Note  *string `json:"note,omitempty"`
	State string  `json:"state"`
}

// ResolveEdit is the user's manual resolution (ResolveThread).
type ResolveEdit struct {
	State string `json:"state"`
	Actor string `json:"actor"`
}

// QueuedEdit is an inbox row: the edit and when the host queued it.
type QueuedEdit struct {
	Seq  int64
	At   time.Time
	Edit Edit
}

// Validate checks an edit before it is queued, so applying it cannot fail.
func (e Edit) Validate() error {
	set := 0
	if e.Brief != nil {
		set++
		if len(*e.Brief) > MaxBriefBytes || !utf8.ValidString(*e.Brief) {
			return fmt.Errorf("brief must be at most %d bytes of text", MaxBriefBytes)
		}
	}
	if e.Reference != nil {
		set++
		r := e.Reference
		if r.State != RefIncluded && r.State != RefExcluded && r.State != RefRemoved {
			return fmt.Errorf("reference state must be %q, %q or %q", RefIncluded, RefExcluded, RefRemoved)
		}
		if _, err := NormalizeTarget(r.Kind, r.Target); err != nil {
			return err
		}
		if r.Note != nil {
			if _, err := normalizeNote(*r.Note); err != nil {
				return err
			}
		}
	}
	if e.Resolve != nil {
		set++
		if !validResolution(e.Resolve.State) {
			return fmt.Errorf("resolution must be %q, %q or %q", ResolutionOpen, ResolutionDone, ResolutionDropped)
		}
		if err := validActor(e.Resolve.Actor); err != nil {
			return err
		}
	}
	if e.Note != nil {
		set++
		if err := e.Note.validate(); err != nil {
			return err
		}
	}
	if set != 1 {
		return errors.New("an edit sets exactly one of brief, reference, resolve or note")
	}
	return nil
}

// DecodeEdit reads a stored edit.
func DecodeEdit(raw []byte) (Edit, error) {
	var e Edit
	if err := json.Unmarshal(raw, &e); err != nil {
		return Edit{}, err
	}
	return e, nil
}

// Apply applies queued edits in order. An edit that no longer validates (written by a foreign writer)
// is skipped; it is consumed with the others.
func (s *State) Apply(edits []QueuedEdit) {
	for _, q := range edits {
		if q.Edit.Validate() != nil {
			continue
		}
		at := q.At.UTC()
		switch e := q.Edit; {
		case e.Brief != nil:
			s.Brief = strings.TrimSpace(*e.Brief)
		case e.Reference != nil:
			s.setReference(*e.Reference, at)
		case e.Resolve != nil:
			s.resolve(e.Resolve.State, strings.TrimSpace(e.Resolve.Actor), at)
		case e.Note != nil:
			s.setNote(*e.Note, at)
		}
	}
}
