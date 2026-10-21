package taskstate

import (
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Notes are short, explicit records kept with Task State so they outlive the transcript rows the
// window drops (ADR-0010 D1). A run writes them with note_write (By agent); the user writes them
// through the inbox (By user). They are never a summary of the transcript and never evidence.
const (
	// MaxNotes bounds a thread's notes, both authors together.
	MaxNotes = 32
	// MaxNoteTextBytes bounds one note's text.
	MaxNoteTextBytes = 512
	// MaxNoteCites bounds the paths one note cites.
	MaxNoteCites = 4
	// notesRenderBytes is the notes' share of the Task State block.
	notesRenderBytes = 2048
)

var (
	// ErrTooManyNotes: the thread has MaxNotes notes; the run must remove one first.
	ErrTooManyNotes = errors.New("too many notes")
	// ErrNoteNotFound: no note has the id.
	ErrNoteNotFound = errors.New("note not found")
	// ErrUserNote: a run may not change or remove a note the user wrote.
	ErrUserNote = errors.New("the user wrote this note; only the user can change or remove it")
)

// Note is one Task State note.
type Note struct {
	ID    string    `json:"id"`
	Text  string    `json:"text"`
	By    string    `json:"by"`
	Cites []Cite    `json:"cites,omitempty"`
	At    time.Time `json:"at"`
}

// Cite is a workspace path a note refers to and the git blob it had when the note was written. Blob is
// empty when the path was not tracked content (an untracked file is hashed as git would store it, so
// this happens only for paths Core could not hash).
type Cite struct {
	Path string `json:"path"`
	Blob string `json:"blob,omitempty"`
}

// NormalizeNoteText validates note text and returns it trimmed.
func NormalizeNoteText(text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" || len(text) > MaxNoteTextBytes || !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
		return "", fmt.Errorf("note text must be 1-%d bytes of text", MaxNoteTextBytes)
	}
	return text, nil
}

// NormalizeCitePaths validates cited paths and returns them as clean, distinct, workspace-relative
// slash paths.
func NormalizeCitePaths(paths []string) ([]string, error) {
	if len(paths) > MaxNoteCites {
		return nil, fmt.Errorf("a note cites at most %d paths", MaxNoteCites)
	}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		clean, err := NormalizeTarget(RefPath, p)
		if err != nil {
			return nil, err
		}
		if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
			return nil, fmt.Errorf("cited path %q must be relative to the workspace", p)
		}
		dup := false
		for _, have := range out {
			dup = dup || have == clean
		}
		if !dup {
			out = append(out, clean)
		}
	}
	return out, nil
}

func (s State) noteIndex(id string) int {
	for i, n := range s.Notes {
		if n.ID == id {
			return i
		}
	}
	return -1
}

// nextNoteID allocates the next id from the thread's counter, so an id is never reused, even after
// its note was removed.
func (s *State) nextNoteID() string {
	s.NoteSeq++
	return "n" + strconv.FormatInt(s.NoteSeq, 10)
}

// WriteNote adds a run's note, or replaces the run's note with that id. text and cites must already be
// normalized and the cites hashed. A full thread refuses a new note and names the oldest agent note.
func (s *State) WriteNote(id, text string, cites []Cite, at time.Time) (Note, error) {
	if id != "" {
		i := s.noteIndex(id)
		if i < 0 {
			return Note{}, ErrNoteNotFound
		}
		if s.Notes[i].By == ByUser {
			return Note{}, ErrUserNote
		}
		n := Note{ID: id, Text: text, By: ByAgent, Cites: cites, At: at}
		s.Notes[i] = n
		return n, nil
	}
	if len(s.Notes) >= MaxNotes {
		if oldest := s.oldestNote(ByAgent); oldest != "" {
			return Note{}, fmt.Errorf("%w (%d); remove one first, e.g. the oldest agent note %s", ErrTooManyNotes, MaxNotes, oldest)
		}
		return Note{}, fmt.Errorf("%w (%d), all written by the user", ErrTooManyNotes, MaxNotes)
	}
	n := Note{ID: s.nextNoteID(), Text: text, By: ByAgent, Cites: cites, At: at}
	s.Notes = append(s.Notes, n)
	return n, nil
}

// RemoveNote removes a run's note.
func (s *State) RemoveNote(id string) error {
	i := s.noteIndex(id)
	if i < 0 {
		return ErrNoteNotFound
	}
	if s.Notes[i].By == ByUser {
		return ErrUserNote
	}
	s.Notes = append(s.Notes[:i:i], s.Notes[i+1:]...)
	return nil
}

func (s State) oldestNote(by string) string {
	for _, n := range s.Notes {
		if n.By == by {
			return n.ID
		}
	}
	return ""
}

// UserNotes counts the notes the user wrote.
func (s State) UserNotes() int {
	count := 0
	for _, n := range s.Notes {
		if n.By == ByUser {
			count++
		}
	}
	return count
}

// NoteEdit is the user's note edit: add, replace the text of any note (it becomes the user's), or remove
// any note. An add is queued with Create and the id the note will have, so the id a host sees in the
// preview is the id the note keeps; it never names another note, whatever a run writes in between.
// An add without an id (ID empty, no Create) gets one from the thread's counter when applied.
type NoteEdit struct {
	ID     string `json:"id,omitempty"`
	Text   string `json:"text,omitempty"`
	Remove bool   `json:"remove,omitempty"`
	Create bool   `json:"create,omitempty"`
}

// UserNoteIDPrefix starts the ids of notes the user adds; a run's notes are "n<seq>", so the two never
// collide.
const UserNoteIDPrefix = "u"

func (e NoteEdit) validate() error {
	if e.Remove {
		if e.ID == "" || e.Text != "" || e.Create {
			return errors.New("a note removal names the note and no text")
		}
		return nil
	}
	if e.Create && !validUserNoteID(e.ID) {
		return fmt.Errorf("a created note needs an id %s<1-32 letters or digits>", UserNoteIDPrefix)
	}
	_, err := NormalizeNoteText(e.Text)
	return err
}

func validUserNoteID(id string) bool {
	rest, ok := strings.CutPrefix(id, UserNoteIDPrefix)
	if !ok || rest == "" || len(rest) > 32 {
		return false
	}
	for _, r := range rest {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z') {
			return false
		}
	}
	return true
}

// setNote applies a user note edit. Adding to a full thread drops the oldest agent note; when every
// note is the user's, the edit is skipped (the host checks the count before queueing).
func (s *State) setNote(e NoteEdit, at time.Time) {
	i := -1
	if e.ID != "" {
		i = s.noteIndex(e.ID)
	}
	if e.Remove {
		if i >= 0 {
			s.Notes = append(s.Notes[:i:i], s.Notes[i+1:]...)
		}
		return
	}
	text, _ := NormalizeNoteText(e.Text)
	if e.Create {
		if i >= 0 {
			return // applied already
		}
	} else if i >= 0 {
		// The user's text replaces the note; its cites were the agent's evidence for other text.
		s.Notes[i] = Note{ID: e.ID, Text: text, By: ByUser, At: at}
		return
	} else if e.ID != "" {
		return // the note was removed in between
	}
	if len(s.Notes) >= MaxNotes {
		oldest := s.oldestNote(ByAgent)
		if oldest == "" {
			return
		}
		j := s.noteIndex(oldest)
		s.Notes = append(s.Notes[:j:j], s.Notes[j+1:]...)
	}
	id := e.ID
	if id == "" {
		id = s.nextNoteID()
	}
	s.Notes = append(s.Notes, Note{ID: id, Text: text, By: ByUser, At: at})
}
