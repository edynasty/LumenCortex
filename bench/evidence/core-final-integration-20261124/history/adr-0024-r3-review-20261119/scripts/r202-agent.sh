#!/bin/bash
# R3 review probe runner (throwaway, /tmp only): R2-02 / R2-04 agent probes, then full agent -race.
set -u
R=/tmp/lc-adr0024-r3
F=$R/fresh
cd $R/probe && go test -c -o $F/ag-head.test ./internal/agent || exit 1
cd $R/prevprobe && go test -c -o $F/ag-prev.test ./internal/agent || exit 1
cd $F
for v in prev head; do
  echo "##### giant newest group: live vs cold resume ($v)"
  ./ag-$v.test -test.count=1 -test.v -test.timeout=10m -test.run '^TestZZR3GiantNewestLiveVsResume$' 2>&1 | grep -E 'zz_probe|^--- ' | cut -c1-2600
done
echo "##### failover before relaxation (head)"
./ag-head.test -test.count=1 -test.v -test.timeout=15m -test.run '^TestZZR3FailoverBeforeRelaxation$' 2>&1 | grep -E 'zz_probe|^--- ' | cut -c1-1800
echo "##### fold differential (head)"
./ag-head.test -test.count=1 -test.v -test.timeout=30m -test.run '^TestZZR3FoldDifferential$' 2>&1 | grep -E 'zz_probe|^--- ' | cut -c1-900
echo "##### resume parity, five configs (head)"
./ag-head.test -test.count=1 -test.v -test.timeout=40m -test.run '^TestZZR3ResumeParity$' > $F/resume-parity-fresh.log 2>&1
echo "resume parity exit=$?"
echo "##### full internal/agent -race (clean head snapshot)"
cd $R/head && go test -race -count=1 -timeout 30m ./internal/agent 2>&1 | tail -15
