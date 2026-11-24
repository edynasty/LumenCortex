//go:build darwin

package editing

import (
	"encoding/binary"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// hasExtendedSecurity reports whether fd carries an ACL. macOS keeps ACLs outside the extended
// attribute namespace, so fgetattrlist(ATTR_CMN_EXTENDED_SECURITY) is the only cgo-free probe.
func hasExtendedSecurity(fd int) (bool, error) {
	al := unix.Attrlist{
		Bitmapcount: unix.ATTR_BIT_MAP_COUNT,
		Commonattr:  unix.ATTR_CMN_RETURNED_ATTRS | unix.ATTR_CMN_EXTENDED_SECURITY,
	}
	buf := make([]byte, 4096)
	_, _, errno := unix.Syscall6(unix.SYS_FGETATTRLIST, uintptr(fd), uintptr(unsafe.Pointer(&al)),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0, 0)
	if errno != 0 {
		return false, errno
	}
	// u_int32_t length, then attribute_set_t whose first word is the returned common attributes.
	returned := binary.LittleEndian.Uint32(buf[4:8])
	return returned&unix.ATTR_CMN_EXTENDED_SECURITY != 0, nil
}

// replicateSwapMetadata gives the staged file tmp (still empty, at tmpPath) the owner, group,
// extended attributes (including resource forks and quarantine), BSD file flags and birth time of
// orig. It refuses files with an ACL, which it cannot copy without libc. Mode bits are applied by
// the caller; set-id bits are cleared as an in-place write would clear them.
func replicateSwapMetadata(orig, tmp *os.File, tmpPath string) error {
	ofd, tfd := int(orig.Fd()), int(tmp.Fd())
	for _, fd := range []int{ofd, tfd} {
		acl, err := hasExtendedSecurity(fd)
		if err != nil {
			return errSwapMetadata("inspect ACL: %v", err)
		}
		if acl {
			return errSwapMetadata("the file or its staged copy has an ACL")
		}
	}
	if err := replicateOwnership(orig, tmp); err != nil {
		return err
	}
	if err := copyXattrs(orig, tmp, func(name string) bool { return name == "com.apple.provenance" }); err != nil {
		return err
	}
	var ost unix.Stat_t
	if err := unix.Fstat(ofd, &ost); err != nil {
		return errSwapMetadata("stat destination: %v", err)
	}
	// UF_COMPRESSED describes transparent storage compression, which a rewrite drops in place too.
	flags := ost.Flags &^ unix.UF_COMPRESSED
	if flags != 0 {
		if err := unix.Fchflags(tfd, int(flags)); err != nil {
			return errSwapMetadata("file flags %#x: %v", flags, err)
		}
	}
	var crtime [16]byte
	binary.LittleEndian.PutUint64(crtime[0:8], uint64(ost.Btim.Sec))
	binary.LittleEndian.PutUint64(crtime[8:16], uint64(ost.Btim.Nsec))
	if err := unix.Setattrlist(tmpPath, &unix.Attrlist{Bitmapcount: unix.ATTR_BIT_MAP_COUNT, Commonattr: unix.ATTR_CMN_CRTIME},
		crtime[:], unix.FSOPT_NOFOLLOW); err != nil {
		return errSwapMetadata("birth time: %v", err)
	}
	var tst unix.Stat_t
	if err := unix.Fstat(tfd, &tst); err != nil {
		return errSwapMetadata("stat staged file: %v", err)
	}
	if tst.Uid != ost.Uid || tst.Gid != ost.Gid || tst.Flags&^unix.UF_COMPRESSED != flags || tst.Btim != ost.Btim {
		return errSwapMetadata("staged owner, group, flags or birth time differ")
	}
	return nil
}
