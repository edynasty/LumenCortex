#!/bin/bash
set -u
V=/private/tmp/lc-ov-remediation; O=$V/oracle; WT=/Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt
for pair in "astra:core-adr-0024-sol-rev-01-astra-final-20261122" "remediation:core-adr-0024-opus-ov-remediation-20261122"; do
  n=${pair%%:*}; w=$WT/${pair#*:}; cd "$w"
  { echo "variant=$n HEAD=$(git rev-parse HEAD) editing.go=$(git rev-parse HEAD:internal/toolset/editing.go) GOMAXPROCS=2 $(go version) $(sysctl -n machdep.cpu.brand_string)"
    echo "cmd: GOMAXPROCS=2 go test -overlay $O/overlay-bench-$n.json -run '^\$' -bench '^BenchmarkOVScale\$' -benchmem -count=3 -timeout=60m ./internal/toolset"
    GOMAXPROCS=2 go test -overlay "$O/overlay-bench-$n.json" -run '^$' -bench '^BenchmarkOVScale$' -benchmem -count=3 -timeout=60m ./internal/toolset 2>&1; echo "exit $?"
    echo "status-after=[$(git status --porcelain=v1 --untracked-files=all | tr '\n' ';')]"; } > "$V/final/bench-$n.log" 2>&1
done
echo BENCH_DONE >> $V/final/bench-remediation.log
