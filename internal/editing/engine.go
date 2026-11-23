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

// Plan is validated as a whole. Publication is sequential in canonical path order.
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

// PublishState describes one destination at its last observation during Commit.
// Rolled-back states remain valid for historical receipts.
type PublishState string

const (
	PublishNone                   PublishState = "not_published"
	PublishNotAttempted           PublishState = "not_attempted"
	PublishFailedBeforeWrite      PublishState = "failed_before_write"
	PublishPartial                PublishState = "partially_modified"
	PublishUnsupported            PublishState = "unsupported"
	PublishSucceededAndVerified   PublishState = "published_verified"
	PublishSucceededWithPostError PublishState = "published_post_error"
	PublishRolledBack             PublishState = "published_rolled_back"
	PublishRollbackFailed         PublishState = "rollback_failed"
	PublishExternalModification   PublishState = "external_modification_detected"
	PublishUncertain              PublishState = "publication_uncertain"
)

// CommitState distinguishes pre-publication failure, full commit, full rollback, post-publication
// failure after destination visibility, uncertain publication, and partial multi-file publication
// (ADR-0024 R4-02, R5). Committed means all planned mutations succeeded and passed in-process
// verification; it does not claim file or directory fsync crash durability.
type CommitState string

const (
	CommitStatePrePublishFailure      CommitState = "failed_before_publication"
	CommitStateCommitted              CommitState = "committed"
	CommitStateRolledBack             CommitState = "rolled_back"
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
	Verified     bool         `json:"verified,omitempty"`
	RolledBack   bool         `json:"rolledBack,omitempty"`
	PublishState PublishState `json:"publishState,omitempty"`
	Error        string       `json:"error,omitempty"`
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
// call (ADR-0024 §4.6, R4-02, R5). Staged records private temp staging; Published records destination
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
	Error        string
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
	// foldsCase is a test hook that replaces the read-only case-folding probe of a directory.
	foldsCase func(dir string) bool
	// afterTruncate is a test fault hook invoked immediately after f.Truncate(0) inside writeCAS.
	afterTruncate  func(path string) error
	publishHooksMu sync.RWMutex
	publishHooks   PublishFaultHooks
}

// PublishFaultHooks injects faults at forward file-operation boundaries in tests.
type PublishFaultHooks struct {
	BeforePublish          func(dstPath string) error
	ForceLinkFallback      func(dstPath string) error
	PublishUncertainErr    func(dstPath string) error
	AfterLinkSuccess       func(dstPath string) error
	AfterRenameSuccess     func(dstPath string) error
	StatAfterPublish       func(dstPath string, f *os.File) (os.FileInfo, error)
	CaptureIdentity        func(dstPath string, f *os.File) error
	CloseAfterPublish      func(dstPath string) error
	AfterInPlaceWriteClose func(dstPath string) error
	WriteStaged            func(dstPath string, f *os.File, data []byte) (int, error)
	AfterTruncate          func(dstPath string) error
	BeforeDeleteUnlink     func(dstPath string) error
	// BeforeFinalExternalCheck and AfterFinalExternalRead bracket the final read of a still-open
	// displaced (update) or unlinked (delete) original descriptor. They only order tests.
	BeforeFinalExternalCheck func(dstPath string)
	AfterFinalExternalRead   func(dstPath string)
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
	// Publication and verification are independent: a visible write may fail verification.
	viaSymlink   bool
	staged       bool
	published    bool
	verified     bool
	wrote        bool
	publishState PublishState
	createdID    fileIdentity
	writtenID    fileIdentity
	postErr      error
}

func (e *Engine) fileOutcome(p *prepared) (FileResult, FileCommitReceipt) {
	file := FileResult{
		Path: filepath.ToSlash(p.rel), OldRev: p.rev, Changed: p.wrote,
		Published: p.published, Verified: p.verified, PublishState: p.publishState,
		Created: p.published && p.created, Deleted: p.published && p.deleted,
		Error: e.sanitizeEngineError(p, p.postErr),
	}
	receipt := FileCommitReceipt{
		Path: file.Path, AbsPath: p.abs, Staged: p.staged, Published: p.published,
		Verified: p.verified, Wrote: p.wrote, PublishState: p.publishState,
		Created: file.Created, Deleted: file.Deleted, Error: file.Error,
	}
	if p.rev != "" {
		receipt.OldSHA256 = sha256Ref(p.original, p.existed)
	}
	if p.verified {
		receipt.NewSHA256 = sha256Ref(p.next, !p.deleted)
		if !p.deleted {
			file.NewRev = RevOf(p.next)
			file.Landed = landedOps(p.ops)
		}
	}
	return file, receipt
}

func failedCommitState(prep []*prepared, dirty bool) CommitState {
	if !dirty {
		return CommitStatePrePublishFailure
	}
	for _, p := range prep {
		if p.published || p.wrote {
			if len(prep) == 1 && (p.publishState == PublishSucceededWithPostError || p.publishState == PublishExternalModification) {
				return CommitStatePublishedWithPostError
			}
			return CommitStatePartiallyPublished
		}
	}
	return CommitStatePublicationUncertain
}

// Commit validates every operation before publication. A failure stops the plan. Already
// published paths stay in place and are observed read-only when the receipt is settled.
func (e *Engine) Commit(ctx context.Context, plan Plan) (Result, error) {
	host := plan.Host
	if host == nil {
		host = e.host
	}
	prep := make([]*prepared, len(plan.Changes))
	for i, change := range plan.Changes {
		prep[i] = &prepared{change: change, publishState: PublishNotAttempted}
	}
	refuse := func(p *prepared, err error) (Result, error) {
		if p != nil {
			p.fail(err)
		}
		// Resolution may have stopped at a refused path. Keep the remaining entries identifiable
		// without trusting their input spelling as a display path.
		for _, entry := range prep {
			if entry.abs == "" {
				entry.rel = e.relativizeDiagnosticPath(entry.change.Path, nil)
			}
		}
		result := e.settle(ctx, plan, host, prep, CommitStatePrePublishFailure, false)
		return result, e.refuse(host, plan, p, err)
	}
	if err := ctx.Err(); err != nil {
		return refuse(nil, err)
	}
	if len(prep) == 0 || plan.Actor.Session == "" {
		return refuse(nil, &Error{Code: CodeInvalid, Detail: "a commit needs an actor session and at least one file change"})
	}
	seen := make(map[string]bool, len(prep))
	for i, p := range prep {
		abs, rel, err := host.Resolve(p.change.Path, true)
		if err != nil {
			return refuse(p, err)
		}
		p.abs, p.rel, p.key = abs, rel, canonicalPath(abs)
		if seen[p.key] {
			return refuse(p, &Error{Code: CodeInvalid, Path: rel, Detail: fmt.Sprintf("change %d names a file the plan already changes", i+1)})
		}
		seen[p.key] = true
	}
	if later, earlier := e.duplicateTarget(prep); later >= 0 {
		return refuse(prep[later], &Error{Code: CodeInvalid, Path: prep[later].rel, Detail: fmt.Sprintf(
			"change %d names the same file as change %d (%s) through another spelling: a hard link or a letter-case variant",
			later+1, earlier+1, filepath.ToSlash(prep[earlier].rel))})
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
			return refuse(p, err)
		}
	}
	for _, p := range prep {
		if err := e.prepare(host, p, plan); err != nil {
			return refuse(p, err)
		}
	}
	diagnostics, err := e.validate(ctx, plan, prep)
	if err != nil {
		for _, p := range prep {
			if cause, ok := AsError(err); ok && cause.Path == filepath.ToSlash(p.rel) {
				return refuse(p, err)
			}
		}
		return refuse(nil, err)
	}
	if err := ctx.Err(); err != nil {
		return refuse(nil, err)
	}
	if plan.Preview {
		result := Result{Diagnostics: diagnostics}
		for _, p := range prep {
			result.Files = append(result.Files, FileResult{Path: p.rel, OldRev: p.rev,
				Changed: p.created || p.deleted || !bytes.Equal(p.original, p.next), Created: p.created, Deleted: p.deleted})
			result.Diffs = append(result.Diffs, UnifiedDiff(p.rel, p.original, p.next))
		}
		return result, nil
	}
	keys := make([]string, len(prep))
	relByKey := make(map[string]string, len(prep)*2)
	for i, p := range prep {
		keys[i] = p.key
		relByKey[p.key] = filepath.ToSlash(p.rel)
		relByKey[p.abs] = filepath.ToSlash(p.rel)
	}
	release, err := e.lease.admit(plan.Actor.Session, keys)
	if err != nil {
		var conflict *LeaseConflict
		if errors.As(err, &conflict) {
			err = &Error{Code: CodeLeaseConflict, Path: e.relativizeDiagnosticPath(conflict.Path, relByKey), Holder: conflict.Holder,
				Detail: fmt.Sprintf("a write lease is held until %s", conflict.Until.UTC().Format(time.RFC3339))}
		}
		return refuse(nil, err)
	}
	dirty, writeErr := e.write(ctx, host, prep)
	release()
	state := CommitStateCommitted
	if writeErr != nil {
		state = failedCommitState(prep, dirty)
	}
	result := e.settle(ctx, plan, host, prep, state, dirty && writeErr != nil)
	result.Diagnostics = diagnostics
	if writeErr != nil {
		return result, e.refuse(host, plan, nil, writeErr)
	}
	return result, nil
}

// settle consumes the same observation for snapshots, events, results and execution evidence.
func (e *Engine) settle(ctx context.Context, plan Plan, host Host, prep []*prepared, state CommitState, dirty bool) Result {
	result := Result{CommitState: state, Files: make([]FileResult, 0, len(prep))}
	receipt := CommitReceipt{Committed: state == CommitStateCommitted, DirtyFailure: dirty, State: state, Files: make([]FileCommitReceipt, 0, len(prep))}
	for _, p := range prep {
		file, rf := e.fileOutcome(p)
		result.Files = append(result.Files, file)
		receipt.Files = append(receipt.Files, rf)
		if plan.Preview || p.key == "" {
			continue
		}
		if !p.verified {
			if p.published || p.wrote || p.publishState == PublishUncertain || p.publishState == PublishExternalModification ||
				changeConflict(CodeOf(p.postErr)) ||
				(p.postErr != nil && p.rev == "" && CodeOf(p.postErr) != CodeLeaseConflict && CodeOf(p.postErr) != CodeInvalid) {
				e.snaps.InvalidatePath(plan.Actor.Session, p.key)
			}
			continue
		}
		if !p.wrote {
			continue
		}
		if p.deleted {
			e.snaps.ForgetPath(plan.Actor.Session, p.key)
		} else {
			e.register(p, plan)
		}
		e.history.record(p)
		host.Event("edit.committed", plan.Actor.Session, map[string]any{
			"path": p.rel, "oldRev": p.rev, "newRev": file.NewRev, "actor": plan.Actor.Session,
			"runId": plan.Actor.Run, "tool": plan.Tool, "created": p.created, "deleted": p.deleted,
		})
	}
	if !plan.Preview {
		sort.SliceStable(receipt.Files, func(i, j int) bool { return receipt.Files[i].Path < receipt.Files[j].Path })
		RecordCommitReceipt(ctx, receipt)
	}
	return result
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
		if linfo, lerr := os.Lstat(p.abs); lerr == nil && linfo.Mode()&os.ModeSymlink != 0 {
			p.viaSymlink = true
		}
		f, err := host.Open(p.abs, false)
		if err != nil {
			return err
		}
		id, idErr := captureFileIdentity(f)
		if idErr != nil {
			return errors.Join(idErr, f.Close())
		}
		p.writtenID = id
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
			latest, reread := e.snaps.writeView(plan.Actor.Session, p.key)
			if reread && existed {
				return &Error{Code: CodeUnknownRev, Path: p.rel, CurrentRev: p.rev,
					Detail: "read history is unavailable; reread this file before replacing it"}
			}
			if latest != nil && latest.Rev != p.rev {
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
	relByKey := make(map[string]string, len(prep)*2)
	for _, p := range prep {
		relByKey[p.key] = filepath.ToSlash(p.rel)
		relByKey[p.abs] = filepath.ToSlash(p.rel)
		if !p.deleted {
			files[p.key] = p.next
		}
	}
	var all []Diagnostic
	for _, validator := range plan.Validate {
		found := validator(files)
		for i := range found {
			found[i].Path = e.relativizeDiagnosticPath(found[i].Path, relByKey)
		}
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

// write publishes in canonical order and stops on the first refusal, short write or
// verification failure. Reobserving a completed prefix never modifies the filesystem.
func (e *Engine) write(ctx context.Context, host Host, prep []*prepared) (bool, error) {
	var dirty bool
	var firstErr error
	for _, p := range prep {
		if err := ctx.Err(); err != nil {
			firstErr = err
			break
		}
		if e.beforeWrite != nil {
			e.beforeWrite(p.abs)
		}
		if err := ctx.Err(); err != nil {
			firstErr = err
			break
		}
		p.publishState = PublishNone
		changed, err := e.applyFile(host, p)
		dirty = dirty || changed || p.wrote || p.published
		if err == nil {
			err = e.verifyActual(host, p)
		}
		if err != nil {
			p.fail(err)
			firstErr = err
			break
		}
		p.verified = true
		if p.published {
			p.publishState = PublishSucceededAndVerified
		}
	}
	// The last attempted path was just observed. Earlier successful paths may have
	// changed while a later operation was running (including during a failing syscall).
	for _, p := range prep[:len(prep)-1] {
		if p.verified {
			if err := e.verifyActual(host, p); err != nil {
				p.fail(err)
				firstErr = errors.Join(firstErr, err)
			}
		}
	}
	return dirty, firstErr
}

// changeConflict reports a code that says this session's view of the file is not the file on disk.
func changeConflict(code string) bool {
	return code == CodeConflict || code == CodeConcurrentWrite || code == CodeStale
}

func (p *prepared) fail(err error) {
	p.postErr = err
	p.verified = false
	switch CodeOf(err) {
	case CodeConflict, CodeConcurrentWrite, CodeStale:
		if p.publishState == PublishNotAttempted && !p.wrote && !p.published {
			// Refused while preparing, before LC opened the path for writing: a stale base revision or
			// an out-of-date session read is the caller's view, possibly of LC's own earlier write. It
			// is not a change observed racing this commit (R7R2-PartialCommit-02); the code says why.
			p.publishState = PublishFailedBeforeWrite
			return
		}
		p.publishState = PublishExternalModification
	case CodeUnsupportedFilesystem:
		if !p.published && p.publishState != PublishUncertain {
			p.publishState = PublishUnsupported
		}
	default:
		switch p.publishState {
		case PublishNone, PublishNotAttempted, PublishSucceededAndVerified:
			if p.published {
				p.publishState = PublishSucceededWithPostError
			} else {
				p.publishState = PublishFailedBeforeWrite
			}
		}
	}
}

func (e *Engine) applyFile(host Host, p *prepared) (bool, error) {
	if p.deleted {
		return e.deleteCAS(host, p)
	}
	if p.existed && bytes.Equal(p.next, p.original) {
		return false, nil
	}
	// Binary artifact hosts create their parents under a descriptor-relative guard.
	parentChange := false
	if p.created && !p.change.Binary {
		dir := filepath.Dir(p.abs)
		if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
			parentChange = true // MkdirAll can create a prefix even on error.
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return true, err
			}
		} else if err != nil {
			return false, err
		}
	}
	changed, err := e.writeCAS(host, p)
	return parentChange || changed, err
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
		"code": CodeOf(err), "error": e.sanitizeEngineError(p, err)}
	if p != nil {
		payload["path"] = p.rel
		payload["rev"] = p.change.BaseRev
		payload["currentRev"] = p.rev
	}
	if editErr, ok := AsError(err); ok {
		if payload["path"] == nil && editErr.Path != "" {
			payload["path"] = e.relativizeDiagnosticPath(editErr.Path, nil)
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
