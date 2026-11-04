package threadctx

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/edynasty/LumenCortex/internal/taskstate"
)

// The locator catalog (plan T2-S2; DATA_AND_FAILURES §3, EVALUATION §6.1): a structured index of earlier
// threads that carries no free text. An entry is a thread's id, two enums, the workspace paths its notes
// cite whose content is unchanged (path and blob), and the configured checks it ran. Titles, goals,
// snippets, note text, reference notes, check logs and tool specs never enter it; a reader pulls any
// content with read_thread. Building it decides nothing about pushing it: when a request carries it is
// the push policy's decision (O-5, T5a-S2).
const (
	// MaxLocatorPaths and MaxLocatorChecks bound one entry; what does not fit sets Truncated.
	MaxLocatorPaths  = 4
	MaxLocatorChecks = 4
	// MaxLocatorPathBytes bounds one path; a longer one is left out (a cut path locates nothing).
	MaxLocatorPathBytes = 256
	// MaxCatalogEntries bounds the entries of one catalog, MaxCatalogBytes its rendered text.
	MaxCatalogEntries = MaxRecallHits
	MaxCatalogBytes   = 2048
	// maxCheckIDBytes bounds a check id the catalog carries.
	maxCheckIDBytes = 64
)

// Locator kinds: what started the thread (session_relations.kind), fixed.
const (
	LocatorThread    = "thread"
	LocatorSubagent  = "subagent"
	LocatorExecution = "execution"
	LocatorOther     = "other"
)

// CheckStatusUnknown is the status of every check entry until a check's result in the reader's scope
// can be established (fresh-at-use, plan T4-S3): a stored verdict of an earlier run is not current.
const CheckStatusUnknown = "unknown"

// LocatorEntry is one catalog entry.
type LocatorEntry struct {
	// Ref is the thread's session id: read_thread opens it.
	Ref  string `json:"ref"`
	Kind string `json:"kind"`
	// Resolution is open, done, dropped, or empty.
	Resolution string         `json:"resolution,omitempty"`
	CitedPaths []LocatorPath  `json:"citedPaths,omitempty"`
	Checks     []LocatorCheck `json:"checks,omitempty"`
	// Truncated is true when a path or check of the thread was left out by a bound.
	Truncated bool `json:"truncated,omitempty"`
}

// LocatorPath is a workspace-relative path and the blob it has now, which is the blob the citing note
// recorded.
type LocatorPath struct {
	Path string `json:"path"`
	Blob string `json:"blob"`
}

// LocatorCheck is a configured check the thread ran.
type LocatorCheck struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// CatalogThread is what the catalog reads about one thread. Only the fields named here are read.
type CatalogThread struct {
	ID         string
	Kind       string
	Resolution string
	// Notes are the thread's notes, oldest first, and NoteFacts the store's facts per note id.
	Notes     []taskstate.Note
	NoteFacts map[string]SourceFacts
	// CheckIDs are the ids of configured checks the thread ran, newest first.
	CheckIDs []string
}

// Catalog builds at most limit entries (MaxCatalogEntries when limit <= 0), in the order of threads.
// A note contributes its cited paths only when it has its own cites, none of its sources is withdrawn,
// and every cited path has the blob the note recorded in current (the reader's workspace): a note with
// one changed or gone source contributes nothing, and a thread left with no path and no check has no
// entry. Paths are deduplicated, newest note first.
func Catalog(threads []CatalogThread, current map[string]string, limit int) []LocatorEntry {
	if limit <= 0 || limit > MaxCatalogEntries {
		limit = MaxCatalogEntries
	}
	var out []LocatorEntry
	for _, t := range threads {
		if len(out) == limit {
			break
		}
		e := LocatorEntry{Ref: t.ID, Kind: locatorKind(t.Kind), Resolution: locatorResolution(t.Resolution)}
		seen := map[string]bool{}
		for i := len(t.Notes) - 1; i >= 0; i-- {
			n := t.Notes[i]
			if len(n.Cites) == 0 || t.NoteFacts[n.ID].Withdrawn || !allCurrent(n.Cites, current) {
				continue
			}
			for _, c := range n.Cites {
				if seen[c.Path] {
					continue
				}
				seen[c.Path] = true
				if !locatorText(c.Path, MaxLocatorPathBytes) || len(e.CitedPaths) == MaxLocatorPaths {
					e.Truncated = true
					continue
				}
				e.CitedPaths = append(e.CitedPaths, LocatorPath{Path: c.Path, Blob: c.Blob})
			}
		}
		checked := map[string]bool{}
		for _, id := range t.CheckIDs {
			if checked[id] || !locatorText(id, maxCheckIDBytes) || strings.ContainsAny(id, " \t") {
				continue
			}
			checked[id] = true
			if len(e.Checks) == MaxLocatorChecks {
				e.Truncated = true
				continue
			}
			e.Checks = append(e.Checks, LocatorCheck{ID: id, Status: CheckStatusUnknown})
		}
		if len(e.CitedPaths) > 0 || len(e.Checks) > 0 {
			out = append(out, e)
		}
	}
	return out
}

// CatalogHeader starts the rendered catalog.
const CatalogHeader = "[Locator catalog — earlier threads of this workspace, as ids, paths and check ids only; no content. A path's blob is its current content; check status is unknown until a check runs in your workspace. Open a thread with read_thread before relying on it.]"

// RenderCatalog is the catalog as one block of text, at most MaxCatalogBytes: the header and one line
// per entry in order; once a line does not fit, it and the entries after it are counted instead. Every
// field is an id, a fixed enum, a path or a blob, so nothing but the header is prose. No entries render
// as "".
func RenderCatalog(entries []LocatorEntry) string {
	if len(entries) == 0 {
		return ""
	}
	// countReserve is room for the count line, "\n(+N more)", kept free while lines are added.
	const countReserve = 16
	var b strings.Builder
	b.WriteString(CatalogHeader)
	for i, e := range entries {
		line := renderLocator(e)
		if b.Len()+1+len(line)+countReserve > MaxCatalogBytes {
			b.WriteString("\n(+" + strconv.Itoa(len(entries)-i) + " more)")
			break
		}
		b.WriteString("\n")
		b.WriteString(line)
	}
	return b.String()
}

func renderLocator(e LocatorEntry) string {
	var b strings.Builder
	b.WriteString("- ref=" + e.Ref + " kind=" + e.Kind)
	if e.Resolution != "" {
		b.WriteString(" resolution=" + e.Resolution)
	}
	if len(e.CitedPaths) > 0 {
		b.WriteString(" paths=")
		for i, p := range e.CitedPaths {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(p.Path + "@" + p.Blob)
		}
	}
	if len(e.Checks) > 0 {
		b.WriteString(" checks=")
		for i, c := range e.Checks {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(c.ID + ":" + c.Status)
		}
	}
	if e.Truncated {
		b.WriteString(" truncated")
	}
	return b.String()
}

func allCurrent(cites []taskstate.Cite, current map[string]string) bool {
	for _, c := range cites {
		if c.Blob == "" || current[c.Path] != c.Blob {
			return false
		}
	}
	return true
}

func locatorKind(kind string) string {
	switch kind {
	case "":
		return LocatorThread
	case LocatorSubagent, LocatorExecution:
		return kind
	}
	return LocatorOther
}

func locatorResolution(r string) string {
	switch r {
	case taskstate.ResolutionOpen, taskstate.ResolutionDone, taskstate.ResolutionDropped:
		return r
	}
	return ""
}

// locatorText reports a value the catalog may carry: valid UTF-8 of at most max bytes with no control
// characters, no separator the rendering uses, and no space at either end.
func locatorText(v string, max int) bool {
	return v != "" && len(v) <= max && utf8.ValidString(v) && strings.TrimSpace(v) == v &&
		strings.IndexFunc(v, unicode.IsControl) < 0 && !strings.ContainsAny(v, ",\n")
}
