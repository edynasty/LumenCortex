package editing

import (
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Snapshot is one file at one revision as a session read it: the decoded document and the set of line
// numbers that were actually shown. Only a line the session was shown may be anchored, so a model
// cannot guess a line number.
type Snapshot struct {
	// Path is the canonical path of the file.
	Path string
	// Rev is the revision the document was decoded from.
	Rev string
	// Doc is the content, lines and terminators.
	Doc Doc
	// Seen holds the 1-based line numbers the session was shown for this revision.
	Seen map[int]bool
	// at is the registration order, used by the LRU.
	at uint64
}

// SeenLine reports whether the session was shown line n of this snapshot.
func (s *Snapshot) SeenLine(n int) bool { return s.Seen != nil && s.Seen[n] }

const (
	// snapshotFilesPerSession and snapshotBytesPerSession bound one session's snapshot store, in
	// memory only: the process restarting loses them, and an edit then has to be based on a fresh read.
	snapshotFilesPerSession = 64
	snapshotBytesPerSession = 16 << 20
	// snapshotRevsPerPath is how many revisions of one path are kept, so an edit computed against the
	// previous read can still be relocated after an unrelated external change.
	snapshotRevsPerPath = 8
)

// SnapshotStore keeps, per session, the documents it has read, bounded by an LRU over (path, rev)
// entries and by bytes. It is safe for concurrent use.
type SnapshotStore struct {
	mu       sync.Mutex
	sessions map[string]*sessionSnapshots
	seq      uint64
}

type sessionSnapshots struct {
	entries []*Snapshot
	bytes   int
}

// NewSnapshotStore returns an empty store.
func NewSnapshotStore() *SnapshotStore {
	return &SnapshotStore{sessions: map[string]*sessionSnapshots{}}
}

// Register records a read of path: its decoded document, and the line range the session was shown.
// from and to are 1-based and inclusive; a from of 0 records no shown line. It returns the snapshot.
func (s *SnapshotStore) Register(session, path string, doc Doc, from, to int) *Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	ss := s.sessions[session]
	if ss == nil {
		ss = &sessionSnapshots{}
		s.sessions[session] = ss
	}
	snap := s.find(ss, path, doc.Rev)
	if snap == nil {
		snap = &Snapshot{Path: path, Rev: doc.Rev, Seen: map[int]bool{}}
		ss.entries = append(ss.entries, snap)
		ss.bytes += len(doc.Raw)
	} else {
		ss.bytes += len(doc.Raw) - len(snap.Doc.Raw)
	}
	snap.Doc = doc
	snap.at = s.seq
	if from < 1 {
		from = 1
	}
	if to <= 0 || to > len(doc.Lines) {
		to = len(doc.Lines)
	}
	for line := from; line <= to; line++ {
		snap.Seen[line] = true
	}
	s.prune(ss, path)
	return snap
}

// RegisterRev records that a session read path at rev without being shown anchored lines. It is the
// light registration a plain read makes: enough for a whole-file write to notice that the file moved
// on, without keeping the text.
func (s *SnapshotStore) RegisterRev(session, path, rev string) {
	if rev == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	ss := s.sessions[session]
	if ss == nil {
		ss = &sessionSnapshots{}
		s.sessions[session] = ss
	}
	snap := s.find(ss, path, rev)
	if snap == nil {
		snap = &Snapshot{Path: path, Rev: rev, Seen: map[int]bool{}}
		ss.entries = append(ss.entries, snap)
	}
	snap.at = s.seq
	s.prune(ss, path)
}

// Lookup returns the snapshot of path at rev, if this session read it.
func (s *SnapshotStore) Lookup(session, path, rev string) (*Snapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss := s.sessions[session]
	if ss == nil {
		return nil, false
	}
	snap := s.find(ss, path, rev)
	if snap == nil {
		return nil, false
	}
	return snap, true
}

// Latest returns the most recently registered snapshot of path, whatever its revision: a session's
// last view of the file. It is how a whole-file write learns the version it would overwrite.
func (s *SnapshotStore) Latest(session, path string) (*Snapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss := s.sessions[session]
	if ss == nil {
		return nil, false
	}
	var best *Snapshot
	for _, entry := range ss.entries {
		if entry.Path != path {
			continue
		}
		if best == nil || entry.at > best.at {
			best = entry
		}
	}
	if best == nil {
		return nil, false
	}
	return best, true
}

// Forget drops every snapshot of a session. The runtime calls it when a session ends, so a later
// session that reuses the id never inherits stale views.
func (s *SnapshotStore) Forget(session string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, session)
}

// ForgetPath drops every snapshot of one path, e.g. after the file was deleted.
func (s *SnapshotStore) ForgetPath(session, path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss := s.sessions[session]
	if ss == nil {
		return
	}
	kept := ss.entries[:0]
	for _, entry := range ss.entries {
		if entry.Path == path {
			ss.bytes -= len(entry.Doc.Raw)
			continue
		}
		kept = append(kept, entry)
	}
	ss.entries = kept
	if ss.bytes < 0 {
		ss.bytes = 0
	}
}

// Reset drops every session's snapshots. Tests use it between independent cases.
func (s *SnapshotStore) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions = map[string]*sessionSnapshots{}
}

func (s *SnapshotStore) find(ss *sessionSnapshots, path, rev string) *Snapshot {
	for _, entry := range ss.entries {
		if entry.Path == path && entry.Rev == rev {
			return entry
		}
	}
	return nil
}

// prune enforces the byte bound, the per-path revision bound and the per-session file bound. The
// entry just registered (keepPath) is never evicted.
func (s *SnapshotStore) prune(ss *sessionSnapshots, keepPath string) {
	byPath := map[string][]*Snapshot{}
	for _, entry := range ss.entries {
		byPath[entry.Path] = append(byPath[entry.Path], entry)
	}
	drop := map[*Snapshot]bool{}
	for _, entries := range byPath {
		if len(entries) <= snapshotRevsPerPath {
			continue
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].at < entries[j].at })
		for _, entry := range entries[:len(entries)-snapshotRevsPerPath] {
			drop[entry] = true
		}
	}
	// Byte bound: evict the least recently used entries, except the one being registered.
	for ss.bytes > snapshotBytesPerSession {
		var oldest *Snapshot
		for _, entry := range ss.entries {
			if drop[entry] || entry.Path == keepPath {
				continue
			}
			if oldest == nil || entry.at < oldest.at {
				oldest = entry
			}
		}
		if oldest == nil {
			break
		}
		drop[oldest] = true
		ss.bytes -= len(oldest.Doc.Raw)
	}
	// Path bound: drop the least recently used paths beyond the limit.
	paths := make([]string, 0, len(byPath))
	for path := range byPath {
		paths = append(paths, path)
	}
	if len(paths) > snapshotFilesPerSession {
		type pathUse struct {
			path string
			at   uint64
		}
		uses := make([]pathUse, 0, len(paths))
		for _, path := range paths {
			var at uint64
			for _, entry := range byPath[path] {
				if entry.at > at {
					at = entry.at
				}
			}
			uses = append(uses, pathUse{path: path, at: at})
		}
		sort.Slice(uses, func(i, j int) bool { return uses[i].at < uses[j].at })
		for _, use := range uses[:len(uses)-snapshotFilesPerSession] {
			if use.path == keepPath {
				continue
			}
			for _, entry := range byPath[use.path] {
				drop[entry] = true
			}
		}
	}
	if len(drop) == 0 {
		return
	}
	kept := ss.entries[:0]
	for _, entry := range ss.entries {
		if drop[entry] {
			ss.bytes -= len(entry.Doc.Raw)
			continue
		}
		kept = append(kept, entry)
	}
	ss.entries = kept
	if ss.bytes < 0 {
		ss.bytes = 0
	}
}

// Lease is a write lease: a path or directory prefix one holder may write for a bounded time. It is
// the way two agents that share one work tree avoid overwriting each other. The default execution
// model gives each writing agent its own work tree, so a lease is only needed when a work tree is
// really shared.
type Lease struct {
	// Root is the workspace the target was resolved in.
	Root string `json:"root"`
	// Target is the canonical absolute path or directory the lease covers.
	Target string `json:"target"`
	// Holder is the actor holding it, normally a session id.
	Holder string `json:"holder"`
	// Prefix is true when Target is a directory whose whole subtree the lease covers.
	Prefix bool `json:"prefix"`
	// AcquiredAt and Expires bound the lease.
	AcquiredAt time.Time `json:"acquiredAt"`
	Expires    time.Time `json:"expires"`
}

// Covers reports whether the lease covers path.
func (l Lease) Covers(path string) bool {
	if l.Target == path {
		return true
	}
	return l.Prefix && strings.HasPrefix(path, strings.TrimSuffix(l.Target, string(filepath.Separator))+string(filepath.Separator))
}

// DefaultLeaseTTL is how long a lease lives without a renewal.
const DefaultLeaseTTL = 10 * time.Minute

// maxLeaseTTL bounds a renewal so a holder that dies cannot keep a path locked forever.
const maxLeaseTTL = 24 * time.Hour

// LeaseConflict reports a write refused because another holder holds the path's lease.
type LeaseConflict struct {
	Path   string
	Holder string
	Until  time.Time
}

func (c *LeaseConflict) Error() string {
	return "lease_conflict: " + c.Holder + " holds a write lease on " + c.Path + " until " + c.Until.UTC().Format(time.RFC3339)
}

// LeaseStore keeps the active write leases. It is safe for concurrent use; expired leases are
// dropped when they are next examined.
type LeaseStore struct {
	mu     sync.Mutex
	leases []Lease
	now    func() time.Time
}

// NewLeaseStore returns an empty store.
func NewLeaseStore() *LeaseStore { return &LeaseStore{now: time.Now} }

// Acquire takes or renews the lease on target for holder. A lease held by another holder on an
// overlapping path is a conflict. The returned lease is a copy with its expiry resolved.
func (s *LeaseStore) Acquire(root, target, holder string, prefix bool, ttl time.Duration) (Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.pruneLocked(now)
	if ttl <= 0 {
		ttl = DefaultLeaseTTL
	}
	if ttl > maxLeaseTTL {
		ttl = maxLeaseTTL
	}
	for i := range s.leases {
		held := &s.leases[i]
		if held.Target == target && held.Prefix == prefix {
			if held.Holder != holder {
				return Lease{}, &LeaseConflict{Path: target, Holder: held.Holder, Until: held.Expires}
			}
			held.Expires = now.Add(ttl)
			return *held, nil
		}
	}
	candidate := Lease{Root: root, Target: target, Holder: holder, Prefix: prefix, AcquiredAt: now, Expires: now.Add(ttl)}
	for _, held := range s.leases {
		if held.Holder == holder {
			continue
		}
		if candidate.Covers(held.Target) || held.Covers(candidate.Target) {
			return Lease{}, &LeaseConflict{Path: candidate.Target, Holder: held.Holder, Until: held.Expires}
		}
	}
	s.leases = append(s.leases, candidate)
	return candidate, nil
}

// Renew extends a lease the holder already has.
func (s *LeaseStore) Renew(target, holder string, ttl time.Duration) (Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.pruneLocked(now)
	if ttl <= 0 || ttl > maxLeaseTTL {
		ttl = DefaultLeaseTTL
	}
	for i := range s.leases {
		held := &s.leases[i]
		if held.Target != target {
			continue
		}
		if held.Holder != holder {
			return Lease{}, &LeaseConflict{Path: target, Holder: held.Holder, Until: held.Expires}
		}
		held.Expires = now.Add(ttl)
		return *held, nil
	}
	return Lease{}, &LeaseConflict{Path: target, Holder: "", Until: time.Time{}}
}

// Release drops a lease the holder has. Releasing a lease that is not held is not an error: a run
// that ends twice must not fail its second cleanup.
func (s *LeaseStore) Release(target, holder string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.leases[:0]
	for _, held := range s.leases {
		if held.Target == target && held.Holder == holder {
			continue
		}
		kept = append(kept, held)
	}
	s.leases = kept
}

// Check refuses a write by holder to path when another holder's lease covers it.
func (s *LeaseStore) Check(holder, path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.pruneLocked(now)
	for _, held := range s.leases {
		if held.Holder == holder {
			continue
		}
		if held.Covers(path) {
			return &LeaseConflict{Path: path, Holder: held.Holder, Until: held.Expires}
		}
	}
	return nil
}

// List returns the active leases, sorted by target then holder.
func (s *LeaseStore) List() []Lease {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(s.now())
	out := append([]Lease(nil), s.leases...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Target != out[j].Target {
			return out[i].Target < out[j].Target
		}
		return out[i].Holder < out[j].Holder
	})
	return out
}

func (s *LeaseStore) pruneLocked(now time.Time) {
	kept := s.leases[:0]
	for _, held := range s.leases {
		if held.Expires.After(now) {
			kept = append(kept, held)
		}
	}
	s.leases = kept
}
