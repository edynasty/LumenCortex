package agent

import (
	"testing"

	"github.com/edynasty/LumenCortex/internal/editing"
	"github.com/edynasty/LumenCortex/internal/repository"
	"github.com/edynasty/LumenCortex/internal/session"
)

// TestR7R2FinalCreateRaceReceiptsSealUnknown pins the MutationEvidence side of R7R2-NEW-02. Each
// receipt below is one a write_file commit returns when an external process creates the target
// first (internal/editing TestR201TwoOSProcessesCreateRace asserts these shapes on real commits).
// None of them is Committed, so the seal is MutationUnknown even when the execution completed with
// full coverage and a clean pre/post sandwich, whether or not the file changed underneath.
func TestR7R2FinalCreateRaceReceiptsSealUnknown(t *testing.T) {
	const (
		emptySHA = "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
		agentSHA = "sha256:83437998e1744f987fc14c5b3ec919f388e9f2af96b202d9eb434c0143aeb69f"
	)
	x := &execution{
		preRef:  "files:1:sha256:1111111111111111111111111111111111111111111111111111111111111111",
		targets: []string{"shared.txt"},
		files: repository.FilesSnapshot{
			Complete: true,
			Files:    []repository.FileState{{Path: "shared.txt", Blob: repository.BlobAbsent}},
		},
	}
	changed := repository.FilesSnapshot{Complete: true, Files: []repository.FileState{{Path: "shared.txt", Blob: "blob-after"}}}
	seal := func(after repository.FilesSnapshot, receipt editing.CommitReceipt) string {
		return sealFilesMutation(x, session.OutcomeComplete, session.CoverageComplete, after, after.Files, receipt, true, true)
	}
	row := func(f editing.FileCommitReceipt) []editing.FileCommitReceipt {
		f.Path = "shared.txt"
		return []editing.FileCommitReceipt{f}
	}
	for _, tc := range []struct {
		name    string
		receipt editing.CommitReceipt
	}{
		{"exists_before_prepare", editing.CommitReceipt{State: editing.CommitStatePrePublishFailure,
			Files: row(editing.FileCommitReceipt{PublishState: editing.PublishFailedBeforeWrite})}},
		{"staged_publish_refused", editing.CommitReceipt{State: editing.CommitStatePrePublishFailure,
			Files: row(editing.FileCommitReceipt{Staged: true, PublishState: editing.PublishExternalModification, OldSHA256: "-"})}},
		{"inplace_refused_before_truncate", editing.CommitReceipt{State: editing.CommitStatePrePublishFailure,
			Files: row(editing.FileCommitReceipt{PublishState: editing.PublishExternalModification, OldSHA256: emptySHA})}},
		// The same receipt whether the foreign bytes are whole or, in the PRE-02 window, mixed.
		{"inplace_overlap_reported_dirty", editing.CommitReceipt{DirtyFailure: true, State: editing.CommitStatePublishedWithPostError,
			Files: row(editing.FileCommitReceipt{Published: true, Wrote: true, PublishState: editing.PublishExternalModification, OldSHA256: emptySHA})}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for label, after := range map[string]repository.FilesSnapshot{"file_changed": changed, "file_unchanged": x.files} {
				if got := seal(after, tc.receipt); got != string(MutationUnknown) {
					t.Fatalf("%s: sealed %q, want %q", label, got, MutationUnknown)
				}
			}
		})
	}
	// The control: the same execution with LC's verified create is proven, so the cases above are
	// decided by their receipts and not by the fixture.
	created := editing.CommitReceipt{Committed: true, State: editing.CommitStateCommitted,
		Files: row(editing.FileCommitReceipt{Staged: true, Published: true, Verified: true, Wrote: true, Created: true,
			PublishState: editing.PublishSucceededAndVerified, OldSHA256: "-", NewSHA256: agentSHA})}
	if got := seal(changed, created); got != string(MutationProven) {
		t.Fatalf("verified create sealed %q, want %q", got, MutationProven)
	}
}
