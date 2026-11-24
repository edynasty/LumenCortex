#!/usr/bin/env bash
set -u

export PATH="/Users/tangxingpeng/.local/share/mise/installs/node/24/bin:/opt/homebrew/bin:/usr/local/go/bin:/usr/local/bin:$PATH"
REPO="/Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-r4-20261120"
cd "$REPO" || exit 1

echo "commit=$(git rev-parse HEAD)"
echo "branch=$(git rev-parse --abbrev-ref HEAD)"
echo "dirty_files=$(git status --porcelain | wc -l | tr -d ' ')"
echo "go_version=$(go version)"
echo "host_uname=$(uname -srm)"
echo "date_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ)"

overall=0

run_step() {
  echo ""
  echo "$ $*"
  "$@"
  local rc=$?
  echo "exit=$rc"
  if [ "$rc" -ne 0 ]; then
    overall=1
  fi
}

run_env_step() {
  local goos="$1"
  local goarch="$2"
  shift 2
  echo ""
  echo "$ GOOS=$goos GOARCH=$goarch $*"
  GOOS="$goos" GOARCH="$goarch" "$@"
  local rc=$?
  echo "exit=$rc"
  if [ "$rc" -ne 0 ]; then
    overall=1
  fi
}

run_step go build ./...
run_step go vet ./...
run_step go test -count=1 ./internal/agent ./internal/editing ./internal/toolset ./internal/session
run_step go test -race -count=1 ./internal/agent ./internal/editing ./internal/toolset ./internal/session
run_step go test -race -count=1 -run 'TestT3|TestT4' ./runtime

for target in "linux arm64" "linux amd64" "windows amd64" "freebsd amd64"; do
  set -- $target
  run_env_step "$1" "$2" go build ./...
  run_env_step "$1" "$2" go vet ./internal/editing ./internal/toolset ./internal/agent ./internal/session
done

TMP_WIN_BINS="$(mktemp -d)"
trap 'rm -rf "$TMP_WIN_BINS"' EXIT
run_env_step windows amd64 go test -c -o "$TMP_WIN_BINS/editing.test.exe" ./internal/editing
run_env_step windows amd64 go test -c -o "$TMP_WIN_BINS/toolset.test.exe" ./internal/toolset
run_env_step windows amd64 go test -c -o "$TMP_WIN_BINS/agent.test.exe" ./internal/agent
run_env_step windows amd64 go test -c -o "$TMP_WIN_BINS/session.test.exe" ./internal/session

echo ""
echo "overall_exit=$overall"
exit "$overall"
