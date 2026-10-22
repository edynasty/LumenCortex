# Agent presets and child prompts: real-model validation

- **Date:** 2026-10-22
- **Status:** validated; one prompt fix (thread-historian), re-run and passing
- **Baseline:** Core `d2eaa01` (HEAD at validation time; the presets and prompts were already committed)
- **Model:** `local/deepseek-v4.1-flash` (the only model authorised for real-model probes)
- **Scope:** `explorePrompt` (`runtime/subagents.go`), `workerPrompt` (`runtime/tasks.go`), and the four
  presets `config/presets/{reviewer,security-reviewer,sonic,thread-historian}.md` as saved into a
  workspace catalog.
- **Non-goals:** no change to the runtime besides one preset body; no new built-in agent ids; no
  model or provider changes.

## Method

The scenarios run in `internal/realmodel/presets_test.go` (build tag `realmodel`), following the
conventions of the existing real-model probes: the user's real global configuration is read only for
the provider entry and the credential store, a private `XDG_CONFIG_HOME` is written with that entry,
the temporary repository is trusted, and the presets are saved into the repository's
`lumencortex.json` as ordinary agents (the shape the editor's "add from preset" writes).

Every run is bounded (`MaxSteps` 8–12, effort `low`). Evidence is captured from the engine's event
stream: every `tool.start`/`tool.end` (name, arguments, outcome, session) and every `llm.request`
(candidate index, model ref, offered tools), plus the child tree, final answers, and the worktree's
`git status --porcelain`. The harness appends one JSON object per run to `LCX_PRESET_OUT`.

Reproduce:

```bash
LCX_REAL_MODEL=local/deepseek-v4.1-flash LCX_PRESET_OUT=/tmp/presets.jsonl \
  go test -tags realmodel -count=1 -run RealModelAgentPresets -v -timeout 40m ./internal/realmodel
```

### Environment note

The main checkout was being refactored concurrently (plugin platform: `runtime/engine.go`,
`runtime/plugins*`, OMP import moved to `runtime/session_import.go`). At validation time
`go test ./runtime` did not compile because of that in-progress work (`runtime/plugins_test.go`
referenced types that did not yet exist), so the full real-model suite was run in a clean copy of
the baseline:

```bash
git archive HEAD | tar -x -C /tmp/lcx-core-validated
cp internal/realmodel/presets_test.go /tmp/lcx-core-validated/internal/realmodel/
cp config/presets/thread-historian.md /tmp/lcx-core-validated/config/presets/
```

The copy contains only committed baseline code plus the two files this task adds/changes. The
report therefore describes runs on the baseline, not on the coordinator's uncommitted plugin work.

## Results

| # | Scenario | Agent | Model requests | Tool calls | Result |
|---|---|---|---|---|---|
| 1 | Reviewer on a seeded bug + decoy | `reviewer` | 5 | 10 | pass |
| 2 | Security review, traversal + SQL injection | `security-reviewer` | 8 | 15 | pass |
| 3 | Mechanical rename across 3 files | `sonic` (via `start_task`) | 11 | 16 (child 10) | pass |
| 4 | Historical thread question | `thread-historian` (via `spawn_subagent`) | 6 (child 3) | 4 (child 2) | pass after prompt fix |
| 5 | Fix a swapped-bounds bug | `worker` (via `start_task`) | 11 (child 7) | 15 (child 10) | pass |
| 6 | Read-only investigation + handoff | `explore` (via `spawn_subagent`) | 9 (child 6) | 19 (child 17) | pass |
| 7 | Read-only ceiling vs. an edit request | `reviewer`, host policy `full` | 6 | 9 | pass |
| 8 | Model-list fallback | `fallback-probe` | 2 | 0 | pass |

All eight subtests passed in one run:
`--- PASS: TestRealModelAgentPresets (199.14s)`.

## Scenario evidence

### 1. Reviewer

Fixture: a committed Go module; the uncommitted change replaces `Mul`'s `a * b` with `a + b` (a
real bug) and rewords the `Add` doc comment (the decoy). Prompt: review the current uncommitted
change, do not edit.

- Tools: `git_status`, `git_diff`, `read_file`, `list_dir`, `search_text`, `search_text`,
  `find_files`, `read_file`, `read_file`, `read_file` (10 calls, 5 requests, completed).
- Final: `Verdict: incorrect`, confidence 0.98, one finding:
  `Priority: P1 · Confidence: 0.98 · Location: calc/calc.go:6-7`, explaining that `Mul` now returns
  the sum and fails `TestMul`.
- The decoy was not reported: exactly one `Location:` line. In this run it was simply absent from
  the output; an earlier run of the same scenario explicitly called it harmless ("The `Add` comment
  rewording is harmless"). No mutating tool was called.

An earlier run of the same scenario reported the same bug as `P0` at `calc/calc.go:7-8`. Both are
valid under the preset's priority definitions; the difference is model judgement, not a format
failure.

### 2. Security Reviewer

Fixture: one Go file with `filepath.Join("/srv/files", r.URL.Query().Get("file"))` passed to
`http.ServeFile` (traversal) and a string-concatenated SQL query (injection).

- Tools: `list_dir` ×3, `git_status`, `read_file` ×4, `find_files` ×4, `search_text` ×4,
  `note_write` (15 calls, 8 requests, completed). `note_write` is a Task State note, not a file
  write; the read-only policy allows it.
- Findings: path traversal (`Severity: critical`, `CWE-22, CWE-73, CWE-306`) and SQL injection
  (`Severity: high`, `CWE-89`, with an explicit reachability caveat because `db` is never
  assigned). An earlier run of the same fixture also reported the never-initialised `db` as a
  low-severity nil-dereference (`CWE-476`).
- Every finding has `Locations:`, `Evidence:` with an excerpt, and `Remediation:`; the output also
  contains `Reviewed paths`, `Deferred`, and `Coverage summary`.
- No write tool was offered or called.

### 3. Sonic

Fixture: `Count` in `counter/counter.go` with call sites in `counter/counter_test.go` and
`main.go`. The parent was asked to start an execution task with the sonic agent, naming the three
files and the exact rename.

- Parent: 6 calls ending in `start_task` (agent `sonic`); it inspected the files first and pinned
  the exact lines in the objective.
- Child (`execution`, agent `sonic`, completed): `read_file` ×3, `apply_patch`, `search_text` ×3,
  `git_diff`, `work_unit_update` ×2 (10 calls).
- Worktree `git status --porcelain`: exactly
  `M counter/counter.go`, `M counter/counter_test.go`, `M main.go` — no extra files.
- The test function name `TestCount` and the format string `"Count=%d"` were left alone, matching
  "rename the function and its calls, change nothing else". Verification was `git_diff` after the
  edit; the final says the build/test half could not be run (workspace policy has no shell), which
  is accurate.

One earlier run with `MaxSteps=8` was `interrupted` after 8 steps at `work_unit_update` (the bound,
not a prompt defect). Re-run with `MaxSteps=12`: completed and passed.

### 4. Thread Historian (prompt fix)

First attempt: import the OMP fixture
(`internal/ompsession/testdata/sessions/-fixture-workspace/…0001.jsonl`) with the Engine's OMP import
(the baseline API was `ImportOMPSession`; the working tree now exposes
`ImportSession(ctx, "omp", root, path)`), then ask the historian what was wrong with `src/parse.go` and what the
test run reported. The imported thread has no notes, tasks, or final answer, and `read_thread`
returns a thread view, not raw messages. The historian correctly refused to invent an answer and
named the gap, but spent 14 calls (`read_thread` ×4, `recall` ×2, `find_files` ×2, `search_text` ×2,
`list_dir` ×3, `note_write`) searching the repository for thread content. A second pre-fix run
against a real prior thread with a final answer spent 8 calls (`read_thread` ×2, `search_text` ×4,
`list_dir`, `recall`) and cited the goal field as "Objective / user turn (order 1, the thread's goal
field)" — inventing the message context the preset asked for. That is a prompt/tool-contract
mismatch: `read_thread` returns a thread view (goal, status, resolution, notes, tasks, children,
final answer), never messages, so "role, order, quoted line" cannot be honoured.

Prompt fix (`config/presets/thread-historian.md`):

```diff
-- Use `recall` to find the thread and `read_thread` to read it. Never modify files, run shell commands or start work.
-- Answer from the thread's evidence and cite the thread id and the message context (role, order, quoted line) for each claim.
+- Use `recall` to find the thread and `read_thread` to read it; the view holds its goal, status,
+  resolution, notes, tasks, open units, children and final answer, not the raw messages. Never
+  modify files, run shell commands or start work, and do not search the repository for what the
+  thread said.
+- Answer from that view and cite the thread id plus the field each claim comes from (goal, final
+  answer, note, resolution, task).
```

After the fix, the scenario creates the prior thread with a real short run whose final answer
records two facts (`staging host is stg-7.internal; token variable is LCX_STAGING_TOKEN`), then
spawns the historian by id.

- Child (`subagent`, agent `thread-historian`, completed): `read_thread` and `recall` (2 calls,
  3 requests).
- Final: answers both facts, cites `session_9ff9424f062e94ce51b1ba73`, labels the evidence as the
  `goal` field and the `final answer` field, and states that the view has no raw message log. No
  repository search, no write tool.
- Parent: `spawn_subagent`, `read_thread`, `wait_subagent`, `recall` (4 calls).

The launch's read-only handoff (`explorePrompt`) still adds `Summary`/`Files`/`Architecture`
sections; the historian filled them honestly ("Files: none", "Architecture: not applicable")
instead of inventing content.

### 5. Worker

Fixture: `Clamp` returns the opposite bound in both branches. The parent was asked to start an
execution task with the worker agent, to read first, verify by reading back, and note the root
cause.

- Child (`execution`, agent `worker`, completed): `read_file`, `work_unit_list`,
  `replace_in_file`, `read_file` ×2, `git_diff`, `git_status`, `note_write`,
  `work_unit_update` ×2 (10 calls).
- Inspect-before-edit: the first two calls inspect (`read_file`, `work_unit_list`); the edit follows.
- Verification after the edit: `read_file` and `git_diff`/`git_status`.
- Worktree `git status --porcelain`: `M clamp/clamp.go` only; the final quotes the corrected
  function and the root cause.
- `note_write` was used in this run. In a second run of the same objective the worker omitted
  `note_write` but still put the root cause in its final answer. The prompt's clause is conditional
  ("what later steps need"); a single execution task has no later step, so both behaviours are
  within the stated contract. This is recorded as variance, not a defect.

**Workflow Contract:** not exercised. No workflow was active, and `executionOptions` does not
forward the parent's `Workflow` to the child, so there was no current action, tool boundary, route,
or gate to obey. Work units were seeded and updated as expected.

### 6. Explore

Fixture: `settings.Load` with three call sites. The parent was asked to delegate the search to a
read-only child and repeat its handoff.

- Child (`subagent`, agent `explore`, completed): 17 calls, all read tools
  (`search_text`, `find_files`, `list_dir`, `read_file`, `git_status`); no write or shell tool.
- Handoff: `Summary`, `Files` (`path:start-end` plus contents), `Architecture`, with all three call
  sites (`worker/worker.go:7`, `worker/worker.go:8`, `server/server.go:6`) and the definition.
- In an earlier run of the same fixture the child found that `search_text` with the regex
  `\.Load\(` returned nothing while identifier searches found the call sites, and switched to
  literal identifier searches — the prompt's "if a search is empty, try at least one alternate
  strategy" clause was followed.

### 7. Read-only ceiling

The host granted `Policy: full`; the `reviewer` preset caps policy at read-only. The prompt asked
it to change `Mul` to addition.

- Offered tools in every `llm.request` include `read_file`, `search_text`, `find_files`, `list_dir`,
  `git_status`, `git_diff`, `note_write` — and none of `write_file`, `replace_in_file`,
  `apply_patch`, `shell`.
- 9 calls, all read-only; the file was byte-identical before and after.
- Final: `Verdict: correct` (there is no change to review) and, for the pending objective, "I did
  not apply it because this run is read-only". The agent ceiling held even though the host granted
  full authority.

### 8. Model-list fallback

An agent with `models: [bogus/b, local/deepseek-v4.1-flash]`, where `bogus` is a workspace provider
whose base URL is `http://127.0.0.1:1/v1` (connection refused, a transport error classified as
availability). The second candidate is the authorised real model.

- Requests: step 1 candidate 0 `bogus/b` (fails), then step 1 candidate 1
  `local/deepseek-v4.1-flash`; final `FALLBACK-OK`. Two requests, no tool calls.

Note on "invalid": a reference outside the catalog is not a fallback case. `planModels` refuses the
Start up front for a list entry it cannot resolve ("A list reference outside the catalog fails the
Start"), and a missing credential also fails before any request. Availability fallback is the
designed path, so the probe uses a catalog model that is unavailable at request time.

## Prompt changes

Only one preset body changed: `config/presets/thread-historian.md` (2 lines, 655 → 838 bytes), shown
above. The body remains under the 16 KiB limit; all four preset descriptions are unchanged and
under 280 bytes. `config/presets_test.go` still passes, including the catalog-validity and
fresh-copy tests.

The built-in `explorePrompt` and `workerPrompt` were **not** changed: the scenarios found no
tool-name leakage, format failure, or scope creep that justified a change.

## Verification actually run

In the clean baseline copy (baseline + harness + preset fix):

```text
go test -count=1 ./config ./runtime          ok config 1.465s; ok runtime 63.421s
go vet -tags realmodel ./internal/realmodel ./config   pass
LCX_REAL_MODEL=local/deepseek-v4.1-flash LCX_PRESET_OUT=… \
  go test -tags realmodel -count=1 -run RealModelAgentPresets -v -timeout 40m ./internal/realmodel
                                             PASS (199.14s; all 8 subtests)
```

CLI sanity check on the baseline tree (the CLI's own configuration path, not a preset scenario):

```text
go build -o /tmp/lcx-go ./cmd/lcx-go          ok
XDG_CONFIG_HOME=<private config trusting the temp repo> LCX_MAX_STEPS=2 LCX_EFFORT=low \
  /tmp/lcx-go agent "Reply with exactly OK and nothing else. Do not call tools."
                                             completed, final "OK", 1 request, 1.5s
```

In the main checkout, once the concurrent plugin-platform refactor settled:

```text
go build ./...                               ok
go test -count=1 ./config ./runtime          ok config 1.640s; ok runtime 66.480s
go test -count=1 -run 'TestAgentPresets|TestResolveAgentPresets' -v ./config   PASS
go build ./runtime                           pass
```

Intermediate attempts against the same checkout failed to compile `runtime/plugins_test.go` and
`runtime/session_import_test.go` while that refactor was mid-flight (`undefined PluginManifest`,
`newPluginRegistry` arity, `undefined ompProvenance`, `Engine.SubagentTree` undefined). Those
failures were in files this task does not touch; the final run above is the required check.

## Environmental failures and limitations

- The main checkout was not buildable as a whole during validation (`cmd/lcx-go` lost the OMP
  methods while `runtime/omp_import.go` was being moved). The suite was therefore run in a clean
  `git archive HEAD` copy. No result here depends on the coordinator's uncommitted changes.
- One sonic run was cut off by the 8-step bound (`status: interrupted`, empty final). Re-running
  with 12 steps completed; this is a harness bound, not a prompt defect.
- The OMP-fixture historian run could not answer because the imported fixture thread has no final
  answer or notes and `read_thread` does not expose messages. That run is reported as the
  before-fix observation; the after-fix scenario uses a real prior thread.
- The first reviewer run failed one assertion because the test expected `Verdict: incorrect`
  without the backticks the preset template itself uses (`**Verdict**: \`incorrect\``). The
  assertion was corrected; the preset was not at fault.
- The model was reached through the user's local provider at `localhost:8080`; no other model or
  network provider was used.

## Residual risks

- **One model, one sample per scenario.** The probe validates behaviour on
  `local/deepseek-v4.1-flash` only. Priority choice (P0 vs P1) and `note_write` use varied between
  runs; both stayed within the prompt's stated rules, but a single run is not a statistical claim.
- **Parent nondeterminism in delegation.** Worker, sonic, explore and thread-historian runs depend
  on the parent choosing the named agent and writing a complete objective. All four did in these
  runs; a parent that delegates vaguely is not covered here.
- **Workflow Contract untested.** Execution children do not inherit a workflow; that clause of
  `workerPrompt` remains unexercised by this validation.
- **Read-only ceiling is enforced twice** (tool registration and `Execute` denial). The probe
  observed the registration side (no write tool offered) and no edit; it did not force a denied
  call to a registered tool.
- **Fallback** was validated for availability (transport error). Capacity failover and credential
  refusal were not re-tested here; they have unit coverage in `runtime/models_test.go`.
- **Evidence location:** `/tmp/lcx-presets/evidence-final3.jsonl` (the final full run),
  `/tmp/lcx-presets/evidence-delegation.jsonl` (the worktree rerun) and
  `/tmp/lcx-presets/evidence.jsonl` (earlier runs) are temporary and not part of the repository.
  The harness reproduces them with `LCX_PRESET_OUT`.

## Files changed

- `config/presets/thread-historian.md` — the citation/tool-contract fix above.
- `internal/realmodel/presets_test.go` (new) — the real-model harness for these scenarios.
- `docs/reports/agent-presets-validation-2026-10-22.md` (this report).
