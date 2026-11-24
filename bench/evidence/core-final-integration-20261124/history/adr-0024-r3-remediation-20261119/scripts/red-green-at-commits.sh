#!/usr/bin/env bash
# Commit-stamped RED/GREEN for the R3 fixes: each new regression test runs against the parent commit's
# production code (RED) and against its own fix commit (GREEN). Snapshots are `git archive` trees.
set -u
WT=/Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-r3-20261119
O=/tmp/lc-adr0024-r3/remed/rg
rm -rf $O; mkdir -p $O
full() { git -C "$WT" rev-parse --verify "$1^{commit}"; }
echo "# R3 commit-stamped RED/GREEN; $(date -u +%FT%TZ); $(sw_vers -productName) $(sw_vers -productVersion) $(uname -m); $(go version | cut -d' ' -f3-)"
# finding | fix commit | test files (taken from the fix commit) | packages | -run
run_case() {
  local id=$1 fix=$2 files=$3 pkg=$4 re=$5
  local F P; F=$(full "$fix"); P=$(full "$fix^")
  for side in RED GREEN; do
    local base=$P; [ $side = GREEN ] && base=$F
    local S=$O/$id-$side; mkdir -p $S; git -C "$WT" archive "$base" | tar -x -C $S
    for f in $files; do git -C "$WT" show "$F:$f" > "$S/$f"; done
    echo "##### $id $side: production code $base + tests from $F ($files)"
    (cd $S && go test -count=1 -run "$re" "$pkg" 2>&1 | grep -E '^(--- |    --- |ok|FAIL|#|.*\.go:[0-9]+:[0-9]+:)' | cut -c1-200; echo "exit=${PIPESTATUS[0]}")
    rm -rf $S
  done
}
run_case R3-01 87a3166711277c0ea311374f0eecaeb03d0d4b7d "internal/editing/remediation_r3_01_unix_test.go" ./internal/editing '^TestR301'
run_case R3-04 b7830e4c4737bab130cc0bc3638c771c54c97639 "internal/agent/remediation_r3_04_test.go" ./internal/agent '^TestR304'
run_case R3-03 0155df659341dfa5e92904d461da669f69b3f305 "internal/agent/remediation_r3_03_test.go internal/agent/remediation_f02_test.go" ./internal/agent '^(TestR303|TestF02)'
