package repository

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/edynasty/LumenCortex/internal/taskstate"
)

// CaptureDelivery records the baseline once, before a Start becomes durable.
func (s *Service) CaptureDelivery(ctx context.Context, d *taskstate.DeliveryContract) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	d.BaseHead = ""
	for i := range d.Files {
		d.Files[i].BaseBlob = ""
	}
	if err := d.Validate(); err != nil {
		return err
	}
	if d.Branch != "" {
		if err := s.checkBranchName(ctx, d.Branch); err != nil {
			return err
		}
	}
	if d.Commit {
		head, _, err := runBoundedEnv(ctx, s.workspace, 256, readOnlyGit, snapshotGit(), "rev-parse", "--verify", "HEAD")
		if err != nil {
			return fmt.Errorf("delivery commit baseline: %w", err)
		}
		d.BaseHead = strings.TrimSpace(string(head))
	}
	var paths []string
	for _, f := range d.Files {
		if f.Changed {
			paths = append(paths, f.Path)
		}
	}
	if len(paths) > 0 {
		snap, err := s.SnapshotFiles(ctx, paths)
		if err != nil {
			return err
		}
		if !snap.Complete {
			return errors.New("delivery baseline files cannot be inspected")
		}
		for i := range d.Files {
			if d.Files[i].Changed {
				d.Files[i].BaseBlob, _ = snap.Blob(d.Files[i].Path)
			}
		}
	}
	return nil
}

// DeliveryMissing never writes, stages or commits. Unobservable conditions fail closed.
func (s *Service) DeliveryMissing(ctx context.Context, d *taskstate.DeliveryContract) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := d.Validate(); err != nil {
		return nil, err
	}
	var missing []string
	var paths []string
	for _, f := range d.Files {
		paths = append(paths, f.Path)
	}
	snap, err := s.SnapshotFiles(ctx, paths)
	if err != nil {
		return nil, err
	}
	for _, f := range d.Files {
		_, _, ok := ConfinedFile(s.workspace, filepath.Join(s.workspace, filepath.FromSlash(f.Path)))
		blob, found := snap.Blob(f.Path)
		if !ok || !found || blob == "" || blob == BlobAbsent {
			missing = append(missing, "file unavailable: "+f.Path)
			continue
		}
		if f.Changed && (f.BaseBlob == "" || blob == f.BaseBlob) {
			missing = append(missing, "file must change: "+f.Path)
		}
	}
	if d.Branch != "" {
		raw, _, err := runBoundedEnv(ctx, s.workspace, 1024, readOnlyGit, snapshotGit(), "symbolic-ref", "--quiet", "--short", "HEAD")
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
		args := []string{"--literal-pathspecs", "diff", "--no-ext-diff", "--no-textconv", "--name-only", d.BaseHead, "HEAD", "--"}
		args = append(args, paths...)
		delta, cut, err := runBoundedEnv(ctx, s.workspace, MaxStatusBytes, readOnlyGit, snapshotGit(), args...)
		if err != nil {
			return nil, err
		}
		if ancestorErr != nil || cut || len(delta) == 0 {
			missing = append(missing, "commit with delivered changes is required")
		}
		// Only declared files must be clean; without a file list the contract covers the workspace.
		args = []string{"--literal-pathspecs", "status", "--porcelain=v1", "-z", "--untracked-files=all", "--"}
		args = append(args, paths...)
		dirty, cut, err := runBoundedEnv(ctx, s.workspace, MaxStatusBytes, readOnlyGit, snapshotGit(), args...)
		if err != nil {
			return nil, err
		}
		if cut || len(dirty) > 0 {
			missing = append(missing, "uncommitted delivery changes remain")
		}
		commitPaths := paths
		if len(paths) == 0 {
			var err error
			commitPaths, err = s.flaggedDeliveryPaths(ctx)
			if err != nil {
				return nil, err
			}
			if len(commitPaths) > 0 {
				snap, err = s.SnapshotFiles(ctx, commitPaths)
				if err != nil {
					return nil, err
				}
				if !snap.Complete {
					return nil, errors.New("flagged delivery files cannot be inspected")
				}
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
				blob, _ := snap.Blob(p)
				if blob == "" || blob == BlobAbsent || committed[p] != blob {
					missing = append(missing, "file content is not committed: "+p)
				}
			}
		}
	}
	return missing, ctx.Err()
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
