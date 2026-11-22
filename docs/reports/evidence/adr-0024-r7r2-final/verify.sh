#!/usr/bin/env bash
# Replays the R7R2 final audit from a Core checkout that contains this directory. Every mode prints
# the command and its exit code. RED modes expect the listed tests to fail and say so. Cross-target
# commands compile or vet only. Nothing here writes to the checkout: overlays and exported baselines
# live in a temporary directory that is removed on exit.
set -eu -o pipefail
root=$(git rev-parse --show-toplevel)
cd "$root"
here=docs/reports/evidence/adr-0024-r7r2-final
r6=4aaf8e7168743d6c8ae8fa6fe87a350ca69f9af2
r7=31172bc274c3a4c54243a72e554862d539d7759d
r7r2=fc1c4368507bea3f4e13099c62ca35936a6fba48
carbon=f3f77e35c98de4647243019568ed5ec3093cf072
packages=(./internal/editing ./internal/toolset ./internal/agent ./internal/session)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

go version
uname -sm
echo "HEAD $(git rev-parse HEAD)"
if [[ -n $(git status --porcelain) ]]; then echo "working tree: modified"; else echo "working tree: clean"; fi

run() {
    printf 'cmd='; printf ' %q' "$@"; printf '\n'
    local status=0
    "$@" || status=$?
    printf 'exit=%d\n' "$status"
    return "$status"
}
# expect_fail runs a RED step: the command must fail.
expect_fail() {
    printf 'cmd(expected to fail)='; printf ' %q' "$@"; printf '\n'
    local status=0
    "$@" || status=$?
    printf 'exit=%d\n' "$status"
    if ((status == 0)); then echo "UNEXPECTED: the RED step passed"; return 1; fi
    echo "RED as expected"
}
# overlay NAME TARGET=SOURCE... writes a go build overlay that replaces or adds TARGET (a path in
# the checkout) with SOURCE, and prints the overlay's path.
overlay() {
    local name=$1 sep='' pair
    shift
    {
        printf '{"Replace": {'
        for pair in "$@"; do
            printf '%s"%s/%s": "%s"' "$sep" "$root" "${pair%%=*}" "${pair#*=}"
            sep=', '
        done
        printf '}}\n'
    } > "$tmp/$name.json"
    echo "$tmp/$name.json"
}
# carbon_editing_with_stub is internal/toolset/editing.go as of the Carbon commit plus a no-op for
# the one helper the new regression tests call, so those tests compile against the unfixed file.
carbon_editing_with_stub() {
    {
        git show "$carbon:internal/toolset/editing.go"
        printf '\n// RED fixture: %s has no distinguishShortenedPaths.\n' "$carbon"
        printf 'func distinguishShortenedPaths(rows []toolFileState, n int, pathAt func(int) string) {}\n'
    } > "$tmp/editing_carbon_stub.go"
    echo "$tmp/editing_carbon_stub.go"
}
mutant_m4() {
    cp internal/editing/publish_file.go "$tmp/publish_file_m4.go"
    patch -s "$tmp/publish_file_m4.go" < "$here/inputs/mutant-m4.diff.txt"
    echo "$tmp/publish_file_m4.go"
}
probe() { cp "$here/inputs/probes/$1.txt" "$tmp/$1"; echo "$tmp/$1"; }

case ${1:-native} in
    native)
        files=()
        while IFS= read -r -d '' file; do files+=("$file"); done < <(git diff --name-only --diff-filter=ACMR -z "$carbon" HEAD -- '*.go')
        unformatted=$(gofmt -l "${files[@]}")
        if [[ -n "$unformatted" ]]; then printf '%s\n' "$unformatted"; exit 1; fi
        printf 'gofmt clean: %d Go files changed since %s\n' "${#files[@]}" "$carbon"
        run go build ./...
        run go vet ./...
        run go test -count=1 ./internal/toolset ./internal/editing
        run go test -race -count=1 ./internal/toolset ./internal/editing
        run go test -race -count=1 ./internal/agent ./internal/session
        run go test -race -count=1 -run 'TestT3|TestT4' ./runtime
        ;;
    focused)
        # The R7 partial-commit, result-propagation and Resume regressions, and every R7R2 test.
        run go test -race -count=1 -v -run '^TestR7|^TestR201|^TestR5ResultPropagationAndResumeIntegrity$|^TestR6NestedJSONPreservationAndProvenance$|^TestR6StructuredPathsAndSanitizeErrorText$' ./internal/editing ./internal/toolset ./internal/agent
        ;;
    new01-original-red)
        # R7R2-NEW-01 as Carbon found it: Carbon's regression tests against the R7R2 baseline file.
        git show "$r7r2:internal/toolset/editing.go" > "$tmp/editing_r7r2.go"
        printf '\n// RED fixture: %s has no distinguishShortenedPaths.\nfunc distinguishShortenedPaths(rows []toolFileState, n int, pathAt func(int) string) {}\n' "$r7r2" >> "$tmp/editing_r7r2.go"
        expect_fail go test -count=1 -overlay "$(overlay r7r2 internal/toolset/editing.go="$tmp/editing_r7r2.go")" -run '^TestR7R2New01' ./internal/toolset
        ;;
    new01-final-red)
        # R7R2-FINAL-01..03: the new regression tests against the Carbon commit's file.
        expect_fail go test -count=1 -overlay "$(overlay carbon internal/toolset/editing.go="$(carbon_editing_with_stub)")" -run '^TestR7R2Final' ./internal/toolset
        ;;
    new01-green)
        run go test -count=1 -v -run '^TestR7R2' ./internal/toolset
        ;;
    carbon-probe)
        # Carbon's original 17-subtest probe (archived from its review workspace) on this checkout.
        cp "$here/inputs/carbon-original-probe-r7r2_results_test.go.txt" "$tmp/carbon_probe_test.go"
        run go test -count=1 -v -overlay "$(overlay probe internal/toolset/r7r2_results_test.go="$tmp/carbon_probe_test.go")" -run '^TestCarbonR7R2' ./internal/toolset
        ;;
    carbon-probe-at-carbon)
        # The same probe against the Carbon commit's file: the case Carbon reported is fixed there.
        cp "$here/inputs/carbon-original-probe-r7r2_results_test.go.txt" "$tmp/carbon_probe_test.go"
        run go test -count=1 -v -overlay "$(overlay probe-carbon internal/toolset/r7r2_results_test.go="$tmp/carbon_probe_test.go" internal/toolset/editing.go="$(carbon_editing_with_stub)")" -run '^TestCarbonR7R2Results01AdversarialMatrix$/^multiple_failed_files_5_very_long_dirs_overflow_order$|^TestR7R2New01' ./internal/toolset
        ;;
    differential)
        # Every payload of a 684-case corpus, rendered by the Carbon commit's file and by HEAD.
        files=(internal/toolset/zz_opus_new01_probe_test.go="$(probe zz_opus_new01_probe_test.go)"
            internal/toolset/zz_opus_new01_dump_test.go="$(probe zz_opus_new01_dump_test.go)")
        run env OPUS_DUMP_DIR="$tmp/dump-carbon" go test -count=1 -overlay "$(overlay dump-carbon "${files[@]}" internal/toolset/editing.go="$(carbon_editing_with_stub)")" -run '^TestOpusProbeDump$' ./internal/toolset
        run env OPUS_DUMP_DIR="$tmp/dump-head" go test -count=1 -overlay "$(overlay dump-head "${files[@]}")" -run '^TestOpusProbeDump$' ./internal/toolset
        run python3 "$here/compare_payloads.py" "$tmp/dump-carbon" "$tmp/dump-head"
        ;;
    new02)
        run go test -count=35 -run '^TestR201TwoOSProcessesCreateRace$' ./internal/editing
        ;;
    new02-soak)
        run go test -count=100 -v -run '^TestR201TwoOSProcessesCreateRace$' ./internal/editing
        run go test -race -count=20 -run '^TestR201TwoOSProcessesCreateRace$' ./internal/editing
        run env GOMAXPROCS=1 go test -count=20 -run '^TestR201TwoOSProcessesCreateRace$' ./internal/editing
        ;;
    new02-carbon)
        # The Carbon commit's version of the test on this checkout's production code, with its
        # stress tallies: every stress round ends the same way.
        git show "$carbon:internal/editing/remediation_r2_01_test.go" > "$tmp/r201_carbon_test.go"
        run go test -count=35 -v -overlay "$(overlay r201-carbon internal/editing/remediation_r2_01_test.go="$tmp/r201_carbon_test.go")" -run '^TestR201TwoOSProcessesCreateRace$' ./internal/editing
        ;;
    new02-original)
        # The test as it was before Carbon's change, 35 runs, and Carbon's deterministic reproducer
        # of the outcome that test rejects.
        git show "$r7r2:internal/editing/remediation_r2_01_test.go" > "$tmp/r201_r7r2_test.go"
        run go test -count=35 -v -overlay "$(overlay r201-r7r2 internal/editing/remediation_r2_01_test.go="$tmp/r201_r7r2_test.go")" -run '^TestR201TwoOSProcessesCreateRace$' ./internal/editing || true
        cp "$here/inputs/carbon-red-new02-remediation_r2_01_test.go.txt" "$tmp/r201_carbon_red_test.go"
        expect_fail go test -count=1 -v -overlay "$(overlay r201-red internal/editing/remediation_r2_01_test.go="$tmp/r201_carbon_red_test.go")" -run '^TestR201DeterministicRound8RedReproducer$' ./internal/editing
        ;;
    new02-mutant)
        # Mutant M4 reports bytes mixed into LC's in-place write as a clean refusal. The Carbon
        # commit's suites pass on it; this checkout's test fails.
        m4=$(mutant_m4)
        git show "$carbon:internal/editing/remediation_r2_01_test.go" > "$tmp/r201_carbon_test.go"
        run go test -count=1 -overlay "$(overlay m4-carbon internal/editing/publish_file.go="$m4" internal/editing/remediation_r2_01_test.go="$tmp/r201_carbon_test.go")" ./internal/editing ./internal/toolset ./internal/agent
        expect_fail go test -count=1 -overlay "$(overlay m4-head internal/editing/publish_file.go="$m4")" -run '^TestR201TwoOSProcessesCreateRace$' ./internal/editing
        ;;
    pre02)
        # The two truncate-window interleavings on R6, R7, R7R2, the Carbon commit and HEAD.
        run git diff --stat "$r7r2" HEAD -- internal/editing ':!internal/editing/*_test.go'
        echo "(no lines above: internal/editing production code is unchanged since $r7r2)"
        for pair in "r6:$r6" "r7:$r7" "r7r2:$r7r2" "carbon:$carbon" "head:$(git rev-parse HEAD)"; do
            name=${pair%%:*}
            mkdir -p "$tmp/base-$name"
            git archive "${pair#*:}" | tar -x -C "$tmp/base-$name"
            variant=zz_opus_pre02_baseline_test.go
            if [[ $name == r6 ]]; then variant=zz_opus_pre02_baseline_r6_variant_test.go; fi
            cp "$here/inputs/probes/$variant.txt" "$tmp/base-$name/internal/editing/zz_opus_pre02_baseline_test.go"
            echo "== $name ${pair#*:}"
            (cd "$tmp/base-$name" && run go test -count=1 -v -run '^TestOpusPRE02Baseline$' ./internal/editing)
        done
        ;;
    coverage)
        # Whether any test of the four packages reaches the branch of writeInPlace that reports
        # bytes another writer mixed into LC's in-place write, at the Carbon commit and at HEAD.
        # The last number of each coverage line is the hit count; one line per package run.
        block='publish_file.go:199.33,203.3'
        sed -n '202p' internal/editing/publish_file.go | grep -q 'another writer changed the file during the write'
        mkdir -p "$tmp/cov-carbon"
        git archive "$carbon" | tar -x -C "$tmp/cov-carbon"
        echo "== carbon $carbon"
        (cd "$tmp/cov-carbon" && run go test -count=1 -coverpkg=./internal/editing -coverprofile="$tmp/carbon.out" "${packages[@]}")
        grep -F "$block" "$tmp/carbon.out"
        echo "== head $(git rev-parse HEAD)"
        run go test -count=1 -coverpkg=./internal/editing -coverprofile="$tmp/head.out" "${packages[@]}"
        grep -F "$block" "$tmp/head.out"
        ;;
    cross)
        for target in linux/arm64 linux/amd64 windows/amd64 windows/arm64 freebsd/amd64; do
            run env "GOOS=${target%/*}" "GOARCH=${target#*/}" CGO_ENABLED=0 go vet "${packages[@]}"
        done
        for pkg in "${packages[@]}"; do
            run env GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go test -c -o "$tmp/${pkg##*/}.test.exe" "$pkg"
        done
        ;;
    repo)
        run go test -timeout=45m -count=1 ./...
        ;;
    *) exit 2 ;;
esac
