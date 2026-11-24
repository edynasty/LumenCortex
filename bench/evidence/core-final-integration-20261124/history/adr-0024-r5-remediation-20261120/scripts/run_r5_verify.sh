#!/usr/bin/env bash
set -uo pipefail

export PATH="/Users/tangxingpeng/.local/share/mise/installs/node/24/bin:/opt/homebrew/bin:/usr/local/go/bin:/usr/local/bin:$PATH"
REPO="/Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-r5-20261120"
cd "$REPO"

COMMIT="$(git rev-parse HEAD)"
BRANCH="$(git rev-parse --abbrev-ref HEAD)"
DIRTY="$(git status --porcelain | wc -l | tr -d ' ')"
echo "=== ADR-0024 R5 Final Verification ==="
echo "commit=$COMMIT"
echo "branch=$BRANCH"
echo "dirty_files=$DIRTY"
echo "go_version=$(go version)"
echo "host_uname=$(uname -srm)"
echo "date_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
echo ""

overall=0
run_step() {
  local label="$1"
  shift
  echo "$ $label"
  "$@"
  local rc=$?
  echo "exit=$rc"
  echo ""
  if [ "$rc" -ne 0 ]; then
    overall=1
  fi
}

run_step "go build ./..." go build ./...
run_step "go vet ./..." go vet ./...
run_step "go test -count=1 ./internal/editing ./internal/toolset ./internal/agent ./internal/session" \
  go test -count=1 ./internal/editing ./internal/toolset ./internal/agent ./internal/session
run_step "go test -race -count=1 ./internal/editing ./internal/toolset ./internal/agent ./internal/session" \
  go test -race -count=1 ./internal/editing ./internal/toolset ./internal/agent ./internal/session
run_step "go test -race -count=1 -run 'TestT3|TestT4' ./runtime" \
  go test -race -count=1 -run 'TestT3|TestT4' ./runtime

for pair in "linux/arm64" "linux/amd64" "windows/amd64" "freebsd/amd64"; do
  os="${pair%/*}"
  arch="${pair#*/}"
  run_step "GOOS=$os GOARCH=$arch go build ./..." env GOOS="$os" GOARCH="$arch" go build ./...
  run_step "GOOS=$os GOARCH=$arch go vet ./internal/editing ./internal/toolset ./internal/agent ./internal/session" \
    env GOOS="$os" GOARCH="$arch" go vet ./internal/editing ./internal/toolset ./internal/agent ./internal/session
done

tmpwin="$(mktemp -d)"
trap 'rm -rf "$tmpwin"' EXIT
for pkg in editing toolset agent session; do
  run_step "GOOS=windows GOARCH=amd64 go test -c -o $tmpwin/${pkg}.test.exe ./internal/${pkg}" \
    env GOOS=windows GOARCH=amd64 go test -c -o "$tmpwin/${pkg}.test.exe" "./internal/${pkg}"
done
ls -la "$tmpwin"
echo ""
echo "overall_exit=$overall"
exit "$overall"
