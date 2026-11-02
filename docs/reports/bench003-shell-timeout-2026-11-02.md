# BENCH003 foreground shell timeout repair

Status: implementation integrated; full Core test/build/vet and related race checks passed. Candidate build and independent benchmark retest next; no product Gate claimed.

Baseline: `5ea827970eedfcd17e5999d994d396d3c2868ce9`, isolated branch `fix/bench003-shell-timeout`.

## Reproducer and boundary

TB4 `bench003-tb4-lc-single`, trial `session-window-debug__7kxnxo7`, model `deepseek-v4.1-flash` issued a recursive `grep -rl "harbor-canary" /` without `timeout_sec`; downstream filters did not exclude `/proc`. Observed open file `/proc/7/task/7/pagemap`, no configured shell default, no return for more than twelve minutes. Owner cancelled the entire native Harbor invocation; classification `OWNER_CANCELLED_STALLED_SHELL`, not task failure or official eight-hour timeout. Raw state, command ID, session ID, process tree, workspace, request usage and billing retained in workspace `bench/evidence/bench003-20261102/remaining202/`. No shared service, Docker settings or unrelated process changed for cancellation.

## Selected repair

Reuse existing shell Runner/process groups and Tool Result schema. Default foreground timeout120s, configurable maximum1800s, explicit argument bounded by maximum and earlier Agent tool deadline. Background jobs and evaluator defaults unchanged. Timeout yields observable failed/timedOut result; user cancellation and Agent deadline remain distinct and stop further model turns. Cancellation and timeout terminate only their process group with TERM, bounded grace, KILL, pipe drain/Wait and watcher join.

## Verification

Controlled FIFO models a blocking special file; no host root scan. Focused `go test -count=1 ./internal/shell` passed (4.610s); config package passed (1.477s). An initial test fixture expired in a local bash login profile before its marker; the controlled pipeline fixture now uses non-login `sh -c`. A duplicated default-value assertion was rejected by vet and removed. Regression scope includes policy precedence, subtree/pipeline termination, concurrent-session isolation, repeated timeout cleanup, timeout continuation, cancellation/deadline persistence and exactly-one tool result. Actual Linux candidate smoke and independent `RUNTIME_FIXED_RETEST` remain pending; original logs and budget unchanged.

An old-candidate task already running when repair was requested completed naturally: `batched-eval-parity__ysARK4w`, Corecompleted, official Reward0 and five failed tests.27requests/2,520,900tokens,27matched local bills USD0.122146140. It is not a runtime-fixed result and is not automatically repeated. Remaining global allowance before the required retest:171requests, conservative local USD6.152397134, retaining prior unknown USD3.20; actual upstream invoice unknown. Workspace evidence `runtime-fixed/BUDGET_BEFORE_RETEST.json`.

Frozen full check passed: `go test -count=1 ./... && go build ./... && go vet ./...`, native job bg218 exit0, wall218.70s (32 packages OK,2 without tests). Exact output retained at workspace `bench/evidence/bench003-20261102/runtime-fixed/full-suite-build-vet.log`. The initial pair-order test bug and login-profile fixture latency were test defects, corrected before this passing frozen run.

Final related race check passed: `go test -race -count=1 ./internal/shell ./internal/toolset ./internal/agent ./runtime`, native jobbg219 exit0, wall280.13s. All four packages passed, runtime278.615s; exact receipt `runtime-fixed/race.log`. Host validation macOS arm64; Linux amd64 binary smoke follows. POSIX process group behavior verified; Windows retains its existing process cancellation implementation and has not been runtime-tested in this change. No Windows process-tree guarantee is claimed.
