#!/bin/bash
# T1-S7 item 6: cross-binary migration round trip.
#   usage: t1-s7-migration-roundtrip.sh <old-core-worktree (42ee234)> <new-core-worktree (T1 head)>
# For each schema version 1..5 it builds an early legacy workspace (no binding) from the migrate
# package's Go fixture, in two copies:
#   direct:    the new binary opens vN (upgrade to 6 in place, binding written) -> the old binary
#              refuses it -> `db downgrade --to 5` with the new binary -> the old binary lists the same
#              sessions -> the new binary re-upgrades;
#   old first: the old binary opens vN (v1..v4 upgraded to 5 by the old binary itself), then the same
#              sequence from the new binary's open on.
# HOME, XDG_* and TMPDIR point into a fresh temporary directory; the real HOME is not touched. Exit
# status is non-zero on the first mismatch.
set -euo pipefail
OLD=$(cd "$1" && pwd); NEW=$(cd "$2" && pwd)
T=$(mktemp -d)
export HOME="$T/home" XDG_STATE_HOME="$T/state" XDG_CONFIG_HOME="$T/config" XDG_DATA_HOME="$T/data" XDG_CACHE_HOME="$T/cache"
mkdir -p "$HOME" "$XDG_STATE_HOME" "$XDG_CONFIG_HOME" "$XDG_DATA_HOME" "$XDG_CACHE_HOME" "$T/tmp" "$T/bin" "$T/fx"
GOTMP="$T/tmp"
(cd "$OLD" && TMPDIR=$GOTMP go build -o "$T/bin/old" ./cmd/lcx-go)
(cd "$NEW" && TMPDIR=$GOTMP go build -o "$T/bin/new" ./cmd/lcx-go)
(cd "$NEW" && TMPDIR=$GOTMP LCX_EMIT_FIXTURES="$T/fx" go test -count=1 -run '^TestEmitVersionFixtures$' ./internal/migrate >/dev/null)
export TMPDIR="$T/tmp"

v=""
fail() { echo "FAIL v$v: $*"; exit 1; }
uv() { sqlite3 "$1" 'PRAGMA user_version;'; }
binding() { sqlite3 "$1" "SELECT count(*) FROM metadata WHERE key = 'workspace_binding';" 2>/dev/null || echo 0; }
ids() { "$1" sessions | grep -o '"id": "[^"]*"' | sort | tr '\n' ' '; }

roundtrip() { # $1 workspace, $2 sessions every listing must show
  local db="$1/.lumencortex/lumencortex.db" want="$2" out
  cd "$1"
  [ "$(ids "$T/bin/new")" = "$want" ] || fail "new binary lists different sessions"
  [ "$(uv "$db")" = "6" ] || fail "new binary left user_version $(uv "$db")"
  [ "$(binding "$db")" = "1" ] || fail "new binary did not bind the workspace"
  if out=$("$T/bin/old" sessions 2>&1); then fail "old binary opened v6: $out"; fi
  echo "$out" | grep -q 'newer than this binary' || fail "old binary on v6 said: $out"
  "$T/bin/new" db downgrade --to 5 >/dev/null
  [ "$(uv "$db")" = "5" ] || fail "downgrade left user_version $(uv "$db")"
  [ "$(ids "$T/bin/old")" = "$want" ] || fail "old binary after downgrade lists different sessions"
  [ "$(ids "$T/bin/new")" = "$want" ] || fail "re-upgrade lists different sessions"
  [ "$(uv "$db")" = "6" ] || fail "re-upgrade left user_version $(uv "$db")"
}

for v in 1 2 3 4 5; do
  ws="$T/fx/v$v"; db="$ws/.lumencortex/lumencortex.db"
  [ "$(uv "$db")" = "$v" ] || fail "fixture user_version $(uv "$db")"
  [ "$(binding "$db")" = "0" ] || fail "fixture already bound"
  # The sessions the fixture holds, read without either binary.
  want=$(sqlite3 "$db" 'SELECT id FROM sessions;' | sed 's/.*/"id": "&"/' | sort | tr '\n' ' ')
  [ -n "$want" ] || fail "fixture holds no sessions"
  direct="$T/fx/v$v-direct"; cp -R "$ws" "$direct"
  roundtrip "$direct" "$want"
  cd "$ws"
  [ "$(ids "$T/bin/old")" = "$want" ] || fail "old binary lists different sessions from the v$v fixture"
  [ "$(uv "$db")" = "5" ] || fail "old binary left user_version $(uv "$db")"
  roundtrip "$ws" "$want"
  echo "v$v: ok ($(echo "$want" | wc -w | awk '{print $1/2}') sessions; direct and old-first: new->6 bound, old refused 6, downgrade->5 read by old, re-upgrade->6)"
done
leaked=$(find "$T/state" -name lumencortex.db | wc -l | tr -d ' ')
[ "$leaked" = "0" ] || { echo "FAIL: legacy workspaces created external state ($leaked databases)"; exit 1; }
echo "ALL OK ($T)"
