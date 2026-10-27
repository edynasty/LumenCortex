# Baseline B — SpecPi's six-harness TB2 run, audited at the harness layer

Research + design only. **Nothing in this file was executed**: no Harbor run, no benchmark, no
model request. Section 5 is a recipe marked **NOT RUN**; every command in it is unverified by execution.

Companion, not replacement, for `bench/public/PUBLIC_BASELINES.md` (here **PB**): PB §1–§4 give the
published numbers, §5 the condition differences, §6 what is unpublished. This file answers a
different question: **how was each arm actually run, and what would an LC run sharing those
conditions have to hold fixed?** Numbers are reused by reference to PB; nothing numeric is
restated unless it is needed to answer a harness-layer question.

---

## 0. Evidence base, and its limits

### 0.1 Citation shorthand

| Prefix | Means |
|---|---|
| `SPR:` | `bench/public/raw/specpi-repo/<file>` — mirror of `TannerMidd/SpecPi@af36b0a1d48e7f706d40e30413e3150b067f45e6` (v0.37.1), hashes in `bench/public/raw/MANIFEST.csv` |
| `SBR:` | `bench/baselines/raw/specpi/<file>` — files fetched for this audit (SpecPi files at the same commit; one OMP doc from `can1357/oh-my-pi@main`), hashes in `bench/baselines/raw/specpi/MANIFEST.csv` |
| `HV:` | `/Users/tangxingpeng/.local/share/uv/tools/harbor/lib/python3.12/site-packages/harbor/` — Harbor **0.25.0**, the version this repo runs (`bench/HARBOR_SETUP.md:12-14`) |
| `SITE:` | `https://tannermidd.github.io/SpecPi/evaluations/`, byte-identical to `SPR:site__evaluations__index.html` (same sha256, PB §1) |

### 0.2 The single most important structural fact

**The SpecPi repository contains no TB2 launcher.** The tree at `af36b0a1` has exactly two
Terminal-Bench files (`SPR:scripts_tb2-metrics.mjs`, `SPR:scripts_tb2-site.mjs`) and no Harbor job
config, no launch script, no container definition and no agent adapter. Verified by enumerating all
5,380 paths in `SPR:git-tree-main.json` and filtering on `harbor|tb2|terminal|adapter|agent`
(only those two plus task fixtures and the site page matched).

Consequences that govern everything below:

1. The **generator** (`scripts/tb2-metrics.mjs`) is published and authoritative for scoring,
   pooling, exclusions and error handling — it reads Harbor `result.json` files.
2. The **procedure** that produced those `result.json` files is not published. Run directories are
   named (`tb2-pair-20260921-131712/pi`, `tb2-omp-widen-20260921-184339`, …,
   `SPR:scripts_tb2-metrics.mjs:62-105`) but the runs themselves are deliberately absent
   (`SPR:scripts_tb2-metrics.mjs:7-10`).
3. Therefore the per-harness *invocation* (adapter, flags, kwargs) is only partly recoverable.
   Where this file says "Harbor built-in `pi`", the evidence is the generator's own description of
   Harbor's agent plus Harbor 0.25.0's source; where it says "adapter not published", nothing in the
   repo or in Harbor 0.25.0 supports a built-in.

### 0.3 What each source can and cannot prove

| Source | Proves | Cannot prove |
|---|---|---|
| `SPR:scripts_tb2-metrics.mjs` | Model id, sittings, trial counts, scoring rule, error exclusion, dropped task, token post-processing, timeout note, concurrency note | How each agent was launched; versions except DSH/Pi |
| `SITE:` §06 + JSON | Six harness labels, slices, versions of Pi/SpecPi, "one endpoint", "Harbor on Docker" | Endpoint identity, Harbor version, kwargs, limits |
| `SPR:CHANGELOG.md` | Pi 0.87.0 reported by every trial (`:89`), OMP 18.2.5 (`:76`), prompt sizes (`:71`), interactive-only tools (`:65`) | TB2 run configs |
| `SBR:site_research_context-measurement.json` | Exact tool list, tool-schema chars, instruction chars per harness, **at SpecPi 0.34.0 / Pi 0.84.4 / OpenCode 1.18.31 / OMP 18.2.5 / DSH 0.1.5-rc.2**, measured 2026-09-24 | The TB2 builds (Pi 0.87.x, SpecPi 0.33.0); [CC] row absent |
| `SBR:scripts_measure-context.mjs` | The exact CLI shape each foreign harness needs (RPC, `opencode run`, `dsh --profile headless`) | Anything about Harbor |
| `SPR:scripts_eval-harnesses.mjs`, `SPR:evals_README.md` | How SpecPi installs itself, which packages, `yoloMode`, tool withdrawal, harness launch mechanics **in the tier suite** | TB2 invocation — different runner, same author |
| `SBR:evals_runs_jev-*.json` | Per-trial IDs and pass flags for the **pi and jev arms only**, 577 trials, 20 task names | Tokens, other arms, wall time |
| `HV:agents/installed/*` | What Harbor 0.25.0's built-in agents install and how they invoke their CLI | What SpecPi ran in September |

A note on the replay artifacts (`SBR:evals_runs_jev-progress-replay.json`, generated
2026-09-23T01:09Z): they carry one record per trial, e.g.
`tb2-pair-20260921-131712/pi/2026-09-21__13-17-18/crack-7z-hash__58xi33e` with `pass: true`. They
cover only the Pi and Jev arms of the 21–22 Sep sittings, but they do two things PB could not:
they confirm the per-sitting Pi counts (30/39, 22/39, 21/39, 30/39, 32/39, 27/39) trial by trial,
and they name **all 20 tasks** (13 widened + 7 calibration): the calibration seven are
`count-dataset-tokens`, `crack-7z-hash`, `git-leak-recovery`, `kv-store-grpc`,
`nginx-request-logging`, `openssl-selfsigned-cert`, `polyglot-c-py`. PB §6 said those names were
unpublished; they are recoverable from this file, which is a correction to PB, not a contradiction.

---

## 1. The seven questions, per harness

Each dossier answers the brief's seven points in order. `U` = unknown, with where it was checked.

### 1.1 Pi (base) — `pi`

1. **Agent / install / pin.** Harbor built-in `pi` (`HV:models/agent/name.py:38`) — **[INFERENCE]**,
   since no adapter is published and the run directory (`.../pi`) and version reporting match that
   agent; nothing in the repo proves it. Harbor installs
   the npm package `@earendil-works/pi-coding-agent` (`HV:agents/installed/pi.py:45`), pinning
   `@<version>` only when `--ak version=` is passed, otherwise `@latest`
   (`HV:agents/installed/pi.py:227-243`). Pi's own trials reported **0.87.0 and 0.87.1** —
   `piVersions` in the site JSON; `SPR:CHANGELOG.md:89` says "every trial reported 0.87.0" at the time of
   writing, so both builds were in use somewhere in the series; when 0.87.1 appeared, and on which
   arm, is not published.
2. **Model / endpoint / wire.** `deepseek-v4.1-flash` (`SPR:scripts_tb2-metrics.mjs:47`), "one
   endpoint" (`SITE:` §06) whose identity is not published. The repo's tier-suite env documents one
   [OI]-compatible base for every harness, `https://opencode.ai/zen/go/v1/chat/completions`, with
   `EVAL_FORWARD_WIRE` empty = chat-completions (`SPR:evals_.env.example:10-33`); the TB2 run
   configs are not in the repo, so the TB2 wire is **U**. No provider override published.
3. **Prompt / tools.** Stock Pi: **4 tools** — `bash`, `edit`, `read`, `write` — 2,901 chars of tool
   schema plus 2,620 chars of instructions, 5,521 chars first request (`SBR:site_research_context-measurement.json`
   `Pi (stock)`). Pi's own system prompt, no AGENTS layer.
4. **Limits / timeouts / effort.** Context cap, max turns, temperature and thinking level: **U**
   (not in the page, generator, README or CHANGELOG). Harbor's `pi` agent does expose
   `max_turns` and `reasoning_effort` kwargs (`HV:agents/installed/pi.py:73-92`), so their absence
   from the publication is an omission, not an impossibility. Timeout multiplier: Harbor default
   (1.0) for the widened sittings; setup 3× default only for the 24 Sep git pair
   (`SPR:scripts_tb2-metrics.mjs:91-93`).
5. **Trials / pooling / concurrency.** 3 attempts per task per sitting; Pi contributed **five
   counted sittings** (s1, s3, s5, s7, s8) = 15 attempts/task, 195 attempts on the widened slice
   (`SPR:scripts_tb2-metrics.mjs:62-89`; PB §1.3). Sittings s4 (21/39) and s6 (32/39) are excluded
   by name (`:112-116`). Concurrency 6 per launch (`:197`).
6. **Scoring.** `reward >= 1` (`:360`); a trial with no numeric reward is pushed to `errors` and
   excluded from the denominator (`:266-272`); `pytorch-model-recovery` dropped entirely (`:189`).
   Tokens: `agent_result.n_input_tokens` / `n_output_tokens`, averaged per attempt (`:359-390`).
7. **Reproducible?** **No.** No job config, no endpoint, no adapter, no per-trial result data, no
   dataset digest, no Harbor version (PB §6).

### 1.2 SpecPi 0.33.0 — `specpi`

1. **Agent / install / pin.** No Harbor adapter is published. In the tier suite SpecPi is installed
   by running its own installer against a pinned Pi CLI: `specpi install --yes
   --skip-browser-install` with `SPECPI_PI` pointing at the Pi binary
   (`SPR:scripts_eval-harnesses.mjs:1020-1030`), into a disposable agent dir that is then copied per
   attempt. The installer pins an exact package set; the 0.34.0 measurement lists eight
   (`SBR:site_research_context-measurement.json` `packages`), while the 0.37.1 runner describes the
   base as "seven pinned packages" (`SPR:scripts_eval-harnesses.mjs:849-850`). **The exact 0.33.0
   set is not published at this commit.** The published release is **0.33.0 with Jev off**
   (`SITE:` §06). Under Harbor the same install would have to be driven by a **custom adapter**
   subclassing `pi` (none exists in the repo); that this is what happened is **[INFERENCE]** from
   the run directory naming (`.../sp033` beside `.../pi`, `SPR:scripts_tb2-metrics.mjs:86-88`).
2. **Model / endpoint / wire.** Same id, same one endpoint as every other arm; no override
   published; wire **U** for the same reason as Pi.
3. **Prompt / tools.** SpecPi adds a global `AGENTS.md` working agreement (the template at the pinned
   commit is 1,216 bytes, `SBR:templates_AGENTS.md`; the 0.33.0 installed copy is not published) and
   up to five tools over Pi: `create_goal`, `get_goal` (pi-goal-x),
   `report_capability_gap`, `request_capability`, `background`
   (`SPR:scripts_eval-harnesses.mjs:886-888`; `SBR:site_research_context-measurement.json`). Two of
   those—`report_capability_gap` and `request_capability`—are omitted in a headless session, and
   `background` is offered only to interactive sessions (`SPR:CHANGELOG.md:65,71`). Measured at
   **0.34.0 in RPC mode**: 9 tools, 12,287 chars first request; headless is quoted at ≈8,300 chars
   (`SPR:CHANGELOG.md:71`). The 0.32.0 entry trims the working agreement from ≈2,400 to ≈1,300 characters
   (`SPR:CHANGELOG.md:72`), so the 0.33.0 layer carries the shorter text; `eval-harnesses` puts the AGENTS file at "4.2 KB of the 8.5 KB SpecPi
   adds" (`:936`) — the two figures are from different builds and cannot both describe 0.33.0;
   the 6-tool headless surface (Pi's four + the two goal tools) is the defensible claim.
   Which mode the TB2 run used is **U**; Harbor's `pi` agent runs `pi --print --mode json`
   (`HV:agents/installed/pi.py:510`), i.e. headless, so a Harbor-driven SpecPi row would have been
   headless unless the author's adapter used RPC.
4. **Limits / timeouts / effort.** **U.** The tier suite's tier-6 compaction window (24,000 tokens,
   `SPR:CHANGELOG.md:76`) is a suite setting, not a TB2 setting, and must not be read across.
5. **Trials / pooling.** **Two sittings**, 3 attempts/task each → 6/task, 77 attempts (38 + 39)
   (`SPR:scripts_tb2-metrics.mjs:86-88,104-105`). Concurrency 6.
6. **Scoring.** Identical rule to Pi, and tokens come from the same Harbor `agent_result` fields
   (the unpublished adapter would have to populate them). One trial has no reward — `raman-fitting`,
   `NetworkConnectionError` (`SITE:` §1.7/errors) — and is excluded from the denominator, which is why SpecPi's
   `raman-fitting` cell reads 2/5 rather than /6.
7. **Reproducible?** **No**, and worse than Pi: the SpecPi arm additionally depends on an
   unpublished adapter and an unpublished install step order.

### 1.3 Oh My Pi (OMP) — `omp`

1. **Agent / install / pin.** **No Harbor built-in.** Harbor 0.25.0's agent registry has no
   `oh-my-pi`/`omp` entry (`HV:models/agent/name.py:3-48`), and `harbor agent schema` lists none —
   checked against 0.25.0 only; a later Harbor release could have added one.
   The repo publishes no adapter either, so the adapter used for TB2 is **U**. Version: the context
   measurement used **18.2.5** (`SBR:site_research_context-measurement.json` "ohMyPiVersion";
   `SPR:CHANGELOG.md:76`), but that is the build measured on 24 Sep, not necessarily the build in
   the 21 Sep or 24 Sep TB2 runs. OMP is a Pi fork shipped as `@oh-my-pi/pi-coding-agent`, needs Bun
   (`SPR:CHANGELOG.md:271`), and its own README lists `bun install -g @oh-my-pi/pi-coding-agent` and
   a prebuilt-binary path (`SBR:ohmy-pi_README.md:53-58`).
2. **Model / endpoint / wire.** Same id and same one endpoint (PB §1.1); the 24 Sep sitting is
   described as "same tasks, seven attempts, model and endpoint" only for the git pair
   (`SPR:scripts_tb2-metrics.mjs:91-93`); for the widened sittings the pairing claim is structural
   ("the arms inside it faced the same provider state", `:55-58`). No override published.
3. **Prompt / tools.** Measured at 18.2.5: **11 tools** — `bash`, `edit`, `eval`, `glob`, `grep`,
   `hub`, `read`, `task`, `todo`, `web_search`, `write` — 48,467 schema chars + 18,241 instruction
   chars = 66,708 (`SBR:site_research_context-measurement.json`; `SPR:CHANGELOG.md:271` quotes the older
   65,843). Measured with `--no-rules --no-extensions` (`SBR:scripts_measure-context.mjs:647-655`),
   so project rules/extensions are excluded; OMP's README advertises 31 built-in tools, of which 11
   ship on by default. TB2's exact tool surface is **U**.
4. **Limits / timeouts / effort.** **U.**
5. **Trials / pooling.** **Two sittings**: s1 contributed 78 trials (39 + 39 from two launches that
   raced into one output directory, measured at 12-way concurrency), s9 contributed 38 → 116
   attempts, **9/task** (6 + 3), except 8 on `vulnerable-secret`
   (`SPR:scripts_tb2-metrics.mjs:78-80,104-105,193-199`).
6. **Scoring.** Same rule; one `NetworkConnectionError` on `vulnerable-secret` excluded
   (`SITE:` §1.7). OMP's rate therefore rests on 8–9 attempts per task, more than any arm except Pi.
7. **Reproducible?** **No** — adapter, version-at-run and per-trial data all unpublished.

### 1.4 `[CC]` (label used by the page; Claude Code CLI)

1. **Agent / install / pin.** Harbor's built-in `claude-code` is the only Claude-family agent in
   Harbor 0.25.0 (`HV:models/agent/name.py:8`) and is the likely adapter — **[INFERENCE]**; SpecPi
   publishes neither the adapter nor the CLI version. It is installed
   via `npm install -g @anthropic-ai/claude-code[@version]` or the native installer
   (`HV:agents/installed/claude_code.py:635-658`). **Version not published** anywhere (PB §2.1).
2. **Model / endpoint / wire.** `deepseek-v4.1-flash` through an **Anthropic Messages →
   chat-completions translation**: "Its traffic crosses this repository's Messages/chat-completions
   translation, which at the time never mapped the provider's cached share back to
   `cache_read_input_tokens`" (`SPR:scripts_tb2-metrics.mjs:22-27`). That is direct evidence a
   translating proxy sat in front of [CC] for the TB2 runs, and that the bug was fixed before the
   22 Sep sitting. Base URL/key used are **U**.
3. **Prompt / tools.** **U.** SpecPi does not measure [CC]'s first-call surface; its context chart
   carries Codex and [CC] as faded rows from HarnessTax, explicitly "a rough reference, not a
   head-to-head test" (`SBR:README.md:62`). [CC]'s own harness prompt and tool set are whatever the
   CLI ships.
4. **Limits / timeouts / effort.** **U**, except the observed failure `AgentSetupTimeoutError` on
   `configure-git-webserver` (`SITE:` §1.7) and the note that setup was given 3× Harbor's default
   for the 24 Sep runs (`SPR:scripts_tb2-metrics.mjs:92-93`).
5. **Trials / pooling.** Two sittings, 3/task each; 77 attempts, one task 5/6 after the setup
   timeout. Cost/cache cover only the 39 attempts after the mapping fix (`:22-27`; PB §1.3).
6. **Scoring.** Same rule; tokens and rewards were recorded correctly, but the 21 Sep cache count is
   recorded as `null`, not zero, and those
   attempts are left out of cache/cost but still count for reward (`:22-27`, `:372-386`).
7. **Reproducible?** **No**, and [CC] is the worst case: version, prompt, tool set and translation
   layer are all unpublished.

### 1.5 OpenCode — `opencode`

1. **Agent / install / pin.** Harbor built-in `opencode` — the generator names "Harbor's OpenCode
   agent" and describes exactly the `tokens.output` vs `tokens.reasoning` split that Harbor's
   OpenCode parser produces (`SPR:scripts_tb2-metrics.mjs:15-19`; `HV:agents/installed/opencode.py:669,689`).
   Install is `npm i -g opencode-ai[@version]` (`HV:agents/installed/opencode.py:417-431`).
   **Version not published**; the context measurement used **1.18.31**
   (`SBR:site_research_context-measurement.json`), which is not evidence for the 22 Sep run.
2. **Model / endpoint / wire.** Same id/endpoint. In the tier suite OpenCode "reads ambient
   credentials … its own auth", i.e. it can bypass the shared key (`SPR:evals_README.md:166`);
   whether the TB2 run did is **U**. Chat-completions is implied by the [OI] endpoint.
3. **Prompt / tools.** Measured at 1.18.31: **10 tools** — `bash`, `edit`, `glob`, `grep`, `read`,
   `skill`, `task`, `todowrite`, `webfetch`, `write` — 21,464 schema + 9,579 instruction chars =
   31,043 (`SBR:site_research_context-measurement.json`). OpenCode maps the logical model id to a
   provider-qualified one (`SPR:evals_README.md:150-155`).
4. **Limits / timeouts / effort.** **U** for TB2. The tier suite can hold OpenCode to a declared
   context window — [CC] is the harness that has no such setting (`SPR:evals_README.md:197-199`) —
   but no TB2 value or compaction setting is published.
5. **Trials / pooling.** **One sitting** (s7), 3/task, 39 attempts (`SPR:scripts_tb2-metrics.mjs:81`).
6. **Scoring.** Same rule; Harbor records tokens per trial. Tokens are corrected upward: reasoning is read from
   `trajectory.json` per step and added to output, because Harbor files it separately; on 22 Sep it
   was **2.6×** the recorded output (`SPR:scripts_tb2-metrics.mjs:15-19,234-245,278-283`).
7. **Reproducible?** **No** — one sitting, version unpublished, model mapping unpublished.

### 1.6 DeepSeek Harness (DSH) — `dsh`

1. **Agent / install / pin.** **No Harbor built-in in 0.25.0** (`HV:models/agent/name.py`; later
   releases not checked). Pinned
   **0.1.5-rc.3**, because "rc.2 — npm's `latest` — now resolves rc.3 sub-packages and fails to
   boot" (`SPR:scripts_tb2-metrics.mjs:77-80`). The tier suite launches it as
   `node <bin> --profile headless "<prompt>"` with `SPECPI_DSH_CLI` pointing at the bin
   (`SPR:evals_README.md` §dsh; `SBR:scripts_measure-context.mjs:526-560`). The Harbor adapter is
   **U**.
2. **Model / endpoint / wire.** Same id/endpoint. Its tokens are **not self-reported**: "what a
   counting proxy inside the container saw leave" (`SPR:scripts_tb2-metrics.mjs:74-80`). Wire:
   chat-completions through its `cordis.patch.yml` provider layer (`SBR:scripts_measure-context.mjs:526-553`).
3. **Prompt / tools.** Measured at 0.1.5-rc.2: **25 tools** (incl. `pwsh`, `subagent`,
   `workflow`, `job_*`, `ralph`, `exit_plan_mode`) — 27,057 schema + 4,686 instruction chars =
   31,743 (`SBR:site_research_context-measurement.json`; `SPR:CHANGELOG.md:243`).
4. **Limits / timeouts / effort.** **U.**
5. **Trials / pooling.** **One sitting** (s7), 3/task, 38 attempts (`sqlite-with-gcov` 2/3 after a
   `NonZeroAgentExitCodeError`, `SPR:scripts_tb2-metrics.mjs:74-80`; `SITE:` §1.7).
6. **Scoring.** Same rule; tokens are proxy-counted, so the counting point is outside Harbor's
   `agent_result` (a different measurement instrument from the other five arms). If the unpublished
   adapter also wrote `agent_result` token fields, which fields were used is **U**.
7. **Reproducible?** **No** — adapter, counting proxy and per-trial data unpublished.

### 1.7 Cross-cutting: what none of the six publish

Read this before treating any per-harness dossier as complete.

| Setting | Status across all six |
|---|---|
| `temperature`, `top_p` | Never mentioned in the page, the generator, `evals/README.md` or the CHANGELOG. **U** for every arm. |
| `reasoning_effort` / thinking level | **U** for every arm. Cline's set is not evidence here (PB §3.1). |
| Context window / output cap | **U** for every arm. The only published window is a tier-suite setting (24,000 tokens, `SPR:CHANGELOG.md:76`). |
| Max turns / max steps | **U** for every arm. |
| AgentTimeout handling | The generator has no special case: a timed-out trial counts by whatever numeric reward its verifier produced, and is excluded only when no numeric reward exists (`SPR:scripts_tb2-metrics.mjs:266-272`). Setup failures that leave no reward appear in `errors` (`SITE:` §1.7). |
| Dataset digest / Harbor version | **U** (PB §6). |
| Per-trial result data | Deliberately absent (`SPR:scripts_tb2-metrics.mjs:7-10`). |

---

## 2. Side-by-side at a glance

| | Pi | SpecPi 0.33.0 | OMP | [CC] | OpenCode | DSH | **LC (ours)** |
|---|---|---|---|---|---|---|---|
| Harbor agent | built-in `pi` | unpublished adapter over `pi` [INFERENCE] | unpublished | built-in `claude-code` (inferred) | built-in `opencode` | unpublished | `lcx_agent:LcxAplus` |
| Version | 0.87.0/0.87.1 | 0.33.0 | 18.2.5 measured; run **U** | **U** | 1.18.31 measured; run **U** | 0.1.5-rc.3 | G-R1a `4968ea8` (`bench/PREREGISTRATION_TB2_W13.md`) |
| Tools (measured) | 4 | 9 RPC / 6 headless [INFERENCE] | 11 | **U** | 10 | 25 | LC full policy (own set) |
| First-request chars | 5,521 | 12,287 RPC / ≈8,300 headless | 66,708 | **U** | 31,043 | 31,743 | not published |
| Attempts/task | 15 (5 sittings) | 6 (2) | 9 (2; 8 on one) | 6 (2) | 3 (1) | 3 (1) | 3 (1) |
| Widened attempts | 195 | 77 | 116 | 77 | 39 | 38 | 39 (13×3), 36 analysed after ENV drop |
| Timeout multiplier | 1.0 (default) | 1.0 | 1.0 | 1.0 | 1.0 | 1.0 | **2.0** |
| Max steps/turns | **U** | **U** | **U** | **U** | **U** | **U** | **150** |
| Context/output cap | **U** | **U** | **U** | **U** | **U** | **U** | **600K / 32,768** |
| Reasoning effort | **U** | **U** | **U** | **U** | **U** | **U** | none sent |
| Concurrency | 6 | 6 | 6 (one slice 12) | 6 | 6 | 6 | **1** |

Tool counts and first-request chars come from `SBR:site_research_context-measurement.json`, measured
2026-09-24 at **SpecPi 0.34.0 / Pi 0.84.4** — not the TB2 builds. They are the best available
evidence of *shape* (4 vs 6–9 vs 11 vs 25 tools), not of the exact TB2 request.

### 2.1 Pooling and trimming mechanics

* Attempts per task **per sitting**: 3 on the widened slice, 5 on calibration, 7 on the git pair
  (`SPR:scripts_tb2-metrics.mjs:160-170`). A sitting is one launch; arms inside it share the hour
  and the provider state, and the page treats only within-sitting comparisons as attributable
  (`:52-58`).
* Pooling is a **micro-average over attempts**, not a mean of per-task rates: `rate = solved /
  attempts` over every pooled row (`:359-374`).
* **Trimming:** Pi's lowest (21/39) and highest (32/39) sittings are removed by name, one from each
  end (`:112-116`); no other arm is trimmed.
* **Raced launch:** OMP's s1 output directory was written by two concurrent launches, so that
  sitting holds 6 attempts/task at 12-way concurrency; the generator counts all of them and says so
  (`:193-199`).
* **Not pooled:** the git pair (2 tasks × 7 attempts, `:167-170,635`) and SWE-bench Verified
  (`:117-137`).
* **Paired statistics:** token and cost ratios are computed only in sittings where both arms ran
  (`:392-430`); solve-rate p-values come from Fisher's exact test, recomputed from the data rather
  than quoted (`:305-345`).

---

## 3. Fairness table — each arm against an LC run

Reference arm is **LC**. `S` = same as LC, `D` = differs, `U` = unknown / not published.
Superscripts point at the evidence below.

| Dimension | Pi | SpecPi | OMP | [CC] | OpenCode | DSH | **LC (ref)** |
|---|---|---|---|---|---|---|---|
| 1. Task set identical? | S¹ | S¹ | S¹ | S¹ | S¹ | S¹ | ref¹ |
| 2. Same model version? | U² | U² | U² | U² | U² | U² | ref² |
| 3. Same endpoint? | D³ | D³ | D³ | D³ | D³ | D³ | ref³ |
| 4. Same trials per harness? | D⁴ | D⁴ | D⁴ | D⁴ | S⁴ | S⁴ | 3/task⁴ |
| 5. Same prompt / tool surface? | D⁵ | D⁵ | D⁵ | D⁵ | D⁵ | D⁵ | ref⁵ |
| 6. Same context / token limits? | U⁶ | U⁶ | U⁶ | U⁶ | U⁶ | U⁶ | 600K/32,768⁶ |
| 7. Same timeouts / max steps? | D⁷ | D⁷ | D⁷ | D⁷ | D⁷ | D⁷ | 2.0/150⁷ |
| 8. Same scoring? | D⁸ | D⁸ | D⁸ | D⁸ | D⁸ | D⁸ | reward 1⁸ |
| 9. Independently reproducible? | N⁹ | N⁹ | N⁹ | N⁹ | N⁹ | N⁹ | Y⁹ |

Cells are relative to the LC column, which is the reference. `N`/`Y` = no/yes for reproducibility;
the other cells use `S`/`D`/`U` as defined above.

¹ Same 13 task names, verified two ways: the generator's three widened-slice directories contain
exactly the 13 (`SPR:scripts_tb2-metrics.mjs:86-105`) and the replay artifacts list 20 task names
(`SBR:evals_runs_jev-progress-replay.json`, `points[].task`). Caveats: no dataset digest is
published (PB §4), and **LC's primary set is 12**, because `pytorch-model-cli` is `ENV`-excluded
(`bench/EXECUTION_LOG_TB2_W13.md`). So dimension 1 is "same names, different analysed set".

² Same model **id** (`deepseek-v4.1-flash`) for all six and for LC. The provider-side revision is
unpublished on both sides; neither run pins one.

³ SpecPi ran "one endpoint" (`SITE:` §06) whose identity is unpublished; the repo documents the
OpenCode Go [OI] base for its tier suite (`SPR:evals_.env.example:10`). LC reaches a local sub2api
gateway through a metering proxy at `host.docker.internal:18080` (`bench/adapters/lcx_agent.py:24,78-95`).
The serving stack, rate limits and cache behaviour differ.

⁴ Pi 15/task, OMP 9/task (8 on one), SpecPi/[CC] 6/task (5 on one task each), OpenCode/DSH 3/task; LC 3/task
(`bench/PREREGISTRATION_TB2_W13.md`; `bench/run_w13.sh`). Pi's own counted sitting spread (22–30 of 39 across the five
kept sittings; 21 and 32 in the two excluded ones, PB §1.4) is larger than most between-harness gaps, so cell 4 is a first-order fairness problem, not
a footnote.

⁵ Different by construction: 4 / 6–9 / 11 / U / 10 / 25 tools (`SBR:site_research_context-measurement.json`),
different system prompts, and SpecPi additionally installs a working agreement
(`SBR:templates_AGENTS.md`). LC sends no task-specific prompt or skill
(`bench/PREREGISTRATION_TB2_W13.md`), so part of any LC-vs-arm gap is prompt, not capability.

⁶ No SpecPi arm publishes a context window, output cap or compaction threshold for TB2. LC fixes
600,000 / 32,768 (`bench/adapters/lcx_agent.py:29-30`). The tier-suite 24,000-token tier-6 window
(`SPR:CHANGELOG.md:76`) is not a TB2 setting.

⁷ All six ran Harbor's default timeout multiplier (1.0), with setup at 3× only for the 24 Sep git
pair (`SPR:scripts_tb2-metrics.mjs:91-93`). LC ran `--timeout-multiplier 2` (`bench/run_w13.sh`) on
amd64 images emulated on arm64. Max steps/turns are unpublished for all six; LC caps at 150.

⁸ Same **reward rule** (`reward >= 1`) but different **denominators**: SpecPi excludes any attempt
without a numeric reward from the denominator (`SPR:scripts_tb2-metrics.mjs:266-272`) and drops one
task for an environment build failure (`:189`); LC re-runs INFRA once and keeps both, and excludes
`pytorch-model-cli` as ENV, leaving 12 tasks (`bench/EXECUTION_LOG_TB2_W13.md`). A 5/6 and a 2/3
are not the same estimator.

⁹ SpecPi side: no job configs, no launch scripts, no adapters, no per-trial result data, no Harbor
version, no endpoint, no kwargs (PB §2.2, §6). The replay artifacts expose per-trial IDs and pass
flags for the Pi/Jev arms only — enough to re-derive Pi's sitting counts, not enough to recompute
any token or cost figure. LC side: preregistration, run script, adapter source, binary SHA-256 and
every trial directory are in-repo (`bench/PREREGISTRATION_TB2_W13.md`, `bench/run_w13.sh`,
`bench/adapters/lcx_agent.py`, `bench/jobs/`), so the LC arm *is* re-runnable on the same machine —
the reproducibility gap is one-directional.

---

## 4. The five gaps that matter most for an LC comparison

1. **Serving path.** The model id matches; the endpoint does not, and SpecPi's is unpublished. Token
   counts, cache-hit rate, latency and timeout exposure all move with the serving stack. LC's
   Harbor record has `n_cache_tokens: null` (`bench/jobs/tb2-w13a/fix-git__5dofXrU/result.json`),
   but its metering proxy logs `usage.prompt_tokens_details.cached_tokens` per request
   (`bench/jobs/metering/proxy.jsonl`), so a cached share is recomputable from proxy logs — at a
   different counting point than SpecPi's Harbor-recorded figure, and only after a per-request join.
2. **Attempts and pooling.** Pi's published rate is 15 attempts/task across five sittings; LC's is
   3 in one. With Pi's own spread of 22–30/39 across the five kept sittings (21 and 32 in the two excluded
   ones), LC's per-task cells carry
   far more variance than the numbers they are placed beside.
3. **Timeout, step and effort asymmetry.** SpecPi at multiplier 1.0 on (probably) native x86 and
   with unknown turn caps; LC at 2.0 on emulated amd64 with 150 steps and no effort setting. A
   timeout counts as a wrong answer in both, but the two runs do not buy the same amount of work
   per timeout.
4. **Scoring denominators and the analysed task set.** SpecPi excludes errored attempts and drops
   `pytorch-model-recovery`; LC re-runs INFRA, excludes `pytorch-model-cli` as ENV and compares on
   12 tasks. Two "same 13 tasks" claims are therefore not the same estimator.
5. **Prompt/tool surface is the treatment, not a controlled variable.** 4 tools vs 6–9 vs 11 vs 25,
   plus an installed working agreement for SpecPi. Any LC-vs-arm difference is
   harness-plus-prompt, which is legitimate for a harness comparison but must not be reported as a
   model or task-solving difference.

---

## 5. Controlled Comparison recipe — **NOT RUN**

Goal: run Pi, OpenCode and SpecPi (optionally OMP) under the *same* Harbor, model, endpoint, tasks,
timeout multiplier, resources and scoring as the LC arm, using Harbor 0.25.0 built-ins plus one
small local adapter for SpecPi. No new benchmark runner; no modifications to tasks or verifiers.

### 5.0 Fixed inputs (identical for every arm)

Borrow `bench/run_w13.sh`'s scaffold; it already encodes LC's conditions.

| Input | Value | Why |
|---|---|---|
| Dataset | `--path ./tb2/terminal-bench` (local export of `terminal-bench@2.0`, 89 tasks) | same bytes LC ran |
| Tasks | `bench/tb2-widened13.txt` via `-i` | identical 13 names |
| Model | `-m oi/deepseek-v4.1-flash` | same id; `oi` is only a provider label |
| Endpoint | `--base-url http://host.docker.internal:18080/v1` | the metering proxy LC uses |
| Wire | `--api-format openai_chat_completions` | same [OI] wire as LC |
| Key | `--api-key-name OPENAI_API_KEY --ae OPENAI_API_KEY=dummy-exp-key` | proxy holds the real key |
| Attempts | `-k 3` | LC's preregistered 3 |
| Concurrency | `-n 1` | LC's preregistered 1; the Docker VM is 3.83 GiB |
| Timeout | `--timeout-multiplier 2` | LC's condition; note it differs from SpecPi's published 1.0 |
| Harbor | 0.25.0 | LC's version; pins agent install code |
| Proxy env | `--ae http_proxy=… --ae no_proxy=host.docker.internal,…` (from `run_w13.sh`) | same network posture |
| Images | pre-pulled and retagged, per `bench/HARBOR_SETUP.md` §2.3 | avoids 600 s build timeouts |

Run one arm per job directory so trial IDs never collide. Record the resolved command with
`harbor run --print-config` before each launch.

### 5.1 Pi (base) — **NOT RUN**

```sh
cd bench
TASKS=$(sed 's/^/-i /' tb2-widened13.txt | tr '\n' ' ')
PYTHONPATH=$PWD/adapters env -u all_proxy -u ALL_PROXY harbor run \
  --path ./tb2/terminal-bench $TASKS \
  --agent pi -m oi/deepseek-v4.1-flash \
  --ak version=0.87.1 \
  --ak model_api=openai-completions \
  --base-url http://host.docker.internal:18080/v1 \
  --api-format openai_chat_completions \
  --api-key-name OPENAI_API_KEY \
  --ae OPENAI_API_KEY=dummy-exp-key \
  -n 1 -k 3 --timeout-multiplier 2 \
  -o ./jobs --job-name pi-w13-controlled -y
```

Why these kwargs: a custom base URL is refused unless `model_api` is set and an API-key env name is
given (`HV:agents/installed/pi.py:127-152,279-283`); `version` pins the npm install
(`:227-243`). Harbor then writes `models.json` with provider `harbor-endpoint` and runs
`pi --print --mode json --provider harbor-endpoint --model deepseek-v4.1-flash`
(`:260-311,485-514`). Version choice: 0.87.1 is the newest build the SpecPi trials reported
(`piVersions`), so it is the closest match to their Pi rows; 0.87.0 is the alternative.

### 5.2 OpenCode — **NOT RUN**

Use the existing adapter so the 1.19.4 emulation hang does not recur:

```sh
cd bench
PYTHONPATH=$PWD/adapters env -u all_proxy -u ALL_PROXY harbor run \
  --path ./tb2/terminal-bench $TASKS \
  --agent opencode_local:OpenCodeLocal -m oi/deepseek-v4.1-flash \
  --base-url http://host.docker.internal:18080/v1 \
  --api-format openai_chat_completions \
  --api-key-name OPENAI_API_KEY \
  --ae OPENAI_API_KEY=dummy-exp-key \
  -n 1 -k 3 --timeout-multiplier 2 \
  -o ./jobs --job-name oc-w13-controlled -y
```

`OpenCodeLocal` uploads `opencode-linux-x64` 1.19.4 and sets
`OPENCODE_EXPERIMENTAL_DISABLE_FILEWATCHER=1`, `OPENCODE_DISABLE_AUTOUPDATE=1`,
`OPENCODE_DISABLE_MODELS_FETCH=1`, `OPENCODE_DISABLE_LSP_DOWNLOAD=1`
(`bench/adapters/opencode_local.py:24-36`). The file watcher must stay off: under amd64 emulation
1.19.4 hung at "project copy refresh" or after "exiting loop" (30-minute AgentTimeout on one
occasion), and the same run exits 0 in 22 s with it off (`bench/adapters/opencode_local.py:31-36`).
Harbor's generated provider block is documented in `bench/HARBOR_SETUP.md` §7.2 (verified output at
Harbor 0.25.0). Version choice is a deliberate disclosure: SpecPi's OpenCode version is unpublished;
1.19.4 is ours, and 1.18.31 is only the build SpecPi measured for context.

### 5.3 SpecPi 0.33.0 — **NOT RUN** (small adapter required)

Harbor has no SpecPi agent and SpecPi ships no Harbor adapter. The minimal honest approach is a
subclass of Harbor's `Pi` that adds the installer step: `bench/adapters/specpi_agent.py`
(`SpecPi033`, added 2026-10-27 with the evaluation plan; **never executed**). It runs Harbor's Pi
install (pinned by `--ak version=0.87.1`), then, as the agent user whose `$HOME` holds Harbor's nvm
Node (`HV:agents/installed/node_install.py`), `npm install -g --ignore-scripts specpi@0.33.0`,
`PI_CODING_AGENT_DIR=/tmp/harbor-pi-agent SPECPI_PI=$(command -v pi) specpi install --yes
--skip-browser-install`, and writes `{"yoloMode":true}` to the permission system's config in that
agent directory. `/tmp/harbor-pi-agent` is `HV:agents/installed/pi.py:_REMOTE_PI_CONFIG_DIR`, which
Harbor's Pi run reads.

Launch it with the same flags as §5.1 plus `--agent specpi_agent:SpecPi033` and
`-m oi/deepseek-v4.1-flash`. The install shape follows the tier suite: installer invoked with
`--yes --skip-browser-install` and `SPECPI_PI` pointing at Pi
(`SPR:scripts_eval-harnesses.mjs:1020-1030`), disposable agent dir per attempt (`:1044-1047`), and
`yoloMode: true` because the installed permission system "fails closed to ask" and an unattended run
cannot answer (`:869-874,1050-1053`). Version pinning is doubled: the npm package is 0.33.0 and the
installer's own package pins apply inside (`:849-850`). SpecPi's installer requires Node.js 22.19+,
Git, npm and an existing Pi on PATH (`SBR:README.md:131`).

Unverified by execution: whether `specpi install` accepts `--skip-browser-install` in 0.33.0 as it
does in the 0.37.1 tree (**U**), and whether the installer writes into `PI_CODING_AGENT_DIR` when
that variable is set rather than the default `~/.pi/agent` (**U**; the tier suite passes `agentDir`
through its own fixture, not through that variable). Both must be checked with
`--install-only` before a counted run.

### 5.4 OMP 18.2.5 (optional) — **NOT RUN**

No built-in and no adapter, so this needs its own adapter, and its CLI flags must be re-verified.
Feasible shape, from the only published launch SpecPi has:

* Install: `bun install -g @oh-my-pi/pi-coding-agent@18.2.5` (`SBR:ohmy-pi_README.md:53-58`); musl images need
  `libstdc++`/`libgcc` (`SBR:ohmy-pi_README.md:45`).
* Provider: `~/.omp/agent/models.yml` with `baseUrl`, `api: openai-completions`, model
  `deepseek-v4.1-flash`; `~/.omp/agent/config.yml` with `modelRoles.default: <provider>/deepseek-v4.1-flash`
  (`SBR:ohmy-pi_README.md:358-384`).
* Invocation: `omp -p "<instruction>"` for one-shot (`SBR:ohmy-pi_README.md:498-500`), or
  `omp --mode rpc --no-session --no-rules --no-extensions -e <provider-extension.ts>`, which is
  what SpecPi's measurement uses (`SBR:scripts_measure-context.mjs:387-412,647-655`).
* Token capture: OMP's own usage must be read from its session output, or counted at the proxy.

Do **not** reuse Harbor's `pi` agent for OMP: it installs the wrong package and passes
`--provider/--model` flags whose OMP equivalents are unverified.

### 5.5 Token recording — **NOT RUN**

Three layers, recorded per trial:

1. **Harbor `result.json`** — `agent_result.n_input_tokens` / `n_output_tokens`. Pi sums session
   usage events (`HV:agents/installed/pi.py:546-560,607-625`); OpenCode reads final metrics
   (`HV:agents/installed/opencode.py:795-796`). Version-dependent: SpecPi had to add OpenCode's
   reasoning back because it was filed apart (`SPR:scripts_tb2-metrics.mjs:15-19`) — check whether
   0.25.0's `total_completion_tokens` already includes it before comparing.
2. **Metering proxy** — `bench/jobs/metering/proxy.jsonl` logs every request. LC tags requests with
   `X-Exp-Tag: <arm>/<trial>` (`bench/adapters/lcx_agent.py:6-8,84-87`); the built-in agents do not.
   With `-n 1`, attribute proxy lines to the one trial whose `started_at`/`finished_at` bracket them,
   and record that method. Do not invent a header the agent cannot send.
3. **Cache tokens** — Harbor's `n_cache_tokens` is `null` for LC (its Core usage has no cache
   field), but the proxy log carries `usage.prompt_tokens_details.cached_tokens`
   (`bench/jobs/metering/proxy.jsonl`). Compute the cached share from the proxy, label it as
   proxy-derived, and do not silently mix it with Harbor's field. SpecPi priced cost at a frozen
   rate list; if LC cost is reported at all, state the rate table used.

### 5.6 Scoring and denominators — **NOT RUN**

Pre-register, before the first trial:

* solved = `verifier_result.rewards.reward >= 1` (Harbor's own rule, same as SpecPi
  `SPR:scripts_tb2-metrics.mjs:360` and LC's preregistration);
* a trial with no numeric reward is classified `INFRA` / `ENV` / `TIMEOUT` / `MODEL` / `HARNESS` by
  the LC rules (`bench/EXECUTION_LOG_TB2_W13.md`), with `INFRA` re-run once and both kept;
* publish both the all-trials denominator and the LC-style denominator, so a reader can see the
  effect of the exclusion rule rather than inheriting SpecPi's;
* do not drop a task on either side without naming it. SpecPi dropped `pytorch-model-recovery`
  (`:189`); LC drops `pytorch-model-cli` as ENV. These are different tasks, so the two "13-task"
  runs compare on 12 and 12 but not the same 12.

### 5.7 Known pitfalls

| # | Pitfall | Mitigation | Evidence |
|---|---|---|---|
| 1 | Host `all_proxy=socks5://…` breaks Harbor at import (`socksio` missing) | `env -u all_proxy -u ALL_PROXY` on every command | `bench/HARBOR_SETUP.md` §2.1 |
| 2 | Docker Hub pulls take 6–26 min and exceed the 600 s build timeout | pre-pull via `docker.1ms.run`, retag to `alexgshaw/<task>:20251031` | `bench/HARBOR_SETUP.md` §2.3 |
| 3 | OpenCode 1.19.4 hangs under amd64 emulation with the file watcher on | use `opencode_local:OpenCodeLocal` (env switches) | `bench/adapters/opencode_local.py:24-36` |
| 4 | OpenCode reports `step_finish.cost = 0` for custom providers | take cost from the proxy, never from OpenCode | `bench/HARBOR_SETUP.md` §11 #6 |
| 5 | Pi refuses a custom base URL without `model_api` + API-key env name | `--ak model_api=openai-completions --api-key-name …` | `HV:agents/installed/pi.py:127-152,279-283` |
| 6 | SpecPi installer needs network and can be slow | `--install-only` smoke first; raise `--agent-setup-timeout-multiplier` if needed (SpecPi used 3× for the git pair) | `SPR:scripts_eval-harnesses.mjs:1027`; `SPR:scripts_tb2-metrics.mjs:91-93` |
| 7 | SpecPi's permission system fails closed to a dialog nobody can answer | `yoloMode: true` in the disposable agent dir | `SPR:scripts_eval-harnesses.mjs:869-874,1050-1053` |
| 8 | SpecPi headless vs RPC changes the tool surface (3 tools appear/disappear) | fix the invocation and record it; do not compare a headless row to an RPC row | `SPR:CHANGELOG.md:65,71` |
| 9 | OMP needs Bun; musl images need `libstdc++`/`libgcc` | install Bun or the prebuilt binary, add libs | `SBR:ohmy-pi_README.md:45,53-58` |
| 10 | `[CC]` speaks Anthropic Messages only; the gateway is chat-completions | would need a translating proxy (SpecPi had one); out of scope for this recipe | `SPR:scripts_tb2-metrics.mjs:22-27` |
| 11 | Verifier network failures under the proxy produce 0.0 without running tests | pre-register the INFRA rule; check `testsRun=0` and `Bad Gateway` | `bench/EXECUTION_LOG_TB2_W13.md` |
| 12 | Harbor's default `-n` is 4; the VM has 3.83 GiB | always pass `-n 1` | `harbor run --help`; `bench/HARBOR_SETUP.md` §1 |

### 5.8 What this recipe still cannot equalize

* SpecPi's endpoint (unpublished) and serving stack — the largest uncontrolled variable.
* Reasoning effort for any arm: Pi's `--thinking` is either set or defaulted, but there is no way to
  make it send *nothing*, which is what LC does. Record what each harness sends rather than
  pretending the setting is neutral.
* Context caps and turn limits: unpublished for every SpecPi arm, so matching LC's 600K/32,768/150
  would *create* a difference rather than remove one. Recommended: leave each harness at its default
  and disclose, since the question is harness behaviour at defaults.
* Host architecture: SpecPi ran Docker on a Windows host (`F:/…` path,
  `SPR:scripts_tb2-metrics.mjs:48`; arch unpublished); LC runs amd64 images emulated on arm64.

---

## 6. What a reader may and may not conclude

* May: on SpecPi's published conditions, the six harnesses span 0.553–0.805 on the widened slice.
  The only paired per-task separation the page highlights is `sanitize-git-repo` (10/10 vs 3/10,
  p = 0.0031); pooled cross-sitting comparisons exist (e.g. OMP vs DSH, p = 0.005) but the page's
  own rule limits causal claims to arms that ran in the same launch (`SPR:CHANGELOG.md:93`;
  PB §1.4–§1.6).
* May: the harness-layer differences above (tool counts, prompt sizes, trials, versions) are large
  enough that the published rates are not estimates of a single "model capability".
* May not: place an LC number beside a SpecPi number as an A/B. Endpoint, trials, timeouts, scoring
  denominators, emulation and effort all differ, and three of those are unpublished on SpecPi's
  side.
* May not: recompute any SpecPi figure below its published aggregates. The per-trial `result.json`
  files are deliberately absent (`SPR:scripts_tb2-metrics.mjs:7-10`).

---

## 7. Files added by this audit

Downloaded byte-for-byte from `raw.githubusercontent.com/TannerMidd/SpecPi/af36b0a1d48e7f706d40e30413e3150b067f45e6/…`,
except the OMP README (from `can1357/oh-my-pi@main`, the only copy of that CLI's install and provider
syntax); URL, UTC fetch time and sha256 for every file in `bench/baselines/raw/specpi/MANIFEST.csv`.

| File | Bytes | sha256 (first 16) | What it adds |
|---|---:|---|---|
| `README.md` | 12,654 | `aa68b5fef7fd3410` | install command, first-call chart caveats, HarnessTax rows |
| `scripts_measure-context.mjs` | 32,502 | `46be7f9d75160826` | exact CLI shape for OMP/OpenCode/DSH |
| `site_research_context-measurement.json` | 11,373 | `fa6514a566ce203f` | per-harness tools, chars, versions |
| `templates_AGENTS.md` | 1,216 | `b3fbc0cb9f834cf6` | SpecPi working agreement text |
| `evals_runs_jev-progress-replay.json` | 139,382 | `94dcc8028e9b7398` | 577 per-trial IDs, 20 task names |
| `evals_runs_jev-completion-replay.json` | 174,212 | `cf20c0d415c55b06` | 479 per-trial records, pass/state |
| `evals_runs_jev-untrusted-replay.json` | 399,024 | `3e93c232f1e21c83` | 737 fetch records (context only) |
| `ohmy-pi_README.md` | 45,832 | `8c6cb4c272a53431` | OMP install, models.yml/config.yml, `omp -p` and RPC |

Reproduce the two derived facts used above:

```sh
# 20 task names and Pi's per-sitting counts, from the replay artifact
python3 - <<'EOF'
import json, collections
d = json.load(open('bench/baselines/raw/specpi/evals_runs_jev-progress-replay.json'))
print(sorted({p['task'] for p in d['points']}))
c = collections.Counter((p['sitting'], p['arm']) for p in d['points'])
print(dict(sorted(c.items())))
EOF

# per-harness tool surface and first-request size
python3 - <<'EOF'
import json
d = json.load(open('bench/baselines/raw/specpi/site_research_context-measurement.json'))
for r in d['results']:
    print(r['label'], r['toolCount'], r['toolSchemaChars'] + r['instructionChars'])
EOF
```
