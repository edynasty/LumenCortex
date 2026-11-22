#!/bin/bash
# Final gate recipe for the OV remediation branch. Read-only with respect to the repository.
set -u
W=/Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-opus-ov-remediation-20261122
D=/private/tmp/lc-ov-remediation/final
LOG=$D/gates.log
cd "$W" || exit 99
export GIT_OPTIONAL_LOCKS=0
run() {
  local label="$1"; shift
  { echo "===== COMMAND: $label ====="; echo "===== START: $(date -u +%Y-%m-%dT%H:%M:%SZ) ====="; } >> "$LOG"
  local s; s=$(date +%s)
  "$@" >> "$LOG" 2>&1
  local rc=$?
  { echo "===== ELAPSED_SECONDS: $(( $(date +%s) - s )) ====="; echo "===== EXIT CODE: $rc ====="; echo; } >> "$LOG"
}
{
  echo "===== HEADER ====="
  echo "HEAD: $(git rev-parse HEAD)"
  echo "branch: $(git branch --show-current)"
  echo "editing.go blob at HEAD: $(git rev-parse HEAD:internal/toolset/editing.go)"
  echo "status before: [$(git status --porcelain=v1 --untracked-files=all | tr '\n' ';')]"
  go version; uname -sm; sysctl -n machdep.cpu.brand_string
  echo "===== END HEADER ====="; echo
} > "$LOG"
CHANGED=$(git diff --name-only a5a1ec6be1081bad4e706944d655a9a9d97f4f70 HEAD -- '*.go' | tr '\n' ' ')
run "gofmt -l $CHANGED" gofmt -l $CHANGED
run "go build ./..." go build ./...
run "go vet ./..." go vet ./...
run "go test -count=1 ./internal/toolset" go test -count=1 ./internal/toolset
run "go test -race -count=1 ./internal/toolset" go test -race -count=1 ./internal/toolset
run "go test -count=1 -v -run '^TestOV' ./internal/toolset" go test -count=1 -v -run '^TestOV' ./internal/toolset
run "go test -count=1 -v -run '^TestSOLREV01|^TestSOL0|^TestR7R2Final|^TestR7R2New01|^TestR7FailurePayload|^TestAstra' ./internal/toolset" go test -count=1 -v -run '^TestSOLREV01|^TestSOL0|^TestR7R2Final|^TestR7R2New01|^TestR7FailurePayload|^TestAstra' ./internal/toolset
run "bash docs/reports/evidence/adr-0024-r7r2-final/verify.sh carbon-probe" bash docs/reports/evidence/adr-0024-r7r2-final/verify.sh carbon-probe
run "go test -race -count=1 -v -run '^TestR7|^TestR201|^TestR5ResultPropagationAndResumeIntegrity\$|^TestR6NestedJSONPreservationAndProvenance\$|^TestR6StructuredPathsAndSanitizeErrorText\$' ./internal/editing ./internal/toolset ./internal/agent ./internal/session" go test -race -count=1 -v -run '^TestR7|^TestR201|^TestR5ResultPropagationAndResumeIntegrity$|^TestR6NestedJSONPreservationAndProvenance$|^TestR6StructuredPathsAndSanitizeErrorText$' ./internal/editing ./internal/toolset ./internal/agent ./internal/session
run "GOOS=windows GOARCH=amd64 go vet ./internal/toolset ./internal/editing (compile/vet only)" env GOOS=windows GOARCH=amd64 go vet ./internal/toolset ./internal/editing
run "GOOS=linux GOARCH=amd64 go vet ./internal/toolset ./internal/editing (compile/vet only)" env GOOS=linux GOARCH=amd64 go vet ./internal/toolset ./internal/editing
run "go test -timeout=45m -count=1 ./... (single repository-wide command)" go test -timeout=45m -count=1 ./...
echo "===== STATUS AFTER: [$(git status --porcelain=v1 --untracked-files=all | tr '\n' ';')] =====" >> "$LOG"
echo "===== HEAD AFTER: $(git rev-parse HEAD) =====" >> "$LOG"
echo GATES_DONE >> "$LOG"
