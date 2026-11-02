# Local-merge inventory: `main` (3e1c3cc) ← `rt/execution-runtime` (006b031)

**Method / caveat.** This session has no shell tool, so `git log/show/diff` could not be executed. Every claim below comes from reading the three checkouts that sit at the exact commits (verified by reading the ref files directly: `.git/refs/heads/main` = `3e1c3cca…`, `.git/refs/heads/cog/r1a` = `15bf4eb5…`, `.git/refs/heads/rt/execution-runtime` = `006b0319…`) plus the untracked design docs. Commit counts (82 / 56 / merge-base `c08f5c4`) are taken as given and match `RUNTIME_UPGRADE_FINAL_REPORT_20261101.md:20` ("56 个提交；`git merge-tree main rt/execution-runtime` 无冲突，合并树 = rt 树 `3f2e4e3`").

## Q1 — R1a delta (`main..cog/r1a`)

| Area | What lands | file:line |
|---|---|---|
| Schema 0006 | 4 tables: `request_envelopes`, `derived_origins`, `source_fences`, `check_approvals` | `wt/core-r1a/internal/migrate/sql/0006_provenance_and_approvals.sql:25,61,95,111` |
| | 8 indexes: 2 on envelopes, 3 on origins, 1 fences, 1 approvals, 1 `eval_results(session_id,episode_id)` | `0006…sql:54,55,88,89,90,106,132,143` |
| | 6 columns added to `eval_results` (`episode_id`, `pre_fingerprint`, `post_fingerprint`, `spec_digest`, `approval_epoch`, `episode_status`) | `0006…sql:137-142` |
| | Nothing backfilled; tables created **without** `IF NOT EXISTS` so a name clash fails the migration | `0006…sql:1-22` |
| Down file | Drops all 8 indexes, then the 4 tables, then the 6 columns — structural inverse; rows are lost (backup taken first) | `…/sql/down/0006_provenance_and_approvals.down.sql:1-19` |
| Schema version "constant" | No literal: `Set.Latest()` = number of up files = 6; asserted by test | `internal/migrate/migrate.go:120-122`, `internal/migrate/migrate_test.go:280-281`; fences gate `fencesVersion = 6` at `migrate.go:59-60` |
| Cognition on an ordinary run | **None.** `internal/cognition` is imported only by `runtime/governor.go` and `cmd/lcx-go/{main,governor}.go`; `GovernorPlan` has no run-path caller (only `lcx-go governor plan`) | `runtime/governor.go:12`, `cmd/lcx-go/main.go:147-196` |
| Curator | Fails closed before any provider call: `ErrGovernorCuratorDisabled` when `curator != nil`; deterministic plan only | `runtime/governor.go:120-128` |
| Config keys / env flags for cognition | None — no new key in `File` and no `os.Getenv` in `runtime`/`config` beyond `XDG_*`/`PI_CODING_AGENT_DIR` | `config/types.go:119-131`, `runtime/statedir.go:63`, `config/dir.go:20` |
| Auto-effort judge | Runs only when `effort.default` (or an agent's effort) is `auto`; no judge model → heuristic, no envelope | `runtime/agent.go:308-324`, `runtime/models.go:228-237` |
| Provenance (always on) | One `AdmitRequest` per provider attempt; one `AdmitExecution` per side-effect tool; fence read inside both | `internal/agent/send.go:167` → `internal/agent/envelope.go:141-158`; `internal/agent/execution.go:168` |
| Protected-path guard | Pre-existing, unchanged: state dir + legacy `.lumencortex` trees are unreachable to file tools | `runtime/statedir.go:249-258` (identical text in `LumenCortex-Core/runtime/statedir.go:249-258`) |
| Approvals / SpecDigest | New: `SpecDigest`/`SetDigest`, set-level approval epoch, `check_approvals` rows, `ApproveChecks`/`ApproveConstraintBinding`/`RevokeApproval` | `internal/evaluator/evaluator.go:100,102-104`; `runtime/approvals.go:19-27,174,217,250,288` |
| **Policy change for existing users** | An existing `checks` block is *unapproved*: required checks are decided `spec_unapproved` without running, and a non-interactive run ends `failed/spec_unapproved` | `runtime/checks.go:184-186`; `internal/agent/loop.go:316-318`; `internal/agent/types.go:247-250`; `internal/evaluator/run.go:39-42` |
| | `AcceptCheck` refuses an unapproved definition | `internal/evaluator/run.go:339-351` |
| | Journal is now a whitelist: tool args, output, goals, final text and shell commands are no longer written | `internal/session/store.go:347-350` |
| | `GovernorPlan` with a curator now returns an error instead of a model plan (CLI-visible only) | `runtime/governor.go:120-128` |
| Breaking public Go API (`runtime`,`protocol`,`pluginapi`) | **No removal/rename found.** `pluginapi.APIVersion` still `1`; `Plugin`/`Host` interfaces identical; `protocol/agent.go` constants/`Message` identical; all 70 `func (e *Engine)` methods of `main` exist in r1a with identical signatures (`DeleteSession`, `ReadThread`, `Recall`, `SessionChecks`, `AcceptCheck`, `GitStatus`, `GitDiff`, `ApplySessionWorktree`, …) | `pluginapi/pluginapi.go:33,62-76` (both trees); `LumenCortex-Core/runtime/*.go` vs `wt/core-r1a/runtime/*.go` |
| | Additive only: new files `approvals.go, constraints.go, deletion.go, projection.go, revoke.go, catalog.go, transcript.go`; new fields on `ChecksInfo`/`CheckSpec`/`CheckResult` (`setDigest`, `approved`, `inputs`, `approvalEpoch`, episode fields) | `runtime/checks.go:227-248`, `internal/evaluator/evaluator.go:44-62,177-181` |
| | Residual unknown: a full `git diff main cog/r1a -- runtime protocol pluginapi` was not runnable here, so an unlisted exported *struct field* removal cannot be excluded | — |
| Config / CLI | No new `lumencortex.json` keys (`File` identical to `main`) | `config/types.go:119-131` vs `LumenCortex-Core/config/types.go:119-130` |
| | New CLI: `lcx-go checks status \| approve <set-digest> [--actor] [--decision-ref]` | `cmd/lcx-go/checks.go:14-36`, `cmd/lcx-go/main.go:220-221` |
| | `lcx-go db downgrade --to N` already exists in `main` (not new) | `LumenCortex-Core/cmd/lcx-go/main.go:33-37`, `db.go:14-24` |
| | No new env vars (`LCX_EVENTS`, `LCX_POLICY`, `LCX_MAX_*`, `LCX_EFFORT`, `LCX_WORKFLOW`, `LCX_WORK_UNITS`, `LCX_AGENT*`, `LCX_WORKTREE_BASE`) | `cmd/lcx-go/main.go:45,468-501` (both trees) |

## Q2 — Migration engine at `rt` HEAD

| Question | Answer | file:line |
|---|---|---|
| Automatic at `Open`? | Yes. `Engine.Open` → `session.OpenBound` → `openStore` → `migrate.Open`; `lcx-go db …` runs *before* Open so it never migrates | `runtime/engine.go:245`; `internal/session/store.go:79-88`; `cmd/lcx-go/main.go:33-37` |
| Forward-only? | `Open` only upgrades (under exclusive `db.lock`, re-reading `user_version`); downgrade is a separate explicit entry point | `internal/migrate/workspace.go:54-105`, `migrate.go:301-360`, `migrate.go:452-547` |
| Backup before migrating? | Yes, before any write: `VACUUM INTO <state>/backups/lumencortex.db.v<N>.<UTC ts>.partial`, reasoning scrubbed, then renamed | `migrate.go:336` (upgrade), `migrate.go:515` (downgrade), `internal/migrate/backup.go:38-70,121-146` |
| | Skipped for a database with no `sessions` table (fresh workspace) | `backup.go:48-56` |
| | **No retention/pruning** — backups accumulate; this is exactly undecided O-2 ("备份保留期限") | `backup.go` (no prune of final names), `OWNER_O1_AUTHORIZATION_20261025.md:17` |
| Binary whose highest schema is N opens a DB at version > N | Refused **before** journal mode/backup/migration, writing nothing: `ErrSchemaTooNew` = `"database schema is newer than this binary"` wrapped as `"…: have user_version %d, binary knows %d"` | `migrate.go:44-45`, `migrate.go:237-238`, callers `workspace.go:150-153`, `migrate.go:304` |
| What v0.1.4/`main` does with a v9 DB | Same refusal, `latest = 5`, so the message is `database schema is newer than this binary: have user_version 9, binary knows 5`; file + WAL left byte-identical | `LumenCortex-Core/internal/migrate/migrate.go:234-235`, `Latest()==5` at `LumenCortex-Core/internal/migrate/migrate_test.go:280-281`, test `LumenCortex-Core/runtime/schema_test.go:77-78`; `runtime/engine.go:43` |
| Down-migration runner reachable by a user? | Yes, not test-only: `lcx-go db downgrade --to N` → `runtime.DowngradeSchema` → `migrate.Downgrade`; refusals: no down file (`migrate.go:491-493`), live fences below v6 (`migrate.go:495-499`), newer DB, workspace open elsewhere (`ErrDatabaseInUse`) | `cmd/lcx-go/db.go:14-24`, `runtime/engine.go:58-73`, `migrate.go:452-547` |
| | In `main` the same CLI exists but its down set is only 0002–0005 (no 0001 down), so `--to 0` is refused | `LumenCortex-Core/internal/migrate/migrate.go:439-441`, `LumenCortex-Core/internal/migrate/sql/down/` |
| Are 0006–0009 down files complete inverses? | Structurally yes: 0006 drops 8 indexes + 4 tables + 6 columns; 0007 drops 3 indexes + `jobs` (leaves `<state>/jobs/` output files, documented); 0008 drops 2 indexes + `scheduled_tasks`; 0009 drops 1 index + `notebook_revisions` + `notebooks`. Data is intentionally lost; a backup precedes every downgrade | `sql/down/0006…:1-19`, `0007_jobs.down.sql:1-5`, `0008_schedules.down.sql:1-4`, `0009_notebooks.down.sql:1-5` |

## Q3 — Classification of `R1A_RELEASE_PREP_20261031.md` §2 (B1…B7) and Owner decisions

Legend: **(a)** needed for a local merge into `main` · **(b)** DB rollback guarantee (G0b/T0-S2) · **(c)** formal release · **(d)** tag/push authorization.

| Item | Class | Deciding sentence (quote) | Where |
|---|---|---|---|
| B1 R1a → `main` | **(a)** authorization only; engineering is clean (`merge-tree` no conflict, merge tree = rt tree) | "G-R1a 结论见 §0；O-6 决定把「R1a 合入 main」列为未批准 \| 授权合入" | `R1A_RELEASE_PREP_20261031.md:93`; clean-merge evidence `RUNTIME_UPGRADE_FINAL_REPORT_20261101.md:20` |
| B2 G0b prerequisite | **(b)** | "二选一：批准 O-2 并先做 T0-S2/G0b；或书面豁免 G0b 发布（后果：v0.1.4 的旧库在 R1a 下原地打开并升级到 schema v6，回退只能用 `lcx-go db downgrade --to 5`，没有迁移/反向迁移）" | `:94`; G0b definition "T0-S2 故障矩阵由 Sonnet 独立重放；反向迁移后 `v0.1.4` 可以打开" `OPUS_ENGINEERING_EXECUTION_PLAN.md:878`; reverse step `lcx-go state migrate --reverse` `:777` |
| B3 version constant | **(c)** | "常量 `0.1.4-preview`，`v0.1.4` 已存在，`release-check.sh --mode release` 不可能通过" | `:95`; constant `wt/core-r1a/runtime/engine.go:35`, `wt/core-rt/runtime/engine.go:40`; release mode requires the exact tag `v<version>` on HEAD `scripts/release-check.sh:68-71` |
| B4 gates after the version commit | **(c)** | "版本提交会产生新 SHA，G-R1a 不能继承" | `:96`; the verdict is SHA-bound: "本裁决只对应 SHA `15bf4eb…`" `G_R1A_REREVIEW_20261031B.md:1-3` |
| B5 Core tag + push | **(d)** | "未授权 \| 授权打 tag、push `main` 和 tag" | `:97`; plan "未经 Owner 授权，不推送、不打 tag" `OPUS_ENGINEERING_EXECUTION_PLAN.md:789` |
| B6 Desktop upgrade | **(c)** (Desktop scope) | "T7-S1 是 Desktop 升级到 R1a Core 的前置；没有审批 UI 时，升级后 required check 全部无法通过" | `:98`; plan "Desktop 随后固定已发布的 tag" `:787` |
| B7 Desktop unpushed `4a6c2f3` | **(c)** | "本地 \| 随 Desktop 发布一起处理" | `:99` |
| O-2 | **(b)** | "**O-2：** 旧库迁移方式与备份保留期限；T0-S2 合入仍受限。" | `OWNER_DECISIONS_T0S1_O3_O4_20261026.md:34`; `OWNER_O1_AUTHORIZATION_20261025.md:17` |
| O-5 | **(a)** boundary — blocks product enablement, not the merge | "**O-5 未批准：** R1b/T5a、T5b 及认知产品启用。" | `OWNER_O1_AUTHORIZATION_20261025.md:20`; `OWNER_DECISIONS_T0S1_O3_O4_20261026.md:35` |
| O-6 | **(d)** (merge/push/tag/release) + scoped model-experiment approval | "O-2；O-5（…）；正式 R2；R1a 合入 `main`；Push、Tag、Release、Deploy；产品默认启用。" — while "**APPROVED（限定范围）：** G-R1a 通过后，使用 `local/deepseek-v4.1-flash` …" | `OWNER_DECISIONS_R2_O6_20261026.md:23`, `:13` |
| Release prerequisite (general) | **(c)/(d)** | "**Core 发布前置：** G-R1a、G0b，以及 Owner 的发布授权。" | `OPUS_ENGINEERING_EXECUTION_PLAN.md:785` |

**Merge-blocking observation.** Merging `rt` (not just `cog/r1a`) brings 56 commits beyond the only SHA any gate signed (`15bf4eb`, `G-R1a=PASS`). Those commits have self-run evidence only (`go test`, `-race`, 21/21 no-model E2E — `RUNTIME_UPGRADE_FINAL_REPORT_20261101.md:203-205,11`), and the final report itself records the merge as blocked: "**BLOCKED**：Core 分支包含整条 `cog/r1a`，其合入 `main` 未获批准（…§2 B1/O-6）" (`:12`, also `:350`).

## Q4 — rt delta (`cog/r1a..rt`)

| Topic | What changes | file:line |
|---|---|---|
| Schema 0007 | `jobs` (durable background-command queue; states `queued…outcome_unknown`; output kept in files, only byte counts in DB) + 3 indexes | `wt/core-rt/internal/migrate/sql/0007_jobs.sql:27,56-58` |
| Schema 0008 | `scheduled_tasks` (once/interval/cron, IANA tz, `missed_policy` default `skip`, `enabled` default 1) + 2 indexes | `0008_schedules.sql:19,45-46` |
| Schema 0009 | `notebooks` (+`UNIQUE(session_id,title)`, 1 index) and `notebook_revisions` (last 50 bodies) | `0009_notebooks.sql:19,34,37` |
| Schema version | `Latest()` = 9, asserted | `internal/migrate/migrate_test.go:280-281` |
| `shell` → `bash` | Model-visible name is `bash`; a rename map translates old names for host lists, workflow `allowedTools` and `tool` conditions; a model calling `shell` gets a corrective tool result naming `bash`; **files are not rewritten** — mapping happens at read | `protocol/toolnames.go:4,9,17-22,28-45`; `internal/agent/loop.go:136-137,466-468`; `runtime/prompt.go:396,405`; `runtime/capabilities.go:136-137`; `internal/workflow/definition.go:141-142`; `internal/workflow/eval.go:65-66` |
| `git_status` / `git_diff` tools removed | Gone from the model's tool list; the **host** Git API is unchanged | `internal/toolset/toolset.go:238-269` (comment `:241-242`); `runtime/repository.go:31-37,115-129` |
| New tools | `job_status`/`job_wait`/`job_list` (plus `job_cancel`) only when a job queue is bound; `notebook_list`/`notebook_read`/`notebook_update`; `wakeup_schedule`/`wakeup_cancel`; bash gains `mode:"background"` | `internal/toolset/toolset.go:304,314-320`; `internal/toolset/jobs.go:92-120`; `internal/toolset/notebook.go:63-90` |
| New config keys (global file only) | `execution.{shellTimeoutSeconds, maxConcurrentJobs, maxQueuedJobs, jobOutputBytes, jobRetentionDays, logLevel, budget{…}}`; defaults 2 / 32 / 4 MiB / 7 d / `info` / no shell timeout / no budget; `lsp.{idleTimeoutSeconds 600, maxServers 3, start 20 s, request 20 s, servers}`; `providers.*.models.*.cost{input,output}` | `config/types.go:181-214,237-249,296-327,443-452` |
| New CLI env | `LCX_DEADLINE`, `LCX_DEADLINE_RESERVE`; new `lcx-go plugins {enable,disable,capabilities,cost,templates,apply,server enable\|disable}` | `cmd/lcx-go/main.go:494,573`; `cmd/lcx-go/plugins.go:63-139` |
| MCP templates | Data only, applied as **disabled** entries; nothing is started at Open | `runtime/templates.go:11-27`; `docs/plugins-mcp-templates.md:100-104`; `RUNTIME_UPGRADE_FINAL_REPORT_20261101.md:81` |
| Schedules by default | `scheduled_tasks` starts empty; the scheduler goroutine runs but has nothing due until a row exists (`wakeup_*` creates one-shot agent tasks) | `runtime/engine.go:350-356,399-404` |
| Runs automatically at Engine Open | `reconcileRunning` (dead `running` sessions settled); job manager `Start` → `recover` (dead owner's *running* jobs → `outcome_unknown`, never re-run; its *queued* jobs adopted) then retention `Sweep`; `resumeDeletions`; `schedules.Start`; diagnostics log opened | `runtime/engine.go:381,387-400,401-404,424-428`; `internal/jobs/jobs.go:204-228,242-244,229`; `runtime/diagnostics.go:149-157` |
| Diagnostics files | `<state>/logs/lumencortex.jsonl` (+ `lumencortex.N.jsonl`), 8 MiB × 5 files, 14-day retention, level from `execution.logLevel`; redacted; failure falls back to a discard logger (Open still succeeds) | `runtime/diagnostics.go:146-157`; `internal/diag/diag.go:31-38,122-168,358-362` |
| Close | Scheduler first, then jobs (kills running job process groups), store, log last | `runtime/engine.go:452-484` |

**Not determinable here (`unknown`):** exact commit-level diff statistics per file, and whether any *unlisted* exported struct field disappeared in `runtime` between the two trees — both need `git diff`, which this session could not run.