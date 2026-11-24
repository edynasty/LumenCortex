#!/usr/bin/env bash
set -uo pipefail

export PATH="/Users/tangxingpeng/.local/share/mise/installs/node/24/bin:/opt/homebrew/bin:/usr/local/go/bin:/usr/local/bin:$PATH"

R6_WT="/Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-r6-20261120"
EVID_DIR="/Users/tangxingpeng/IdeaProjects/me/LumenCortex/bench/evidence/adr-0024-r6-remediation-20261120"
LOG_DIR="${EVID_DIR}/logs"
mkdir -p "${LOG_DIR}"

# 1. Commit 2 focused GREEN log
{
  echo "=== R6 PHASE C: COMMIT 2 GREEN VERIFICATION ==="
  echo "timestamp=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "platform=$(uname -s)/$(uname -m)"
  echo "go_version=$(go version)"
  echo "commit=$(git -C "${R6_WT}" rev-parse HEAD)"
  echo "dirty_files=$(git -C "${R6_WT}" status --porcelain | wc -l | tr -d ' ')"
  echo "cmd=go test -race -v -count=1 -run '^TestR6' ./internal/toolset ./internal/agent"
  (cd "${R6_WT}" && go test -race -v -count=1 -run '^TestR6' ./internal/toolset ./internal/agent)
  echo "exit=$?"
} > "${LOG_DIR}/r6-commit2-green.log" 2>&1

# 2. Full §14 Verification Matrix on frozen code candidate
{
  echo "=== R6 FINAL CANDIDATE VERIFICATION MATRIX (ADR-0024 §14) ==="
  echo "timestamp=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "platform=$(uname -s)/$(uname -m) ($(sw_vers -productName) $(sw_vers -productVersion))"
  echo "go_version=$(go version)"
  echo "branch=$(git -C "${R6_WT}" rev-parse --abbrev-ref HEAD)"
  echo "commit=$(git -C "${R6_WT}" rev-parse HEAD)"
  echo "r5_base=bfc493634d819bc3be7d30d669bbb2fbef30212e"
  echo "dirty_files=$(git -C "${R6_WT}" status --porcelain | wc -l | tr -d ' ')"
  echo "--- changed_files_vs_r5 ---"
  git -C "${R6_WT}" diff --name-status bfc493634d819bc3be7d30d669bbb2fbef30212e..HEAD

  overall=0
  run_step() {
    local label="$1"
    shift
    echo ""
    echo "--- [${label}] cmd=$* ---"
    (cd "${R6_WT}" && "$@")
    local rc=$?
    echo "step_exit=${rc}"
    if [ "${rc}" -ne 0 ]; then
      overall=1
    fi
  }

  run_step "go_build_all" go build ./...
  run_step "go_vet_all" go vet ./...
  run_step "go_test_nonrace" go test -count=1 ./internal/editing ./internal/toolset ./internal/agent ./internal/session
  run_step "go_test_race" go test -race -count=1 ./internal/editing ./internal/toolset ./internal/agent ./internal/session
  run_step "go_test_runtime_t3_t4_race" go test -race -count=1 -run 'TestT3|TestT4' ./runtime

  TMP_BIN="$(mktemp -d "${TMPDIR:-/tmp}/lc-r6-xbuild.XXXXXX")"
  for target in "linux/arm64" "linux/amd64" "windows/amd64" "freebsd/amd64"; do
    os_name="${target%/*}"
    arch_name="${target#*/}"
    echo ""
    echo "--- [cross_build_vet_${os_name}_${arch_name}] cmd=GOOS=${os_name} GOARCH=${arch_name} go build & go vet ./internal/editing ./internal/toolset ./internal/agent ./internal/session ---"
    (cd "${R6_WT}" && GOOS="${os_name}" GOARCH="${arch_name}" go build ./internal/editing ./internal/toolset ./internal/agent ./internal/session && GOOS="${os_name}" GOARCH="${arch_name}" go vet ./internal/editing ./internal/toolset ./internal/agent ./internal/session)
    rc=$?
    echo "step_exit=${rc}"
    if [ "${rc}" -ne 0 ]; then
      overall=1
    fi
  done

  echo ""
  echo "--- [windows_amd64_test_binaries_compile_only_NOT_native_execution] ---"
  for pkg in editing toolset agent session; do
    (cd "${R6_WT}" && GOOS=windows GOARCH=amd64 go test -c -o "${TMP_BIN}/${pkg}-r6-win.test.exe" "./internal/${pkg}")
    rc=$?
    echo "pkg=internal/${pkg} compile_exit=${rc}"
    if [ "${rc}" -ne 0 ]; then
      overall=1
    fi
  done
  ls -lh "${TMP_BIN}"/*.test.exe
  rm -rf "${TMP_BIN}"

  echo ""
  echo "post_dirty_files=$(git -C "${R6_WT}" status --porcelain | wc -l | tr -d ' ')"
  echo "exit=${overall}"
  exit "${overall}"
} > "${LOG_DIR}/verify-r6-final.log" 2>&1

echo "=== VERIFICATION COMPLETE ==="
tail -n 25 "${LOG_DIR}/verify-r6-final.log"
