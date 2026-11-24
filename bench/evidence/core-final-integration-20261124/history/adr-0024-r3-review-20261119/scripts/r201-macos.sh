#!/bin/bash
# R3 review probe runner (throwaway, /tmp only). Builds probe test binaries from the verified
# snapshot trees and runs R2-01 publish/rollback probes on APFS and attached disk images.
set -u
R=/tmp/lc-adr0024-r3
F=$R/fresh
cd $R/probe && go test -c -o $F/ed-head.test ./internal/editing || exit 1
cd $R/prevprobe && go test -c -o $F/ed-prev.test ./internal/editing || exit 1
cd $R/mainprobe && go test -c -o $F/ed-main.test ./internal/editing || exit 1
cd $F

echo "##### APFS real-process probes (head)"
./ed-head.test -test.count=1 -test.v -test.timeout=20m \
  -test.run '^TestZZR3(MultiProcessPublishRace|ForeignRenameReplaceRace|RollbackOfPublishedCreate|DestinationAppearsBeforePublish|PublishFaults|KillAroundPublication|TempWriteFailsEFBIG)$' 2>&1 \
  | grep -E '^(=== RUN|--- |PASS|FAIL|ok)|zz_probe_r3_publish_test.go:[0-9]+:' | cut -c1-600

echo "##### rollback restore vs foreign append (main/prev/head)"
for v in main prev head; do
  echo "--- $v"; ./ed-$v.test -test.count=1 -test.v -test.run '^TestZZR3RollbackRestoreVsForeign$' 2>&1 | grep -E 'zz_probe|^--- ' | cut -c1-400
done

attached=()
cleanup() { for m in "${attached[@]}"; do hdiutil detach "$m" -quiet || hdiutil detach "$m" -force -quiet; done; }
trap cleanup EXIT
for img in ExFAT HFS MSDOSFAT32 small; do
  hdiutil attach -nobrowse -noverify -mountpoint $R/mnt-$img $R/img-$img.dmg >/dev/null && attached+=("$R/mnt-$img") || echo "ATTACH FAILED $img"
done
echo "attached: ${attached[*]}"
for img in ExFAT HFS MSDOSFAT32; do
  m=$R/mnt-$img
  echo "##### FS=$img ($(diskutil info $m 2>/dev/null | awk -F: '/Type \(Bundle\)/{gsub(/ /,"",$2);print $2}'))"
  echo "--- head"; LC_R3_FSROOT=$m ./ed-head.test -test.count=1 -test.v -test.timeout=10m -test.run '^TestZZR3FSMatrix$' 2>&1 | grep -E 'zz_probe|^--- ' | cut -c1-700
  echo "--- prev (a8b4786)"; LC_R3_FSROOT=$m ./ed-prev.test -test.count=1 -test.v -test.timeout=10m -test.run '^TestZZR3PrevFSCreate$' 2>&1 | grep -E 'zz_probe|^--- ' | cut -c1-500
done
echo "##### ENOSPC on 8MiB image (main/prev/head)"
for v in main prev head; do
  echo "--- $v"; LC_R3_SMALLFS=$R/mnt-small ./ed-$v.test -test.count=1 -test.v -test.run '^TestZZR3ENOSPC$' 2>&1 | grep -E 'zz_probe|^--- ' | cut -c1-500
done
echo "leftover on small fs: $(ls -A $R/mnt-small | tr '\n' ' ')"
