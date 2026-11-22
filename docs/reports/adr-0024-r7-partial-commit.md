# ADR-0024 R7 — Filesystem partial commits and Agent recovery

## Decision record and implementation report

**`R7R2_OPUS_REMEDIATION_COMPLETE_PENDING_INDEPENDENT_REVIEW`** (branch `fix/adr-0024-r7r2-opus-final-20261121` based on `f3f77e35c98de4647243019568ed5ec3093cf072`; see [R7R2 final audit and remediation](#r7r2-final-audit-and-remediation), for independent review by GPT-6.1 Sol). It audits **`R7R2_P3_REMEDIATION_COMPLETE_PENDING_INDEPENDENT_REVIEW`** (follow-up branch `fix/adr-0024-r7r2-carbon-p3-20261121` based on `fc1c4368507bea3f4e13099c62ca35936a6fba48`; see [R7R2 Carbon P3 remediation](#r7r2-carbon-p3-remediation)). Historical R7 handoff status: **`R7_IMPLEMENTATION_COMPLETE_PENDING_INDEPENDENT_REVIEW`**. Carbon's review target is the production candidate `f60738ad4bc5ca5304910617b3889ec0b55a89e2`. Start with the [four-finding ledger](#implementation-and-evidence-ledger) and [replay instructions](#carbon-handoff). The second independent review (Report A, 2026-11-21) is remediated on branch `fix/adr-0024-r7r2-remediation-20261121`; see [R7R2 remediation](#r7r2-remediation). That remediation received `PASS_WITH_LIMITATIONS` from Gemini 4 Carbon, which raised `R7R2-NEW-01` and `R7R2-NEW-02`.

Input is R6 `4aaf8e7168743d6c8ae8fa6fe87a350ca69f9af2`, production `aa7b77a63c86d1951d36bfc12234cb17495cd871`, on the verified R5 → R6 chain. The input spec is `v0.4-rc3.7`, SHA256 `5c9edaeb6b917d7cf508677036f17ab1911dd6b5f1a78ee25d8e0082f6cbb89f`. Sol's R6 review blocked merge. Section 8 of ADR-0024 governs the R7 editing contract. Existing execution admission, causal seals, fences and cold Resume govern the evidence contract.

The ancestry check covered original main `c73d17359f5d9dd16ef88b0de4525862593fddaa`, R4 `0b3b8e721163890ebb664abb4b5c9fc3ef39f4f1`, R5 `bfc493634d819bc3be7d30d669bbb2fbef30212e`, the R6 production commit and the R6 branch tip. At entry the R6 worktree was clean and matched `origin/fix/adr-0024-r6-safety-20261120`. The main worktree already held `M AGENTS.md`.

### Prior art and reuse assessment

| Implementation and source snapshot | Failure, external writers and recovery | Decision for LC |
|---|---|---|
| OpenCode, MIT, [`patch.ts` lines 82–96](https://github.com/anomalyco/opencode/blob/b5e0abd7306f35f989f419de286aec946223d977/packages/core/src/tool/plugin/patch.ts#L82-L96), [219–267](https://github.com/anomalyco/opencode/blob/b5e0abd7306f35f989f419de286aec946223d977/packages/core/src/tool/plugin/patch.ts#L219-L267) | Prepares hunks and obtains permission, then writes sequentially under cooperating locks. On a later I/O failure, the tool names paths completed before failure. A move can write its destination and fail to remove its source. Earlier writes remain. | Closest result semantics. Adapt the completed subset and fail-fast order. LC also records the failed operation's possible effects and the untouched suffix. |
| Oh My Pi, MIT, [`crates/edit/src/session.rs` lines 330–406](https://github.com/can1357/oh-my-pi/blob/b636e19d10b664950d600505d194337bbeef42f0/crates/edit/src/session.rs#L330-L406), [TS result boundary](https://github.com/can1357/oh-my-pi/blob/b636e19d10b664950d600505d194337bbeef42f0/packages/coding-agent/src/edit/index.ts#L558-L586) | Stages the entire batch and checks every guard, writes in payload order, stops on the first writer error. Completed files remain. The TS failure branch ([lines 575–580](https://github.com/can1357/oh-my-pi/blob/b636e19d10b664950d600505d194337bbeef42f0/packages/coding-agent/src/edit/index.ts#L575-L580)) returns text plus `details: {diff: "", errorKind}` and no files field. Its edit tags track accepted file changes. | Adapt preflight and stop-on-error. LC must expose its verified subset even on error. |
| Pi, MIT, [mutation queue](https://github.com/earendil-works/pi/blob/e5d93ea0ab3747293dca5d12280361d04142eeb2/packages/coding-agent/src/core/tools/file-mutation-queue.ts#L15-L59), [write](https://github.com/earendil-works/pi/blob/e5d93ea0ab3747293dca5d12280361d04142eeb2/packages/coding-agent/src/core/tools/write.ts#L43-L88), [edit](https://github.com/earendil-works/pi/blob/e5d93ea0ab3747293dca5d12280361d04142eeb2/packages/coding-agent/src/core/tools/edit.ts#L158-L208) | One path per call. Validates all replacements before one `writeFile`. Realpath-keyed process-local queue serializes cooperating calls. A failing direct write can leave partial bytes. Success count goes to the model and diff to UI details. No built-in file undo in this path. | Reuse LC canonical-path stripes. External descriptor writers remain outside this contract. |
| OpenHands software-agent-sdk, MIT, [editor](https://github.com/OpenHands/software-agent-sdk/blob/d5b327bd1e687c9bc44671327a50537e0980e998/openhands-tools/openhands/tools/file_editor/editor.py#L494-L533), [undo](https://github.com/OpenHands/software-agent-sdk/blob/d5b327bd1e687c9bc44671327a50537e0980e998/openhands-tools/openhands/tools/file_editor/editor.py#L710-L726), [executor](https://github.com/OpenHands/software-agent-sdk/blob/d5b327bd1e687c9bc44671327a50537e0980e998/openhands-tools/openhands/tools/file_editor/impl.py#L65-L100), [resource locks](https://github.com/OpenHands/software-agent-sdk/blob/d5b327bd1e687c9bc44671327a50537e0980e998/openhands-tools/openhands/tools/file_editor/definition.py#L198-L208) | Independent one-path actions, success/error observations, cooperating resource locks declared per resolved path (`declared_resources`), sibling temp + replace. Explicit undo writes a prior file snapshot without a content-CAS against external writers. | Adopt distinction between a tool action and a separate user recovery operation. Overwrite-rename can detach a descriptor and change hard-link semantics. |
| Aider, Apache-2.0, [edit blocks](https://github.com/Aider-AI/aider/blob/5dc9490bb35f9729ef2c95d00a19ccd30c26339c/aider/coders/editblock_coder.py#L40-L123), [undo](https://github.com/Aider-AI/aider/blob/5dc9490bb35f9729ef2c95d00a19ccd30c26339c/aider/commands.py#L551-L650) | Writes matching blocks as it goes, accumulates match failures and tells the model which blocks need correction and how many succeeded. Direct writes can tear. Git-backed undo has dirty-file and commit checks. | Adopt its instruction to retry only outstanding work. LC stops before touching unrelated paths after a failure. |
| Claude Code, proprietary, public [release v2.1.72](https://github.com/anthropics/claude-code/releases/tag/v2.1.72) and [license](https://github.com/anthropics/claude-code/blob/16043fc1342dbd65c13a94621af692c02086f978/LICENSE.md) | The tested CLI preserved A after B's later edit failed. Individual calls return keyed results. A user can separately rewind tracked files. The table below gives the evidence boundary. | Adopt explicit recovery feedback. Checkpoint storage, UI and agent architecture fall outside this change. |
| Git, GPL-2.0-only, [lockfile.h lines 5–80](https://github.com/git/git/blob/f7de81456940dd6f3274e461149bba12965de609/lockfile.h#L5-L80), [tempfile.c](https://github.com/git/git/blob/f7de81456940dd6f3274e461149bba12965de609/tempfile.c#L333-L360) | `O_EXCL` lockfile excludes peers using the protocol. Commit closes and renames over one destination. Abort and normal exit/signal cleanup remove unpublished private state. Uncooperative writers can race replacement. | Adopt the publication boundary. A lockfile rollback has no authority over already-published files. No code copied. |
| Rust tempfile, MIT OR Apache-2.0, [Unix persist](https://github.com/Stebalien/tempfile/blob/5fe9b1280fba8421203ab49b421dae009dd0b7b7/src/file/imp/unix.rs#L94-L133), [Windows persist](https://github.com/Stebalien/tempfile/blob/5fe9b1280fba8421203ab49b421dae009dd0b7b7/src/file/imp/windows.rs#L92-L119) | `persist_noclobber` publishes a single name without overwriting. Uses no-replace rename or link then unlink, or `MoveFileExW` without replace. A failed persist can return the temp to its caller, and a successful link can leave a private name. | Reuse LC's existing Go OS primitives for new files and explicit recreation. Fail closed on unsupported filesystems. |
| google/renameio, Apache-2.0, [tempfile.go lines 153–204](https://github.com/google/renameio/blob/2cc5e3041a104ebcd289803dcb055d8a8c70dce2/tempfile.go#L153-L204) | Syncs and closes the file then renames **over** one destination. Callers clean unpublished temporary files. No Windows implementation or directory-sync guarantee here. | Its publish semantics fit replace-by-name, whereas LC needs no-replace for creates and descriptor semantics for normal text updates. No dependency added. |

None of the examined implementations supplies multi-file content-CAS rollback against uncooperative writers. Snapshot identifiers, precise clocks, full hashes, lock files and a successful readback cannot authorize a later destructive write. LC needs independent Go receipt aggregation because it binds actual file results to existing execution IDs, agents, leases, path guards and durable provenance.

### Claude Code capability matrix

The black-box probe used the published `@anthropic-ai/claude-code@2.1.72` in a disposable directory. Package SHA512 matched the [registry](https://registry.npmjs.org/@anthropic-ai/claude-code/2.1.72). A loopback Messages fixture selected tool calls. The real CLI executed the file tools and sent their results back to the fixture. No live model or production account was involved. The public release tag points to `00553dec20a7b3ad58252a22de7cf1b45ee20330`. Current documentation and later releases describe capabilities beyond that tested version. `UNVERIFIED` in the internals column denotes proprietary implementation with no internal source inspection.

| Claude Code capability | Evidence | Internal implementation | Failure behavior | LC relevance | Reuse decision |
|---|---|---|---|---|---|
| Multi-file work through Edit/Write | CLI wrote `a.txt`, read `b.txt`, then Edit failed with `String to replace not found in file`. Observed A=`committed A\n`, B=`original B\n`. [Tool reference](https://code.claude.com/docs/en/tools-reference) | UNVERIFIED | Model input contained separate successful `toolu_fixture_0` and failed `toolu_fixture_2` results. No aggregate filesystem receipt in this trace. Concurrent batches and crash windows untested. | Direct | Adopt partial-state reporting and a reread instruction. |
| File checkpoints and explicit recovery | [Checkpointing](https://code.claude.com/docs/en/checkpointing), [SDK rewind](https://code.claude.com/docs/en/agent-sdk/file-checkpointing), [changelog line 6401](https://github.com/anthropics/claude-code/blob/16043fc1342dbd65c13a94621af692c02086f978/CHANGELOG.md#L6401) in the 2.1.216 section, not the tested 2.1.72 | UNVERIFIED | `/rewind` offers file and conversation choices. SDK restores tracked files by prompt UUID; from 2.1.216 it skips symlinks and hard links at tracked paths and reports the count. Bash-only paths, other agents and unrelated changes have coverage limits. Later edits to a tracked path can be overwritten by a requested rewind. | Direct | Keep restoration a fresh operation with fresh guards. Reject automatic rollback. |
| Tool lifecycle, call/result identity and user feedback | CLI stream output and next request, [SDK loop](https://code.claude.com/docs/en/agent-sdk/agent-loop), [hooks and PostToolBatch](https://code.claude.com/docs/en/hooks#posttoolbatch) | UNVERIFIED | Tool validation errors return to model. Success/failure of the overall CLI conversation is distinct from success/failure of individual tools. | Direct | Preserve LC's execution IDs, error payload and exact receipt. |
| Subagents and parent-child attribution | [SDK subagents](https://code.claude.com/docs/en/agent-sdk/subagents), [Anthropic engineering](https://www.anthropic.com/engineering/building-agents-with-the-claude-agent-sdk) | UNVERIFIED | Child messages carry `parent_tool_use_id`. Isolated context does not imply filesystem isolation. Main-session rewind has limited coverage of child edits. | Direct | Keep LC's existing parent call ID and nested execution record. |
| Hooks and permission gates | [Hook events](https://code.claude.com/docs/en/hooks#hook-lifecycle), [permissions](https://code.claude.com/docs/en/permissions) | UNVERIFIED | Pre-tool gates can refuse execution. Post hooks report already-executed work. Validation/denial/interruption have different observable events. | Partial | Keep existing LC fences. Post-hook feedback must preserve structured failure content. |
| Session history, interruption, resume and compaction | CLI `--resume`, `--continue`, `--no-session-persistence`, [CLI reference](https://code.claude.com/docs/en/cli-reference), [checkpoint vs conversation](https://code.claude.com/docs/en/checkpointing#guide-a-summary), [v2.1.72 release](https://github.com/anthropics/claude-code/releases/tag/v2.1.72) | UNVERIFIED | Resume selects persisted conversation. Summarization changes context and leaves files in place. Loss of a session or checkpoint can limit recovery. A transcript is insufficient to certify current bytes. | Partial | Retain SQLite cold Resume from execution evidence and require reread before retry. |

Supplement only: a user reported a missing rewind option and partial restoration on 2.0.69/macOS. It is a report about a particular version. The observed CLI sequence and official documentation above establish the behavior used in this decision. [Community issue #14002](https://github.com/anthropics/claude-code/issues/14002)

### Per-file and aggregate contract

Resolve paths, acquire LC stripes in deterministic order, check leases, prepare the entire plan and validate it. Preview describes a proposal. For a live plan, process the prepared paths in canonical sort order. At each path, check cancellation, compare expected content and apply at most one operation. Stop at the first failure. Preserve all published names and bytes. Observe successful paths read-only at settlement. Verification describes bytes or absence at observation time. External changes after observation remain possible.

| File outcome | Receipt / snapshot rule | Recovery |
|---|---|---|
| `not_attempted` | Zero destination writes and no new revision or SHA | Recompute from a fresh read before retrying |
| `failed_before_write` | Refusal with its cause and zero destination writes | Correct the cause |
| `not_published`, `Verified=true` | Unchanged, verified named content and hash | Existing content may be reused at its revision |
| `published_verified` (create/update) | Named regular file matches the planned bytes and expected identity at observation | Keep the verified revision and SHA |
| `published_verified` (delete) | Observed absence, absence sentinel, no live revision | Invalidate the snapshot. Explicit recreation requires a new no-replace create |
| `partially_modified` | A truncate or short write changed the destination. No trusted revision or SHA | Reread and reconcile |
| `external_modification_detected` | Record whether LC wrote before the change and invalidate stale snapshots | Preserve foreign state |
| `published_post_error` / `publication_uncertain` | A known publication or an uncertain effect. No trusted post revision or SHA | Reconcile from disk |
| `unsupported` | Safe publication unavailable. Any verified prefix stays published | Choose a supported filesystem |
| `published_rolled_back` / `rollback_failed` | Historical restoration outcomes | Keep their original meaning in older records |

Retain ordinary guarded descriptor updates: atomic replacement would change inode, hard-link, permission and open-descriptor behavior. A normal existing-file write can truncate before I/O failure (PRE-01). Its compare-to-write and delete's check-to-unlink intervals remain PRE-02. Only new-file namespace publication uses a per-file no-replace primitive. Private staging can leave debris on abrupt process death. Neither file nor directory power-loss durability is promised. A confirmed published prefix plus failure is partial. Possible effects with no confirmed prefix stay uncertain. A clean failed plan has no LC destination mutation. A verified complete plan can still include unchanged files.

Use the same `CommitReceipt`, `FileCommitReceipt`, `FileResult`, `SnapshotStore`, tool message, SQLite execution and derived edges. Dirty failure forces `outcome_unknown`, incomplete lineage and `;cm=unknown` where a file post-ref exists. Reconcile closes an interrupted admission without replay. Live and cold consumers classify the same persisted execution. A bounded model display can omit rows only with explicit omission counts/recovery guidance. Preserve source extraction and fence checks across any display overflow. No schema change or second ledger.

### Findings carried forward

| Finding | Disposition and review boundary |
|---|---|
| PRE-01, P2 | OPEN. Failure on an ordinary existing-file update after truncate can leave an empty or short file. Evidence must say `partially_modified`. |
| PRE-02, P2 | OPEN. Descriptor comparisons and path checks are observations. A normal write or unlink can race an uncooperative external writer. The truncate-to-write race can be silent: bytes appended between `Truncate(0)` and `Write` are overwritten and the file still reports `published_verified` (Report A §9, reproduced on R6 and R7). R7 does not worsen it. |
| R3-01 | Owner-accepted macOS exFAT limitation; the Owner ledger assigns no severity. Safe no-replace creation can be unavailable. |
| R6-07 / R5-06, potential P2 | UNVERIFIED on native Windows. R6 compared a pre-close LastWriteTime against a later value as a condition for restoration. R7 removes that restoration path. R7 adds the same comparison to private staging cleanup (R7R2-BASE-01, P3, UNVERIFIED), which can only make cleanup more conservative. The remaining Windows filesystem behavior requires native execution; see [R7R2 remediation](#r7r2-remediation). |
| R3-02, P3 | OPEN. A process interrupted before publication can leave private staging names. No wildcard cleanup. |
| R3-04, P3 | OPEN. A single tool step exceeding the whole model window can still produce a live/Resume projection difference. |
| R2-03, P3 | OPEN. The recheck notice and the final gate have different enforcement boundaries. |
| F-05, F-06, F-07, F-08, P3 | OPEN. The historical projection, capacity and coverage dispositions remain in the original ledger. |

Microsoft guarantees accurate write times after modifying handles close. NTFS and ReFS represent LastWriteTime in 100 ns units, FAT in two seconds, and exFAT uses 10 ms increments. Representation does not guarantee update timing. A clock match cannot prove content ownership. The R6 strict comparison could cause a conservative refusal. Sol observed no Windows-specific data-loss case. [File Times](https://learn.microsoft.com/en-us/windows/win32/sysinfo/file-times) · [MS-FSA filesystem table](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-fsa/4e3695bd-7574-4f24-a223-b4679c065b63)

OpenCode release behavior depends on the executable. In the 1.x `apply_patch` implementation a writer error preserved the prefix without enumerating it. The 2.x `patch` tool names completed operations. Both complete hunk validation before entering the write loop. [1.18.32 write loop](https://github.com/anomalyco/opencode/blob/545f51d26cc39a907d2867492d498d9607ea5fa4/packages/opencode/src/tool/apply_patch.ts#L220-L256) · [2.3.2 failure result](https://github.com/anomalyco/opencode/blob/c0ad18b402eb2827fc1ee71c171b1177b3fc02d1/packages/core/src/tool/plugin/patch.ts#L89-L91)

The input specification is preserved byte for byte. The new local master `v0.4-rc3.8` has SHA256 `a24d8b18b04aa83ff144fd416d1c5b8751e307d8eb1b7baf08efec74b217fb3f`. Core's bilingual ADR carries its editing rule for standalone checkouts. The architecture-audit and workspace evidence directories live outside the Core Git repository.

### Implementation and evidence ledger

Branch `fix/adr-0024-r7-partial-commit-20261120`. Transaction commit **`dcc0589c2b89921506053713e03b6ae218f6ed5e`**; result propagation and frozen production candidate **`f60738ad4bc5ca5304910617b3889ec0b55a89e2`**. The final handoff commit changes only documentation and evidence. [Full production diff against R6](evidence/adr-0024-r7/production.diff) · [normative specification delta](evidence/adr-0024-r7/spec-rc3.7-to-rc3.8.patch).

Sol supplied the R6 findings. The R7 implementation owner performed the fixes and the runs below. For **each** of R6-F01, F02, F03 and F04 the status is **`FIXED_BY_IMPLEMENTER` / `SELF_VERIFIED` / `PENDING_INDEPENDENT_REVIEW`**. None has independent R7 closure. The frozen baseline for all four is `4aaf8e7168743d6c8ae8fa6fe87a350ca69f9af2`.

The primary implementation owner integrated the changes and ran central verification. Run metadata for the seven research and implementation workers identifies the model route `local/combo/deepseek-v4.1-flash`. Carbon owns the requested independent review.

| Finding | Baseline trigger and impact | Change and self-verification |
|---|---|---|
| R6-F01 · P2 | A separate process appended to A after restoration validation; the later shrink erased its append. R6 returned `rolled_back` and disk held only `old\n`. | `dcc0589c2b`: removed published-file compensation. Original child and assertion, scheduled at the replacement read-only settlement boundary, yielded `longer-new\nFOREIGN-GAP\n`; A was published, externally changed and unverified, with no new revision. [R6 RED](evidence/adr-0024-r7/sol-r6-replay.txt) · [R7 GREEN](evidence/adr-0024-r7/sol-r7-replay.txt). |
| R6-F02 · P3 | Unsupported no-replace operations selected a direct restore; a 20,000-byte deleted file became visible with only 8192 bytes. The [original test](evidence/adr-0024-r7/inputs/lc-adr0024-r6-fallback-probe.go.txt) passes **by observing the defect**, not by proving safety. [Baseline run](evidence/adr-0024-r7/r6-f02-original.txt). | `dcc0589c2b`: a successful delete stays deleted. Explicit recreation stages privately; unsupported publication does not write the destination. Tests cover zero/short writes, real `EFBIG` in a child, injected `ENOSPC`, permissions, destination contention, unsupported operations and process exit inside staging. [Fault matrix](evidence/adr-0024-r7/focused.txt). |
| R6-F03 · P3 | Protected source IDs alone exceeded 8192 bytes. R6 rendered 8556 bytes, then 8580 on error preservation. | `f60738ad4b`: take lineage from the original result before formatting. An 8446-byte input produced a 377-byte JSON overflow display; reading the display alone reports incomplete sources. In a real SQLite call, all 64 distinct source edges survived, including the last source tested for revocation after cold reopen. [Size probe](evidence/adr-0024-r7/sol-r7-replay.txt) · [SQLite, nested, cancellation and Resume tests](evidence/adr-0024-r7/focused.txt). |
| R6-F04 · P3 | Quoted external paths with spaces leaked their directory tail; a generic LSP error serialized the absolute workspace path. | `dcc0589c2b` + `f60738ad4b`: share path rendering between engine and toolset, including structured errors, path boundaries, Windows/UNC spellings, symlinks and generic LSP errors. Original probe returned `<outside-workspace>/key.txt` and `lstat missing.go: file does not exist`. New outputs only; no historical SQLite rows rewritten. [Privacy replay](evidence/adr-0024-r7/sol-r7-replay.txt). |

`Engine.Commit` is the only edit settlement path. Preflight leaves a refused entry and an unattempted suffix; a live failure stops further mutations. For example, a verified write to A, a short write to B, and an untouched C yield `published_verified`, `partially_modified`, and `not_attempted`. Only A can carry a trusted revision and SHA; its final read-only observation can still revoke that trust. The aggregate is `partially_published`, not three successes and not a successful rollback. After process death before SQLite settlement, recovery knows **less**: it can mark the admitted execution unknown, not reconstruct an exact per-file receipt that was never persisted.

The direct commit failure display is at most **4096 encoded bytes**. It prioritizes the failure and verified prefix, with counts for omitted rows. Preserved errors and nested partial results have an **8192 encoded byte** display budget. If protected fields cannot fit, a controlled overflow names the withheld fields and directs reconciliation. The display is not an alternate source of provenance. For a verified prefix with a failed or uncertain suffix, reread affected paths, compare with the receipt and plan only outstanding work. No blind retry of the entire batch. This is guidance to the Agent, not a mechanism: a blind whole-batch replay of a non-idempotent edit, such as an anchored insertion, applies the already committed edit a second time (R7R2-CT-02).

### Carbon handoff

1. Inspect the [production diff](evidence/adr-0024-r7/production.diff), then `git diff 4aaf8e7168743d6c8ae8fa6fe87a350ca69f9af2 f60738ad4bc5ca5304910617b3889ec0b55a89e2` for tests as well. Review receipt aggregation, trusted-revision issuance and deletion of the compensation paths first.
2. From the R7 checkout, run `E=docs/reports/evidence/adr-0024-r7; python3 "$E/review_probes.py" r7 "$E/inputs"`. To repeat the historical RED, invoke the same script with `r6` and its absolute inputs directory from a separate clean R6 checkout. The script verifies input digests. Only the R7 scheduling overlay changes: there is no R7 pre-shrink instruction. The child, append, oracle and checked-in sources are otherwise unchanged.
3. Use `bash "$E/verify.sh" native`, `bash "$E/verify.sh" cross` and `bash "$E/verify.sh" repo`. The [targeted race transcript](evidence/adr-0024-r7/focused.txt) identifies cases for selective independent review. [Claude CLI observation](evidence/adr-0024-r7/claude-2.1.72.txt) is separate from Go acceptance.
4. On a **native Windows** host, run `go test -count=1 -v -run '^(TestR7WindowsNativeTimestampAndNoCompensation|TestR7CreateContention|TestR7CreateRecreationFaultMatrix|TestR7R2Windows.*)$' ./internal/editing` with `LC_R7R2_DIR` on each filesystem being assessed (NTFS, ReFS, exFAT, FAT32). `TestR7WindowsNativeTimestampAndNoCompensation` alone only logs timestamps and does not reach the staging-cleanup comparison (R7R2-BASE-02). Record OS/filesystem and observations; a compiled test executable is not a native PASS. The normal update/unlink race, lack of multi-file atomicity, possible private debris, and lack of power-loss durability remain explicit limits.

### Verification on the frozen candidate

| Surface | Execution and result |
|---|---|
| Native Go 1.26.5, Darwin ARM64 | [Native transcript](evidence/adr-0024-r7/native.txt): 38 changed, present Go files clean under `gofmt`; `go build ./...` and `go vet ./...` passed. `editing`, `toolset`, `agent`, and `session` passed both uncached regular and race runs; `TestT3\|TestT4` passed under `-race` in `./runtime`. |
| Filesystem and recovery | [Targeted race transcript](evidence/adr-0024-r7/focused.txt): independent writer, staged-write failures, process exit, cancelled partial receipt, nested results, cold SQLite and source revocation. The cancelled plan preserved A, left B unattempted, produced one paired tool result and remained `MutationUnknown` on cold reopen. No migrations or changes to the SQLite schema. |
| Cross-target checks | [Cross transcript](evidence/adr-0024-r7/cross.txt): build and vet for the four affected packages on Linux ARM64/AMD64, Windows AMD64 and FreeBSD AMD64 with CGO disabled. All four Windows test binaries compiled. No foreign test binary was run. |
| Repository-wide invocation | `go test -count=1 ./...` exited 1 at the `runtime` package's default 10-minute timeout. The other packages passed or had no test files. The active test had run for one second when the package timer expired. [First run](evidence/adr-0024-r7/repo.txt) |
| Extended runtime package | `go test -timeout=45m -count=1 ./runtime` passed on the same production candidate in 788.942 seconds, exit 0. Together the two runs cover all packages. [Extended run](evidence/adr-0024-r7/runtime-extended.txt) |

Verify the portable evidence from the Core root with `shasum -a 256 -c docs/reports/evidence/adr-0024-r7/SHA256SUMS`. Its entries cover the bilingual ADR, reports, comparison inputs, logs, production diff and replay scripts; the manifest excludes itself. The unmodified 22-file historical probe archive is also retained in the workspace under `bench/evidence/adr-0024-r7-partial-commit-20261120/private-r6-originals/`, with its separate `ORIGINALS.sha256`. That manifest describes the R7 handoff commit `31172bc274c3a4c54243a72e554862d539d7759d`. R7R2 amends the bilingual ADR and this report, so check those four document entries against that commit (`git show 31172bc:<path> | shasum -a 256`); the evidence files themselves are unchanged.

## R7R2 remediation

Input: R7 handoff `31172bc274c3a4c54243a72e554862d539d7759d` on `fix/adr-0024-r7-partial-commit-20261120`, matching its remote. Branch `fix/adr-0024-r7r2-remediation-20261121`, worktree `wt/core-adr-0024-r7r2-20261121`. The finding source is the second independent review, Report A (workspace file `LumenCortex-architecture-audit/reports/2026-11-21/REPORT_A.md`, outside the Core repository), verdict `PASS_WITH_LIMITATIONS`, whose Part A was read-only. The R7 implementation owner fixed the findings below under Owner authorization. Every finding fixed here is **`FIXED_BY_IMPLEMENTER` / `SELF_VERIFIED` / `PENDING_INDEPENDENT_REVIEW`**. None is independently closed.

Production commits: `c08a1e3fe21e3d281454deecce1e2e16db3d053f` (toolset display) and `e37976dafca4dc2882d44df9859b96b2661e81ce` (editing aliases and pre-write classification). No SQLite migration, schema change, new ledger or public API change. Evidence lives in [`evidence/adr-0024-r7r2/`](evidence/adr-0024-r7r2/); verify it with `shasum -a 256 -c docs/reports/evidence/adr-0024-r7r2/SHA256SUMS`.

### Fixed findings

| Finding | Trigger and impact at `31172bc` | Change | RED → GREEN |
|---|---|---|---|
| R7R2-Results-01, P3 | A 16–32-file batch with deep package paths and a path-less error (EIO, cancellation). The shrink loop halved every row path to 64 bytes before dropping rows, cutting from the end, so all rows collapsed to one shared directory prefix and the failed file had no name in the payload. Disk, receipt and seal were correct; the Agent could not tell which file failed. | `c08a1e3`: drop diagnostics, the `publishedFiles` duplicate, per-row error text, the top-level error and per-row `sha256`/`newRev` before rows; drop verified or untouched rows before shortening the rest. Failed rows are never dropped and keep a 1024-byte path bound. Paths are shortened from the front at a component boundary. The overflow envelope keeps failed rows with tail-preserving paths and reports `filesOmittedByState`. Output stays valid JSON within 4096 bytes. | [RED](evidence/adr-0024-r7r2/logs/02-red-toolset.txt) (`TestR7R2DeepPathRowsStayIdentifiable` n=16/20/32 and cancellation, `TestR7R2OversizedFailedPathKeepsFilename`) · [GREEN](evidence/adr-0024-r7r2/logs/04-green-toolset.txt) · [review probe replay](evidence/adr-0024-r7r2/logs/06-review-probe-results.txt): n=12/16/20, every row path exact and distinct. |
| R7R2-PartialCommit-01, P3 | Two spellings of one physical file in one plan: a hard link, or a letter-case variant on a case-folding volume (macOS APFS default). `canonicalPath` resolves symlinks only, so both reached the write stage: the first was published, the second failed as a conflict and was mislabelled as external modification. | `e37976d`, `internal/editing/aliases.go`: existing targets are compared with `os.SameFile`. A new target is compared by its deepest existing directory plus a case-folded remainder, only when that directory folds case. The fold check looks up a case-swapped spelling of an entry of the directory, then of the directory in its parent, and compares identities; it never creates a file. Only when no spelling can be tested does it fall back to the platform default (darwin and windows fold). The plan is refused with `CodeInvalid` before locks, leases or writes. Distinct names on a case-sensitive volume stay distinct. | [RED](evidence/adr-0024-r7r2/logs/01-red-editing.txt) (hard link, existing-file case variant, three create variants) · [GREEN](evidence/adr-0024-r7r2/logs/03-green-editing.txt) · [review probe replay](evidence/adr-0024-r7r2/logs/05-review-probe-partialcommit.txt): every `ALIAS_*` and `PCCASECREATE` row refused with zero write attempts. |
| R7R2-PartialCommit-02, P3 | A stale base revision or out-of-date session read found while preparing, often caused by LC's own earlier write, was reported as `external_modification_detected`. | `e37976d`: `prepared.fail` reports a conflict or stale code raised before LC opened the target for writing as `failed_before_write`, keeping the original code and message. `external_modification_detected` remains for changes observed after LC entered publication. Snapshot invalidation and `MutationEvidence` are unchanged: these refusals were and remain `dirty=false`, and the seal stays `;cm=unknown` because the outcome is a failure. | Same RED/GREEN logs (`TestR7R2StaleBaseIsFailedBeforeWrite`, `TestR7R2SelfReplayIsFailedBeforeWrite`, `TestR7R2SessionViewConflictIsFailedBeforeWrite`); probe `PREFLIGHT_stale_base_on_C` and `RT1_replay` now show `failed_before_write`, `dirty=false`. |
| R7R2-CT-01, P3 (test evidence) | Mutants M2 (dirty receipt sealed `;cm=none`) and M3 (partially modified file reported verified with `NewRev`/`NewSHA256`) passed Carbon's tests and, for M2, the whole official `TestR7\|TestR6` set. | `TestR7PartialCommitAndDiagnosticsColdResume` now uses three files and asserts each persisted row: A `external_modification_detected`, B `partially_modified`, C `not_attempted`, none verified or carrying `newRev`/`sha256`; and the SQLite seal `outcome_unknown` + `;cm=unknown`. Carbon's own tests are not part of this branch and were not edited. | [unmutated PASS](evidence/adr-0024-r7r2/logs/07-ct01-unmutated.txt) · [M2 FAIL on the seal](evidence/adr-0024-r7r2/logs/08-ct01-mutant-m2.txt) · [M3 FAIL on B's row](evidence/adr-0024-r7r2/logs/09-ct01-mutant-m3.txt); mutant diffs in `inputs/`. |
| R7R2-BASE-02, P3 (acceptance gap) | The named native Windows acceptance test only logs timestamps and never reaches the staging-cleanup comparison. | New `internal/editing/r7r2_windows_test.go` (build tag `windows`): `TestR7R2WindowsPublishContentionCleansPrivateTemp` asserts foreign bytes intact, `not_published` and no private leftover at a `MoveFileExW` conflict; `TestR7R2WindowsLongNameCreateVerifies` asserts a verified create where FAT may reassign the file ID; `TestR7R2WindowsLastWriteTimeAcrossClose` records the pre/post-close comparison. The handoff names the full native command. | [Cross build and vet, windows/amd64 and arm64](evidence/adr-0024-r7r2/logs/10-windows-cross.txt). **Not executed natively.** |
| R7R2-BASE-03, P3 (evidence) | `sol-r6-replay.txt` lacked the `fallback` case the committed script emits; `focused.txt` and `r6-f02-original.txt` lacked command lines or exit codes. | Regenerated with command, script digest, toolchain and exit codes. Historical files are left unchanged. | [Full R6 replay](evidence/adr-0024-r7r2/logs/11-sol-r6-replay-full.txt): `gap exit 1`, `jsonpath exit 1`, `fallback exit 0`, script exit 0 (all expected R6 outcomes). The R7R2 focused race run is in the verification table below. |
| R7R2-BASE-04, P3 (governance) | The zh ADR called the unapproved rc3.8 "authoritative"; rc3.8 claimed Owner authorization, had no date and kept seven rollback passages beside scenario F; R3-01 carried a severity the Owner never assigned. | Spec draft 1 archived byte for byte. Draft 2 adds a date and `PENDING_OWNER_APPROVAL` status, marks or rewrites the rollback passages as historical, and records the R7R2 rules in a scenario F supplement. Both ADRs use neutral wording and state the pending approval. R3-01 has no severity label. | [rc3.8 draft 1 → draft 2](evidence/adr-0024-r7r2/spec-rc3.8-draft1-to-draft2.patch) · [rc3.7 → draft 2](evidence/adr-0024-r7r2/spec-rc3.7-to-rc3.8-draft2.patch). **Owner decision still required.** |
| R7R2-CT-02, P3 (documentation) | A blind whole-batch replay after a partial commit applies an anchored insertion twice. Report A and the prior text described "no blind retry" without saying it is not enforced. | Recorded as a limitation in this report, both ADRs and the spec supplement. No mechanism added: the Owner-authorized Partial Commit First semantics keep the prefix, and retry idempotence for non-idempotent edits would need a new design. | [Probe on `e37976d`](evidence/adr-0024-r7r2/logs/12-ct02-blind-replay.txt): first call `partially_published`; the identical replay returns `ok=true` and A holds `added-once` twice. Behavior unchanged, as documented. |
| R7R2-CT-04, P3 (citations) | Four citation precision errors. | Claude changelog line 6401 attributed to the 2.1.216 section; OpenHands resource locks linked at `definition.py` 198–208; OMP TS failure branch described as `details: {diff, errorKind}` without a files field; OpenHands undo range corrected to 710–726. Each line was checked against the pinned upstream source. | Upstream snapshots in the review workspace. |
| PRE-02 documentation | The truncate-to-write race can be silent. | One sentence in the carried-forward ledger, both ADRs and the spec supplement. PRE-02 remains **OPEN P2**; the mechanism is unchanged. | Report A §9 reproduction (R6 and R7). |

### Not fixed, with reason

| Finding | Status | Reason and requested decision |
|---|---|---|
| R7R2-BASE-01, P3 | OPEN, UNVERIFIED | Windows private-staging cleanup compares a LastWriteTime captured before `CloseHandle` with one read after. If close moves the timestamp, a clean `MoveFileExW` conflict can be reported `publication_uncertain` and leave `.lcx-create-*` debris. No data-loss path: the comparison only withholds cleanup. Dropping the timestamp condition without a native run would weaken the external-modification check on `removeCreatedWhileOpen`, which guards a user-visible path. The native tests above settle it; fix after a native run shows the false uncertainty. |
| R7-F01, P3 | OPEN | Outside this remediation's scope (path tokens with three or more spaces). Unchanged. |
| R6-07 / R5-06 | UNVERIFIED | Native Windows is unavailable on this host. Cross-compilation is not a PASS. |
| PRE-01, PRE-02 (P2), R3-02, R3-04, R2-03, F-05..F-08 (P3), R3-01 (accepted) | Carried forward | Unchanged and not worsened. |
| OBS-1..OBS-3 | Observations | Not findings; unchanged. |

### rc3.8 approval recommendation

Recommend that the Owner approve `v0.4-rc3.8` draft 2, SHA256 `4a56762c9b74b5ad2065082f8dc12ad9fd12ca93ab70b56ee510461b0971da9b`. The semantic change from the approved rc3.7 is limited to §4.6 scenario F and its supplement: whole-batch validation, per-file commit in canonical order, stop at first failure, no automatic compensation of published files, no-replace publication only for new names, per-file receipts with trusted revisions only for verified content, `outcome_unknown` and `;cm=unknown` for failures with known or possible effects, and the four R7R2 rules (alias refusal, `failed_before_write` for prepare-time conflicts, silent PRE-02 race, blind-replay duplication). Everything else in draft 2 versus rc3.7 is metadata or historical annotation. If the Owner rejects it, ADR-0024 §8 still documents what the code does, and the spec reverts to rc3.7 plus an explicit contradiction notice.

### R7R2 verification

All runs on Darwin ARM64, Go 1.26.5, from `e37976d` plus the uncommitted R7R2 tests and documents that the final commit adds. Each log records its command and exit code.

| Surface | Result |
|---|---|
| `gofmt -l` on the 8 changed or new Go files | No output, exit 0 ([log](evidence/adr-0024-r7r2/logs/20-gofmt.txt)) |
| `go build ./...`, `go vet ./...` | exit 0 ([build](evidence/adr-0024-r7r2/logs/21-build.txt), [vet](evidence/adr-0024-r7r2/logs/22-vet.txt)) |
| `go test -count=1` editing, toolset, agent, session | All `ok`, exit 0 ([log](evidence/adr-0024-r7r2/logs/23-test.txt)) |
| Same packages with `-race` | All `ok`, exit 0 ([log](evidence/adr-0024-r7r2/logs/24-race.txt)) |
| `go test -race -count=1 -run 'TestT3\|TestT4' ./runtime` | `ok`, exit 0 ([log](evidence/adr-0024-r7r2/logs/25-runtime-t3t4-race.txt)) |
| Sol's original probes, R7 mode, via the committed `review_probes.py` | `gap exit 0`, `jsonpath exit 0`, script exit 0 ([log](evidence/adr-0024-r7r2/logs/26-sol-r7-replay.txt)) |
| Focused race run `^TestR7\|TestR5ResultPropagation…\|TestR6Nested…\|TestR6StructuredPaths…` | 104 PASS lines, 0 FAIL, exit 0 ([log](evidence/adr-0024-r7r2/logs/27-focused-race.txt)) |
| `go vet` of the four packages for linux/arm64, linux/amd64, windows/amd64, windows/arm64, freebsd/amd64 (CGO disabled) | exit 0 each (`logs/28-cross-*.txt`). Windows test binaries compiled ([log](evidence/adr-0024-r7r2/logs/10-windows-cross.txt)); none was run. |

Not run for R7R2: the repository-wide `go test ./...` and the full 45-minute `./runtime` package. Packages that import `editing` or `toolset`, such as `runtime`, are covered only by the T3/T4 subset above. No native Windows, Linux or FreeBSD execution.

## R7R2 Carbon P3 remediation

**Status:** **`R7R2_P3_REMEDIATION_COMPLETE_PENDING_INDEPENDENT_REVIEW`** (assigned for independent review by Claude Opus 5.5).

- **Verified baseline:** R7R2 branch `fix/adr-0024-r7r2-remediation-20261121` @ `fc1c4368507bea3f4e13099c62ca35936a6fba48` (clean, synced to origin). R7 (`31172bc274c3a4c54243a72e554862d539d7759d`), R6 (`4aaf8e7168743d6c8ae8fa6fe87a350ca69f9af2`), original `main` (`c73d17359f5d9dd16ef88b0de4525862593fddaa` keeping `M AGENTS.md`), `docs/reports/evidence/adr-0024-r7/`, and `docs/reports/evidence/adr-0024-r7r2/` remain untouched.
- **Specification governance:** `v0.4-rc3.8 draft 2` (`4a56762c9b74b5ad2065082f8dc12ad9fd12ca93ab70b56ee510461b0971da9b`) was NOT modified and remains **`PENDING_OWNER_APPROVAL`**.
- **Branch & commits:** `fix/adr-0024-r7r2-carbon-p3-20261121` in `wt/core-adr-0024-r7r2-carbon-p3-20261121`:
  1. `8d8420684e75ec175dce19db0848a4f08a74449e` — `fix(toolset): preserve failed rows in overflow fallback` (`R7R2-NEW-01`)
  2. `d8e2d719ea4e03e8ceb1258593b16ecee86dd8da` — `test(editing): make cross-process create race deterministic` (`R7R2-NEW-02`; frozen code/test candidate)
  3. Final commit on this branch adds only documentation and [`evidence/adr-0024-r7r2-carbon-p3/`](evidence/adr-0024-r7r2-carbon-p3/).
- **Confirmation of production boundary:** [production.diff](evidence/adr-0024-r7r2-carbon-p3/production.diff) modifies **only** `internal/toolset/editing.go` (`+55/-22`). **Zero** production code was modified for `R7R2-NEW-02` (no SQLite schema, Cognitive Core, `MutationEvidence`, or `PRE-01`/`PRE-02` behavioral changes).
- **Evidence manifests:** `docs/reports/evidence/adr-0024-r7r2/SHA256SUMS` remains untouched and corresponds to `fc1c4368507bea3f4e13099c62ca35936a6fba48` (`git show fc1c4368:<path> | shasum -a 256` for the three amended doc files). Check all R7R2 Carbon P3 artifacts from the Core root with `shasum -a 256 -c docs/reports/evidence/adr-0024-r7r2-carbon-p3/SHA256SUMS`.

### Remediated findings (both `FIXED_BY_IMPLEMENTER / SELF_VERIFIED / PENDING_INDEPENDENT_REVIEW`)

| Finding | Root cause and fix | RED → GREEN verification |
|---|---|---|
| `R7R2-NEW-01` (P3) | In `internal/toolset/editing.go` (`formatBoundedEditError`), the extreme `overflow` fallback (`len > 4096`) nested `count` (`[fileFloor, 1]`) inside `limit` (`[1024, 256]`), so 5 failed rows with `~780B` dirs and bare `syscall.EIO` returned `(1024, 1)` instead of shortening failed-row paths first. Fixed with `renderOverflow`: preserves all `fileFloor` rows while front-truncating paths (`1024 → 512 → 256 → 128 → 96`, retaining filenames/tail directories), and only if `fileFloor` rows physically cannot fit in `4096B` binary-searches the maximum fitting failed-row prefix across `{128, 96, 64}` while preserving `filesOmitted` and `filesOmittedByState`. | `TestR7R2New01FiveFailedLongDirOverflowKeepsAllFailedIdentities` and `TestR7R2New01OverflowBoundaryMatrix` ([RED](evidence/adr-0024-r7r2-carbon-p3/logs/01-red-new01-toolset.txt) `exit=1`, `len=1`; [GREEN](evidence/adr-0024-r7r2-carbon-p3/logs/02-green-new01-toolset.txt) `exit=0`, all 5/5 failed rows visible, 8/8 visible, 50-row impossible-to-fit retains max prefix + omission breakdown). [Original Carbon probe GREEN](evidence/adr-0024-r7r2-carbon-p3/logs/03-carbon-original-probe-green.txt): `multi_5_longdir` now `rows=5`, `len=3268 <= 4096`. |
| `R7R2-NEW-02` (P3, test-only) | `TestR201TwoOSProcessesCreateRace` (`internal/editing/remediation_r2_01_test.go`) raced a two-syscall child (`OpenFile(O_CREAT\|O_EXCL)` + `Write(foreign)`) against `AllowCreate: true` and asserted `DirtyFailure == false` on all failures. When the child creates a 0-byte inode before `Engine.prepare`, `Engine` enters `writeInPlace` (`Created=false, Staged=false, OldSHA256=sha256("")`) and accurately sets `DirtyFailure=true` (`CodeConcurrentWrite`, `external_modification_detected`) if `Write(foreign)` overlaps before `verifyActual`. Replaced guesswork `DirtyFailure == false` with four deterministic IPC-barrier subtests (`outcome_a`, `outcome_b1`, `outcome_b2`, `outcome_b3`) asserting all 11 invariants, plus 30-round `concurrent_exclusive_create_stress` (`Create: true` vs `race_once`) and 30-round `concurrent_atomic_allow_create_stress` (`AllowCreate: true` vs `atomic_race_once`). **Zero production changes.** Note: an intermediate `-count=35` run with uncoordinated two-syscall `race_once` under `AllowCreate: true` observed `2 / 1,050` rounds hit the pre-existing `OPEN P2 PRE-02` `Truncate(0) → child.Write(44B) → LC.Write(25B)` descriptor window (`"agent os process content\nD 0123456789ABCDEF\n"`, caught as `CodeConcurrentWrite` + `DirtyFailure=true`), motivating separation of `atomic_race_once` vs deterministic `O_EXCL + Write` barriers rather than weakening the mixed-bytes assertion or changing `PRE-02`. | [Deterministic Round-8 RED](evidence/adr-0024-r7r2-carbon-p3/logs/04-red-new02-editing.txt) (`exit=1`); [`go test -count=35 -run ^TestR201TwoOSProcessesCreateRace$ ./internal/editing`](evidence/adr-0024-r7r2-carbon-p3/logs/05-green-new02-count35.txt) (`exit=0`, 35/35 runs PASS). |

### Verification summary at `d8e2d719ea4e03e8ceb1258593b16ecee86dd8da` (Darwin ARM64, Go 1.26.5)

- [`06-gofmt-build-vet.txt`](evidence/adr-0024-r7r2-carbon-p3/logs/06-gofmt-build-vet.txt): `gofmt -l`, `go build ./...`, `go vet ./...` (`exit=0`).
- [`07-test-toolset-editing.txt`](evidence/adr-0024-r7r2-carbon-p3/logs/07-test-toolset-editing.txt): `go test -count=1 ./internal/toolset ./internal/editing` (`exit=0`).
- [`08-race-toolset-editing.txt`](evidence/adr-0024-r7r2-carbon-p3/logs/08-race-toolset-editing.txt): `go test -race -count=1 ./internal/toolset ./internal/editing` (`exit=0`).
- [`09-race-agent-session.txt`](evidence/adr-0024-r7r2-carbon-p3/logs/09-race-agent-session.txt): `go test -race -count=1 ./internal/agent ./internal/session` (`exit=0`).
- [`10-windows-cross.txt`](evidence/adr-0024-r7r2-carbon-p3/logs/10-windows-cross.txt): `GOOS=windows GOARCH={amd64,arm64} CGO_ENABLED=0` `go vet` and `go test -c` (`exit=0`; cross-compilation only, NOT native execution).
- Not run on this branch: full-repo `go test ./...`, extended `./runtime`, or native Windows/Linux/FreeBSD execution.

### Windows native handoff & preserved P2/P3 ledger

- **`BASE-01 / R6-07` stays `OPEN / UNVERIFIED` on native Windows.** Run on Windows Server 2019 or Windows 11 NTFS (`go1.26.5 windows/amd64`):
  ```powershell
  go test -v -count=1 -run "^(TestR7R2|TestR7)" ./internal/editing
  go test -v -count=1 -run "^TestR7R2" ./internal/toolset
  go test -race -count=1 ./internal/editing ./internal/toolset ./internal/agent ./internal/session
  ```
- **Preserved ledger:** `PRE-01` (OPEN P2), `PRE-02` (OPEN P2, silent race disclosed), `BASE-01`/`R6-07` (OPEN/UNVERIFIED), `R3-01` (Owner-accepted exFAT limit), `CT-02` (OPEN P3 documented blind-replay limit), `R7-F01` (OPEN P3 `>=3`-space display path tail), `R3-02`, `R3-04`, `R2-03`, `F-05`..`F-08` (OPEN P3).

## R7R2 final audit and remediation

**Status:** **`R7R2_OPUS_REMEDIATION_COMPLETE_PENDING_INDEPENDENT_REVIEW`**. Claude Opus 5.5 audited Carbon's two fixes, found four further defects and fixed them. Every change below is **`FIXED_BY_IMPLEMENTER` / `SELF_VERIFIED` / `PENDING_INDEPENDENT_REVIEW`**; none is independently closed. GPT-6.1 Sol performs the independent review.

- **Baseline, verified before any change:** `fix/adr-0024-r7r2-carbon-p3-20261121` at `f3f77e35c98de4647243019568ed5ec3093cf072`, clean and equal to its remote. `fc1c436`, `31172bc`, `c73d173`, `8d84206` and `d8e2d71` are its ancestors. Carbon's manifest verified 17/17. Its production diff is `internal/toolset/editing.go` only; `internal/editing` production code is identical at `fc1c436` and `f3f77e3`. The main worktree still holds only `M AGENTS.md`.
- **Branch and commits:** `fix/adr-0024-r7r2-opus-final-20261121`, worktree `wt/core-adr-0024-r7r2-opus-final-20261121`.
  1. `1679fc1634b71744b899661358ec8d38a487a09d` — `fix(toolset): keep failed rows that fit and tell same-named rows apart` (R7R2-FINAL-01, -02, -03).
  2. `fa909ad555f8ba42a7a910cd12f6bf153ea645ae` — `test(editing): race the two-process create for real and name every outcome` (R7R2-FINAL-04; frozen code and test candidate).
  3. The last commit on the branch adds this section, one sentence in each ADR and [`evidence/adr-0024-r7r2-final/`](evidence/adr-0024-r7r2-final/).
- **Production boundary:** [production.diff](evidence/adr-0024-r7r2-final/production.diff) changes `internal/toolset/editing.go` only (`+157/-53`). No file under `internal/editing` other than a test changed, so NEW-02 and PRE-02 behavior are untouched. No SQLite migration, schema change, second ledger or public API change.
- **Specification:** `v0.4-rc3.8` draft 2, SHA256 `4a56762c9b74b5ad2065082f8dc12ad9fd12ca93ab70b56ee510461b0971da9b`, was read and not modified. It remains `PENDING_OWNER_APPROVAL`.
- **Replay:** `bash docs/reports/evidence/adr-0024-r7r2-final/verify.sh <mode>` from the Core root. Each log below names its mode. The script writes only to a temporary directory.

### Verdict on Carbon's remediation

| Finding | Verdict | Basis |
|---|---|---|
| `R7R2-NEW-01`, the finding | **CONFIRMED** | Carbon's regression tests against the `fc1c436` file: five failed rows come back as one ([log 01](evidence/adr-0024-r7r2-final/logs/01-new01-original-red.txt), exit 1). |
| `R7R2-NEW-01`, Carbon's fix | **PARTIALLY_CONFIRMED** | The reported case is fixed at `f3f77e3`: Carbon's original probe, archived here because its source was not committed, shows `multi_5_longdir rows=5` against that commit's file ([log 04a](evidence/adr-0024-r7r2-final/logs/04a-carbon-original-probe-at-carbon.txt)), and all 17 of its subtests pass on this branch ([log 04](evidence/adr-0024-r7r2-final/logs/04-carbon-original-probe.txt)). Three defects remained in the same fallback: R7R2-FINAL-01, -02 and -03. |
| `R7R2-NEW-02`, the finding | **CONFIRMED** | The old assertion rejects a correct `DirtyFailure=true`: Carbon's reproducer, also archived here, fails as it should ([log 07](evidence/adr-0024-r7r2-final/logs/07-new02-original.txt)). The flake is rare: the old test passed 35 of 35 runs, 1050 rounds, on this host. |
| `R7R2-NEW-02`, Carbon's fix | **PARTIALLY_CONFIRMED** | The four barrier subtests are correct, the test passes 35/35, and no production code changed. The two stress subtests did not race: R7R2-FINAL-04. |

The function Carbon's row calls `formatBoundedEditError` is `formatCommitFailurePayload`; no Go file in this branch's history holds the other name.

The ten points the audit had to check for NEW-01:

| # | Point | At `f3f77e3` | At `fa909ad` |
|---|---|---|---|
| 1 | Every failed row is kept when it fits | No: 20 failed rows that fit at a 64-byte path came back as 15 | Yes (FINAL-01) |
| 2 | Verified and untouched rows go before failed rows | Yes | Yes, unchanged |
| 3 | A shortened path keeps its filename | Yes | Yes, unchanged; the tail a row keeps is the same |
| 4 | One filename in several directories stays distinguishable | Only when the directory that differs sits inside the kept tail; otherwise five failed rows read as one text | Yes when the segment that tells the paths apart is at most 48 bytes (FINAL-02) |
| 5 | Unicode paths stay valid | Yes | Yes; new cases cover multi-byte names that share leading bytes |
| 6 | Error classification is kept | Yes: `code`, `commitState`, per-row `publishState` | Yes, unchanged |
| 7 | Omission counts are exact | Yes | Yes; checked per state against the omitted rows |
| 8 | The fallback keeps the longest failed-row prefix | No: it returned the prefix that fits at 128 bytes, 15 rows where 22 fit | Yes (FINAL-01) |
| 9 | No failed row is lost because a longer path was tried first | No: same cause as point 8 | Yes (FINAL-01) |
| 10 | Valid JSON within 4096 bytes | Yes | Yes; 684 payloads and a worst-case test, none over 4096 |

### Findings raised and fixed in this audit

| Finding | Trigger and impact at `f3f77e3` | Root cause | Change | RED → GREEN |
|---|---|---|---|---|
| `R7R2-FINAL-01`, P3 | 20 to 22 failed files under a deep directory with a path-less error. All of them fit at a 64-byte path, 15 were shown. With 23 or more, 15 were shown where 22 fit. The agent loses failed files that had room. | Carbon's first loop tried every failed row down to 96 bytes. The second searched at 128 bytes first and returned as soon as one row fit, so its 96 and 64 byte budgets could not be reached. This is the inversion of NEW-01 again, one level down. | `1679fc1`: both loops share one list of budgets that ends at 64 bytes. When the rows cannot all fit, the envelope keeps the longest prefix any budget holds; a shorter budget replaces a longer one only by keeping more rows. | [RED](evidence/adr-0024-r7r2-final/logs/02-new01-final-red.txt): `15 failed rows shown, want all 20`, `15 failed rows shown but 16 fit`. [GREEN](evidence/adr-0024-r7r2-final/logs/03-new01-green.txt). The prefix test checks its result against an independent re-encoding of the returned envelope: one more row must not fit. |
| `R7R2-FINAL-02`, P3 | The NEW-01 shape with one filename: five failed `config.yaml` files in five module directories that share a long path behind the module name. All five rows read the same text, the shared tail ending in `/config.yaml`. The same happens to two failed paths over 1024 bytes outside the overflow envelope. The agent sees five states and cannot tell which file has which. | A shortened path keeps only its tail (`truncatePathTail`, R7R2-Results-01). Paths that differ in front of the tail shorten to one text. Carbon's probe logged this as an observation. | `1679fc1`, `distinguishShortenedPaths`: a row whose kept tail also fits another path of the commit gains the segment where its path leaves those paths, and keeps its tail: `module00/…/config.yaml`, `…/svc00/…/gateway/config/AppConfig.kt`. The comparison covers every path of the commit, shown or not. Rows whose tail is unique are untouched. | Same RED log: `TestR7R2FinalSameNameRowsStayDistinct` (five of its six shapes fail; the sixth, 24 services, guards FINAL-01 against collapsing paths that were shown whole), `TestR7R2FinalFiveFailedSameNameFilesStayDistinct` (real files through `apply_patch`), `TestR7R2FinalDistinguishShortenedPaths` (exact forms). GREEN in the same GREEN log. |
| `R7R2-FINAL-03`, P3 | A single-file refusal has no receipt row. When JSON escaping pushes its path past the budget, the overflow envelope held `error`, `code` and no path: nothing named the file, and about 3500 bytes stayed unused. It needs roughly 700 bytes of characters that JSON escapes (`&`, `<`, `>`, control characters), so it is very unlikely in practice. | The envelope carried identity only through rows. | `1679fc1`: a refusal without rows keeps its path, shortened from the front with the same budgets. | Same logs: `overflow envelope does not name the refused file: path=""`, then GREEN. |
| `R7R2-FINAL-04`, P3 (test) | The two stress subtests of `TestR201TwoOSProcessesCreateRace` always ended the same way. In 35 runs, 1050 of 1050 exclusive-create rounds were the pre-flight `CodeExists`, and the atomic rounds were a staged refusal in all but 8. The `AllowCreate` race against a create followed by a write, the scenario the test is named for and the one that produced NEW-02, was no longer run. No test in the four packages reached the branch that reports bytes another writer mixed into LC's write. A mutant that reports those bytes as a clean refusal passed the `editing`, `toolset` and `agent` suites with Carbon's version of the test. | The external process signalled and went on while the test was still waking from a pipe read, so it finished before `Commit` started. Carbon removed the in-place race because the test treated mixed bytes as a failure whatever the receipt said. | `fa909ad`, test only. The external process is let go over a pipe at a named point of the commit, so both sides race from there. Each round is checked against the outcomes that point leaves possible. The in-place race is back. A new deterministic subtest drives the truncate window: the disk holds mixed bytes and LC must report a dirty, unverified failure. Two more cover the exclusive-create refusal and a file LC read whole and replaced. Pipes replace the 100 µs polling loops; nothing waits on a clock. | [Carbon's test, 35 runs with tallies](evidence/adr-0024-r7r2-final/logs/06-new02-carbon-stress.txt). [Coverage](evidence/adr-0024-r7r2-final/logs/12-inplace-mismatch-coverage.txt): no package reached that branch before; the `editing` tests reach it now. [Mutant M4](evidence/adr-0024-r7r2-final/logs/10-new02-mutant-m4.txt): the `editing` suite with Carbon's version of the test passes on it, as do the `toolset` and `agent` suites; the new test fails. [35/35](evidence/adr-0024-r7r2-final/logs/08-new02-count35.txt). |

What FINAL-01 and FINAL-02 change for the agent, measured on 684 rendered payloads ([log 05](evidence/adr-0024-r7r2-final/logs/05-differential.txt)): 482 are byte-identical to `f3f77e3`. 134 show more rows. 56 show the same number of rows, which used to repeat one text. 12 show fewer rows than before: a distinguishing segment costs up to 56 bytes, so a set of same-named files can show, for example, 29 rows that each name their file where 32 rows used to read `…/config.yaml`. The omitted rows are counted by state. No field other than the rows and their omission counts changed in any payload.

A path is now sanitized once per call instead of once per rendering. The same corpus takes 20 s instead of 125 s, and 200 failed rows take 0.3 s instead of 5.7 s.

**Limits of FINAL-02.** A row keeps its plain tail when the bytes that tell it apart span more than 48 bytes, for example two directories that differ at both ends of a long shared middle. `difference_wider_than_the_bound_stays` pins this. The `publishedFiles` list still shortens tail-only; it repeats the rows and is the first field dropped. Scored low and not changed.

### NEW-02 test reliability

`go test -count=35 -run '^TestR201TwoOSProcessesCreateRace$' ./internal/editing`: `ok`, exit 0 ([log 08](evidence/adr-0024-r7r2-final/logs/08-new02-count35.txt)). A longer run: 100 of 100 passes, 20 of 20 under `-race`, 20 of 20 with `GOMAXPROCS=1` ([log 09](evidence/adr-0024-r7r2-final/logs/09-new02-soak.txt)).

The 100 runs hold 13,000 stress rounds, and all nine outcomes occurred ([tally](evidence/adr-0024-r7r2-final/logs/09a-new02-soak-tally.txt)):

| Outcome | Rounds | Meaning |
|---|---|---|
| `staged_publish_refused` | 6290 | External file appeared while LC staged; no-replace publication refused; foreign bytes whole |
| `inplace_overlap_foreign_whole` | 1972 | External write landed after LC's in-place write; dirty failure |
| `lc_replaced_read_foreign` | 1928 | External file was whole when LC read it; LC replaced it and recorded what it read |
| `inplace_refused_before_truncate` | 1002 | External write landed before LC's compare; nothing written |
| `exists_before_prepare` | 942 | `Create:true` found the external file |
| `lc_created` | 817 | LC published; the external create lost with `EEXIST` |
| `pre02_mixed_bytes_reported_dirty` | 21 | PRE-02 window: mixed bytes on disk, reported as a dirty, unverified failure |
| `external_write_after_verified` | 19 | External write landed after LC verified its bytes |
| `pre02_external_write_overwritten` | 9 | PRE-02, silent: see below |

The ten assertions the audit required are made in every round, with the one exception stated below. One creator is identified: LC with `Staged`, `Created` and an external `EEXIST`, or the external process with LC claiming no creation. The bytes on disk are whole, or mixed only together with `DirtyFailure=true`, `external_modification_detected`, no `NewRev` and no `NewSHA256`. A failed commit never registers a snapshot and is never `Committed`. The seal for those receipts is in `internal/agent`: `TestR7R2FinalCreateRaceReceiptsSealUnknown` shows each one seals `MutationUnknown` even with a complete outcome and a clean sandwich. `TestR7PartialCommitAndDiagnosticsColdResume` covers the same rule through SQLite and a cold Resume.

One assertion cannot be made for the last row. "No external content is silently discarded" holds wherever LC can know: staged publication never replaces an existing file, a refused in-place write changes nothing, and an atomically published file is replaced only after LC read it whole. In the nine `pre02_external_write_overwritten` rounds the external write landed between LC's compare and its truncate, and LC reported a verified write. That is PRE-02. The test names and counts these rounds; it does not accept them as correct, and it cannot fail on them without failing on an open finding.

### PRE-02 boundary

PRE-02 stays **OPEN P2**. Nothing here fixes it. The two interleavings inside the truncate window were driven deterministically on R6, R7, R7R2, Carbon's commit and this branch ([log 11](evidence/adr-0024-r7r2-final/logs/11-pre02-baselines.txt)).

| Question | Answer |
|---|---|
| Does the same behavior exist on the previous R7R2 baseline? | Yes. `fc1c436`, `f3f77e3` and `fa909ad` print the same lines. A 44-byte foreign write inside the window leaves `agent os process content\nD 0123456789ABCDEF\n` on disk. An 8-byte one is overwritten. |
| Did Carbon's remediation change production behavior? | No. `git diff fc1c436 f3f77e3 -- internal/editing` shows test files only. Nor did this branch. |
| Does the test keep detecting the risk? | Not at `f3f77e3`: the in-place race was gone and the reporting branch had no coverage. It does now, deterministically (`outcome_b4`) and under the real race (21 rounds above). |
| Are `DirtyFailure` and `MutationUnknown` right for the mixed bytes? | Yes: `committed=false`, `dirtyFailure=true`, `published_with_post_error`, `external_modification_detected`, `verified=false`, no revision, no hash, no snapshot. The receipt seals `MutationUnknown`. |
| Are the mixed bytes reported as uncertain? | Yes, on R7, R7R2, Carbon's commit and this branch. R6 also failed dirty, as `publication_uncertain`. |
| Did R7R2 introduce or worsen it? | No. R7 and R7R2 behave the same, and the window is older than both: R6 shows it. The silent case, a foreign write no longer than LC's, reports `published_verified` on all five commits. |

No new production defect was found in `internal/editing`.

### Verification at `fa909ad555f8ba42a7a910cd12f6bf153ea645ae` (Darwin ARM64, Go 1.26.5, APFS)

Every log was produced with Go sources equal to that commit ([state](evidence/adr-0024-r7r2-final/logs/00-state.txt)). The line `working tree: modified` in the logs is the evidence directory itself, not yet committed while they ran.

| Surface | Result |
|---|---|
| `gofmt -l` on the 4 changed Go files, `go build ./...`, `go vet ./...` | Clean, exit 0 ([log 20](evidence/adr-0024-r7r2-final/logs/20-native.txt)) |
| `go test -count=1 ./internal/toolset ./internal/editing` | `ok`, exit 0 (same log) |
| `go test -race -count=1 ./internal/toolset ./internal/editing` | `ok`, exit 0 (same log) |
| `go test -race -count=1 ./internal/agent ./internal/session` | `ok`, exit 0 (same log) |
| `go test -race -count=1 -run 'TestT3\|TestT4' ./runtime` | `ok`, exit 0 (same log) |
| `go test -count=35 -run '^TestR201TwoOSProcessesCreateRace$' ./internal/editing` | `ok`, exit 0 ([log 08](evidence/adr-0024-r7r2-final/logs/08-new02-count35.txt)) |
| R7 partial-commit, result-propagation and Resume regressions, every `TestR7*` and `TestR201*`, under `-race` | 158 PASS lines, 0 FAIL, exit 0 ([log 21](evidence/adr-0024-r7r2-final/logs/21-focused-race.txt)) |
| `go test -timeout=45m -count=1 ./...` | 43 packages `ok`, 6 without tests, exit 0; `runtime` took 689.5 s ([log 23](evidence/adr-0024-r7r2-final/logs/23-repo.txt)) |
| `go vet` of the four packages for linux/arm64, linux/amd64, windows/amd64, windows/arm64, freebsd/amd64, and the four Windows test binaries | exit 0 each ([log 22](evidence/adr-0024-r7r2-final/logs/22-cross.txt)). Compiled only; none was run. |

Not run: native Windows, Linux or FreeBSD. The `./runtime` coverage measurement was taken at this commit only ([log 12a](evidence/adr-0024-r7r2-final/logs/12a-inplace-mismatch-coverage-runtime.txt)); its tests and the code they reach in `internal/editing` are the same at `f3f77e3`.

### Evidence

Verify from the Core root with `shasum -a 256 -c docs/reports/evidence/adr-0024-r7r2-final/SHA256SUMS`. The manifest covers the changed sources, both ADRs, this report, the production diff, the replay script, the inputs and every log; it excludes itself.

- `inputs/carbon-original-probe-r7r2_results_test.go.txt` and `inputs/carbon-red-new02-remediation_r2_01_test.go.txt` are the two overlay sources behind Carbon's logs 03 and 04. They were not in Carbon's commit. They were copied unchanged from Carbon's review workspace on this host, and their digests are in the manifest.
- `inputs/probes/` holds the audit's own probes, `inputs/mutant-m4.diff.txt` the mutant.
- Earlier manifests describe earlier commits and were not edited. `adr-0024-r7r2-carbon-p3/SHA256SUMS` lists `internal/toolset/editing.go` and `internal/editing/remediation_r2_01_test.go`, which this branch changes: check those two entries, the two ADRs and this report against `f3f77e3` with `git show f3f77e3:<path> | shasum -a 256`. The R7 and R7R2 manifests verify the same way against `31172bc` and `fc1c436`; all three were checked.

### Remaining ledger

| Item | Status |
|---|---|
| `R7R2-FINAL-01`, `-02`, `-03`, `-04` (P3) | Fixed here, self-verified, pending independent review |
| `R7R2-NEW-01`, `R7R2-NEW-02` (P3) | Carbon's fixes kept and completed by the four items above; pending independent review |
| `PRE-01` (P2) | OPEN, unchanged |
| `PRE-02` (P2) | OPEN, unchanged. Now measured: 30 of 13,000 real-race rounds fell in the window, 9 of them silent |
| `R7R2-BASE-01`, `R6-07` / `R5-06` | OPEN / UNVERIFIED on native Windows |
| `R3-01` | Owner-accepted exFAT limit |
| `R7R2-CT-02`, `R7-F01`, `R3-02`, `R3-04`, `R2-03`, `F-05`..`F-08` (P3) | OPEN, unchanged |
| `v0.4-rc3.8` draft 2 | `PENDING_OWNER_APPROVAL`; Owner decision still required |

## SOL-01 / SOL-02 remediation

- **Scope:** Agent-visible commit failure rendering only. No change to publication, receipts, snapshots, mutation evidence, SQLite, Resume, or the specification.
- **Verified base:** `10f135d1f904ab88c6a086319ca5820ea6b9e09c`, equal to `origin/fix/adr-0024-r7r2-opus-final-20261121`; the Go tree equals `fa909ad555f8ba42a7a910cd12f6bf153ea645ae`. The isolated remediation branch is `fix/adr-0024-sol-01-02-remediation-20261122`.
- **Findings (P3):** The fixed path-budget ladder leaves room for more identifiable failed rows in the archived `deep_f32_v00_u00_eio` case (SOL-01). Distinct workspace paths can also have identical shortened labels (SOL-02). Finding owner: Sol; independent closure belongs to a different reviewer.
- **Unchanged gates:** `PRE-01` and `PRE-02` remain OPEN P2. `v0.4-rc3.8` draft 2 remains `PENDING_OWNER_APPROVAL`, SHA256 `4a56762c9b74b5ad2065082f8dc12ad9fd12ca93ab70b56ee510461b0971da9b`.

### Rendering strategy

Index sanitized paths from the whole attempted batch, including the omitted suffix and `publishedFiles`. The renderer considers full paths, unique whole-component tails, and distinguishing parent components, including a root-anchored parent when two components are needed (for example `alpha/…/deep/…/f.kt`). A long parent may keep a UTF-8-safe distinguishing suffix. An ellipsis marks removed text; it is not a literal workspace path to open. Use the shown components to locate the original file. Generated shorthands cannot equal a literal path from the batch. Paths conflated by redaction or invalid UTF-8 are not given invented identities; their rows are omitted and counted by state.

The rich envelope keeps failures before verified and untouched rows, including explicit failure/refusal states that have no error text. It preserves optional diagnostics, error detail, revisions, hashes and `publishedFiles` where space permits. The overflow form keeps error classification, commit flags, recovery guidance, per-file states and accurate omissions, without repeating the published view or retaining optional row details. It selects the cheapest eligible label for each row using its actual JSON byte cost, including escaping. Labels are stable across prefix lengths. Cumulative exact row costs give an upper bound; the renderer descends through feasible prefix lengths including their exact omission metadata. This does not assume that a longer payload always costs more (the last omission of a state removes a key). Longer readable labels are tried only at the winning row count. This maximizes the failed-row prefix within the stated candidate space, not across all possible encodings. A prefix that cannot fit is shortened, not replaced by ambiguous labels. With no failed rows, existing priority behavior for other rows applies.

### RED → GREEN

The [archived Carbon output](evidence/adr-0024-sol-01-02/inputs/deep_f32_v00_u00_eio.carbon.json) had 19 rows at 3,975 bytes; the [archived Opus output](evidence/adr-0024-sol-01-02/inputs/deep_f32_v00_u00_eio.opus.json) had 22 at 3,987 bytes. Replaying the original corpus source against both revisions matched every archived output byte-for-byte. The [exact receipt and result](../../internal/toolset/testdata/adr0024-sol/deep_f32_v00_u00_eio.input.json) were captured immediately before the original probe invoked the renderer. `TestSOL01ArchivedDeep32` uses them as input, not a reconstructed similar fixture. With the same `syscall.EIO`, all 32 rows fit at **4,030 bytes**, without omitted receipt rows, changed states, or fabricated revisions/hashes.

`TestSOL02PathIdentity` failed at the Opus baseline for both a wide distinguishing span and repeated directory names at different depths. The new renderer keeps 3/3 unique rows in the first case (1,240 bytes) and 4/4 in the second (3,350 bytes). Tests also cover identical and similar basenames, long Unicode names with spaces, literal ellipsis paths, redaction collisions across the two result views, accurate omissions under pressure, escaped stand-alone refusals, and pathless cancellation. The exact baseline assertions and final results are in the [RED](evidence/adr-0024-sol-01-02/logs/red.log) and [GREEN](evidence/adr-0024-sol-01-02/logs/green.log) logs.

### Differential corpus

| Comparison (684 paired cases) | Byte-identical | More / fewer rows | More / fewer failed rows | Cases with fewer duplicate labels (`files` / `publishedFiles`) |
|---|---:|---:|---:|---:|
| Carbon → Opus | 482 | 134 / 12 | 134 / 8 | 68 / 0 |
| Carbon → Sol | 450 | 156 / 8 | 142 / 8 | 68 / 4 |
| Opus → Sol | 509 | 106 / 0 | 92 / 0 | 0 / 4 |

All eight fewer-row cases versus Carbon replace its repeated same-basename labels with distinct identities; none is a loss relative to Opus. In the 32-file same-name case Carbon displays 32 rows with one shared label, whereas both Opus and Sol display 29 unique rows and account for the omitted three by state. Across the corpus, `files` emits 850 / 0 / 0 repeated labels and `publishedFiles` emits 4 / 4 / 0 for Carbon / Opus / Sol. No new duplicate label appears in either view. The check resolves every candidate label against the actual input paths and verifies its row fields and omission totals, rather than relying on distinct strings alone.

| Serialized bytes | Min | Median | P95 | Max |
|---|---:|---:|---:|---:|
| Carbon | 166 | 3,901.5 | 4,070 | 4,096 |
| Opus | 166 | 3,875 | 4,075 | 4,096 |
| Sol | 166 | 3,836 | 4,075 | 4,096 |

All 2,052 outputs are valid UTF-8 JSON. See the [per-case measurements](evidence/adr-0024-sol-01-02/corpus.csv) and [comparison log](evidence/adr-0024-sol-01-02/logs/comparison.log).

| Renderer | Median process CPU ms/payload | Allocated MiB/payload | Allocation count/payload |
|---|---:|---:|---:|
| Carbon | 196.00 | 50.39 | 205,159 |
| Opus | 39.76 | 8.18 | 32,197 |
| Sol | 11.67 | 2.88 | 10,014 |

Each of three samples per renderer is one pass over all 684 cases, on an Apple M1 Pro (Darwin arm64, Go 1.26.5, `GOMAXPROCS=2`). CPU is process user + system time divided by case count; allocations are cumulative Go benchmark allocations, not peak memory. Timings are for this synthetic corpus, including display-path resolution but excluding fixture construction and output writes. CPU and allocation totals improve by 71% and 69% respectively versus Opus. Build and test jobs also ran on the host, so elapsed times are noisy. Raw samples: [Carbon](evidence/adr-0024-sol-01-02/logs/bench-carbon.log), [Opus](evidence/adr-0024-sol-01-02/logs/bench-opus.log), [Sol](evidence/adr-0024-sol-01-02/logs/bench-sol.log).

### Validation

| Check (Darwin arm64) | Result |
|---|---|
| `gofmt -w` on the three changed Go files; `go build ./...`; `go vet ./...` | Exit 0 each |
| `go test -count=1 ./internal/toolset`; `go test -race -count=1 ./internal/toolset` | Exit 0 each |
| SOL and R7R2 focused adversarial tests, including 20/23/32/50/100/200 failed rows | Exit 0. The 50/100/200 cases retain 30 failed rows at 4,015/4,021/4,022 bytes, respectively. The independent size oracle verifies that adding the next row does not fit. |
| `bash docs/reports/evidence/adr-0024-r7r2-final/verify.sh carbon-probe` | Exit 0; 17/17 subtests and the existing CT-02 replay observation |
| Targeted race run for R7 Partial Commit, cold Resume, receipt seal and earlier propagation/provenance regressions | Exit 0; 149 PASS lines including subtests. No session test matched the filter. |
| `go test -timeout=45m -count=1 ./...` | Exit 0; 43 packages `ok`, 6 without tests. `runtime` took 755.463 s. |

See the [native log](evidence/adr-0024-sol-01-02/logs/native.log) for exact commands and exit codes. Its initial HEAD is the reviewed base; all four tested source/fixture blob IDs match the code commit.

### Reproduction and handoff

Run `go test -count=1 -v -run '^TestSOL0[12]|^TestR7R2(Final|New01)|^TestR7FailurePayload' ./internal/toolset` from Core. For RED, use `-run '^TestSOL0[12]'` with a Go build overlay replacing only `internal/toolset/editing.go` with the file at `10f135d1f904ab88c6a086319ca5820ea6b9e09c`.

The [replay tool](evidence/adr-0024-sol-01-02/replay.py) takes an existing scratch directory outside Core, a mode (`dump` or `bench`), and a renderer (`carbon`, `opus`, or `sol`). Use `dump sol --capture` to save independent input receipts alongside the outputs. Compare the three output directories and the candidate's inputs with [compare.py](evidence/adr-0024-sol-01-02/compare.py); its fifth argument is a new per-case CSV path. Benchmark mode uses Go 1.24+ and Unix `getrusage`. It uses all 684 cases, prebuilds inputs before timing, and excludes file I/O from the measured rendering loop. No provider calls or filesystem mutation benchmarks are involved.

- **Code and tests:** `fc107ba867e8d4e6ef3c7db975a67c1c210069ac`; [production diff](evidence/adr-0024-sol-01-02/production.diff) confined to `internal/toolset/editing.go` (`git diff -U0 10f135d fc107ba -- internal/toolset/editing.go`). There are no new output fields and no changes to editing/publication or Agent persistence. Earlier evidence directories remain intact; verify each historical manifest against its own commit. Run `shasum -a 256 -c docs/reports/evidence/adr-0024-sol-01-02/SHA256SUMS` from Core.
- **SOL-01 (P3):** `FIXED_BY_IMPLEMENTER / SELF_VERIFIED / PENDING_INDEPENDENT_REVIEW`.
- **SOL-02 (P3):** `FIXED_BY_IMPLEMENTER / SELF_VERIFIED / PENDING_INDEPENDENT_REVIEW`.
- **Carry forward:** PRE-01/PRE-02 OPEN P2; BASE-01/R6-07 (R5-06) OPEN/UNVERIFIED; R7-F01, CT-02, R3-02, R3-04, R2-03 and F-05–F-08 OPEN P3. R3-01 stays Owner-accepted. Native Windows, Linux and FreeBSD were not tested here. Owner specification approval and risk acceptance remain separate from remediation verification.

Handoff status: `SOL_01_02_REMEDIATION_COMPLETE_PENDING_INDEPENDENT_REVIEW`. The implementer does not grant independent closure or merge authorization.

## SOL-01 / SOL-02 independent review and SOL-REV-01 remediation

- **Status:** `SOL_REV_01_REMEDIATION_COMPLETE_PENDING_INDEPENDENT_REVIEW` on `fix/adr-0024-sol-rev-01-opus-20261122`, based on `3d85d06e31ae8b424b2ed3d0b9b94be70627e5ac`. Fix `6588f37a92c43fbc4ce908e07dcaffd747e7ba48`; tests `4eb9b082fa6a5c7d708e2428003ba75efb71d1be` (the tested Go tree). Independent review: Gemini 4 Carbon.
- **Review of Sol's branch (Claude Opus 5.5, read-only):** `PASS_WITH_LIMITATIONS`. SOL-01 `PARTIALLY_CONFIRMED`, SOL-02 `CONFIRMED`; no branch-introduced P0/P1/P2. The review raised SOL-REV-01 (P3); finding owner: Opus. See the archived [review report](evidence/adr-0024-sol-rev-01/inputs/review/REVIEW-adr-0024-sol-01-02-independent-20261122.md); its probes and [digests](evidence/adr-0024-sol-rev-01/inputs/review/REVIEW-SHA256SUMS) are alongside.
- **Scope:** the [production diff](evidence/adr-0024-sol-rev-01/production.diff) is confined to `internal/toolset/editing.go`; tests are in `internal/toolset/r7r2_sol_rev01_test.go`. No change to publication, receipts, mutation evidence, snapshots, SQLite, Resume, public APIs, row priority, the 4,096-byte limit or the specification. PRE-01 and PRE-02 are untouched.

### Root cause

Sol's compact labels shorten directories only: `makeLabel`, the readable tail check in `label` and `identifiable` all keep the filename whole. In the overflow form a long filename costs its full length on every row. Opus's tail labels cut long filenames while staying unique, so Opus kept more identifiable failed rows (review sweep: 21 vs 10 at 246-byte filenames).

### Filename abbreviation

Label grammar: a component that is exactly `…` stands for removed components; `…` inside a component stands for removed bytes of that component (`…abc`, `abc…kt`, `…abc…kt`). Only a filename longer than 32 bytes is abbreviated. Each form keeps at least 16 visible bytes and matches exactly one filename of the attempt; the cheapest encoded form wins, otherwise the whole name stays:

1. `…suffix`: no other filename ends with the suffix. It is anchored at the end, so filename uniqueness suffices.
2. `prefix…ext`: no other component, filename or directory, starts with the prefix; `ext` is a kept extension of at most 8 bytes.
3. `prefix…suffix`: every other filename shares a shorter prefix or a shorter suffix than the kept ones, found in one sweep over (common prefix, common suffix) pairs; the prefix starts no directory component.
4. `…window…ext`: the bytes around the first byte that the nearest sorted names do not share; no other component contains the window.

Forms 3 and 4 run only when the cheaper forms are not already minimal and the attempt's components total at most 256 KiB. Prefix-based forms also avoid directory names, so a reader who lets `…` span `/` still finds one file. A label abbreviates the filename only when the filename is unique in the attempt (`dir/abbrev` or `…/abbrev`). A repeated filename keeps Sol's presence-based parent labels, now with the abbreviation; Sol's tail form keeps the whole filename because its directory adjacency would not survive the loose reading. Outside-workspace and literal-ellipsis paths are never abbreviated. A filename whose JSON exceeds the budget can now be shown abbreviated. Readable labels at the winning row count still use whole filenames.

### RED → GREEN

The five original cases, exactly as defined by the archived review probes (32 or 3 failed rows, bare `syscall.EIO`, dirty `partially_published` receipt, all rows failed):

| Case | Opus | Sol (RED) | New (GREEN) | New bytes / omitted | New labels |
|---|---:|---:|---:|---:|---|
| `src/main/kotlin/pkg/` + `L`×70 + `NNN.kt` | 21 | 19 | **29** | 4,010 / 3 | `…/…LLLLLLLLLL000.kt` |
| same, `L`×100 | 21 | 17 | **29** | 4,010 / 3 | `…/…LLLLLLLLLL000.kt` |
| same, `L`×240 | 21 | 10 | **29** | 4,010 / 3 | `…/…LLLLLLLLLL000.kt` |
| `NNN` + `L`×240 + `.kt` | 20 | 10 | **29** | 4,010 / 3 | `…/000LLLLLLLLLL….kt` |
| three 1,404-byte filenames | 3 | 2 | **3** | 1,286 / 0 | `a/…nnnnnnnnnnnn1.kt` |

Omissions are reported by state (for the 32-row cases: one each of `external_modification_detected`, `partially_modified`, `rollback_failed`). No label repeats or resolves to another file. The [five-case record](evidence/adr-0024-sol-rev-01/logs/five-cases.jsonl) holds the bytes, labels and per-state counts for all three renderers. The new tests fail on Sol's renderer and pass on the fix; see the [native log](evidence/adr-0024-sol-rev-01/logs/native.log).

### Regression matrix

`r7r2_sol_rev01_test.go` covers the five floors and a start/middle/end sweep at eleven lengths. Every sweep case meets the higher of Opus's and Sol's measured row counts. It also covers:

- identical long prefixes and suffixes;
- one long filename in 32 modules and in two siblings;
- Unicode filenames and filenames with spaces;
- long names under 190-byte directories, and repeated directories at different depths;
- case variants;
- a directory that shares a filename's prefix;
- pathless cancellation;
- mixed states, including `failed_before_write` and `unsupported` rows without error text;
- the exact 4,096-byte boundary;
- 200 failed rows;
- an omitted file that shares the shown file's long suffix;
- a filename whose JSON exceeds the budget;
- each abbreviation form, including the whole-name fallback and a directory named like the filename.

Each case asserts:

- attempted and failed counts, exact retained rows, and omissions by state;
- unique labels that resolve to the expected row under both the component and loose readings;
- error classification;
- no revision or hash on unverified rows, and unchanged inputs;
- for overflow payloads, a maximal prefix.

### Differential evidence

| Set | Comparison | Result |
|---|---|---|
| 684-case corpus | Sol → new | 684 byte-identical (longest corpus filename: 27 bytes) |
| 684-case corpus | Opus → new | 509 identical, 106 more rows, 0 fewer; no duplicate or ambiguous label |
| 1,180-case review matrix | Sol → new | 1,170 identical, 10 more failed rows, 0 fewer, no new ambiguity |
| 1,180-case review matrix | Opus → new | 28 more failed rows, 4 fewer |
| 33-case filename sweep | new vs Opus, Sol | more rows than Opus in all 33; at least Sol's rows in all 33 (30 more) |

In the four matrix cases with fewer failed rows than Opus, Opus's extra rows were 16 ambiguous labels (`B_repeated_dirs_40_pressure`, `H_combined_100`) or duplicate redacted labels (`J2`, `J6`). That is unchanged SOL-02 behavior. The only ambiguous label in all three renderers is a literal directory named `…` (`J5`), which is inherent to the notation. A maximal-prefix oracle passes every overflow payload: 348 in the corpus and 43 in the matrix. See [corpus](evidence/adr-0024-sol-rev-01/logs/corpus-compare3.log), [matrix](evidence/adr-0024-sol-rev-01/logs/matrix-compare3.log) and [sweep](evidence/adr-0024-sol-rev-01/logs/sweep-compare3.log) comparisons, the [new](evidence/adr-0024-sol-rev-01/logs/matrix-check-new.log) and [Sol](evidence/adr-0024-sol-rev-01/logs/matrix-check-sol.log) matrix checks, and per-case [tables](evidence/adr-0024-sol-rev-01/tables/).

### Performance

| Renderer (684 corpus payloads) | Median CPU ms/payload | MiB/payload | Allocations/payload |
|---|---:|---:|---:|
| Opus | 28.20 | 8.15 | 32,196 |
| Sol | 8.64 | 2.87 | 10,014 |
| New | 8.68 | 2.87 | 10,024 |

Inputs are prebuilt. Each sample renders all 684 payloads three times; there are five samples per renderer. Runs used `GOMAXPROCS=2` on an idle Apple M1 Pro (Darwin arm64, Go 1.26.5). CPU is process user + system time; allocations are per-payload means. No corpus filename exceeds 32 bytes, so the corpus cost of the change is its length check.

At scale (one failed render, first 40 rows failed, median of three runs), short filenames stay within ±7% of Sol at 100 to 50,000 attempted files. With 126-byte filenames, abbreviation adds:

- 2–3% when the ID is at the end;
- 6–10% when it is at the start;
- 5–26% when it is in the middle: +0.9 ms at 100 files and +7.2 ms at 1,000.

The window check scans at most 256 KiB of component bytes and is skipped above that. Cost stays linear in the number of attempted files, and allocations rise by at most about 1,200 per render. Raw logs: [corpus](evidence/adr-0024-sol-rev-01/logs/bench/) and scale (`scale3-*.log`).

### Validation

| Check (Darwin arm64, HEAD `4eb9b082fa6a5c7d708e2428003ba75efb71d1be`) | Result |
|---|---|
| `gofmt -l` on both changed Go files; `go build ./...`; `go vet ./...` | Exit 0 each |
| `go test -count=1 ./internal/toolset`; `go test -race -count=1 ./internal/toolset` | Exit 0 each |
| `go test -count=1 -v -run '^TestSOLREV01' ./internal/toolset` (GREEN) | Exit 0; 69 PASS lines |
| Same tests with Sol's `editing.go` overlaid (RED) | Exit 1 as expected; 52 FAIL lines across floors, sweep, shapes, cancellation, 200 rows, shared suffix and over-budget JSON. The boundary and mixed-state cases also pass at baseline and act as regression guards |
| SOL-01/SOL-02 and R7R2 focused tests | Exit 0; 40 PASS lines |
| `bash docs/reports/evidence/adr-0024-r7r2-final/verify.sh carbon-probe` | Exit 0; 17/17 |
| R7/R201/R5/R6 race run over `internal/editing`, `toolset`, `agent`, `session` | Exit 0; 149 PASS lines, including cold Resume and the receipt seal |
| `go test -timeout=45m -count=1 ./...` | Exit 0; 43 packages `ok`, 6 without tests; `runtime` took 737 s |
| `git status` before and after | Clean both times |

The [native log](evidence/adr-0024-sol-rev-01/logs/native.log) records the commands, exit codes and the HEAD blob IDs of both changed Go files; the [RED test file](evidence/adr-0024-sol-rev-01/inputs/red-test-file.go.txt) is archived. No native Windows, Linux or FreeBSD run took place.

### Reproduction and handoff

From Core, run `go test -count=1 -v -run '^TestSOLREV01' ./internal/toolset`. For RED, replace only `internal/toolset/editing.go` with the file at `3d85d06e31ae8b424b2ed3d0b9b94be70627e5ac` through a Go build overlay. Drop `TestSOLREV01ShortBaseForms` from that run, since it calls the new helper.

For the differential, export trees with [setup_tree.sh](evidence/adr-0024-sol-rev-01/scripts/setup_tree.sh) (`opus` = `10f135d`, `sol` = `fc107ba`, new = `4eb9b08`). Then run the v2 probes with `REVIEW_DUMP_DIR`/`REVIEW_INPUT_DIR` (corpus), `REVIEW_ADV_IN`/`REVIEW_ADV_OUT` (matrix) or `REVIEW_SWEEP_OUT`/`REVIEW_VARIANT` (sweep). Compare with [compare3.py](evidence/adr-0024-sol-rev-01/scripts/compare3.py), which uses the grammar in [labelgrammar.py](evidence/adr-0024-sol-rev-01/scripts/labelgrammar.py). The matrix inputs are generated by the archived probes. Run `shasum -a 256 -c docs/reports/evidence/adr-0024-sol-rev-01/SHA256SUMS` from Core.

- **SOL-REV-01 (P3):** `FIXED_BY_IMPLEMENTER / SELF_VERIFIED / PENDING_INDEPENDENT_REVIEW`.
- **Carried forward:**
  - PRE-01 and PRE-02 remain OPEN P2.
  - BASE-01/R6-07 (R5-06) remain OPEN/UNVERIFIED.
  - R7-F01, CT-02, R3-02, R3-04, R2-03 and F-05–F-08 remain OPEN P3.
  - R3-01 stays Owner-accepted.
  - The review's SOL-01/SOL-02 limits remain for Owner decision. Directory distinctions that depend only on component order fall back to whole paths, and a literal `…` path is inherently ambiguous.
  - The specification (`v0.4-rc3.8` draft 2, `4a56762c…`) is unchanged and `PENDING_OWNER_APPROVAL`.
  - Native Windows, Linux and FreeBSD were not tested here.

Handoff status: `SOL_REV_01_REMEDIATION_COMPLETE_PENDING_INDEPENDENT_REVIEW`. The implementer does not grant independent closure or merge authorization.

## Final architecture audit of the Opus filename renderer

**Original Opus verdict: REVISE.** The five SOL-REV-01 regressions are fixed. The original branch is not ready to merge: recovery can still identify the wrong file when a literal path contains `…`. There are also three reproducible ways to lose identifiable rows. The read-only audit was completed before opening `fix/adr-0024-sol-rev-01-astra-final-20261122`. Baseline: `52ac7d7df7d582bc2cb95e36618381cf30f7ab80`; production `6588f37a92c43fbc4ce908e07dcaffd747e7ba48`; tested tree `4eb9b082fa6a5c7d708e2428003ba75efb71d1be`. Nothing was written to the reviewed branch.

| ID | Finding on Opus | Attribution |
|---|---|---|
| ASTRA-02, P2, CONFIRMED | A displayed `a/…/f.kt` resolves to the literal path and an omitted file under `a/b/`. A real `a/real…tail.kt` likewise matches `a/realXYZtail.kt`. This is a recovery identity defect, not a filesystem write. | Predates Opus. |
| ASTRA-01, P3, CONFIRMED | The 40-file repeated-directory case retains 3 rows at 4,073 B. An anchored, counted component gap gives 31 unique rows at 4,093 B on the same envelope. The older 30-row result has 16 ambiguous rows. | Inherited from Sol. |
| ASTRA-03, P3, CONFIRMED | The joint filename abbreviation chooses a split by unescaped byte length. With ampersands near the end, Opus keeps 20 failures at 3,989 B; an equally identifiable split keeps 29 at 4,059 B. | Introduced by Opus. |
| ASTRA-04, P3, CONFIRMED | A first compact row requiring at least 4,390 B leaves `files:[]` despite a later identifiable failure fitting at 577 B with omissions. | Inherited from Sol. |

The read-only replay and individual reproducers are documented in the [audit evidence](evidence/adr-0024-astra-final/README.md). The independent measurements apply to the renderer, not to transactions or production workloads. The code changes on this branch are limited to the confirmed findings; prior architecture and risk decisions remain in force.

### Remediation and measured outcome

Finding owner: Astra. The behavioral correction and its black-box RED/GREEN tests are `b1b459ac1e64400913a98f78427f0e7da974d166`. The earlier `0e97b1a4534c27ac54467c6e00eee4115769e290` freezes the initial review. The final tested code is **`52dfd64f56d9b46dfbe7293c787b0f883a501717`**: it also addresses an inherited zero-gap ambiguity (ASTRA-05, P3) and a repeated suffix scan exposed by the first Astra fix (ASTRA-PERF-01, P3). [Additional reproducers and attribution](evidence/adr-0024-astra-final/followup.md). All six findings are **`FIXED_BY_IMPLEMENTER / SELF_VERIFIED / PENDING_INDEPENDENT_REVIEW`**; these statuses do not grant an independent PASS.

| Input (all 32 failed except the last) | Earlier Opus | Sol | Reviewed Opus | Astra | Astra wire bytes / omitted |
|---|---:|---:|---:|---:|---:|
| ID at end, 70-byte prefix | 21 | 19 | 29 | 29 | 4,010 / 3 |
| ID at end, 100-byte prefix | 21 | 17 | 29 | 29 | 4,010 / 3 |
| ID at end, 240-byte prefix | 21 | 10 | 29 | 29 | 4,010 / 3 |
| ID at start, 240-byte padding | 20 | 10 | 29 | 29 | 4,010 / 3 |
| Three 1,404-byte basenames | 3 | 2 | 3 | 3 | 1,286 / 0 |

All four 32-file results omit one `external_modification_detected`, one `partially_modified` and one `rollback_failed`. No failed row acquired a verified revision or hash. The historical deep-32 receipt is a different fixture: all 32 failed rows fit in 4,030 bytes.

**Interpreting labels.** `./a/…/f.kt` is an exact workspace-relative path: remove only the leading `./` if a bare relative path is needed. `…/./tail` removes one or more directories and treats everything after `./` literally. Ordinary `…` still marks omitted text/components. In a repeated-basename whole-path fallback, `r00/…{7}/f.kt` means root `r00`, **exactly seven directories**, then literal `f.kt`. Counts are canonical positive decimal integers of at most four digits. A real directory named `…{7}` gets literal treatment; a candidate spelling a competing actual path is not emitted. Redacted paths remain redacted. These conventions cost two bytes for an exact literal path, require no percent decoding or platform-dependent backslash semantics, and preserve UTF-8. They are display-only; the receipt, result, filesystem and persisted identities are unchanged.

**Stable order and tested optimization.** For the exact 40-file batch, earlier Opus emits 30 rows but 16 do not uniquely identify a file; the reviewed Opus emits 3 unique rows. The remedy emits 31 unique failures in all six permutations, retaining the supplied order of equal-priority rows. In the original order 31 compact rows cost 4,093 bytes and the 32-row prefix costs 4,208 bytes including omissions. With a result-only literal collision added to that batch the conflicting candidate is rejected; 30 identifiable rows remain. There is no global assignment optimizer. Labels are independent because each must resolve against all attempted paths, irrespective of other displayed labels. The bounded candidate search now prices joint filename fragments by escaped JSON cost. A row that cannot fit even by itself with the mandatory envelope no longer prevents trying a later failure. Counts are subtracted by the actual shown indices, including when a row is skipped. We do **not** claim optimality over arbitrary subsets, all filesystem notations or a shared dictionary.

**Every changed matrix case** (Astra vs reviewed Opus): the 40-file case improves 3 to 31; `H_combined_100` improves 8 to 28 failed rows among 110 total records. In `B_repeated_dirs_pressure` and `D_prefix_overlaps_tail_pressure` all four failures stay visible and all four `publishedFiles` compatibility rows now fit (1,580 vs 3,008 bytes). J3 retains 29 rows at 4,067 vs 4,054 bytes without matching the omitted root suffix even under a zero-component interpretation. In J5 only the two-byte literal qualifier changes (4,051 vs 4,049 bytes; 29 rows). No matrix case loses a row. Outside the archived matrix the escaping witness improves 20 to 29 failures at 4,030 bytes (99/128 omitted); the individually blocked case recovers `good.kt` with its state and error at 606 bytes, omitting one external modification and one unattempted file. The unique normal basename under an expensive literal directory now yields both failures at 659 bytes.

| Replay | Reviewed Opus vs Sol | Astra vs reviewed Opus |
|---|---|---|
| 684 historical payloads | 684 identical | 684 identical |
| 1,180 adversarial cases | 1,170 identical; 10 gain failures | 1,174 identical; two gain failures, four improve identity or detail, none lose rows |
| 33 filename sweep cases | 3 identical, 30 gain failures | 33 identical; all remain above earlier Opus |

The independent byte checker enumerated feasible prefixes using the actual compact label choices and mandatory row fields: 348 corpus, 43 matrix and 33 sweep overflow payloads. Under all three readings (whole-component nonempty gaps, component gaps allowed to be empty, loose cross-slash text), all 9,648 primary/283 compatibility rows on the corpus, 16,774/5,331 on the matrix and 941/0 on the sweep resolve against the complete attempted batch. These checks do not prove that arbitrary future inputs are unambiguous. The five original, permutation and collision witnesses include state-specific omissions. The 1,101 boundary-family matrix entries are error-padding variations, **not** 1,101 different path shapes. [Raw changed-case table](evidence/adr-0024-astra-final/changes.tsv).

**Measured cost.** Earlier Opus / Sol / reviewed Opus / final Astra: median CPU **30.18 / 9.09 / 10.08 / 9.86 ms per payload**; memory **8.148 / 2.871 / 2.872 / 2.869 MiB**; allocations **32,196 / 10,014 / 10,024 / 10,025** (same 684 inputs, 3 samples, Go 1.26.5, M1 Pro, `GOMAXPROCS=2`). Final Astra wall times on middle-distinguished names were 4.15 ms at 100, 36.89 ms at 1,000, and 1.85 s at 50,000 attempts. Large repeated-basename groups now compute suffix maxima once from adjacent entries in reverse-lexicographic order. In the all-but-last-impossible stress test this cut the 1,024-row render from 660–664 ms on the first Astra revision to 66–67 ms on final code. Sorting is O(N log N) comparisons; string comparison cost and the older per-group/root path candidate searches still depend on path shape. These numbers establish neither a uniform latency improvement nor universal linear complexity.

**Native gates on the frozen code.** `gofmt -l internal/toolset/editing.go internal/toolset/r7r2_astra_final_test.go` (empty output), `go build ./...`, `go vet ./...`, `go test -count=1 ./internal/toolset`, `go test -race -count=1 ./internal/toolset`, the original 17 archived Carbon subtests, and the focused four-package R7/Resume race run all exited **0**. The same new black-box tests fail behaviorally with only Opus's `editing.go` overlaid (RED **1**); on the final production file GREEN is **0**. The prior purity snapshots for receipts/results and the 4,096-byte and one-byte-over boundary assertions still pass. [Exact commands and raw logs](evidence/adr-0024-astra-final/logs/astra-final-gates.log).

`go test -count=1 ./...` exited **1**: 42 packages passed, six had no tests, and `runtime` hit the default 10-minute **package** timeout. The same package on the previous Opus tree had taken 737.492 s under its archived `-timeout=45m` gate. On final Astra, **`go test -timeout=45m -count=1 ./runtime` exited 0 in 760.557 s**. All 43 packages with tests therefore passed across the two invocations, not in a single green default-timeout full-suite command. [Isolated rerun](evidence/adr-0024-astra-final/logs/runtime-45m.log).

**Unchanged gates.** No transaction, `CommitReceipt`, `MutationEvidence`, resume, public API, database, architecture or Desktop changes. `PRE-01` and `PRE-02` remain **OPEN P2**, not accepted by this audit; `BASE-01 / R6-07` remains OPEN / UNVERIFIED; `CT-02`, `R7-F01`, `R3-02`, `R3-04`, `R2-03` and `F-05` through `F-08` remain open P3; `R3-01` remains Owner-accepted. Native Windows, Linux and FreeBSD remain unverified. `v0.4-rc3.8` draft 2 remains `PENDING_OWNER_APPROVAL`. **Do not merge:** Claude Opus 5.5 must independently verify the Astra changes, then the Owner decides.

Evidence: [SHA256SUMS](evidence/adr-0024-astra-final/SHA256SUMS), 50 entries; manifest SHA256 `75f435e6f22f918ce18ebeb26dd14207d0f3e96af4d77d5998281457c0289d38`. The delivery commit following `52dfd64f56d9b46dfbe7293c787b0f883a501717` changes only this report and audit evidence.

## OV-01..OV-06 remediation (Claude Opus 5.5 as finding owner)

Branch `fix/adr-0024-opus-ov-remediation-20261122` from Astra `a5a1ec6be1081bad4e706944d655a9a9d97f4f70`.

- `11d3222e2a134e705ae9a173e156553ec15be11e` is the label layer (OV-02, OV-03).
- `7e0610fa86bdf0c8001187b4d4985b601603d104` is the payload layer (OV-01, OV-04, OV-05, OV-06) and the tested code.

The production diff is confined to `internal/toolset/editing.go`. There are no transaction, receipt, SQLite, public API, tool-description or Desktop changes. All six findings are **`FIXED_BY_IMPLEMENTER / SELF_VERIFIED / PENDING_INDEPENDENT_REVIEW`**. [Evidence and replay](evidence/adr-0024-ov-remediation/README.md).

**Design.** The overflow fallback is now one pipeline:

1. Classify rows by priority.
2. Index the whole batch.
3. Generate identity-safe compact labels.
4. Price each row by its exact JSON bytes.
5. Per class, choose the largest feasible count with an exact DP over publish states. Ties go to the smallest set by sanitized path. Lower classes use only leftover budget.
6. Size with mandatory omission metadata and the `pathNotation` legend included.
7. Render in priority/original order.

`truncated` now means removed content only. The legend lists exactly the notation forms present: `…/` for one or more directories, `…` inside a name for characters, `…{N}/` for exactly N directories, `…/./x` for omitted directories followed by an exact tail, and `./x` for an exact path. It also says shortened labels are IDs, to be matched against the paths the agent sent.

**Verification.** All gates pass, including a single `go test -timeout=45m -count=1 ./...` (exit 0, 773 s). The new behavioural tests fail on the base renderer. An independent oracle found 0 problems across 2,229 renders and 52,891 labels.

**Visible trade-offs.**
- The legend costs about 164–200 bytes. 38 of the 684 corpus payloads, the five long-filename cases (29 → 28) and the 40-file batch (31 → 30) therefore show one or two fewer failed rows.
- In exchange, the 40-file selection is identical in all 60 orders. The metadata band never hides a fitting row. The large-row witness shows 34 rows instead of 20. Impossible first rows no longer hide later rows of any class.
- Realistic render cost stays linear. Up to 256 candidates per class are labelled, so mid-size long-name batches cost up to 2.8× more (103 ms at 1,024 rows). The adversarial 1,024-row case drops from 7.8 s to 0.27 s.

**Owner decision.** The legend could instead live in tool descriptions, which costs no payload bytes but is outside this scope. The legend stays in the payload until the Owner decides.

**Unchanged.** PRE-01 and PRE-02 remain OPEN P2. BASE-01/R6-07 remains OPEN/UNVERIFIED. The historical P3 ledger is unchanged. Native Windows, Linux and FreeBSD remain unverified, and rc3.8 draft 2 remains `PENDING_OWNER_APPROVAL`.

**Status:** `OV_REMEDIATION_COMPLETE_PENDING_INDEPENDENT_REVIEW`. Gemini 4 Carbon must verify independently. Do not merge.
