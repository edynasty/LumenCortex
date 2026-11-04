# H-1 — Test-contract inventory (report)

- **Date:** 2026-10-25
- **Slice:** H-1 (execution plan §5), read-only inventory. No test, source or Desktop file was run or modified.
- **Branch / base:** `cog/h1` at `4c20959` (`cog/r1a`). Worktree `wt/core-h1`.
- **Core scope:** the worktree above. **Desktop scope:** `/Users/tangxingpeng/IdeaProjects/me/LumenCortex/LumenCortex-Desktop` at its working tree (`go.mod`: Core `v0.1.4`), read only.
- **Status:** inventory complete. Every row is a grep hit; no assertion was evaluated by running tests.
- **Contract sources:** `LumenCortex-Cognitive-Design-20261023/OPUS_FINAL_TECHNICAL_APPROVAL.md` §4 (C-1…C-9),
  `.../DATA_AND_FAILURES.md` §3/§5/§6/§7, `.../OPUS_ENGINEERING_EXECUTION_PLAN.md` §5 (H-1, T0-S1, T1-S1…S4).

## 0. Baseline finding

`cog/r1a@4c20959` **already contains T0-S1 and T1-S1** (external state + workspace binding + path guard;
schema v6 + ADR-0018 Store contract + run id after `Reconcile`). Therefore:

- Class 1 is mostly **legacy-mode fixtures, project configuration, and guard fixtures** that must keep working
  exactly as written; the only assertion that still encodes "the database lives in the workspace" is in **Desktop**
  (C1-D1 below), which is pinned to Core `v0.1.4`.
- No Core test calls `Store.Journal` and no test asserts journal payload content, so C-7's projection changes no
  existing assertion (class 3).
- No test asserts the `goal`/`final` fields of a parent checkpoint; only the checkpoint `reason` is asserted
  (class 4). C-7's reduction of `subagent.spawn`/`subagent.stopped` payloads is therefore unasserted today.

## 1. Reproducibility

All commands are run from the repository root (Core worktree, or the Desktop repository for the `D` commands).
`grep` prints one line per hit; every row's `file:line` is one of those hits. Counts are `lines/files`.

| ID | Command | Result |
|---|---|---|
| G0 | `grep -rn --include='*_test.go' '^func Test' .` | every test declaration |
| G1 | `grep -rn --include='*_test.go' -e '\.lumencortex' -e 'lumencortex\.db' .` | 100 / 22 |
| G1n | `grep -rln --include='*_test.go' '\.lumencortex"' .` | 15 files (the plan's §5 figure) |
| G2 | `grep -rn --include='*_test.go' -e 'workspaceKey' -e 'stateRoot(' -e 'externalStateDir' -e 'canonicalPath(' .` | 2 files |
| G3 | `grep -rn --include='*_test.go' -e 'Reused' -e 'reused' .` | 24 / 6 |
| G3b | `grep -rn --include='*_test.go' 'Satisfies(' .` | 2 (both `internal/evaluator/evaluator_test.go`) |
| G4 | `grep -rn --include='*_test.go' 'journal' .` | 38 / 8 |
| G5 | `grep -rn --include='*_test.go' -e 'SessionCheckpoints' -e 'AppendCheckpoint' -e 'Checkpoints(' -e 'subagent.spawn' -e 'subagent.stopped' .` | 10 / 3 |
| G5b | `grep -rn --include='*_test.go' -e 'SubagentTree' -e 'session_checkpoints' .` | 21 / 10 |
| G6 | `grep -rn --include='*_test.go' -e 'AutoEffort' -e 'ClassifyWithJudge' -e 'ReusedAutoDecision' -e 'start_atomic' .` | 30 / 7 |
| G7 | `grep -rn --include='*_test.go' -e 'AutoDone' -e 'CompletionGate' -e 'completion gate' -e 'completion_blocked' -e 'required check' -e 'Required' .` | 53 / 12 |
| G8 | `grep -rn --include='*_test.go' -e 'Reconcile' -e 'reconcile' .` | 60 / 8 |
| D1 | Desktop: `grep -rn -e '\.lumencortex' -e 'lumencortex\.db' --include='*test*' .` | 21 / 12 |
| D1n | Desktop: `grep -rn '\.lumencortex"' --include='*test*' .` | 6 / 4 (the plan's §5 figure) |

`G1` is broader than the plan's `G1n` because it also finds backup-file names (`lumencortex.db.vN.`), SQLite
`-journal` file names and `lumencortex.db` fixtures. The rows below are the in-scope subset; §9 lists the excluded
hits.

Convention: a row's `file:line` is either a hit of the class's own grep (G1–G8, G5b, G3b, D1/D1n) or, where the
class grep only matches inside the function body, the test's declaration line from **G0**. Both are listed above,
so every row is reproducible with one of the commands in this table.

## 2. Per-class counts

| Class | Grep | Files (in scope) | Rows | `NEEDS_SONNET` |
|---|---|---|---|---|
| 1 default state path / DB location | G1, G1n, G2, D1n | 19 Core + 4 Desktop | 43 Core + 4 Desktop | 0 |
| 2 cross-run fingerprint reuse `Reused` | G3, G3b | 2 (+2 cross-ref) | 8 (+2 cross-ref) | 4 |
| 3 journal content | G4 | 5 | 7 | 0 |
| 4 parent checkpoint goal/final | G5, G5b | 3 | 7 | 0 |
| 5 Auto judge ordering + `start_atomic_test.go` | G6, G0 | 4 | 13 | 0 |
| 6 completion gate / AutoDone / required checks | G7 | 8 | 17 | 9 |
| 7 Reconcile | G8 | 7 | 17 | 0 |

Row IDs (`C1-01` …) are the ledger keys later slices must cite when they change a test (plan H-1: "后续所有切片的
测试变更都必须引用台账条目").

## 3. Class 1 — default state path and database location

Clause key: **C-1** (`OPUS_FINAL_TECHNICAL_APPROVAL.md` §4) + plan §5 **T0-S1** decisions 1–7 + **DATA §7** R0.
All Core rows below are already aligned with T0-S1 and must stay as they are; they are listed because they are the
tests that would move if the state path rule ever changes again.

### 3a. State-root derivation (G2)

| ID | file:line | Test | Asserted | Clause |
|---|---|---|---|---|
| C1-01 | `runtime/statedir_test.go:24` | `TestStateRootFollowsXDGStateHomeOnlyWhenAbsolute` | absolute `XDG_STATE_HOME` → `<xdg>/lumencortex/workspaces`; otherwise `~/.local/state/...` | C-1 / T0-S1(1) — should not change |
| C1-02 | `runtime/statedir_test.go:43` | `TestWorkspaceKeyShape` | key is `<sanitized basename>-<16 hex>`, `workspace-` fallback, bounded length | C-1 / T0-S1(2) — should not change |
| C1-03 | `runtime/statedir_test.go:81` | `TestCanonicalPathAliasesAgree` | symlink, chain, `..`, trailing slash and relative spellings canonicalize to one path | C-1 / T0-S1(2) — should not change |
| C1-04 | `runtime/statedir_test.go:125` | `TestCanonicalPathFoldsCaseAndNormalization` | case and NFC/NFD spellings share one state key | C-1 / T0-S1(2) — should not change |
| C1-05 | `runtime/statedir_test.go:155` | `TestCanonicalPathOfMissingDirectoryFails` | a workspace that does not exist has no canonical path | T0-S1(2) — should not change |

### 3b. Engine open: external, legacy, binding (G1)

| ID | file:line | Test | Asserted | Clause |
|---|---|---|---|---|
| C1-06 | `runtime/state_test.go:82` (also 87, 111, 123) | `legacyState` helper (fixture for the tests below) | a legacy DB is `<workspace>/.lumencortex/lumencortex.db`, schema 5, no binding | T0-S1(4) legacy in place — should not change (fixture) |
| C1-07 | `runtime/state_test.go:201` | `TestNewWorkspaceStateIsExternal` | state is `<root>/<key>`, the workspace gains no `.lumencortex`, binding is the canonical path, `Health().Database` is the external file | C-1 / T0-S1 — should not change |
| C1-08 | `runtime/state_test.go:216` | `TestEquivalentWorkspacePathsShareOneState` | every alias opens one state directory and one history | T0-S1(2) — should not change |
| C1-09 | `runtime/state_test.go:251` | `TestDifferentWorkspacesDoNotShareState` | two workspaces never share a state directory | T0-S1(2) — should not change |
| C1-10 | `runtime/state_test.go:266` | `TestLegacyStateOpensInPlace` | `StateInfo.Mode == legacy`, path is `<ws>/.lumencortex`, `db.lock` stays there, no external state created | T0-S1(4) — should not change |
| C1-11 | `runtime/state_test.go:297` | `TestExternalStateWinsOverLegacyResidue` | external state wins, legacy residue is reported and untouched | T0-S1(4) — should not change |
| C1-12 | `runtime/state_test.go:332` (347) | `TestGitTrackedLegacyDatabaseIsRefused` | Git-tracked legacy DB → `ErrRepositoryCarriedState`, no state created, files unchanged; untracked → legacy mode | T0-S1(4) — should not change |
| C1-13 | `runtime/state_test.go:355` | `TestGitProbeFailureRefusesLegacyDatabase` | a failing git probe refuses the legacy DB instead of adopting it | T0-S1(4) — should not change |
| C1-14 | `runtime/state_test.go:426` (428, 432) | `TestStateBoundToAnotherWorkspaceIsRefused` | copied external state, copied legacy state and explicit `RepoDir` all → `ErrStateBindingMismatch`; the copy is not rebound | T0-S1(3)(6) — should not change |
| C1-15 | `runtime/state_test.go:442` | `TestUnreadableBindingIsRefusedNotReplaced` | an unparsable binding is refused and not rewritten | T0-S1(3) — should not change |
| C1-16 | `runtime/state_test.go:465` | `TestUnwritableStateRootFailsOpen` | an unwritable state root fails Open; no fallback into the workspace | T0-S1 — should not change |
| C1-17 | `runtime/state_test.go:480` | `TestCancelledOpenLeavesNoState` | a cancelled Open leaves no database; the next Open starts clean | T0-S1 — should not change |
| C1-18 | `runtime/state_test.go:506` | `TestProjectConfigIsReadFromTheWorkspaceInBothModes` | project `mcp.json` is read from `<ws>/.lumencortex` in external and legacy modes | C-1 / T0-S1(5) — should not change |
| C1-19 | `runtime/state_test.go:567` (helper 538) | `TestConcurrentFirstOpenMakesOneStateAndBinding` | four processes × three rounds produce one state directory and one binding | T0-S1 failure path — should not change |
| C1-20 | `runtime/state_test.go:675` | `TestAgentFileToolsCannotReachTheEnginesState` | `write_file` through symlink, hard link and absolute path to the DB is refused in both modes; an ordinary file is written | T0-S1(7) — should not change |

### 3c. CLI, project configuration, schema fixtures (G1)

| ID | file:line | Test | Asserted | Clause |
|---|---|---|---|---|
| C1-21 | `cmd/lcx-go/db_test.go:36` | `stateHash` helper | hashes `lumencortex.db`, `-wal` and `backups/` under a `StateInfo` directory | fixture — should not change |
| C1-22 | `cmd/lcx-go/db_test.go:90` | `schemaVersion` helper | reads `user_version` from the state directory's DB | fixture — should not change |
| C1-23 | `cmd/lcx-go/db_test.go:170` (comment 151) | `TestDBCommandDispatchesBeforeOpen` | `db downgrade` on an empty workspace creates neither `<ws>/.lumencortex` nor external state | C-1 / T0-S1 — should not change |
| C1-24 | `internal/repository/exclude_test.go:67` (74) | `TestEnsureLocalExcludeThroughSymlinkedWorkspacePath` | `/.lumencortex/` is added to `.git/info/exclude` even when the workspace is reached through a symlink | C-1 (project config stays) — should not change |
| C1-25 | `internal/repository/exclude_test.go:89` (96) | `TestEnsureLocalExcludePlainPathStillWorks` | same exclude entry for a plain path | C-1 — should not change |
| C1-26 | `internal/mcp/registry_test.go:24` | `TestRegistryPersistsConfigWithoutAutostart` | config path is `<root>/.lumencortex/mcp.json` (passed explicitly) | C-1 / T0-S1(5) — should not change (fixture passes the path) |
| C1-27 | `internal/mcp/registry_test.go:189` | `TestLayeredRegistryProjectOverridesGlobalAndFallsBack` | the project file in `<root>/.lumencortex` overrides global entries | C-1 / T0-S1(5) — should not change |
| C1-28 | `internal/mcp/registry_test.go:304` (341, 378, 423) | `TestUntrustedProjectServerIsRefusedWithoutSpawning` (+ trust/withdraw siblings) | project server trust is decided from the workspace's project file | C-1 / T0-S1(5) — should not change |
| C1-29 | `internal/mcp/registry_test.go:551` (578) | `TestListingFieldsAreNotPersisted` (+ `TestProjectTrustIsIndependentOfTheRuntimeWorkspace`) | listing fields are not persisted; project trust does not depend on the runtime workspace | C-1 — should not change |
| C1-30 | `internal/toolset/mcp_test.go:76` | `TestMCPToolOverrideDecidesWhatAReadOnlyRunSees` | a project override in `<root>/.lumencortex/mcp.json` makes an unannotated tool read-only | C-1 / T0-S1(5) — should not change |
| C1-31 | `runtime/mcp_trust_test.go:30` | `writeEngineMCP` helper | project `mcp.json` is written under `<ws>/.lumencortex` | C-1 / T0-S1(5) — should not change (fixture) |
| C1-32 | `runtime/prompt_test.go:45` | `newPromptFixture` (used by `TestEffectivePrompt*`) | project skill is read from `<ws>/.lumencortex/skills` | C-1 / T0-S1(5) — should not change |
| C1-33 | `runtime/engine_test.go:12` | `TestEnginePersistsSessionAndStreamsEvents` | opens with explicit `RepoDir: <ws>/.lumencortex-test` | T0-S1(6) `RepoDir` retained — should not change |
| C1-34 | `runtime/schema_test.go:23` (71, 144) | `stateDirFor` helper; `TestOpenRefusesNewerSchemaWithoutWriting`; `TestOpenRefusesLegacyNodeDatabaseWithoutWriting` | refusals write nothing in the `StateInfo`/legacy state directory | T0-S1(3) — should not change |

### 3d. Path guard (G1)

| ID | file:line | Test | Asserted | Clause |
|---|---|---|---|---|
| C1-35 | `internal/toolset/guard_test.go:57` (58–62) | `newGuardFixture` | protected legacy set (`lumencortex.db*`, `db.lock`, `bind.lock`, `checkpoint.pending`, `runs/`, `backups/`, `legacy-state-*`) and in/out-of-workspace external state | T0-S1(7) — should not change (fixture) |
| C1-36 | `internal/toolset/guard_test.go:224` | `TestFileToolsCannotCreateInsideState` | no protected name/dir can be created through absolute, relative, `..` or symlinked-parent routes | T0-S1(7) — should not change |
| C1-37 | `internal/toolset/guard_test.go:264` | `TestOrdinaryWorkspaceFilesAreNotAffected` | project config and ordinary files named `lumencortex.db` stay writable; `list_dir .lumencortex` works | C-1 / T0-S1(5)(7) — should not change |
| C1-38 | `internal/toolset/guard_test.go:314` | `TestPathSwappedAfterResolveIsRefused` | a path swapped to state after resolution is refused by file identity | T0-S1(7) — should not change |
| C1-39 | `internal/toolset/guard_test.go:389` | `TestZeroProtectedChangesNothing` | without a `Protected` value the guard is off | T0-S1(7) — should not change |
| C1-40 | `internal/toolset/guard_test.go:454` | `TestCaseVariantCannotCreateState` | case variants of state paths are refused and create nothing | T0-S1(7) — should not change |

### 3e. Store-level and real-model fixtures (G1)

| ID | file:line | Test | Asserted | Clause |
|---|---|---|---|---|
| C1-41 | `internal/session/store_test.go:12`, `internal/session/agent_test.go:12`, `internal/session/metadata_test.go:17`, `internal/session/gap_test.go:19` | `TestSessionHistoryIsPagedFromSQLite`, `TestAgentPatchAndNextStepDoNotRewriteHistory`, metadata and gap fixtures | `Store` is opened at `t.TempDir()/.lumencortex` | fixture — should not change |
| C1-42 | `internal/session/migration_test.go:24` (29, 126, 130, 299, 304); `internal/session/downgrade_test.go:23` (29, 64, 123, 142, 205, 212, 270, 290, 304, 355, 375, 391, 447); `internal/session/binding_test.go:142` | legacy schema and downgrade fixtures | create the old schema and `lumencortex.db` in a directory named `.lumencortex` | fixture — should not change |
| C1-43 | `internal/realmodel/continuity_test.go:159` (272) | `readAll` / `unbindState` helpers | the continuity harness skips `.lumencortex` when copying a workspace and downgrades a copied history DB to 5 before deleting the binding | fixture — should not change |

### 3f. Desktop (D1n / D1)

| ID | file:line | Test | Asserted | Clause |
|---|---|---|---|---|
| C1-D1 | `internal/backend/config_test.go:775` | `TestOpenWorkspaceRefusesNewerSchemaWithCoreSentinel` | patches `user_version` at **`<workspace>/.lumencortex/lumencortex.db`** and expects `lcx.ErrSchemaTooNew` on reopen | **C-1 / T0-S1 — MUST CHANGE at the Core bump**: after the bump the DB is external, so the test must locate it through `StateInfo()`/the state root. Desktop is pinned to Core `v0.1.4` today (`go.mod`), so it passes now. |
| C1-D2 | `internal/backend/mcp_test.go:20` (57, 134) | MCP project tests | project `mcp.json` is read from `<ws>/.lumencortex` | C-1 / T0-S1(5) — should not change |
| C1-D3 | `internal/backend/tool_permissions_test.go:43` | tool-permission tests | `ToolPermissionsFilename` lives under `<ws>/.lumencortex` | C-1 / T0-S1(5) — should not change |
| C1-D4 | `frontend/src/lib/error-presentation.test.ts:7` | `error-presentation` cases | a `database is open in another process: /repo/.lumencortex` message maps to `errDatabaseInUse` | C-1 — should not change (the match is on the error text, the path is incidental; see OPEN-3) |

Adjacent Desktop hits, not rows: `frontend/src/components/settings/SettingsPanes.test.tsx:32,59` renders a fixture
database path; `internal/backend/worktree_test.go:17` is a comment about the `.lumencortex` exclude entry;
`.lumencortex-worktrees` and `.lumencortex/skills` hits are worktree/project paths.

## 4. Class 2 — cross-run fingerprint reuse (`Reused`)

Clause key: **DATA §3** (N2): a verdict only counts from a **closed episode** with an approved `specDigest`;
same-run reuse is allowed only for an in-run closed episode with no later mutation-capable tool; **cross-run reuse
defaults to a rerun**. C-6 defines the episode shape. The current `Satisfies`/`CheckVerdict`/`Decide` reuse is
fingerprint-only and has no run/episode identity, which is exactly what the contract may change.

| ID | file:line | Test | Asserted | Clause |
|---|---|---|---|---|
| C2-01 | `internal/evaluator/run_test.go:124` (129) | `TestDecideStoresEvidenceAndReusesOnlyAtTheSameFingerprint` | a pass at the same fingerprint is reused without running and appends no row | DATA §3 episode/same-run reuse — **NEEDS_SONNET** (whether the reuse now requires an in-run closed episode) |
| C2-02 | `internal/evaluator/run_test.go:134` | same test | an edit reruns the check and appends a row | DATA §3 — should not change (fingerprint change still invalidates) |
| C2-03 | `internal/evaluator/run_test.go:139` | same test | editing the check command invalidates its stored results | DATA §3 `specDigest` — should not change |
| C2-04 | `internal/evaluator/run_test.go:160` | `TestHumanAcceptanceHoldsUntilTheFingerprintChanges` | a human acceptance is reused at the current fingerprint without running | DATA §3 approval predicate — **NEEDS_SONNET** |
| C2-05 | `internal/evaluator/run_test.go:169` | same test | after an edit the acceptance no longer counts and the check runs | DATA §3 — should not change |
| C2-06 | `internal/evaluator/run_test.go:183` | `TestOutsideGitEveryDecisionRuns` | outside git there is no fingerprint, so nothing is reused | DATA §3 — should not change |
| C2-07 | `internal/evaluator/evaluator_test.go:66` (67) | `TestCheckVerdict` | a stored human/runtime pass at the current fingerprint decides without running | DATA §3 — **NEEDS_SONNET** |
| C2-08 | `internal/evaluator/evaluator_test.go:41` | `TestSatisfies` | only runtime/human passes at the current non-empty fingerprint satisfy; llm/model/`human:` do not | DATA §3 approval predicate — **NEEDS_SONNET** |
| C2-X1 | `internal/agent/auto_test.go:186` | `TestAutoDecision` | `ReusedAutoDecision` marks a prior auto decision `"reused":true` | cross-ref class 5 (C5-12); not fingerprint reuse — should not change |
| C2-X2 | `runtime/auto_effort_test.go:254` | `TestAutoEffortResumeReusesDecision` | the reused auto decision is visible in the step record | cross-ref class 5 (C5-09) — should not change |

## 5. Class 3 — journal content

Clause key: **C-7** + **DATA §6** journal whitelist projection. Projection happens at the single write point
`Store.Journal`; R1a adds no numbered migration, so old rows are not rewritten.

**Finding:** no test calls `Store.Journal` (`grep -rn --include='*_test.go' 'Journal' .` → 0 hits). The only
content-bearing assertion is the migration fixture snapshot; no test asserts `session.start`/`tool.shell`/
`subagent.event` payloads. C-7 therefore changes no existing assertion.

| ID | file:line | Test | Asserted | Clause |
|---|---|---|---|---|
| C3-01 | `internal/migrate/fixtures_test.go:111` | `TestUpgradeFixtures` (fixture) | the legacy fixture contains one `session.create` journal row | C-7 — should not change (fixture) |
| C3-02 | `internal/migrate/fixtures_test.go:162` (183) | `TestUpgradeFixtures` | journal `event|payload_json` rows are byte-identical before and after the upgrade | C-7 / DATA §6 — should not change (projection is write-time; no rewrite in R1a) |
| C3-03 | `internal/migrate/migrate_test.go:66` (67, 69) | `TestUpgradeFixtures` | migrated DB is WAL and has a `journal` table | C-7 — should not change |
| C3-04 | `internal/migrate/migrate_test.go:642` | `TestRefusalLeavesHotWALUntouched` | a journal row written into a hot WAL copy is crash residue and is not touched | C-7 — should not change |
| C3-05 | `internal/session/migration_test.go:68` (166, 336) | `TestWindowStartSeqGapRepair`, `TestGoalRowFollowsTrimSpace` (fixtures) | legacy schema fixtures declare the `journal` table | C-7 — should not change (fixture) |
| C3-06 | `internal/session/downgrade_test.go:64` | `TestDowngradeRoundTrip`, `TestDowngradeEveryBoundary` (fixture) | legacy schema fixture declares the `journal` table | C-7 — should not change (fixture) |
| C3-07 | `runtime/prompt_test.go:317` | `TestEffectivePromptWritesNothing` | building a prompt adds no row to `sessions`, `session_messages`, `agent_steps`, `task_states`, `journal` | C-7 — should not change |

Excluded hits: `_journal_mode` pragmas (`runtime/schema_test.go:145`, `internal/migrate/migrate_test.go:223`,
`internal/session/migration_test.go:29/130/304`, `downgrade_test.go:29/123/…`), SQLite `-journal` file names
(`internal/toolset/guard_test.go:224,455`), and comments.

## 6. Class 4 — parent checkpoint `goal`/`final`

Clause key: **C-7** + **DATA §6** bullet ② and the tail of the journal-projection bullet: the parent checkpoint
payload for `subagent.spawn`/`subagent.stopped` is to keep only `childSessionId/status/agent`; today
`runtime/subagents.go` (lines 122 and 360–367) writes `goal` and `final` on the **parent** session, and deleting the
child does not cascade.

**Finding:** no test asserts those fields. Only `reason`, `phase`, order, cascade and retention are asserted, so
the payload reduction changes no assertion. Desktop `WindowPane.tsx` renders only `reason`; no Desktop test covers
checkpoint JSON.

| ID | file:line | Test | Asserted | Clause |
|---|---|---|---|---|
| C4-01 | `runtime/subagents_test.go:288` | `TestSubagentPersistsRelationEventsAndCheckpoints` | the parent has `subagent.spawn` and `subagent.stopped` checkpoints (payload not asserted) | C-7 — should not change; payload fields are unasserted (OPEN-1) |
| C4-02 | `runtime/subagents_test.go:269` (assertion at 276) | same test | the subagent tree node carries the child's `Final` (a tree node, not a checkpoint) | should not change |
| C4-03 | `runtime/checkpoint_resume_test.go:104` | `TestInterruptedRunResumesFromPersistedSafeBoundaryWithoutToolReplay` | `agent.safe` checkpoints carry `phase: tool_result` and `step_complete` | should not change |
| C4-04 | `runtime/checkpoint_resume_test.go:150` | same test | an `agent.resume` checkpoint exists after the interrupted run is resumed | should not change |
| C4-05 | `internal/session/relations_test.go:56` (60, 67) | `TestRelationsAndCheckpointsPersistAndCascade` | checkpoint seqs and newest-first order; payload round-trips | should not change |
| C4-06 | `internal/session/relations_test.go:86` | same test | deleting the parent deletes its checkpoints | should not change |
| C4-07 | `internal/session/relations_test.go:125` (136) | `TestCheckpointRetentionIsBounded` | at most 200 checkpoints are retained, newest kept | should not change |

## 7. Class 5 — Auto judge ordering and `runtime/start_atomic_test.go`

Clause key: **DATA §5** auto-effort bullet + plan §5 **T1-S3** + C-2/C-5 + ADR amendment 8. The judge runs before
`BeginRun`; only a real judge call writes one envelope (`purpose=auto_effort`); `AutoFallbackNoModel` and
`ReusedAutoDecision` write none; a failed or cancelled `Start` must not touch `sessions`/`session_messages`
(`start_atomic_test.go` assertions are explicitly frozen).

| ID | file:line | Test | Asserted | Clause |
|---|---|---|---|---|
| C5-01 | `runtime/start_atomic_test.go:47` | `TestFailedStartTransactionLeavesNoTraceAndReleasesTheLock` | an injected status-update failure leaves session identity, transcript, status and cause exactly as before, calls no provider and releases the run lock | DATA §5 + T1-S3 ("`start_atomic_test.go` 不改断言") — should not change |
| C5-02 | `runtime/start_atomic_test.go:157` | `TestSuccessfulStartCommitsInputAndRunningBeforeTheLoop` | at the first provider call the status is `running`, the previous final/cause are cleared, the identity is the new model and the Input is the newest row | T1-S3 — should not change |
| C5-03 | `runtime/auto_effort_test.go:87` | `TestAutoEffortJudgeLevelReachesRequest` | the judge is called once, its request has no tools and two messages, the level reaches the main request, and step records carry no user prompt | DATA §5 (envelope `purpose=auto_effort`) — should not change |
| C5-04 | `runtime/auto_effort_test.go:125` | `TestAutoEffortHeuristicWithoutJudge` | no judge model → heuristic, `AutoFallbackNoModel` | T1-S3 (no envelope on this path) — should not change |
| C5-05 | `runtime/auto_effort_test.go:144` | `TestAutoEffortCeiling` | the ceiling caps the resolved level | should not change |
| C5-06 | `runtime/auto_effort_test.go:162` | `TestAutoEffortOverrideWins` | a concrete effort override resolves no auto decision | should not change |
| C5-07 | `runtime/auto_effort_test.go:186` | `TestAutoEffortUnavailable` | no supported effort produces `effort.auto_unavailable` and no reasoning parameter | should not change |
| C5-08 | `runtime/auto_effort_test.go:212` | `TestAutoEffortJudgeFailureFallsBack` | a judge error falls back to the heuristic with `fallbackReason` = the protocol class | should not change |
| C5-09 | `runtime/auto_effort_test.go:231` | `TestAutoEffortResumeReusesDecision` | a resume does not call the judge again, keeps the persisted level and records a `"reused":true` decision | T1-S3 (`ReusedAutoDecision` writes no envelope) — should not change |
| C5-10 | `runtime/auto_effort_test.go:265` | `TestAutoEffortConfigDefault` | `effort.default: auto` reaches the judge's level | should not change |
| C5-11 | `internal/agent/auto_test.go:79` | `TestClassifyWithJudge` | parse/fallback classes (`unparsable`, `auth`, `timeout`, cancellation) and secrets are not echoed | should not change |
| C5-12 | `internal/agent/auto_test.go:186` | `TestAutoDecision` | decision round-trip, `ReusedAutoDecision`, and `ReplayDecision` rejecting forged output/inputs | should not change |
| C5-13 | `runtime/auto_judge_test.go:54` (99) | `TestJudgeClassifies[OI]Compatible`, `TestJudgeClassifiesAnthropic` | the judge request carries no tools, no thinking, and reasoning effort `low` | should not change |

Excluded: `config/auto_test.go:16,32` (config defaults only, no ordering).

## 8. Class 6 — completion gate, AutoDone, required checks

Clause key: **DATA §3** (closed episodes, `specDigest`/`CheckSpecApproval`, unapproved checks never satisfy and
are not run by the gate; interactive turns end normally, non-interactive gates exit with a dedicated reason) and
**C-6** (episode row shape, legacy `open` rows never satisfy).

| ID | file:line | Test | Asserted | Clause |
|---|---|---|---|---|
| C6-01 | `internal/agent/loop_test.go:597` | `TestCompletionGateDecidesRequiredChecks` | only the required check is decided (twice), optional checks never; `completion_blocked` names the check, status, artifact and tail; the run completes after the pass | DATA §3 approval predicate + C-6 — **NEEDS_SONNET** |
| C6-02 | `internal/agent/loop_test.go:631` | `TestCheckDecisionFailuresEndTheRun` | cancellation → interrupted/cancelled; store error → failed/storage; workspace error → failed/config; a failed decision does not complete the unit | should not change |
| C6-03 | `internal/agent/loop_test.go:565` | `TestWorkUnitChecksAreDecidedByTheEvaluator` | unit checks are decided by the evaluator and a blocked final names the open unit | should not change |
| C6-04 | `internal/agent/taskstate_test.go:130` | `TestAutoDoneNeedsEvidenceAndNoObligation` | done latches only with a finishable workflow/evidence; pending application keeps it open; no workflow is no evidence; a cancelled unit is not acceptance | DATA §3 + C-6 — **NEEDS_SONNET** |
| C6-05 | `internal/taskstate/resolution_test.go:54` | `TestEvidenceRuleAcceptanceSources` | done sources and obligations (unsatisfied required check, pending application, running task); refs recorded; a dropped thread is never replaced | DATA §3 — **NEEDS_SONNET** |
| C6-06 | `internal/taskstate/workunit_test.go:65` | `TestWorkUnitCompletionGateAndDependencyOrder` | a unit completes only with required evidence and a passed verdict; dependencies gate the next unit | should not change |
| C6-07 | `internal/taskstate/workunit_test.go:60` | `TestPreEvaluatorVersionDecodesWithoutModelVerdicts` | a pre-M4.1 version decodes and its model-written verification neither gates nor accepts | should not change |
| C6-08 | `runtime/checks_test.go:66` | `TestHumanAcceptanceHoldsUntilTheFingerprintChanges` | an acceptance is a pass at the current fingerprint, the gate runs no check, the thread latches done with `check:status@eval:1`, and the acceptance stops counting after an edit | DATA §3 `CheckSpecApproval`/`specDigest` — **NEEDS_SONNET** |
| C6-09 | `runtime/checks_test.go:123` | `TestCompletionGateRunsRequiredChecksWithArtifacts` | a failing required check interrupts the run, stores log and diff artifacts, and appends `completion_blocked` | should not change (artifacts/row shape preserved; C-6 adds episode columns) |
| C6-10 | `runtime/checks_test.go:186` | `TestChecksRunInTheWorktreeAndApplyLatchesOnlyOnEvidence` | checks run in the session worktree; Apply discharges only pending application; done latches only with satisfied required checks at the current fingerprint | DATA §3 — **NEEDS_SONNET** |
| C6-11 | `internal/agent/interactive_test.go:41` | `TestInteractiveTurnEndsWithOpenUnits` | an interactive turn ends completed with open units and an unpassed required check, without checks or correction rows; the non-interactive run keeps the gate | DATA §3 / C-6 (unapproved checks end normally in interactive turns) — **NEEDS_SONNET** |
| C6-12 | `internal/agent/interactive_test.go:123` | `TestInteractiveRunKeepsTheWorkflowGate` | an active workflow keeps CanFinish, the tool boundary, the unit gate and human gates in interactive runs | should not change |
| C6-13 | `internal/agent/interactive_test.go:197` | `TestInteractiveTurnResolution` | an answer is not acceptance; a stored acceptance latches at the turn end; a user's done/dropped survives a question | DATA §3 — **NEEDS_SONNET** |
| C6-14 | `internal/agent/interactive_test.go:250` | `TestInteractiveTurnThatChangedTheWorkspaceDecidesChecks` | a turn that changed the workspace decides its required checks once at the end; failure keeps it open, pass latches | DATA §3 — **NEEDS_SONNET** |
| C6-15 | `runtime/interactive_test.go:146` | `TestInteractiveTurnThatEditsDecidesChecksWithTheEvaluator` | the real evaluator decides the required check at the turn end; the pass latches done; a following question runs no check | DATA §3 — **NEEDS_SONNET** |
| C6-16 | `runtime/interactive_test.go:124` | `TestSubagentRunsAreNeverInteractive` | a child with an open unit is held to the completion gate whatever the options say | should not change |
| C6-17 | `runtime/models_test.go:485` | `TestHiddenReasoningIsNeverPersisted` | a required check forces the gate to write `eval_results`/artifacts; the assertion is that no reasoning sentinel is at rest | should not change (assertion is about persistence, not gate semantics) |

Excluded hits: JSON-Schema `required` (`config/agents_test.go:94,185`, `config/schema_test.go:56,62,63`,
`internal/agent/schema_test.go:106,107,109,115`, `runtime/delegation_agents_test.go:104,107`), evaluator `Spec.Required`
fixtures (`internal/evaluator/run_test.go:90,201,230`), `bindingRequiredFromVersion`
(`internal/session/binding_test.go`), workflow human gates (`TestEveryExitPathStoresStatusAndCause` in
`internal/agent/exit_test.go`, `internal/workflow/workflow_test.go`), and
`TestOptionsProviderFactoryBuildsEveryRequiredClientBeforeTheStart`.

## 9. Class 7 — Reconcile

Clause key: **C-2** (run id is drawn after Reconcile), **ADR-0018 / DATA §7** (Reconcile rule extension: a leftover
`admitted` envelope becomes `unknown`, an `admitted` execution without post becomes `outcome_unknown` with
`lineage_complete=0`, terminal rows unchanged, idempotent, in the transaction of the transcript repair). T1 adds
rules; it does not change the existing transcript-gap rules, so the existing assertions should not change.

| ID | file:line | Test | Asserted | Clause |
|---|---|---|---|---|
| C7-01 | `internal/session/gap_test.go:71` | `TestReconcileClosesOnlyTheUnansweredCallsOfTheTailGap` | only unanswered calls of the tail gap are closed (2 rows), a second Reconcile inserts 0 | should not change |
| C7-02 | `internal/session/gap_test.go:101` | `TestReconcileLeavesANonTailGapUntouched` | a gap followed by another row is left alone (0 inserted) | should not change |
| C7-03 | `internal/session/gap_test.go:119` | `TestReconcileKeepsTheCauseOfANonRunningSession` | a non-running session keeps its status and cause | should not change |
| C7-04 | `internal/session/gap_test.go:139` | `TestReconcileMarksARunningSessionStaleRunRecovered` | a running session with a free lock becomes `stale_run_recovered` | should not change |
| C7-05 | `internal/session/gap_test.go:162` | `TestReconcileRunningSessionWithoutGapOnlyChangesStatus` | no gap: only the status changes, 0 rows inserted | should not change |
| C7-06 | `internal/session/gap_test.go:181` | `TestReconcileUnknownSession` | an unknown session returns `ErrNotFound` | should not change (also required by C7-09) |
| C7-07 | `internal/session/migration_test.go:250` | `TestReconcileInterruptedSession` | a dangling call on an already interrupted session is closed; status and error_json unchanged; second Reconcile 0 | should not change |
| C7-08 | `internal/session/provenance_test.go:660` | `TestReconcileSettlesProvenanceIdempotently` | admitted envelope → unknown (`restart_no_answer`), answered/failed untouched, crashed execution → `outcome_unknown`/incomplete, idempotent, other session untouched | ADR-0018 (the test pins the T1-S1 rule) — should not change |
| C7-09 | `internal/session/provenance_test.go:768` | `TestReconcileOfAMissingSessionStillFailsAndWritesNothing` | a missing session still fails and writes nothing | should not change |
| C7-10 | `internal/session/provenance_test.go:961` | `TestWriteMethodsDoNotSelfLockUnderConcurrentReads` | Reconcile does not self-lock with concurrent readers (ADR-0018 3.1 rule 4) | should not change |
| C7-11 | `internal/session/provenance_test.go:1034` | `TestLegacyRowsHaveNoProvenance` | Reconcile of a legacy session writes no provenance | should not change (DATA §7 legacy rows never gain trust) |
| C7-12 | `runtime/run_id_test.go:16` | `TestPrepareRunDrawsAFreshRunIDAfterReconcile` | each Start draws a fresh run id after Reconcile | C-2 — should not change |
| C7-13 | `runtime/run_id_test.go:63` | `TestPrepareRunOfAMissingSessionFailsBeforeAnyRun` | prepareRun of a missing session fails at Reconcile and builds no run | C-2 — should not change |
| C7-14 | `runtime/runlock_test.go:364` | `TestCancelDuringToolThenStartWithInputLeavesAPairedTranscript` | after cancel during a tool, Reconcile precedes the follow-up input and the transcript is paired | should not change |
| C7-15 | `runtime/runlock_test.go:391` | `TestOpenReconcilesARunningSessionWhoseLockIsFree` | Open reconciles a running session whose run lock is free | should not change |
| C7-16 | `runtime/runlock_process_unix_test.go:102` | `TestKillHolderMidToolThenResume` | after `kill -9` of the holder a fresh Open reconciles to `interrupted/stale_run_recovered`; resume sends a paired transcript | should not change (T1-S2/S4 add envelope/execution assertions in new tests) |
| C7-17 | `runtime/delegation_agents_test.go:207` | `TestSpawnRunsTheChosenAgentAndResumeKeepsIt` | a crashed running child is reconciled at the next Open and resume keeps its agent | should not change |

## 10. Excluded grep hits (out of class)

- `internal/migrate/{fixtures,lock,migrate,provenance}_test.go`: `lumencortex.db.vN.<ts>` backup **file names** and
  `lumencortex.db-wal`; not the state path or journal content.
- `internal/toolset/guard_test.go:224,455`: `lumencortex.db-journal` is a protected SQLite file name (class 1
  guard), not the `journal` table.
- `runtime/schema_test.go:145`, `internal/migrate/migrate_test.go:223`, `internal/session/{migration,downgrade}_test.go`:
  `_journal_mode` pragmas (SQLite journal mode, not the `journal` table).
- `internal/agent/notes_test.go:90`, `internal/session/provenance_test.go:493`: the word "reused" about IDs, not
  fingerprint reuse.
- Desktop `frontend/src/**` `.lumencortex-worktrees`, `.lumencortex/skills`, `.lumencortex/cognition.json`:
  worktree/project-config paths, not the state database.

## 11. Test-change ledger (baseline)

This slice changes **no test**. The ledger is this file; later slices must cite the row IDs above when they change
an assertion. The only row that is already known to require a change is **C1-D1** (Desktop, at the Core bump).
No assertion was modified to produce this inventory.

## 12. OPEN items and known limits

1. **OPEN-1 (class 4):** no test asserts parent-checkpoint `goal`/`final`; whether T1 must add a negative assertion
   that they are gone is a semantic decision for the T1 owner (C-7 says the payload keeps only
   `childSessionId/status/agent`). No guess was made.
2. **OPEN-2 (class 2):** `Satisfies`/`CheckVerdict`/`Decide` currently have no run or episode identity. Whether
   the reuse assertions (C2-01, C2-04, C2-07, C2-08) keep their shape or gain an episode/approval input is a
   design decision; marked `NEEDS_SONNET`.
3. **OPEN-3 (Desktop C1-D4):** the error-presentation test matches on the Core error text; if the message starts
   naming the external state path the fixture string still matches the `database is open in another process`
   prefix. Not verified by running Desktop tests (out of scope: read-only, tests not run).
4. **OPEN-4:** counts in §1 are from the worktree at `4c20959`; a later merge changes them. Re-run G1–G8/D1 before
   citing a count.
5. **Not run:** no Core or Desktop test, build, vet or lint was executed for this slice; every claim about an
   assertion is a reading of the source, not an observed pass. Class 6 `NEEDS_SONNET` rows are precisely the ones
   whose observable behaviour cannot be settled by reading alone.
