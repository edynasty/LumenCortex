# ADR-0024: Candidate-specific context projection, causal repair floors, and deterministic budget guards

- **Status:** Accepted for implementation from approved specification `v0.4-rc3.5`; merge/release approval remains with Owner.
- **Date:** 2026-11-18
- **Owners:** Astra, Context Space implementation owner
- **Authoritative Specification:** `LumenCortex-architecture-audit/design/handoffs/context-space-architecture-master.zh-CN.md` (`v0.4-rc3.5`, SHA256 `bd8dfb00c9781d4d375ce4779789a71f0cbef9f547918ea1f4aa1a0c7481abd9`)
- **Source Baseline:** Core `c73d17359f5d9dd16ef88b0de4525862593fddaa`
- **Supersedes:** Single-model in-place `presentSpecs` / single-`base` pre-screening in `internal/agent` and `32 KiB` skill-catalog omission in `internal/skills`; no historical database rows or evidence records are rewritten.
- **Superseded by:** none

> **Implementation Scope:** This ADR records the post-approval implementation contract of the approved Context Space Architecture (`v0.4-rc3.5`). It maps settled design invariants onto Go runtime wiring without reopening architectural design.

## 1. Context and Problem

At baseline `c73d17359f5d9dd16ef88b0de4525862593fddaa`, LumenCortex provides Cognitive Core v1.0 (`ADR-0023`), request provenance and source fences (`ADR-0018`), execution budgets (`ADR-0019`), workspace trust (`ADR-0013`), and atomic group windowing (`internal/window`). Six production gaps remain on the projection path from durable state to provider requests:

1. **Cross-candidate pollution and premature elimination:** `Loop.Run` (`internal/agent/loop.go:313-336`) mutates `specs` in place and computes a single `base`, disqualifying `32K` failover models behind `200K` primaries or polluting `200K` backups, while `requestRefs` (`internal/agent/envelope.go:84-119`) hashes raw `opts.SystemLayers` instead of transmitted layers.
2. **Optimistic token estimates and unbounded gate feedback:** `(len(json)+2)/3 + 4` (`internal/window/estimate.go:19-35`) underestimates dense JSON/punctuation, and `presentFeedback` (`internal/agent/presentation.go:63-94`) leaves `ProfileDetailed` gate rejections unbounded, allowing rejected proposals plus gate output to evict the edits under repair.
3. **Silent eviction of repair context and `TurnBoundary` progress:** `window.Build` can fit a tiny `[OriginCompletionBlocked]` or `[OriginTurn]` suffix while dropping preceding code-change or tool-output groups; `TurnBoundary` (`internal/agent/send.go:183-194`) commits durable `OriginTurn` rows before capacity verification, and `BeforeModelCall` steer injection (`send.go:302-326`) leaves `req.Echo` stale.
4. **Causal authorship gaps in snapshots (`RC34-F01`, `RC34-F02`):** `WorkspaceSnapshot` (`git status`) excludes `.gitignore` files and lacks pre-blobs, so `PreRef == PostRef` never proves zero mutation. Conversely, `SnapshotFiles` runs outside `editing.Engine.Commit` path locks: if a host edits `A -> B` and an unprimed or resumed agent calls `write_file(B)` (`p.next == p.original`, `wrote == false` at `internal/editing/engine.go:527-529`), `PreRef(A) != PostRef(B)` falsely credits the host's edit to the agent unless Commit disk-write receipts are bound per `ExecutionID` and sealed atomically.
5. **Skill catalog omission and mid-run torn reads (`REV-F02`, `RC33-F02`):** `skills.Registry.Prompt` (`internal/skills/registry.go:305-311`) drops enabled skills past `32 KiB` from `Included` (blocking both discovery and `read_skill`), and `ReadSkill` re-reads `SKILL.md` from disk without a run snapshot or registry `RWMutex`.
6. **Missing or lossy model identity on envelopes (`RC3-NEW-06`, `REV-F03`, `REV-F05`, `RC33-F03`):** `request_envelopes` forbids `decision_json` when `state != 'answered'`, `requestRefs` omits `ModelRef` and includes unrendered `TaskState` items that crowd out the `128`-ref ceiling, and unsealed chunks risk partial decoding if truncated.

## 2. Goals and Non-Goals

- **Goals:** (1) Immutable per-step Canonical Context; (2) Stateless Candidate-specific Projection across `{ModeFull, ModeCompact}`; (3) Demand-Driven Monotonic Projection without cross-step compact lock-in; (4) Unified integer token budget (`EstimateBytes`, `ReserveTokensForBytes`, `GuardTokens`, `BaseGateSlack`, `EffectiveGateSlack`, `OutputBudget`, `UsableInput`); (5) Repair Context protection (`MinRequiredSeq`, `r.groups` clamp, `TurnBoundary` dry-run & minimum-progress guard, controlled `KindWindowOverflow`); (6) Three-valued causal `MutationEvidence` with per-`ExecutionID` `CommitReceipt` and `;cm=proven|none|unknown` seals; (7) Concurrency-safe Run-level `SkillSnapshot` and dual-mode `read_skill`; (8) `RequestEnvelope` v2 lossless `ModelRef` chunking and `10+7+111=128` slot priority; (9) Bit-identical deterministic `Resume`.
- **Non-Goals:** Altering Cognitive Core (`ADR-0023`) truth/privacy/Governor semantics; adding a second Memory, Transcript, Context Manager, or file ledger; adding any SQLite migration (`0018+`) or `agent_steps` schema change; bundling proprietary BPE tokenizers; mutating `TaskState.WindowStartSeq` during runs; offline `Full Projection Replay` of ephemeral reasoning `Echo`.

## 3. Two-Plane Orthogonal Architecture

- **Plane A — Truth & Governance (`internal/session`, `internal/taskstate`, `internal/cognition`, `internal/evaluator`, `internal/verify`):** Owns `session_messages`, `request_envelopes`, `derived_origins` (carrying `;cm=*`), `source_fences`, versioned `TaskState` (`WindowStartSeq` read-only), cognitive graph/Experience, and completion gates.
- **Plane B — Budgeted Projection (`runtime`, `internal/agent`, `internal/window`, `internal/skills`, `internal/toolset`):** Computes a read-only candidate view per attempt (`ModeFull`/`ModeCompact` prefixes + contiguous transcript suffix). Plane B never modifies Plane A authority, policy, budgets, cognitive facts, or provenance.

## 4. Option F2: Binary `ModeFull / ModeCompact` and Demand-Driven Projection

LumenCortex adopts **Option F2**—a stateless binary `{ModeFull, ModeCompact}` selector (`WindowPrefixDecision`, `RuleWindowPrefix`) evaluated in strict order:
1. **`candidate_profile` (`ModeCompact`):** `toolPref == config.ProfileCompact` (highest priority).
2. **`fixed_prefix_budget` (`ModeCompact`):** `toolPref == ""` and `fullFixed > usableFull / 2` (skipped when `toolPref == config.ProfileDetailed`).
3. **`newest_group_fit` (`ModeCompact`):** `fullFixed + requiredTranscript > usableFull` and `compactFixed + requiredTranscript <= usableCompact` (single-step demand fallback for `""` and `ProfileDetailed`; reverts to `ModeFull` on later steps once required suffix shrinks).
4. **`fits_full` (`ModeFull`):** Otherwise (if `compactFixed + requiredTranscript > usableCompact`, Step 7 returns `window.ErrOverflow`).

Prefix caching is distinguished across four layers: **Prefix Stability** (byte-stable `[Tools, System]` per candidate in steady state), **Prefix Identity** (`ToolSpecsDigest` + `sys:*`), **Provider Cache Eligibility** (OpenAI-compatible automatic prefix match; Anthropic wire sends no `cache_control`), and **Actual Cache Hit** (`UNVERIFIED` performance byproduct).

## 5. Candidate-Specific Projection and Governance Boundary

- **Governance boundary:** Run-level `Policy`, `Delegation`, `ToolAllowlist/Denylist`, `Workflow`, `MaxSteps`, `MaxToolCallsPerStep`, `Budget`, `Deadline`, and `Verification` lock once in `Engine.prepareRun` and never change on failover. Each `agent.Model` carries its own `Context`, `Output`, `ToolCalling`, `Efforts`, `Price`, `ProviderPkg`, and `Presentation` (`ToolDescriptions`, `Feedback`).
- **Static pre-screen (`CandidatesByStaticPrefix`):** Checks only `ToolCalling`, advisory health, and `c.Context >= compactFixed(c) + GuardTokens(c) + OutputBudget(maxTokens, c)`.
- **7-step acyclic `projectCandidate`:** (1) Build fresh `Full` and `Compact` prefixes (`SentLayers`, `System`, `Tools`); (2) Compute `fullFixed`, `compactFixed`, `outBudget`, `guard`, `poolBeforeSlack`, `requiredRepairTokens`, `requiredTranscript`; (3) Compute `baseSlackFull/Compact`; (4) Compute `effSlackFull/Compact`; (5) Apply `r.transcriptCap[cand.Ref]` to obtain `marginFull/Compact` and `usableFull/Compact`; (6) Run `WindowPrefixDecision`; (7) Execute `window.Build` once on the selected mode, enforce `built.FirstSeq <= minRequiredSeq`, and compute `r.requestEcho(built, cand.Ref)`.
- **Compact rules:** `<globalDir>/AGENTS.md` is never truncated in projection and `toolset/path.go` sandboxing is unchanged. Trusted project instructions `> 12 KiB` emit first `6 KiB` + CommonMark fence-aware 1-based heading/200-line outline + last `2 KiB` + `read_file(..., anchors=true)` hint. Budget `ModeCompact` keeps 100% of `builtin/task/delegation` and non-read-only external tool descriptions, preserves `(MCP server: <id>)`, and retains read-only `mcp:*/lsp` code fences unless all four positive pure-example criteria hold. Explicit `ProfileCompact` (`REV-F06`) preserves safety/paging invariants on `read_file`, `write_file`, `replace_in_file`, `edit_lines`, `bash`, and `eval`.

## 6. Context Budget and Serialization Guards

- **`EstimateBytes(b []byte) int`:** `0` if empty; else `floor((4*A + 7*S + 6*U + 11)/12) + 4` (`A`: ASCII letters/space; `S`: other ASCII `<0x80`; `U`: `>=0x80`), shared by `window.Estimate` and `provider/{anthropic,openai}` `Echo`. `EchoTokens` is counted once in `GroupTokens`, zeroed when a `user` row closes the turn, and never double-counted.
- **`ReserveTokensForBytes(n int) int`:** `0` for `n <= 0`; else `floor((7*n + 11)/12) + 4` (`1456 -> 854`, `2458 -> 1438`, `12346 -> 7206`), upper-bounding `EstimateBytes` for any slice `<= n` bytes.
- **`presentFeedback` dual guard:** Normalizes UTF-8/newlines, strips control bytes except `\n\t`, caps raw UTF-8 at `1200B` (`compact`) / `6144B` (`detailed`), and caps marshaled `OriginCompletionBlocked` JSON at `MaxCompactGateJSONBytes = 2458` / `MaxDetailedGateJSONBytes = 12346` (`GateMessageEmptyJSONBytes = 58`).
- **Margins:** `GuardTokens = max(ctx/16, 512) + 512*[pkg=="anthropic-messages" && nTools>0]`; `BaseGateSlack = 1950` (`compact`) / `7718` (`detailed`, including `ProposalReserveTokens = 512`); `EffectiveGateSlack` is `baseSlack` when inactive (clamped in `ModeCompact` to `poolBeforeSlack - compactFixed - requiredTranscript` when `0 <= room < baseSlackCompact` so small-window candidates that pass static pre-screening reserve all remaining room without false Step-1 overflow), `0` when `repair.Active && repair.NewestIsBlock`, and `min(baseSlack, max(0, poolBeforeSlack - candFixed - requiredRepairTokens))` during multi-step repair. `CapacityRetry` raises per-candidate margin via `r.transcriptCap[cand.Ref]` without mutating raw budgets.

## 7. Window Selection and Repair Context Protection

- **`deriveRepairContext`:** Forward-monotonic scan after the latest external user row (`Origin == ""` or `OriginImported`). Locks `preBlockFloor` on the earliest `MutationProven` or `MutationUnknown` tool group before the first `OriginCompletionBlocked`; across repeated blocks, advances `preBlockFloor` **only** when `postBlockProven == true` occurred after the prior block. When `postBlockProven == true` after the latest block, `FloorIdx = activeBlock`.
- **`r.groups(maxContext, minKeepSeq)`:** Clamps `keep` so no group with `Seq >= minKeepSeq` is trimmed from memory.
- **Dry-run `TurnBoundary` & `BeforeModelCall`:** Pre-check clones `r.transcript`, zeroes trailing `EchoTokens`, appends synthetic `OriginTurn`, and sets `minRequiredSeq = minPositiveSeq(prevGroupFirstSeq, repair.MinRequiredSeq)` (Minimum Progress Guard, zero DB writes on overflow). Durable `OriginTurn` commit and `ProviderRequest` construction happen only after `Cooldown`, `BeforeModelCall`, and `applyPendingInputs` with a full `projectCandidate` re-run on persisted `seq`s. All-candidate overflow returns `StatusFailed / KindWindowOverflow`.

## 8. `MutationEvidence` and Causal `CommitReceipt` Seals

1. **`editing.CommitReceipt` (`internal/editing/engine.go`):** Exports `Committed`, `DirtyFailure` (true on post-`Truncate` write/readback failure, `CodeConcurrentWrite`, or `rollback`), and sorted `[]FileCommitReceipt{Path, Wrote, Created, Deleted, OldSHA256, NewSHA256}` using full-byte `Wrote` and full SHA-256 `writeCAS` read-back verification.
2. **Per-`ExecutionID` `commitRecorder`:** Bound into `toolCtx` via `WithCommitRecorder` per `admitExecution` / `admitCallbackExecution` (nested callbacks shadow parent recorders); `toolset.commitPlan` records the receipt and sets `MutatesWorkspace = anyWrote || receipt.DirtyFailure`.
3. **Triple-verified `postFiles` seal:** Appends `;cm=proven|none|unknown` onto `derived_origins.post_ref` inside the existing `CommitToolResult` / `SettleExecution` transaction (`DirtyFailure` forces `OutcomeUnknown` + `;cm=unknown`). `;cm=proven` requires `OutcomeComplete`, `CoverageComplete`, `hasPositiveFilesSnapshotRef(PreRef)`, `receipt.Committed`, at least one `Wrote == true`, all `!Wrote` targets `preBlob == postBlob`, all `Wrote` targets `preBlob != postBlob`, pre/post-`SnapshotFiles` `os.Lstat` stability + raw disk `SHA256 == NewSHA256` on `Wrote && !Deleted` targets (and `os.ErrNotExist && postBlob == "-"` on `Deleted`), and `PreRef != PostSnapRef`. `;cm=none` requires all targets `!Wrote && preBlob == postBlob` and zero file edges. Everything else seals `;cm=unknown`.
4. **Classification & `Unknown` veto:** All `tierWorkspace` (`bash`, `edit_go`, `lsp_rename`, `eval_artifact`), all `mcp__*`, imported tool-call groups (`OriginImported`), and unsealed legacy `tierFiles` rows classify as `MutationUnknown`. Fixed read-only/delegation admitted builtins (`start_task`, `spawn_subagent`, `callbackAllowedBuiltins` with `FileEdgeCount == 0 && Outcome != OutcomeUnknown`) classify as `MutationNone`. `aggregateGroupMutation` enforces `Unknown > Proven > None` per `answered_seq`.

## 9. Skills Run Snapshot and Progressive Discovery

- `skills.Registry` adds `sync.RWMutex`, `os.CreateTemp` atomic writes, `MaxSkills = 128` root truncation warnings, and `PromptResult{Eligible []SnapshottedSkill, Text (<=32KiB), CompactText (<=8KiB)}`.
- `Engine.prepareRun` builds one immutable `skillReader` (`ordered`, `byID`) from `prompt.Skills.Eligible`—shared by `SystemPrompt`, `ListSkills`, and `ReadSkill`. `ReadSkill(ctx, id, ""|"SKILL.md")` serves in-memory `SnapshottedSkill.Content` directly, immune to mid-run disk `Save/Delete/SetEnabled`; auxiliary relative files use allowlisted `skills.ReadResource`.
- `toolset/skills.go` exposes flat dual-mode `{id, path, query, offset}` without top-level `required`/`oneOf`, enforcing mutual exclusion, `MaxSkillCatalogPage = 50`, and `MaxSkillCatalogResponseBytes = 16384` (`DescriptionTruncated: true` fallback for oversized first items).

## 10. `RequestEnvelope` v2, Lossless `ModelRef`, and Lineage Priority

- `config` enforces `MaxProviderIDBytes = 64` and `MaxModelKeyBytes = 192` (`<= 257B` UTF-8, `<= 769B` percent-escaped).
- `EncodeEnvelopeModelRef` emits `["model:" + escaped]` (`<= 90B`) or UTF-8 codepoint-aligned chunks `model~0~...` ending in terminal seal `model~<K-1>.~...` ($2 \le K \le 10$); `DecodeEnvelopeModelRef` decodes offline without config tables and rejects missing `.~` with `ErrIncompleteModelRefChunks`. Used across `PurposeMain`, `PurposeAutoEffort`, and `PurposeReview`.
- `PurposeMain` `requestRefs` (`projectionVersion = 2`) orders: **P0** `ModelRef` (`<=10`), **P1** `proj:mode:<full|compact>` (`1`), **P2** `sys:*` (`<=5` over transmitted `SentLayers`), **P3** `ts:v<ver>` (`1`), **P4** `taskstate.RenderBlock.ItemRefs` (`startByteOffset < cutoff`, `<=111` before `128`). Overflow past `128` truncates only P4 tail and sets `LineageComplete = false`; `128 × 96B` marshals to at most `12,673B < 16,384B`.

## 11. Deterministic `Resume` and Five-Level Replay Boundary

- Pre-commit crashes reconcile via `Store.Reconcile` (`OriginToolInterrupted` + `outcome_unknown` -> `MutationUnknown`); post-commit crashes recover `;cm=*` identically via `Store.ListAnsweredExecutions` + `classifyExecutionSummary` + `aggregateGroupMutation` + `deriveRepairContext`. New `Resume` runs refresh `SkillSnapshot` in `prepareRun`.
- Supported verification levels: (1) **Decision Replay** (`VerifyStep` / `ReplayDecision` including `RuleWindowPrefix` and `CompactFixedByModel`), (2) **Request Envelope Verification** (offline SQLite), and (3) **Static Projection Verification** (same in-memory inputs). **Full Projection Replay** (offline reconstruction of ephemeral `Echo`) and **Full Run Replay** are explicit non-goals.

## 12. Security, Privacy, and Database Compatibility

- **Security & privacy:** Preserves `ADR-0018` `consumerFence` / `OriginKnowledge` provenance and `window.Withhold`, `ADR-0013` untrusted workspace withholding and `toolset/path.go` confinement, and unexported-key `commitRecorder` isolation.
- **Database compatibility:** **Zero SQLite migrations** (`0001`–`0017` unchanged). `;cm=*` in `derived_origins.post_ref` (`<= 94B <= 512B`) satisfies `checkLooseRef`, keeps exact **2 DB writes** per side-effect tool call, and degrades legacy unsealed rows safely to `MutationUnknown`.

## 13. Module Ownership, Rejected Alternatives, and Risks

- **Single module ownership:** `internal/window` (token estimation & suffix build), `internal/skills` + `runtime/skills.go` + `internal/toolset/skills.go` (run skill snapshot & catalog), `runtime/prompt.go` (prompt layers & project outline), `internal/taskstate` (`RenderBlock`), `internal/editing` + `internal/toolset` + `internal/agent/execution.go` (`CommitReceipt` & `;cm=*`), `internal/session` (`ListAnsweredExecutions`), and `internal/agent` (7-step projection, repair floor, envelope v2, replay).
- **Rejected alternatives:** (1) Advancing durable `WindowStartSeq`; (2) Moving `OriginKnowledge/Notebook` out of `BeginRun`; (3) Fixed five-zone or `60%` quotas; (4) Optical image handles; (5) 4-level stateful `r.degradeLevel` / `RepairAnchor`; (6) `tierWorkspace` `MutationProven`/`MutationNone` from `git status`; (7) `model:sha256:` one-way hashes or `agent_steps` dual-writes; (8) Live `ToolSpec.Permission` classification of `mcp__*`; (9) `PreRef != PostRef` without `CommitReceipt` or `edit.committed` event recovery; (10) Migration `0018` or a second file table.
- **Risks & `UNVERIFIED` limits:** Closed-source BPE variance and remote cache hit rates remain `UNVERIFIED` (bounded by `GuardTokens + CapacityRetry + Failover`); shared-worktree Scenario C1 credits actual agent overwrite `C != A`; skill auxiliary files (`references/*`) remain allowlisted disk reads.

## 14. Three-Phase Plan and `T1–T12` Acceptance Mapping

- **Phase A:** Commit `ADR-0024` (EN/ZH), ADR indexes, and baseline/wiring report `docs/reports/adr-0024-context-space-implementation-2026-11-18.md`; verify build/vet/baseline tests.
- **Phase B:** Implement core modules in clean atomic sub-commits: (B-Stage 1) `window` estimation, `taskstate.RenderBlock`, `SkillSnapshot` + dual-mode `read_skill`, project outline; (B-Stage 2) `config` UTF-8 bounds, candidate `Presentation`/`ProviderPkg`, `stepspecs`, `presentation` (`REV-F06` + MCP fences + feedback dual guards), `policy` (`ReplayDecision`), `envelope` v2; (B-Stage 3) `editing.CommitReceipt`, `postFiles` `;cm=*`, `ListAnsweredExecutions`, `deriveRepairContext`, `r.groups` clamp, 7-step `projectCandidate`, `TurnBoundary` dry-run, `BeforeModelCall` rebuild.
- **Phase C:** Pass full offline deterministic matrix `T1–T12` (`T1` bidirectional failover; `T2` stateless `newest_group_fit`; `T3` overflow + `RC34-F02` T1–T6 + `.gitignore` matrix; `T4` `RC34-F02` T7–T10 multi-call/crash/`Resume`; `T5` fenced `AGENTS.md` `anchors=true`; `T6` estimator & JSON escape guards; `T7` `BeforeModelCall` `Steer` `Echo` reset; `T8` `TurnBoundary` dry-run & Minimum Progress Guard; `T9` MCP fences & `REV-F06` built-ins; `T10` `240+` skill snapshot & `-race`; `T11` candidate profile governance; `T12` 10-chunk `ModelRef`, `.~` seal, `128/129` slots & `12,673B`), run macOS ARM64 & Linux ARM64 build/vet/test/race plus Windows AMD64 build, and complete independent review.

[中文](0024-candidate-context-projection-and-budget-guards.zh-CN.md) · [ADR-0018](0018-request-provenance-and-fences.md) · [ADR-0023](0023-unified-cognitive-lifecycle.md).
