# PRE-01 / PRE-02 — Sol L1 fail-closed corrections

Scope: SOL-PRE-L1-01 and SOL-PRE-L1-02 (P2). Finding owner: GPT-6.2 Sol. Branch: `fix/editing-pre01-pre02-sol-l1-remediation-20261123`. Exact base: `71cce56acb06b457dad2a4fec3a2c3db56537fd6`.

This report supersedes the affected policy and evidence statements in [the Opus fail-closed report](editing-pre01-pre02-failclosed-2026-11-23.md). Historical reports, raw logs and manifests remain unchanged.

- **SOL-PRE-L1-01:** an outside-workspace alias created after staging but before final validation was not refused. The successful exchange split the two names across inodes and issued a trusted hash.
- **SOL-PRE-L1-02:** an injected `ENOTSUP` at exchange still triggered overwrite-style rename and a verified receipt. Unsupported exchange must be treated as a refusal, not a different publication algorithm.

## Correction to the earlier silent-loss evidence

The historical candidate probe refused the already hard-linked file during preparation, before the write hook could execute. The original target was not modified by that attempt; **the probe did not independently demonstrate a completed external write whose bytes were subsequently preserved**. The original RED still records the silent-overwrite failure on the older path. Do not infer preservation of external bytes from the candidate probe's PASS line.

## Verification

Both findings: **FIXED_BY_IMPLEMENTER / SELF_VERIFIED / PENDING_INDEPENDENT_REVIEW**. PRE-01 and PRE-02 are not independently closed.

On macOS 27.0.1 arm64 with Go 1.26.5, the test-only overlay on `71cce56acb06b457dad2a4fec3a2c3db56537fd6` exited 1: the child completed the late link and baseline exchanged once, splitting the aliases; `ENOTSUP` reached the post-publication path and changed the target inode and bytes. Both outcomes produced `published_verified` with a trusted revision/hash. The corrected regression run exited 0, including the whole-plan case and the unchanged hard-link delete case. The whole-plan test also passed on the exact baseline, where the production preflight was already in place.

Each requested final gate ran once on the tree at `4c79bc05f4fc89e375550b30d61950be58ce1b3f`, before the report-only commit. `gofmt -l internal/editing` returned no filenames; normal editing, editing Race, four-package Race (editing/toolset/agent/session), `go build ./...`, and `go vet ./...` all exited 0. The full Core test suite and non-macOS runtime tests were not run. Timestamped command lines and raw output are in the evidence bundle; the original 17-entry Opus manifest verified without changes.

## Scope boundaries

The final link-count read is an observation, not an atomic predicate on exchange. A non-cooperative process can still add a link after that read. L2 remains open: writes through an already-open descriptor after the last check are outside the guarantee; kqueue does not lock the inode and rename does not revoke the descriptor. L3 remains open (P3): SIGKILL can leave recoverable private staging/quarantine entries; no scanner or automatic deletion was added. No change to exclusive create, delete quarantine, or Partial Commit First.

## Implementation and regression contract

- The final `captureFileIdentity(origFile)` occurs after completed staging, content readback, Host/fault barriers and final pathname identity validation. A current link count greater than one uses the existing owned-stage abort; no swap occurs.
- Unsupported exchange uses the same abort with `CodeUnsupportedFilesystem`. The overwrite-style `os.Rename` fallback, `usedSwap` branches and platform handle-close constants are removed. Actual exchanges retain the existing displaced-inode restoration; delete still uses its existing `preserveExternalBytes` path.
- The late-link test replaces the writer-entry hook with `BeforePublish`: it first reads the complete staging file, executes an independent child `os.Link`, waits for its exit and verifies the alias names the original inode. It asserts zero exchanges, clean refusal, no trusted revision/hash, preserved target/alias/unrelated bytes and inodes, and no private staging leftovers.
- Whole-plan preflight orders `0-plain.txt` before `a/x.txt`, counts writer entries (must be zero), checks the ordinary row is not attempted, and compares both originals and all aliases against pre-commit inode observations.
- The unsupported-exchange test reads the complete stage inside the injected syscall, requires exactly one invocation, and checks the post-publication hook was never entered. Target inode/bytes, unrelated file and receipt assertions fail if plain rename is restored.
- The ineffective quota-plus-ENOTSUP row is removed because its ENOSPC prevented exchange from running. The two actual quota failure cases remain. The obsolete fallback recovery test is replaced by the unsupported-exchange contract; the separate delete recovery tests are unchanged.

The Opus report's "plain rename where no exchange exists" and universal "re-checked at publication" descriptions are superseded: exchange is required, and the fresh link check covers completed alias creation before that observation, not arbitrary later mutation. No raw historical evidence is rewritten.

## Independent review handoff — Claude Opus 5.5 Max

Review this branch from `71cce56acb06b457dad2a4fec3a2c3db56537fd6` through its published tip, not the original Opus implementation in isolation. The production commits are `315dc74f779e40c55c513e0bd65630ca9da19a33` (late link) and `4c79bc05f4fc89e375550b30d61950be58ce1b3f` (unsupported exchange); this report is a separate documentation commit. The local evidence bundle `bench/evidence/editing-pre01-pre02-sol-l1-20261123/` is outside the Core repository. Its `MANIFEST.txt` records the exact final tip, and `SHA256SUMS` seals the commands, test input and raw logs.

1. Reproduce both assertions on the exact Opus baseline with the test-only overlay, then check the same assertions on the Sol tip. Confirm the RED reaches each intended boundary rather than failing during setup.
2. Inspect the final descriptor observation and the owned-stage abort. No late-link swap; no unsupported-exchange replacement; no clean refusal if staging ownership cannot be proved and cleanup is uncertain.
3. Verify the ordinary file ordered first is not attempted when an already-linked later target fails preflight. Check result/receipt agreement and absence of trusted new revisions/hashes on refusal.
4. Check unchanged exclusive create, delete quarantine, and displaced-inode recovery on a real exchange. Keep L2/L3 open and do not infer native platform behavior from source inspection or prior VM runs.

Independent review has not been run by the implementer. No merge, PR, or closure is authorized by this handoff.
