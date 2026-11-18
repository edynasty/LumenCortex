# ADR-0024 Context Space Architecture — Implementation & Verification Report

- **Date Started:** 2026-11-18
- **Authoritative Spec:** `LumenCortex-architecture-audit/design/handoffs/context-space-architecture-master.zh-CN.md` (`v0.4-rc3.5`)
- **Spec SHA256:** `bd8dfb00c9781d4d375ce4779789a71f0cbef9f547918ea1f4aa1a0c7481abd9` (verified)
- **Frozen Source Baseline:** `c73d17359f5d9dd16ef88b0de4525862593fddaa` (verified)
- **Implementation Worktree & Branch:** `/Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-context-space-20261118` (`feat/adr-0024-context-space-20261118`)
- **Current Overall Status:** `PHASE_A_COMPLETE_ENTERING_PHASE_B`

## 1. Evidence Boundary Audit (Pre-Implementation Verification)

| Audit Item | Expected | Observed Evidence | Status |
| :--- | :--- | :--- | :--- |
| Architecture Spec Version & SHA256 | `v0.4-rc3.5` / `bd8dfb00...` | `shasum -a 256` = `bd8dfb00c9781d4d375ce4779789a71f0cbef9f547918ea1f4aa1a0c7481abd9` | PASS |
| Core `HEAD` | `c73d17359f5d9dd16ef88b0de4525862593fddaa` | `git -C LumenCortex-Core rev-parse HEAD` = `c73d17359f5d9dd16ef88b0de4525862593fddaa` | PASS |
| Main Worktree Preservation | Do not touch `main` or uncommitted edits | `LumenCortex-Core` on `main` retains uncommitted `M AGENTS.md` untouched; isolated worktree created under `wt/core-adr-0024-context-space-20261118` | PASS |
| Cognitive Core Baseline | Frozen v1.0 (`ADR-0023`) | `internal/cognition/*`, `internal/session/cognitive_*.go`, and migrations `0014`–`0017` present and untouched | PASS |
| SQLite Schema & Migrations | Frozen at `0017_cognitive_completion.sql` | Verified `internal/migrate/sql/0001_v1.sql` .. `0017_cognitive_completion.sql`; zero migration required (`derived_origins.post_ref <= 512B`, `request_envelopes.refs_json <= 16384B`) | PASS |
| Baseline Build / Vet / Package Tests | Clean baseline on macOS ARM64 (`go1.26.5`) | `go build ./...` PASS, `go vet ./...` PASS; `window`, `taskstate`, `skills`, `editing`, `config`, `provider/...`, `session`, `agent`, `toolset` PASS (`runtime` requires `-timeout 25m` due to 661 integration tests) | PASS |

## 2. Phase A2 — Architecture Wiring & Single-Module Ownership Map

Every Context Space capability is wired to a single authoritative module with zero duplicate transcripts, memory stores, or budget calculators:

1. **`runtime/agent.go` (`prepareRun`) & `runtime/prompt.go` (`assemblePrompt`, `EffectivePrompt`):**
   - Calls `e.skillRegistry.Prompt(IncludeProject: plan.trusted)` once per run under the session run lock, producing immutable `PromptResult.Eligible []SnapshottedSkill`, `Text` (`<=32 KiB`), and `CompactText` (`<=8 KiB`).
   - Passes `prompt.Skills.Eligible` into `newSkillReader` (`runtime/skills.go`) so `SystemPrompt`, `ListSkills`, and `ReadSkill` share one immutable snapshot.
   - Generates `compactProjectInstructions` (CommonMark fence-aware 1-based heading/200-line outline with `read_file(..., anchors=true)`) for trusted project instructions `> 12 KiB`, while keeping `<globalDir>/AGENTS.md` `CompactText == ""`.
2. **`runtime/models.go`, `runtime/auto.go`, `runtime/verification.go` & `config`:**
   - Enforces `MaxProviderIDBytes = 64` and `MaxModelKeyBytes = 192` in `config` and `docs/lumencortex.schema.json`.
   - Populates `agent.Model.{ProviderPkg, Presentation}` per candidate without altering run-level `MaxSteps`, `MaxToolCallsPerStep`, `Budget`, or `Policy`.
   - Encodes candidate `ModelRef` via `agent.EncodeEnvelopeModelRef` in `PurposeAutoEffort` and `PurposeReview` envelopes.
3. **`internal/skills/{types,registry}.go`, `runtime/skills.go` & `internal/toolset/skills.go`:**
   - Adds `sync.RWMutex` and `os.CreateTemp` atomic writes to `skills.Registry`, `MaxSkills = 128` root truncation warnings, `SnapshottedSkill` (preserving `SKILL.md` body), snapshot-backed `ListSkills` / `ReadSkill`, and dual-mode flat `read_skill` schema (`<=50` items, `<=16384B`, `DescriptionTruncated` fallback).
4. **`internal/window/{estimate,window}.go` & `provider/{anthropic,openai}/wire.go`:**
   - Owns `EstimateBytes` and `ReserveTokensForBytes` in `internal/window/estimate.go`; provider `Echo` token sizing delegates to `window.EstimateBytes`; `window/window.go` remains the sole contiguous atomic-group selector.
5. **`internal/taskstate/render.go`:**
   - Owns `RenderBlock(goal, s, wf) RenderedBlock`, returning truncated `Text` (`<=10240B`) and `ItemRefs` strictly satisfying `startByteOffset < cutoff`; `Render` delegates to `RenderBlock`.
6. **`internal/editing/engine.go`, `internal/toolset/{editing,files,patch,editing_tools}.go` & `internal/agent/{execution,callback}.go`:**
   - Exports `CommitReceipt` / `FileCommitReceipt` (`Wrote`, `Created`, `Deleted`, `OldSHA256`, `NewSHA256`, `DirtyFailure`) from `editing.Engine.Commit` with full-byte `writeCAS` read-back verification.
   - Binds per-`ExecutionID` `WithCommitRecorder` in `loop.go` and `callback.go`; `toolset.commitPlan` records the receipt and sets `MutatesWorkspace = anyWrote || DirtyFailure`.
   - `postFiles` runs the triple causal check (`CommitReceipt.Wrote` + pre/post `SnapshotFiles` `os.Lstat` & raw SHA-256 sandwich check + `PreBlob != PostBlob`) and seals `;cm=proven|none|unknown` onto `derived_origins.post_ref` inside the existing `CommitToolResult` / `SettleExecution` transaction (`DirtyFailure` -> `OutcomeUnknown`).
7. **`internal/session/{requests,executions}.go`:**
   - Retains `AdmitRequest` (`MaxEnvelopeRefs = 128`, `MaxRefsJSONBytes = 16384`) unchanged and adds read-only `Store.ListAnsweredExecutions(ctx, sessionID, fromSeq)` joining `request_envelopes` and `derived_origins` with file-edge counts.
8. **`internal/agent/{types,stepspecs,presentation,policy,transcript,envelope,loop,send}.go`:**
   - `transcript.go`: `splitFilesSnapshotPostRef`, `classifyExecutionSummary`, `aggregateGroupMutation` (`Unknown` veto), `deriveRepairContext` (forward-monotonic), and `r.groups(maxContext, minKeepSeq)` floor clamp.
   - `presentation.go` & `policy.go`: `presentStepTools`, MCP fence preservation, `REV-F06` explicit `ProfileCompact` built-in safety preconditions, `presentFeedback` (`2458B / 12346B` dual guards), `CandidatesByStaticPrefix`, `GuardTokens`, `BaseGateSlack`, `EffectiveGateSlack`, `WindowPrefixDecision` (`RuleWindowPrefix`), and `ReplayDecision` / `VerifyStep` v2 support.
   - `envelope.go`: `projectionVersion = 2`, `.~`-sealed `EncodeEnvelopeModelRef` / `DecodeEnvelopeModelRef` (`<=10` chunks), and `10 + 7 + 111 = 128` priority `requestRefs`.
   - `loop.go` & `send.go`: Canonical step input, 7-step stateless `projectCandidate`, zero-write `TurnBoundary` dry-run with Minimum Progress Guard, post-`BeforeModelCall` / `applyPendingInputs` full projection rebuild, candidate-mode gate feedback formatting, and step-end `r.mutations` refresh.

## 3. Implementation TODO & Phase Progress

- [x] **Phase A — ADR-0024 & Architecture Wiring** (`IMPLEMENTED` / `TESTED` baseline)
  - [x] Verify spec SHA256, baseline `c73d17359f5d9dd16ef88b0de4525862593fddaa`, SQLite schema `0001`–`0017`, and isolated worktree.
  - [x] Author [0024-candidate-context-projection-and-budget-guards.md](../adr/0024-candidate-context-projection-and-budget-guards.md) and [0024-candidate-context-projection-and-budget-guards.zh-CN.md](../adr/0024-candidate-context-projection-and-budget-guards.zh-CN.md), and update ADR indexes.
  - [x] Document module ownership and call-chain wiring.
- [ ] **Phase B — Core Production Implementation**
  - [ ] **B-Stage 1:** Token estimator (`EstimateBytes`, `ReserveTokensForBytes`) + provider `Echo` alignment; `taskstate.RenderBlock`; `skills.Registry` RWMutex + `SnapshottedSkill` + `runtime.skillReader` + dual-mode `toolset/skills.go`; fence-aware `compactProjectInstructions` (`anchors=true`).
  - [ ] **B-Stage 2:** `config` + `lumencortex.schema.json` UTF-8 byte caps (`64B / 192B`); candidate `ProviderPkg` / `Presentation` & auto/review `ModelRef` refs; `IncludedStepTools`; `presentStepTools` (MCP fences + `REV-F06` built-in compact safety) & `presentFeedback` dual JSON guards; `CandidatesByStaticPrefix`, budget helpers, `WindowPrefixDecision`, `ReplayDecision`; `envelope.go` v2 (`EncodeEnvelopeModelRef` / `DecodeEnvelopeModelRef` + `10+7+111=128` priority).
  - [ ] **B-Stage 3:** `editing.CommitReceipt` + `DirtyFailure` + `toolset.commitPlan`; per-`ExecutionID` `WithCommitRecorder` + `postFiles` triple verification & `;cm=*` seal; `Store.ListAnsweredExecutions`; `classifyExecutionSummary` / `aggregateGroupMutation` / `deriveRepairContext` / `r.groups` clamp; 7-step `projectCandidate`, `TurnBoundary` dry-run & `BeforeModelCall` rebuild in `loop.go` & `send.go`.
- [ ] **Phase C — T1–T12 Acceptance Matrix, Cross-Platform Validation & Independent Review**
  - [ ] Implement & verify full deterministic suite `T1–T12` (including all 10 `RC34-F02` concurrency/crash tests).
  - [ ] Full `go build ./...`, `go vet ./...`, `go test -count=1 ./...`, `go test -race -count=1 ./...` on macOS ARM64 & Ubuntu Linux ARM64 + `GOOS=windows GOARCH=amd64 go build ./...`.
  - [ ] Cognitive Core & harness regression audit.
  - [ ] Independent code review and Finding closure.

## 4. Finding Ledger

| ID | Severity | Owner | Scope / Files | Trigger & Impact | Root Cause & Fix | Regression Evidence | Fix Commit | Status |
| :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- |
| *(None in Phase A — tracking active across Phases B & C)* | — | — | — | — | — | — | — | — |

## 5. Acceptance Matrix (`T1–T12`) & Platform Verification Status

- **T1–T12 Status:** `PENDING_PHASE_B_AND_C` (never claimed before real test execution).
- **Platform Status:**
  - macOS ARM64 (`darwin/arm64`, `go1.26.5`): Baseline `go build ./...` & `go vet ./...` PASS (`PHASE_A`). Full & `-race` suites pending Phase B/C.
  - Ubuntu Linux ARM64 (`linux/arm64` via Docker): Pending Phase C native execution.
  - Windows AMD64 (`windows/amd64`): Pending Phase C cross-compilation check (`GOOS=windows GOARCH=amd64 go build ./...`).

## 6. Commit & SHA256 Manifest

- **Baseline Commit:** `c73d17359f5d9dd16ef88b0de4525862593fddaa`
- **Spec SHA256:** `bd8dfb00c9781d4d375ce4779789a71f0cbef9f547918ea1f4aa1a0c7481abd9`
- **Phase A Commit:** *(recorded upon Phase A commit & push)*
