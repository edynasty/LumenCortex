#!/usr/bin/env bash
# Run from the R7 Core checkout. Cross-target commands compile or vet only.
set -eu -o pipefail
root=$(git rev-parse --show-toplevel)
cd "$root"
go version
uname -sm
git rev-parse HEAD
base=4aaf8e7168743d6c8ae8fa6fe87a350ca69f9af2
packages=(./internal/editing ./internal/toolset ./internal/agent ./internal/session)
run() {
    printf 'cmd='; printf ' %q' "$@"; printf '\n'
    local status=0
    "$@" || status=$?
    printf 'exit=%d\n' "$status"
    return "$status"
}
case ${1:-native} in
    native)
        files=()
        while IFS= read -r -d '' file; do files+=("$file"); done < <(git diff --name-only --diff-filter=ACMR -z "$base" HEAD -- '*.go')
        if ((${#files[@]})); then
            unformatted=$(gofmt -l "${files[@]}")
            if [[ -n "$unformatted" ]]; then printf '%s\n' "$unformatted"; exit 1; fi
            printf 'gofmt clean: %d Go files\n' "${#files[@]}"
        fi
        run go build ./...
        run go vet ./...
        run go test -count=1 "${packages[@]}"
        run go test -race -count=1 "${packages[@]}"
        run go test -race -count=1 -run 'TestT3|TestT4' ./runtime
        ;;
    cross)
        for target in linux/arm64 linux/amd64 windows/amd64 freebsd/amd64; do
            target_env=(env "GOOS=${target%/*}" "GOARCH=${target#*/}" CGO_ENABLED=0)
            run "${target_env[@]}" go build "${packages[@]}"
            run "${target_env[@]}" go vet "${packages[@]}"
        done
        tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
        for pkg in "${packages[@]}"; do
            run env GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go test -c -o "$tmp/${pkg##*/}.test.exe" "$pkg"
        done
        ;;
    repo)
        run go test -count=1 ./...
        ;;
    runtime-extended)
        run go test -timeout=45m -count=1 ./runtime
        ;;
    *) exit 2 ;;
esac
