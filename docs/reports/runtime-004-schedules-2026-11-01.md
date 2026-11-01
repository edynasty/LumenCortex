# RUNTIME-004: timers and scheduled tasks (slice report)

- **Status:** implemented, tested on `rt/schedule`
- **Base:** `44c534a` (execution runtime base, ADR-0019)
- **Contract:** `docs/adr/0019-execution-runtime.md` §4
- **Branch:** `rt/schedule` in worktree `wt/core-rt-sched`

## Scope

Durable scheduled tasks and delayed wake-ups. Out of scope (other slices): the job queue
(`internal/jobs`), diagnostics storage, Desktop UI, agent-loop changes.

## What was built

| Area | File | Notes |
|---|---|---|
| Row SQL | `internal/session/schedules.go` | insert/get/list/update/delete, due query, earliest due, claim, revert, missed advance, result write, observation, disable |
| Schema | `internal/migrate/sql/0008_schedules.sql` | added `cron_expr`, `timezone`; `trigger_kind` accepts `cron` |
| Scheduler | `internal/schedule/schedule.go`, `scheduler.go` | one goroutine, one timer, kick channel, missed policy, overlap skip, `ErrNoRunner` |
| Cron | `internal/schedule/cron.go` | 5-field parser + next-fire search, Vixie day rule, DST, 5-year bound |
| Engine binding | `runtime/schedules.go` | Actions, events, journal, Engine API, wake-up controller |
| Wake-up tools | `internal/toolset/wakeup.go` | `wakeup_schedule`, `wakeup_cancel` (`write`) |

### Firing protocol

1. `DueSchedules(now)` reads the due rows.
2. `ClaimSchedule(id, next_run_at_read, next)` is one `UPDATE … WHERE schedule_id = ? AND next_run_at IS ?
   AND enabled = 1`; a lost claim means another scheduler took the occurrence and nothing runs.
3. Overlap: `Actions.Active(previous last_ref)` → `skipped_overlap`, no new firing. An action that
   reports `schedule.ErrBusy` (the runtime maps a held run lock to it) is the same case.
4. `Actions.RunShell` / `RunAgent`; `ErrNoRunner` gives the claim back (`RevertScheduleClaim`) and defers
   the task locally for 30 s, so it stays due for a process that has a runner.
5. `RecordScheduleResult` writes `last_status`/`last_ref`/`last_error`; the host is notified.

### Missed occurrences

Applied at `Start` before the loop: `skip` advances `next_run_at` past the missed slots (a one-shot is
disabled) and records `missed`; `run_once` fires one occurrence and then advances. Interval catch-up never
produces a burst: the next slot is the first one strictly after now.

### Cron

Standard 5 fields (`*`, numbers, ranges, lists, `*/n`, `a-b/n`), read in an explicit IANA zone (`UTC` when
absent; `time/tzdata` is imported so the zone resolves on hosts without zoneinfo). Day-of-month and
day-of-week use Vixie's OR rule when both are restricted. A local time the spring-forward gap skips does
not fire; a wall time the fall-back hour repeats fires once. The search is bounded at five years and
returns `ErrNoNextFire` instead of spinning.

### Engine API

`Schedules`, `Schedule`, `CreateSchedule`, `UpdateSchedule`, `DeleteSchedule`; `ScheduleInfo` carries
`trigger {kind, at, everySeconds, cron, timezone}`, flat `cron`/`timezone`, `nextRunAt`, `lastFiredAt`,
`lastStatus`, `lastRef`, `lastError`, `fireCount`. Events `schedule.changed` and `schedule.fired`; journal
rows carry `scheduleId`, `kind` and `status` only (content stays in `scheduled_tasks`).

## Decisions and deviations

- **0008 amended, not a new 0009.** The cron expression and zone are columns of the task row; keeping them
  there keeps the claim and the missed-policy advance one statement. `TestV8DownRoundTrip` passes.
  *Limitation:* a v8 database created by a build of this branch before this amendment keeps the old
  table shape; delete the state directory (or downgrade to 6 and upgrade) to recreate it. No released
  binary wrote v8.
- **`scheduledJobs` stays nil in this branch.** `RunShell` returns `job queue unavailable`, which the
  scheduler records as a failed firing; the integrator binds the job manager at merge.
- **Agent ref is the session id.** `Active` asks the supervisor whether that session runs, which is the
  question the overlap rule needs; a run started elsewhere is still a live run.
- **Wake-up prompt prefix** `[runtime] Scheduled wake-up: ` marks tasks the tools created; the pending
  limit (3) counts only those. The tools are bound for thread runs only: a child run's lifecycle belongs
  to its parent, and the binding uses the run's capped policy so a wake-up never widens it.

## Tests

| Command | Result |
|---|---|
| `go test ./internal/schedule/ -count=1 -race` | pass |
| `go test ./internal/session/ -count=1` | pass (35 s) |
| `go test ./internal/session/ -run 'Schedule\|Claim\|Journal' -race` | pass |
| `go test ./internal/migrate/ -count=1` | pass |
| `go test ./internal/toolset/ -count=1` | pass |
| `go test ./runtime/ -count=1` | pass |
| `go test ./runtime/ -run 'Schedule\|Wakeup' -race` | pass |
| `go build ./...`, `go vet` on the touched packages, `gofmt -l` | clean |

Coverage: once exactly once; interval next times and no catch-up burst; disable/enable; delete; missed
policy across a close/reopen for once and interval and cron; overlap skip; two schedulers on one database
(two store handles) and an 8-goroutine claim race at the SQL layer; action failure recorded; `ErrNoRunner`
keeps the task due and the retry after a runner registers; `Close` stops the loop; `ObserveRef` and stale
refs; vanished session and task deleted mid-firing; cron parsing table, timezone, DST gap and repeated
hour, never-matching bound; a busy action recorded as an overlap; Engine API CRUD, cron and wake-up
scoping; wake-up tool bounds and permissions.

## Open items for the integrator

- Bind `Engine.bindScheduledJobs` to the job manager (`SubmitScheduled`, `JobActive`).
- Diagnostics (`OBS-001`) should adopt the scheduler's logger instead of `slog.Default()`.
- Desktop: `CreateSchedule` accepts `trigger.kind = "cron"` with `cron` and `timezone`.
