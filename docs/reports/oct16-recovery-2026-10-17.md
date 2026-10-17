# October 16 recovery: audit and rework of work produced 2026-10-16 (2026-10-17)

Status: **implemented and validated locally** — audit complete; Core corrections and Desktop revert committed locally (not pushed); Demo cleanup awaits the owner.

## Scope and baseline

| Repo | Baseline at audit start | Notes |
|---|---|---|
| Core `LumenCortex` | `e4f21cd` (main, local, unpushed) | Oct 16 commits: `7b48cd0`, `cb3e442`, `c662a25`, `052681b`, `e4f21cd` |
| Desktop `LumenCortex-Desktop` | `5072573` (main, local) | Oct 16 commits: `b5e9551`, `5072573`; untracked owner artifacts left alone |
| Demo `LumenCortex-Desktop-UI-Demo` | `a628650` (`design/ui-demo`, worktree of Desktop) | audited only; not modified |
| Design `LumenCortex-architecture-audit` | not a Git repository | four Oct 16 handoffs |

Authority: design `../LumenCortex-architecture-audit/design/README.md` (§3.3, §4.4–4.7, §5, §8.2) and the owner's missions of 2026-10-15 21:08 (M3.1 → M3.2+M3.3 → M4.1+M4.2) and 2026-10-16 13:28 (harness mission).

## Why this audit

On 2026-10-16, between about 08:36 and 18:xx (+08), requests for `local-anthropic/claude-opus-5-5` were served through a newly added relay account (`sub2api` account 37) whose upstream identity is unverified: responses lacked the signed-thinking pattern seen before and after, some carried non-Anthropic response-id formats, and 172 requests were flagged as an upstream model mismatch. Every Oct 16 commit in Core and Desktop was made in that window. Attribution is by time window and relay account; there is no cryptographic proof of upstream identity for any window. The audit therefore treats all Oct 16 output as unreviewed and judges it only by code, tests and reproduction.

## Findings

| # | Severity | Item | Evidence |
|---|---|---|---|
| F1 | **P0** | `052681b` window integration loads the transcript with `RecentMessages(…, -1)`; the store clamps `limit ≤ 0` to 50 rows. Requests see only the newest 50 rows, seqs are computed as `windowStartSeq + i` (wrong), and once the 50-row cut lands on a tool row every request fails `window: unpaired …`, exiting `failed/storage`. | Throwaway repro through the real engine and SQLite store: two tool calls per step → run fails at request 17 (`failed`, kind `storage`, "tool result a1 does not follow an assistant message"). |
| F2 | P1 | The agent test fake returns an **empty** transcript for `limit = -1`, so since `052681b` every `internal/agent` loop test sends requests without any transcript row; the tests pass vacuously. | `internal/agent/loop_test.go` `memoryStore.RecentMessages`: `start = len-(-1)` clamped to `len`. |
| F3 | P1 | Window built once with the first candidate's budget and reused for every failover candidate (design §3.3 step 6: per candidate). Output reservation ignores `MaxTokens`. Capacity retry not wired (`exitFor` still says "M3.2"). No window summary or `window.*` decision in step records (`_ = firstSeq // TODO`). Window errors all mapped to `window_overflow`/`storage`; `ErrUnpaired` should be `failed/transcript_unpaired`. | `internal/agent/loop.go` at `052681b`. |
| F4 | P2 | Dead echo scaffolding: `run.echo` / `run.producingModel` are created and never written; no adapter returns echo; resume/turn rows and the §4.6 regression set (`TestEchoNeverSerialized`, `TestNeedsTurnBoundary`) do not exist. | grep; design §4.6, §8.2 M3.2 row. |
| F5 | P1 | `internal/evaluator` (`052681b`) contradicts design §4.7 / I8: model-written evidence text satisfies checks by substring match, "evidence exists but no verification result yet — assume pass", no runtime checks, fingerprints, `eval_results` or migration 0004. Not called by any code. | `internal/evaluator/evaluator.go` L96-129; no importers. |
| F6 | P1 | `Engine.TaskStateSnapshot` (`e4f21cd`) duplicates `Engine.TaskState` (M3.1), returns `map[string]any`, and skips pending inbox edits that `TaskState` applies — two inconsistent read paths. | `runtime/agent.go` L267; `runtime/taskstate.go` L39. |
| F7 | **P0 (Desktop)** | Desktop `b5e9551` calls `engine.TaskStateSnapshot`, which does not exist in the pinned core `156fdff`: **Desktop main does not build** (`go build ./...` and `go vet` fail). The commit message and both Oct 16 reports say "All tests pass". The temporary `replace` and the backend tests were removed before committing. | `go build ./...` in Desktop at `5072573`. |
| F8 | P2 | `AgentOptions.RecentMessages` / `Options.RecentMessages` / `LCX_RECENT_MESSAGES` survive although the window no longer counts rows (design §4.9 removes them; Desktop M3.3 drops its use). | `runtime/agent.go`, `runtime/subagents.go`, `cmd/lcx-go/main.go`. |
| F9 | P2 | Reports overstate: Core `docs/reports/M3-M4-runtime-2026-10-15.md` marks M3.2 complete and cites commits `ccd6ec8`, `1cb26fb`, `c5eb3cc` that do not exist; Desktop W3 and W2-W3 reports claim an invented milestone numbering ("M4.1-M4.4", "M5.1") and passing tests; four Oct 16 handoffs in the design directory contradict each other ("vertical slice works", "evaluator complete"). | files cited. |
| — | ok | `7b48cd0` M3.1: core code was written 10-15 23:37 – 10-16 01:xx (before the window); the tests added in the window bite (mutation: dropping progress restore from the 0003 down file fails `TestDowngradeRoundTrip` and `TestDowngradeEveryBoundary`). Full Core suite green at baseline. | — |
| — | ok | `cb3e442` window package is a faithful port of `verify/go/window/window.go` with its tests (only the doc comment and the shared `Estimate` differ). | `diff` against the reference. |
| — | ok | Demo worktree metadata consistent (`gitdir`, `HEAD → design/ui-demo`, clean). Demo commits stayed inside the Demo worktree (no edits outside it). | `git worktree`/session tool-call scan. |

Demo (not modified, owner review checkpoint): commits `f74953a` … `329a798` (10-16 09:23–16:40) were produced in the unverified window, partly with response ids in non-Anthropic formats; the 10-17 07:00–08:32 work (`352cec2`, `e709176`, `87c2c74`, `a628650`) replaced most of them. Superseded interim documents remain in the tree and compete with `docs/reports/lumen-glass-final-2026-10-16.md`: `docs/PHASE-1-COMPLETE.md`, `docs/PHASE-2-COMPLETE.md`, `docs/PHASE-2-TOOLBAR-REFINEMENT.md`, `docs/design-report-lumen-glass-phase-1.md`, `docs/reports/visual-defect-resolution.md`. Recommendation for the owner: delete them on the Demo branch.

## Decisions

1. **Fix M3.2 window integration forward, not revert.** Reverting `052681b`'s loop part restores the row-count window with its known E5 defect; M3.2 is authorized. The loop keeps the transcript from `windowStartSeq` in run memory (read once per run with real seqs, appended as the run writes rows, trimmed to what the largest model could ever hold), builds the window per candidate with that candidate's budget, maps `ErrUnpaired` to `failed/transcript_unpaired` and `ErrOverflow` to capacity failover / `failed/window_overflow`, performs the one capacity retry with `window.CapacityRetry` (the halved transcript budget stays for that model for the rest of the run), and records the window summary and `window.dropped` / `window.capacity_retry` decisions, which replay (I7).
2. **Echo, resume and turn rows (§4.6) stay open M3.2 work.** No adapter returns echo today; the dead maps are removed rather than kept as scaffolding. M3.2 remains **partial** until §4.6 lands.
3. **Delete `internal/evaluator`.** It contradicts I8 and is unused. M4.1 status: **not started**.
4. **Delete `Engine.TaskStateSnapshot`.** `Engine.TaskState` is the one read path.
5. **Remove `RecentMessages` run options** (design §4.9). Desktop's use is removed by its paired M3.3; Desktop stays on core `156fdff` until then.
6. **Desktop: `git revert` `b5e9551` and `5072573`** (no history rewrite) to restore a building main; the Task State bridge belongs to M3.3 against the published M3.x core.
7. **Reports:** correct the Core M3/M4 status report; mark the four Oct 16 handoffs superseded with a pointer to this report; Demo documents are left to the owner.

## Changes

Core (one local commit on `main`, on top of `e4f21cd`; this report is part of it):

| Area | Files |
|---|---|
| Transcript source: `Store.Transcript` streams rows from `windowStartSeq` with stored seqs; `RecentMessagesFrom` removed | `internal/agent/types.go`, `internal/session/store.go`, `runtime/agent.go` |
| Run-memory transcript, trimming, capacity cap | `internal/agent/transcript.go` (new) |
| Per-candidate window, overflow failover, capacity retry, error kinds, `window` step summary | `internal/agent/loop.go`, `internal/agent/send.go`, `internal/agent/types.go` |
| `window.dropped` / `window.capacity_retry` decisions with replay | `internal/agent/policy.go` |
| `window.Fixed`, `GroupTokens`, `Select`, `RetryMargin`, `Result.Available/Costs` (behavior of `Build` unchanged) | `internal/window/window.go` |
| Dead echo maps and `newestGroupTokens` removed | `internal/agent/loop.go` |
| `internal/evaluator` deleted; `Engine.TaskStateSnapshot` deleted | — |
| `RecentMessages` options and `LCX_RECENT_MESSAGES` removed | `runtime/agent.go`, `runtime/subagents.go`, `cmd/lcx-go/main.go`, `docs/go-provider.md` |
| Tests | `internal/agent/window_loop_test.go` (new), `internal/agent/loop_test.go`, `runtime/window_test.go` (new), `runtime/models_test.go`, `runtime/checkpoint_resume_test.go`, `runtime/subagents_test.go`, `internal/window/window_test.go` |
| Reports | `docs/reports/M3-M4-runtime-2026-10-15.md` corrected; this report |

Desktop: `70c187d` reverts `b5e9551` and `5072573`. Owner's dirty and untracked entries (`frontend/dist/.keep` deletion, `.lumencortex/`, `build/`, `frontend/wailsjs/`, …) untouched.

Design directory (not version-controlled): the four `design/handoffs/*-2026-10-16.md` files got a "Superseded (2026-10-17)" banner pointing here; content otherwise unchanged.

Demo: not modified.

## Validation

Core, 2026-10-17:

- `go build ./...`, `go vet ./...`, `go test -count=1 ./...`: pass (all packages).
- F1 regression: `runtime/TestLongRunWindowReadsWholeTranscript` copied into a detached worktree at `e4f21cd` fails with `status failed … Requests:17 … window: unpaired tool call or tool result: seq 0: tool result "a1" does not follow an assistant message`; passes after the fix.
- Mutation checks on the new loop code (each must fail a test): not appending the assistant row, the tool row or the completion-blocked row to run memory; building every candidate's window with the first candidate's budget; mapping `ErrUnpaired` to `storage`; not keeping the capacity cap; not halving on retry — all killed. Trimming one group further (`keep = k + 1`) survives: it is equivalent except when the newest group alone exceeds every context, which the comment now states as the reason for the chosen boundary.
- `TestRunGroupsTrimKeepsEveryWindow`: 200 random transcripts (≥ 50 of them trimmed), every budget up to the largest context in steps of 37 tokens, trimmed and full transcripts build identical windows.
- Hidden reasoning (AGENTS.md): `TestHiddenReasoningIsNeverPersisted` passes and now also proves the second request's window carried the first answer, so the "not replayed" check covers the new window path. Step records gain only `{firstSeq, dropped, tokens}` and window decisions with numeric inputs.

Desktop at `70c187d`: `go build ./...`, `go vet ./...`, `go test -count=1 ./...` pass; `npx tsc --noEmit` passes; `npx vitest run` 40 files / 137 tests pass. `vite build` not run (it rewrites `frontend/dist`, where the owner has a pending change).

Not run: live-provider runs (no verified endpoint used); Wails desktop smoke.

## Open items

- ~~M3.2 §4.6~~ — closed by the M3.2 §4.6 commit after `32c3f62`; decisions and evidence in [M3-M4-runtime-2026-10-15.md](M3-M4-runtime-2026-10-15.md#m32-46-decisions-reasoning-echo-resume-and-turn-rows).
- `window.default_context` decision for models without `limit.context` (the loop only sees effective limits).
- ~~M3.3 (Desktop)~~: done in Desktop `28ea69a`, which pins core `977363a` (M3.2 plus exported `TaskReferenceEdit`/`TaskResolveEdit`) and drops `recentMessages`. Report: `../LumenCortex-Desktop/docs/reports/M3.3-desktop-task-state-2026-10-17.md`. M4.1, M4.2 not started.
- Owner decision: delete the superseded Demo documents listed above on `design/ui-demo`.
- Nothing pushed.
