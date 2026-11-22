# ADR-0024 SOL-01 / SOL-02 — Independent review (read-only)

- **Reviewer:** Claude Opus 5.5, independent review; no repository file was modified, committed, pushed or merged.
- **Range:** `10f135d1f904ab88c6a086319ca5820ea6b9e09c..3d85d06e31ae8b424b2ed3d0b9b94be70627e5ac`; tested code commit `fc107ba867e8d4e6ef3c7db975a67c1c210069ac` (its Go tree equals HEAD's).
- **Verdict:** `PASS_WITH_LIMITATIONS`. No branch-introduced P0/P1/P2. One new branch-introduced P3 (`SOL-REV-01`) needs an Owner decision.
- **Independence note:** this review ran in the session that also holds the implementation history. Claims from that history were treated as unverified; every number below was re-derived with review-owned probes.
- **Location:** this file and all probes/logs live outside the repository in `/private/tmp/lc-sol-indep-review.mY38Qu/` (not version-controlled; `/private/tmp` can be purged).

## Baseline

| Check | Result |
|---|---|
| Branch / HEAD / upstream | `fix/adr-0024-sol-01-02-remediation-20261122` at `3d85d06…`; `git ls-remote` equals local; worktree clean |
| Ancestry | `fc107ba` → `10f135d`; `fa909ad`, `f3f77e3`, `fc1c436`, `c73d173` are ancestors |
| Production diff | non-test Go: `internal/toolset/editing.go` only; equals `production.diff` byte-for-byte; no exported identifier, JSON tag or payload key added/removed |
| Evidence manifest | 21/21 OK, also equal to committed blobs; manifest SHA256 `56c8a0d178c13e7e96bdc102f6c0962fec2946a25d4e1d26c20ef8e871687cf1` |
| Historical manifests | r7 23/23 @31172bc, r7r2 45/45 @fc1c436, carbon-p3 17/17 @f3f77e3, r7r2-final 40/40 @10f135d (`df1900aa…`) |
| Other refs / worktrees | all ADR-0024 refs equal their remotes; historical worktrees clean; main keeps only `M AGENTS.md` |
| Specification | `context-space-architecture-master.zh-CN.md` SHA256 `4a56762c…` unchanged; `PENDING_OWNER_APPROVAL` |

## SOL-01 — `PARTIALLY_CONFIRMED`

- Archived fixture: regenerated from the archived generator (`zz_opus_new01_dump_test.go.txt`, hash in the r7r2-final manifest). The captured input is byte-identical to `testdata/adr0024-sol/deep_f32_v00_u00_eio.input.json` (SHA256 `67075d57…`). Carbon 19 rows/3,975 B (13 omitted), Opus 22/3,987 B (10 omitted), Sol **32/4,030 B, 0 omitted**; all three equal the committed evidence outputs.
- Algorithm (`editing.go:899-951`): one compact label per row (`label(raw,0)`), exact per-row JSON costs give an upper bound, then a descending scan with exact omission metadata. This is correct even where size is not monotonic. The readable upgrade runs only at the winning count. `maxCandidateFileRows` = 54 is a safe bound (no real row is under 75 B).
- Oracle: every k > shown up to min(failed, 54) rebuilt with exact omissions. No longer prefix fits in 348 corpus or 44 adversarial overflow outputs.
- Label minimality: an independent brute-force enumerator (strict reading) never found a cheaper unique label than Sol's compact label in 1,846 cases.
- Not universal: see `SOL-REV-01` (long basenames).

## SOL-02 — `CONFIRMED` (with residual notation limits)

- 684 corpus outputs (files + publishedFiles, 9,931 Sol labels) and 1,180 adversarial cases (16,617 labels): 0 duplicates. Every label resolves to exactly one attempted file under a component reading and under a loose "ellipsis = removed text" regex reading, with two exceptions below.
- Cases A–J, the >48 B distinguishing components and the old `last >= keep` overlap all pass. Letter case is preserved; there is no case folding.
- Redaction collisions are omitted and counted by state.
- Residuals (not regressions): J3, a whole-component tail `…/common/f.kt` with `common/f.kt` omitted, is ambiguous only if `…` may stand for nothing. J5, a literal directory named `…`, is inherent to the notation in all versions.

## New finding

**SOL-REV-01 — P3 — CONFIRMED — introduced by Sol**

- Location: `editing.go:660` (`identifiable` requires the whole basename), `:399-489` (`makeLabel` never shortens the basename), `:510` (tail must keep the whole basename).
- Root cause: Sol's candidate space drops basename shortening. Opus's byte-tail and distinguishing-segment labels used it, and they were unique for long basenames.
- Input: 32 failed rows `src/main/kotlin/pkg/<L×k><NNN>.kt` (or `<NNN><L×k>`, `<L×k/2><NNN><L×k/2>`), bare `syscall.EIO`.
- Impact: identifiable failed rows, Opus → Sol:

  | Position | k | Opus | Sol |
  |---|---:|---:|---:|
  | end | 70 | 21 | 19 |
  | end | 100 | 21 | 17 |
  | end | 240 | 21 | 10 |
  | middle | 120 | 19 | 15 |
  | start | 120 | 20 | 15 |
  | start | 240 | 20 | 10 |
  | 3 × 1,404 B basenames | — | 3 | 2 |

  All Opus rows in these cases resolve uniquely. Omitted rows are counted correctly, so nothing is lost silently. The class is absent from the 684 corpus (its longest basename is 27 B).
- Remediation: add one bounded basename candidate, checked against the whole attempted batch: a UTF-8-safe shortest unique `…suffix` or `prefix…ext`. Relax `identifiable` for long basenames. Add the sweep as a regression test asserting rows ≥ the Opus baseline.
- Merge impact: P3; needs explicit Owner acceptance or a fix. Under governance, its fix owner is this reviewer, pending authorization.

## Observations (no finding)

1. Some files differ only by component order, so the only unique label is the full path. With 1 KB components, `B_repeated_dirs_40_pressure` keeps 3/40. Opus showed 30 rows there, 16 of them ambiguous. This follows the "omit rather than mislead" rule, inside the documented candidate space.
2. Corpus, equal row counts: 26 cases have shorter (still unique) labels, 22 keep more row-error text and 2 keep more hashes. `publishedFiles` shows fewer entries in 14 cases and more in 5 (labels are reused from `files`), and every drop is counted in `publishedFilesOmitted`.
3. Scale: Sol resolves every attempted path once. Cost is linear, about 24-36 µs/file: 0.23-0.34 s at 10k files and 1.2-1.8 s at 50k. Opus took 48-53 ms at 50k when no row was shortened, but 2.87 s with long parents. There is no plan-size limit in the engine.
4. Rank change: `failed_before_write`/`unsupported` without error text now rank as failures (display order only). Engine rows of these states carry error text in practice.
5. Test gaps: Sol tests assert distinct strings plus the basename, not resolution against the whole batch. There is no long-basename pressure test. The prefix oracle checks only k+1 for unique basenames.
6. A redaction conflation through identical engine raw strings (J1) is not reachable through `apply_patch`: escaping paths are refused before the engine, with no receipt content.

## Performance (Apple M1 Pro, Go 1.26.5, GOMAXPROCS=2, 684 prebuilt payloads, 5 samples × 3 passes)

| Renderer | CPU ms/payload (median) | MiB/payload | allocs/payload |
|---|---:|---:|---:|
| Opus | 28.03 | 8.15 | 32,196 |
| Sol | 8.58 | 2.87 | 10,014 |

These match the reported allocations (32,197 / 10,014; 8.18 / 2.88 MiB). Reported CPU (39.76 / 11.67 ms) is higher in absolute terms from host load, with the same ratio (−69% here vs −71%). CPU is the median of per-sample means; allocations are per-payload means, not medians over payloads.

## Gates (all in the clean SOL worktree at `3d85d06`, Go tree = `fc107ba`)

| Command | Exit |
|---|---|
| `gofmt -l` (3 changed Go files) | 0, no output |
| `go build ./...`; `go vet ./...` | 0; 0 |
| `go test -count=1 ./internal/toolset`; `go test -race -count=1 ./internal/toolset` | 0; 0 |
| SOL/R7R2 focused `-v` | 0; `-count=20` stable |
| RED: `-overlay` Opus `editing.go`, `-run '^TestSOL0[12]'` | 1 as expected (22 rows/3,987 B; duplicate labels; redaction duplicates) |
| `verify.sh carbon-probe` | 0; 17/17 + CT-02 |
| R7/R201/R5/R6 race (4 pkgs) | 0; 149 PASS lines, 53 top-level tests incl. cold Resume and receipt seal |
| `go test -timeout=45m -count=1 ./...` | 0; 43 ok, 6 without tests, 859 s |
| `git status` before/after | clean/clean |

The implementer's own full-suite log starts at HEAD `10f135d`; its four tested blob IDs equal `fc107ba`, and `fc107ba` was committed during that run. The rerun above removes that ambiguity. No native Windows, Linux or FreeBSD run.

## Ledger

PRE-01, PRE-02 OPEN P2 (unchanged, not accepted). BASE-01/R6-07 (R5-06) OPEN/UNVERIFIED. CT-02, R7-F01, R3-02, R3-04, R2-03, F-05–F-08 OPEN P3. R3-01 Owner-accepted. New: SOL-REV-01 OPEN P3. Spec rc3.8 draft 2 `PENDING_OWNER_APPROVAL`.

## Artifacts

`gates.log`, `corpus/analysis.log`, `corpus/oracle.log`, `adv/sol-check.log`, `adv/sol-check-strict.log`, `adv/compare.log`, `sweep*/`, `bench/*.log`, `probe-src/*`, `analyze_corpus.py`, `compare_adv.py`, `sweep_compare.py`; digests in `REVIEW-SHA256SUMS`.
