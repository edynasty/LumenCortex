package editing

import (
	"os"
	"sync"
)

// History bounds: a host diff view needs the last few revisions of the files an agent is editing,
// not an archive. Older revisions are refused as unknown_rev rather than approximated.
const (
	historyRevsPerPath = 8
	historyMaxPaths    = 256
	historyMaxBytes    = 32 << 20
)

// revisionHistory keeps the content of the revisions commits produced and replaced, per canonical
// path, so a host can show what a committed edit changed. Only content that passed a commit's path
// guard enters it.
type revisionHistory struct {
	mu    sync.Mutex
	paths map[string][]historyEntry
	order []string // least recently written path first
	bytes int
}

type historyEntry struct {
	rev     string
	content []byte
	// absent records a revision in which the file did not exist (before a create, after a delete).
	absent bool
}

func newRevisionHistory() *revisionHistory {
	return &revisionHistory{paths: make(map[string][]historyEntry)}
}

func (h *revisionHistory) add(key string, entry historyEntry) {
	h.mu.Lock()
	defer h.mu.Unlock()
	entries := h.paths[key]
	for _, e := range entries {
		if e.rev == entry.rev && e.absent == entry.absent {
			h.touchLocked(key)
			return
		}
	}
	entry.content = append([]byte(nil), entry.content...)
	entries = append(entries, entry)
	h.bytes += len(entry.content)
	for len(entries) > historyRevsPerPath {
		h.bytes -= len(entries[0].content)
		entries = entries[1:]
	}
	h.paths[key] = entries
	h.touchLocked(key)
	for (len(h.order) > historyMaxPaths || h.bytes > historyMaxBytes) && len(h.order) > 1 {
		oldest := h.order[0]
		h.order = h.order[1:]
		for _, e := range h.paths[oldest] {
			h.bytes -= len(e.content)
		}
		delete(h.paths, oldest)
	}
}

func (h *revisionHistory) touchLocked(key string) {
	for i, k := range h.order {
		if k == key {
			h.order = append(h.order[:i], h.order[i+1:]...)
			break
		}
	}
	h.order = append(h.order, key)
}

// lookup returns the content of rev for key. The empty rev names the absent file; so does the revision
// of empty content, which is the oldRev an edit.committed event reports for a created file. An empty
// file and an absent one render the same diff, so either entry serves.
func (h *revisionHistory) lookup(key, rev string) ([]byte, bool, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	entries, tracked := h.paths[key]
	if !tracked {
		return nil, false, false
	}
	empty := rev == "" || rev == RevOf(nil)
	for _, e := range entries {
		if e.absent && empty {
			return nil, true, true
		}
		if !e.absent && e.rev == rev {
			return e.content, true, true
		}
	}
	return nil, false, true
}

// record keeps the replaced and the written content of every file a commit really wrote.
func (h *revisionHistory) record(p *prepared) {
	if p.existed && !p.created {
		h.add(p.key, historyEntry{rev: p.rev, content: p.original})
	} else {
		h.add(p.key, historyEntry{absent: true})
	}
	if p.deleted {
		h.add(p.key, historyEntry{absent: true})
		return
	}
	h.add(p.key, historyEntry{rev: RevOf(p.next), content: p.next})
}

// FileRevision returns the editing revision of path's current content and its workspace-relative
// name. A missing file has revision "" and no error; a path the host refuses is an error.
func (e *Engine) FileRevision(host Host, path string) (rev, relative string, err error) {
	if host == nil {
		host = e.host
	}
	abs, rel, err := host.Resolve(path, true)
	if err != nil {
		return "", "", err
	}
	raw, exists, err := readCurrent(abs)
	if err != nil {
		return "", rel, err
	}
	if !exists {
		return "", rel, nil
	}
	return RevOf(raw), rel, nil
}

// Diff returns the unified diff of path between two revisions a commit produced or replaced. An empty
// after names the current content on disk; an empty before names the file before it existed. Only a
// file a commit has written is diffable, and a revision the bounded history no longer holds is refused
// as unknown_rev, never approximated.
func (e *Engine) Diff(host Host, path, before, after string) (string, error) {
	if host == nil {
		host = e.host
	}
	abs, rel, err := host.Resolve(path, true)
	if err != nil {
		return "", err
	}
	key := canonicalPath(abs)
	beforeContent, ok, tracked := e.history.lookup(key, before)
	if !tracked {
		return "", &Error{Code: CodeUnknownRev, Path: rel, Rev: before, Detail: "no committed edit of this file is held"}
	}
	if !ok {
		return "", &Error{Code: CodeUnknownRev, Path: rel, Rev: before, Detail: "that revision is not in the bounded edit history"}
	}
	var afterContent []byte
	if after == "" {
		raw, exists, err := readCurrent(abs)
		if err != nil {
			return "", err
		}
		if exists {
			afterContent = raw
		}
	} else {
		afterContent, ok, _ = e.history.lookup(key, after)
		if !ok {
			return "", &Error{Code: CodeUnknownRev, Path: rel, Rev: after, Detail: "that revision is not in the bounded edit history"}
		}
	}
	return UnifiedDiff(rel, beforeContent, afterContent), nil
}

func readCurrent(abs string) ([]byte, bool, error) {
	f, err := os.Open(abs)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	raw, err := readLimited(f, MaxFileBytes)
	if err != nil {
		return nil, true, err
	}
	return raw, true, nil
}
