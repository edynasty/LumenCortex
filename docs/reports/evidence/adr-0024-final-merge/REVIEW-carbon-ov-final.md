# LumenCortex ADR-0024 — OV Final Independent Acceptance Audit Report

**Independent Reviewer:** Gemini 4 Carbon (Read-Only Acceptance Audit; zero participation in `OV-01..OV-06` or legend compression)  
**Implementer Under Audit:** Claude Opus 5.5  
**Audit Timestamp:** 2026-11-22 (Darwin arm64, Go 1.26.5, Apple M1 Pro)  
**Final Audit Verdict:** **`PASS_WITH_LIMITATIONS`**

---

## 1. Exact Branch, Full HEAD, Remote Sync, Ancestry & Worktree Hygiene

All repository state was verified directly via Git plumbing and `git ls-remote origin` without modifying any repository or worktree:

- **Reviewed Worktree:** `/Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-opus-ov-remediation-20261122`
- **Branch:** `fix/adr-0024-opus-ov-remediation-20261122`
- **Verified Local & Remote HEAD (`git ls-remote origin`):** `d0f114be20ab578388e2333f2c6c71392f509de7` (exact match; working tree clean before and after audit: `git status --porcelain` = empty).
- **Verified Commit Chain (linear, 0 merge commits from `main`):**
  - `c73d17359f5d9dd16ef88b0de4525862593fddaa` (`main`, `origin/main` unchanged; preserves only pre-existing `M AGENTS.md`)
  - `a5a1ec6be1081bad4e706944d655a9a9d97f4f70` (`fix/adr-0024-sol-rev-01-astra-final-20261122` — OV remediation base)
  - `11d3222e2a134e705ae9a173e156553ec15be11e` (`fix(toolset): bound identity scans and veto brace-literal counted gaps`)
  - `7e0610fa86bdf0c8001187b4d4985b601603d104` (`fix(toolset): select overflow rows by priority and exact bytes; explain notation`)
  - `a3f19d9d98af8d0ffc59e9da3c202fb3f0e42299` (`docs(adr-0024): record OV-01..OV-06 remediation, oracle results and benchmarks`)
  - `d0f114be20ab578388e2333f2c6c71392f509de7` (`fix(toolset): compress the path-notation legend in failure results`)
- **Historical Branches & Worktrees Preserved Untouched:** Verified all 14 historical ADR-0024 worktrees (`context-space`, `r2`, `r3`, `r4`, `r5`, `r6` @ `4aaf8e7168743d6c8ae8fa6fe87a350ca69f9af2`, `r7` @ `31172bc274c3a4c54243a72e554862d539d7759d`, `r7r2` @ `fc1c4368507bea3f4e13099c62ca35936a6fba48`, `r7r2-carbon-p3` @ `f3f77e35c98de4647243019568ed5ec3093cf072`, `r7r2-opus-final` @ `10f135d1f904ab88c6a086319ca5820ea6b9e09c`, `sol-01-02` @ `3d85d06e31ae8b424b2ed3d0b9b94be70627e5ac`, `sol-rev-01-opus` @ `52ac7d7df7d582bc2cb95e36618381cf30f7ab80`, `sol-rev-01-astra-final` @ `a5a1ec6be1081bad4e706944d655a9a9d97f4f70`, and `opus-ov-remediation` @ `d0f114be20ab578388e2333f2c6c71392f509de7`) have empty `git status --porcelain`.
- **Architecture Specification:** [context-space-architecture-master.zh-CN.md](file:///Users/tangxingpeng/IdeaProjects/me/LumenCortex/LumenCortex-architecture-audit/design/handoffs/context-space-architecture-master.zh-CN.md) (`v0.4-rc3.8 draft 2`), verified SHA256 `4a56762c9b74b5ad2065082f8dc12ad9fd12ca93ab70b56ee510461b0971da9b`, status remains `PENDING_OWNER_APPROVAL`.

---

## 2. Reviewed Production & Test Diffs (`a5a1ec6..d0f114b` and `a3f19d9..d0f114b`)

- **Complete OV Production Scope (`a5a1ec6..d0f114b`):** Confined **exclusively** to [editing.go](file:///Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-opus-ov-remediation-20261122/internal/toolset/editing.go#L318-L1863) (`1 file changed, 524 insertions(+), 81 deletions(-)`, blob `864247b3a7c73f6d85a2a5666665db343bb276fa`).
- **Boundary Compliance (§十二):** Zero changes to `internal/editing` transaction or Partial Commit First semantics, `CommitReceipt`, `MutationEvidence`, `SnapshotStore`, SQLite/migrations, Cognitive Core, Provenance, `internal/window` / `internal/agent` Context Space projection, Tool Descriptions, Node.js/external runtimes, or `LumenCortex-Desktop`.
- **Incremental Legend-Compression Production Diff (`a3f19d9..d0f114b`):** Confined **strictly** to the 5 string constants at [editing.go](file:///Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-opus-ov-remediation-20261122/internal/toolset/editing.go#L1148-L1154) (`5 insertions(+), 5 deletions(-)` in production code; zero algorithmic edits).
- **Test Diffs (`a5a1ec6..d0f114b`):** Added [r7r2_ov_remediation_test.go](file:///Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-opus-ov-remediation-20261122/internal/toolset/r7r2_ov_remediation_test.go) (`886` lines) and [r7r2_ov_remediation_units_test.go](file:///Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-opus-ov-remediation-20261122/internal/toolset/r7r2_ov_remediation_units_test.go) (`151` lines), and updated exact row-count / `pathNotation` expectations in [r7r2_astra_final_test.go](file:///Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-opus-ov-remediation-20261122/internal/toolset/r7r2_astra_final_test.go), [r7r2_final_test.go](file:///Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-opus-ov-remediation-20261122/internal/toolset/r7r2_final_test.go), [r7r2_results_test.go](file:///Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-opus-ov-remediation-20261122/internal/toolset/r7r2_results_test.go), [r7r2_sol_rev01_test.go](file:///Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-opus-ov-remediation-20261122/internal/toolset/r7r2_sol_rev01_test.go), and [r7r2_sol_test.go](file:///Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-opus-ov-remediation-20261122/internal/toolset/r7r2_sol_test.go) without weakening identity, provenance, or budget invariants.

---

## 3. Separate Verdict A — `OV-01` through `OV-06`

| Item | Verdict | Independent Verification & Reproduction Evidence | Remaining Limitations & Impact |
| :--- | :--- | :--- | :--- |
| **`OV-01`** Complete Payload Budget Accounting | **`PASS_WITH_LIMITATIONS`** | Verified [editing.go](file:///Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-opus-ov-remediation-20261122/internal/toolset/editing.go#L1247-L1284) (`omissionBytes`, `rowBudget.size`) and [editing.go](file:///Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-opus-ov-remediation-20261122/internal/toolset/editing.go#L1702-L1825). `omissionBytes` computes exact JSON byte overhead of `,"filesOmitted":<n>,"filesOmittedByState":{...}` to the single byte (`TestOVOmissionBytesMatchesJSON`). Verified RED on base `a5a1ec6` (`TestOVMetadataBand31Rows` left unshown fitting row `g/m00.kt` at `686B <= 4096B`; `red-stage-b.log:70-72`) → GREEN on `d0f114b`. Verified oversized first rows, multiple oversized rows, mixed states, valid UTF-8/JSON, and exact `4,096B` hard cap (`0 / 2,229` violations). | **`OV-CARBON-01` (`CONFIRMED`, `P3`):** In `overflow()` (`editing.go:1735, 1743-1776`), Pass 1 initializes `forms` to the union across *all* candidates (`c=0..2`). In a synthetic `3,966..4,096B` band with an impossible failed row + a `~3,550B` `formExact` failed row + an unchosen `not_attempted` row carrying disjoint `notationForms` (`+131B`), Pass 1 rejects the failed row and Pass 2 excludes it (`0 / 2,229` realistic/adversarial cases affected; `dirtyFailure`, `overflow`, and `filesOmittedByState` stay exact). |
| **`OV-02`** Bounded Complexity | **`PASS`** | Verified [editing.go](file:///Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-opus-ov-remediation-20261122/internal/toolset/editing.go#L544-L901) & [editing.go](file:///Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-opus-ov-remediation-20261122/internal/toolset/editing.go#L1235). `ensureNames` sorts distinct components/reversed filenames once; `ownedSuffix` (`L794-843`) precomputes nearest different-owner suffixes in $O(G \log G)$ instead of $O(G^2)$ per skipped row; `labelScanMaxBytes = 256 KiB` (`L544`) bounds middle-window filename scans; `maxOverflowCandidates = 256` (`L1235`) bounds DP candidates per priority class. Live benchmark confirms `blocked_ordinary_oversize/1024` dropped from **`7.85 s`** (`a5a1ec6`) to **`0.260 s`** (`d0f114b`, **30.1×** faster) with zero stale-cache identity drift (`ProbeB >256KiB` verified at `321,003B`). | Above `256` renderable candidates per priority class or `256 KiB` distinct component bytes, middle-window abbreviation falls back to whole/prefix/suffix forms without losing identity uniqueness. |
| **`OV-03`** Literal Paths & Synthetic Ellipsis | **`PASS`** | Verified [editing.go](file:///Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-opus-ov-remediation-20261122/internal/toolset/editing.go#L351-L467) & [editing.go](file:///Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-opus-ov-remediation-20261122/internal/toolset/editing.go#L903-L1035). All 8 notation categories (`./a/…/f.kt`, `./a/lit…name.kt`, `…/dir`, `r00/…{7}/f.kt`, `pre…suf.kt`, zero-gap veto `L933`, `./` exact prefix, and combined `…/./lit…dir/g.kt`) are cleanly distinguished. `bracedKeys`/`countedTails` (`L352-467`) vetoes `r00/…{7}/f.kt` whenever a competing `r00/q{7}/f.kt` or `r00/p{6}/common/f.kt` exists in the batch (verified RED `red-stage-a-counted-gap.log` → GREEN `TestOVCountedGapNeverReadsAsBraceLiteral` + Carbon `ProbeB`). | Counted gaps are restricted to `1..9999` interior directories with 1- or 2-component tails; deeper collisions fall back safely to longer unambiguous paths. |
| **`OV-04`** Agent-Facing Notation Contract | **`PASS_WITH_LIMITATIONS`** | Verified Owner's binding **Option C** in [editing.go](file:///Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-opus-ov-remediation-20261122/internal/toolset/editing.go#L1148-L1233). Across all `2,229` payloads, `pathNotation` emits **exact** active legend clauses in deterministic order, omits unused clauses, warns `"Display-only labels, not file paths."` whenever any `…` shortening is used, includes its own JSON bytes inside the `4,096B` cap, is untouched by `ModeFull`/`ModeCompact` context projections ([presentation.go](file:///Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-opus-ov-remediation-20261122/internal/agent/presentation.go#L98-L121)), and adds `0B` of recurring Tool Description overhead. | **`OV-CARBON-02` (`CONFIRMED`, `P3` workflow limitation):** For `lsp_rename` ([lsp.go](file:///Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-opus-ov-remediation-20261122/internal/toolset/lsp.go#L164-L213)), the LSP server expands target files not listed in `ToolCall.Arguments`; recovering an abbreviated path requires a workspace discovery step (`find_files`/`lsp_references`/`search_text`). See §6. |
| **`OV-05`** Accurate `truncated` Semantics | **`PASS`** | Verified [editing.go](file:///Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-opus-ov-remediation-20261122/internal/toolset/editing.go#L1565-L1645) & Carbon `ProbeC` (all 7 required cases): `./a/…/f.kt` standalone refusal (`false`), literal `…` dir in receipt (`false`), `<outside-workspace>/key…file.kt` (`false`), shortened `>256B` dir (`true`), shortened `>256B` filename (`true`), omitted file rows (`true`), truncated diagnostics (`true`), and exact `4,096B` untruncated payload (`false`, flipping to `true` at `4,097B` input). Legend compression caused `0` `truncated` flips across all `2,229` payloads. | None. `truncated` strictly reflects removed content (omitted rows/diagnostics/publishedFiles/hashes or shortened text/paths), never display qualification prefixes. |
| **`OV-06`** Priority-Aware Row Selection | **`PASS_WITH_LIMITATIONS`** | Verified [editing.go](file:///Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-opus-ov-remediation-20261122/internal/toolset/editing.go#L1286-L1395) (`fewestBytes`, `selectRows`). Exact 3-class priority (`0: failed/unresolved`, `1: published_verified`, `2: not_attempted`) with exact DP over `(rowCount, stateCounts)` and deterministic lexicographic sanitized-path tie-breaking (`TestOVSelectRowsMatchesExhaustive`, `TestOVEqualCostTiesByPath`). Verified 40-file fixture retains **`31/40` rows (`<= 4,096B`) across all 60 input orders** (`30` `perm40` + `30` `perm40fixed`, selecting the **exact same 1 canonical 31-file set** across all 30 fixed-state permutations). | Subject only to the same `OV-CARBON-01` (`P3`) cross-class `forms` union edge case in `overflow()` noted under `OV-01`, plus the documented `maxOverflowCandidates = 256` per-class cap. |

---

## 4. Separate Verdict B — Legend Compression (`a3f19d9..d0f114b`)

**Incremental Verdict:** **`PASS`**

1. **Five Text Changes Only:** Confirmed via `git diff a3f19d9..d0f114b -- internal/toolset/editing.go` that production changes consist solely of shortening `notationShort`, `notationCount`, `notationTail`, `notationExact`, and `notationRedacted` at [editing.go](file:///Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-opus-ov-remediation-20261122/internal/toolset/editing.go#L1148-L1154).
2. **Exact JSON-Encoded Byte Savings (Independently Measured):**
   - Shortened labels only (`notationShort`): **`166B` → `109B`** (`-57B` including JSON key, quotes, and comma).
   - Shortened + counted gap (`notationShort` + `notationCount`): **`200B` → `135B`** (`-65B`).
   - Exact `./` path only (`notationExact`): **`76B` → `52B`** (`-24B`).
   - Shortened + literal tail: **`214B` → `149B`** (`-65B`); Redacted literal `…`: **`88B` → `63B`** (`-25B`).
3. **Row Recovery Across All `2,229` Rendered Payloads (Independently Verified from Raw Outputs):**
   - **`2,229` total rendered outputs** (note: suites are overlapping stress slices, audited per-suite and in total without assuming disjointness): **`0` payloads lost a failed row, `0` payloads lost any row**, total displayed failed rows increased **`25,758 → 25,932` (`+174` across `174` payloads)**, total displayed rows increased **`32,702 → 32,966` (`+264`)**.
   - **Per-dataset verification:** `684` historical corpus (`+39` payloads gain 1 failed row: `7,291 → 7,330`); `1,180` adversarial matrix (`+17`: `14,515 → 14,532`); `33` filename sweep (`+30`: `911 → 941`); `5` long-filename cases (`end_k70`, `end_k100`, `end_k240`, `start_k240`, `three_1404` = **`29 / 29 / 29 / 29 / 3`** at `4,096B` / `1,395B`); `60` 40-file orders (`30` `perm40` + `30` `perm40fixed` = **`31/40` rows in all 60 orders**, `4,090..4,096B`); archived `deep_f32_v00_u00_eio` (**`31` rows at `4,092B`**, `filesOmitted: 1`, `{"partially_modified": 1}`).
4. **Agent Comprehension, `4096B` Boundary & Zero Context Budget Regression:**
   - Every compressed clause (`…/ = 1+ omitted dirs; … in a name = omitted chars. Display-only labels, not file paths.`, `…{N}/ = exactly N dirs.`, `…/./x = omitted dirs, then exactly x.`, `./x = exactly path x, … literal.`, `<outside-workspace>/x = outside file named x.`) is self-contained, unambiguous without reading Go source, and preserves `0` invalid JSON/UTF-8 outputs and max size **`4,096B`**.
   - **Trade-Off Verification Against Rejected Tool-Description Patch (`/private/tmp/lc-ov-desc/rejected-descriptions.patch`, read-only):** Moving `PathLabelContract` into the 6 editing tool specs (`write_file`, `replace_in_file`, `apply_patch`, `edit_lines`, `edit_go`, `lsp_rename`) required cross-package `presentSpecs`/`keepPathLabelContract`/`shareContract` hooks in `internal/agent/presentation.go`, imposed **`~660B` recurring schema overhead on every ordinary model request** (pressuring 8K-context admission), and recovered only `+51` additional failed rows across the entire `2,229`-payload failure corpus (`25,983` vs `25,932`, i.e., `+0.20%`) while still needing a `73B` inline fallback note on elided results. Owner's **Option C** is decisively validated on engineering grounds.

---

## 5. Complete Attempted-Batch Identity Verdict

**Verdict:** **`VERIFIED — 0 AMBIGUOUS OR MISROUTED LABELS ACROSS ALL 2,229 PAYLOADS`**

- Carbon implemented two independent clean-room label resolvers (in Python `/tmp/lc-adr0024-ov-carbon-audit/audit_all_2229_payloads.py` and in Go `/tmp/lc-adr0024-ov-carbon-audit/zz_carbon_ov_audit_test.go`) directly from the 5-line `pathNotation` contract, without reusing Opus's or Astra's helper code.
- Across all **`2,229` rendered payloads (`38,709` total emitted labels**, including `12,444 × 2` strict + zero-gap checks on the `684` historical corpus inputs and live replay of all `1,545` adversarial/sweep/permutation/literal/zero-gap/window payloads), **every emitted label resolved to `1` and only `1` path in the complete attempted batch (`receipt.Files ∪ result.Files ∪ editErr.Path`), including omitted rows and result-only unattempted paths**, with `0` collisions, `0` brace-literal masquerades, and `0` zero-gap root-suffix collisions.

---

## 6. Authoritative Original Path Recoverability Assessment

Distinguishing **(i) unique display label**, **(ii) unambiguous meaning**, and **(iii) actionable recovery of the exact filesystem path** across every production caller of `formatCommitFailurePayload` / `commitOutcome` / `editOutcome`:

1. **`write_file`, `replace_in_file`, `edit_lines`, `apply_patch` ([files.go](file:///Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-opus-ov-remediation-20261122/internal/toolset/files.go#L148-L191), [editing_tools.go](file:///Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-opus-ov-remediation-20261122/internal/toolset/editing_tools.go#L101-L115), [patch.go](file:///Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-opus-ov-remediation-20261122/internal/toolset/patch.go#L109)):** **Direct, zero-IO authoritative recovery.** In [window.go](file:///Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-opus-ov-remediation-20261122/internal/window/window.go#L59-L106), `window.Groups` pairs each `assistant` message (`ToolCalls[].Arguments`) atomically with its `tool` result messages. Across `ModeFull`, `ModeCompact`, Floor Relaxation, and cold SQLite `Resume`, `ToolCalls[].Arguments` is never split from its `tool` failure message and never truncated. Because every emitted label uniquely resolves within the attempted batch, matching the label against `args.path` / `args.patches[].path` deterministically recovers the exact original path.
2. **`edit_go` ([editing_tools.go](file:///Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-opus-ov-remediation-20261122/internal/toolset/editing_tools.go#L186-L213), [goedit.go](file:///Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-opus-ov-remediation-20261122/internal/editing/goedit/goedit.go#L82-L143)):** **Bounded single-directory recovery.** `goedit.Plan` only modifies `.go` files inside `filepath.Dir(args["path"])`. Even if a sibling `.go` path is abbreviated, `filepath.Dir(args["path"])` is present in `ToolCall.Arguments`, so `list_dir` on that single directory yields the exact sibling filenames.
3. **Nested `eval` Callbacks ([execution.go](file:///Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-opus-ov-remediation-20261122/internal/agent/execution.go#L455-L495)):** Inside the `eval` cell, the caller holds the exact arguments it passed; on the parent `eval` tool result, `formatDirtyReceiptSummary` also attaches up to 8 raw workspace-relative `CommitReceipt` paths (`512B` per note).
4. **`lsp_rename` ([lsp.go](file:///Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-opus-ov-remediation-20261122/internal/toolset/lsp.go#L164-L213)):** **Indirect discovery required when abbreviated (`OV-CARBON-02`, `P3`).** `lsp_rename` arguments contain `{path, line, column, new_name}` while the language server chooses the `WorkspaceEdit` file set across the workspace (`tierWorkspace` in [execution.go](file:///Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-opus-ov-remediation-20261122/internal/agent/execution.go#L70-L74)), and `failurePathIndex` indexes only the `WorkspaceEdit` batch rather than untouched workspace files. When an `lsp_rename` failure shortens a deep path (`>256B` or `4,096B` overflow), the Agent cannot invert the label from `ToolCall.Arguments` alone and must use existing read tools (`find_files` on the non-elided suffix/basename plus `lsp_references` or `search_text` if untouched workspace twins share that basename) to locate the exact file on disk.

---

## 7. Complete Corpus & Adversarial Verification Matrix

| Evidence Family | Expected | Independently Verified Result |
| :--- | :--- | :--- |
| Historical corpus (`oracle-corpus.jsonl`) | `684` cases | **`684` verified** (`0` errors; `+39` gain 1 failed row; `12,444` labels resolved 1-to-1 by Carbon resolver under strict & zero-gap modes) |
| Adversarial matrix (`oracle-matrix.jsonl`) | `1,180` cases | **`1,180` verified** (`0` errors; `+17` gain 1 failed row; max `4,096B`) |
| Filename position sweep (`oracle-sweep.jsonl`) | `33` cases | **`33` verified** (`0` errors; `+30` gain 1 failed row; `27..29` rows shown, max `4,096B`) |
| Total rendered outputs (`13` suites) | `2,229` | **`2,229` verified** (`38,709` labels, `0` problems, `0` lost rows, `+174` failed rows, `2,229/2,229` wire SHA256 matches) |
| Original Carbon subtests (`verify.sh carbon-probe`) | `17` subtests | **`17/17` PASS** (+ `TestCarbonR7R2CT02ReplaySemantics` PASS, live run on `d0f114b`, `exit=0`) |
| 40-file permutations (`perm40` + `perm40fixed`) | `60` orders | **`60/60` verified** (`31/40` rows in all 60 orders; `1` identical 31-file set across all 30 `perm40fixed` orders; `TestOVSelection40Permutations` PASS) |
| Five long-filename cases (`TestSOLREV01SolFloorCases`) | `29/29/29/29/3` | **`29 / 29 / 29 / 29 / 3` verified** (`4,096B` × 4, `1,395B` × 1) |
| Archived 32-file case (`TestSOL01ArchivedDeep32`) | `31` @ `4,092B` | **`31` rows at `4,092B` verified** (`filesOmitted: 1`, `{"partially_modified": 1}`) |
| R7 Partial Commit & Cold Resume (`-race`) | All PASS | **`PASS` (`exit=0`)** across `internal/editing` (`10.66s`), `internal/toolset` (`26.81s`), `internal/agent` (`4.56s`) |

---

## 8. Performance & Complexity Assessment (`32`, `100`, `1,024`, `4,096` Rows)

Live benchmark executed independently on `d0f114be20ab578388e2333f2c6c71392f509de7` (`GOMAXPROCS=2`, Darwin arm64, Apple M1 Pro) compared against archived Astra baseline (`a5a1ec6`, [bench-astra.log](file:///Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-opus-ov-remediation-20261122/docs/reports/evidence/adr-0024-ov-remediation/logs/bench-astra.log)):

| Workload | `32` rows | `100` (`128`*) rows | `1,024` rows | `4,096` rows | Scaling & Comparison vs `a5a1ec6` |
| :--- | ---: | ---: | ---: | ---: | :--- |
| `unique_deep` | `4.69 ms` (`0.84 MiB`, `4.6k` allocs) | `8.08 ms` (`1.42 MiB`, `13.1k` allocs) | `49.44 ms` (`11.93 MiB`, `133.3k` allocs) | `171.95 ms` (`44.69 MiB`, `400.7k` allocs) | Linear $O(N \log N)$ (`42.4 ms → 49.4 ms` @ `1024`) |
| `samebase_modules` | `2.02 ms` (`0.79 MiB`, `5.0k` allocs) | `4.28 ms` (`1.24 MiB`, `11.5k` allocs) | `37.14 ms` (`10.06 MiB`, `104.3k` allocs) | `141.46 ms` (`37.68 MiB`, `319.6k` allocs) | Linear (`34.5 ms → 37.1 ms` @ `1024`) |
| `samebase_longsuffix` | `4.79 ms` (`2.12 MiB`, `6.6k` allocs) | `10.15 ms` (`3.12 MiB`, `13.8k` allocs) | `95.23 ms` (`26.59 MiB`, `159.5k` allocs) | `369.54 ms` (`99.77 MiB`, `411.6k` allocs) | Linear (`87.8 ms → 95.2 ms` @ `1024`) |
| `longname_middle` | `2.42 ms` (`1.00 MiB`, `5.1k` allocs) | `7.27 ms` (`1.68 MiB`, `13.3k` allocs) | `105.71 ms` (`19.99 MiB`, `127.2k` allocs) | `122.60 ms` (`31.10 MiB`, `223.1k` allocs) | Sublinear above `1024` (`256`-cand + `256 KiB` caps kick in!) |
| `blocked_literal` | `6.56 ms` (`5.91 MiB`, `5.0k` allocs) | `6.29 ms` (`1.88 MiB`, `7.3k` allocs) | `63.77 ms` (`19.42 MiB`, `77.2k` allocs) | `261.77 ms` (`77.79 MiB`, `307.8k` allocs) | Linear (`62.9 ms → 63.8 ms` @ `1024`) |
| `blocked_ordinary_oversize`* | `14.79 ms` (`7.18 MiB`, `5.5k` allocs) | `29.83 ms`* (`10.09 MiB`, `9.9k` allocs) | **`260.33 ms`** (`83.07 MiB`, `82.5k` allocs) | *(512 rows: `121.29 ms`)* | **Pathology eliminated:** **`7,845.8 ms → 260.3 ms` (`30.1×` speedup)** |

---

## 9. Full Raw Test Evidence & Exit Codes

### Rerun Live by Carbon on `d0f114be20ab578388e2333f2c6c71392f509de7` (Darwin arm64)
1. `gofmt -l internal/toolset/editing.go internal/toolset/r7r2_*.go` → empty output, **`exit=0`**.
2. `go build ./...` → **`exit=0`**.
3. `go vet ./...` → **`exit=0`**.
4. `GOOS=windows GOARCH=amd64 go vet ./internal/toolset ./internal/editing` & `GOOS=linux GOARCH=amd64 go vet ./internal/toolset ./internal/editing` (compile/vet only) → **`exit=0`** each.
5. `bash docs/reports/evidence/adr-0024-r7r2-final/verify.sh carbon-probe` → `17/17` subtests + `CT02` PASS (`1.384s`), **`exit=0`**.
6. `go test -count=1 ./internal/toolset` → `ok ... 30.396s`, **`exit=0`**.
7. `go test -race -count=1 ./internal/toolset` → `ok ... 101.264s`, **`exit=0`**.
8. `go test -race -count=1 -run '^TestR7|^TestR201|^TestR5|^TestR6' ./internal/editing ./internal/toolset ./internal/agent` → `editing` `10.660s`, `toolset` `26.806s`, `agent` `4.555s`, **`exit=0`**.
9. Carbon independent overlay probes (`TestCarbonAuditProbeA..D` + `BenchmarkOVScale` + `audit_all_2229_payloads.py` in `/tmp/lc-adr0024-ov-carbon-audit/`) → **`exit=0`**.

### Inspected from Archived Raw Logs (Not Re-Executed Full `runtime` `773s` Package)
- [gates.log](file:///Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-opus-ov-remediation-20261122/docs/reports/evidence/adr-0024-ov-legend-compression/logs/gates.log#L1-L967): Verified header records working-tree `internal/toolset/editing.go` blob **`864247b3a7c73f6d85a2a5666665db343bb276fa`**, which matches `git ls-tree d0f114be20ab578388e2333f2c6c71392f509de7 internal/toolset/editing.go` identically (the log was captured immediately prior to committing `d0f114b`, so `HEAD` in the log header was parent `a3f19d9`). Single repository-wide `go test -timeout=45m -count=1 ./...` passed all 43 tested packages (`runtime` `772.913s`, total elapsed `800s`, **`EXIT CODE: 0`**). Because zero files outside `internal/toolset/` and `docs/` changed since `a5a1ec6` and our live `-race` runs across `toolset`, `editing`, and `agent` all exited `0`, re-running the 13-minute `runtime` suite was unnecessary.
- Native execution on **Windows, Linux, and FreeBSD** was **not performed** and remains **`UNVERIFIED`**.

---

## 10. SHA256 Manifest Verification Results

1. **Legend Compression Manifest ([SHA256SUMS](file:///Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-opus-ov-remediation-20261122/docs/reports/evidence/adr-0024-ov-legend-compression/SHA256SUMS)):**
   - Manifest's own SHA256: **`16929c9d062528048e55518c0c4bf0d0f753b05caa20dff651e1a1f5169db70e`** (exact match).
   - `(cd docs/reports/evidence/adr-0024-ov-legend-compression && shasum -a 256 -c SHA256SUMS)` → **`23/23 OK` (`exit=0`)**. *(Note: entries are relative to the evidence directory rather than repo root.)*
2. **OV Remediation Manifest ([SHA256SUMS](file:///Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-opus-ov-remediation-20261122/docs/reports/evidence/adr-0024-ov-remediation/SHA256SUMS)):**
   - `(cd docs/reports/evidence/adr-0024-ov-remediation && shasum -a 256 -c SHA256SUMS)` → **`31/31 OK` (`exit=0`)**.
3. **Wire Payload Digests (`legend-wire.sha256` & `remediation-wire.sha256`):** **`2,229/2,229`** raw serialized payloads verified byte-for-byte (`0` mismatches).
4. **Architecture Specification (`v0.4-rc3.8 draft 2`):** **`4a56762c9b74b5ad2065082f8dc12ad9fd12ca93ab70b56ee510461b0971da9b`** (unchanged).

---

## 11. New Findings Discovered in This Audit

No `P0`, `P1`, or `P2` defects, no identity misrouting, and no file-state corruption were found. Two bounded **`P3`** edge-case limitations were confirmed and calibrated per §十五:

### `OV-CARBON-01` — Cross-class `notationForms` union in `overflow()` Pass 1 can displace a near-`4096B` single-form failed row (`CONFIRMED`, `P3`)
1. **Exact file and lines:** [editing.go](file:///Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-opus-ov-remediation-20261122/internal/toolset/editing.go#L1735) and [editing.go](file:///Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-opus-ov-remediation-20261122/internal/toolset/editing.go#L1743-L1776).
2. **Reproduction:** `TestCarbonAuditProbeA_FormsFixedPointLoop` (`/tmp/lc-adr0024-ov-carbon-audit/zz_carbon_ov_audit_test.go`, `ampCount=558..578`): Batch with an individually `>4,096B` failed row `imp0…x/...`, a second failed row `f1…x/<558-&s>/target.kt` (`PublishPartial`, `formExact` = `52B`, fitting alone at `3,979B <= 4,096B`) + tail-sharing twin, plus an unattempted row whose compact label uses `formShort | formTail` (`+131B` legend). Adding the `PublishNotAttempted` row causes `overflow()` to emit either `[not_attempted]` (`991B`, Case A1) or `files: []` (Case A2) instead of `[failed1]` (`3,979B`).
3. **Root cause:** Line `1735` initializes `forms` to the bitwise OR across all candidates in `cands[0..2]` (including lower-priority `not_attempted` rows). In Pass 1 (`L1744`), unused legend clauses inflate `b.base` by up to `+204B`, pushing a `3,966..4,096B` `c=0` failed row over `4,096B`. `got` (`L1768`) then drops that failed row's `r.forms` bit, and Pass 2's `if r.forms&^forms == 0` (`L1757`) excludes it permanently.
4. **Severity:** **`P3` (Low)**. Requires entering `overflow()` via an already `>4,096B` failed row (or `>64` failures), a surviving literal-`…` failed row with hundreds of JSON-escaped characters and a tail twin landing in a ~`130B` window below `4,096B`, and a competing candidate with disjoint `notationForms`. `0 / 2,229` corpus and adversarial payloads are affected; `dirtyFailure`, `partialPublication`, `overflow: true`, `truncated: true`, and `filesOmittedByState` remain 100% accurate.
5. **Introduced by OV branch:** Yes (`7e0610fa86bdf0c8001187b4d4985b601603d104`).
6. **Real impact on Agent:** In this synthetic corner, the Agent sees exact `filesOmittedByState` (`"partially_modified": 1`) + `overflow: true` rather than the inline failed row, requiring disk inspection of the batch paths per `recovery`.
7. **Minimal remediation (optional follow-up, not required before merge):** Seed or retry `c=0` selection using only `forms` present in `cands[0]` (or evaluate single-form subsets when `selectRows` for `c=0` returns empty while `len(cands[0]) > 0`).
8. **Merge impact:** **Non-blocking** (`P3` limitation acceptable with Owner acknowledgment).

### `OV-CARBON-02` — Abbreviated labels on server-expanded `lsp_rename` failures require workspace discovery (`CONFIRMED`, `P3`)
1. **Exact file and lines:** [lsp.go](file:///Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-opus-ov-remediation-20261122/internal/toolset/lsp.go#L186-L213) calling [editing.go](file:///Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-opus-ov-remediation-20261122/internal/toolset/editing.go#L1397).
2. **Reproduction:** Any `lsp_rename` partial-commit failure where the LSP server's `WorkspaceEdit` touches deep paths (`>256B` non-failed or `4,096B` overflow) that were shortened to `…/<base>` while untouched files outside `WorkspaceEdit` share `<base>`.
3. **Root cause:** Unlike `write_file`/`replace_in_file`/`edit_lines`/`apply_patch`, `lsp_rename` `ToolCall.Arguments` carries only `{path, line, column, new_name}`, not the server-chosen `WorkspaceEdit` file list, and `failurePathIndex` deduplicates against the attempted `WorkspaceEdit` batch rather than the entire repository tree.
4. **Severity:** **`P3` (Low — documented workflow limitation)**.
5. **Introduced by OV branch:** No (inherent to applying batch-scoped path abbreviation to `lsp_rename` since R7R2/SOL-01; OV's `Display-only labels, not file paths.` warning actually improves safety by preventing the Agent from passing `…/x` to `read_file`).
6. **Real impact on Agent:** Agent cannot match the label against `ToolCall.Arguments` directly and must run `find_files` + `lsp_references` / `search_text` to locate the exact workspace file.
7. **Minimal remediation (optional future enhancement):** None needed for ADR-0024 acceptance; if desired later, `lsp_rename` could index workspace files sharing target basenames or prefer suffix-disambiguated paths when `tool == "lsp_rename"`.
8. **Merge impact:** **Non-blocking** (`P3` workflow limitation).

---

## 12. Remaining Historical Risks & Governance Boundaries

All pre-existing ledger items are preserved unchanged without silent acceptance or downgrade:

- **`PRE-01`:** `OPEN P2` (Unix cross-process rename race window between final check and `os.Rename` where POSIX lacks `RENAME_NOREPLACE` / `renameat2`).
- **`PRE-02`:** `OPEN P2` (`writeInPlace` non-atomic `Truncate(0)` + `Write` window for hardlinked/special/create-raced existing inodes).
- **`BASE-01` / `R6-07` (`R5-06`):** `OPEN / UNVERIFIED` (external pre-existing `evalruntime` / Go toolchain environmental behavior).
- **Open Historical `P3` Items:** `CT-02` (blind `apply_patch` replay after crash without re-reading), `R7-F01`, `R3-02`, `R3-04`, `R2-03`, `F-05` through `F-08`, plus `SOL-01`/`SOL-02` inherent notation limits (component-order-only directory permutations fall back to whole paths) and `OV-CARBON-01` / `OV-CARBON-02` above.
- **`R3-01`:** Remains Owner-accepted limitation.
- **Cross-Platform Native Verification:** Native Windows, Linux, and FreeBSD execution remains **`UNVERIFIED`** (`GOOS=windows` and `GOOS=linux` `go vet` passed on Darwin arm64).
- **Architecture Specification:** `v0.4-rc3.8 draft 2` (`4a56762c9b74b5ad2065082f8dc12ad9fd12ca9b` — exact SHA256 `4a56762c9b74b5ad2065082f8dc12ad9fd12ca93ab70b56ee510461b0971da9b`) remains **`PENDING_OWNER_APPROVAL`**.

---

## 13. Explicit Merge-Readiness Recommendation

**Final Verdict:** **`PASS_WITH_LIMITATIONS`**

- **Technical Assessment:** Claude Opus 5.5's `OV-01` through `OV-06` remediation and subsequent Option C `pathNotation` legend compression (`fix/adr-0024-opus-ov-remediation-20261122` @ `d0f114be20ab578388e2333f2c6c71392f509de7`) are **technically sound, deterministic, bounded (`<= 4,096B`), linear-scaling (`7.85s → 0.26s` on the 1,024-row adversarial stress case), strictly confined to `internal/toolset/editing.go`, and free of any `P0`/`P1`/`P2` branch-introduced defects or identity misrouting across all `2,229` verified payloads (`38,709` labels)**.
- **Owner Decision Gate Before Merge:** Ready for Owner acceptance and merge into `main` once the Owner explicitly decides/acknowledges:
  1. Approval of architecture specification `v0.4-rc3.8 draft 2` (`PENDING_OWNER_APPROVAL`).
  2. Acceptance of the carried-forward pre-existing `OPEN P2` filesystem race limitations (`PRE-01`, `PRE-02`), the `UNVERIFIED` native Windows/Linux/FreeBSD runtime status, and the bounded `P3` limitations (`CT-02`, `R7-F01`, `R3-02`, `R3-04`, `R2-03`, `F-05..F-08`, `OV-CARBON-01`, `OV-CARBON-02`).