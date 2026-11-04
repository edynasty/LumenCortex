# Pre-merge review — domain `state` (scheduler / wake-ups, notebook, migrations 0007–0009, diagnostics log)

Baseline: `cog/r1a` (15bf4eb) .. `15f9039`, branch `rt/execution-runtime`, review worktree `wt/core-rt-review` (detached at 15f9039). All of my probe files are removed. The remaining untracked `zz_probe_*` files belong to other reviewers.
Reviewer did not write this code. Findings only; nothing in Core was changed.

## Scope reviewed

Commits (read with `git show` / `git log` / `git diff cog/r1a..15f9039`):
- Scheduler / wake-ups: a413ca4, eb9815d, 338a233, f6a23f1, 35101f9, 35af5e8, 5e4cc9c, bc62dec (d2a79b4 via the merged tree).
- Notebook: bba97ae, 95a61c9, d9f0909, 416bcd7, aee2e7a, merge 797a375 (b80369c, 1ca515a, e27a320 via the merged tree); context-reset test 006b031 (`runtime/taskstate.go` itself is unchanged since R1a).
- Diagnostics: 7094665, ed9a740, b9cee65 (full diff read), 67e7dc9 (diag touch only); 6bdc9d2, 9897ff7 via the tree.

Files read in full at 15f9039: `internal/schedule/{schedule,scheduler,cron}.go`, `internal/session/schedules.go`, `runtime/schedules.go`, `internal/toolset/wakeup.go`, `internal/session/notebooks.go`, `runtime/notebooks.go`, `internal/diag/redact.go`. Relevant sections read in: `internal/diag/diag.go` (enqueue, writer loop, node tree, encode), `runtime/diagnostics.go` (bridge, logEvent, export, manifest), `runtime/engine.go` (Open/Close wiring, NewSession), `runtime/agent.go` (policy, wake-up binding, Start notices), `runtime/supervisor.go`, `runtime/jobs.go` (SubmitScheduled/JobActive), `internal/session/purge.go` (purgeTables, purgeWhere, copyFilter), `runtime/revoke.go`, `runtime/deletion.go`, `runtime/taskstate.go` (ResetContext), `internal/notebook/notebook.go` (normalize/Append), migrations `0007/0008/0009` up + down, `internal/migrate/migrate.go` (checkVersion, backup, Downgrade), ADR-0019 §4/§5, ADR-0020, and `docs/reports/runtime-004-schedules-2026-11-01.md`.
Two read-only scouts were used for breadth (diag; notebook+migrations). I re-checked every finding below in the code and with a probe I ran myself.

## Probes run

All probes were throwaway `_test.go` files in the review worktree, run with `go test -race -count=1 -run <name>`; the fast-path and diag re-run were run without `-race`. All were then deleted. Their sources are kept outside the repo at `/tmp/lcx-review/state-probes/`.

| Probe | Package | Observed output (verbatim, trimmed) |
|---|---|---|
| `TestZZProbeScheduleAfterJobRetention` | runtime | `firing 2 after sweep (deleted [job_3fb9…]): status="failed" ref="job_3fb9…" error="checking the previous firing: session not found"`; firing 3 identical; `jobs submitted after the sweep: 0` |
| `TestZZProbeWakeupDuringOwnRunIsLost` | runtime | `while run active: status="skipped_overlap" next=<nil> fireCount=1`; `after run ended: status="skipped_overlap" next=<nil> fireCount=1 (no run was ever started for the wake-up)` |
| `TestZZProbeNotebookReadIgnoresRevocation` | runtime | `child withdrawn=true; agent notebook_read err=<nil> body-has-sentinel=true; host Notebook err=<nil> body-has-sentinel=true` |
| `TestZZProbeNotebookReaderNotFencedOnRevoke` | runtime | `edges parent->child=0; revocation fenced sessions=[<child>]; parent fenced=false` |
| `TestZZProbeNotebookCopySurvivesChildDeletion` | runtime | `run=completed; parent rows holding the child's notebook body: before delete=1, after delete=1 (child get: session not found)` |
| `TestZZProbeUpdateRacingFiringRefires` | schedule | `after rename: next_run_at=2026-11-01 10:00:00 +0000 UTC fire_count=1`; `shell firings for one occurrence at 2026-11-01T10:00:00Z: 2 (fire_count=2)` |
| `TestZZProbeSecondStartMarksLiveOccurrenceMissed` | schedule | `y: last_status="missed" enabled=false fire_count=0 next_run_at=<nil>; A shell firings=1 (A was running throughout)` |
| `TestZZProbeCronSpringForward` | schedule | `"0 * * * *" after 2026-03-08T01:50:00-05:00 -> 2026-03-09T00:00:00-04:00`; `"*/15 * * * *" -> 2026-03-09T00:00:00-04:00`; `"0 2,9 * * *" -> 2026-03-09T02:00:00-04:00` |
| `TestZZProbeLocalTimezoneAccepted` | schedule | `validate(Local)=<nil> loadTimezone=Local err=<nil>` |
| `TestZZProbeWakeupCancelDeletesHostSchedule` | runtime | `read-only run's wakeup_cancel(host cron task) err=<nil>; task afterwards: session not found` |
| `TestZZProbeGoalInDiagnosticsLog` | runtime | `level=INFO … goal-in-log-after-delete=true line={"…","level":"INFO","msg":"session.start","session_id":"session_69e8…","data":{"goal":"GOAL-SENTINEL-the user's private task text"}}` |
| `TestZZProbeSecretKeyNames` (D5) | diag | `IsSecretKey` = false for `secret_key`, `aws_secret_access_key`, `SecretKey`, `access_key`, `passphrase`, `pwd`, `auth`, `private_key_id`, `api.key`, `x-authorization` |
| `TestZZProbeGroupCollisionPanics` (D1) | diag | `panic on caller goroutine: interface conversion: interface {} is string, not *diag.node` |
| `TestZZProbeWithGroupSecretName` (D2) | diag | `{"…","msg":"probe","api_key":{"value":"hunter2-password"}}` |
| `TestZZProbeMapInSlice` (D3) | diag | `{"…","msg":"probe","rows":[{}]}` |
| `TestZZProbeTornLine` (D4) | diag | `query after restart: n=0 err=<nil>` (one record logged after a torn line) |
| `TestZZProbeUnmarshalableMap` (D6) | diag | `{"…","msg":"probe","data":"map[password:hunter2-password]"}` |
| `TestZZProbeReservedKey` (D7) | diag | `{"…","level":"INFO","msg":"probe","level":"custom"} ; query n=0 err=<nil>` |
| `TestZZProbeValueShapes` (D8) | diag | `"https://user:p@ss@host/x" -> "https://[redacted]@ss@host/x"`; `-----BEGIN RSA PRIVATE KEY-----\n…secretbody` (no END line) unchanged; `Authorization: Basic dXNl…` unchanged; Bearer and `sk-` masked |
| `TestZZProbeFastPathEquivalence` | diag | `checked=360000 differing=0` (MaskSecrets with the b9cee65 pre-check vs. all seven regexps on a mutated corpus incl. case variants and `ſ`/`K`) |
| `TestZZProbeNotebookRevisionBytes` | runtime | `doc.Bytes=314 revision[0].Bytes=114` |
| `TestZZProbeNotebookCRLF` | runtime | `create body="a\r\nb\r\n" bytes=6; after append body="a\nb\n\nc\n"` |
| existing tests, targeted | migrate, session, diag | `go test -race -count=1 -run 'Downgrade\|Latest\|Migrat\|TooNew\|Backup\|Provenance\|Notebook\|Schedule' ./internal/migrate/ ./internal/session/ ./internal/notebook/` → ok / ok / ok; `-run 'Queue\|Drop\|Full\|Rotat\|Retention\|Export\|Close\|Redact\|Secret' ./internal/diag/` → all PASS |
| grep | — | `RegisterDiagnosticSummary` → definition `runtime/diagnostics.go:447` and `diagnostics_test.go:470` only; no production caller |

## Findings

| ID | Sev | file:line @15f9039 | Evidence (trigger → effect) | Probe | Suggested minimal fix |
|---|---|---|---|---|---|
| RT-state-1 | **P1** | `internal/schedule/scheduler.go:301-306`; `runtime/schedules.go:128` → `runtime/jobs.go:283-292` | A shell schedule keeps the previous firing's job id in `last_ref`. The job manager deletes finished job rows after `JobRetentionDays` (default 7, `config/types.go:241`), sweeping hourly and once at `Manager.Start`. Next firing: `Active` → `JobActive` → `GetJob` → `ErrNotFound` → the firing is recorded as `failed` with `ref = t.LastRef` unchanged, and nothing is submitted. Every later firing repeats this, so **the schedule is permanently dead**. Triggers: any shell task whose period is longer than the retention (a monthly cron, an interval up to 365 d), a task disabled for more than 7 d and then re-enabled, or the app closed for more than 7 d. | `TestZZProbeScheduleAfterJobRetention`: firings 2 and 3 `failed … session not found`, 0 jobs submitted | Treat a vanished previous ref as not active: in `run` (or `JobActive`), map `session.ErrNotFound` to `active=false`. |
| RT-state-2 | **P1** | `runtime/schedules.go:108-113` (ErrRunHeld→ErrBusy) + `internal/schedule/scheduler.go:282` (the claim of a once task writes `next_run_at=NULL` before the action), `:321-323` | A wake-up is a one-shot agent task. Suppose it comes due while its session holds the run lock (the run that scheduled it is still working, or the user is in a turn). `RunSupervisor.Start` returns `ErrRunHeld` → `ErrBusy` → `skipped_overlap`, and the already-claimed one-shot has no further slot. The wake-up is consumed and **never fires**, with no error (it also stops counting as pending). The tool promises "after delay_sec the runtime starts a new run here" (`internal/toolset/wakeup.go:41-45`), and delays down to 1 s are accepted. | `TestZZProbeWakeupDuringOwnRunIsLost`: `skipped_overlap`, `next=<nil>`, no run ever started, also after the blocking run ended | For an `ErrBusy` firing whose trigger has no next slot (once), give the claim back (`RevertScheduleClaim`, as for `ErrNoRunner`) and defer locally, instead of recording a terminal skip. |
| RT-state-3 | **P1** | `runtime/notebooks.go:172-186` (`target`), `:222-247` (`Read`), `:57-59` (`Engine.Notebook`; same for `NotebookRevision(s)`, `ExportNotebook`) | Notebook reads check only `session_relations`. They never check the source fences that every R1a read exit honours (`ReadThread` → `Withdrawn`, `MessagePage` → `Withheld`, list/wait_subagent withdrawn; see `TestReadExitsAfterRevokingASubagent`). After `RevokeSource(child)`, the parent's `notebook_read {session_id: child}` and the host's `Engine.Notebook(child, …)` still return the child's body. The read also stores no derived edge, so the revocation does **not** fence the parent that consumed the content, and that content keeps going into the parent's later requests. | `TestZZProbeNotebookReadIgnoresRevocation`: `child withdrawn=true … body-has-sentinel=true` (agent and host); `TestZZProbeNotebookReaderNotFencedOnRevoke`: `edges parent->child=0 … parent fenced=false` | In `notebookAccess.List/Read` (child target) and the host read APIs, check `store.SessionWithdrawn(target)` and return a withdrawn result, as `ReadThread` does. Record the read as a session-source edge, as `read_thread` does, so a revocation fences the reader. |
| RT-state-4 | **P1** | `runtime/notebooks.go:231-233` (`Read` result has no session id) vs `internal/session/purge.go:356-366` (`copyFilter` matches `instr(json, <deleted id>)`) | `copyFilter` relies on "every exit that returns content of a session names it". `notebook_list` returns `"session": target`, but `notebook_read` returns only `{title,id,version,bytes,body}`. Sequence: the parent reads the child's notebook → `DeleteSession(child)`. The privacy purge removes the child's rows, but the parent's tool row keeps the child's body at rest and in every later window of the parent. This breaks the R1a deletion invariant asserted by `TestDeletingASessionRemovesEveryCopyOfItsContent` (no sentinel bytes left in the DB). | `TestZZProbeNotebookCopySurvivesChildDeletion`: `before delete=1, after delete=1` | Add `"session": target` to the `Read` result (as `List` does) so `copyFilter` finds and rewrites the copy; add a regression test to the deletion suite. |
| RT-state-5 | P2 | `internal/schedule/scheduler.go:521-579` + `internal/session/schedules.go:326-336` (`next_run_at = ?` written unconditionally) | `Update` reads the row, then writes `next_run_at` from that read even when the patch does not touch the trigger or enablement (e.g. a rename). If a firing (this process's scheduler goroutine or another Engine) claims the row between the read and the write, the write undoes the claim: the old `next_run_at` comes back and the **same occurrence fires again** (a once task re-fires). The doc comment at `schedules.go:324-325` ("an update cannot erase what a concurrent firing recorded") claims the opposite. | `TestZZProbeUpdateRacingFiringRefires`: 2 shell firings, `fire_count=2`, for one occurrence | Write `next_run_at` only when the patch recomputed it, or make the UPDATE a compare-and-set on the `next_run_at` that was read (`AND next_run_at IS ?`) and retry or refuse on a miss. |
| RT-state-6 | P2 | `internal/schedule/scheduler.go:170-205` (`applyMissed`) | Every Engine's `Start` treats every task due *now* as "missed while no scheduler ran". Two Engines can share one DB (Desktop + a CLI invocation, or a second Desktop window). The second Engine's Start then advances occurrences that the live scheduler is about to fire, or that are queued behind a slow serial firing (an agent `Start` runs reconcile, MCP discovery and constraint checks on the scheduler goroutine). With the default `skip` policy, a once task — every wake-up — is disabled as `missed` and never fires. | `TestZZProbeSecondStartMarksLiveOccurrenceMissed`: A is firing task x; B starts; y → `last_status="missed" enabled=false`, A fired only x | Apply the missed policy only to occurrences older than a grace window (e.g. `next_run_at < now - maxSleep`), and leave recent ones to the normal claim. |
| RT-state-7 | P2 | `internal/schedule/cron.go:188-195` | When a matching minute falls in the spring-forward gap, `next` jumps to the **next day 00:00**, skipping every later match of that day. The comment says the rest of the day "is still to come"; ADR-0019 §4 says only the nonexistent local time is skipped. Hourly and sub-hourly crons lose about 21 h of firings once a year in each DST zone. The existing `TestCronSkipsNonexistentLocalTime` uses a daily `30 2` expression, which both behaviours satisfy. | `TestZZProbeCronSpringForward`: `"0 * * * *"` and `"*/15 * * * *"` after 01:50 EST → next `2026-03-09T00:00`, not `03:00` EDT | On a nonexistent instant, advance the cursor to the next hour (`h++, m=0`, rolling the day only past 23), not to the next day. |
| RT-state-8 | P2 | `runtime/engine.go:606` (`session.start` carries `goal`) → `runtime/diagnostics.go:241ff` (non-correlation keys kept in `data`) → `internal/diag/redact.go:63` (`goal` is not in `contentKeys`) | The session goal (the user's task text; it becomes the first user row when `Input` is empty) is written verbatim (up to 2 KiB) at INFO, the default level, and goes into `ExportDiagnostics` zips. ADR-0019 §5 says prompts are "reduced to their byte length". The goal also survives `DeleteSession`, and the deletion preview's `Outside` list (`runtime/deletion.go:70`) does not name the diagnostics log. | `TestZZProbeGoalInDiagnosticsLog`: after `DeleteSession`, the log holds `"data":{"goal":"GOAL-SENTINEL-…"}` at INFO | Add `goal` to `contentKeys`, and audit the other event payload keys that carry user text. |
| RT-state-9 | P2 | `internal/diag/redact.go:35-59,127-138` | Secret-named keys are matched only as exact names or suffixes, so names that *contain* a secret word pass: `secret_key`, `aws_secret_access_key`, `SecretKey`, `access_key`, `passphrase`, `pwd`, `private_key_id`. ADR-0019 §5 lists "secret … private key" as secret-named keys. | `TestZZProbeSecretKeyNames`: all listed names `IsSecretKey=false` | Also treat a normalized key that *contains* `secret`, `password`, `passphrase`, `privatekey` or `accesskey` as secret; keep the suffix rule for `token` so `total_tokens` stays loggable. |
| RT-state-10 | P3 | `internal/diag/diag.go:588-591,635-637` | Handler group paths have two defects. (a) `With(k, v)` followed by `WithGroup(k)` or `slog.Group(k, …)` panics in `node.child` on the caller's goroutine. (b) A secret-named `WithGroup` name is never checked, so `WithGroup("api_key").Info("m","value",x)` writes `x`. No production code calls `WithGroup` today, so both are latent. | `TestZZProbeGroupCollisionPanics` (panic text above); `TestZZProbeWithGroupSecretName` | (a) Replace a non-node value instead of asserting; (b) redact the subtree when a path element passes `isSecretName`. |
| RT-state-11 | P3 | `internal/diag/redact.go:298-303,313-316`; `internal/diag/diag.go:662-665,730` | Encoding edge cases: (1) objects inside arrays are written as `{}` (`*node` has no exported fields), so event payloads with arrays of objects lose their fields; (2) a value `json.Marshal` cannot encode (`map[any]any`) falls back to `fmt.Sprint`, with keys and values unredacted; (3) an attribute named `level`/`msg`/`time` duplicates the JSON key, and the record disappears from `Query`. | `TestZZProbeMapInSlice` `[{}]`; `TestZZProbeUnmarshalableMap` `map[password:hunter2-password]`; `TestZZProbeReservedKey` `query n=0` | `writeValue`: recurse into `[]any`; fallback: emit a type stub, not `fmt.Sprint`; prefix or drop reserved keys in `encode`. |
| RT-state-12 | P3 | `internal/diag/diag.go:445-457` (`open` appends without checking the last byte) | After a crash mid-write, the file ends without `\n`. The next process appends its first record to the torn line, and `Query` loses it. Once the merged line exceeds `maxRecordBytes+1024`, the scanner fails the whole read. | `TestZZProbeTornLine`: `n=0 err=<nil>` | In `open`, if the file is non-empty and its last byte is not `\n`, write `\n` first. |
| RT-state-13 | P3 | `internal/diag/redact.go:143-152` | Value masking has gaps against the ADR's "masked inside any string": `Authorization: Basic …` and a PEM private key without its END line pass unchanged. A URL password containing `@` is masked only up to the first `@` (`https://[redacted]@ss@host`). | `TestZZProbeValueShapes` | Add Basic-auth and unterminated-PEM (BEGIN…end of string) patterns; make the userinfo pattern match up to the last `@` before the host. Add the new literals (`basic`) to `mayHoldSecret`. |
| RT-state-14 | P3 | `runtime/diagnostics.go:447-454,522-524` | ADR-0019 §5: "Export writes … a manifest and job/schedule summaries". `RegisterDiagnosticSummary` has no production caller, so `manifest.summaries` is never written. | `grep RegisterDiagnosticSummary` → definition + test only | Register job and schedule summaries (ids, kinds, states, counts; no commands or prompts) in `OpenContext`. |
| RT-state-15 | P3 | `internal/session/notebooks.go:426,458` | `NotebookRevision.Bytes` uses SQLite `length(body)`, which counts characters; every other `bytes` in the slice is UTF-8 bytes. | `TestZZProbeNotebookRevisionBytes`: `314` vs `114` | `length(CAST(body AS BLOB))`. |
| RT-state-16 | P3 | `internal/session/notebooks.go:112-123` vs `internal/notebook/notebook.go:146-152,205-210` | `checkNotebookBody`'s doc says it "returns it with CRLF read as LF", but it returns the body unchanged. The first `append`/`section` then normalizes the whole body, silently rewriting lines nobody touched (and the history). | `TestZZProbeNotebookCRLF` | Normalize CRLF in `checkNotebookBody`, as documented. |
| RT-state-17 | P3 | `runtime/schedules.go:510-517` | `wakeup_cancel` checks only `kind=agent` and the session, not that the task is a wake-up. A run (even a read-only one) can therefore delete a host-created recurring agent schedule of its session if it learns the id. ADR-0019 §4 scopes the tool to wake-ups. | `TestZZProbeWakeupCancelDeletesHostSchedule`: host cron task deleted | Also require `wakeupPromptPrefix` (the same check `pending` uses). |
| RT-state-18 | P3 | `internal/schedule/cron.go:230-237` | `loadTimezone("Local")` is accepted, so a cron task can follow the host's local zone, which differs between processes and hosts. This contradicts ADR-0019 §4 ("explicit IANA timezone", "means the same thing … in another process"). | `TestZZProbeLocalTimezoneAccepted` | Refuse `Local`. |

### Checked and found sound (with evidence)
- **One-timer loop, CAS claim, two schedulers**: `ClaimSchedule` is a single `UPDATE … WHERE next_run_at IS ? AND enabled = 1`; `RevertScheduleClaim`, `MarkScheduleMissed` and `DisableSchedule` are CAS too. Firings run serially on one goroutine, and `Close` cancels and waits. Exceptions are RT-state-5 and RT-state-6.
- **Delete during firing**: `record` tolerates `ErrNotFound` (`internal/schedule/scheduler.go:363-368`); covered by the existing f6a23f1 tests.
- **Wake-up policy ceiling**: `wakeupsFor(sessionID, policy)` is bound with the run's narrowed policy (`runtime/agent.go:290`), and the wake-up run starts with that policy. Child runs get no wake-up tools. The **per-session cap** of 3 holds, because runs on one session are serialized by the run lock.
- **Notebook optimistic concurrency**: read + `UPDATE … WHERE id=? AND version=?` inside `BEGIN IMMEDIATE`. The **revision cap** prunes `version <= MAX-50` in the same transaction. **Purge** covers `jobs`, `scheduled_tasks`, `notebook_revisions` (via `purgeWhere`) and `notebooks`, plus `ON DELETE CASCADE` with `_foreign_keys=on`. The **run-start index row** is bounded (`notebookIndexRow`) with a single insert path (`runtime/agent.go:427-428`).
- **ResetContext** (R1a code, unchanged): it takes the run lock and only moves `WindowStartSeq` forward. The next Start's notebook index row lands after the reset, so notebooks stay reachable. 006b031 tests the dropped rows only.
- **Migrations 0007–0009**: up and down files mirror each other, and the drop order respects FKs. `Latest()=9`. A newer schema is refused with `ErrSchemaTooNew` before any write. Upgrade (`internal/migrate/migrate.go:336`) and downgrade (`:515`) each take a backup first, and a backup failure aborts. The targeted migrate and session tests pass.
- **b9cee65 fast path**: each of the seven patterns has a literal in `mayHoldSecret`. Only `bearer` is matched case-insensitively, which matches the only `(?i)` pattern, and Go's `(?i)` adds no non-ASCII case folds for b/e/a/r. Across 360 000 mutated inputs there were 0 differences. **No secret that the regexps would catch can slip past the pre-check.** The only risk is maintenance: a new pattern added without its literal would be dead code, and no test checks for that.
- **Queue**: the send never blocks; when the queue is full the newest record is dropped and counted (`internal/diag/diag.go:214-224`). After Close, records are counted as dropped and never cause a panic, and the writer drains on stop. **Rotation and retention**: size is checked before each write, rotated files are ordered by numeric suffix, the file-name regex is anchored, and retention uses mtime. The existing tests pass.

## Unverified (not proven; not counted in the verdict)
- U1 `notebook_list` returns every `## ` heading of up to 32 notebooks with no total bound, since headings are not limited inside the 256 KiB body. That could produce a multi-MB tool result unless a generic tool-result cap applies. Not probed.
- U2 The notebook index row is built from `ListNotebooks` before `BeginRun` (`runtime/agent.go:427` vs `:449`), so it can be one write stale; ADR-0020 says the Start transaction writes it.
- U3 Notebook writes do not check the consumer fence that other write paths check; I could not show a leak from it.
- U4 The diagnostics bridge resolves `run_id` at log time (`runtime/diagnostics.go`, logEvent). A lagging bridge could tag run 1's `run.stopped` with run 2's id and then forget run 2. This is timing-dependent; no deterministic repro.
- U5 Rotation racing `Query`/`Export` (no writer lock between listing the files and `os.Open`) could duplicate or miss a file's records.
- U6 `cronField` counts `*/n` as restricted, so the DOM/DOW OR rule applies where Vixie (a field starting with `*` is unrestricted) uses AND, e.g. `0 0 */2 * 1`. Whether this is a bug depends on which Vixie behaviour is intended.
- U7 Wake-ups use the default `skip` missed policy, so a wake-up that comes due while the app is closed is disabled as `missed` at the next Start and never runs. This may be intended, but it is not documented for wake-ups.
- U8 `Scheduler.ObserveRef` has no production caller, so `last_status` stays `fired` and never reaches the terminal status of the job or run. No contract requires it.
- U9 JWTs and `password=` / `api_key=` query parameters inside strings are probably not masked (no pattern covers them); not probed.

## Verdict

Four P1s can be hit by users:
- A shell schedule stops for good once job retention removes its previous job.
- A wake-up is silently lost if it comes due while its session is running.
- Notebook reads bypass both the revocation fences and the copy scrubbing done by privacy deletion, which breaks R1a invariants (two findings).

Each P1 has a small, local fix. The migrations, CAS claims, notebook concurrency, the revision cap, purge coverage, the diagnostics queue and the b9cee65 fast path are sound.

VERDICT: REVISE


## Re-verification @ de6ccac

Scope: incremental check of the fix commits for the state domain, `git diff 5ccc350 de6ccac`. I read the diffs of `internal/schedule/{schedule,scheduler,cron}.go`, `internal/session/schedules.go`, `runtime/{schedules,jobs,notebooks,agent}.go`, `internal/agent/provenance.go`, `internal/session/{notebooks,purge}.go`, `internal/diag/redact.go`, and the ADR-0019 §4 text. I also checked the capability-U1 change `dd46e25` against the scheduler invariants.

How the probes were run:
- My original probes were re-run unchanged where the API allowed. The wake-up probes were adapted to the new `wakeupsFor(session, policy, allow, deny)` signature. New probes were added for the holes Main asked about.
- Everything ran with `go test -race -count=1 -run …` (except one non-race cron re-run).
- All probe files are deleted from the worktree. `git status` is clean at de6ccac. Probe sources are in `/tmp/lcx-review/state-probes/` (`zz_probe_rv_*`).

Targeted regression tests: all `ok`.
- `./internal/schedule/`, `./internal/session/`:
  - the new tests TestAgentPayloadKeepsToolFilters, TestBusyOnceTaskFiresWhenTheSessionIsFree, TestCronSpringForwardResumesAtTheNextValidTime, TestSecondStartLeavesRecentOccurrencesToTheLiveScheduler, TestUpdateRacingAFiringDoesNotRefire
  - the existing Missed/Cron/Once/Interval/NoRunner/Busy/ScheduleRows tests
- `./runtime/`:
  - the new tests TestShellScheduleFiresAfterRetentionDeletedItsLastJob, TestWakeupDueDuringItsOwnRunFiresAfterTheRunEnds, TestWakeupCancelLeavesHostAgentSchedules, TestWakeupKeepsTheRunToolFilters, TestWakeupToolFiltersSurviveAHostUpdate, TestNotebookReadsOfARevokedChildAreWithdrawn, TestNotebookReaderIsFencedWhenItsChildIsRevoked, TestDeletingAChildRemovesItsNotebookFromTheParentTranscript, TestSessionGoalIsReducedToItsLengthInTheLog
  - the R1a invariants TestReadExitsAfterRevokingASubagent and TestDeletingASessionRemovesEveryCopyOfItsContent
  - TestNotebook*, TestEngineWakeups*, TestEngineCronSchedule*, TestEngineScheduler*
- `./internal/diag/`: TestSecretKeysAreRedactedAtEveryLevel, TestBenignKeysAreNotSecret, TestContentKeysAreReducedToTheirLength.

### Per-finding status

| ID | Fix | Status | Probe output @ de6ccac |
|---|---|---|---|
| RT-state-1 | c4ce940 | **FIXED** | `TestZZProbeRVScheduleAfterJobRetention`: `firing 2 after sweep (deleted 1): status="fired" ref="job_ba25…"`, `firing 3 … status="fired" ref="job_f35c…"`, `jobs submitted after the sweep: 2` (was: `failed … session not found`, 0 jobs). |
| RT-state-2 | 49d1b87 | **FIXED** (the run-lock case). See RT-state-19 for the same loss through the agent limit. | `TestZZProbeRVWakeupBusyRetry`: `while run active (7s): status="" next=…12:54:09 fireCount=0 enabled=true`; `schedule.* events during the busy window: 1` (the create; none per retry); after `Cancel` of the blocking run: `recorded=true in 1.4s status="fired" ref="session_e588…" fireCount=1 next=<nil>`. |
| RT-state-3 | ce5c4ca | **FIXED** | `TestZZProbeRVNotebookRevocation`: `edges parent->child=2 parent->self=0` (notebook_list + notebook_read of the child; own list cites nothing); `revocation sessions=[child parent]; parent withdrawn=true`; agent `read=map[session:<child> withdrawn:true]`, `list=map[session:<child> withdrawn:true]`, no sentinel; host `Notebook withdrawn=true title="" sentinel=false`, `Notebooks [{… Title: … Sections:[] Withdrawn:true}]`, `Revisions withdrawn=true`, `Revision body-sentinel=false`, `Export err=the session is withdrawn as a source…`. Original `TestZZProbeNotebookReadIgnoresRevocation`: `body-has-sentinel=false` (agent and host). |
| RT-state-4 | 504a147 | **FIXED** | `TestZZProbeNotebookCopySurvivesChildDeletion`: `before delete=1, after delete=0`. |
| RT-state-5 | 1b595e9 | **FIXED** | `TestZZProbeUpdateRacingFiringRefires` (unchanged probe; `racingStore` embeds the new `UpdateSchedule(t, from)`): `after rename: next_run_at=…11:00 fire_count=1`; `shell firings for one occurrence: 1 (fire_count=1)` (was 2). |
| RT-state-6 | 7764690 | **FIXED** for the reported case; a narrower residual remains (P3, below). | `TestZZProbeSecondStartMarksLiveOccurrenceMissed`: `y: last_status="fired" enabled=true fire_count=1` (was `missed`, disabled). |
| RT-state-7 | 2baa40d | **FIXED** | `TestZZProbeCronSpringForward`: `"0 * * * *"` and `"*/15 * * * *"` after 01:50 EST → `2026-03-08T03:00:00-04:00`; `"0 2,9 * * *"` → `09:00`. `TestZZProbeRVCronGaps`: Lord Howe `*/10` from 01:55+10:30 → `02:30+11:00`, then `02:40`; NY `* * * * *` 01:58 → 01:59 → `03:00-04:00`; daily `30 2` on the gap day → next day 02:30 (skipped, not shifted); fall-back `30 1` fires once (01:30-04:00, then the next day); London `15 1` in the gap → next day. No regression. |
| RT-state-8 | b3c76fe | **FIXED** | `TestZZProbeRVGoalInDiagnosticsLog`: `goal-in-log=false`, line `"data":{"goal":{"redacted":"content","bytes":42}}`. `goal` is not debug-keepable (`debugKeepable` = command/args/arguments only). |
| RT-state-9 | 2f34051 | **FIXED** | `TestZZProbeRVSecretKeyNames`: `secret_key, aws_secret_access_key, SecretKey, access_key, passphrase, pwd, private_key_id, db_password, clientSecret` → true. Correlation and count keys stay loggable (`total_tokens, max_tokens_used, input_tokens, session_id, run_id, tool_call_id, job_id, schedule_id, duration_ms, error_code, provider, model, step, status, exitCode` → false). Newly hidden benign shapes: `password_policy, has_secret, secret_count, passwordless, accessKeyCount` → true. No Core event or log key contains these words (grep over non-test sources), so no Core diagnostic is lost. `auth`, `api.key` and `x-authorization` stay unredacted (not part of the original finding). |
| RT-state-17 | b2277cc | **FIXED** | `TestZZProbeRVWakeupCancelScope`: `cancel host cron task: err=session not found; task afterwards err=<nil>` (kept). Residual P3 below. |
| RT-state-10..16, 18 | — | Deferred to backlog (not re-verified) | `TestZZProbeNotebookRevisionBytes` still `doc.Bytes=314 revision[0].Bytes=114` (RT-state-15 open, as expected). |

### Holes checked in the fixes
- **RT-state-2, retry storm or starvation.** No retry storm.
  - A busy one-shot is claimed and reverted every 2 s (`busyRetry`) while the session is held. Each retry costs two small row writes and one non-blocking run-lock attempt: `RunSupervisor.Start` takes the lock first (`supervisor.go:79`), before any expensive work.
  - Retries emit no event, journal row or notification.
  - The wake-up fired 1.4 s after the blocking run ended. Recurring tasks still record `skipped_overlap`.
  - In another process with no runner, `ErrNoRunner` still gives the claim back and defers 30 s, so retries cannot ping-pong between Engines.
- **RT-state-5, CAS retry exhaustion.** `Scheduler.Update` returns a plain `schedule … kept changing while it was updated; try again` only after 3 lost compare-and-sets.
  - A loss needs a firing or missed-mark between one `GetSchedule` and its `UpdateSchedule`, which is a millisecond window. A busy one-shot moves its row every 2 s. Three consecutive losses are not reachable in practice, and if they happen the host gets a retryable error, not corruption.
  - During a claim→revert window (`next_run_at` NULL) an Update CAS on `IS NULL` succeeds. Afterwards the revert either lands, or loses because the update moved the row. Either order is consistent.
- **RT-state-6, missedGrace vs genuine missed-after-restart.**
  - Occurrences more than 2 min overdue are still handled by the missed policy (TestMissedPolicyAcrossRestart and TestCronMissedPolicyAcrossRestart pass).
  - A restart within 2 min of a slot now fires the occurrence even under `skip`: `TestZZProbeRVShortDowntimeSkipFires` → `last_status="fired" shells=1`. ADR-0019 §4:167-169 documents this ("applied … to the occurrences more than two minutes overdue"), so it is intended.
- **RT-state-3, withdrawn host reads vs ReadThread.**
  - Same predicate (`SessionWithdrawn`), same shape: identity, version, size and author are kept; title, headings and body are dropped. Export is refused.
  - Edges are recorded for notebook_list and notebook_read of a child, never for the run's own session. A withdrawn result (`{session, withdrawn:true}`) cites nothing.
  - A parent fenced through a notebook edge is treated like a read_thread reader. Its next Start is isolated (`exposed`) when a model answer followed the read, so notebook content copied into the parent cannot re-enter a request.
  - The host can still write to a withdrawn session's notebook (`SaveNotebook` → `err=<nil>`), and the readback stays withdrawn. That is not a leak.
- **capability U1 (dd46e25) vs scheduler invariants.**
  - The filters live in the JSON payload (no schema change), well under the 64 KiB `MaxSchedulePayloadBytes`.
  - `Create` (default policy) and `Update` (agent patch) now copy the whole payload, so the filters survive. `Engine.UpdateSchedule` re-applies the current filters, so a host edit cannot widen a wake-up.
  - The claim, overlap, missed-policy and wake-up-cap paths do not read the filters.
  - `CanonicalToolNames` keeps nil and empty distinct. An empty allowlist is dropped by `omitempty`, but a run with an empty allowlist is offered no `wakeup_schedule`, so such a wake-up cannot exist.
  - Cap, ceiling and claim tests pass. No invariant broken.

### New / residual findings @ de6ccac

| ID | Sev | file:line @de6ccac | Evidence | Probe | Suggested minimal fix |
|---|---|---|---|---|---|
| RT-state-19 | P2 | `runtime/schedules.go:112` (only `ErrRunHeld` maps to `schedule.ErrBusy`) → `internal/schedule/scheduler.go:342-344` (any other error is recorded `failed`) | Same loss class as RT-state-2, on the other transient refusal. A wake-up that comes due while the process is at its agent-concurrency limit gets `ErrAgentLimitExceeded` from `RunSupervisor.Start`. It is recorded `failed`, its once slot is already consumed, and it never fires. Defaults are `MaxAgents=4`, `MaxChildrenPerParent=3`: one session running three subagents saturates the limit, so any wake-up due on another session at that moment is lost without notice. | `TestZZProbeRVWakeupAtAgentLimit` (MaxAgents 1, another session's run holds the slot): `status="failed" error="runtime agent concurrency limit exceeded (1 running, limit 1)" fireCount=1 next=<nil> enabled=true`; still `failed`, `next=<nil>` after the slot was freed. | In `scheduleActions.RunAgent`, also map `errors.Is(err, ErrAgentLimitExceeded)` to `schedule.ErrBusy`, so a one-shot gets the RT-state-2 retry. |
| RT-state-6r | P3 | `internal/schedule/schedule.go:105` (`missedGrace = 2*maxSleep`), `scheduler.go:applyMissed` | Residual of RT-state-6. If the live scheduler's serial firings fall more than 2 min behind, a second Engine's Start still marks the queued one-shot `missed` and disables it. Not reachable with today's firing costs: a shell firing is a job submit, and an agent Start waits at most `mcpCatalogDiscoveryBudget` = 10 s, once per undiscovered server. | `TestZZProbeRVSecondStartAfterGrace` (A's firing held for 2 fake minutes): `y last_status="missed" enabled=false; A shell firings=1` | Accept as documented, or skip `applyMissed` for rows another live scheduler has claimed recently (e.g. a heartbeat column). No action needed for merge. |
| RT-state-17r | P3 | `runtime/schedules.go:556-559` (`owns` = kind agent + session + prompt prefix) | A host can create an agent schedule whose prompt starts with the runtime's reserved `[runtime] Scheduled wake-up: ` prefix. A run can then cancel it, and it counts toward the 3-wake-up cap. This needs a deliberate host prompt. | `TestZZProbeRVWakeupCancelScope`: `host create with wake-up prefix: err=<nil>`; `cancel host prefixed task: err=<nil>; task afterwards err=session not found` | Refuse the prefix in `Engine.CreateSchedule`/`UpdateSchedule` prompts, or mark wake-ups with a payload field instead of a prompt prefix. |

### Unverified (re-verification)
- U10 The RT-state-19 class for one-shot **shell** tasks: a full job queue (`ErrQueueFull`) records `failed` and consumes the occurrence. Existing test `schedule_test.go:707-727` asserts `failed` for a recurring task, so this may be intended for shell tasks. Not probed for one-shots.
- U11 A host Update that disables a one-shot inside its claim→busy-revert window lets the revert write `next_run_at` on a disabled row. Due queries require `enabled = 1`, so it does not fire. Re-enabling recomputes the slot. No harmful sequence found.

### Verdict @ de6ccac
- **Fixed:** all ten fixed IDs (RT-state-1, 2, 3, 4, 5, 6, 7, 8, 9, 17). No regressions were found in the scheduler, notebook, migration or diagnostics paths, and capability U1 keeps the scheduler invariants.
- **Remaining P1:** none.
- **New P2:** RT-state-19. A wake-up is lost when it comes due at the agent-concurrency limit. It is the same class as RT-state-2 and has a one-line fix.
- **Residual P3s:** RT-state-6r and RT-state-17r are acceptable for merge.
- **Deferred to backlog:** RT-state-10..16 and 18, as agreed.

VERDICT: PASS_WITH_FINDINGS


## Re-verification RT-state-19 @ 72d613f

**RT-state-19: FIXED.** No new findings.

Scope: fix commit `7f13626`, merged at `72d613f`. It touches `runtime/schedules.go`, the `ErrBusy` doc comment in `internal/schedule/schedule.go`, ADR-0019 §4, and adds a new regression test. I worked in my own detached worktree `/tmp/lcx-review/wt-state` at 72d613f and removed it afterwards with `git worktree remove`. `wt/core-rt-review` was not touched. Probe sources are archived in `/tmp/lcx-review/state-probes/` (`zz_probe_rv19*`).

What changed: `scheduleActions.RunAgent` now maps `ErrAgentLimitExceeded` to `schedule.ErrBusy`, the same way it maps `ErrRunHeld`. A one-shot task therefore takes the RT-state-2 busy-retry path (its claim is given back with `next_run_at` = now + 2 s), and a recurring task records `skipped_overlap`. `errors.Is` matches the wrapped `ErrAgentLimitExceeded (n running, limit n)` error. The limit can never be zero (config ignores values ≤ 0, and the resource manager falls back to 4), so this can never turn into a permanent retry loop by configuration.

### Probes (`go test -race -count=1 -run …`)

| Probe | Observed |
|---|---|
| `TestZZProbeRVWakeupAtAgentLimit` (original, MaxAgents 1, another session holds the slot) | at the limit: `recorded=false status="" fireCount=0 next=…00:42:03 enabled=true` (before: `status="failed" … next=<nil>`); after the slot was freed: `status="fired" fireCount=1 next=<nil>`. One adaptation: the second half now frees the slot with `sup.Cancel(other.ID)`, not `sup.Close()`. `Close` unregisters the Engine's only agent runner, so a busy wake-up then takes the `ErrNoRunner` 30 s deferral instead, as the unadapted run showed (still pending 4 s later). |
| `TestZZProbeRV19RecurringAtAgentLimit` (interval agent task due at the limit) | `status="skipped_overlap" error="" fireCount=1 next=<+60 s> enabled=true`; next occurrence after the slot was freed: `status="fired" ref=<session> fireCount=2`. |
| `TestZZProbeRV19RetryPressure` (12 pending wake-ups: 3 on each of 4 sessions, limit held for 10 s) | `retries per task 5..5` (one every 2 s); `events map[]`; `journal rows +0`; transcript rows unchanged (4, the session goals); sessions stay `created`; still 3 pending on a session (the cap holds through the retries). |
| `TestZZProbeRV19Drain12` (same 12 wake-ups; each session first bound to a provider that answers at once) | `after 10 s at the limit: pending 12/12, model requests by woken runs 0`; after cancelling the slot holder: `fired 12/12 in 4.9s; woken runs' model requests 12`. Each fired exactly once, and no run started early. |
| `TestZZProbeRV19Drain` / `TestZZProbeRV19Stall` (diagnostics, with goroutine dump and per-second `next_run_at` samples) | The scheduler goroutine is idle in `loop`'s select (`scheduler.go:224`), and `next_run_at` advances every 2 s for every still-waiting wake-up. A first RetryPressure run that showed "fired 1/12" was a probe artefact: `startBlockedRun`'s provider never answers, so the first woken run kept the only slot. The scheduler was not stuck. |
| Regression tests | `TestWakeupDueAtTheAgentLimitFiresWhenASlotFrees`, `TestWakeupDueDuringItsOwnRunFiresAfterTheRunEnds`, `TestBusyOnceTaskFiresWhenTheSessionIsFree`, `TestBusyActionIsSkippedAsOverlap`, `TestEngineWakeups*` → `ok` (runtime, internal/schedule). |

### Holes checked
- **Recurring agent task at the limit:** it records `skipped_overlap` and keeps its next slot; before the fix it was recorded `failed`. So a daily cron agent task whose time falls while the Engine is saturated skips that day. ADR-0019 §4 now states this, and it matches the existing run-lock overlap rule, so it is intended. No regression.
- **Retry pressure while the limit stays exhausted:** bounded and silent.
  - Cost per pending one-shot: one claim and one revert row write every 2 s, plus `RunSupervisor.Start`'s checks before the limit refusal. Those are a session read, a non-blocking run-lock file open, a parent-relation read, a runtime read, and `config.Load`. No model call is made, nothing is written to the transcript or journal, and no event is published.
  - Pending one-shots are capped at 3 per session. With 12 pending, every task still got exactly 5 retries in 10 s, so none starved.
  - Once the slot frees, all of them drain in order and each fires exactly once.
  - Their `next_run_at` stays within 2 s of now, so a second Engine's missed-policy check (more than 2 min overdue) never marks them missed.
- **Interaction with the other busy causes:**
  - While one woken run holds the session, the session's other wake-ups get `ErrRunHeld`.
  - The other sessions' wake-ups get `ErrAgentLimitExceeded`.
  - Both go through the same `overlap` path. No retry was lost and none went to `failed`.

### Verdict @ 72d613f
- **Fixed:** all eleven fixed IDs (RT-state-1, 2, 3, 4, 5, 6, 7, 8, 9, 17, 19), with no regression found.
- **Residual P3s:** RT-state-6r and RT-state-17r, acceptable for merge.
- **Deferred to backlog:** RT-state-10..16 and 18, as agreed.
- **Remaining P0/P1/P2:** none.

VERDICT: PASS_WITH_FINDINGS


## Re-verification host notebook write @ 4cc5eb0

**Host save on a withdrawn session (fix `3027535`): FIXED.** No P0, P1 or P2 issues. One new P3 (RT-state-20) is acceptable for merge.

Scope: the `3027535` diff (`internal/session/notebooks.go` `UpdateNotebook`, `runtime/notebooks.go` `SaveNotebook` and the `ErrNotebookWithdrawn` alias, both new tests, ADR-0020 and agent-runtime.md). I also read the code around it: `sessionFence`, `writeTx`/`writeTxOn` (`BEGIN IMMEDIATE`), `RevokeSession`/`revokeIn`, `notebookAccess.target`/`Update`, `projectionIsolated`, and `resultSessions`/`sessionResultEdges`. I worked in my own detached worktree `/tmp/lcx-review/wt-state` at 4cc5eb0 and removed it with `git worktree remove` when done. Probe sources are saved in `/tmp/lcx-review/state-probes/zz_probe_nbw_*`.

### Probes (`go test -race -count=1 -run …`)

| # | Probe | Observed |
|---|---|---|
| 1 | `TestZZProbeNBWFencedReaderContinues` (N1), via the Engine API. Setup: the parent read the child's notebook, then the child was revoked, which also fences the parent. | Saving the revoked child: `ErrNotebookWithdrawn`, body unchanged, `version=2`, revisions `2->2`. Saving the fenced parent's own notebook: `ErrNotebookWithdrawn`, with `expectedVersion=-1` too; body unchanged, `version=1`, revisions `1->1`; the host list shows `withdrawn=true`. |
| 2a | `TestZZProbeNBWFenceCommitsDuringSave` (S1): two Stores on one DB. Store B holds an uncommitted `revokeIn` in `BEGIN IMMEDIATE`; Store A's host save at the version read before the revocation. | `save still waiting after 400 ms while the fence is uncommitted`. Once the fence commits, the save returns `ErrNotebookWithdrawn` (57 ms later); `body="# Plan\noriginal\n" version=1 revisions=1`. |
| 2b | `TestZZProbeNBWRevocationDuringSave` (S2): the host save is held inside its transaction, after the fence read; B runs `RevokeSession`. | `revocation still waiting after 400 ms while the save holds its transaction`. The save lands (v2; its read came before the revocation, so this is correct). The revocation commits 63 ms after the save releases. The next host save is refused, and the body stays at v2. |
| 2c | `TestZZProbeNBWConcurrentStress` (S3): 60 fresh sessions, each with a save and a revocation started at the same moment. | `fence first, save refused, body unchanged: 28`, `save first, then fenced: 32`, no other outcome, no data race. |
| 3 | `TestZZProbeNBWFencedReaderContinues` (N2). Setup: the child has a transcript; the parent read the child's notebook in its last step (`MaxSteps 1`, no answer derived from it); then the child was revoked. | Parent projection: `isolated="" withheld=[4]`. The next parent run (`notebook_update` append, then `notebook_read` of its own plan) ends `status="completed"`. Plan `version=2 updatedBy="agent:<run>"`, append present. The model was given its own body (1 message) and the child's sentinel 0 times. The host save afterwards is still `withdrawn`. |
| 3' | `TestZZProbeNBWExposedReaderIsolated` (N3): the parent's model answered after the read. | The next run is refused before any request: `fenced … (exposed)`, `requests sent=0`. The plan is unchanged, so no agent write can reach it. |
| 4 | Regression tests | 23 tests PASS, 0 FAIL in `runtime` + `internal/session`, including `TestReadExitsAfterRevokingASubagent`, `TestDeletingASessionRemovesEveryCopyOfItsContent`, `TestDeletingAChildRemovesItsNotebookFromTheParentTranscript`, `TestSaveNotebookOfAWithdrawnSessionIsRefused`, `TestHostNotebookWriteOfAWithdrawnSessionIsRefused`, every `*Notebook*` test, and `TestSaveNotebookConcurrentWritersHaveOneWinner`. `internal/notebook` and `internal/toolset` → `ok`. |

**Serialisation claim: holds.** The fence is read on the pinned connection after `BEGIN IMMEDIATE`, which takes the database write lock. Every fence write is a write transaction (`RevokeSession` → `writeTx` → `revokeIn`). So a fence commits either before the save's read, and the save is refused (2a), or after the save commits, and the save's read genuinely came first (2b). This held across processes and under `-race` (2c).

**Note on the fix's own test:** `TestSaveNotebookOfAWithdrawnSessionIsRefused` checks the "fenced reader continues" case only by calling `notebookAccess.Update` directly. In the `childWithNotebook` fixture the child has no transcript, so the read edge is stored incomplete (`complete=0`, the documented no-seq rule), and the reader becomes `incomplete_lineage` and could not actually continue. Probe 3 covers the real path: a child with a transcript, then a real continuing run. No defect, only a coverage gap in that test.

### Is refusing only host writes the right line? Yes.
- **Same predicate as the host reads.** The refusal uses `sessionFence`, the same check behind `SessionWithdrawn`, which blanks the host's reads. So it covers exactly the sessions whose bodies the host is not shown: no session is visible but unsaveable, and none is saveable while hidden.
- **Agent writes are never blind.** `notebookAccess.Update` writes only the run's own session (`a.sessionID`), and `target()` never hides a run's own session from it. Probe 3 confirms the model was given its body. Its compare-and-set is therefore an informed one, so the reason behind the fix doesn't apply.
- **Agent writes can't carry the withdrawn content.** An agent write reaches a withdrawn session only through a run admitted after projection (probe 3). A reader whose answer derived from the fenced content is isolated and never starts (probe 3'). So a continuing reader's own notebook can't hold the child's content.
- **Refusing agent writes would only freeze a legitimate thread.** Its notebooks would stop updating, with no privacy gain.
- **Delete staying open is right.** It is a deliberate destroy, not a blind overwrite, and it is the user's privacy path. S4: a host delete of the unseen notebook works (`get after: notebook not found`).

### New and residual findings @ 4cc5eb0

| ID | Sev | file:line | Evidence | Probe | Minimal fix |
|---|---|---|---|---|---|
| RT-state-20 | P3 | `internal/session/notebooks.go` `CreateNotebook` (no fence check) vs `:349-355` | Create was left unchanged, so on a withdrawn session the host can create a notebook. From then on it can neither read it (withdrawn) nor save it (`ErrNotebookWithdrawn`). A Desktop "new notebook" in a fenced main thread becomes write-once, invisible content. Nothing is lost (the agent can use it), so this is UX only. | `TestZZProbeNBWHostCreateDeleteOnWithdrawn` (S4): `host create on withdrawn: err=<nil> version=1; host save of what it just created: err=…withdrawn…` | Apply the same `author == "host"` `sessionFence` check inside `CreateNotebook`'s write transaction. Not needed for merge. |

**Observation for the Owner (from ce5c4ca's read rule; not a defect of 3027535).** `sessionFence` ignores projection. So once a revocation reaches a reader, here the user's main thread, the host permanently loses read and now write access to *all* that session's notebooks. That includes ones the agent keeps writing after the thread was legitimately projected and continued (probe 3: agent v2 written, host save still withdrawn).
- **Possible alternative [INFERENCE]:** by the argument above, a *projected* reader's notebooks cannot hold the fenced content, so host read and save could be allowed for a projected reader.
- **Not changed for merge:** this would change both RT-state-3's read rule and this fix together; it is a product decision.

### Verdict @ 4cc5eb0
- **Fixed:** the host-notebook-write hole is closed, with all four requested checks satisfied, and RT-state-1–9, 17 and 19 remain FIXED.
- **Residual P3s:** RT-state-6r, RT-state-17r and RT-state-20, all acceptable for merge.
- **Deferred to backlog:** RT-state-10..16 and 18, as agreed.
- **Remaining P0/P1/P2:** none.

VERDICT: PASS_WITH_FINDINGS
