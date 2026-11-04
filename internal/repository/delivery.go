package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edynasty/LumenCortex/internal/taskstate"
)

// CaptureDelivery captures file evidence once. Infrastructure failures are durable
// evidence, not Start failures; malformed host requirements are still rejected.
func (s *Service) CaptureDelivery(ctx context.Context, d *taskstate.DeliveryContract) error {
	if err := d.Validate(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	d.BaseHead, d.BaselineIssues = "", nil
	for i := range d.Files {
		f := &d.Files[i]
		f.BaseBlob = ""
		var err error
		f.Baseline, err = s.deliverySnapshot(ctx, f.Path)
		if err != nil {
			d.BaselineIssues = append(d.BaselineIssues, taskstate.DeliveryIssue{Phase: "baseline", Path: f.Path, Reason: err.Error()})
		}
	}
	if d.Commit {
		head, cut, err := runBoundedEnv(ctx, s.workspace, 256, readOnlyGit, snapshotGit(), "rev-parse", "--verify", "HEAD")
		if err != nil || cut {
			d.BaselineIssues = append(d.BaselineIssues, taskstate.DeliveryIssue{Phase: "baseline", Path: deliveryHeadPath, Reason: fmt.Sprintf("commit baseline unavailable: %v (truncated=%t)", err, cut)})
		} else {
			d.BaseHead = strings.TrimSpace(string(head))
		}
	}
	return nil
}

// VerifyDelivery is read-only and never drives Agent execution or tool replay.
// An unavailable observation is not a negative (or positive) delivery verdict.
func (s *Service) VerifyDelivery(ctx context.Context, d *taskstate.DeliveryContract) taskstate.DeliveryResult {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	r := taskstate.DeliveryResult{Status: taskstate.DeliveryVerified}
	issue := func(phase, path string, err error) {
		r.Issues = append(r.Issues, taskstate.DeliveryIssue{Phase: phase, Path: path, Reason: err.Error()})
	}
	if err := d.Validate(); err != nil {
		issue("contract", "", err)
	} else {
		observed := make(map[string]*taskstate.DeliverySnapshot, len(d.Files))
		for _, f := range d.Files {
			current, err := s.deliverySnapshot(ctx, f.Path)
			if err == nil {
				observed[f.Path] = current
			}
			evidence := taskstate.DeliveryFileEvidence{Path: f.Path, Baseline: f.Baseline, Current: current, Change: "unknown"}
			if err != nil {
				issue("verification", f.Path, err)
			} else {
				if f.Baseline != nil {
					evidence.Change = deliveryChange(*f.Baseline, *current)
				} else if f.Changed {
					if recorded := baselineIssues(d, f.Path); len(recorded) > 0 {
						r.Issues = append(r.Issues, recorded...)
					} else {
						issue("baseline", f.Path, errors.New("native baseline unavailable; historical Git blobs cannot be recaptured after execution"))
					}
				}
				if f.Absent {
					if current.Exists {
						r.Missing = append(r.Missing, "file must be absent: "+f.Path)
					}
				} else if !current.Exists || current.Kind != "regular" {
					r.Missing = append(r.Missing, "regular file required: "+f.Path)
				}
				if f.Changed && evidence.Change == "unchanged" {
					r.Missing = append(r.Missing, "file must change: "+f.Path)
				}
			}
			r.Files = append(r.Files, evidence)
		}
		if d.Commit && d.BaseHead == "" {
			r.Issues = append(r.Issues, baselineIssues(d, deliveryHeadPath)...)
		}
		if d.Branch != "" || d.Commit {
			missing, err := s.deliveryGitMissing(ctx, d, observed)
			r.Missing = append(r.Missing, missing...)
			if err != nil {
				issue("git", ".git", err)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		issue("verification", "", err)
	}
	// A general capture failure stands behind every changed file; report each distinct cause once.
	seen := map[taskstate.DeliveryIssue]bool{}
	r.Issues = slices.DeleteFunc(r.Issues, func(is taskstate.DeliveryIssue) bool {
		dup := seen[is]
		seen[is] = true
		return dup
	})
	if len(r.Issues) > 0 {
		r.Status = taskstate.DeliveryUnavailable
	} else if len(r.Missing) > 0 {
		r.Status = taskstate.DeliveryFailed
	}
	return r
}

func deliveryChange(before, after taskstate.DeliverySnapshot) string {
	switch {
	case !before.Exists && after.Exists:
		return "created"
	case before.Exists && !after.Exists:
		return "deleted"
	case before == after:
		return "unchanged"
	default:
		return "modified"
	}
}

// deliveryHeadPath names the commit baseline in issues; Validate refuses it as a file.
const deliveryHeadPath = ".git/HEAD"

// baselineIssues are the recorded capture failures behind a missing baseline for path: its own, else
// those naming no declared requirement (workspace, panic). A requirement that never compares with a
// baseline (existence, absence, branch) stays verifiable when capture failed.
func baselineIssues(d *taskstate.DeliveryContract, path string) []taskstate.DeliveryIssue {
	declared := map[string]bool{deliveryHeadPath: true}
	for _, f := range d.Files {
		declared[f.Path] = true
	}
	var own, general []taskstate.DeliveryIssue
	for _, is := range d.BaselineIssues {
		switch {
		case is.Path == path:
			own = append(own, is)
		case !declared[is.Path]:
			general = append(general, is)
		}
	}
	if len(own) > 0 {
		return own
	}
	return general
}

// maxDeliveryLinks bounds symlink expansion during one lookup, as the kernel's ELOOP limit does.
const maxDeliveryLinks = 40

// errDeliveryAbsent ends a lookup at a proven absence: a missing entry, or a non-directory with
// components left below it, reached through a directory chain that stays inside the workspace.
var errDeliveryAbsent = errors.New("delivery path absent")

// resolveDelivery walks rel one component at a time from the real workspace root, following every
// intermediate symlink and confining each step, so absence is only ever proven inside the workspace. A
// dangling link whose missing target lies elsewhere, an absolute target outside the root, ".." past the
// root that does not enter it again, or a link loop is an error, never absence. The final entry is not
// followed: a symlink there is itself the delivered entry. It returns the entry's symlink-free path and
// its Lstat.
func resolveDelivery(root, rel string) (string, os.FileInfo, error) {
	pending := strings.Split(filepath.FromSlash(rel), string(filepath.Separator))
	cur, links := root, 0
	for len(pending) > 0 {
		c := pending[0]
		pending = pending[1:]
		switch c {
		case "", ".":
			continue
		case "..":
			if cur == root {
				// root is symlink-free, so its parent is physical. A path leaving through it is
				// judged like the same absolute target: confined only if it enters the root again.
				parent := filepath.Dir(root)
				if !strings.HasSuffix(parent, string(filepath.Separator)) {
					parent += string(filepath.Separator)
				}
				inside, err := deliveryAbsoluteTarget(root, parent+strings.Join(pending, string(filepath.Separator)))
				if err != nil {
					return "", nil, err
				}
				pending = strings.Split(inside, string(filepath.Separator))
				continue
			}
			cur = filepath.Dir(cur)
			continue
		}
		next := filepath.Join(cur, c)
		info, err := os.Lstat(next)
		if errors.Is(err, os.ErrNotExist) {
			return "", nil, errDeliveryAbsent
		}
		if err != nil {
			return "", nil, err
		}
		last := len(pending) == 0
		if info.Mode()&os.ModeSymlink == 0 || last {
			if last {
				return next, info, nil
			}
			if !info.IsDir() {
				// Nothing exists below a confined regular file (ENOTDIR).
				return "", nil, errDeliveryAbsent
			}
			cur = next
			continue
		}
		if links++; links > maxDeliveryLinks {
			return "", nil, fmt.Errorf("delivery path has too many symbolic links: %s", rel)
		}
		target, err := os.Readlink(next)
		if err != nil {
			return "", nil, err
		}
		if filepath.IsAbs(target) {
			inside, err := deliveryAbsoluteTarget(root, target)
			if err != nil {
				return "", nil, err
			}
			cur, target = root, inside
		}
		pending = append(strings.Split(target, string(filepath.Separator)), pending...)
	}
	return "", nil, errors.New("delivery path names the workspace root")
}

// Find the existing absolute prefix that enters the real workspace (including
// aliases such as /var -> /private/var). Never clean the unvisited suffix: a/..
// must first inspect a, which may be a symlink, missing, or not a directory.
func deliveryAbsoluteTarget(root, target string) (string, error) {
	sep := string(filepath.Separator)
	prefix := root
	if !strings.HasSuffix(prefix, sep) {
		prefix += sep
	}
	if target == root {
		return ".", nil
	}
	if strings.HasPrefix(target, prefix) {
		return strings.TrimPrefix(target, prefix), nil
	}
	volume := filepath.VolumeName(target)
	parts := strings.Split(strings.TrimPrefix(target[len(volume):], sep), sep)
	cur := volume + sep
	for i, part := range parts {
		// cur is already resolved; resolving before the next component preserves
		// the kernel's symlink-before-parent traversal order.
		real, err := filepath.EvalSymlinks(cur + sep + part)
		if err != nil {
			return "", fmt.Errorf("delivery absolute target cannot be confined: %w", err)
		}
		if insideReal(root, real) {
			rel, err := filepath.Rel(root, real)
			if err != nil {
				return "", err
			}
			return rel + sep + strings.Join(parts[i+1:], sep), nil
		}
		cur = real
	}
	return "", errors.New("delivery path resolves outside workspace")
}

// deliverySnapshot hashes bytes in Go, independent of Git, filters, index state,
// or installed executables. Stat observations distinguish absence from read failure.
func (s *Service) deliverySnapshot(ctx context.Context, path string) (*taskstate.DeliverySnapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := filepath.EvalSymlinks(s.workspace)
	if err != nil {
		return nil, err
	}
	abs, info, err := resolveDelivery(root, path)
	if errors.Is(err, errDeliveryAbsent) {
		return &taskstate.DeliverySnapshot{}, nil
	}
	if err != nil {
		return nil, err
	}
	snap := &taskstate.DeliverySnapshot{Exists: true, Kind: "other", Mode: uint32(info.Mode()), Size: info.Size()}
	if !info.Mode().IsRegular() {
		if info.IsDir() {
			snap.Kind = "directory"
		}
		if info.Mode()&os.ModeSymlink != 0 {
			snap.Kind = "symlink"
		}
		return snap, nil
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	if !insideReal(root, real) {
		return nil, errors.New("delivery file resolves outside workspace")
	}
	file, err := os.Open(real)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return nil, errors.New("delivery file changed during inspection")
	}
	h := sha256.New()
	var buf [32 * 1024]byte
	var size int64
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := file.Read(buf[:])
		if n > 0 {
			_, _ = h.Write(buf[:n])
			size += int64(n)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	// Re-resolve the declared path, not just its old canonical target: an
	// intermediate directory link may have been replaced while the file was read.
	_, after, err := resolveDelivery(root, path)
	if err != nil {
		return nil, err
	}
	if !os.SameFile(opened, after) || size != info.Size() || after.Size() != info.Size() || after.Mode() != info.Mode() || !after.ModTime().Equal(info.ModTime()) {
		return nil, errors.New("delivery file changed during hashing")
	}
	snap.Kind, snap.SHA256 = "regular", hex.EncodeToString(h.Sum(nil))
	return snap, nil
}

// Only explicitly declared Git obligations use Git. Native hashes are never
// compared to Git object IDs, which may incorporate clean filters. current holds the confined native
// observation of each declared file: a proven absence (including below a file ancestor) is the absent
// blob without asking Git, and only existing files are hashed into Git object IDs.
func (s *Service) deliveryGitMissing(ctx context.Context, d *taskstate.DeliveryContract, current map[string]*taskstate.DeliverySnapshot) ([]string, error) {
	var missing []string
	var paths []string
	for _, f := range d.Files {
		paths = append(paths, f.Path)
	}
	blobs := map[string]string{}
	if d.Commit {
		var present []string
		for _, p := range paths {
			switch c := current[p]; {
			case c == nil:
				return nil, errors.New("committed delivery files cannot be inspected")
			case !c.Exists:
				blobs[p] = BlobAbsent
			default:
				present = append(present, p)
			}
		}
		if err := s.deliveryBlobs(ctx, present, blobs); err != nil {
			return nil, err
		}
	}
	if d.Branch != "" {
		raw, cut, err := runBoundedEnv(ctx, s.workspace, 1024, readOnlyGit, snapshotGit(), "symbolic-ref", "--quiet", "--short", "HEAD")
		var exit *exec.ExitError
		if cut || (err != nil && (!errors.As(err, &exit) || exit.ExitCode() != 1)) {
			return nil, fmt.Errorf("branch inspection unavailable: %v (truncated=%t)", err, cut)
		}
		if err != nil || strings.TrimSpace(string(raw)) != d.Branch {
			missing = append(missing, "required branch: "+d.Branch)
		}
	}
	if d.Commit {
		if len(d.BaseHead) != 40 && len(d.BaseHead) != 64 {
			return nil, errors.New("delivery commit baseline unavailable")
		}
		// A new descendant with an actual tree delta, not an empty commit or checkout.
		_, _, ancestorErr := runBoundedEnv(ctx, s.workspace, 1024, readOnlyGit, snapshotGit(), "merge-base", "--is-ancestor", d.BaseHead, "HEAD")
		var exit *exec.ExitError
		if ancestorErr != nil && (!errors.As(ancestorErr, &exit) || exit.ExitCode() != 1) {
			return nil, ancestorErr
		}
		args := []string{"--literal-pathspecs", "diff", "--no-ext-diff", "--no-textconv", "--name-only", d.BaseHead, "HEAD", "--"}
		args = append(args, paths...)
		delta, cut, err := runBoundedEnv(ctx, s.workspace, MaxStatusBytes, readOnlyGit, snapshotGit(), args...)
		if err != nil || cut {
			return nil, fmt.Errorf("commit delta unavailable: %v (truncated=%t)", err, cut)
		}
		if ancestorErr != nil || len(delta) == 0 {
			missing = append(missing, "commit with delivered changes is required")
		}
		// Only declared files must be clean; without a file list the contract covers the workspace.
		args = []string{"--literal-pathspecs", "status", "--porcelain=v1", "-z", "--untracked-files=all", "--"}
		args = append(args, paths...)
		dirty, cut, err := runBoundedEnv(ctx, s.workspace, MaxStatusBytes, readOnlyGit, snapshotGit(), args...)
		if err != nil || cut {
			return nil, fmt.Errorf("commit status unavailable: %v (truncated=%t)", err, cut)
		}
		if len(dirty) > 0 {
			missing = append(missing, "uncommitted delivery changes remain")
		}
		commitPaths := paths
		if len(paths) == 0 {
			var err error
			commitPaths, err = s.flaggedDeliveryPaths(ctx)
			if err != nil {
				return nil, err
			}
			if err := s.deliveryBlobs(ctx, commitPaths, blobs); err != nil {
				return nil, fmt.Errorf("flagged %w", err)
			}
		}
		if len(commitPaths) > 0 {
			args := append([]string{"--literal-pathspecs", "ls-tree", "-z", "HEAD", "--"}, commitPaths...)
			entries, cut, err := runBoundedEnv(ctx, s.workspace, MaxStatusBytes, readOnlyGit, snapshotGit(), args...)
			if err != nil || cut {
				return nil, errors.New("committed delivery files cannot be inspected")
			}
			committed := make(map[string]string, len(commitPaths))
			for _, entry := range strings.Split(string(entries), "\x00") {
				meta, path, ok := strings.Cut(entry, "\t")
				fields := strings.Fields(meta)
				if ok && len(fields) == 3 && fields[1] == "blob" {
					committed[path] = fields[2]
				}
			}
			for _, p := range commitPaths {
				blob := blobs[p]
				if blob == BlobAbsent && committed[p] == "" {
					continue
				}
				if blob == "" || blob == BlobAbsent || committed[p] != blob {
					missing = append(missing, "file content is not committed: "+p)
				}
			}
		}
	}
	return missing, ctx.Err()
}

// deliveryBlobs adds the Git object ID of each path to blobs; a path Git cannot hash makes the whole
// committed comparison unavailable rather than a mismatch.
func (s *Service) deliveryBlobs(ctx context.Context, paths []string, blobs map[string]string) error {
	if len(paths) == 0 {
		return nil
	}
	snap, err := s.SnapshotFiles(ctx, paths)
	if err != nil {
		return err
	}
	if !snap.Complete {
		return errors.New("delivery files cannot be inspected for commit")
	}
	for _, p := range paths {
		blobs[p], _ = snap.Blob(p)
	}
	return nil
}

// Ordinary index entries are streamed and discarded; only flagged paths consume
// the inspection budget. NUL framing preserves tabs and newlines in Git paths.
type deliveryIndexWriter struct {
	entry   []byte
	flagged []string
	bytes   int
}

func (w *deliveryIndexWriter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, 0)
		part := p
		if i >= 0 {
			part = p[:i]
		}
		if len(w.entry)+len(part) > MaxStatusBytes {
			return 0, errors.New("delivery index entry too large")
		}
		w.entry = append(w.entry, part...)
		if i < 0 {
			break
		}
		if len(w.entry) > 2 && (w.entry[0] == 'S' || (w.entry[0] >= 'a' && w.entry[0] <= 'z')) {
			w.bytes += len(w.entry)
			if w.bytes > MaxStatusBytes {
				return 0, errors.New("flagged delivery paths exceed inspection budget")
			}
			w.flagged = append(w.flagged, string(w.entry))
		}
		w.entry = w.entry[:0]
		p = p[i+1:]
	}
	return n, nil
}

func (s *Service) flaggedDeliveryPaths(ctx context.Context) ([]string, error) {
	w := &deliveryIndexWriter{}
	cmd := exec.CommandContext(ctx, snapshotGit(), "ls-files", "-v", "-z")
	cmd.Dir = s.workspace
	cmd.Env = append(os.Environ(), readOnlyGit...)
	cmd.Stdout = w
	cmd.Stderr = &cappedWriter{max: 64 << 10}
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("inspecting flagged delivery paths: %w", err)
	}
	if len(w.entry) != 0 {
		return nil, errors.New("incomplete delivery index entry")
	}
	var paths []string
	var sparse, checkedSparse bool
	for _, entry := range w.flagged {
		p := entry[2:]
		if entry[0] == 'S' || entry[0] == 's' {
			if !checkedSparse {
				raw, _, _ := runBoundedEnv(ctx, s.workspace, 32, readOnlyGit, snapshotGit(), "config", "--bool", "core.sparseCheckout")
				sparse, checkedSparse = strings.TrimSpace(string(raw)) == "true", true
			}
			if sparse {
				if _, err := os.Lstat(filepath.Join(s.workspace, filepath.FromSlash(p))); errors.Is(err, os.ErrNotExist) {
					continue
				}
			}
		}
		paths = append(paths, p)
	}
	return paths, ctx.Err()
}
