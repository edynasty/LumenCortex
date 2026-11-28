# L4: candidate acceptance, baseline integration, release

Load this for: independent acceptance of a Core candidate, integrating a candidate into `main`, a schema-migration release, a version release, or closing Findings. L4 is run on a **frozen SHA in a clean worktree**; it is never the inner loop of development.

Authority boundaries (workspace `AGENTS.md`): the implementer never grants their own independent PASS; whoever raises a Finding owns its fix; tags, releases, merges and pushes need the authorization the task states. L4 authorizes none of them.

## 1. Preconditions

```bash
git -C <review-worktree> rev-parse HEAD          # equals the frozen candidate SHA
git -C <review-worktree> status --porcelain      # empty (ignored files only)
go version; go env CGO_ENABLED GOFLAGS           # cgo=1; GOFLAGS empty or unset with env -u
uptime                                            # record load; no other heavy suite running on the host
```

Create one evidence directory per acceptance (workspace `bench/evidence/<task>-<date>/`, outside Git). Every gate writes its own receipt (command, SHA, start/end, seconds, exit, log path) and never overwrites an earlier receipt. The existing pattern to reuse: `bench/evidence/core-final-independent-acceptance-20261125/{final_gates.sh,gate.py,RECEIPTS.tsv}` — gates run in a chain, the first non-zero exit stops it, nothing is rerun to get green.

## 2. Matrix

Run in this order; stop at the first `FAIL`/`TIMEOUT`, diagnose, then decide (fix by the Finding owner → new frozen SHA → rerun the chain from the failed gate's layer, with a reason).

| # | Gate | Command | Notes |
|---|---|---|---|
| A1 | Module integrity | `go mod verify` | |
| A2 | Build / vet | `env -u GOFLAGS go build ./... && env -u GOFLAGS go vet ./...` | |
| A3 | Diff-to-Test plan vs base | `scripts/affected.sh <base-sha> <candidate-sha>` (range mode; run in a checkout of the candidate so importers and runtime groups match) + `test-matrix.md` rows | lists the anchors that must be green and the areas that need L3 |
| A4 | Area anchors and probes | the anchors of every touched area; reviewer's own red/green probes via `-overlay` (never committed into the candidate) | probes prove the reviewer's own hypotheses; keep overlay files in evidence |
| A5 | Offline replays (when the candidate touches prompt/cache/usage/window) | the replay scripts named by the area reference (`prompt-tools-cache.md` §4) | `NOT_RUN(evidence-missing)` if their inputs are absent; never PASS |
| A6 | Full module | `env -u GOFLAGS go test -count=1 -timeout=60m -json ./... > full.jsonl` | count with `test-matrix.md` §4 |
| A7 | Race | `env -u GOFLAGS go test -race -count=1 -timeout=90m <race set>` | race set = packages with shared state touched by the candidate; the `7c032ae` acceptance used `./cmd/lcx-go ./config ./internal/agent ./internal/editing ./internal/session ./internal/taskstate ./internal/toolset ./internal/verify ./protocol ./provider/anthropic ./provider/internal/wireusage ./provider/openai ./runtime` (`wireusage` exists only on the candidate). A full-module race run is optional evidence, not a substitute for choosing the set |
| A8 | Release check | `scripts/release-check.sh --mode local` (clean tree) | `--mode release` only for an authorized tagged release |
| A9 | Platform | `COMPILE_ONLY` cross-checks for changed `//go:build linux` packages (`reliability.md` §6) | Linux runtime: `NOT_RUN(platform)` |
| A10 | Cross-repository | if exported Engine API / event JSON / CLI output changed: Desktop backend tests against this exact Core SHA, run in the Desktop repo by its own rules | one side passing is not integration PASS; otherwise `NOT_RUN(out-of-scope)` |
| A11 | Hosted CI | the latest `go-runtime.yml` result for this SHA, if any | separate signal; a hosted failure is investigated from logs, never hidden, never counted as the macOS gate |

Schema-migration releases add: upgrade from every shipped fixture, downgrade refusal/losslessness, close/reopen data preservation (`reliability.md` §1), and a copy of a real (non-secret) development database migrated in a temp dir when one is available.

## 3. Reuse instead of rerun

- A frozen SHA gets **one** A6/A7 run. A later commit that changes no Go build or test input (docs, reports) is proven with `bench/evidence/core-final-independent-acceptance-20261125/docs_only_delta.sh <gated-sha> <later-sha>` (it checks `*.go`, `go.mod`/`go.sum`, every `go:embed` source, and Go files that read `docs/`); `0` changed inputs → the earlier A6/A7 receipts stand.
- Unchanged code that passed independently stays passed; re-review the diff since the last PASS plus affected interfaces.
- Historical results are cited with their SHA and date, never counted as fresh runs.

## 4. Findings

Each actionable Finding: ID, severity, owner (= finder), affected SHA/files, trigger and impact, reproducer, root cause, fix commit, red (fails on the unfixed SHA or a faithful overlay) / green (passes on the fix), regression set run, closure status. A different reviewer verifies the closure. P0–P2 cannot remain open at a `READY` verdict; P3 deferrals state the reason.

## 5. Verdict

Report the per-gate status table (`PASS`/`FAIL`/`TIMEOUT`/`BLOCKED`/`NOT_RUN`/`SKIP` with reasons), counts from A6, the race set and its result, Findings with status, known pre-existing failures with base evidence, and what was not covered. Use the task's verdict vocabulary; a verdict is `PASS` only when every required gate is `PASS` and no required item is `NOT_RUN` without an accepted reason.
