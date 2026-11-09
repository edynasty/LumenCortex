//go:build darwin || linux

// Package safefs provides descriptor-relative access to untrusted file trees.
// A Root is opened before untrusted code runs; every later component is opened
// without following links. Close revokes that handle, including queued operations.
package safefs

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

var ErrLimit = errors.New("safe filesystem traversal or byte limit exceeded")

type Root struct {
	mu sync.RWMutex
	fd int
}
type Entry struct {
	Name string
	Size int64
	Mode uint32
}

func (e Entry) IsDir() bool     { return e.Mode&unix.S_IFMT == unix.S_IFDIR }
func (e Entry) IsRegular() bool { return e.Mode&unix.S_IFMT == unix.S_IFREG }

// Open pins an existing trusted root. Absolute parent components must not be
// symlinks either; the host may canonicalize its own root before creating it.
func Open(path string) (*Root, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("safe root must be absolute: %q", path)
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	parts, err := components(strings.TrimPrefix(path, "/"), true)
	if err != nil {
		unix.Close(fd)
		return nil, err
	}
	for _, part := range parts {
		next, e := unix.Openat(fd, part, dirFlags, 0)
		unix.Close(fd)
		if e != nil {
			return nil, e
		}
		fd = next
	}
	return &Root{fd: fd}, nil
}

const dirFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK

func components(path string, allowRoot bool) ([]string, error) {
	if allowRoot && (path == "" || path == ".") {
		return nil, nil
	}
	if path == "" || filepath.IsAbs(path) || strings.ContainsAny(path, "\\\x00") {
		return nil, fmt.Errorf("invalid relative path %q", path)
	}
	parts := strings.Split(path, "/")
	for _, p := range parts {
		if p == "" || p == "." || p == ".." {
			return nil, fmt.Errorf("invalid path component in %q", path)
		}
	}
	return parts, nil
}
func (r *Root) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fd < 0 {
		return nil
	}
	fd := r.fd
	r.fd = -1
	return unix.Close(fd)
}
func (r *Root) dir(parts []string, create bool) (int, error) {
	if r.fd < 0 {
		return -1, os.ErrClosed
	}
	fd, err := unix.Openat(r.fd, ".", dirFlags, 0)
	if err != nil {
		return -1, err
	}
	for _, p := range parts {
		if create {
			if err := unix.Mkdirat(fd, p, 0700); err != nil && !errors.Is(err, unix.EEXIST) {
				unix.Close(fd)
				return -1, err
			}
		}
		next, e := unix.Openat(fd, p, dirFlags, 0)
		unix.Close(fd)
		if e != nil {
			return -1, e
		}
		fd = next
	}
	return fd, nil
}
func (r *Root) MkdirAll(path string) error {
	parts, err := components(path, true)
	if err != nil {
		return err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	fd, err := r.dir(parts, true)
	if err != nil {
		return err
	}
	return unix.Close(fd)
}

// ChownDir changes only an opened directory below the pinned root, without
// following symlinks. Used before a broker releases an unprivileged worker.
func (r *Root) ChownDir(path string, uid, gid int) error {
	parts, err := components(path, true)
	if err != nil {
		return err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	fd, err := r.dir(parts, false)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	return unix.Fchown(fd, uid, gid)
}

// OpenRegular opens nonblocking before fstat, rejecting FIFO/socket/device/dir
// without reading them. The returned descriptor is the exact object validated.
func (r *Root) OpenRegular(path string, limit int64) (*os.File, os.FileInfo, error) {
	parts, err := components(path, false)
	if err != nil {
		return nil, nil, err
	}
	if limit < 0 {
		return nil, nil, ErrLimit
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	fd, err := r.dir(parts[:len(parts)-1], false)
	if err != nil {
		return nil, nil, err
	}
	defer unix.Close(fd)
	n, err := unix.Openat(fd, parts[len(parts)-1], unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	f := os.NewFile(uintptr(n), path)
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = fmt.Errorf("%q is not a regular file", path)
	}
	if err == nil && info.Size() > limit {
		err = ErrLimit
	}
	// A worker cannot create host-file hard links through this API. Refuse a
	// multiply-linked inode as well, including links planted before admission.
	var stat unix.Stat_t
	if err == nil {
		err = unix.Fstat(n, &stat)
		if err == nil && stat.Nlink != 1 {
			err = fmt.Errorf("%q has multiple links", path)
		}
	}
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, info, nil
}
func (r *Root) ReadFile(path string, limit int64) ([]byte, error) {
	f, _, err := r.OpenRegular(path, limit)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, ErrLimit
	}
	return b, nil
}

// WriteFile never truncates an existing inode. A fresh exclusive file is written
// and renamed relative to the pinned parent; a replacement symlink is replaced,
// never followed. Existing non-regular entries are explicitly refused.
func (r *Root) WriteFile(path string, data []byte, mode os.FileMode) error {
	parts, err := components(path, false)
	if err != nil {
		return err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	fd, err := r.dir(parts[:len(parts)-1], false)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	name := parts[len(parts)-1]
	var st unix.Stat_t
	err = unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW)
	if err == nil {
		if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 {
			return fmt.Errorf("refusing non-regular or linked destination %q", path)
		}
	} else if !errors.Is(err, unix.ENOENT) {
		return err
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return err
	}
	tmp := ".lc-write-" + hex.EncodeToString(nonce[:])
	n, err := unix.Openat(fd, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, uint32(mode.Perm()))
	if err != nil {
		return err
	}
	defer unix.Unlinkat(fd, tmp, 0)
	f := os.NewFile(uintptr(n), tmp)
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return unix.Renameat(fd, tmp, fd, name)
}
func readEntries(fd, limit int) ([]Entry, error) {
	if limit < 1 {
		return nil, ErrLimit
	}
	copyFD, err := unix.Openat(fd, ".", dirFlags, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(copyFD), "directory")
	defer f.Close()
	names, err := f.Readdirnames(limit + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(names) > limit {
		return nil, ErrLimit
	}
	entries := make([]Entry, 0, len(names))
	for _, name := range names {
		var st unix.Stat_t
		if err := unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			return nil, err
		}
		entries = append(entries, Entry{Name: name, Size: st.Size, Mode: uint32(st.Mode)})
	}
	return entries, nil
}
func (r *Root) Entries(path string, limit int) ([]Entry, error) {
	parts, err := components(path, true)
	if err != nil {
		return nil, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	fd, err := r.dir(parts, false)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	return readEntries(fd, limit)
}
func removeAt(fd int, name string, remaining *int, depth int) error {
	if depth > 64 || *remaining <= 0 {
		return ErrLimit
	}
	*remaining--
	child, err := unix.Openat(fd, name, dirFlags, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		if errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
			return unix.Unlinkat(fd, name, 0)
		}
		return err
	}
	defer unix.Close(child)
	entries, err := readEntries(child, *remaining+1)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := removeAt(child, e.Name, remaining, depth+1); err != nil {
			return err
		}
	}
	return unix.Unlinkat(fd, name, unix.AT_REMOVEDIR)
}

// RemoveAll removes links themselves, never their targets, using directory fds.
func (r *Root) RemoveAll(path string) error {
	parts, err := components(path, false)
	if err != nil {
		return err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	fd, err := r.dir(parts[:len(parts)-1], false)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	left := 100000
	return removeAt(fd, parts[len(parts)-1], &left, 0)
}
func (r *Root) Bytes(maxEntries, maxDepth int) (int64, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	fd, err := r.dir(nil, false)
	if err != nil {
		return 0, err
	}
	defer unix.Close(fd)
	var total int64
	left := maxEntries
	var walk func(int, int) error
	walk = func(fd, depth int) error {
		if depth > maxDepth {
			return ErrLimit
		}
		entries, err := readEntries(fd, left)
		if err != nil {
			return err
		}
		for _, e := range entries {
			left--
			if left < 0 {
				return ErrLimit
			}
			if e.IsRegular() {
				total += e.Size
			}
			if e.IsDir() {
				child, err := unix.Openat(fd, e.Name, dirFlags, 0)
				if err != nil {
					return err
				}
				err = walk(child, depth+1)
				unix.Close(child)
				if err != nil {
					return err
				}
			}
		}
		return nil
	}
	err = walk(fd, 0)
	return total, err
}
