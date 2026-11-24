# Core Final Integration Addendum — `INT-PRE-REG-01`, `M2` Regression & Post-Merge Acceptance (2026-11-24)

Original pre-remediation documents (`INDEX.md`, `SHA256SUMS`, `docs/reports/core-final-integration-2026-11-24.md` SHA256 `00eec92b1313c9e93723ff2912060c6d9abb2dad4128b39b519c7308bcbb518f`, and `bench/evidence/core-final-integration-postmerge-20261124/04-test-all.stdout` SHA256 `eda4550cfe8a4b6c33e34b2dc6ea1b27920850f57e8e1d73743b9da5566cc961`) remain preserved byte-for-byte without modification.

## 1. Git Integration Sequence (`main`)

1. **Prior merged `main`:** `ef10ca7c8e8031e75fdf27f449924b8254f0e2bb` (PR #11).
2. **PR #12 (`M1`):** `https://github.com/edynasty/LumenCortex/pull/12` — normal merge commit `cfef043083a37b8fb6f67a96002b0d9b8e22a9f9` (parents `ef10ca7c8e8031e75fdf27f449924b8254f0e2bb` and `35409ca0e2015c631d3dd620a354124436a27d2d`), preserving the post-merge `ef10ca7` failure logs and `docs/reports/core-final-integration-2026-11-24.md`.
3. **PR #13 (`M2`):** `https://github.com/edynasty/LumenCortex/pull/13` — `fix(runtime): correct pre-publication staging failure contract`, normal merge commit `d0cb171d0319bd0d523bbc19241343b9d1c3e60c` (parents `cfef043083a37b8fb6f67a96002b0d9b8e22a9f9` and `2852b56023088c3c33b2678af0c8dd7debff06e4`), integrating Carbon's `INT-PRE-REG-01` remediation (`316e99b90fad5d6e22b6f9a9b01f240be2b55370` `runtime/context_space_t3_test.go` only; `2852b56023088c3c33b2678af0c8dd7debff06e4` `bench/evidence/runtime-int-pre-reg-01-carbon-20261124/`). Zero production `.go` files changed in `ef10ca7..M2`.

## 2. Full Regression on Exact Merged `M2` (`d0cb171d0319bd0d523bbc19241343b9d1c3e60c`)

Executed once sequentially on clean detached worktree `wt/core-final-merged-regression-20261124` (`go version go1.26.5 darwin/arm64`, `GOFLAGS=[]`, `CGO_ENABLED=1`, `GOWORK=off`, `GOMODCACHE=/Users/tangxingpeng/go/pkg/mod`). Raw logs and SHA256 manifest archived in `postmerge-regression-m2/`:

| Gate | Command | Window (UTC) | Exit | Result |
|---|---|---|---|---|
| `01a-gofmt-changed` | Changed Go files since `7aeb160` + `internal/editing` + `runtime/context_space_t3_test.go` | `02:43:15Z` | `0` | Empty output (`0` bytes) |
| `01b-gofmt-all-audit` | Tracked non-evidence Go audit | `02:43:15Z` | `0` | 16 untouched pre-existing legacy `.go` files (unchanged since before `7aeb160`) |
| `02-build` | `go build ./...` | `02:43:15Z–02:43:24Z` | `0` | PASS |
| `03-vet` | `go vet ./...` | `02:43:24Z–02:43:27Z` | `0` | PASS |
| `04-test-all` | `go test -count=1 -timeout=45m ./...` | `02:43:27Z–02:56:26Z` | `0` | `43` packages `ok` (`runtime` `747.662s`), `6` `[no test files]`, `0` `FAIL` (non-verbose; test-level skips not enumerated) |
| `05-race-four-packages` | `go test -race -count=1 -timeout=45m ./internal/editing ./internal/toolset ./internal/agent ./internal/session` | `02:56:26Z–02:58:09Z` | `0` | `4/4` PASS (`28.023s`, `98.132s`, `51.937s`, `42.993s`) |
| `06-race-runtime-targeted` | `go test -race -count=1 -v -timeout=45m ./runtime -run "^(TestT3B_\|TestT4_\|TestT11_)"` | `02:58:09Z–02:58:32Z` | `0` | `PASS` (`14.111s`, `18` `--- PASS:` lines across `TestT3B_` [9], `TestT4_` [4], `TestT11_` [2], `0` skips) |

## 3. Independent Review & Acceptance Records

1. **Runtime `INT-PRE-REG-01` Candidate (`2852b56023088c3c33b2678af0c8dd7debff06e4`) Independent Review:** Claude Opus 5.5 — `PASS_WITH_LIMITATIONS` (`INT_PRE_REG_01_INDEPENDENTLY_VERIFIED`). Original logs archived byte-for-byte from `/private/tmp/opus-intpre-review.fkO0cN/_logs/` into `temporary/opus-intpre-review-20261124/` (`13/13` `SHA256SUMS` OK; `REPORT.md` SHA256 `ddf70aaaf8f772a5fc6c124ffdd720727fa3cb8092959279fec4c45ff57a52f5`).
2. **Post-Merge `M2` (`d0cb171d0319bd0d523bbc19241343b9d1c3e60c`) Independent Acceptance:** Fresh-context read-only swarm reviewer `opus-final-accept` (`claude-opus-5-5-high`) — `Verdict: PASS_WITH_LIMITATIONS` across all 14 checks (including independent re-execution on `REG_WT` of changed-file `gofmt`, `go build ./...`, `go vet ./...`, and `go test -race -v ./runtime -run '^(TestT3B_|TestT4_|TestT11_)'`). Verbatim response archived in `postmerge-regression-m2/OPUS_POSTMERGE_ACCEPTANCE.md`.

## 4. Checksums & Scope Statement

- Original historical manifest `SHA256SUMS` (`525/525` entries) remains untouched and passes `shasum -a 256 -c SHA256SUMS`.
- All added archive files (`FINAL_FINDING_LEDGER.md`, `FINAL_INTEGRATION_ADDENDUM.md`, `temporary/opus-intpre-review-20261124/*`, and `postmerge-regression-m2/*`) are covered by `ADDENDUM_SHA256SUMS` in addition to their inner directory manifests.
- Supported scope and open limitations remain governed by `docs/adr/governance/pre01-pre02-owner-approval-2026-11-24.md` and `FINAL_FINDING_LEDGER.md` (development `main` only; no production release, universal filesystem guarantee, native ext4/Windows/FreeBSD claim, or TB2 authorization).
