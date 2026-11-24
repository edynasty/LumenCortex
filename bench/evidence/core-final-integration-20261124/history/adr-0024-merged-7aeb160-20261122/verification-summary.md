# ADR-0024 merged verification: execution summary (7aeb1607ffa588727c6fe04873f7b58a1dc16f15)

- Worktree: /Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-merged-verification-20261122 (detached)
- HEAD 7aeb1607ffa588727c6fe04873f7b58a1dc16f15, tree 15ad21e5e6128e6a5f96c5621da2f40bc8115e58; porcelain-v1 empty before and after
- Toolchain: go1.26.5 darwin/arm64 CGO_ENABLED=1, Darwin 27.0.0 arm64 (macOS 27.0.1 26A434); go mod verify: all modules verified; go-sqlite3 cgo files present
- Evidence: before/, gates/, after/, SHA256SUMS (manifest)

| # | Gate (argv) | GOFLAGS | Start UTC | End UTC | Elapsed | Exit | Result |
|---|---|---|---|---|---|---|---|
| 1 | go build ./... | unset | 14:43:30 | 14:43:41 | 11s | 0 | clean |
| 2 | go vet ./... | unset | 14:43:45 | 14:43:48 | 3s | 0 | clean |
| 3 | go test -count=1 -timeout=45m ./... | -json | 14:43:52 | 14:57:41 | 829s | 1 | FAIL: 1 package (cmd/lcx-go, TestDBCommandDispatchesBeforeOpen) |
| 4 | env -u GOFLAGS go test -race -count=1 -timeout=45m -v ./internal/editing ./internal/toolset ./internal/agent ./internal/session | unset | 14:57:57 | 14:59:41 | 104s | 0 | ok x4 |
| 5 | env -u GOFLAGS go test -race -count=1 -timeout=45m -v ./runtime -run '^(TestT3B_|TestT4_|TestT11_)' | unset | 14:59:45 | 15:00:09 | 24s | 0 | ok, 3 top-level PASS |

## Gate 3 (ordinary full test) detail
- 49 packages: 42 pass, 1 fail, 6 skip with `[no test files]` (cmd/lcx-experience-demo, docs, internal/cgroupv2, internal/debugger/dapfake, pluginapi, provider/internal/retryafter). No `[no tests to run]` entries.
- Failure: `github.com/edynasty/LumenCortex/cmd/lcx-go` :: `TestDBCommandDispatchesBeforeOpen` (db_test.go:156). Output: `go: GOMODCACHE entry is relative; must be absolute path: "{\n\t\"GOMODCACHE\": ...}"`. The inherited `GOFLAGS=-json` made the nested `go env GOMODCACHE` emit JSON. Attribution is the env inheritance, per the lead's update; this gate was not rerun.
- 46 test-level skips, each with its skip reason in `gates/03-go-test-all/test-skips-with-output.json`. Raw JSON stream: `gates/03-go-test-all/stdout.json`; package results: `package-results.json`.

## Gates 4 and 5 detail
- Gate 4: editing, toolset, agent and session all `ok`. Skips: TestMaintenanceHelperProcess ("helper process only"), TestT1StoreTransactionLatency ("measurement only: set LCX_T1_PERF=1"). No failures.
- Gate 5: `ok github.com/edynasty/LumenCortex/runtime`. Selected top-level tests: 3 PASS, 0 FAIL, 0 SKIP.
- The meta.txt argv for gate 5 shows the regex unquoted for readability; the executed argv was quoted as in the handoff.

## Limitations
- Gates 4 and 5 used `env -u GOFLAGS` plus the `-v` test flag, as directed by the lead update; gate 3 was run with GOFLAGS=-json as originally directed.
- Disk: 16 Gi free at start and end of gate runs; 13 Gi used per sampled `df`.
- No reruns; no retries; no production or CI actions; no commits.
