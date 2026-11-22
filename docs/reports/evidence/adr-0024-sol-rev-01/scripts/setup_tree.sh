#!/usr/bin/env bash
# setup_tree.sh SCRATCH NAME SOURCE
# Export a Core tree into SCRATCH/trees3/NAME and add the v2 review probes. SOURCE is a commit SHA, or
# "worktree:<path>" for a working tree. Run from a Core checkout; nothing is written to the checkout.
set -euo pipefail
scratch=$1 name=$2 src=$3
here=$(cd "$(dirname "$0")/.." && pwd)
dst=$scratch/trees3/$name
rm -rf "$dst"; mkdir -p "$dst"
if [[ $src == worktree:* ]]; then
  rsync -a --exclude .git "${src#worktree:}/" "$dst/"
else
  git archive "$src" | tar -x -C "$dst"
fi
probes=(zz_review_corpus_dump_test zz_review_corpus_helpers_test zz_review_adv_common_test zz_review_adv_render_test zz_review_sweep_test zz_review_bench_test)
if [[ $name != opus ]]; then
  probes+=(zz_review_adv_check_test zz_review_adv_cases_test zz_review_corpus_oracle_test)
fi
for p in "${probes[@]}"; do cp "$here/inputs/probes-v2/$p.go.txt" "$dst/internal/toolset/$p.go"; done
(cd "$dst" && go vet ./internal/toolset) && echo "tree $name ready"
