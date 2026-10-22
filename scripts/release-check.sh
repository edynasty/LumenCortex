#!/usr/bin/env bash
# Pre-release gate for the LumenCortex Core Go module.
#
# Checks the repository state, the version/tag agreement and the toolchain, then runs the full Go
# checks. It creates nothing and never tags or pushes.
#
# Usage: scripts/release-check.sh [--mode local|release] [--allow-dirty] [--skip-tests]
#
#   --mode release  A publishable Core release: clean tree and the exact stable SemVer tag
#                   v<runtime version> on HEAD. Core must be released before Desktop.
#   --mode local    A local check; no tag is required. Default.
#   --allow-dirty   Local mode only: list uncommitted changes as warnings instead of failing.
#   --skip-tests    Run only the static checks.
#
# Version agreement: runtime.GoRuntimePreviewVersion is the runtime API version. Its numeric core
# (X.Y.Z) must equal the module tag, so a tag v0.1.0 ships a runtime reporting 0.1.0-preview.
set -euo pipefail

cd "$(dirname "$0")/.."

die() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

step() {
  printf '\n== %s ==\n' "$*"
}

mode=local
allow_dirty=0
skip_tests=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --mode) mode="${2:-}"; shift 2 ;;
    --mode=*) mode="${1#*=}"; shift ;;
    --allow-dirty) allow_dirty=1; shift ;;
    --skip-tests) skip_tests=1; shift ;;
    -h|--help) sed -n '2,18p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done
[[ "$mode" == local || "$mode" == release ]] || die "--mode must be local or release (got: $mode)"
if [[ "$allow_dirty" -eq 1 && "$mode" == release ]]; then
  die "--allow-dirty cannot be combined with --mode release"
fi

step "Repository state (mode: $mode)"
grep -q '^module github.com/edynasty/LumenCortex$' go.mod || die "go.mod is not the LumenCortex Core module"
runtime_version="$(sed -n 's/^const GoRuntimePreviewVersion = "\(.*\)"$/\1/p' runtime/engine.go | sed -n '1p')"
[[ -n "$runtime_version" ]] || die "cannot read GoRuntimePreviewVersion from runtime/engine.go"
numeric_version="${runtime_version%%-*}"
[[ "$numeric_version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] ||
  die "GoRuntimePreviewVersion '${runtime_version}' does not start with X.Y.Z"
expected_tag="v$numeric_version"
commit="$(git rev-parse HEAD)"
branch="$(git rev-parse --abbrev-ref HEAD)"
printf 'commit: %s\nbranch: %s\nruntime version: %s\nexpected tag: %s\n' \
  "$commit" "$branch" "$runtime_version" "$expected_tag"

tag=""
if tag_at_head="$(git describe --tags --exact-match HEAD 2>/dev/null)"; then
  tag="$tag_at_head"
fi
if [[ -n "$tag" && "$tag" != "$expected_tag" ]]; then
  die "tag $tag does not agree with GoRuntimePreviewVersion $runtime_version (expected $expected_tag)"
fi
if [[ "$mode" == release ]]; then
  [[ -n "$tag" ]] || die "release mode requires the exact tag $expected_tag on HEAD (none found)"
  [[ "$expected_tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] ||
    die "Core releases require a stable SemVer tag such as v0.1.0"
  printf 'tag: %s (HEAD)\n' "$tag"
else
  printf 'tag: %s\n' "${tag:-none (local check)}"
fi

status="$(git status --porcelain --untracked-files=normal)"
tracked_changes="$(printf '%s\n' "$status" | grep -v '^?? ' | grep -v '^$' || true)"
untracked_files="$(printf '%s\n' "$status" | grep '^?? ' || true)"
if [[ -n "$tracked_changes" || -n "$untracked_files" ]]; then
  printf '\nUncommitted state:\n' >&2
  [[ -z "$tracked_changes" ]] || printf 'tracked modifications:\n%s\n' "$tracked_changes" >&2
  [[ -z "$untracked_files" ]] || printf 'untracked files (not ignored):\n%s\n' "$untracked_files" >&2
  if [[ "$allow_dirty" -eq 1 ]]; then
    printf 'Continuing because --allow-dirty was given.\n' >&2
  else
    die "working tree is not clean; commit or stash the changes (ignored files are allowed)"
  fi
else
  printf 'OK: working tree clean\n'
fi

step "Toolchain"
printf 'go: %s\n' "$(go version 2>/dev/null || printf 'missing')"
printf 'git: %s\n' "$(git --version 2>/dev/null || printf 'missing')"
printf 'platform: %s (%s)\n' "$(uname -s 2>/dev/null || printf 'unknown')" "$(uname -m 2>/dev/null || printf 'unknown')"
printf 'cgo: %s\n' "$(go env CGO_ENABLED 2>/dev/null || printf 'unknown')"

if [[ "$skip_tests" -eq 1 ]]; then
  step "Tests skipped (--skip-tests)"
else
  step "Go checks"
  go mod verify
  go build ./...
  go vet ./...
  go test -count=1 ./...
fi

step "Result"
printf 'OK: release check passed (mode=%s, runtime %s, commit %s)\n' "$mode" "$runtime_version" "$commit"
