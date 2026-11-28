#!/usr/bin/env bash
# run-regex.sh - print a `go test -run` regex that selects exactly the top-level tests declared in
# the given test files (one package at a time). Helper-process entry points (TestHelper*) and
# TestMain are left out: they skip themselves when run directly.
#
# Usage: run-regex.sh runtime/worktree_*_test.go runtime/deletion*_test.go
#        go test -count=1 -timeout=20m -run "$(run-regex.sh runtime/worktree_*_test.go)" ./runtime
set -euo pipefail
(($#)) || { echo "usage: run-regex.sh <file_test.go>..." >&2; exit 2; }
dirs=$(for f in "$@"; do dirname "$f"; done | sort -u)
[[ $(wc -l <<<"$dirs") -eq 1 ]] || { echo "run-regex.sh: files span several packages; call once per package" >&2; exit 2; }
names=$(sed -n -E 's/^func (Test[A-Za-z0-9_]*)\(t \*testing\.T\).*/\1/p' "$@" | grep -v -E '^(TestMain$|TestHelper)' | sort -u | paste -sd'|' -)
[[ -n $names ]] || { echo "run-regex.sh: no tests in the given files" >&2; exit 1; }
printf '^(%s)$\n' "$names"
