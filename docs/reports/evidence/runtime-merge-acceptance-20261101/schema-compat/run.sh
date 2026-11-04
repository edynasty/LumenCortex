#!/usr/bin/env bash
# Cross-binary schema compatibility: Core main (v5) -> rt (v9) -> refusals -> downgrade -> main again.
set -u
export XDG_STATE_HOME=/tmp/lcx-schema/xdg; B=/tmp/lcx-schema/bin; W=/tmp/lcx-schema/ws
rm -rf "$XDG_STATE_HOME"
rm -rf "$W"; mkdir -p "$W"; cd "$W"; git init -q; echo seed >seed.txt; git add seed.txt; git -c user.email=t@t -c user.name=t commit -qm seed
ver() { sqlite3 "$DB" 'PRAGMA user_version;'; }
tables() { sqlite3 "$DB" "select group_concat(name,' ') from sqlite_master where type='table' and name in ('jobs','scheduled_tasks','notebooks','notebook_revisions','source_fences','approvals','provenance_edges');"; }
sum() { shasum -a 256 "$DB"* 2>/dev/null | awk '{print $1" "$2}' | sed "s#$ST/##"; }
step() { printf '\n## %s\n' "$*"; }
step "1 main binary creates the state"
$B/lcx-main session-new "main goal" | tr -d '\n' | cut -c1-160; echo
ST=$(dirname "$($B/lcx-main | python3 -c 'import json,sys;print(json.load(sys.stdin)["database"])')"); DB=$ST/lumencortex.db
echo "state dir: ${ST#"$XDG_STATE_HOME"/}"
SID=$(sqlite3 "$DB" "select id from sessions limit 1;")
$B/lcx-main shell "$SID" echo from-main | tr -d '\n' | cut -c1-120; echo
echo "user_version=$(ver) sessions=$(sqlite3 "$DB" 'select count(*) from sessions;')"
step "2 rt binary opens it (upgrade)"
$B/lcx-rt sessions | grep -c '"id"' | sed 's/^/sessions listed by rt: /'
echo "user_version=$(ver) tables: $(tables)"; ls "$ST/backups/"
step "3 rt API writes rows in the new tables"
$B/populate "$W"
for t in jobs scheduled_tasks notebooks; do echo "$t=$(sqlite3 "$DB" "select count(*) from $t;")"; done
step "4 main and r1a binaries refuse the v9 database without writing"
before=$(sum); $B/lcx-main sessions; echo "main exit=$?"; $B/lcx-r1a sessions; echo "r1a exit=$?"; after=$(sum)
[ "$before" = "$after" ] && echo "database files byte-identical before/after refusals" || { echo "CHANGED"; echo "$before"; echo "$after"; }
step "5 rt binary downgrades to 5 (main's schema)"
$B/lcx-rt db downgrade --to 5; echo "exit=$?"
echo "user_version=$(ver) tables: $(tables)"; ls "$ST/backups/"
step "6 main binary opens the downgraded database"
$B/lcx-main sessions | grep -E '"(id|goal)"' | tr -s ' '; echo "main exit=${PIPESTATUS[0]}"
$B/lcx-main shell "$SID" echo after-downgrade | tr -d '\n' | cut -c1-120; echo
echo "user_version=$(ver) sessions=$(sqlite3 "$DB" 'select count(*) from sessions;')"
step "7 rt binary upgrades again"
$B/lcx-rt sessions | grep -c '"id"' | sed 's/^/sessions listed by rt: /'
echo "user_version=$(ver)"; for t in jobs scheduled_tasks notebooks; do echo "$t=$(sqlite3 "$DB" "select count(*) from $t;")"; done
step "8 the v9 backup taken before the downgrade still holds the rt rows"
bk=$(ls "$ST/backups/" | grep '\.v9\.' | head -1); echo "backup=$bk"
for t in jobs scheduled_tasks notebooks; do echo "$t=$(sqlite3 "$ST/backups/$bk" "select count(*) from $t;")"; done
