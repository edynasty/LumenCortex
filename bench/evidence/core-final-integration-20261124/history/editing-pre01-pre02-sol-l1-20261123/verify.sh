#!/bin/bash
set -u
set -o pipefail
ROOT=$(pwd)
E="$ROOT/bench/evidence/editing-pre01-pre02-sol-l1-20261123"
W="$ROOT/wt/core-editing-pre01-pre02-sol-l1-20261123"
BASE=71cce56acb06b457dad2a4fec3a2c3db56537fd6
export GOWORK=off GOFLAGS= CGO_ENABLED=1
run() {
  local label=$1; shift
  local log="$E/logs/$label.txt" rc
  { printf 'start=%s\ncwd=%s\n' "$(date -u +%FT%TZ)" "$PWD"; printf 'command='; printf '%q ' "$@"; printf '\n'; "$@"; rc=$?; printf '\nend=%s\nexit=%s\n' "$(date -u +%FT%TZ)" "$rc"; } > "$log" 2>&1
  printf '%s exit=%s\n' "$label" "$rc"
  return "$rc"
}
RED=$(mktemp -d /tmp/lc-sol-l1-red.XXXXXX)
printf 'baseline=%s\nexport=%s\n' "$BASE" "$RED" > "$E/red-source.txt"
git -C "$W" archive "$BASE" | tar -xf - -C "$RED" || exit 2
cp "$W/internal/editing/remediation_pre01_pre02_opus_unix_test.go" "$E/regression_test.go"
printf '{"Replace":{"%s/internal/editing/remediation_pre01_pre02_opus_unix_test.go":"%s/regression_test.go"}}\n' "$RED" "$E" > "$E/red-overlay.json"
cd "$RED" || exit 2
run 01-red go test -overlay "$E/red-overlay.json" -count=1 -v -run 'TestSolPREL1UnsupportedSwapRefused|TestOpusPRE01HardLinkedUpdateRefusedBeforeAnyWrite' ./internal/editing
rc=$?
if [ "$rc" -eq 0 ]; then printf 'RED unexpectedly passed\n'; exit 2; fi
cd "$W" || exit 2
run 02-green-regressions go test -count=1 -v -run 'TestSolPREL1UnsupportedSwapRefused|TestOpusPRE01HardLinkedUpdateRefusedBeforeAnyWrite' ./internal/editing || exit 2
run 03-gofmt gofmt -l internal/editing || exit 2
if grep -q '^internal/' "$E/logs/03-gofmt.txt"; then printf 'gofmt output is not empty\n'; exit 2; fi
run 04-editing-normal go test -count=1 ./internal/editing || exit 2
run 05-editing-race go test -race -count=1 ./internal/editing || exit 2
run 06-four-package-race go test -race -count=1 ./internal/editing ./internal/toolset ./internal/agent ./internal/session || exit 2
run 07-build go build ./... || exit 2
run 08-vet go vet ./... || exit 2
printf 'All requested gates passed once.\n'
