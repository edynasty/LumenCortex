# Task delivery completion — 2026-11-03

Status: isolated candidate on `fix/bench003-shell-timeout`, based on `7111ddb182f230fcf956ca6bc99c28e3c3ceba4d`; clean before work. No merge to main, push, tag, release or official-score rewrite. Product Gate remains unapproved.

## Problem and decision

The original loop checks Workflow, Work Units and configured required Checks. With none declared, text `stop` completes. `TestDeliveryStopIsNotProof` first failed with `Status:completed Final:The implementation is not finished.` after one request. The added host contract makes that same response interrupt instead of proving delivery.

`AgentOptions.delivery` / CLI `LCX_DELIVERY` declares files (exist/change), branch and commit separately. Task State captures pre-execution blobs/HEAD once, persists them and renders the requirements each request. Existing approved Checks and Workflow remain behavioral verification. No natural-language-to-shell inference, DSML execution, automatic commit, unconditional dirty gate or new recovery FSM. Two corrections, then `interrupted/delivery_incomplete`; deadline, budget and maxSteps still apply. Conversation turn completion remains ADR-0006 D3, with unmet delivery retained as an objective obligation. A notebook/summary is not proof.

This is not automatic understanding of arbitrary prose. Existing callers that do not select a contract retain their prior semantics; a file condition is not proof of semantic correctness. Historical True-Myth's 80,697 B dirty patch / empty official patch is motivation, not a new benchmark success. Tests use real files/Git and runtime tools but deterministic provider responses.

## Verification actually executed

- Failing-before regression above, then passing `go test ./internal/agent -run '^TestDeliveryStopIsNotProof$'`.
- `go test ./internal/agent ./internal/taskstate ./internal/repository ./runtime ./cmd/lcx-go -run 'TestDelivery' -count=1`: pass after correcting fixture compilation/import and explicit reset input. Missing files, bounded repeated answers, unchanged baseline after reset/resume, actual `write_file` delivery, branch mismatch, empty commit, dirty commit, and no-commit dirty delivery covered.
- `go test -race ./internal/agent ./internal/taskstate ./internal/repository ./runtime -run TestDelivery -count=1`: pass; final runtime rerun after committed-blob comparison also pass.
- `go test ./runtime ./internal/agent ./internal/taskstate ./internal/repository ./provider/openai ./cmd/lcx-go -run 'TestDelivery|TestInteractive|TestContextReset' -count=1`: pass.
- `go test ./provider/openai ./internal/agent ./internal/taskstate ./internal/repository ./cmd/lcx-go`: pass. `go vet ./...` and `go build -o /tmp/lcx-delivery-candidate ./cmd/lcx-go`: pass.
- Initial full `go test ./...` failed `internal/jobs/TestOutputIsBoundedOnDisk`: it incorrectly combined stdout+stderr against a per-stream bound. Owner's correction checks each stream independently; `TestOutputPerStreamDiskBoundWithBothChannels` fills both streams. Production limits unchanged. BENCH005 initial full test/vet/build/Delivery race and captured SSE replay all exited0; final rerun follows independently reproduced sparse/large-index correction below.
- Actual CLI against isolated local SSE fixture (not a paid model): three missing-file answers produced exit1/interrupted, then explicit `resume` after operator-created file produced exit0/completed. Receipt `bench/evidence/task-delivery-20261103/cli-smoke.json`; temporary server and workspace removed. Initial fixture mistakes (literal credential schema, JSON response to streaming client) failed and are not success evidence.
- Original captured SSE: Tengo/Testem reject as availability, True-Myth remains valid stop with no tools. `LCX_BENCH004_TRACE_ROOT=.../bench004-20261103 /opt/homebrew/bin/go test ./provider/openai -run 'TestCapturedFinalResponsesReplay|TestMalformedDSML|Test.*(Incomplete|Truncated|Partial|EOF|Availability)' -count=1 -v` pass. Output `artifact://1864`; exact raw paths, sizes and SHA256 in `bench/evidence/task-delivery-20261103/sse-replay-sources.json`. No raw reasoning added to runtime recovery.
- Final corrected candidate: full `go test ./... -count=1` exit0 (214.74s), `go vet ./...` exit0, `go build ./...`/CLI build exit0, Delivery Race exit0 and original captured SSE replay exit0. Exact commands/times/logs: `bench/evidence/bench005-20261103/acceptance.json`. Final review closes reproduced P2s; many thousands of materialized flagged paths can still hit the bounded inspection ceiling (P3, explicit error, never false completion). Product/Opus approval remains separate.

## Evaluation boundary

Read `bench/EVALUATION_ROADMAP_20261103.md`: TB2 W13 first, SWE-bench Verified12 next, same 1017 route, independent cohorts, no historical reward changes. GET model catalog confirms both `deepseek-v4.1-flash` and `deepseek-v4.1-flash-1017`; cached fix-git image is amd64. No inference request sent. No full cohort started.

Paid TB2 smoke follows final passing acceptance, bounded independent review and exact Core commit/Linux binary. SWE environment preparation runs independently. No claim that this candidate improves reward, Token efficiency, RSS or mainstream-harness parity.

## Scope and risk

Paths are literal relative regular files, bounded to 32 entries/128 bytes each; no deletion/artifact content validator. Existing snapshot hashing bounds individual files to 4 MiB. Commit delivery needs a descendant baseline with a real tree delta, clean scoped files and actual blob equality with HEAD, including index-flag bypass protection. Same-UID full-shell actors can change local runtime state; this does not create an OS sandbox. The host must declare requirements; Checks approve behavior, not model self-assertions. No Desktop change is needed for this optional Core API; Desktop does not yet expose contract selection. Independent Opus final audit is not replaced by this session's review.

## Independent implementation review

`DeliveryReview` found one P2: commit-only contracts with no file list could miss dirty tracked files hidden by `assume-unchanged`/`skip-worktree`. Fixed by bounded `ls-files -v -z` inspection of flagged tracked paths, actual file hashing and HEAD blob comparison; incomplete inspection fails closed. Both flags now have a commit-only consumer-visible regression. The targeted regression and `go test -race ./runtime -run TestDelivery -count=1` passed after the fix; vet/build also passed. Reviewer re-read the correction and closed the finding with no additional defect; no reviewer checks were run. Baseline persistence, Checks/Workflow order, context reset and Conversation semantics had no additional concrete review blocker. This is not a product or Opus approval.

FinalDeliveryReview found no P0/P1 blocker and one P2: commit-only inspection rejected large ordinary indices and absent sparse-checkout files. A real Git regression reproduced both failures before correction. Stream NUL index entries, retain only bounded flagged paths, ignore absent skip-worktree files only in sparse mode, and batch committed-blob comparison; materialized hidden modifications still fail. Targeted regression/runtime Delivery pass after correction. Final full checks and commit receipts belong in `bench/evidence/bench005-20261103/`. Native uncapped admission is already implemented and validated; no fake infinite caps, account/provider changes, main merge or publication.

`DSMLConformance` independently inspected the captured raw responses and all resolvable BENCH004 attempts, made no edits and ran no tests. Exactly two captured closing-DSML text-only stops matched the existing guard; True-Myth's ordinary stop is outside that guard. No concrete observed false-positive/negative was found. The parent's actual adapter replay is the test evidence, not the reviewer's source inspection. Original benchmark classifications and scores are unchanged.

Durable evidence: original `bench/evidence/task-delivery-20261103/` and new `bench/evidence/bench005-20261103/` acceptance/commit/build/real-Trial receipts. Workspace evidence and TODO are outside Core Git; this report is included with the candidate commit.
