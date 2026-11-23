package editing

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// A binary artifact host uses descriptor-relative replacement. Existing files on stageable hosts are
// replaced by a staged atomic swap; new files require staged no-replace publication. Anything that
// cannot be published that way (hard-linked files, metadata a swap cannot reproduce, hosts without
// staging) is refused as unsupported, never rewritten in place. Neither is a multi-file CAS.
type replaceFileHost interface {
	ReplaceFile(path string, before, after []byte, existed bool) error
}

func isPlainLocalHost(host Host) bool {
	switch host.(type) {
	case LocalHost, *LocalHost:
		return true
	}
	return false
}

func allocateSiblingName(dir, prefix string) (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	return filepath.Join(dir, prefix+hex.EncodeToString(nonce[:])), nil
}

// writeCAS applies one non-deleted change. It returns true when the destination may have changed
// (including a known partial in-place mutation and unprovable private staging debris), and false
// when no destination write happened. p.published/p.wrote are set only for known effects: a
// completed publication, a successful truncate or write, or a known partial mutation. A clean
// pre-write refusal leaves both false.
func (e *Engine) writeCAS(host Host, p *prepared) (bool, error) {
	if p.change.Binary {
		if replacer, ok := host.(replaceFileHost); ok {
			return e.replaceBinary(host, replacer, p)
		}
	}
	if p.created {
		return e.createAtomicNoReplace(host, p)
	}
	if _, hasStager := host.(AtomicFileStager); hasStager || isPlainLocalHost(host) {
		return e.updateAtomicSwap(host, p)
	}
	// A host without private sibling staging could only truncate and rewrite the live file.
	p.publishState = PublishUnsupported
	return false, &Error{Code: CodeUnsupportedFilesystem, Path: p.rel,
		Detail: "this host cannot stage a replacement next to the file, so it cannot be updated atomically; nothing was written"}
}

// errMultiLinkUpdate refuses an update of a file that more than one directory entry names: atomic
// replacement would detach the other hard links, and an in-place rewrite is not failure-safe.
func errMultiLinkUpdate(rel string, links uint64) error {
	return &Error{Code: CodeUnsupportedFilesystem, Path: rel,
		Detail: fmt.Sprintf("the file has %d hard links; LC does not update hard-linked files because replacing it would detach the other links; nothing was written", links)}
}

// replaceBinary publishes binary content through a host's descriptor-rooted replacement
// primitive (the artifact host's safefs CompareAndSwap). It preserves that host's guards and
// parent-directory creation. A replacement error is a clean pre-write refusal: the primitive
// publishes only after its own destination check, and rename/link failure leaves the destination
// as it was. The actual destination bytes and identity are re-observed by verifyActual.
func (e *Engine) replaceBinary(host Host, replacer replaceFileHost, p *prepared) (bool, error) {
	if err := replacer.ReplaceFile(p.abs, p.original, p.next, p.existed); err != nil {
		// Distinguish an observed external change from an inspection refusal without claiming
		// any destination effect of this commit.
		p.publishState = PublishFailedBeforeWrite
		if observed, observeErr := e.observeBeforeWrite(host, p); observeErr != nil {
			return false, err
		} else if observed != nil {
			return false, &Error{Code: CodeConflict, Path: p.rel, Rev: p.rev, CurrentRev: RevOf(observed),
				Detail: "binary replacement was refused because the destination changed; nothing was written"}
		}
		return false, err
	}
	p.published = true
	p.wrote = true
	id, idErr := e.captureIdentityThroughHost(host, p)
	if idErr != nil {
		// The replacement reported success, but the published identity cannot be observed.
		// Publication is known; verification is not.
		p.publishState = PublishUncertain
		return true, idErr
	}
	if p.created {
		p.createdID = id
	} else {
		p.writtenID = id
	}
	p.publishState = PublishSucceededAndVerified
	return false, nil
}

// captureIdentityThroughHost opens the destination through the host's guard and captures its
// identity. The host's guard (workspace confinement and protected-state check) runs on the
// descriptor, so a path swapped after publication is reported by identity verification later.
func (e *Engine) captureIdentityThroughHost(host Host, p *prepared) (fileIdentity, error) {
	f, err := host.Open(p.abs, false)
	if err != nil {
		return fileIdentity{}, err
	}
	id, idErr := captureFileIdentity(f)
	closeErr := f.Close()
	return id, errors.Join(idErr, closeErr)
}

// observeBeforeWrite reads the current destination bytes without modifying it. A nil slice with
// nil error means the destination still holds the bytes the plan was computed from.
func (e *Engine) observeBeforeWrite(host Host, p *prepared) ([]byte, error) {
	f, err := host.Open(p.abs, false)
	if err != nil {
		return nil, err
	}
	current, readErr := readLimited(f, MaxFileBytes)
	closeErr := f.Close()
	if readErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if !bytes.Equal(current, p.original) {
		return current, nil
	}
	return nil, nil
}

func openPrivateStageTemp(host Host, targetAbs string) (*os.File, string, AtomicFileStager, bool, error) {
	stager, hasStager := host.(AtomicFileStager)
	switch {
	case hasStager:
		f, path, err := stager.OpenCreateTemp(targetAbs)
		return f, path, stager, true, err
	case isPlainLocalHost(host):
		f, path, err := NewCreateTempFile(filepath.Dir(targetAbs))
		return f, path, nil, false, err
	default:
		return nil, "", nil, false, errNoReplacePublishUnsupported
	}
}

// updateAtomicSwap updates an existing regular file by staging the full replacement into a private
// sibling temporary file first (closing EDITING-PRE-01: ENOSPC, EFBIG and short writes never touch
// the live destination) and then publishing via kernel atomic swap (RENAME_SWAP on macOS,
// RENAME_EXCHANGE on Linux) while holding the original descriptor and EVFILT_VNODE watch across the
// compare-to-publish window so concurrent external writes are neither overwritten nor corrupted into
// mixed bytes (closing EDITING-PRE-02).
func (e *Engine) updateAtomicSwap(host Host, p *prepared) (bool, error) {
	linfo, lerr := os.Lstat(p.abs)
	if lerr != nil {
		p.publishState = PublishFailedBeforeWrite
		if errors.Is(lerr, os.ErrNotExist) {
			return false, &Error{Code: CodeConflict, Path: p.rel, Rev: p.rev,
				Detail: "the file was removed after the change was computed; nothing was written"}
		}
		return false, lerr
	}
	targetAbs := p.abs
	if linfo.Mode()&os.ModeSymlink != 0 {
		evalPath, err := filepath.EvalSymlinks(p.abs)
		if err != nil {
			p.publishState = PublishFailedBeforeWrite
			if errors.Is(err, os.ErrNotExist) {
				return false, &Error{Code: CodeConflict, Path: p.rel, Rev: p.rev,
					Detail: "the symlink target was removed after the change was computed; nothing was written"}
			}
			return false, err
		}
		resolvedAbs, _, err := host.Resolve(evalPath, false)
		if err != nil {
			p.publishState = PublishFailedBeforeWrite
			return false, err
		}
		targetAbs = resolvedAbs
	}

	origFile, err := host.Open(targetAbs, false)
	if err != nil {
		p.publishState = PublishFailedBeforeWrite
		if errors.Is(err, os.ErrNotExist) {
			return false, &Error{Code: CodeConflict, Path: p.rel, Rev: p.rev,
				Detail: "the file was removed after the change was computed; nothing was written"}
		}
		return false, err
	}
	origClosed := false
	closeOrig := func() error {
		if origClosed || origFile == nil {
			return nil
		}
		origClosed = true
		return origFile.Close()
	}

	openedID, idErr := captureFileIdentity(origFile)
	if idErr != nil {
		p.publishState = PublishFailedBeforeWrite
		return false, errors.Join(idErr, closeOrig())
	}
	if p.writtenID.valid && !p.writtenID.matchesInfo(openedID.info) {
		p.publishState = PublishFailedBeforeWrite
		return false, errors.Join(&Error{Code: CodeConflict, Path: p.rel, Rev: p.rev,
			Detail: "the file was replaced after the change was computed; nothing was written"}, closeOrig())
	}

	// A swap would leave the other hard links on the old inode. Refuse instead of breaking the aliases
	// or rewriting the shared inode in place (prepare refuses this too; the link count can change).
	if openedID.linkCount() > 1 {
		p.publishState = PublishUnsupported
		return false, errors.Join(errMultiLinkUpdate(p.rel, openedID.linkCount()), closeOrig())
	}

	watch, _ := newVnodeWriteWatch(origFile)
	defer func() { _ = watch.Close() }()

	readOrig := func() ([]byte, error) {
		if _, err := origFile.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		return readLimited(origFile, MaxFileBytes)
	}

	current, err := readOrig()
	if err != nil {
		p.publishState = PublishFailedBeforeWrite
		if CodeOf(err) == CodeTooLarge {
			return false, errors.Join(&Error{Code: CodeConcurrentWrite, Path: p.rel, Rev: p.rev,
				Detail: "the file grew beyond the edit limit after the change was computed; nothing was written"}, closeOrig())
		}
		return false, errors.Join(err, closeOrig())
	}
	if !bytes.Equal(current, p.original) {
		p.publishState = PublishFailedBeforeWrite
		return false, errors.Join(&Error{Code: CodeConflict, Path: p.rel, Rev: p.rev, CurrentRev: RevOf(current),
			Detail: "the file changed after the change was computed; nothing was written"}, closeOrig())
	}

	tmpFile, tmpPath, stager, hasStager, err := openPrivateStageTemp(host, targetAbs)
	if err != nil {
		p.publishState = PublishFailedBeforeWrite
		return false, errors.Join(err, closeOrig())
	}
	tmpClosed := false
	closeTmp := func() error {
		if tmpClosed || tmpFile == nil {
			return nil
		}
		tmpClosed = true
		return tmpFile.Close()
	}

	tmpID, idErr := captureFileIdentity(tmpFile)
	if idErr != nil {
		p.publishState = PublishUncertain
		return true, errors.Join(idErr, closeTmp(), closeOrig())
	}

	abortBeforeSwap := func(cause error, expected []byte) (bool, error) {
		id, cerr := captureFileIdentity(tmpFile)
		if cerr != nil || !tmpID.matchesInfo(id.info) {
			p.publishState = PublishUncertain
			return true, errors.Join(cause, cerr, closeTmp(), closeOrig())
		}
		tmpClosed = true
		removed, rmCloseErr := removeCreatedWhileOpen(tmpPath, tmpFile, id, expected)
		origCloseErr := closeOrig()
		if removed {
			p.publishState = PublishFailedBeforeWrite
			return false, errors.Join(cause, rmCloseErr, origCloseErr)
		}
		p.publishState = PublishUncertain
		return true, errors.Join(cause, rmCloseErr, origCloseErr)
	}

	// A swap publishes a different inode. Reproduce the metadata it would otherwise drop (owner,
	// group, extended attributes, file flags, birth time) on the staged file; when that cannot be
	// reproduced exactly (for example a macOS ACL), refuse before the destination is touched.
	if metaErr := replicateSwapMetadata(origFile, tmpFile, tmpPath); metaErr != nil {
		return abortBeforeSwap(&Error{Code: CodeUnsupportedFilesystem, Path: p.rel,
			Detail: metaErr.Error() + "; nothing was written"}, nil)
	}

	if err := tmpFile.Chmod(openedID.info.Mode().Perm()); err != nil {
		return abortBeforeSwap(err, nil)
	}

	hooks := e.getPublishFaultHooks()
	var (
		n        int
		writeErr error
	)
	if hooks.WriteStaged != nil {
		n, writeErr = hooks.WriteStaged(targetAbs, tmpFile, p.next)
	} else {
		n, writeErr = tmpFile.Write(p.next)
	}
	if n < 0 || n > len(p.next) {
		p.publishState = PublishUncertain
		return true, errors.Join(io.ErrShortWrite, writeErr, closeTmp(), closeOrig())
	}
	if n != len(p.next) || writeErr != nil {
		if writeErr == nil {
			writeErr = io.ErrShortWrite
		}
		return abortBeforeSwap(writeErr, p.next[:n])
	}
	// Flush the staged bytes so a deferred allocation or quota failure surfaces before publication.
	if err := tmpFile.Sync(); err != nil {
		return abortBeforeSwap(err, p.next)
	}
	if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
		return abortBeforeSwap(err, p.next)
	}
	staged, err := readLimited(tmpFile, MaxFileBytes)
	if err != nil {
		return abortBeforeSwap(err, p.next)
	}
	if !bytes.Equal(staged, p.next) {
		return abortBeforeSwap(&Error{Code: CodeConcurrentWrite, Path: p.rel, CurrentRev: RevOf(staged),
			Detail: "staged temporary file content changed before publication"}, nil)
	}

	// Capture post-write temp identity (including updated mtime) and verify tmpPath before swapping.
	stagedID, idErr := captureFileIdentity(tmpFile)
	if idErr != nil || !tmpID.matchesInfo(stagedID.info) {
		p.publishState = PublishUncertain
		return true, errors.Join(idErr, closeTmp(), closeOrig())
	}
	tmpNamed, err := os.Lstat(tmpPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			p.publishState = PublishFailedBeforeWrite
			return false, errors.Join(&Error{Code: CodeConflict, Path: p.rel,
				Detail: "the staged temporary file disappeared before publication; nothing was written"}, closeTmp(), closeOrig())
		}
		p.publishState = PublishUncertain
		return true, errors.Join(err, closeTmp(), closeOrig())
	}
	if !stagedID.matchesInfo(tmpNamed) || !os.SameFile(stagedID.info, tmpNamed) || tmpNamed.Size() != int64(len(p.next)) {
		p.publishState = PublishUncertain
		return true, errors.Join(&Error{Code: CodeConcurrentWrite, Path: p.rel,
			Detail: "the staged temporary file was replaced before publication; nothing was written"}, closeTmp(), closeOrig())
	}

	if hasStager {
		if err := stager.BeforeAtomicPublish(p.abs); err != nil {
			return abortBeforeSwap(err, p.next)
		}
		if targetAbs != p.abs {
			if err := stager.BeforeAtomicPublish(targetAbs); err != nil {
				return abortBeforeSwap(err, p.next)
			}
		}
	}
	if hooks.BeforePublish != nil {
		if err := hooks.BeforePublish(p.abs); err != nil {
			return abortBeforeSwap(err, p.next)
		}
	}
	if e.afterTruncate != nil {
		if err := e.afterTruncate(p.abs); err != nil {
			return abortBeforeSwap(err, p.next)
		}
	}
	if hooks.AfterTruncate != nil {
		if err := hooks.AfterTruncate(p.abs); err != nil {
			return abortBeforeSwap(err, p.next)
		}
	}

	// Re-verify the live destination descriptor and path immediately before swapping so any external
	// write (short, same-length or long) or replacement in the compare-to-swap window is refused cleanly
	// without ever modifying targetAbs.
	preSwapBytes, preSwapReadErr := readOrig()
	if preSwapReadErr != nil {
		if CodeOf(preSwapReadErr) == CodeTooLarge {
			return abortBeforeSwap(&Error{Code: CodeConcurrentWrite, Path: p.rel, Rev: p.rev,
				Detail: "the file grew beyond the edit limit after the change was computed; nothing was written"}, p.next)
		}
		return abortBeforeSwap(preSwapReadErr, p.next)
	}
	if watch.wrote() || !bytes.Equal(preSwapBytes, p.original) {
		return abortBeforeSwap(&Error{Code: CodeConflict, Path: p.rel, Rev: p.rev, CurrentRev: RevOf(preSwapBytes),
			Detail: "the file changed after the change was computed; nothing was written"}, p.next)
	}
	dstNamed, err := os.Lstat(targetAbs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return abortBeforeSwap(&Error{Code: CodeConflict, Path: p.rel, Rev: p.rev,
				Detail: "the file was removed after the change was computed; nothing was written"}, p.next)
		}
		return abortBeforeSwap(err, p.next)
	}
	if !dstNamed.Mode().IsRegular() || !os.SameFile(openedID.info, dstNamed) ||
		!openedID.matchesInfo(dstNamed) || dstNamed.Size() != int64(len(p.original)) {
		return abortBeforeSwap(&Error{Code: CodeConflict, Path: p.rel, Rev: p.rev,
			Detail: "the file was replaced after the change was computed; nothing was written"}, p.next)
	}
	// Sample the opened inode again after staging and the pathname check. Planning and open-time
	// counts miss aliases created while staging; this observation is not atomic with the swap.
	currentID, idErr := captureFileIdentity(origFile)
	if idErr != nil {
		return abortBeforeSwap(idErr, p.next)
	}
	if links := currentID.linkCount(); links > 1 {
		return abortBeforeSwap(errMultiLinkUpdate(p.rel, links), p.next)
	}

	usedSwap := true
	swapErr := renameSwapForPublish(tmpPath, targetAbs)
	if swapErr != nil && isSwapRenameUnsupportedErr(swapErr) {
		usedSwap = false
		if closeHandlesBeforeFallbackRename {
			if cerr := errors.Join(closeTmp(), closeOrig()); cerr != nil {
				_ = os.Remove(tmpPath)
				p.publishState = PublishFailedBeforeWrite
				return false, cerr
			}
		}
		swapErr = os.Rename(tmpPath, targetAbs)
	}
	if swapErr != nil {
		// Nothing swapped: clean up our private staging file if it is still ours.
		if linfo, lerr := os.Lstat(tmpPath); lerr == nil && stagedID.matchesOwnedCleanup(linfo) && linfo.Size() == int64(len(p.next)) {
			_ = os.Remove(tmpPath)
		}
		_ = closeTmp()
		_ = closeOrig()
		if errors.Is(swapErr, os.ErrNotExist) {
			p.publishState = PublishFailedBeforeWrite
			return false, &Error{Code: CodeConflict, Path: p.rel, Rev: p.rev,
				Detail: "the file was removed before publication; nothing was written"}
		}
		if errors.Is(swapErr, os.ErrPermission) || isCleanPrePublishErr(swapErr) {
			p.publishState = PublishFailedBeforeWrite
			return false, swapErr
		}
		p.publishState = PublishUncertain
		return true, swapErr
	}

	p.published = true
	p.wrote = true

	var postID fileIdentity
	var postIDErr error
	if !tmpClosed {
		postID, postIDErr = captureFileIdentity(tmpFile)
	} else {
		postID, postIDErr = e.captureIdentityThroughHost(host, p)
	}
	closeErr := closeTmp()
	if hooks.AfterInPlaceWriteClose != nil {
		closeErr = errors.Join(closeErr, hooks.AfterInPlaceWriteClose(p.abs))
	}

	removeDisplacedOrig := func() {
		if !usedSwap {
			return
		}
		if linfo, lerr := os.Lstat(tmpPath); lerr == nil && openedID.matchesInfo(linfo) {
			_ = os.Remove(tmpPath)
		}
	}

	if postIDErr != nil || closeErr != nil {
		// The outcome is uncertain: keep the displaced original (it may hold external writes) rather
		// than deleting it to tidy up a private name.
		_ = closeOrig()
		p.publishState = PublishUncertain
		if usedSwap {
			return true, errors.Join(postIDErr, closeErr,
				fmt.Errorf("the displaced original was kept at %s", tmpPath))
		}
		return true, errors.Join(postIDErr, closeErr)
	}
	p.writtenID = postID

	// Check whether an uncooperative external writer wrote to the displaced original descriptor while
	// the swap was in flight. If so, put the intact external inode back (swap) or preserve its bytes
	// (no-swap fallback) without overwriting any entry another writer may have placed at targetAbs.
	// Writes through a pre-opened descriptor after this final check reach the displaced inode only
	// and are outside the guarantee.
	if !origClosed {
		if hooks.BeforeFinalExternalCheck != nil {
			hooks.BeforeFinalExternalCheck(p.abs)
		}
		afterOrig, afterOrigErr := readOrig()
		if hooks.AfterFinalExternalRead != nil {
			hooks.AfterFinalExternalRead(p.abs)
		}
		if watch.wrote() || afterOrigErr != nil || !bytes.Equal(afterOrig, p.original) {
			if afterOrigErr == nil {
				// Read again after detection so a write that landed between the read and the event
				// poll is part of what is preserved.
				afterOrig, afterOrigErr = readOrig()
			}
			var outcome string
			switch {
			case usedSwap:
				outcome = restoreDisplacedOriginal(tmpPath, targetAbs, postID, openedID)
			case afterOrigErr != nil:
				outcome = fmt.Sprintf("the external bytes could not be read back (%v)", afterOrigErr)
			default:
				// Without an exchange primitive the original name was replaced, so the external bytes
				// survive only in the open descriptor. Never rename over targetAbs: another writer
				// may own it now.
				if savedAt, saveErr := preserveExternalBytes(host, targetAbs, afterOrig, openedID.info.Mode().Perm()); saveErr != nil {
					outcome = fmt.Sprintf("the external bytes could not be preserved (%v)", saveErr)
				} else {
					outcome = "the external bytes were preserved at " + savedAt
				}
			}
			_ = closeOrig()
			p.publishState = PublishExternalModification
			curRev := ""
			if afterOrigErr == nil {
				curRev = RevOf(afterOrig)
			}
			return true, &Error{Code: CodeConcurrentWrite, Path: p.rel, CurrentRev: curRev,
				Detail: "another writer changed the file during publication; " + outcome}
		}
	}

	removeDisplacedOrig()
	if origCloseErr := closeOrig(); origCloseErr != nil {
		p.publishState = PublishUncertain
		return true, origCloseErr
	}
	p.publishState = PublishSucceededAndVerified
	return false, nil
}

// createAtomicNoReplace stages a new file privately and publishes it without replacing any
// existing directory entry. Every create is staged: a host with an AtomicFileStager uses its
// guard, a plain LocalHost uses the local sibling temp, and any other host fails closed as
// unsupported because a direct O_CREATE at the destination would expose an intermediate empty
// name. Binary hosts with a ReplaceFile primitive are handled by replaceBinary.
func (e *Engine) createAtomicNoReplace(host Host, p *prepared) (bool, error) {
	var (
		tmpFile *os.File
		tmpPath string
		err     error
	)
	stager, hasStager := host.(AtomicFileStager)
	switch {
	case hasStager:
		tmpFile, tmpPath, err = stager.OpenCreateTemp(p.abs)
	case isPlainLocalHost(host):
		tmpFile, tmpPath, err = NewCreateTempFile(filepath.Dir(p.abs))
	default:
		p.publishState = PublishUnsupported
		return false, &Error{Code: CodeUnsupportedFilesystem, Path: p.rel,
			Detail: "this file host cannot stage a new file privately; nothing was written"}
	}
	if err != nil {
		p.publishState = PublishFailedBeforeWrite
		if errors.Is(err, errNoReplacePublishUnsupported) {
			return false, &Error{Code: CodeUnsupportedFilesystem, Path: p.rel,
				Detail: err.Error() + "; nothing was written"}
		}
		return false, err
	}
	p.staged = true
	tmpID, idErr := captureFileIdentity(tmpFile)
	if idErr != nil {
		// Without an identity the private name cannot be proven ours. Closing may remove the
		// last reference on some platforms, but no unlink by name is attempted.
		_ = tmpFile.Close()
		p.publishState = PublishUncertain
		return true, idErr
	}

	// Even a short staging write may change the timestamp. Check the actual number of bytes
	// written and the still-open descriptor, never delete a published destination.
	abort := func(cause error, expected []byte) (bool, error) {
		id, err := captureFileIdentity(tmpFile)
		if err != nil || !tmpID.matchesInfo(id.info) {
			p.publishState = PublishUncertain
			return true, errors.Join(cause, err, tmpFile.Close())
		}
		removed, closeErr := removeCreatedWhileOpen(tmpPath, tmpFile, id, expected)
		if removed {
			p.publishState = PublishFailedBeforeWrite
			return false, errors.Join(cause, closeErr)
		}
		p.publishState = PublishUncertain
		return true, errors.Join(cause, closeErr)
	}

	hooks := e.getPublishFaultHooks()
	var (
		n        int
		writeErr error
	)
	if hooks.WriteStaged != nil {
		n, writeErr = hooks.WriteStaged(p.abs, tmpFile, p.next)
	} else {
		n, writeErr = tmpFile.Write(p.next)
	}
	if n < 0 || n > len(p.next) {
		p.publishState = PublishUncertain
		return true, errors.Join(io.ErrShortWrite, writeErr, tmpFile.Close())
	}
	if n != len(p.next) || writeErr != nil {
		if writeErr == nil {
			writeErr = io.ErrShortWrite
		}
		return abort(writeErr, p.next[:n])
	}
	if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
		return abort(err, p.next)
	}
	staged, err := readLimited(tmpFile, MaxFileBytes)
	if err != nil {
		return abort(err, p.next)
	}
	if !bytes.Equal(staged, p.next) {
		return abort(&Error{Code: CodeConcurrentWrite, Path: p.rel, CurrentRev: RevOf(staged),
			Detail: "staged temporary file content changed before publication"}, nil)
	}

	// Narrow the temp-name swap window: the name must still resolve to our inode with the
	// staged size immediately before publication. A same-privilege writer can still race this
	// check; verifyActual observes the published destination afterwards.
	named, err := os.Lstat(tmpPath)
	if err != nil {
		_ = tmpFile.Close()
		if errors.Is(err, os.ErrNotExist) {
			p.publishState = PublishFailedBeforeWrite
			return false, &Error{Code: CodeConflict, Path: p.rel,
				Detail: "the staged temporary file disappeared before publication; nothing was written"}
		}
		p.publishState = PublishUncertain
		return true, err
	}
	if !tmpID.matchesInfo(named) || !os.SameFile(tmpID.info, named) || named.Size() != int64(len(p.next)) {
		// Another writer controls the private name now: do not unlink it and do not publish.
		_ = tmpFile.Close()
		p.publishState = PublishUncertain
		return true, &Error{Code: CodeConcurrentWrite, Path: p.rel,
			Detail: "the staged temporary file was replaced before publication; nothing was written"}
	}

	if hasStager {
		if err := stager.BeforeAtomicPublish(p.abs); err != nil {
			return abort(err, p.next)
		}
	}
	if hooks.BeforePublish != nil {
		if err := hooks.BeforePublish(p.abs); err != nil {
			return abort(err, p.next)
		}
	}
	outcome, pubErr := publishTempNoReplace(tmpPath, tmpFile, p.abs, hooks)
	p.publishState = outcome.state
	p.published = outcome.published
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
				p.publishState = PublishUnsupported
				return outcome.tempLeftover, &Error{Code: CodeUnsupportedFilesystem, Path: p.rel,
					Detail: pubErr.Error() + ", so a new file cannot be published atomically; nothing was written"}
			}
		}
		dirty := outcome.published || outcome.state == PublishUncertain || outcome.tempLeftover
		return dirty, pubErr
	}
	return outcome.tempLeftover, nil
}

// publishBytesNoReplace stages content privately next to dstAbs and publishes it at dstAbs without
// replacing an existing entry. published reports that dstAbs now names the staged bytes, even if a
// post-publication identity or close step failed.
func publishBytesNoReplace(host Host, dstAbs string, content []byte, mode os.FileMode) (bool, error) {
	tmpFile, tmpPath, _, _, err := openPrivateStageTemp(host, dstAbs)
	if err != nil {
		return false, err
	}
	if err := tmpFile.Chmod(mode); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		return false, err
	}
	n, werr := tmpFile.Write(content)
	if werr != nil || n != len(content) {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		if werr != nil {
			return false, werr
		}
		return false, io.ErrShortWrite
	}
	outcome, pubErr := publishTempNoReplace(tmpPath, tmpFile, dstAbs, PublishFaultHooks{})
	return outcome.published, pubErr
}

// preserveExternalBytes persists bytes recovered from a still-open descriptor whose name is gone. It
// publishes them at dstAbs when that name is free and otherwise at a new visible sibling
// "<name>.lc-conflict-<hex>", never replacing an entry another writer created. It returns the path
// that holds the bytes, and reports success only when such a path exists.
func preserveExternalBytes(host Host, dstAbs string, content []byte, mode os.FileMode) (string, error) {
	path := dstAbs
	for range 8 {
		published, err := publishBytesNoReplace(host, path, content, mode)
		if published {
			return path, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
		if path, err = allocateSiblingName(filepath.Dir(dstAbs), filepath.Base(dstAbs)+".lc-conflict-"); err != nil {
			return "", err
		}
	}
	return "", errors.New("could not allocate a free name for the externally modified bytes")
}

// restoreDisplacedOriginal swaps the externally modified original inode (displaced to tmpPath by the
// publication swap) back into targetAbs only while targetAbs still names this commit's staged inode
// and tmpPath still names the original. It never deletes the original or another writer's entry,
// and describes where each ended up.
func restoreDisplacedOriginal(tmpPath, targetAbs string, postID, openedID fileIdentity) string {
	curDst, errDst := os.Lstat(targetAbs)
	curTmp, errTmp := os.Lstat(tmpPath)
	if errTmp != nil || !openedID.matchesInfo(curTmp) {
		return "the externally modified original is no longer at its private name " + tmpPath + " and was not restored"
	}
	if errDst != nil || !postID.matchesInfo(curDst) {
		return "another writer replaced the target, so the externally modified original was kept at " + tmpPath
	}
	if err := renameSwapForPublish(tmpPath, targetAbs); err != nil {
		return fmt.Sprintf("restoring the externally modified original failed (%v); it was kept at %s", err, tmpPath)
	}
	if linfo, lerr := os.Lstat(tmpPath); lerr == nil && postID.matchesInfo(linfo) {
		_ = os.Remove(tmpPath)
		return "the externally modified file was restored"
	}
	return "the externally modified file was restored; an entry another writer placed at the target was moved to " + tmpPath
}

// deleteCAS checks the opened descriptor identity and exact bytes, moves the entry to a private
// sibling quarantine name via no-replace rename, re-verifies identity and content on the quarantined
// entry, and either unlinks it or atomically restores it to p.abs if an external modification raced
// the check-to-delete window (closing EDITING-PRE-02 for deletes).
func (e *Engine) deleteCAS(host Host, p *prepared) (bool, error) {
	if _, _, err := host.Resolve(p.rel, false); err != nil {
		p.publishState = PublishFailedBeforeWrite
		return false, err
	}
	f, err := host.Open(p.abs, false)
	if err != nil {
		p.publishState = PublishFailedBeforeWrite
		if errors.Is(err, os.ErrNotExist) {
			return false, &Error{Code: CodeConflict, Path: p.rel, Rev: p.rev,
				Detail: "delete target was removed after validation; nothing was removed"}
		}
		return false, err
	}
	fClosed := false
	closeF := func() error {
		if fClosed || f == nil {
			return nil
		}
		fClosed = true
		return f.Close()
	}

	openedID, idErr := captureFileIdentity(f)
	if idErr != nil {
		p.publishState = PublishFailedBeforeWrite
		return false, errors.Join(idErr, closeF())
	}
	if p.writtenID.valid && !p.writtenID.matchesInfo(openedID.info) {
		p.publishState = PublishFailedBeforeWrite
		return false, errors.Join(&Error{Code: CodeConflict, Path: p.rel,
			Detail: "delete target identity changed after validation; nothing was removed"}, closeF())
	}

	watch, _ := newVnodeWriteWatch(f)
	defer func() { _ = watch.Close() }()

	readOpened := func() ([]byte, error) {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		return readLimited(f, MaxFileBytes)
	}

	current, err := readOpened()
	if err != nil {
		p.publishState = PublishFailedBeforeWrite
		if CodeOf(err) == CodeTooLarge {
			return false, errors.Join(&Error{Code: CodeConcurrentWrite, Path: p.rel, Rev: p.rev,
				Detail: "delete target grew after validation; nothing was removed"}, closeF())
		}
		return false, errors.Join(err, closeF())
	}
	if !bytes.Equal(current, p.original) {
		p.publishState = PublishFailedBeforeWrite
		return false, errors.Join(&Error{Code: CodeConflict, Path: p.rel, Rev: p.rev, CurrentRev: RevOf(current),
			Detail: "delete target changed after validation; nothing was removed"}, closeF())
	}
	named, err := os.Lstat(p.abs)
	if err != nil {
		p.publishState = PublishFailedBeforeWrite
		if errors.Is(err, os.ErrNotExist) {
			return false, errors.Join(&Error{Code: CodeConflict, Path: p.rel,
				Detail: "delete target was removed after validation; nothing was removed"}, closeF())
		}
		return false, errors.Join(err, closeF())
	}
	if !named.Mode().IsRegular() || !os.SameFile(openedID.info, named) ||
		!p.writtenID.matchesInfo(named) || named.Size() != int64(len(p.original)) {
		p.publishState = PublishFailedBeforeWrite
		return false, errors.Join(&Error{Code: CodeConflict, Path: p.rel,
			Detail: "delete target was replaced after validation; nothing was removed"}, closeF())
	}

	if stager, hasStager := host.(AtomicFileStager); hasStager {
		if err := stager.BeforeAtomicPublish(p.abs); err != nil {
			p.publishState = PublishFailedBeforeWrite
			return false, errors.Join(err, closeF())
		}
	}
	hooks := e.getPublishFaultHooks()
	if hooks.BeforeDeleteUnlink != nil {
		if err := hooks.BeforeDeleteUnlink(p.abs); err != nil {
			p.publishState = PublishFailedBeforeWrite
			return false, errors.Join(err, closeF())
		}
	}

	var quarantinePath string
	var moveErr error
	for attempt := 0; attempt < 8; attempt++ {
		quarantinePath, err = allocateSiblingName(filepath.Dir(p.abs), ".lcx-del-")
		if err != nil {
			p.publishState = PublishFailedBeforeWrite
			return false, errors.Join(err, closeF())
		}
		var closedByPlatform bool
		closedByPlatform, moveErr = quarantineMoveForDelete(p.abs, quarantinePath, f)
		if closedByPlatform {
			fClosed = true
		}
		if errors.Is(moveErr, os.ErrExist) && !closedByPlatform {
			continue
		}
		break
	}

	if moveErr != nil && isQuarantineUnsupportedErr(moveErr) {
		// Without a no-replace move the target could only be removed by name after a separate check,
		// which loses a write landing in between. Refuse rather than delete unsafely.
		_ = closeF()
		p.publishState = PublishUnsupported
		return false, &Error{Code: CodeUnsupportedFilesystem, Path: p.rel,
			Detail: "the filesystem cannot move the file aside without replacing another entry (" + moveErr.Error() + "), so it cannot be deleted safely; nothing was removed"}
	}

	if moveErr != nil {
		_ = closeF()
		if errors.Is(moveErr, os.ErrNotExist) {
			p.publishState = PublishFailedBeforeWrite
			return false, &Error{Code: CodeConflict, Path: p.rel,
				Detail: "delete target was removed by another writer; nothing was removed"}
		}
		if errors.Is(moveErr, os.ErrPermission) {
			p.publishState = PublishFailedBeforeWrite
			return false, moveErr
		}
		p.publishState = PublishUncertain
		return true, moveErr
	}

	qNamed, lerr := os.Lstat(quarantinePath)
	var (
		qBytes []byte
		qErr   error
		rErr   error
	)
	if qf, openErr := os.Open(quarantinePath); openErr != nil {
		qErr = openErr
	} else {
		qBytes, rErr = readLimited(qf, MaxFileBytes)
		qErr = qf.Close()
	}

	unchanged := lerr == nil &&
		qNamed.Mode().IsRegular() &&
		os.SameFile(openedID.info, qNamed) &&
		openedID.matchesInfo(qNamed) &&
		p.writtenID.matchesInfo(qNamed) &&
		qNamed.Size() == int64(len(p.original)) &&
		!watch.wrote() &&
		qErr == nil &&
		rErr == nil &&
		bytes.Equal(qBytes, p.original)

	if !unchanged {
		_ = closeF()
		restoreErr := quarantineRestoreForDelete(quarantinePath, p.abs)
		curRev := ""
		code := CodeConflict
		if CodeOf(rErr) == CodeTooLarge {
			code = CodeConcurrentWrite
		} else if qErr == nil && rErr == nil {
			curRev = RevOf(qBytes)
		}
		conflictErr := &Error{Code: code, Path: p.rel, Rev: p.rev, CurrentRev: curRev,
			Detail: "delete target changed after validation; nothing was removed"}
		if restoreErr != nil {
			p.publishState = PublishUncertain
			return true, errors.Join(conflictErr, restoreErr,
				fmt.Errorf("the changed delete target could not be moved back and was kept at %s", quarantinePath))
		}
		p.publishState = PublishFailedBeforeWrite
		return false, conflictErr
	}

	// A path recreated by another writer after the move aside makes the delete outcome a conflict:
	// keep the original in quarantine and leave the new file alone rather than reporting a delete.
	if _, recreatedErr := os.Lstat(p.abs); !errors.Is(recreatedErr, os.ErrNotExist) {
		_ = closeF()
		if recreatedErr != nil {
			p.publishState = PublishUncertain
			return true, errors.Join(recreatedErr, fmt.Errorf("the delete target was kept at %s", quarantinePath))
		}
		p.publishState = PublishExternalModification
		return true, &Error{Code: CodeConflict, Path: p.rel, Rev: p.rev,
			Detail: "another writer recreated the path while it was being deleted; the original file was kept at " + quarantinePath}
	}

	if rmErr := os.Remove(quarantinePath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
		_ = closeF()
		p.publishState = PublishUncertain
		return true, rmErr
	}

	if !fClosed {
		if hooks.BeforeFinalExternalCheck != nil {
			hooks.BeforeFinalExternalCheck(p.abs)
		}
		postBytes, postReadErr := readOpened()
		if hooks.AfterFinalExternalRead != nil {
			hooks.AfterFinalExternalRead(p.abs)
		}
		wroteDuringUnlink := watch.wrote() || postReadErr != nil || !bytes.Equal(postBytes, p.original)
		if wroteDuringUnlink && postReadErr == nil {
			// Read again after detection so a write that landed between the read and the event poll
			// is part of what is preserved.
			postBytes, postReadErr = readOpened()
		}
		closeErr := closeF()
		if wroteDuringUnlink {
			if postReadErr != nil {
				p.publishState = PublishUncertain
				return true, errors.Join(&Error{Code: CodeConcurrentWrite, Path: p.rel,
					Detail: "delete target changed during removal and its bytes could not be read back"}, postReadErr, closeErr)
			}
			curRev := RevOf(postBytes)
			savedAt, saveErr := preserveExternalBytes(host, p.abs, postBytes, openedID.info.Mode().Perm())
			if saveErr != nil {
				p.publishState = PublishUncertain
				return true, errors.Join(&Error{Code: CodeConcurrentWrite, Path: p.rel, CurrentRev: curRev,
					Detail: "delete target changed during removal and the external bytes could not be preserved"}, saveErr, closeErr)
			}
			p.publishState = PublishExternalModification
			detail := "delete target changed during removal; external modification restored"
			if savedAt != p.abs {
				detail = "delete target changed during removal and the path was recreated by another writer; the external bytes were preserved at " + savedAt
			}
			return true, errors.Join(&Error{Code: CodeConcurrentWrite, Path: p.rel, CurrentRev: curRev, Detail: detail}, closeErr)
		}
		if closeErr != nil {
			p.publishState = PublishUncertain
			return true, closeErr
		}
	}

	p.published = true
	p.wrote = true
	p.publishState = PublishSucceededAndVerified
	return false, nil
}

// verifyActual observes the named destination read-only and returns nil only when it holds the
// exact planned bytes and the expected identity: a live regular file for a create/update, or
// absence for a delete. An observed external change is a CodeConflict (path swapped, removed,
// recreated or wrong kind) or CodeConcurrentWrite (same inode with different bytes). An
// inability to inspect is returned as the underlying I/O error and leaves the outcome uncertain.
// It never writes, unlinks or repairs a destination.
func (e *Engine) verifyActual(host Host, p *prepared) error {
	if p.deleted {
		// Absence cannot be observed through an open descriptor; still run the host's path
		// guard on the spelling (resolving the existing part) before the no-follow check.
		if _, _, err := host.Resolve(p.rel, true); err != nil {
			p.publishState = PublishUncertain
			return err
		}
		_, err := os.Lstat(p.abs)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			p.publishState = PublishUncertain
			return err
		}
		p.publishState = PublishExternalModification
		return &Error{Code: CodeConflict, Path: p.rel,
			Detail: "delete target exists after the removal was published"}
	}

	f, err := host.Open(p.abs, false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			p.publishState = PublishExternalModification
			return &Error{Code: CodeConflict, Path: p.rel, Rev: p.rev,
				Detail: "the published target was removed or replaced by another writer"}
		}
		p.publishState = PublishUncertain
		return err
	}
	openedID, idErr := captureFileIdentity(f)
	if idErr != nil {
		_ = f.Close()
		p.publishState = PublishUncertain
		return idErr
	}
	current, readErr := readLimited(f, MaxFileBytes)
	closeErr := f.Close()
	if readErr != nil {
		p.publishState = PublishUncertain
		if CodeOf(readErr) == CodeTooLarge {
			p.publishState = PublishExternalModification
			return &Error{Code: CodeConcurrentWrite, Path: p.rel,
				Detail: "the published target grew beyond the edit limit after publication"}
		}
		return errors.Join(readErr, closeErr)
	}
	if closeErr != nil {
		p.publishState = PublishUncertain
		return closeErr
	}
	checkAbs := p.abs
	if p.viaSymlink && !p.created {
		linkInfo, linkErr := os.Lstat(p.abs)
		if linkErr != nil {
			if errors.Is(linkErr, os.ErrNotExist) {
				p.publishState = PublishExternalModification
				return &Error{Code: CodeConflict, Path: p.rel,
					Detail: "the published target was removed after verification"}
			}
			p.publishState = PublishUncertain
			return linkErr
		}
		if linkInfo.Mode()&os.ModeSymlink == 0 {
			p.publishState = PublishExternalModification
			return &Error{Code: CodeConflict, Path: p.rel,
				Detail: "the published symlink was replaced after verification"}
		}
		evalPath, evalErr := filepath.EvalSymlinks(p.abs)
		if evalErr != nil {
			p.publishState = PublishExternalModification
			return &Error{Code: CodeConflict, Path: p.rel,
				Detail: "the published symlink target could not be resolved after verification"}
		}
		resolvedAbs, _, resErr := host.Resolve(evalPath, false)
		if resErr != nil {
			p.publishState = PublishExternalModification
			return resErr
		}
		checkAbs = resolvedAbs
	}
	named, statErr := os.Lstat(checkAbs)
	if statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			p.publishState = PublishExternalModification
			return &Error{Code: CodeConflict, Path: p.rel,
				Detail: "the published target was removed after verification"}
		}
		p.publishState = PublishUncertain
		return statErr
	}
	if !named.Mode().IsRegular() {
		p.publishState = PublishExternalModification
		return &Error{Code: CodeConflict, Path: p.rel,
			Detail: "the published target is no longer a regular file"}
	}
	expected := p.writtenID
	if p.created && p.createdID.valid {
		expected = p.createdID
	}
	if !os.SameFile(openedID.info, named) ||
		(expected.valid && (!expected.matchesInfo(openedID.info) || !expected.matchesInfo(named))) {
		p.publishState = PublishExternalModification
		return &Error{Code: CodeConflict, Path: p.rel, Rev: p.rev,
			Detail: "the published target identity changed after verification"}
	}
	if !bytes.Equal(current, p.next) {
		p.publishState = PublishExternalModification
		return &Error{Code: CodeConcurrentWrite, Path: p.rel, CurrentRev: RevOf(current),
			Detail: "the published target bytes changed after verification"}
	}
	return nil
}
