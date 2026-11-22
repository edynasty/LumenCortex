# ADR-0024 Owner approval and integration conditions

- Decision date: 2026-11-22. Authority: Owner's explicit ADR-0024 Final Integration authorization, Decisions 1-3.
- Target Core candidate: `d0f114be20ab578388e2333f2c6c71392f509de7`; integration base: `c73d17359f5d9dd16ef88b0de4525862593fddaa`.
- Authoritative specification: [Context Space Architecture v0.4-rc3.8 draft 2](0024-context-space-v0.4-rc3.8-draft2.zh-CN.md), SHA256 `4a56762c9b74b5ad2065082f8dc12ad9fd12ca93ab70b56ee510461b0971da9b`. The draft's status line is preserved for byte identity; this separate ledger records approval. Scenario F and conforming references supersede rc3.7 for current behavior; historical versions and rollback meanings are unchanged.
- Approved: whole-batch preflight; publication one file at a time in canonical order; stop at the first failure; preserve already published files, without automatic destructive compensation; reconcile from verified per-file evidence. Existing Context Space and Cognitive Core admission, evidence, provenance, privacy and recovery invariants remain binding.

## Evidence and open risks

Gemini 4 Carbon independently reviewed the frozen candidate: `PASS_WITH_LIMITATIONS`. The [original report](../../reports/evidence/adr-0024-final-merge/REVIEW-carbon-ov-final.md) remains unchanged (SHA256 `b0670902243c297695a3ccf9183c01869ca9b765188420da017bb5628140e434`). The [canonical reconciliation ledger](../../reports/evidence/adr-0024-final-merge/README.md#canonical-open-p2-ledger), verified against the [original R3 records](../../reports/adr-0024-context-space-implementation-2026-11-18.md), controls PRE attribution instead of the historical report's section 12.

- **EDITING-PRE-01: OPEN P2.** Existing-file truncate followed by a failed write can destroy the original under ENOSPC, EFBIG or similar faults. Introduced before ADR-0024 at `e5777459f0e32d3070635836f7eebdac33f082dd`; survives in `internal/editing/publish_file.go` (`writeInPlace`). Closure requires independently verified preservation or recoverability under real write failures, including ENOSPC.
- **EDITING-PRE-02: OPEN P2.** Non-cooperative external modifications between validation and mutation can be overwritten or lost, leave mixed content, or yield a falsely trusted success. The original rollback overwrite predates ADR-0024 (`e5777459f0e32d3070635836f7eebdac33f082dd`); `dcc0589c2b89921506053713e03b6ae218f6ed5e` removed that automatic rollback path, not the surviving compare-to-mutate windows in `writeInPlace` and `deleteCAS`. Closure requires independent verification that external modifications are preserved or that conflicts remain explicit without false success across the surviving paths.
- Historical P3 limits remain: CT-02, R7-F01, R3-02, R3-04, R2-03, F-05 through F-08, SOL-01/02 notation limits, OV-CARBON-01 (legend reserve can omit a fitting failed row) and OV-CARBON-02 (abbreviated server-expanded LSP rename paths require workspace discovery). R3-01 remains an accepted macOS exFAT limitation. No power-loss durability guarantee; BASE-01/R6-07 (R5-06) remains OPEN/UNVERIFIED.
- Native Windows, Linux and FreeBSD verification remains UNVERIFIED. Cross-builds or cross-vet results are not native execution.

## Development-main only

Temporary acceptance is limited to the requested development-main integration and conditional on the integration gates. Preserve recoverable snapshots or backups including untracked data, monitor disk and quota headroom, avoid non-cooperative concurrent writers, and authoritatively re-read uncertain or dirty results before recovery. Never blindly replay a partially committed batch. Both PRE findings remain OPEN at P2 until independently fixed and verified.

This decision does not authorize production release, general availability, stable Core certification, unrestricted use against irreplaceable files, formal TB2 execution, or closure or downgrade of either PRE finding. Desktop and production changes after the frozen candidate are out of scope.
