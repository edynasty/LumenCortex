package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Snapshots record the state of the workspace around a side-effect tool (ADR-0018, plan T1-S4). They
// are taken outside every SQLite transaction, they only read, and they never store content: a snapshot
// is a set of paths with blob ids or stat tokens, and its ref is a digest of that set.

// readOnlyGit is the extra environment of every git process a snapshot runs. GIT_OPTIONAL_LOCKS=0
// keeps `git status` from taking index.lock to refresh the index, so a snapshot never writes the
// repository and never makes a concurrent git command fail. No snapshot command writes objects or the
// index (no write-tree, no hash-object -w).
var readOnlyGit = []string{"GIT_OPTIONAL_LOCKS=0"}

// BlobAbsent is the blob of a path that does not exist.
const BlobAbsent = "-"

// FileState is one path of a FilesSnapshot: its workspace-relative slash path and its git blob id,
// BlobAbsent when the path does not exist, or "" when it exists and could not be hashed (not a regular
// file, larger than MaxBlobFileBytes, or resolving outside the workspace).
type FileState struct {
	Path string
	Blob string
}

// FilesSnapshot is the state of a named set of files.
type FilesSnapshot struct {
	// Files are sorted by path, each path once.
	Files []FileState
	// Complete is false when a named path could not be placed in the workspace or one file's state is
	// unknown ("" blob).
	Complete bool
}

// SnapshotFiles records the blob of each named path: one `git hash-object` for all of them (FileBlobs),
// and no scan of the work tree. Paths may be workspace-relative or absolute inside the workspace; one
// that is not is left out and the snapshot is incomplete. The error is the context's.
func (s *Service) SnapshotFiles(ctx context.Context, paths []string) (FilesSnapshot, error) {
	snap := FilesSnapshot{Complete: true}
	seen := map[string]bool{}
	var rels []string
	for _, p := range paths {
		abs, err := s.resolve(p)
		if err != nil || abs == s.workspace {
			snap.Complete = false
			continue
		}
		rel, err := filepath.Rel(s.workspace, abs)
		if err != nil {
			snap.Complete = false
			continue
		}
		rel = filepath.ToSlash(rel)
		if !seen[rel] {
			seen[rel] = true
			rels = append(rels, rel)
		}
	}
	sort.Strings(rels)
	if len(rels) == 0 {
		return snap, ctx.Err()
	}
	blobs, err := s.FileBlobs(ctx, rels)
	if err != nil {
		return FilesSnapshot{}, err
	}
	snap.Files = make([]FileState, 0, len(rels))
	for _, rel := range rels {
		blob := blobs[rel]
		if blob == "" {
			if _, err := os.Lstat(filepath.Join(s.workspace, filepath.FromSlash(rel))); errors.Is(err, os.ErrNotExist) {
				blob = BlobAbsent
			} else {
				snap.Complete = false
			}
		}
		snap.Files = append(snap.Files, FileState{Path: rel, Blob: blob})
	}
	return snap, nil
}

// Ref names the snapshot: the number of paths and a digest of every path with its blob.
func (f FilesSnapshot) Ref() string {
	h := sha256.New()
	for _, file := range f.Files {
		fmt.Fprintf(h, "%s\x00%s\n", file.Path, file.Blob)
	}
	return fmt.Sprintf("files:%d:sha256:%s", len(f.Files), hex.EncodeToString(h.Sum(nil)))
}

// Blob returns the recorded blob of path and whether path is in the snapshot.
func (f FilesSnapshot) Blob(path string) (string, bool) {
	i := sort.Search(len(f.Files), func(i int) bool { return f.Files[i].Path >= path })
	if i < len(f.Files) && f.Files[i].Path == path {
		return f.Files[i].Blob, true
	}
	return "", false
}

// WorkspaceSnapshot is the work tree as `git status` sees it: HEAD and, for every listed path (tracked
// changes and untracked files, ignored files excluded), its status code, size and modification time.
// Two snapshots tell which listed paths changed in between; a path that stayed clean, an ignored file
// and anything outside the work tree are not seen.
type WorkspaceSnapshot struct {
	// OK is false when the state could not be read at all: not a git work tree, or git failed.
	OK bool
	// Complete is false when the status was cut at MaxStatusBytes.
	Complete bool
	// Head is the commit HEAD names, "" in a repository without a commit.
	Head    string
	entries map[string]string
	digest  string
}

// SnapshotWorkspace runs one bounded, read-only `git status --porcelain=v2 --branch`: its
// `# branch.oid` header is HEAD and its entries are the listed paths, read by the same process.
func (s *Service) SnapshotWorkspace(ctx context.Context) WorkspaceSnapshot {
	raw, truncated, err := runBoundedEnv(ctx, s.workspace, MaxStatusBytes, readOnlyGit, snapshotGit(),
		"status", "--porcelain=v2", "-z", "--branch", "--no-ahead-behind", "--untracked-files=all")
	if err != nil {
		return WorkspaceSnapshot{}
	}
	head, entries, ok := parseStatusV2(raw)
	if !ok {
		// No `# branch.oid` header: HEAD is unknown, and a snapshot without HEAD cannot see a commit.
		return WorkspaceSnapshot{}
	}
	snap := WorkspaceSnapshot{OK: true, Complete: !truncated, Head: head, entries: map[string]string{}}
	for _, f := range entries {
		token := f.Index + f.Worktree + " " + BlobAbsent
		if info, err := os.Lstat(filepath.Join(s.workspace, filepath.FromSlash(f.Path))); err == nil {
			token = fmt.Sprintf("%s%s %d %d", f.Index, f.Worktree, info.Size(), info.ModTime().UnixNano())
		}
		snap.entries[f.Path] = token
	}
	paths := make([]string, 0, len(snap.entries))
	for p := range snap.entries {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	h := sha256.New()
	fmt.Fprintf(h, "%s\n%t\n", snap.Head, snap.Complete)
	for _, p := range paths {
		fmt.Fprintf(h, "%s\x00%s\n", p, snap.entries[p])
	}
	snap.digest = hex.EncodeToString(h.Sum(nil))
	return snap
}

// parseStatusV2 reads `git status --porcelain=v2 -z --branch` output: HEAD from `# branch.oid` (""
// for an unborn branch) and one entry per listed path with v1-style status letters (' ' for v2's
// '.'). A rename or copy lists both its new path and its source path, so a `git mv` is seen at both
// ends. ok is false without a `# branch.oid` header. A last entry cut by the output bound is read as
// far as it goes; the caller marks such a snapshot incomplete.
func parseStatusV2(raw []byte) (head string, files []GitFileStatus, ok bool) {
	parts := strings.Split(string(raw), "\x00")
	add := func(xy, path string) {
		if len(xy) != 2 || path == "" {
			return
		}
		x, y := strings.ReplaceAll(xy[:1], ".", " "), strings.ReplaceAll(xy[1:], ".", " ")
		files = append(files, GitFileStatus{Path: filepath.ToSlash(path), Index: x, Worktree: y})
	}
	for i := 0; i < len(parts); i++ {
		item := parts[i]
		switch {
		case strings.HasPrefix(item, "# branch.oid "):
			oid := strings.TrimPrefix(item, "# branch.oid ")
			if oid == "(initial)" {
				oid = ""
			}
			head, ok = oid, true
		case strings.HasPrefix(item, "1 "):
			if f := strings.SplitN(item, " ", 9); len(f) == 9 {
				add(f[1], f[8])
			}
		case strings.HasPrefix(item, "2 "):
			if f := strings.SplitN(item, " ", 10); len(f) == 10 {
				add(f[1], f[9])
				if i+1 < len(parts) {
					i++
					add(f[1], parts[i])
				}
			}
		case strings.HasPrefix(item, "u "):
			if f := strings.SplitN(item, " ", 11); len(f) == 11 {
				add(f[1], f[10])
			}
		case strings.HasPrefix(item, "? "):
			add("??", item[2:])
		}
	}
	return head, files, ok
}

// Ref names the snapshot: HEAD and a digest of the listed paths with their tokens. It is "" for a
// snapshot that is not OK.
func (w WorkspaceSnapshot) Ref() string {
	if !w.OK {
		return ""
	}
	head := w.Head
	if head == "" {
		head = "unborn"
	}
	return "ws:" + head + ":sha256:" + w.digest
}

// ChangedPaths returns the paths whose status, size or modification time differs between pre and post,
// sorted, and whether that list is the whole change the two snapshots can show: both read whole and HEAD
// unchanged. A moved HEAD means commits were made or checked out in between, and the paths they touched
// are not listed. Neither snapshot OK returns nothing and false. Another writer in the same work tree
// in between (a host command, an editor, another session) is attributed too.
func ChangedPaths(pre, post WorkspaceSnapshot) ([]string, bool) {
	if !pre.OK || !post.OK {
		return nil, false
	}
	var changed []string
	for p, token := range post.entries {
		if pre.entries[p] != token {
			changed = append(changed, p)
		}
	}
	for p := range pre.entries {
		if _, ok := post.entries[p]; !ok {
			changed = append(changed, p)
		}
	}
	sort.Strings(changed)
	return changed, pre.Complete && post.Complete && pre.Head == post.Head
}
