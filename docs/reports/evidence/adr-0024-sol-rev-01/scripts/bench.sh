#!/usr/bin/env bash
# Corpus benchmark (Opus, Sol, new) then scale benchmark (Sol, new), sequentially on an idle host.
set -u
R=$(cat /private/tmp/lc-sol-indep-review.current); E3=$R/rev01-evidence
go version; uname -sm; sysctl -n machdep.cpu.brand_string
for v in opus sol new; do
  echo "== corpus $v $(date -u +%FT%TZ)"
  (cd $R/trees3/$v && GOMAXPROCS=2 REVIEW_CORPUS_IN=$R/corpus/sol/in go test -count=5 -run '^$' -bench '^BenchmarkReviewCorpus$' -benchtime=3x -benchmem ./internal/toolset > $E3/bench/corpus-$v.log 2>&1; echo "exit=$?")
done
for v in sol new; do
  echo "== scale $v $(date -u +%FT%TZ)"
  (cd $R/trees3/$v && GOMAXPROCS=2 go test -count=1 -run '^$' -bench '^BenchmarkReviewScale$' -benchtime=2x -benchmem -timeout 60m ./internal/toolset > $E3/bench/scale-$v.log 2>&1; echo "exit=$?")
done
echo BENCH_DONE
