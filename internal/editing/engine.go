package editing

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Host is the file layer a commit runs through. The file tools implement it with the workspace path
// guard (workspace confinement, symlink escape) and the descriptor-level protected-state check; a
// standalone engine can use LocalHost.
type Host interface {
	// Resolve turns a tool's path into the absolute path and the workspace-relative one, refusing a
	// path that escapes the workspace or reaches protected state.
	Resolve(input string, allowMissing bool) (path, relative string, err error)
	// Open opens a file for reading and writing without truncating, creating it when create is true.
	// It must refuse protected state on the descriptor, so a path swapped after Resolve still ends
	// here, and leave a refused file as it was.
	Open(path string, create bool) (*os.File, error)
	// Event publishes one runtime event. It may be a no-op.
	Event(kind, sessionID string, payload map[string]any)
}

// Actor is the identity a read or a commit belongs to. A tool write uses the run's session.
type Actor struct {
	// Session is the session whose snapshots are read and written, and whose id is the lease holder.
	Session string
	// Run is the run that made the call, when there is one.
	Run string
}

// TextEdit is one exact replacement: Old must occur exactly once in the base content.
type TextEdit struct {
	Old string
	New string
}

// FileChange is one file's part of a plan. Exactly one content source applies: Delete, Content, Edits,
// Splices, or LineOps.
type FileChange struct {
	// Path is the path as the tool named it.
	Path string
	// BaseRev is the revision the change was computed against. When it is not the current revision the
	// commit refuses, except for LineOps, which may be relocated within the diff budget.
	BaseRev string
	// Delete removes the file.
	Delete bool
	// Content replaces the whole file.
	Content *[]byte
	// Edits applies exact, unique replacements to the base content.
	Edits []TextEdit
	// Splices replaces byte ranges of the base content.
	Splices []Splice
	// LineOps replaces, deletes or inserts lines of the BaseRev snapshot.
	LineOps []LineOp
	// Create requires the file to be absent.
	Create bool
	// AllowCreate lets a Content change create a missing file (write_file).
	AllowCreate bool
	// RejectStaleRead refuses a Content change when the session read this file before and the file has
	// changed since. It is what makes a whole-file write computed from an old read fail loudly instead
	// of silently dropping another writer's change.
	RejectStaleRead bool
}

// Validator checks the planned content of the whole commit before anything is written. files maps a
// canonical path to its planned bytes; a deleted file is absent. A returned error-severity diagnostic
// refuses the plan.
type Validator func(files map[string][]byte) []Diagnostic

// Plan is what a front end asks the engine to commit, all files or none.
type Plan struct {
	Actor    Actor
	Tool     string
	Changes  []FileChange
	Validate []Validator
	// Preview computes the result and the diffs but writes nothing.
	Preview bool
}

// FileResult is one file's outcome.
type FileResult struct {
	Path    string `json:"path"`
	OldRev  string `json:"oldRev"`
	NewRev  string `json:"newRev"`
	Created bool   `json:"created,omitempty"`
	Deleted bool   `json:"deleted,omitempty"`
	Changed bool   `json:"changed"`
	// Landed is where each line operation ended up in the new file, 1-based and inclusive, so a caller
	// can render fresh anchors around the edit without re-reading the whole file.
	Landed []LandedOp `json:"landed,omitempty"`
}

// LandedOp is one line operation after the commit, in the new file's line numbers.
type LandedOp struct {
	Kind  string `json:"kind"`
	Start int    `json:"start"`
	End   int    `json:"end"`
}

// Result is a commit's outcome.
type Result struct {
	Files       []FileResult `json:"files"`
	Diffs       []string     `json:"diffs,omitempty"`
	Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
}

// stripeCount is the number of per-path lock stripes a commit takes.
const stripeCount = 256

// Options configure an Engine.
type Options struct {
	// Root is the workspace the engine resolves relative paths in when Host is nil.
	Root string
	// Host is the file layer; nil selects LocalHost over Root.
	Host Host
	// Snapshots and Leases default to fresh stores. The runtime passes shared ones so every run of a
	// session sees the same snapshots and the collaboration layer sees the same leases.
	Snapshots *SnapshotStore
	Leases    *LeaseStore
	// Now overrides the clock (tests).
	Now func() time.Time
}

// Engine is the single write path. It is safe for concurrent use.
type Engine struct {
	host  Host
	root  string
	snaps *SnapshotStore
	lease *LeaseStore
	now   func() time.Time
	// stripes serialize commits per canonical path; 256 stripes bound the memory a workspace with many
	// paths would otherwise take, and a shared stripe only serializes two unrelated files.
	stripes [stripeCount]sync.Mutex
	// beforeWrite is a test hook that runs between validation and the compare-and-swap write.
	beforeWrite func(path string)
	// afterRead is a test hook that runs after a file's current content was read.
	afterRead func(path string)
}

// New builds an engine.
func New(opts Options) (*Engine, error) {
	if opts.Host == nil {
		host, err := NewLocalHost(opts.Root)
		if err != nil {
			return nil, err
		}
		opts.Host = host
	}
	if opts.Root == "" {
		opts.Root = "."
	}
	root, err := filepath.Abs(opts.Root)
	if err != nil {
		return nil, err
	}
	e := &Engine{host: opts.Host, root: root, snaps: opts.Snapshots, lease: opts.Leases, now: opts.Now}
	if e.snaps == nil {
		e.snaps = NewSnapshotStore()
	}
	if e.lease == nil {
		e.lease = NewLeaseStore()
	}
	if e.now == nil {
		e.now = time.Now
	}
	return e, nil
}

// Root is the workspace the engine was built over.
func (e *Engine) Root() string { return e.root }

// Snapshots is the session snapshot store the engine consults and fills.
func (e *Engine) Snapshots() *SnapshotStore { return e.snaps }

// Leases is the write lease store the engine enforces.
func (e *Engine) Leases() *LeaseStore { return e.lease }

// Host is the file layer the engine writes through.
func (e *Engine) Host() Host { return e.host }

// prepared is one change between resolution and the write.
type prepared struct {
	change   FileChange
	abs      string
	key      string
	rel      string
	original []byte
	next     []byte
	rev      string
	existed  bool
	created  bool
	deleted  bool
	rebased  bool
	moved    map[int]int
	ops      []LineOp
}

// Commit applies the plan: every file or none. It resolves and locks the paths, checks write leases,
// compares each file's revision, validates the result, writes with a compare-and-swap on the open
// descriptor, rehashes, and publishes edit.committed or edit.conflict.
func (e *Engine) Commit(ctx context.Context, plan Plan) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if len(plan.Changes) == 0 {
		return Result{}, &Error{Code: CodeInvalid, Detail: "a plan needs at least one file change"}
	}
	if plan.Actor.Session == "" {
		return Result{}, &Error{Code: CodeInvalid, Detail: "a commit needs an actor session"}
	}
	prep := make([]*prepared, 0, len(plan.Changes))
	seen := make(map[string]bool, len(plan.Changes))
	for i, change := range plan.Changes {
		// A missing file is resolved too, so a delete or an update of a vanished file reports
		// not_found rather than a raw lstat error; the guard still checks the deepest existing part.
		abs, rel, err := e.host.Resolve(change.Path, true)
		if err != nil {
			return Result{}, err
		}
		key := canonicalPath(abs)
		if seen[key] {
			return Result{}, &Error{Code: CodeInvalid, Path: rel, Detail: fmt.Sprintf("change %d names a file the plan already changes", i+1)}
		}
		seen[key] = true
		prep = append(prep, &prepared{change: change, abs: abs, key: key, rel: rel})
	}
	sort.Slice(prep, func(i, j int) bool { return prep[i].key < prep[j].key })
	unlock := e.lockKeys(prep)
	defer unlock()

	for _, p := range prep {
		if err := e.lease.Check(plan.Actor.Session, p.key); err != nil {
			var conflict *LeaseConflict
			if errors.As(err, &conflict) {
				err = &Error{Code: CodeLeaseConflict, Path: p.rel, Holder: conflict.Holder,
					Detail: fmt.Sprintf("a write lease is held until %s", conflict.Until.UTC().Format(time.RFC3339))}
			}
			return Result{}, e.refuse(plan, p, err)
		}
	}
	for _, p := range prep {
		if err := e.prepare(p, plan); err != nil {
			return Result{}, e.refuse(plan, p, err)
		}
	}
	diagnostics, err := e.validate(ctx, plan, prep)
	if err != nil {
		return Result{}, e.refuse(plan, nil, err)
	}
	if plan.Preview {
		result := Result{Diagnostics: diagnostics}
		for _, p := range prep {
			result.Files = append(result.Files, fileResult(p))
			result.Diffs = append(result.Diffs, UnifiedDiff(p.rel, p.original, p.next))
		}
		return result, nil
	}
	written, err := e.write(plan, prep)
	if err != nil {
		return Result{}, e.refuse(plan, nil, err)
	}
	result := Result{Diagnostics: diagnostics}
	for _, p := range written {
		result.Files = append(result.Files, fileResult(p))
		e.register(p, plan)
		e.host.Event("edit.committed", plan.Actor.Session, map[string]any{
			"path": p.rel, "oldRev": p.rev, "newRev": RevOf(p.next), "actor": plan.Actor.Session,
			"runId": plan.Actor.Run, "tool": plan.Tool, "created": p.created, "deleted": p.deleted,
		})
	}
	for _, p := range prep {
		if p.deleted {
			e.snaps.ForgetPath(plan.Actor.Session, p.key)
		}
	}
	return result, nil
}

func fileResult(p *prepared) FileResult {
	return FileResult{
		Path: p.rel, OldRev: p.rev, NewRev: RevOf(p.next),
		Created: p.created, Deleted: p.deleted,
		Changed: p.created || p.deleted || RevOf(p.next) != p.rev,
		Landed:  landedOps(p.ops),
	}
}

// landedOps maps each line operation from the coordinates it was resolved in to the new file's lines,
// so a caller can render fresh anchors around exactly what changed.
func landedOps(ops []LineOp) []LandedOp {
	if len(ops) == 0 {
		return nil
	}
	sorted := append([]LineOp(nil), ops...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Start != sorted[j].Start {
			return sorted[i].Start < sorted[j].Start
		}
		return opRank(sorted[i].Kind) < opRank(sorted[j].Kind)
	})
	out := make([]LandedOp, 0, len(sorted))
	delta := 0
	for _, op := range sorted {
		start := op.Start + delta
		switch op.Kind {
		case OpReplace:
			end := start + len(op.Text) - 1
			out = append(out, LandedOp{Kind: op.Kind, Start: start, End: end})
			delta += len(op.Text) - (op.End - op.Start + 1)
		case OpDelete:
			out = append(out, LandedOp{Kind: op.Kind, Start: start, End: start + (op.End - op.Start)})
			delta -= op.End - op.Start + 1
		case OpInsertAfter:
			out = append(out, LandedOp{Kind: op.Kind, Start: start + 1, End: start + len(op.Text)})
			delta += len(op.Text)
		case OpInsertBefore:
			out = append(out, LandedOp{Kind: op.Kind, Start: start, End: start + len(op.Text) - 1})
			delta += len(op.Text)
		}
	}
	return out
}

// SnapshotFor returns the session's snapshot of path at rev. It is how a line-edit tool turns a revision
// and anchors into resolved operations.
func (e *Engine) SnapshotFor(actor Actor, path, rev string) (*Snapshot, error) {
	snap, ok := e.snaps.Lookup(actor.Session, canonicalPath(path), rev)
	if !ok {
		return nil, &Error{Code: CodeUnknownRev, Path: path, Rev: rev,
			Detail: "that revision of this file was not read in this session"}
	}
	return snap, nil
}

// RegisterRead records that a session read path at rev without anchored lines.
func (e *Engine) RegisterRead(actor Actor, path, rev string) {
	e.snaps.RegisterRev(actor.Session, canonicalPath(path), rev)
}

// RegisterAnchoredRead records a read of path and returns the snapshot, whose seen set holds exactly
// the lines the caller is about to show.
func (e *Engine) RegisterAnchoredRead(actor Actor, path string, doc Doc, from, to int) *Snapshot {
	return e.snaps.Register(actor.Session, canonicalPath(path), doc, from, to)
}

// LatestRev is the revision of the session's most recent read or write of path, or "" when the session
// never saw it. A whole-file write uses it to tell that it would overwrite a change it never saw.
func (e *Engine) LatestRev(actor Actor, path string) string {
	snap, ok := e.snaps.Latest(actor.Session, canonicalPath(path))
	if !ok {
		return ""
	}
	return snap.Rev
}

// prepare reads a file, enforces its preconditions, and computes the planned bytes.
func (e *Engine) prepare(p *prepared, plan Plan) error {
	change := p.change
	info, statErr := os.Stat(p.abs)
	existed := statErr == nil
	if statErr != nil && !os.IsNotExist(statErr) {
		return statErr
	}
	if existed && !info.Mode().IsRegular() {
		return &Error{Code: CodeInvalid, Path: p.rel, Detail: "target is not a regular file"}
	}
	if change.Create {
		if _, err := os.Lstat(p.abs); err == nil {
			return &Error{Code: CodeExists, Path: p.rel, Detail: "create target already exists"}
		} else if !os.IsNotExist(err) {
			return err
		}
		existed = false
	}
	if !existed && !change.AllowCreate && !change.Create {
		return &Error{Code: CodeNotFound, Path: p.rel, Detail: "file does not exist"}
	}
	var original []byte
	if existed {
		f, err := e.host.Open(p.abs, false)
		if err != nil {
			return err
		}
		raw, readErr := readLimited(f, MaxFileBytes)
		closeErr := f.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		original = raw
		if e.afterRead != nil {
			e.afterRead(p.abs)
		}
	}
	p.existed, p.original, p.rev = existed, original, RevOf(original)
	if change.Delete {
		if !existed {
			return &Error{Code: CodeNotFound, Path: p.rel, Detail: "delete target does not exist"}
		}
		p.next, p.deleted = nil, true
		return nil
	}
	if existed && !utf8.Valid(original) {
		return &Error{Code: CodeNotUTF8, Path: p.rel, Detail: "the file is not UTF-8"}
	}
	doc := DecodeDoc(original)
	switch {
	case change.Content != nil:
		if err := staleUnless(change.BaseRev, p.rev); err != nil {
			return refuseStale(err, p)
		}
		if change.BaseRev == "" && change.RejectStaleRead {
			if latest, ok := e.snaps.Latest(plan.Actor.Session, p.key); ok && latest.Rev != "" && latest.Rev != p.rev {
				return &Error{Code: CodeConflict, Path: p.rel, Rev: latest.Rev, CurrentRev: p.rev,
					Detail: "the file changed since this session last read it"}
			}
		}
		p.next = append([]byte(nil), (*change.Content)...)
	case len(change.Edits) > 0:
		if err := staleUnless(change.BaseRev, p.rev); err != nil {
			return refuseStale(err, p)
		}
		next, err := applyTextEdits(original, change.Edits, p.rel)
		if err != nil {
			return err
		}
		p.next = next
	case len(change.Splices) > 0:
		if err := staleUnless(change.BaseRev, p.rev); err != nil {
			return refuseStale(err, p)
		}
		if err := ValidateSplices(change.Splices); err != nil {
			return &Error{Code: CodeInvalid, Path: p.rel, Detail: err.Error()}
		}
		next, err := ApplySplices(doc, change.Splices)
		if err != nil {
			return &Error{Code: CodeInvalid, Path: p.rel, Detail: err.Error()}
		}
		p.next = next
	case len(change.LineOps) > 0:
		snap, ok := e.snaps.Lookup(plan.Actor.Session, p.key, change.BaseRev)
		if !ok && change.BaseRev != p.rev {
			return &Error{Code: CodeUnknownRev, Path: p.rel, Rev: change.BaseRev, CurrentRev: p.rev,
				Detail: "that revision of this file was not read in this session"}
		}
		ops := change.LineOps
		if snap != nil && snap.Doc.Rev != p.rev {
			if snap.Doc.Rev == "" {
				return &Error{Code: CodeUnknownRev, Path: p.rel, Rev: change.BaseRev, CurrentRev: p.rev,
					Detail: "that revision was read without anchors, so it cannot be relocated"}
			}
			relocated, moved, err := rebaseLineOps(snap.Doc, doc, ops)
			if err != nil {
				return &Error{Code: CodeStale, Path: p.rel, Rev: change.BaseRev, CurrentRev: p.rev, Detail: err.Error()}
			}
			ops, p.moved, p.rebased = relocated, moved, true
		}
		next, err := ApplyLineOps(doc, ops)
		if err != nil {
			return &Error{Code: CodeInvalid, Path: p.rel, Detail: err.Error()}
		}
		p.ops = ops
		p.next = next
	default:
		return &Error{Code: CodeInvalid, Path: p.rel, Detail: "the change carries no content"}
	}
	if len(p.next) > MaxFileBytes {
		return &Error{Code: CodeTooLarge, Path: p.rel, Detail: fmt.Sprintf("the result is %d bytes, over the %d byte limit", len(p.next), MaxFileBytes)}
	}
	if !utf8.Valid(p.next) {
		return &Error{Code: CodeNotUTF8, Path: p.rel, Detail: "the result is not UTF-8"}
	}
	p.created = !existed
	return nil
}

func staleUnless(baseRev, currentRev string) error {
	if baseRev == "" || baseRev == currentRev {
		return nil
	}
	return fmt.Errorf("base revision %s is not current", baseRev)
}

func refuseStale(err error, p *prepared) error {
	return &Error{Code: CodeStale, Path: p.rel, CurrentRev: p.rev, Detail: err.Error()}
}

// applyTextEdits applies exact, unique replacements, the replace_in_file and apply_patch semantics.
func applyTextEdits(raw []byte, edits []TextEdit, rel string) ([]byte, error) {
	next := string(raw)
	for i, edit := range edits {
		if edit.Old == "" {
			return nil, &Error{Code: CodeInvalid, Path: rel, Detail: fmt.Sprintf("edit %d has empty old text", i+1)}
		}
		first := strings.Index(next, edit.Old)
		if first < 0 {
			return nil, &Error{Code: CodeNotFound, Path: rel, Detail: fmt.Sprintf("edit %d old_text not found", i+1)}
		}
		if strings.Index(next[first+len(edit.Old):], edit.Old) >= 0 {
			return nil, &Error{Code: CodeAmbiguous, Path: rel, Detail: fmt.Sprintf("edit %d old_text is ambiguous", i+1)}
		}
		next = next[:first] + edit.New + next[first+len(edit.Old):]
	}
	return []byte(next), nil
}

// validate runs the plan's validators over the planned content.
func (e *Engine) validate(ctx context.Context, plan Plan, prep []*prepared) ([]Diagnostic, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(plan.Validate) == 0 {
		return nil, nil
	}
	files := make(map[string][]byte, len(prep))
	for _, p := range prep {
		if !p.deleted {
			files[p.key] = p.next
		}
	}
	var all []Diagnostic
	for _, validator := range plan.Validate {
		found := validator(files)
		all = append(all, found...)
		var failure *Diagnostic
		for i := range found {
			if found[i].Severity == SeverityError {
				failure = &found[i]
				break
			}
		}
		if failure != nil {
			code := failure.Code
			if code == "" {
				code = CodeSyntax
			}
			return all, &Error{Code: code, Path: failure.Path, Detail: failure.Text, Diagnostics: all}
		}
	}
	return all, nil
}

// write applies the prepared content, all or none: a failure restores every file already written.
func (e *Engine) write(plan Plan, prep []*prepared) ([]*prepared, error) {
	written := make([]*prepared, 0, len(prep))
	for _, p := range prep {
		if p.deleted {
			if _, _, err := e.host.Resolve(p.rel, false); err != nil {
				e.rollback(written)
				return nil, err
			}
			if err := os.Remove(p.abs); err != nil {
				e.rollback(written)
				return nil, err
			}
			written = append(written, p)
			continue
		}
		if p.existed && !p.created && string(p.next) == string(p.original) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p.abs), 0o755); err != nil {
			e.rollback(written)
			return nil, err
		}
		if e.beforeWrite != nil {
			e.beforeWrite(p.abs)
		}
		if err := e.writeCAS(p); err != nil {
			e.rollback(written)
			return nil, err
		}
		written = append(written, p)
	}
	return written, nil
}

// writeCAS writes the prepared bytes only if the file still has the revision that was validated. The
// check and the write happen on the same descriptor, which is opened without truncating and refused
// when it is protected state, so a refused write leaves the file as it was.
func (e *Engine) writeCAS(p *prepared) error {
	f, err := e.host.Open(p.abs, true)
	if err != nil {
		return err
	}
	fail := func(err error) error {
		return errors.Join(err, f.Close())
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fail(err)
	}
	current, err := readLimited(f, MaxFileBytes)
	if err != nil {
		return fail(err)
	}
	if rev := RevOf(current); rev != p.rev {
		return fail(&Error{Code: CodeConflict, Path: p.rel, Rev: p.rev, CurrentRev: rev,
			Detail: "the file changed after the change was computed; nothing was written"})
	}
	if err := f.Truncate(0); err != nil {
		return fail(err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fail(err)
	}
	if _, err := f.Write(p.next); err != nil {
		return fail(err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fail(err)
	}
	after, err := readLimited(f, MaxFileBytes)
	if err != nil {
		return fail(err)
	}
	if rev := RevOf(after); rev != RevOf(p.next) {
		return fail(&Error{Code: CodeConcurrentWrite, Path: p.rel, CurrentRev: rev,
			Detail: "another writer changed the file during the write"})
	}
	return f.Close()
}

// rollback restores the files already written, newest first.
func (e *Engine) rollback(written []*prepared) {
	for i := len(written) - 1; i >= 0; i-- {
		p := written[i]
		if p.deleted {
			if err := e.writeRestore(p.abs, p.original); err != nil {
				continue
			}
			continue
		}
		if p.created {
			_ = os.Remove(p.abs)
			continue
		}
		_ = e.writeRestore(p.abs, p.original)
	}
}

func (e *Engine) writeRestore(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := e.host.Open(path, true)
	if err != nil {
		return err
	}
	if err := f.Truncate(0); err != nil {
		return errors.Join(err, f.Close())
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return errors.Join(err, f.Close())
	}
	_, err = f.Write(data)
	return errors.Join(err, f.Close())
}

// register records the new revision in the session's snapshots, so a later edit can be based on it and
// a whole-file write can tell it is stale.
func (e *Engine) register(p *prepared, plan Plan) {
	if p.deleted {
		return
	}
	e.snaps.Register(plan.Actor.Session, p.key, DecodeDoc(p.next), 0, 0)
}

// refuse publishes edit.conflict and returns the refusal.
func (e *Engine) refuse(plan Plan, p *prepared, err error) error {
	payload := map[string]any{"actor": plan.Actor.Session, "runId": plan.Actor.Run, "tool": plan.Tool,
		"code": CodeOf(err), "error": err.Error()}
	if p != nil {
		payload["path"] = p.rel
		payload["rev"] = p.change.BaseRev
		payload["currentRev"] = p.rev
	}
	if editErr, ok := AsError(err); ok {
		if payload["path"] == nil && editErr.Path != "" {
			payload["path"] = editErr.Path
		}
		payload["code"] = editErr.Code
		payload["hint"] = editErr.Hint()
		if editErr.Holder != "" {
			payload["holder"] = editErr.Holder
		}
	}
	e.host.Event("edit.conflict", plan.Actor.Session, payload)
	return err
}

// lockKeys takes one stripe per distinct key, in ascending stripe order, so two commits whose paths
// share stripes cannot deadlock.
func (e *Engine) lockKeys(prep []*prepared) func() {
	indexes := make([]int, 0, len(prep))
	seen := map[int]bool{}
	for _, p := range prep {
		index := stripeOf(p.key)
		if seen[index] {
			continue
		}
		seen[index] = true
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	for _, index := range indexes {
		e.stripes[index].Lock()
	}
	return func() {
		for i := len(indexes) - 1; i >= 0; i-- {
			e.stripes[indexes[i]].Unlock()
		}
	}
}

func stripeOf(key string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return int(h.Sum32() % uint32(stripeCount))
}

// canonicalPath resolves the symlinks of the part of path that exists and appends the rest, so two
// spellings of one file take one lock, one lease and one snapshot entry.
func canonicalPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	existing, rest := abs, ""
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return abs
		}
		rest = filepath.Join(filepath.Base(existing), rest)
		existing = parent
	}
	real, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return abs
	}
	return filepath.Join(real, rest)
}

// readLimited reads at most max bytes and refuses a longer file.
func readLimited(r io.Reader, max int) ([]byte, error) {
	buf, err := io.ReadAll(io.LimitReader(r, int64(max)+1))
	if err != nil {
		return nil, err
	}
	if len(buf) > max {
		return nil, &Error{Code: CodeTooLarge, Detail: fmt.Sprintf("the file exceeds the %d byte limit", max)}
	}
	return buf, nil
}

// LocalHost is the workspace-confining host a standalone engine uses: no protected state, the same
// workspace confinement the file tools apply (a path outside the workspace, or through a symlink out
// of it, is refused).
type LocalHost struct {
	root string
	real string
}

// NewLocalHost resolves root and returns a host over it.
func NewLocalHost(root string) (LocalHost, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return LocalHost{}, err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return LocalHost{}, err
	}
	return LocalHost{root: abs, real: real}, nil
}

// Resolve confines input to the workspace.
func (h LocalHost) Resolve(input string, allowMissing bool) (string, string, error) {
	if strings.TrimSpace(input) == "" {
		input = "."
	}
	candidate := input
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(h.root, candidate)
	}
	candidate = filepath.Clean(candidate)
	rel, err := filepath.Rel(h.root, candidate)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("path escapes workspace: %s", input)
	}
	check := candidate
	if allowMissing {
		for {
			if _, err := os.Lstat(check); err == nil {
				break
			}
			parent := filepath.Dir(check)
			if parent == check {
				return "", "", fmt.Errorf("cannot resolve parent for %s", input)
			}
			check = parent
		}
	}
	real, err := filepath.EvalSymlinks(check)
	if err != nil {
		return "", "", err
	}
	realRel, err := filepath.Rel(h.real, real)
	if err != nil || realRel == ".." || strings.HasPrefix(realRel, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("path escapes workspace through symlink: %s", input)
	}
	return candidate, filepath.ToSlash(rel), nil
}

// Open opens a file for reading and writing, creating it when asked.
func (h LocalHost) Open(path string, create bool) (*os.File, error) {
	flags := os.O_RDWR
	if create {
		flags |= os.O_CREATE
	}
	return os.OpenFile(path, flags, 0o644)
}

// Event is a no-op: a standalone engine has no event bus.
func (h LocalHost) Event(string, string, map[string]any) {}
