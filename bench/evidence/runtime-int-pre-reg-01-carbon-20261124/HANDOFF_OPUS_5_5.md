# Independent Review Handoff — Claude Opus 5.5 (High)

- **Finding:** `INT-PRE-REG-01` (`P1 — Integration Acceptance Blocker`)
- **Authoritative Baseline (`main` / `origin/main`):** `ef10ca7c8e8031e75fdf27f449924b8254f0e2bb`
- **Remediation Branch:** `fix/runtime-int-pre-reg-01-carbon-remediation-20261124`
- **Code Commit:** `316e99b90fad5d6e22b6f9a9b01f240be2b55370` (`runtime/context_space_t3_test.go` only)
- **Implementer Status:** `INT_PRE_REG_01_FIXED_BY_IMPLEMENTER / SELF_VERIFIED / PENDING_INDEPENDENT_REVIEW`
- **Scope Note:** Focused runtime contract verification only — do NOT re-open a full PRE-01/PRE-02 architecture audit.

## Point-by-Point Verification Checklist (9 Required Items)

1. **Original T6 expectation was obsolete**
   - `runtime/context_space_t3_test.go:512` (`T6-3` on `ef10ca7`) expected `OutcomeUnknown` for an `AfterTruncate` fault on `single.txt`, assuming PRE-PRE in-place truncation.
   - Under PRE-01/PRE-02 (`internal/editing/publish_file.go:375-377` and `abortBeforeSwap`), `AfterTruncate` occurs on `.lcx-create-*` before `renameSwapForPublish`; clean unlink returns `PublishFailedBeforeWrite` + `CommitStatePrePublishFailure` (`DirtyFailure=false`).

2. **Corrected result matches actual publication state**
   - `T6-3a` (`runtime/context_space_t3_test.go:669-685`): clean pre-swap failure asserts `session.OutcomeFailed`, `CoverageComplete`, `FileEdgeCount=0`, `agent.MutationUnknown`, `;cm=unknown`, unchanged `PreRef == stripCMSeal(PostRef)`, `CommitStatePrePublishFailure`, `DirtyFailure=false`, `PublishFailedBeforeWrite`.
   - `T6-3b` (`L725-778`): tampered `.lcx-create-*` preventing safe unlink asserts `CommitStatePublicationUncertain`, `DirtyFailure=true`, `PublishUncertain`, `OutcomeUnknown`.
   - `T6-3c` (`L780-812`): `AfterInPlaceWriteClose` post-swap fault asserts `CommitStatePartiallyPublished`, `DirtyFailure=true`, `PartialPublication=true`, `PublishUncertain`, `OutcomeUnknown`.

3. **Original bytes and identity are preserved**
   - Checked via full `os.ReadFile` byte equality and `os.Lstat` + `os.SameFile`: `T6-1` (`p2.txt == "old2\n"`), `T6-2` (`p1.txt == "foreign\n"`), `T6-3a` (`single.txt == "orig\n"` + `SameFile`), `T6-3b` (`single_dirty_stage.txt == "orig-stage\n"` + `SameFile`), `T6-3c` (displaced original `"orig-swap\n"` at temp path), `T6-3d` (`resume_clean.txt == "orig-resume\n"`), `T6-4a` (`pre.txt == "hello\n"`).
   - Killed by overlay mutant `m5_original_modified`.

4. **No publication or trusted revision is fabricated**
   - `T6-3a` (`L651-664`, `L682-685`): asserts zero `edit.committed` events via `drainCommittedEvents`, `FileRevision == RevOf("orig\n")`, `Diff(..., RevOf("orig\n"), RevOf("next\n"))` returns `CodeUnknownRev`, `Actor.LatestRev == ""`, and file row `Published=false`, `Verified=false`, `Wrote=false`, `NewRev=""`, `SHA256=""`.
   - Killed by overlay mutants `m1_false_success` and `m6a_incorrect_receipt_verified_and_rev`. (Note: an initial mutant touching only `engine.go` `fileOutcome` was independently blocked by `receiptFileState`'s defense-in-depth `f.Verified` guard in `internal/toolset/editing.go`; `m6a` mutates `p.verified = true` in `internal/editing/engine.go:948` to exercise end-to-end receipt fabrication and dies at `L592`.)

5. **Repair Watermark semantics are correct**
   - `T6-3a` (`L688-723`), `T6-3b` (`L768-771`), `T6-3c` (`L802-805`), `T6-3d` (`L840-870`, `L894-899`): verify `MinRequiredSeq` remains locked at `g0`, does NOT advance across a second premature proposal (`BlockCount == 2`, `MinRequiredSeq == g03a`), and advances cleanly to `repBlock2.BlockSeq > g03a` only after a valid `write_file` repair succeeds.
   - Killed by overlay mutant `m4_false_watermark_advance`.

6. **Dirty and clean failure cases remain distinct**
   - Covered distinctly across `T6-1` (multi-file partial commit: `p1.txt` verified published, `p2.txt` clean pre-swap failure), `T6-2` (external writer + pre-swap failure), `T6-3a` (clean staging failure -> `OutcomeFailed` + `DirtyFailure=false`), `T6-3b` (refused temp cleanup -> `OutcomeUnknown` + `DirtyFailure=true` + `CommitStatePublicationUncertain`), and `T6-3c` (post-swap close fault -> `OutcomeUnknown` + `DirtyFailure=true` + `CommitStatePartiallyPublished`).
   - Killed by `m2_false_unknown_for_clean` and `m3_false_failed_for_uncertain`.

7. **Receipt and Resume remain correct**
   - Persisted tool payloads verified via `findToolResultJSON`; Cold SQLite Resume (`T6-3d`, `L814-906`) reopens engines (`eng1 -> eng2`, `eng3 -> eng4`) and checks `inspectPersistedRepairState` (`ListAnsweredExecutions`, `DeriveRepairContext`) and `agent.StreamRepairContextFromStore`.
   - Killed by `m6b_incorrect_resume_outcome` (`internal/session/executions.go:584-586`).

8. **No assertions were weakened**
   - All existing assertions for `T6-1`, `T6-2`, `T6-4` were kept intact; `T6-3` was split into `T6-3a`, `T6-3b`, `T6-3c`, `T6-3d` with strictly stronger filesystem, event-bus, revision, payload, watermark Block2, and cold-resume checks, and verified via 7 overlay mutants (`7/7` killed).

9. **Production code was not unnecessarily changed**
   - `git diff --name-only ef10ca7c8e8031e75fdf27f449924b8254f0e2bb..316e99b90fad5d6e22b6f9a9b01f240be2b55370` contains strictly one file: `runtime/context_space_t3_test.go`. Zero `.go` files were added to `bench/evidence/`.

## Reproducible Independent Verification Commands (`GOFLAGS` unset)

```bash
unset GOFLAGS
gofmt -l runtime/context_space_t3_test.go
git diff --name-only ef10ca7c8e8031e75fdf27f449924b8254f0e2bb..HEAD
go test -count=1 -v ./runtime -run '^TestT3B_CausalAuthorshipConcurrencyAndIgnoredFilesMatrix/RC34_F02_T6_dirty_and_clean_write_failures$'
go test -count=1 -v ./runtime -run '^TestT3B_CausalAuthorshipConcurrencyAndIgnoredFilesMatrix'
go test -race -count=1 ./runtime -run '^TestT3B_CausalAuthorshipConcurrencyAndIgnoredFilesMatrix'
```

## Remaining Scope Boundaries / Inherited Limitations

- Per Section 9 of the remediation charter, the complete Core test suite (`go test ./...`) was not run on this branch and remains for post-review integration gating.
- Inherited PRE-01/PRE-02 platform boundaries remain unchanged and out of scope: L2 old-FD writes, L3 SIGKILL residue, late hard-link race, binary export boundary, native Linux/ext4, Windows/FreeBSD, NFS/FUSE, Desktop, and TB2.
