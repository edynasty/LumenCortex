package editing

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// A binary artifact host uses descriptor-relative replacement. Ordinary existing files keep
// their inode; new files require staged no-replace publication. Neither is a multi-file CAS.
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
	return e.writeInPlace(host, p)
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

// writeInPlace updates an existing regular file on its guarded descriptor. The opened descriptor
// identity is compared with the identity captured at prepare, so an external same-byte inode
// replacement is refused instead of truncating a file this commit did not plan. Bytes, not a
// short revision hash, decide the compare-and-swap.
func (e *Engine) writeInPlace(host Host, p *prepared) (bool, error) {
	f, err := host.Open(p.abs, false)
	if err != nil {
		p.publishState = PublishFailedBeforeWrite
		if errors.Is(err, os.ErrNotExist) {
			return false, &Error{Code: CodeConflict, Path: p.rel, Rev: p.rev,
				Detail: "the file was removed after the change was computed; nothing was written"}
		}
		return false, err
	}
	openedID, idErr := captureFileIdentity(f)
	if idErr != nil {
		p.publishState = PublishFailedBeforeWrite
		return false, errors.Join(idErr, f.Close())
	}
	if p.writtenID.valid && !p.writtenID.matchesInfo(openedID.info) {
		p.publishState = PublishFailedBeforeWrite
		return false, errors.Join(&Error{Code: CodeConflict, Path: p.rel, Rev: p.rev,
			Detail: "the file was replaced after the change was computed; nothing was written"}, f.Close())
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		p.publishState = PublishFailedBeforeWrite
		return false, errors.Join(err, f.Close())
	}
	current, err := readLimited(f, MaxFileBytes)
	if err != nil {
		p.publishState = PublishFailedBeforeWrite
		if CodeOf(err) == CodeTooLarge {
			return false, errors.Join(&Error{Code: CodeConcurrentWrite, Path: p.rel, Rev: p.rev,
				Detail: "the file grew beyond the edit limit after the change was computed; nothing was written"}, f.Close())
		}
		return false, errors.Join(err, f.Close())
	}
	if !bytes.Equal(current, p.original) {
		p.publishState = PublishFailedBeforeWrite
		return false, errors.Join(&Error{Code: CodeConflict, Path: p.rel, Rev: p.rev, CurrentRev: RevOf(current),
			Detail: "the file changed after the change was computed; nothing was written"}, f.Close())
	}

	// A successful truncate is the first point where the destination is known to have changed.
	if err := f.Truncate(0); err != nil {
		p.publishState = PublishFailedBeforeWrite
		return false, errors.Join(err, f.Close())
	}
	p.published = true
	p.wrote = true
	p.publishState = PublishPartial
	partialFail := func(cause error) (bool, error) {
		p.publishState = PublishPartial
		return true, errors.Join(cause, f.Close())
	}
	uncertainFail := func(cause error) (bool, error) {
		p.publishState = PublishUncertain
		return true, errors.Join(cause, f.Close())
	}
	if e.afterTruncate != nil {
		if err := e.afterTruncate(p.abs); err != nil {
			return partialFail(err)
		}
	}
	hooks := e.getPublishFaultHooks()
	if hooks.AfterTruncate != nil {
		if err := hooks.AfterTruncate(p.abs); err != nil {
			return partialFail(err)
		}
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return partialFail(err)
	}
	n, writeErr := f.Write(p.next)
	if writeErr != nil {
		return partialFail(writeErr)
	}
	if n != len(p.next) {
		return partialFail(io.ErrShortWrite)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return uncertainFail(err)
	}
	after, err := readLimited(f, MaxFileBytes)
	if err != nil {
		return uncertainFail(err)
	}
	if !bytes.Equal(after, p.next) {
		p.publishState = PublishExternalModification
		return true, errors.Join(&Error{Code: CodeConcurrentWrite, Path: p.rel, CurrentRev: RevOf(after),
			Detail: "another writer changed the file during the write"}, f.Close())
	}
	if postID, idErr := captureFileIdentity(f); idErr == nil {
		if p.created {
			p.createdID = postID
		} else {
			p.writtenID = postID
		}
	} else {
		return uncertainFail(idErr)
	}
	closeErr := f.Close()
	if hooks.AfterInPlaceWriteClose != nil {
		closeErr = errors.Join(closeErr, hooks.AfterInPlaceWriteClose(p.abs))
	}
	if closeErr != nil {
		// Bytes were read back equal, but the close is not verified: the outcome is uncertain.
		p.publishState = PublishUncertain
		return true, closeErr
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

// deleteCAS checks the opened descriptor identity and exact bytes, then removes the name. The
// check-to-unlink interval remains a PRE-02 window against an uncooperative writer; no
// compensation or re-creation happens after this returns. A removal error that cannot prove the
// name is still present is reported as uncertain, not as a clean refusal.
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
	openedID, idErr := captureFileIdentity(f)
	if idErr != nil {
		p.publishState = PublishFailedBeforeWrite
		return false, errors.Join(idErr, f.Close())
	}
	if p.writtenID.valid && !p.writtenID.matchesInfo(openedID.info) {
		p.publishState = PublishFailedBeforeWrite
		return false, errors.Join(&Error{Code: CodeConflict, Path: p.rel,
			Detail: "delete target identity changed after validation; nothing was removed"}, f.Close())
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		p.publishState = PublishFailedBeforeWrite
		return false, errors.Join(err, f.Close())
	}
	current, err := readLimited(f, MaxFileBytes)
	if err != nil {
		p.publishState = PublishFailedBeforeWrite
		if CodeOf(err) == CodeTooLarge {
			return false, errors.Join(&Error{Code: CodeConcurrentWrite, Path: p.rel, Rev: p.rev,
				Detail: "delete target grew after validation; nothing was removed"}, f.Close())
		}
		return false, errors.Join(err, f.Close())
	}
	if !bytes.Equal(current, p.original) {
		p.publishState = PublishFailedBeforeWrite
		return false, errors.Join(&Error{Code: CodeConflict, Path: p.rel, Rev: p.rev, CurrentRev: RevOf(current),
			Detail: "delete target changed after validation; nothing was removed"}, f.Close())
	}
	named, err := os.Lstat(p.abs)
	if err != nil {
		p.publishState = PublishFailedBeforeWrite
		if errors.Is(err, os.ErrNotExist) {
			return false, errors.Join(&Error{Code: CodeConflict, Path: p.rel,
				Detail: "delete target was removed after validation; nothing was removed"}, f.Close())
		}
		return false, errors.Join(err, f.Close())
	}
	if !named.Mode().IsRegular() || !os.SameFile(openedID.info, named) ||
		!p.writtenID.matchesInfo(named) || named.Size() != int64(len(p.original)) {
		p.publishState = PublishFailedBeforeWrite
		return false, errors.Join(&Error{Code: CodeConflict, Path: p.rel,
			Detail: "delete target was replaced after validation; nothing was removed"}, f.Close())
	}
	// Close before unlink so Windows can delete the name and the verified bytes are final.
	if err := f.Close(); err != nil {
		p.publishState = PublishFailedBeforeWrite
		return false, err
	}
	if err := os.Remove(p.abs); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			p.publishState = PublishFailedBeforeWrite
			return false, &Error{Code: CodeConflict, Path: p.rel,
				Detail: "delete target was removed by another writer; nothing was removed"}
		}
		if errors.Is(err, os.ErrPermission) {
			p.publishState = PublishFailedBeforeWrite
			return false, err
		}
		p.publishState = PublishUncertain
		return true, err
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
	named, statErr := os.Lstat(p.abs)
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
