//go:build darwin

package editing

import (
	"os"

	"golang.org/x/sys/unix"
)

func renameNoReplacePlatform(tmpPath, dstPath string) error {
	return unix.RenameatxNp(unix.AT_FDCWD, tmpPath, unix.AT_FDCWD, dstPath, unix.RENAME_EXCL)
}

func renameSwapPlatform(srcPath, dstPath string) error {
	return unix.RenameatxNp(unix.AT_FDCWD, srcPath, unix.AT_FDCWD, dstPath, unix.RENAME_SWAP)
}

// vnodeWriteWatch observes kernel VFS write/extend events (EVFILT_VNODE) on an open descriptor so
// even a same-length in-place overwrite with coarse timestamp granularity is detected on macOS.
type vnodeWriteWatch struct {
	kq       int
	ident    uint64
	sawWrite bool
}

func newVnodeWriteWatch(f *os.File) (*vnodeWriteWatch, error) {
	if f == nil {
		return &vnodeWriteWatch{kq: -1}, nil
	}
	kq, err := unix.Kqueue()
	if err != nil {
		return &vnodeWriteWatch{kq: -1}, err
	}
	ident := uint64(f.Fd())
	reg := []unix.Kevent_t{{
		Ident:  ident,
		Filter: unix.EVFILT_VNODE,
		Flags:  unix.EV_ADD | unix.EV_ENABLE | unix.EV_CLEAR,
		Fflags: unix.NOTE_WRITE | unix.NOTE_EXTEND,
	}}
	if _, err := unix.Kevent(kq, reg, nil, nil); err != nil {
		_ = unix.Close(kq)
		return &vnodeWriteWatch{kq: -1}, err
	}
	return &vnodeWriteWatch{kq: kq, ident: ident}, nil
}

func (w *vnodeWriteWatch) wrote() bool {
	if w == nil || w.kq < 0 {
		return false
	}
	if w.sawWrite {
		return true
	}
	var events [1]unix.Kevent_t
	var zero unix.Timespec
	n, err := unix.Kevent(w.kq, nil, events[:], &zero)
	if err == nil && n > 0 && events[0].Fflags&(unix.NOTE_WRITE|unix.NOTE_EXTEND) != 0 {
		w.sawWrite = true
	}
	return w.sawWrite
}

func (w *vnodeWriteWatch) Close() error {
	if w == nil || w.kq < 0 {
		return nil
	}
	kq := w.kq
	w.kq = -1
	return unix.Close(kq)
}
