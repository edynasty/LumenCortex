#!/usr/bin/env bash
# R3 remediation: real exFAT probe (macOS) + Linux ARM64 container editing/toolset tests at the
# committed code candidate. Snapshot is `git archive` of the commit, so the tree is exactly that commit.
set -u
WT=/Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt/core-adr-0024-r3-20261119
R=/tmp/lc-adr0024-r3
O=$R/remed
C=$(git -C "$WT" rev-parse --verify 0155df659341dfa5e92904d461da669f69b3f305^{commit}) || exit 1
S=$O/snap-$C
echo "# R3 post-commit FS + Linux verification; code candidate $C (git archive snapshot); $(date -u +%FT%TZ); $(sw_vers -productName) $(sw_vers -productVersion) $(uname -m); $(go version | cut -d' ' -f3-)"
rm -rf "$S"; mkdir -p "$S"; git -C "$WT" archive "$C" | tar -x -C "$S"
cp $O/probes/zz_probe_r301_exfat_test.go "$S/internal/editing/"
cp $O/probes/zz_probe_r301_toolset_test.go "$S/internal/toolset/"
cd "$S" || exit 1
go test -c -o $O/pc-ed-darwin.test ./internal/editing && go test -c -o $O/pc-ts-darwin.test ./internal/toolset || { echo "darwin compile failed"; exit 1; }
rm -f internal/editing/zz_probe_r301_exfat_test.go internal/toolset/zz_probe_r301_toolset_test.go
GOOS=linux GOARCH=arm64 go test -c -o $O/pc-ed-linux.test ./internal/editing && GOOS=linux GOARCH=arm64 go test -c -o $O/pc-ts-linux.test ./internal/toolset || { echo "linux compile failed"; exit 1; }

echo "##### real exFAT (macOS) at $C"
m=$R/mnt-ExFAT
hdiutil attach -nobrowse -noverify -mountpoint $m $R/img-ExFAT.dmg >/dev/null || { echo "ATTACH FAILED"; exit 1; }
trap 'hdiutil detach "$m" -quiet || hdiutil detach "$m" -force -quiet' EXIT
mount | grep " $m " | sed 's/^/# mount: /'
cd $O
LC_R301_ROOT=$m ./pc-ed-darwin.test -test.count=1 -test.v -test.run '^TestZZR301RealFS$' 2>&1 | grep -E 'zz_probe|^--- |^PASS|^FAIL' | cut -c1-260
LC_R301_ROOT=$m ./pc-ts-darwin.test -test.count=1 -test.v -test.run '^TestZZR301ToolsetRealFS$' 2>&1 | grep -E 'zz_probe|^--- |^PASS|^FAIL' | cut -c1-260
echo "leftover .lcx-create files on exFAT: $(find $m -name '.lcx-create-*' 2>/dev/null | wc -l | tr -d ' ')"

echo "##### docker lc-ubuntu-go:24.04 uid 1000 at $C: editing.test (all) + toolset.test (-run Create|R201|WriteFile), overlayfs and tmpfs TMPDIR"
echo "# snapshot mounted read-only at its host path so toolset TestMain can 'go build ../lsp/testdata/fakeserver' (stdlib only; GOPROXY=off, no network)"
docker run --rm --init --user 1000:1000 --network none -v $O:/w:ro -v "$S:$S:ro" --tmpfs /tmpfs:rw,exec,uid=1000,gid=1000 \
  -e HOME=/tmpfs -e GOCACHE=/tmpfs/gocache -e GOPROXY=off -e GOTOOLCHAIN=local -e GOFLAGS=-mod=readonly \
  lc-ubuntu-go:24.04 bash -c '
  uname -srm; id; go version
  for d in /tmp /tmpfs; do
    echo "--- TMPDIR=$d ($(stat -f -c %T $d))"
    TMPDIR=$d /w/pc-ed-linux.test -test.count=1 2>&1 | tail -2; echo "editing exit=${PIPESTATUS[0]}"
    TMPDIR=$d /w/pc-ts-linux.test -test.count=1 -test.run "Create|R201|WriteFile" 2>&1 | tail -2; echo "toolset exit=${PIPESTATUS[0]}"
    TMPDIR=$d /w/pc-ed-linux.test -test.count=1 -test.v -test.run "^TestR301" 2>&1 | grep -E "^(--- |    --- )"
  done'
echo "docker exit=$?"
rm -f $O/pc-*.test; rm -rf "$S"
