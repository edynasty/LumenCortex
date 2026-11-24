# INT-PRE-REG-01 Remediation Evidence Pack

- **Status:** `INT_PRE_REG_01_FIXED_BY_IMPLEMENTER / SELF_VERIFIED / PENDING_INDEPENDENT_REVIEW`
- **Baseline (`main` / `origin/main`):** `ef10ca7c8e8031e75fdf27f449924b8254f0e2bb`
- **Remediation Branch:** `fix/runtime-int-pre-reg-01-carbon-remediation-20261124`
- **Code Remediation Commit:** `316e99b90fad5d6e22b6f9a9b01f240be2b55370` (`runtime/context_space_t3_test.go` only; `421 insertions(+), 25 deletions(-)`)
- **Production Code Changes:** None (`0` production files modified)

## 1. Root Cause Summary

1. `runtime/context_space_t3_test.go:512` (`T6-3`) previously injected `editing.PublishFaultHooks{AfterTruncate: ...}` on `single.txt` and asserted `OutcomeUnknown`, assuming a pre-PRE failure after `f.Truncate(0)` had mutated the live target file.
2. Under the merged PRE-01/PRE-02 atomic staging architecture (`internal/editing/publish_file.go:375-377`), `AfterTruncate` fires while writing to the sibling private staging file `.lcx-create-*` **before** `renameSwapForPublish`. When staging fails, `abortBeforeSwap` cleanly unlinks `.lcx-create-*`, leaving the original file byte-for-byte and inode-wise untouched and returning `PublishFailedBeforeWrite` / `CommitStatePrePublishFailure` with `DirtyFailure=false`.
3. `internal/agent/execution.go:834` accurately classifies clean pre-publication failures as `session.OutcomeFailed` + `MutationUnknown` (`;cm=unknown`, `FileEdgeCount=0`, unchanged `PreRef == stripCMSeal(PostRef)`) while locking `MinRequiredSeq` at `g0` — without treating an untouched pre-swap abort as a dirty file corruption.

## 2. Remediated Test Contract (`runtime/context_space_t3_test.go:556-923`)

| Subcase | Scenario | Verified Assertions |
|---|---|---|
| `T6-1` (`L557-593`) | Multi-file Partial Commit First (`p1.txt` verified published, `p2.txt` fails `AfterTruncate` pre-swap) | `MutationUnknown`, `OutcomeUnknown`, `;cm=unknown`, locked `MinRequiredSeq=g01`; `p1.txt` == `"new1\n"`, `p2.txt` == `"old2\n"`, no staging residue; payload `CommitStatePartiallyPublished` / `DirtyFailure=true` / `PartialPublication=true`, `p1.txt` verified (`PublishSucceededAndVerified`), `p2.txt` clean (`PublishFailedBeforeWrite`, `Verified=false`, `NewRev=""`, `SHA256=""`). |
| `T6-2` (`L595-617`) | Multi-file external writer overwrites `p1.txt` before `p2.txt` fails | `MutationUnknown`, `OutcomeUnknown`, locked `MinRequiredSeq=g02`; external bytes `"foreign\n"` preserved; no staging residue. |
| `T6-3a` (`L619-723`) | Single-file pre-swap `AfterTruncate` failure with clean `.lcx-create-*` cleanup (`INT-PRE-REG-01`) | Byte equality `"orig\n"` + `os.SameFile`; zero `edit.committed` events; `FileRevision == RevOf("orig\n")`; `Diff` -> `CodeUnknownRev`; `Actor.LatestRev == ""`; zero staging residue; `OutcomeFailed`, `CoverageComplete`, `FileEdgeCount=0`, `PreRef == stripCMSeal(PostRef)`, `;cm=unknown`, `MutationUnknown`; payload `CommitStatePrePublishFailure` / `DirtyFailure=false` / `PartialPublication=false`, row `PublishFailedBeforeWrite` with `Published=false`, `Verified=false`, `Wrote=false`, `NewRev=""`, `SHA256=""`; Watermark locked at `g03a` across Block2 (`BlockCount=2`), then advances to `repBlock2.BlockSeq > g03a` upon valid repair. |
| `T6-3b` (`L725-778`) | Single-file pre-swap `AfterTruncate` failure with tampered temp preventing clean unlink | Original bytes `"orig-stage\n"` + `os.SameFile`; 1 leftover staging file confirmed then test-cleaned; `OutcomeUnknown`, `MutationUnknown`, locked `MinRequiredSeq=g03b`; payload `CommitStatePublicationUncertain` / `DirtyFailure=true` / non-empty `Recovery`, row `PublishUncertain` with empty `NewRev`/`SHA256`. |
| `T6-3c` (`L780-812`) | Single-file post-swap `AfterInPlaceWriteClose` failure | Displaced original `"orig-swap\n"` preserved at temp path; `OutcomeUnknown`, `MutationUnknown`, locked `MinRequiredSeq=g03c`; payload `CommitStatePartiallyPublished` / `DirtyFailure=true` / `PartialPublication=true` / non-empty `Recovery`, row `PublishUncertain` with `Published=true`, `Wrote=true`, `Verified=false`, empty `NewRev`/`SHA256`. |
| `T6-3d` (`L814-906`) | Cold SQLite Resume (`eng1 -> eng2` clean failure + follow-up repair; `eng3 -> eng4` post-swap uncertain) | `eng2` `ListAnsweredExecutions`, `DeriveRepairContext`, and `StreamRepairContextFromStore` preserve `OutcomeFailed`, `FileEdgeCount=0`, `PreRef+";cm=unknown"`, `MutationUnknown`, locked `MinRequiredSeq=g0Clean`, `"orig-resume\n"`, `CommitStatePrePublishFailure`, and allow subsequent valid repair to advance `MinRequiredSeq`; `eng4` preserves `OutcomeUnknown`, `MutationUnknown`, locked `MinRequiredSeq=g0Unc`, and `CommitStatePartiallyPublished`/`PublishUncertain`. |
| `T6-4a`, `T6-4b` (`L908-922`) | Pre-write validation failures (`replace_in_file` missing `old_text`, `write_file` `../escape.txt`) | Original bytes preserved (`"hello\n"`), locked `MinRequiredSeq`, `MutationUnknown`, `OutcomeFailed` (`T6-4a`) and `OutcomeUnknown` with `PreUnavailable=true` (`T6-4b`). |

## 3. Verification Matrix (`GOFLAGS` unset)

| Check | Command | Exit | Result | Log File |
|---|---|---|---|---|
| Baseline RED | `go test -count=1 -v ./runtime -run '^TestT3B_CausalAuthorshipConcurrencyAndIgnoredFilesMatrix/RC34_F02_T6_dirty_and_clean_write_failures$'` | `1` | Fails at `context_space_t3_test.go:512` (`Outcome:failed`, `1.660s`) | `red-baseline.txt` |
| Formatting | `gofmt -l runtime/context_space_t3_test.go` | `0` | Empty output (`0` bytes) | `gofmt.txt` |
| Focused T6 GREEN | `go test -count=1 -v ./runtime -run '^TestT3B_CausalAuthorshipConcurrencyAndIgnoredFilesMatrix/RC34_F02_T6_dirty_and_clean_write_failures$'` | `0` | `PASS` (`2.598s`, subtest `1.45s`) | `green-t6.txt` |
| Full T3B Matrix GREEN | `go test -count=1 -v ./runtime -run '^TestT3B_CausalAuthorshipConcurrencyAndIgnoredFilesMatrix'` | `0` | `PASS` (`9/9` subtests, `4.527s`) | `green-t3b.txt` |
| T3B Race GREEN | `go test -race -count=1 ./runtime -run '^TestT3B_CausalAuthorshipConcurrencyAndIgnoredFilesMatrix'` | `0` | `PASS` (`8.454s`) | `green-t3b-race.txt` |
| Overlay Mutants | 7 production mutants via `go test -overlay` on T6 | `1` (7/7) | `7/7` killed (see `mutants-summary.txt` & `mutants-raw.txt`) | `mutants-summary.txt`, `mutants-raw.txt` |

Note: Per Section 9 of the remediation charter, the complete Core suite (`go test ./...`) was NOT executed on this remediation branch and remains for post-review acceptance. Checksums for all evidence files in this directory are recorded in `SHA256SUMS`.
