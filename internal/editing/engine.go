package editing

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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

// AtomicFileStager is an optional Host extension for staging newly created files in a private
// sibling temp file and validating workspace/protected-state guards before atomic no-replace
// publication (ADR-0024 §8, R2-01).
type AtomicFileStager interface {
	OpenCreateTemp(targetPath string) (tmpFile *os.File, tmpPath string, err error)
	BeforeAtomicPublish(targetPath string) error
}

// NewCreateTempFile opens a private sibling temporary file in dir with O_RDWR|O_CREATE|O_EXCL.
func NewCreateTempFile(dir string) (*os.File, string, error) {
	for attempt := 0; attempt < 8; attempt++ {
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return nil, "", err
		}
		tmpPath := filepath.Join(dir, ".lcx-create-"+hex.EncodeToString(nonce[:]))
		f, err := os.OpenFile(tmpPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		return f, tmpPath, nil
	}
	return nil, "", errors.New("failed to allocate unique create temp file")
}

// errNoReplacePublishUnsupported marks a create whose filesystem supports neither a hard link nor a
// no-replace rename of the staged temp file (for example exFAT on macOS). Publishing anyway would
// need a direct create that can lose a concurrently created file, so the create is refused instead
// (ADR-0024 R3-01).
var errNoReplacePublishUnsupported = errors.New("filesystem supports neither hard links nor no-replace rename")

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
	// Binary permits a whole-file artifact replacement without text decoding.
	Binary bool
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
	Actor Actor
	Tool  string
	// Host is the caller's file layer: its workspace guard and descriptor-level protected-state check.
	// Nil uses the engine's own host (LocalHost for a standalone engine). The runtime shares one engine
	// between toolsets, so the guard travels with the call rather than the engine.
	Host     Host
	Changes  []FileChange
	Validate []Validator
	// Preview computes the result and the diffs but writes nothing.
	Preview bool
}

// PublishState distinguishes file staging, destination publication, post-publication handle/identity
// verification, uncertain mutation, and multi-file rollback without claiming file or directory fsync
// crash durability (ADR-0024 R4-02).
type PublishState string

const (
	PublishNone                   PublishState = "not_published"
	PublishSucceededAndVerified   PublishState = "published_verified"
	PublishSucceededWithPostError PublishState = "published_post_error"
	PublishUncertain              PublishState = "publication_uncertain"
	PublishRolledBack             PublishState = "published_rolled_back"
)

// CommitState distinguishes pre-publication failure, full commit, post-publication failure after
// destination visibility, uncertain publication, and partial multi-file publication (ADR-0024 R4-02).
// Committed means all planned mutations succeeded and passed in-process verification; it does not
// claim file or directory fsync crash durability.
type CommitState string

const (
	CommitStatePrePublishFailure      CommitState = "failed_before_publication"
	CommitStateCommitted              CommitState = "committed"
	CommitStatePublishedWithPostError CommitState = "published_with_post_error"
	CommitStatePublicationUncertain   CommitState = "publication_uncertain"
	CommitStatePartiallyPublished     CommitState = "partially_published"
)

type publishOutcome struct {
	state        PublishState
	published    bool
	verified     bool
	tempLeftover bool
	id           fileIdentity
}

// FileResult is one file's outcome.
type FileResult struct {
	Path         string       `json:"path"`
	OldRev       string       `json:"oldRev"`
	NewRev       string       `json:"newRev"`
	Created      bool         `json:"created,omitempty"`
	Deleted      bool         `json:"deleted,omitempty"`
	Changed      bool         `json:"changed"`
	Published    bool         `json:"published,omitempty"`
	RolledBack   bool         `json:"rolledBack,omitempty"`
	PublishState PublishState `json:"publishState,omitempty"`
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
	CommitState CommitState  `json:"commitState,omitempty"`
}

// FileCommitReceipt records the per-file causal write and publication outcome of one Engine.Commit
// call (ADR-0024 §4.6, R4-02). Staged records private temp staging; Published records destination
// visibility; Verified records post-publication handle/identity/close verification (not fsync).
type FileCommitReceipt struct {
	Path         string
	AbsPath      string
	Staged       bool
	Published    bool
	Verified     bool
	RolledBack   bool
	Wrote        bool
	PublishState PublishState
	Created      bool
	Deleted      bool
	OldSHA256    string
	NewSHA256    string
}

// CommitReceipt records whether Engine.Commit committed cleanly, whether a failure entered the disk
// mutation or destination publication window, the explicit CommitState, and sorted per-file receipts.
type CommitReceipt struct {
	Committed    bool
	DirtyFailure bool
	State        CommitState
	Files        []FileCommitReceipt
}

type commitRecorderKey struct{}

// WithCommitRecorder binds a host-owned CommitReceipt callback to ctx for one execution.
func WithCommitRecorder(ctx context.Context, fn func(CommitReceipt)) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, commitRecorderKey{}, fn)
}

// RecordCommitReceipt delivers r to the CommitReceipt recorder bound on ctx, if any.
func RecordCommitReceipt(ctx context.Context, r CommitReceipt) {
	if ctx == nil {
		return
	}
	if fn, ok := ctx.Value(commitRecorderKey{}).(func(CommitReceipt)); ok && fn != nil {
		fn(r)
	}
}

func sha256Ref(b []byte, present bool) string {
	if !present {
		return "-"
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
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
	// history holds the content of recently committed and replaced revisions for host diffs.
	history *revisionHistory
	// stripes serialize commits per canonical path; 256 stripes bound the memory a workspace with many
	// paths would otherwise take, and a shared stripe only serializes two unrelated files.
	stripes [stripeCount]sync.Mutex
	// beforeWrite is a test hook that runs between validation and the compare-and-swap write.
	beforeWrite func(path string)
	// afterRead is a test hook that runs after a file's current content was read.
	afterRead func(path string)
	// afterTruncate is a test fault hook invoked immediately after f.Truncate(0) inside writeCAS.
	afterTruncate func(path string) error
	// onRollback is a test fault hook invoked before restoring each file in rollback.
	onRollback     func(path string) error
	publishHooksMu sync.RWMutex
	publishHooks   PublishFaultHooks
}

// PublishFaultHooks provides deterministic fault injection around atomic file staging, publication,
// post-publication Stat/Close/identity checks, and uncertain-publication paths (ADR-0024 R4-02).
type PublishFaultHooks struct {
	BeforePublish       func(dstPath string) error
	ForceLinkFallback   func(dstPath string) error
	PublishUncertainErr func(dstPath string) error
	AfterLinkSuccess    func(dstPath string) error
	AfterRenameSuccess  func(dstPath string) error
	StatAfterPublish    func(dstPath string, f *os.File) (os.FileInfo, error)
	CaptureIdentity     func(dstPath string, f *os.File) error
	CloseAfterPublish   func(dstPath string) error
}

// SetTestFaultHooks installs deterministic fault-injection hooks for tests; pass nil, nil to clear.
func (e *Engine) SetTestFaultHooks(afterTruncate, onRollback func(path string) error) {
	e.afterTruncate = afterTruncate
	e.onRollback = onRollback
}

// SetPublishFaultHooks installs deterministic atomic-publication fault hooks for tests; pass zero
// PublishFaultHooks{} to clear.
func (e *Engine) SetPublishFaultHooks(hooks PublishFaultHooks) {
	e.publishHooksMu.Lock()
	e.publishHooks = hooks
	e.publishHooksMu.Unlock()
}

func (e *Engine) getPublishFaultHooks() PublishFaultHooks {
	e.publishHooksMu.RLock()
	hooks := e.publishHooks
	e.publishHooksMu.RUnlock()
	return hooks
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
	e := &Engine{host: opts.Host, root: root, snaps: opts.Snapshots, lease: opts.Leases, now: opts.Now, history: newRevisionHistory()}
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
	// wrote records that the commit really wrote this file (and did not roll it back), so a no-op
	// change or rolled-back create emits no committed snapshot registration.
	staged       bool
	published    bool
	verified     bool
	rolledBack   bool
	wrote        bool
	publishState PublishState
	createdID    fileIdentity
}

func buildFileCommitReceipt(p *prepared) FileCommitReceipt {
	state := p.publishState
	if state == "" {
		state = PublishNone
	}
	return FileCommitReceipt{
		Path:         filepath.ToSlash(p.rel),
		AbsPath:      p.abs,
		Staged:       p.staged,
		Published:    p.published,
		Verified:     p.verified,
		RolledBack:   p.rolledBack,
		Wrote:        p.wrote,
		PublishState: state,
		Created:      p.created,
		Deleted:      p.deleted,
		OldSHA256:    sha256Ref(p.original, p.existed),
		NewSHA256:    sha256Ref(p.next, !p.deleted),
	}
}

func classifyFailedCommitState(prep []*prepared, dirty bool) CommitState {
	if !dirty {
		return CommitStatePrePublishFailure
	}
	publishedCount := 0
	uncertainCount := 0
	postErrCount := 0
	for _, p := range prep {
		if p.published {
			publishedCount++
		}
		switch p.publishState {
		case PublishUncertain:
			uncertainCount++
		case PublishSucceededWithPostError:
			postErrCount++
		}
	}
	if len(prep) > 1 && publishedCount > 0 {
		return CommitStatePartiallyPublished
	}
	if publishedCount > 0 && postErrCount > 0 && uncertainCount == 0 {
		return CommitStatePublishedWithPostError
	}
	if publishedCount > 0 {
		return CommitStatePartiallyPublished
	}
	return CommitStatePublicationUncertain
}

// Commit applies the plan: every file or none. It resolves and locks the paths, checks write leases,
// compares each file's revision, validates the result, writes with a compare-and-swap on the open
// descriptor, rehashes, and publishes edit.committed or edit.conflict.
func (e *Engine) Commit(ctx context.Context, plan Plan) (Result, error) {
	failClean := func(err error) (Result, error) {
		if !plan.Preview {
			RecordCommitReceipt(ctx, CommitReceipt{
				Committed:    false,
				DirtyFailure: false,
				State:        CommitStatePrePublishFailure,
			})
		}
		return Result{CommitState: CommitStatePrePublishFailure}, err
	}
	if err := ctx.Err(); err != nil {
		return failClean(err)
	}
	if len(plan.Changes) == 0 {
		return failClean(&Error{Code: CodeInvalid, Detail: "a plan needs at least one file change"})
	}
	if plan.Actor.Session == "" {
		return failClean(&Error{Code: CodeInvalid, Detail: "a commit needs an actor session"})
	}
	host := plan.Host
	if host == nil {
		host = e.host
	}
	prep := make([]*prepared, 0, len(plan.Changes))
	seen := make(map[string]bool, len(plan.Changes))
	for i, change := range plan.Changes {
		// A missing file is resolved too, so a delete or an update of a vanished file reports
		// not_found rather than a raw lstat error; the guard still checks the deepest existing part.
		abs, rel, err := host.Resolve(change.Path, true)
		if err != nil {
			return failClean(err)
		}
		key := canonicalPath(abs)
		if seen[key] {
			return failClean(&Error{Code: CodeInvalid, Path: rel, Detail: fmt.Sprintf("change %d names a file the plan already changes", i+1)})
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
			return failClean(e.refuse(host, plan, p, err))
		}
	}
	for _, p := range prep {
		if err := e.prepare(host, p, plan); err != nil {
			return failClean(e.refuse(host, plan, p, err))
		}
	}
	diagnostics, err := e.validate(ctx, plan, prep)
	if err != nil {
		return failClean(e.refuse(host, plan, nil, err))
	}
	if err := ctx.Err(); err != nil {
		return failClean(e.refuse(host, plan, nil, err))
	}
	if plan.Preview {
		result := Result{Diagnostics: diagnostics}
		for _, p := range prep {
			result.Files = append(result.Files, fileResult(p))
			result.Diffs = append(result.Diffs, UnifiedDiff(p.rel, p.original, p.next))
		}
		return result, nil
	}
	keys := make([]string, len(prep))
	for i, p := range prep {
		keys[i] = p.key
	}
	release, err := e.lease.admit(plan.Actor.Session, keys)
	if err != nil {
		var conflict *LeaseConflict
		if errors.As(err, &conflict) {
			err = &Error{Code: CodeLeaseConflict, Path: conflict.Path, Holder: conflict.Holder,
				Detail: fmt.Sprintf("a write lease is held until %s", conflict.Until.UTC().Format(time.RFC3339))}
		}
		return failClean(e.refuse(host, plan, nil, err))
	}
	if err := ctx.Err(); err != nil {
		release()
		return failClean(err)
	}
	if _, dirty, err := e.write(host, plan, prep); err != nil {
		release()
		for _, p := range prep {
			if p.published || p.publishState == PublishUncertain || p.publishState == PublishRolledBack || p.wrote {
				dirty = true
			}
		}
		state := classifyFailedCommitState(prep, dirty)
		receiptFiles := make([]FileCommitReceipt, 0, len(prep))
		partial := Result{CommitState: state}
		for _, p := range prep {
			receiptFiles = append(receiptFiles, buildFileCommitReceipt(p))
			if p.published && !p.rolledBack {
				partial.Files = append(partial.Files, fileResult(p))
				e.register(p, plan)
				e.history.record(p)
			}
		}
		sort.Slice(receiptFiles, func(i, j int) bool { return receiptFiles[i].Path < receiptFiles[j].Path })
		if !dirty {
			receiptFiles = nil
		}
		RecordCommitReceipt(ctx, CommitReceipt{
			Committed:    false,
			DirtyFailure: dirty,
			State:        state,
			Files:        receiptFiles,
		})
		return partial, e.refuse(host, plan, nil, err)
	}
	release()
	result := Result{Diagnostics: diagnostics, CommitState: CommitStateCommitted}
	receiptFiles := make([]FileCommitReceipt, 0, len(prep))
	for _, p := range prep {
		// Every file of the plan is reported, changed or not, so a caller can read the current revision
		// even when the edit was a no-op; only a file that was really written is registered and emits an
		// event.
		result.Files = append(result.Files, fileResult(p))
		receiptFiles = append(receiptFiles, buildFileCommitReceipt(p))
		if !p.wrote {
			continue
		}
		e.register(p, plan)
		e.history.record(p)
		host.Event("edit.committed", plan.Actor.Session, map[string]any{
			"path": p.rel, "oldRev": p.rev, "newRev": RevOf(p.next), "actor": plan.Actor.Session,
			"runId": plan.Actor.Run, "tool": plan.Tool, "created": p.created, "deleted": p.deleted,
		})
	}
	for _, p := range prep {
		if p.deleted {
			e.snaps.ForgetPath(plan.Actor.Session, p.key)
		}
	}
	sort.Slice(receiptFiles, func(i, j int) bool { return receiptFiles[i].Path < receiptFiles[j].Path })
	RecordCommitReceipt(ctx, CommitReceipt{
		Committed:    true,
		DirtyFailure: false,
		State:        CommitStateCommitted,
		Files:        receiptFiles,
	})
	return result, nil
}

func fileResult(p *prepared) FileResult {
	return FileResult{
		Path: p.rel, OldRev: p.rev, NewRev: RevOf(p.next),
		Created: p.created, Deleted: p.deleted,
		Changed:      (p.created || p.deleted || RevOf(p.next) != p.rev) && !p.rolledBack,
		Published:    p.published,
		RolledBack:   p.rolledBack,
		PublishState: p.publishState,
		Landed:       landedOps(p.ops),
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
func (e *Engine) prepare(host Host, p *prepared, plan Plan) error {
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
		f, err := host.Open(p.abs, false)
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
		if err := staleUnless(change.BaseRev, p.rev); err != nil {
			return refuseStale(err, p)
		}
		p.next, p.deleted = nil, true
		return nil
	}
	if existed && !change.Binary && !utf8.Valid(original) {
		return &Error{Code: CodeNotUTF8, Path: p.rel, Detail: "the file is not UTF-8"}
	}
	var doc Doc
	if !change.Binary {
		doc = DecodeDoc(original)
	} else if change.Content == nil || len(change.Edits)+len(change.Splices)+len(change.LineOps) != 0 {
		return &Error{Code: CodeInvalid, Path: p.rel, Detail: "binary changes require whole-file content"}
	}
	switch {
	case change.Content != nil:
		if err := staleUnless(change.BaseRev, p.rev); err != nil {
			return refuseStale(err, p)
		}
		if change.BaseRev == "" && change.RejectStaleRead {
			latest, ok := e.snaps.Latest(plan.Actor.Session, p.key)
			if !ok && e.snaps.incomplete(plan.Actor.Session) && existed {
				return &Error{Code: CodeUnknownRev, Path: p.rel, CurrentRev: p.rev,
					Detail: "read history was evicted; reread this file before replacing it"}
			}
			if ok && latest.Rev != "" && latest.Rev != p.rev {
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
	if !change.Binary && !utf8.Valid(p.next) {
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

// write applies the prepared content, all or none: a failure restores every file already written in
// a multi-file plan and reports whether the commit entered the disk-mutation or publication window.
func (e *Engine) write(host Host, plan Plan, prep []*prepared) ([]*prepared, bool, error) {
	written := make([]*prepared, 0, len(prep))
	for _, p := range prep {
		p.publishState = PublishNone
		if e.beforeWrite != nil {
			e.beforeWrite(p.abs)
		}
		if p.deleted {
			if dirtyDel, err := e.deleteCAS(host, p); err != nil {
				dirty := dirtyDel || len(written) > 0
				if len(written) > 0 {
					e.rollback(host, written)
				}
				return written, dirty, err
			}
			p.published = true
			p.verified = true
			p.wrote = true
			p.publishState = PublishSucceededAndVerified
			written = append(written, p)
			continue
		}
		if p.existed && !p.created && string(p.next) == string(p.original) {
			p.verified = true
			continue
		}
		createdParent := false
		if !p.change.Binary {
			if _, statErr := os.Stat(filepath.Dir(p.abs)); os.IsNotExist(statErr) {
				createdParent = true
			}
			if err := os.MkdirAll(filepath.Dir(p.abs), 0o755); err != nil {
				dirty := len(written) > 0 || createdParent
				if len(written) > 0 {
					e.rollback(host, written)
				}
				return written, dirty, err
			}
		}
		if dirtyCAS, err := e.writeCAS(host, p); err != nil {
			dirty := dirtyCAS || p.published || len(written) > 0 || createdParent
			if p.published {
				written = append(written, p)
			}
			// In a multi-file plan, roll back all published files (including p if p published and then
			// failed a post-publication step). In a single-file plan where p was already atomically
			// published with validated bytes, never unlink the published destination (ADR-0024 R4-02).
			if len(prep) > 1 && len(written) > 0 {
				e.rollback(host, written)
			}
			return written, dirty, err
		}
		p.wrote = true
		written = append(written, p)
	}
	return written, false, nil
}

// deleteCAS rechecks both bytes and descriptor identity before unlinking. In-process
// writers are serialized; an uncooperative external writer can still race unlink.
func (e *Engine) deleteCAS(host Host, p *prepared) (bool, error) {
	if _, _, err := host.Resolve(p.rel, false); err != nil {
		return false, err
	}
	f, err := host.Open(p.abs, false)
	if err != nil {
		return false, err
	}
	defer f.Close()
	current, err := readLimited(f, MaxFileBytes)
	if err != nil {
		return false, err
	}
	if rev := RevOf(current); rev != p.rev {
		return false, &Error{Code: CodeConflict, Path: p.rel, Rev: p.rev, CurrentRev: rev,
			Detail: "delete target changed after validation; nothing was removed"}
	}
	opened, err := f.Stat()
	if err != nil {
		return false, err
	}
	named, err := os.Lstat(p.abs)
	if err != nil {
		return false, err
	}
	if !os.SameFile(opened, named) {
		return false, &Error{Code: CodeConflict, Path: p.rel, Detail: "delete target identity changed"}
	}
	if err := os.Remove(p.abs); err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
			return false, err
		}
		p.publishState = PublishUncertain
		return true, err
	}
	return false, nil
}

// writeCAS writes the prepared bytes only if the file still has the revision that was validated. The
// check and the write happen on the same descriptor, which is opened without truncating and refused
// when it is protected state, so a refused write leaves the file as it was.
func (e *Engine) createAtomicNoReplace(stager AtomicFileStager, p *prepared) (bool, error) {
	var (
		tmpFile *os.File
		tmpPath string
		err     error
	)
	if stager != nil {
		tmpFile, tmpPath, err = stager.OpenCreateTemp(p.abs)
	} else {
		tmpFile, tmpPath, err = NewCreateTempFile(filepath.Dir(p.abs))
	}
	if err != nil {
		p.publishState = PublishNone
		return false, err
	}
	p.staged = true
	abortTmp := func(cause error) (bool, error) {
		closeErr := tmpFile.Close()
		rmErr := os.Remove(tmpPath)
		leftover := rmErr != nil && !errors.Is(rmErr, os.ErrNotExist)
		if leftover {
			p.publishState = PublishUncertain
		} else {
			p.publishState = PublishNone
		}
		return leftover, errors.Join(cause, closeErr)
	}

	if e.afterTruncate != nil {
		if err := e.afterTruncate(p.abs); err != nil {
			return abortTmp(err)
		}
	}
	if _, err := tmpFile.Write(p.next); err != nil {
		return abortTmp(err)
	}
	if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
		return abortTmp(err)
	}
	after, err := readLimited(tmpFile, MaxFileBytes)
	if err != nil {
		return abortTmp(err)
	}
	if !bytes.Equal(after, p.next) {
		return abortTmp(&Error{Code: CodeConcurrentWrite, Path: p.rel, CurrentRev: RevOf(after),
			Detail: "staged temp file content changed before publication"})
	}
	if stager != nil {
		if err := stager.BeforeAtomicPublish(p.abs); err != nil {
			return abortTmp(err)
		}
	}
	hooks := e.getPublishFaultHooks()
	if hooks.BeforePublish != nil {
		if err := hooks.BeforePublish(p.abs); err != nil {
			return abortTmp(err)
		}
	}
	outcome, pubErr := publishTempNoReplace(tmpPath, tmpFile, p.abs, hooks)
	p.publishState = outcome.state
	p.published = outcome.published
	p.verified = outcome.verified
	if outcome.published {
		p.wrote = true
		p.createdID = outcome.id
	}
	if pubErr != nil {
		if !outcome.published {
			if errors.Is(pubErr, os.ErrExist) {
				return outcome.tempLeftover, &Error{Code: CodeConflict, Path: p.rel,
					Detail: "the file was created by another writer before commit; nothing was written"}
			}
			if errors.Is(pubErr, errNoReplacePublishUnsupported) {
				return outcome.tempLeftover, &Error{Code: CodeUnsupportedFilesystem, Path: p.rel,
					Detail: pubErr.Error() + ", so a new file cannot be published atomically; nothing was written"}
			}
		}
		dirty := outcome.published || outcome.state == PublishUncertain || outcome.tempLeftover
		return dirty, pubErr
	}
	return false, nil
}

func (e *Engine) writeCAS(host Host, p *prepared) (bool, error) {
	if p.change.Binary {
		if replacer, ok := host.(interface {
			ReplaceFile(path string, before, after []byte, existed bool) error
		}); ok {
			if err := replacer.ReplaceFile(p.abs, p.original, p.next, p.existed); err != nil {
				p.publishState = PublishUncertain
				return true, err
			}
			p.published = true
			p.verified = true
			p.publishState = PublishSucceededAndVerified
			return false, nil
		}
	}
	create := !p.existed
	if create && e.afterTruncate == nil {
		if stager, ok := host.(AtomicFileStager); ok {
			return e.createAtomicNoReplace(stager, p)
		}
		if _, isPlainLocalHost := host.(LocalHost); isPlainLocalHost {
			return e.createAtomicNoReplace(nil, p)
		}
	}
	f, err := host.Open(p.abs, create)
	if err != nil {
		if create && errors.Is(err, os.ErrExist) {
			return false, &Error{Code: CodeConflict, Path: p.rel,
				Detail: "the file was created by another writer before commit; nothing was written"}
		}
		if !create && errors.Is(err, os.ErrNotExist) {
			return false, &Error{Code: CodeConflict, Path: p.rel, Rev: p.rev,
				Detail: "the file was removed after the change was computed; nothing was written"}
		}
		return false, err
	}
	var createdID fileIdentity
	if create {
		var idErr error
		createdID, idErr = captureFileIdentity(f)
		if idErr != nil {
			return false, errors.Join(idErr, f.Close())
		}
		p.createdID = createdID
	}
	// Pre-write CAS verification: on create, any non-empty read or readLimited error means an
	// external writer wrote into this inode before we wrote anything — preserve their data untouched!
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		if create {
			p.publishState = PublishUncertain
			_, closeErr := removeCreatedWhileOpen(p.abs, f, createdID, nil)
			return true, errors.Join(err, closeErr)
		}
		return false, errors.Join(err, f.Close())
	}
	current, err := readLimited(f, MaxFileBytes)
	if err != nil {
		if create {
			p.publishState = PublishUncertain
			return true, errors.Join(err, f.Close())
		}
		return false, errors.Join(err, f.Close())
	}
	if rev := RevOf(current); rev != p.rev {
		conflict := &Error{Code: CodeConflict, Path: p.rel, Rev: p.rev, CurrentRev: rev,
			Detail: "the file changed after the change was computed; nothing was written"}
		return false, errors.Join(conflict, f.Close())
	}
	// Only clean pre-write failures where the inode is still provably 0-byte and unmodified by
	// any external writer may unlink; post-Write / concurrent-write failures NEVER unlink!
	failPreWrite := func(err error) (bool, error) {
		p.publishState = PublishUncertain
		if create {
			_, closeErr := removeCreatedWhileOpen(p.abs, f, createdID, nil)
			return true, errors.Join(err, closeErr)
		}
		return true, errors.Join(err, f.Close())
	}
	if err := f.Truncate(0); err != nil {
		return failPreWrite(err)
	}
	if create {
		if postTruncID, idErr := captureFileIdentity(f); idErr == nil {
			createdID = postTruncID
			p.createdID = postTruncID
		}
	}
	if e.afterTruncate != nil {
		if err := e.afterTruncate(p.abs); err != nil {
			return failPreWrite(err)
		}
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return failPreWrite(err)
	}
	if _, err := f.Write(p.next); err != nil {
		p.publishState = PublishUncertain
		return true, errors.Join(err, f.Close())
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		p.publishState = PublishUncertain
		return true, errors.Join(err, f.Close())
	}
	after, err := readLimited(f, MaxFileBytes)
	if err != nil {
		p.publishState = PublishUncertain
		return true, errors.Join(err, f.Close())
	}
	if !bytes.Equal(after, p.next) {
		p.publishState = PublishUncertain
		return true, errors.Join(&Error{Code: CodeConcurrentWrite, Path: p.rel, CurrentRev: RevOf(after),
			Detail: "another writer changed the file during the write"}, f.Close())
	}
	p.published = true
	p.wrote = true
	if create {
		if postID, idErr := captureFileIdentity(f); idErr == nil {
			p.createdID = postID
		}
	}
	if err := f.Close(); err != nil {
		p.publishState = PublishSucceededWithPostError
		return true, err
	}
	p.verified = true
	p.publishState = PublishSucceededAndVerified
	return false, nil
}

// rollback restores the files already written, newest first, without unlinking created files that
// have been modified or replaced externally.
func (e *Engine) rollback(host Host, written []*prepared) {
	for i := len(written) - 1; i >= 0; i-- {
		p := written[i]
		if e.onRollback != nil {
			if err := e.onRollback(p.abs); err != nil {
				continue
			}
		}
		if p.deleted {
			if err := e.writeRestore(host, p.abs, p.original, true); err != nil {
				continue
			}
			p.rolledBack = true
			p.wrote = false
			p.publishState = PublishRolledBack
			continue
		}
		if p.created {
			if removeCreatedAtRollback(p.abs, p.createdID, p.next) {
				p.rolledBack = true
				p.wrote = false
				p.publishState = PublishRolledBack
			}
			continue
		}
		if p.change.Binary {
			if replacer, ok := host.(interface {
				ReplaceFile(path string, before, after []byte, existed bool) error
			}); ok {
				if err := replacer.ReplaceFile(p.abs, p.next, p.original, true); err == nil {
					p.rolledBack = true
					p.wrote = false
					p.publishState = PublishRolledBack
				}
				continue
			}
		}
		if err := e.writeRestore(host, p.abs, p.original, false); err == nil {
			p.rolledBack = true
			p.wrote = false
			p.publishState = PublishRolledBack
		}
	}
}

func (e *Engine) writeRestore(host Host, path string, data []byte, create bool) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := host.Open(path, create)
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
	if p.change.Binary {
		e.snaps.RegisterRev(plan.Actor.Session, p.key, RevOf(p.next))
		return
	}
	e.snaps.Register(plan.Actor.Session, p.key, DecodeDoc(p.next), 0, 0)
}

// refuse publishes edit.conflict and returns the refusal.
func (e *Engine) refuse(host Host, plan Plan, p *prepared, err error) error {
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
	host.Event("edit.conflict", plan.Actor.Session, payload)
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

// CanonicalPath resolves the symlinks of the part of path that exists and appends the rest. Two
// spellings of one file then take one lock, one lease and one snapshot entry.
func CanonicalPath(path string) string { return canonicalPath(path) }

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

// Open opens a file for reading and writing, creating it exclusively when asked.
func (h LocalHost) Open(path string, create bool) (*os.File, error) {
	flags := os.O_RDWR
	if create {
		flags |= os.O_CREATE | os.O_EXCL
	}
	return os.OpenFile(path, flags, 0o644)
}

// Event is a no-op: a standalone engine has no event bus.
func (h LocalHost) Event(string, string, map[string]any) {}
