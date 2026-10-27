# Pre-registration: LumenCortex on the 13 SpecPi "widened" Terminal-Bench 2.0 tasks

Registered 2026-10-26, before any counted trial of this batch. Supersedes the A/A+/A+fix/OpenCode
plan in `PREREGISTRATION_TB2.md` for new runs (that file stays as the record of the earlier smoke and
the partial `jobs/tb2-afix` job, which is not counted here). Authorisation: O-6
(`LumenCortex-Cognitive-Design-20261023/OWNER_DECISIONS_R2_O6_20261026.md`): real-model runs of
LumenCortex Core on `local/deepseek-v4.1-flash`, arm A+ = G-R1a SHA `4968ea8`, isolated config,
public/synthetic tasks, usage recorded. Only LumenCortex runs; the other harnesses are taken from
public data (`bench/public/PUBLIC_BASELINES.md`) and are **not** re-run.

## Question

How does LumenCortex Core (A+) compare per task with the published Pi, Oh My Pi, OpenCode,
SpecPi, Claude Code and DeepSeek Harness results on the same 13 tasks and the same model? This is a
historical comparison, not a same-environment A/B: conditions that differ are listed below and in
`PUBLIC_BASELINES.md` §5.

## Arm and binary

| Arm | Core revision | Binary (linux/amd64, static) | SHA-256 |
|---|---|---|---|
| LC A+ | `cog/r1a@4968ea8` (G-R1a PASS) | `build/bin/lcx-go-Aplus` | `c33ff097cf9449a6b7c6195ee46c57045ab81b2a5e1ddc5609fe8f24c4b03435` |

Adapter `adapters/lcx_agent.py:LcxAplus` (unchanged from `PREREGISTRATION_TB2.md`): global config
under `XDG_CONFIG_HOME`, state under `XDG_STATE_HOME` (nothing written into the task workspace),
`LCX_POLICY=full`, `LCX_MAX_STEPS=150`, context limit 600K, output 32,768, provider timeout 3600 s /
idle 600 s. No `reasoning_effort` is sent (upstream default); no prompt, tool or skill is specific to
Terminal-Bench or to any task.

## Tasks and attempts

`tb2-widened13.txt`: cobol-modernization, configure-git-webserver, extract-elf, fix-git,
hf-model-inference, multi-source-data-merger, prove-plus-comm, pytorch-model-cli, raman-fitting,
sanitize-git-repo, sqlite-with-gcov, tune-mjcf, vulnerable-secret — the SpecPi widened slice.
Dataset `terminal-bench@2.0` as downloaded under `tb2/terminal-bench` (tasks and verifiers
unmodified). 3 attempts per task (`-k 3`), one sitting, concurrency 1 (`-n 1`: the model gateway
shares the 3.8 GiB Docker VM). Harbor 0.25.0, `--timeout-multiplier 2`.

## Metrics

- **Primary:** solved = verifier reward 1.0, per attempt, pooled per task (SpecPi's metric). A trial
  whose agent hit the timeout still counts by its verifier reward, as Harbor and SpecPi score it.
- **Secondary (timeout sensitivity):** solved with agent execution ≤ 900 s, a conservative proxy for
  the default multiplier that SpecPi and the official leaderboard use.
- Per trial: agent wall time, model requests, prompt / cached / completion tokens (metering proxy),
  tool calls (Core `session_messages`), steps, finish status, exception type.
- Failure class per non-solved trial: `INFRA` (gateway non-200 that ended the run, Docker/Harbor
  error, operator cancel), `ENV` (task environment broken under emulation — confirmed by an oracle
  run failing), `TIMEOUT`, `MODEL` (wrong or incomplete solution with a clean run), `HARNESS`
  (Core error, tool failure attributable to Core).

## Rules

- INFRA trials are re-run once and both are kept; the re-run counts. Everything else counts as is.
- No retuning between attempts; no change to tasks, verifiers, adapter or binary during the batch.
  A Core defect found here becomes a backlog item, not a mid-batch fix.
- Oracle runs (no model) for every task not already oracle-verified under emulation, before the
  batch; a task whose oracle fails is reported as `ENV` and excluded from the comparison.
- Every trial directory, Core state database and proxy log is kept.

## Conditions that differ from the public baseline (disclosed, not corrected)

Host arch (macOS arm64, amd64 images emulated; SpecPi: Docker, likely x86, unpublished), endpoint
(local sub2api gateway vs SpecPi's unpublished endpoint), reasoning effort (not sent vs
unpublished), timeout multiplier (2 vs default), concurrency (1 vs 6), attempts (3 in one sitting vs
pooled sittings), harness versions (OpenCode/OMP unpublished). Wall times are not comparable across
sources; SpecPi publishes none.
