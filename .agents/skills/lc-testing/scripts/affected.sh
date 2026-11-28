#!/usr/bin/env bash
# affected.sh — L0 preflight for lc-testing: what changed, which Go packages it touches, which
# in-module packages import them, and which risk flags apply. Read-only: runs git and go list.
#
# Usage: affected.sh [BASE [TARGET]]   (run from anywhere inside the Core worktree)
#   BASE defaults to `git merge-base HEAD main`. Without TARGET: committed (BASE...HEAD), staged,
#   unstaged and untracked changes. With TARGET (a commit): only the committed range BASE...TARGET,
#   read from Git objects (to plan a review of a range without checking it out); the package graph
#   and runtime groups still come from this checkout, so run it in a checkout close to TARGET.
# Output: sections CHANGED, PACKAGES, IMPORTERS, FLAGS, RUNTIME GROUPS. Exit 0 on success, 2 on usage/setup error.
set -euo pipefail
export GIT_OPTIONAL_LOCKS=0
unset GOFLAGS

root=$(git rev-parse --show-toplevel 2>/dev/null) || { echo "affected.sh: not inside a git worktree" >&2; exit 2; }
cd "$root"
[[ -f go.mod ]] || { echo "affected.sh: no go.mod at $root" >&2; exit 2; }
module=$(go list -m)
base=${1:-$(git merge-base HEAD main 2>/dev/null || true)}
target=${2:-}
[[ -n "$base" ]] || { echo "affected.sh: cannot resolve BASE; pass one explicitly" >&2; exit 2; }
git rev-parse --verify -q "$base^{commit}" >/dev/null || { echo "affected.sh: unknown BASE $base" >&2; exit 2; }
if [[ -n "$target" ]]; then
  git rev-parse --verify -q "$target^{commit}" >/dev/null || { echo "affected.sh: unknown TARGET $target" >&2; exit 2; }
  changed=$(git diff --name-only "$base"...$target | sort -u)
  echo "BASE $(git rev-parse --short "$base")  TARGET $(git rev-parse --short "$target")  (committed range only; package graph from HEAD $(git rev-parse --short HEAD))"
else
  changed=$({ git diff --name-only "$base"...HEAD; git diff --name-only HEAD; git ls-files --others --exclude-standard; } | sort -u)
  echo "BASE $(git rev-parse --short "$base")  HEAD $(git rev-parse --short HEAD)  dirty=$([[ -n $(git status --porcelain) ]] && echo yes || echo no)"
fi
echo "== CHANGED"
[[ -n "$changed" ]] && printf '%s\n' "$changed" || echo "(none)"

# A changed file is a Go build/test input when it is a .go file, go.mod/go.sum, or a non-Markdown
# file inside a directory that holds Go files (embedded assets, testdata). Markdown counts only
# where it is embedded (config/presets).
pkgdirs=""
wide=""
while IFS= read -r f; do
  [[ -z "$f" ]] && continue
  case "$f" in
    go.mod|go.sum) wide="$wide $f"; continue ;;
  esac
  d=$(dirname "$f")
  if [[ "$f" == *.go ]]; then
    pkgdirs="$pkgdirs"$'\n'"$d"; continue
  fi
  # walk up to the nearest directory with Go files (testdata/ lives below its package)
  probe=$d
  while [[ "$probe" != "." && "$probe" != "/" ]]; do
    if compgen -G "$probe/*.go" >/dev/null; then
      if [[ "$f" != *.md || "$probe" == config ]]; then pkgdirs="$pkgdirs"$'\n'"$probe"; fi
      break
    fi
    probe=$(dirname "$probe")
  done
done <<<"$changed"
pkgdirs=$(printf '%s\n' "$pkgdirs" | sed '/^$/d' | sort -u)

echo "== PACKAGES"
pkgs=""
if [[ -n "$pkgdirs" ]]; then
  while IFS= read -r d; do
    if [[ -d "$d" ]]; then p=$(go list "./$d" 2>/dev/null || echo "$module/$d (deleted or not buildable)"); else p="$module/$d (deleted)"; fi
    pkgs="$pkgs"$'\n'"$p"
  done <<<"$pkgdirs"
  pkgs=$(printf '%s\n' "$pkgs" | sed '/^$/d' | sort -u)
  printf '%s\n' "$pkgs"
else
  echo "(none: no Go build or test input changed)"
fi

echo "== IMPORTERS (direct, in-module, incl. test imports; not the changed packages themselves)"
importers=""
if [[ -n "$pkgs" ]]; then
  graph=$(go list -f '{{.ImportPath}}{{range .Imports}} {{.}}{{end}}{{range .TestImports}} {{.}}{{end}}{{range .XTestImports}} {{.}}{{end}}' ./...)
  importers=$(printf '%s\n' "$pkgs" | awk '{print $1}' | while IFS= read -r p; do
    printf '%s\n' "$graph" | awk -v p="$p" '{for(i=2;i<=NF;i++) if($i==p){print $1; break}}'
  done | sort -u | grep -vxF -f <(printf '%s\n' "$pkgs" | awk '{print $1}') || true)
  [[ -n "$importers" ]] && printf '%s\n' "$importers" || echo "(none)"
  total=$(printf '%s\n' "$graph" | wc -l | tr -d ' ')
  n=$(printf '%s\n' "$importers" | sed '/^$/d' | wc -l | tr -d ' ')
  echo "($n of $total module packages)"
else
  echo "(none)"
fi

echo "== FLAGS"
flags=()
# content of a changed Go file: the working tree, or TARGET's blob in range mode (deleted files: none)
content() { if [[ -n "$target" ]]; then git show "$target:$1" 2>/dev/null || true; elif [[ -f "$1" ]]; then cat "$1"; fi; }
gofiles=$(printf '%s\n' "$changed" | grep '\.go$' || true)
has() {
  [[ -n "$gofiles" ]] || return 1
  local f c
  while IFS= read -r f; do c=$(content "$f"); grep -qE "$1" <<<"$c" && return 0; done <<<"$gofiles"
  return 1
}
[[ -z "$pkgs" && -z "$wide" ]] && flags+=("DOCS_ONLY: no Go tests required (L0)")
[[ -n "$wide" ]] && flags+=("WIDE:$wide changed -> full module run")
printf '%s\n' "$pkgs" | grep -qE "^$module/(config|protocol)( |$)" && flags+=("WIDE: config/protocol changed -> full module run")
printf '%s\n' "$pkgs" | grep -qE "^$module/runtime( |$)" && flags+=("RUNTIME: runtime package (~11 min whole); L2 runs its groups (see RUNTIME GROUPS)")
printf '%s\n' "$changed" | grep -qE '^internal/migrate/' && flags+=("MIGRATION: schema/migration -> L3 (reliability.md §1)")
printf '%s\n' "$changed" | grep -qE '^(provider|protocol)/' && flags+=("PROVIDER_WIRE -> prompt-tools-cache.md")
printf '%s\n' "$changed" | grep -qE '^(internal/window|internal/taskstate|internal/session/projection|internal/agent/(receipt_notes|window_loop|send|envelope))' && flags+=("CONTEXT_CACHE -> context-cognitive.md, prompt-tools-cache.md")
printf '%s\n' "$changed" | grep -qE '(^runtime/cognition|^internal/cognition/|^internal/knowledge/|run_posture)' && flags+=("COGNITIVE -> context-cognitive.md (L3)")
printf '%s\n' "$changed" | grep -qE '_test\.go$|/testdata/' && flags+=("TEST_CHANGE: list every changed assertion in a Test changes table")
printf '%s\n' "$changed" | grep -qE '^(\.github/|scripts/)' && flags+=("CI_SCRIPTS: bash -n; release-check --skip-tests; hosted CI NOT_RUN(hosted)")
has '(^|[^A-Za-z])go func|sync\.(Mutex|RWMutex|WaitGroup|Once|Cond)|atomic\.|<-|make\(chan' && flags+=("CONCURRENCY: -race on affected packages (L3)")
has 'database/sql|\.ExecContext\(|\.QueryContext\(|BeginTx\(|\bCOMMIT\b' && flags+=("PERSISTENCE: transaction/close-reopen tests (L3)")
has 'exec\.Command|os/exec|syscall\.(Kill|Setpgid|SIGKILL)|Setpgid|Process\.Kill' && flags+=("PROCESS: exit/timeout/orphan cleanup tests (L3)")
has '^//go:build' && flags+=("PLATFORM: build-constrained files -> native run + cross-compile (reliability.md §6)")
if ((${#flags[@]})); then printf '%s\n' "${flags[@]}"; else echo "(none: L1, then L2 for the listed packages if logic crosses functions)"; fi
echo "(CONCURRENCY/PERSISTENCE/PROCESS/PLATFORM scan whole changed files, not only changed lines: confirm against the diff)"

# Suggest runtime test groups from the stems of changed runtime files (runtime/worktree_safety.go ->
# stem worktree -> group whose file globs match worktree*_test.go).
rt=$(printf '%s\n' "$changed" | sed -n -E 's#^runtime/([^/]+)\.go$#\1#p' | sed -E 's/_test$//; s/_(unix|linux|darwin|windows)$//')
if [[ -n "$rt" ]]; then
  echo "== RUNTIME GROUPS (suggested by file stem; scripts/runtime-group.sh)"
  here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
  listing=$("$here/runtime-group.sh" --list)
  set -f   # the group globs are patterns to compare, not to expand here
  while IFS= read -r stem; do
    hit=""
    while read -r group globs; do
      for g in $globs; do
        # shellcheck disable=SC2053
        if [[ ${stem}_test.go == $g || ${stem%%_*}_test.go == $g || ${stem%%_*}_x_test.go == $g ]]; then hit+="$group "; break; fi
      done
    done <<<"$listing"
    printf '%-28s %s\n' "runtime/$stem" "${hit:-(no group: pick by content, or run the whole package)}"
  done < <(sort -u <<<"$rt")
  set +f
fi
