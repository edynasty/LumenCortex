# Public Terminal-Bench 2.0 baselines — the 13 widened tasks

Compiled 2026-10-26. Scope: what is publicly known about six harnesses on the thirteen
Terminal-Bench 2.0 tasks our run uses, on model `deepseek-v4.1-flash` where such data exists,
plus the official leaderboard's rules. Everything numeric below is traceable to a file under
`bench/public/raw/` (hashes in `raw/MANIFEST.csv`) or to a cited URL and commit.

This is **not** a same-environment A/B. Section 5 lists every condition difference we found.

Companions (2026-10-27): `bench/baselines/BASELINE_A_DEEPSEEK_OFFICIAL.md` (DeepSeek's own numbers and
documented sampling/protocol semantics) and `bench/baselines/BASELINE_B_SPECPI_HARNESS.md` (SpecPi's
harness rows audited per harness, fairness table, and the NOT RUN controlled-comparison recipe). The
evaluation plan that uses all three is `bench/EVAL_PLAN_TB2_DUAL_BASELINE.md`.

---

## 0. What can and cannot be compared

| Question | Answer |
|---|---|
| Is there a published per-task baseline on `deepseek-v4.1-flash` for these 13 tasks? | **Yes, one**: SpecPi's evaluations page (six harnesses, same model, same endpoint, Harbor on Docker). |
| Does the official tbench.ai TB2 leaderboard have `deepseek-v4.1-flash`? | **No.** 142 rows; the only DeepSeek row is `DeepSeek-V3.2` (39.6% ± 2.8). |
| Does the cline `2026-06-open-weights` set have `deepseek-v4.1-flash`? | **No.** Four open-weight models: `glm-5.2`, `minimax-m3`, `mimo-v2.5-pro`, `kimi-k2.7-code`. |
| Is per-attempt data (wall time, trajectories, tool calls) published for the deepseek runs? | **No** for SpecPi (aggregates only, runs deliberately not in the repo). **Yes** for cline — but for other models. |
| Can our 13-task result be placed on the official leaderboard? | **No.** Official rules require the full dataset, ≥5 trials per task, `timeout_multiplier` 1.0 and no resource overrides (§4). |

---

## 1. Source S1 — SpecPi evaluations page (primary)

| Item | Value |
|---|---|
| URL | https://tannermidd.github.io/SpecPi/evaluations/ |
| Page raw file | `raw/specpi-site/evaluations-index.html` — 58,489 B, sha256 `1bfa325a90a6f89d…` (full hash in `raw/MANIFEST.csv`), fetched 2026-10-26T13:37:11Z |
| Data file | https://tannermidd.github.io/SpecPi/evaluations/terminal-bench-2.json → `raw/specpi-site/terminal-bench-2.json`, 132,076 B, sha256 `052fb135166cefd66a5106764288ed0d9f7204b9faa18523907349bc6f843655` |
| Data `generatedAt` | `2026-09-24` (JSON field) |
| Provenance | The page HTML is **byte-identical** to `site/evaluations/index.html` in the SpecPi repo at commit `af36b0a1` (same sha256) — so the page and the repo agree exactly. |

### 1.1 What it is

Six harnesses on one model through one endpoint, run by Harbor on Docker, on Terminal-Bench 2.0
plus SWE-bench Verified. The page's own method text (§06, quoted verbatim from the HTML):

> Model deepseek-v4.1-flash through one endpoint, run by Harbor on Docker. … Terminal-Bench
> 7 calibration × 5 attempts, 13 widened × 3 per sitting, and a git pair of 7 attempts on each
> git task for every harness, kept out of the pooled totals. SpecPi the published 0.33.0 with
> Jev off. … Cost recomputed from logged tokens at a frozen price list. Left out one broken
> task; failed setups.

Slice definitions (JSON `slices`):

| Slice id | Label | Design | Pooled into the headline? |
|---|---|---|---|
| `slice1` | Calibration | 7 tasks × 5 attempts | yes (`overall` only) |
| `widen` | Widened | 13 tasks × 3 attempts **per sitting** | yes — this is the comparison slice |
| `focused` | Git pair | `sanitize-git-repo` + `fix-git`, 7 attempts each | **no** ("kept out of pooled totals") |

`totalAttempts` = 731 scored TB2 attempts; `taskCount` = 20 (13 widened + 7 calibration; the
seven calibration task names are **not published**). This is 20 of TB2's 89 tasks, and nothing on
the page or in the generator indicates the remaining 69 ran; the 0.33.0 CHANGELOG entry says
"Terminal-Bench 2.0's hard split and a SWE-Gym slice have not run".

### 1.2 Field semantics (from the generator, `scripts/tb2-metrics.mjs`)

These matter because the JSON's `inputTokens` is not a total:

- `inputTokens` / `outputTokens` — **mean per attempt** (`summarize()` divides by `attempts`,
  `scripts_tb2-metrics.mjs:359-390`). The site labels it "Prompt tokens per attempt … tokens
  sent, summed over the attempt's model calls", i.e. cumulative across all model calls in one
  attempt, so a value above the model's context window is normal.
- `rate` = `solved / attempts`; `solved` counts trials with `verifier_result.rewards.reward >= 1`.
- `cacheHitRate` = cached share of prompt tokens, `null` when nobody recorded it.
- `cost` = mean per attempt over the attempts whose cached share was recorded; `costAttempts`
  says how many that is, `costIsUpperBound` flags "no cache data, so all prompt tokens billed
  fresh".
- Aggregates pool attempts across sittings (micro-average). Per-task figures are on the
  widened slice only (`scripts_tb2-metrics.mjs:558-575`).

### 1.3 Harness totals

Harness ids in the data file map to labels as: `pi` = Pi (base), `specpi` = SpecPi 0.33.0,
`omp` = Oh My Pi, `claude-code` = `[CC]` (Claude Code, the name the page uses),
`opencode` = OpenCode, `dsh` = DeepSeek Harness.

| Harness | Widened (13 tasks) | Rate | Prompt tok/attempt | Output tok/attempt | Cache hit | Cost/attempt | Cost attempts |
|---|---|---:|---:|---:|---:|---:|---:|
| Pi (base) | 134/195 | 0.687 | 457,810 | 16,844 | 92.8% | $0.01631 | 195 |
| SpecPi 0.33.0 | 62/77 | 0.805 | 407,416 | 14,342 | 92.4% | $0.01435 | 77 |
| Oh My Pi | 84/116 | 0.724 | 1,255,661 | 28,024 | 96.5% | $0.02722 | 116 |
| [CC] | 50/77 | 0.649 | 764,901 | 36,413 | 95.4% | $0.02734 | 39 |
| OpenCode | 29/39 | 0.744 | 494,173 | 14,039 | 96.5% | $0.01243 | 39 |
| DeepSeek Harness | 21/38 | 0.553 | 1,126,586 | 19,348 | 95.4% | $0.02261 | 38 |

Pooled-over-everything figures (`overall`, includes the calibration slice) are 164/230, 62/77,
115/151, 81/112, 29/39, 21/38 for the same order. `[CC]`'s cost covers 39 of 112 overall
attempts (21 Sep runs predate its cache-mapping fix).

### 1.4 Per-sitting results (the noise baseline)

Bare Pi — unchanged software, same tasks, same machine — spans 22/39 to 30/39 across sittings.
That spread is the reason the page reports per sitting and why any single-sitting gap between
harnesses is weak evidence.

| Harness | Sitting | Solved/attempts | Prompt tok/attempt | Cost/attempt |
|---|---|---:|---:|---:|
| Pi (base) | 21 Sep (s1) | 30/39 | 689,706 | $0.02096 |
| Pi (base) | 22 Sep · b (s3) | 22/39 | 341,575 | $0.01486 |
| Pi (base) | 22 Sep · d (s5) | 30/39 | 478,303 | $0.01609 |
| Pi (base) | 22 Sep · f (s7) | 27/39 | 325,072 | $0.01500 |
| Pi (base) | 24 Sep · a (s8) | 25/39 | 454,394 | $0.01464 |
| SpecPi | 24 Sep · a (s8) | 30/38 | 382,362 | $0.01141 |
| SpecPi | 24 Sep · b (s9) | 32/39 | 431,828 | $0.01722 |
| Oh My Pi | 21 Sep (s1) | 59/78 | 1,320,404 | $0.02587 |
| Oh My Pi | 24 Sep · b (s9) | 25/38 | 1,122,766 | $0.02999 |
| [CC] | 21 Sep (s1) | 25/38 | 783,804 | $0.14130 (upper bound; no cache data) |
| [CC] | 22 Sep · a (s2) | 25/39 | 746,483 | $0.02734 |
| OpenCode | 22 Sep · f (s7) | 29/39 | 494,173 | $0.01243 |
| DeepSeek Harness | 22 Sep · f (s7) | 21/38 | 1,126,586 | $0.02261 |

Pi's sittings s4 (21/39) and s6 (32/39) exist but are **excluded** from the published pool —
one from each end, named in `excluded` rather than hidden (`scripts_tb2-metrics.mjs:112-116`).
They are not in the table above because they are not in the data file.

### 1.5 Per-task results — the 13 widened tasks

**Table A — solved/attempts** (widened slice, pooled across that harness's sittings).

| Task | Pi (base) | SpecPi | Oh My Pi | [CC] | OpenCode | DeepSeek Harness |
|---|---|---|---|---|---|---|
| `cobol-modernization` | 10/15 | 6/6 | 7/9 | 3/6 | 2/3 | 3/3 |
| `configure-git-webserver` | 10/15 | 3/6 | 3/9 | 2/5 | 3/3 | 1/3 |
| `extract-elf` | 10/15 | 5/6 | 7/9 | 3/6 | 3/3 | 2/3 |
| `fix-git` | 15/15 | 6/6 | 9/9 | 5/6 | 3/3 | 3/3 |
| `hf-model-inference` | 14/15 | 6/6 | 6/9 | 6/6 | 3/3 | 1/3 |
| `multi-source-data-merger` | 15/15 | 6/6 | 9/9 | 6/6 | 3/3 | 3/3 |
| `prove-plus-comm` | 13/15 | 6/6 | 9/9 | 6/6 | 3/3 | 3/3 |
| `pytorch-model-cli` | 3/15 | 0/6 | 1/9 | 3/6 | 0/3 | 1/3 |
| `raman-fitting` | 3/15 | 2/5 | 4/9 | 0/6 | 2/3 | 1/3 |
| `sanitize-git-repo` | 1/15 | 6/6 | 7/9 | 2/6 | 3/3 | 0/3 |
| `sqlite-with-gcov` | 14/15 | 6/6 | 7/9 | 6/6 | 0/3 | 0/2 |
| `tune-mjcf` | 11/15 | 4/6 | 7/9 | 2/6 | 1/3 | 0/3 |
| `vulnerable-secret` | 15/15 | 6/6 | 8/8 | 6/6 | 3/3 | 3/3 |

**Table B — mean prompt tokens / mean output tokens per attempt** (cumulative across an
attempt's model calls).

| Task | Pi (base) | SpecPi | Oh My Pi | [CC] | OpenCode | DeepSeek Harness |
|---|---|---|---|---|---|---|
| `cobol-modernization` | 1,263k / 46.9k | 1,280k / 41.1k | 6,870k / 96.7k | 2,898k / 129.2k | 1,190k / 38.0k | 2,891k / 47.9k |
| `configure-git-webserver` | 245k / 13.7k | 274k / 16.1k | 431k / 14.9k | 665k / 23.1k | 540k / 16.2k | 1,969k / 35.9k |
| `extract-elf` | 1,071k / 29.0k | 862k / 17.3k | 981k / 27.3k | 683k / 81.8k | 1,012k / 17.8k | 839k / 19.1k |
| `fix-git` | 47k / 2.8k | 62k / 3.2k | 414k / 10.8k | 235k / 6.7k | 98k / 1.9k | 151k / 5.8k |
| `hf-model-inference` | 30k / 2.5k | 53k / 2.9k | 228k / 5.1k | 154k / 2.5k | 145k / 3.2k | 157k / 5.3k |
| `multi-source-data-merger` | 62k / 6.3k | 46k / 5.4k | 190k / 9.4k | 116k / 5.4k | 87k / 5.9k | 119k / 8.3k |
| `prove-plus-comm` | 23k / 2.2k | 40k / 2.3k | 143k / 4.1k | 115k / 2.5k | 45k / 1.1k | 76k / 2.3k |
| `pytorch-model-cli` | 175k / 9.6k | 191k / 10.3k | 801k / 21.7k | 794k / 29.6k | 305k / 10.2k | 1,298k / 26.4k |
| `raman-fitting` | 1,209k / 60.0k | 1,562k / 61.3k | 2,261k / 86.1k | 1,198k / 110.3k | 1,897k / 54.8k | 1,279k / 33.3k |
| `sanitize-git-repo` | 1,226k / 20.0k | 470k / 9.2k | 1,074k / 18.3k | 882k / 12.5k | 615k / 11.5k | 1,636k / 20.4k |
| `sqlite-with-gcov` | 185k / 5.5k | 153k / 4.1k | 511k / 9.4k | 689k / 12.8k | 18k / 0.4k | 3,834k / 29.4k |
| `tune-mjcf` | 347k / 17.3k | 422k / 18.7k | 2,097k / 54.5k | 1,266k / 50.7k | 373k / 19.4k | 1,120k / 18.1k |
| `vulnerable-secret` | 70k / 3.2k | 74k / 2.4k | 206k / 3.2k | 232k / 3.9k | 100k / 2.1k | 178k / 2.7k |

**Table C — cost per attempt (USD, frozen price list) and cache-hit share.**

| Task | Pi (base) | SpecPi | Oh My Pi | [CC] | OpenCode | DeepSeek Harness |
|---|---|---|---|---|---|---|
| `cobol-modernization` | 0.0412 (95%) | 0.0373 (95%) | 0.1030 (98%) | 0.0833 (97%) | 0.0285 (99%) | 0.0564 (96%) |
| `configure-git-webserver` | 0.0119 (92%) | 0.0136 (92%) | 0.0127 (96%) | 0.0208 (95%) | 0.0126 (98%) | 0.0383 (96%) |
| `extract-elf` | 0.0321 (93%) | 0.0214 (93%) | 0.0277 (94%) | 0.0628 (94%) | 0.0220 (94%) | 0.0211 (94%) |
| `fix-git` | 0.0031 (81%) | 0.0035 (84%) | 0.0113 (94%) | 0.0094 (90%) | 0.0020 (96%) | 0.0062 (90%) |
| `hf-model-inference` | 0.0020 (89%) | 0.0028 (89%) | 0.0051 (96%) | 0.0031 (94%) | 0.0029 (97%) | 0.0051 (94%) |
| `multi-source-data-merger` | 0.0051 (88%) | 0.0044 (85%) | 0.0075 (95%) | 0.0061 (85%) | 0.0042 (97%) | 0.0069 (91%) |
| `prove-plus-comm` | 0.0018 (88%) | 0.0020 (91%) | 0.0043 (93%) | 0.0031 (87%) | 0.0012 (94%) | 0.0031 (87%) |
| `pytorch-model-cli` | 0.0084 (92%) | 0.0089 (92%) | 0.0193 (97%) | 0.0251 (97%) | 0.0081 (98%) | 0.0278 (96%) |
| `raman-fitting` | 0.0502 (94%) | 0.0602 (92%) | 0.0707 (96%) | 0.0667 (94%) | 0.0462 (97%) | 0.0354 (94%) |
| `sanitize-git-repo` | 0.0307 (92%) | 0.0146 (89%) | 0.0246 (93%) | 0.0184 (93%) | 0.0167 (91%) | 0.0300 (95%) |
| `sqlite-with-gcov` | 0.0063 (91%) | 0.0048 (92%) | 0.0103 (96%) | 0.0185 (96%) | 0.0004 (96%) | 0.0416 (98%) |
| `tune-mjcf` | 0.0151 (93%) | 0.0176 (92%) | 0.0501 (96%) | 0.0317 (97%) | 0.0140 (98%) | 0.0247 (94%) |
| `vulnerable-secret` | 0.0039 (82%) | 0.0031 (87%) | 0.0047 (93%) | 0.0066 (91%) | 0.0027 (92%) | 0.0036 (94%) |

The same numbers as machine-readable rows: `tb2-widened-matrix.csv` (task × harness, all fields
above), `tb2-sittings.csv`, `tb2-gitpair.csv`.

Rates: `deepseek-v4.1-flash`, frozen at `inputPerMTok` 0.15, `outputPerMTok` 0.6,
`cacheReadPerMTok` 0.003 (`evals/prices.json`, `pricedAt` 2026-09-17, mirrored at
`raw/specpi-repo/evals_prices.json`).

### 1.6 The git pair (top-up, not in the pooled totals)

7 attempts per task per harness in the 24 Sep sitting; SpecPi vs Pi Fisher p shown.

| Task | Pi | SpecPi | Oh My Pi | [CC] | OpenCode | DeepSeek Harness | p (SpecPi vs Pi) |
|---|---|---|---|---|---|---|---|
| `fix-git` | 10/10 | 10/10 | 7/7 | 7/7 | 7/7 | 7/7 | 1.00 |
| `sanitize-git-repo` | 3/10 | 10/10 | 4/7 | 0/7 | 5/7 | 6/7 | 0.0031 |

### 1.7 Errors, exclusions, dropped task (as published)

JSON `errors` (verbatim, four entries):

| Slice | Arm | Task | Kind |
|---|---|---|---|
| widen | `claude-code` | `configure-git-webserver` | `AgentSetupTimeoutError` |
| widen | `dsh` | `sqlite-with-gcov` | `NonZeroAgentExitCodeError` |
| widen | `specpi` | `raman-fitting` | `NetworkConnectionError` |
| widen | `omp` | `vulnerable-secret` | `NetworkConnectionError` |

These are attempts with no numeric reward; they are absent from the denominators, which is why
`[CC]` shows 2/5 and DSH 0/2 on those tasks rather than /6 and /3.

- **Dropped task** `pytorch-model-recovery` (`DROP_TASKS`, `scripts_tb2-metrics.mjs:189`):
  "Every arm's trials on this task produce zero tokens in about two seconds, which is the task's
  environment failing to build rather than four harnesses failing to solve it."
- **Excluded sittings**: Pi s4 (21/39, "lowest Pi sitting") and s6 (32/39, "highest Pi
  sitting"), one from each end so the trim moves the average in neither direction.
- **Oh My Pi's widened slice holds 78 trials rather than 39**: two launches of the same script
  raced into one output directory; both ran the same 13 tasks at the same settings, so all
  count — six attempts per task, measured at **twelve-way concurrency** (`scripts_tb2-metrics.mjs:191-199`).

### 1.8 Task-level notes the page gives

- `configure-git-webserver`: "only passes when a test file is left behind: its checker's own
  push never succeeds" (page, task-by-task section). Anyone reading 10/15 or 3/6 should know the
  pass condition is narrower than the task text suggests.
- `sanitize-git-repo` is the 0.33.0 "working agreement" target: every attempt from either arm
  that left Git history alone passed; every failure had rewritten it (CHANGELOG 0.33.0 entry).
- `[CC]` 21 Sep runs: cache count is zero in every trial — a measurement gap in the
  Messages/chat-completions translation, not a finding; cached share is `null` and those
  attempts are left out of `[CC]`'s cache and cost. `[CC]`'s own `cost_usd` is computed at
  Anthropic prices for a DeepSeek model (≈2 orders of magnitude high); all costs on the page are
  recomputed from logged tokens.
- OpenCode's output count leaves out reasoning; the page adds it back from `trajectory.json`
  (it was 2.6× OpenCode's recorded output on 22 Sep).
- DeepSeek Harness reports no usage of its own; its tokens are what a counting proxy inside the
  container saw leave.

### 1.9 Gaps in S1

Not published anywhere in S1: wall/agent time, tool-call counts, per-attempt trajectories or
per-attempt rows, per-task × per-sitting cells, the seven calibration task names, Harbor
version, dataset digest, reasoning/thinking effort, context/turn caps, agent kwargs, container
arch. Concurrency appears only as prose in the generator ("concurrency six each", "twelve-way").

---

## 2. Source S2 — SpecPi GitHub repo (method + generator)

| Item | Value |
|---|---|
| Repo | https://github.com/TannerMidd/SpecPi |
| Commit used | `af36b0a1d48e7f706d40e30413e3150b067f45e6` (committed 2026-10-03T13:16:27Z, "Pin specpi-lancet-guard 0.6.1 and prepare SpecPi 0.37.1") |
| Files mirrored | `raw/specpi-repo/` (15 files + the git tree listing), see `raw/MANIFEST.csv` |
| Key files | `scripts/tb2-metrics.mjs` (builds the JSON), `scripts/tb2-site.mjs` (renders the page), `evals/prices.json`, `evals/.env.example`, `CHANGELOG.md`, `site/evaluations/index.html` |

### 2.1 Published configuration

| Field | Value | Where |
|---|---|---|
| Model | `deepseek-v4.1-flash` | JSON `model`; page §06 |
| Endpoint | "one endpoint"; identity **not published for the TB2 runs**. The repo's eval tooling documents the OpenCode Go base `https://opencode.ai/zen/go/v1/chat/completions` and one key for every harness | page §06; `evals/.env.example:6-19` |
| Wire | chat-completions (the eval tooling's default; `EVAL_FORWARD_WIRE` empty) — the TB2 run configs are not published | `evals/.env.example:21-33` |
| Harbor | "run by Harbor on Docker" — **version not published** | page §06 |
| Execution host | runs root `F:/Development/tb-bench/runs` → Windows host **[INFERENCE from the path]**; arch not published | `scripts/tb2-metrics.mjs:48` |
| Attempts per task | 3 per sitting (widened), 5 (calibration), 7 (git pair) | JSON `slices` |
| Concurrency | 6 per launch; one slice at 12-way (raced launches) | `scripts/tb2-metrics.mjs:197` |
| Timeout multiplier | Default, except setup given **3× Harbor's default** for the 24 Sep git-pair runs | `scripts/tb2-metrics.mjs:91-93` |
| Reasoning/thinking effort | **Not published** (no mention in page, README, or generator) | — |
| Agent kwargs | **Not published** for TB2 (the tier-suite runner's kwargs are a different experiment) | — |
| Pi version | `piVersions` = `0.87.0`, `0.87.1` (whatever each trial reported; per-arm split not published) | JSON `piVersions` |
| SpecPi version | 0.33.0, Jev off | page §06; CHANGELOG 0.37.0 |
| OpenCode / Oh My Pi / [CC] versions | **Not published** | — |
| DeepSeek Harness version | pinned `0.1.5-rc.3` ("rc.2 — npm's `latest` — now resolves rc.3 sub-packages and fails to boot") | `scripts/tb2-metrics.mjs:77-80` |
| Task exclusions | `pytorch-model-recovery` dropped; four errored attempts; two Pi sittings excluded | §1.7 |
| Prices | `inputPerMTok` 0.15 / `outputPerMTok` 0.6 / `cacheReadPerMTok` 0.003, `pricedAt` 2026-09-17 | `evals/prices.json` |

### 2.2 Not in the repo

There are **no Harbor job configs, no launch scripts, and no run artifacts for TB2** in this
repository. `evals/runs/` holds only the harness's own tier-suite results (Jev calibration
replays), not Terminal-Bench. The generator reads `result.json` per trial from a runs root that
is deliberately not published:

> The runs are deliberately not in this repository. Terminal-Bench task content carries canary
> strings that are not meant to enter a training corpus, so only aggregates and public task
> names cross into the data file. — `scripts/tb2-metrics.mjs:7-10`

Consequence: **no per-attempt data, no trajectories, no wall time, no tool calls** for the
deepseek runs, and no way to recompute anything below the published aggregates.

---

## 3. Source S3 — cline/benchmark-results, `terminal-bench/2026-06-open-weights`

| Item | Value |
|---|---|
| Repo path | https://github.com/cline/benchmark-results/tree/main/terminal-bench/2026-06-open-weights |
| Commit used | `61c1e5a099cb3286b965dc9f114df2135e874b75` (2026-06-26) |
| Runs launched | 24–25 June 2026 |
| Files mirrored | `raw/cline/` — README, summary.{md,csv,json}, run-configs.{md,csv,json}, 15 × trial-results.json + manifest.json, provenance archive manifest, one LFS archive |

### 3.1 Configuration (as published)

- Benchmark: **full 89-task Terminal-Bench 2.0**, `terminal-bench@2.0`, task git commit
  `69671fbaac6d67a7ef0dfec016cc38a64ef7a77c` (`laude-institute/terminal-bench-2`).
- Scoring: **pass@1**, one attempt per task; `unscored` column counts trials whose `result.json`
  exposed no reward of 0.0 or 1.0 (1–2 per run).
- Harnesses: Cline CLI `3.0.29`, OpenCode `1.17.9`, Pi-Code `@mariozechner/pi-coding-agent`
  `0.73.1`. "These are not official benchmark numbers from OpenCode or Pi-Code."
- Execution: **Modal**, concurrency **n=20**, `--timeout-multiplier 2.0`, OpenRouter model routes.
- Agent kwargs: Cline `reasoning-effort=none|medium`, `setup-retries=3`,
  `setup-command-timeout-sec=240`; OpenCode `opencode_config=set`; Pi `thinking=medium`.
- Models: `glm-5.2`, `minimax-m3`, `mimo-v2.5-pro`, `kimi-k2.7-code` — **`deepseek-v4.1-flash`
  does not appear**, so this source gives no baseline on our model.
- Harbor version: **not published** (launch command is `python -m harbor.cli.main run …` from a
  source checkout at `/home/robin_cline_bot/harbor`).

### 3.2 Per-task results for our 13 tasks (different models — reference only)

Cells are **solved / runs**; each run is one attempt per task on one model.

| Task | cline-non (3 models) | cline-medium (4) | opencode-medium (4) | pi-medium (4) |
|---|---|---|---|---|
| `cobol-modernization` | 3/3 | 4/4 | 4/4 | 4/4 |
| `configure-git-webserver` | 2/3 | 2/4 | 3/4 | 2/4 |
| `extract-elf` | 3/3 | 3/4 | 2/4 | 3/4 |
| `fix-git` | 3/3 | 4/4 | 3/4 | 3/4 |
| `hf-model-inference` | 3/3 | 4/4 | 4/4 | 4/4 |
| `multi-source-data-merger` | 3/3 | 4/4 | 4/4 | 4/4 |
| `prove-plus-comm` | 3/3 | 4/4 | 4/4 | 4/4 |
| `pytorch-model-cli` | 2/3 | 3/4 | 3/4 | 3/4 |
| `raman-fitting` | 0/3 | 0/4 | 0/4 | 1/4 |
| `sanitize-git-repo` | 1/3 | 2/4 | 1/4 | 4/4 |
| `sqlite-with-gcov` | 3/3 | 4/4 | 3/4 | 3/4 |
| `tune-mjcf` | 3/3 | 4/4 | 4/4 | 4/4 |
| `vulnerable-secret` | 3/3 | 4/4 | 4/4 | 3/4 |

`pi-medium` detail (mean over its four models; one attempt per model per task):

| Task | Solved | Prompt tok | Output tok | Cost $ |
|---|---|---|---|---|
| `cobol-modernization` | 4/4 | 464,378 | 20,651 | 0.2058 |
| `configure-git-webserver` | 2/4 | 80,636 | 4,684 | 0.0376 |
| `extract-elf` | 3/4 | 229,887 | 13,582 | 0.1312 |
| `fix-git` | 3/4 | 30,774 | 2,243 | 0.0159 |
| `hf-model-inference` | 4/4 | 29,511 | 1,907 | 0.0154 |
| `multi-source-data-merger` | 4/4 | 24,647 | 3,418 | 0.0200 |
| `prove-plus-comm` | 4/4 | 21,137 | 1,679 | 0.0105 |
| `pytorch-model-cli` | 3/4 | 437,412 | 10,297 | 0.1420 |
| `raman-fitting` | 1/4 | 1,647,977 | 52,526 | 0.5585 |
| `sanitize-git-repo` | 4/4 | 816,725 | 5,524 | 0.2125 |
| `sqlite-with-gcov` | 3/4 | 205,119 | 3,787 | 0.0649 |
| `tune-mjcf` | 4/4 | 422,657 | 19,318 | 0.1896 |
| `vulnerable-secret` | 3/4 | 71,000 | 4,393 | 0.0381 |

All 195 rows (15 runs × 13 tasks) are in `cline-13tasks.csv`. Full-run scores (89 tasks) are in
`raw/cline/summary.md` — e.g. pi-medium glm-5.2 51/89, opencode-medium glm-5.2 53/89,
cline-medium glm-5.2 61/89.

### 3.3 Per-attempt data: available (for these models)

`runs/<group>/<model>/trial-results.json` gives one row per task with reward, `n_input_tokens`,
`n_output_tokens`, `cost_usd` — but `duration_sec` is **empty for every row** (`raw/cline/runs/*`).
The LFS archives carry the full per-trial records. One archive was downloaded and verified:

| Archive | Bytes | sha256 | Matches `summary.csv`? |
|---|---:|---|---|
| `archives/cline-non/glm-5.2.zip` | 10,265,789 | `02f2bcc719a5b5d688914ed42d91f14fac5543eba834784547686effa817a2fd` | yes |

Inside, per trial: `result.json` (reward, tokens incl. `n_cache_tokens`, `started_at`,
`finished_at`, `agent_execution` start/finish), `agent/trajectory.json`, `agent/*.txt`, and the
verifier output. Example, `cobol-modernization__pqC9bNV` (glm-5.2, reward 1.0):
1,438,430 prompt tokens (1,388,800 cached), 32,075 output, agent execution 26 min 37 s
(01:17:34 → 01:44:11 UTC). That is the only published wall-time evidence for any of our 13
tasks. The other fourteen archives are 10.3 MB–233.6 MB and were not downloaded; sizes and sha256 are in
`raw/cline/provenance-archive-manifest.csv`.

Tool-call counts are not tabulated anywhere, but `trajectory.json` steps make them countable per
trial for these models.

---

## 4. Source S4 — tbench.ai official leaderboard and scoring rules

| Item | Value |
|---|---|
| Leaderboard API | `POST https://ofhuhcpkvzjlejydnvyd.supabase.co/functions/v1/leaderboard-read` with `{"package":"terminal-bench/terminal-bench-2","name":"2-0"}` (the call the site's own JS makes) |
| Raw response | `raw/tbench/leaderboard-tb2.0.json` (117,567 B), fetched 2026-10-26T13:45:02Z |
| Also fetched | TB 2.1 (`leaderboard-tb2.1.json`, 22 rows), TB 3.0 (12 rows), TB 4.0 (35 rows) |
| Rows | 142 on TB 2.0; `status: "display"`, `n_trials: 0` — seeded display rows, not live trial sets |
| `deepseek-v4.1-flash` | **absent** from all four leaderboards |
| DeepSeek rows | `DeepSeek-V3.2` + `Terminus 2`, 2025-12-01, **39.6% ± 2.8** (rank 86) |
| Pi / SpecPi / Oh My Pi | **absent**; one `OpenCode` row exists (rank 63, 51.7% ± N/A, 2025-11-24, model key `opencode__claude opus 4.5`) |
| Dataset version | The site's benchmark table declares `datasetVersion: "latest"` for `2.0` and `runDataset: "terminal-bench/terminal-bench-2"`; the API row carries an opaque `dataset_version_ids` UUID. **No digest is published.** |

Reading note: in the copy of this API response we received, some display strings are redacted
(`model_display: "the model 4.5"`, `model_org: "[OI]"`), while `metadata.key` and
`model_providers` keep the real names. The absence of `deepseek-v4.1-flash` was checked against
`metadata.key`, `model_names`, `model_display` **and** `model_providers`, not display text alone.

### 4.1 Official scoring rule (verified in source, not inferred)

From `raw/tbench/leaderboard__src__leaderboard__core__metrics.py` at commit
`e67a2cb7db4d9795c212909b3395d8c22ad60aec`:

```python
def is_success(reward) -> bool:
    """A trial succeeds iff reward > 0 (errored trials have none -> failure)."""
    return bool(reward) and float(reward) > 0

# accuracy = successful / total trials, matching the Harbor importer
accuracy = 100.0 * successful / total if total else 0.0
```

- **Accuracy** is a micro-average over all trials (`successful / total`), not a mean of per-task
  rates.
- **pass@k** is reported beside it (`k = 2,3,4,5`) as the unbiased per-task estimator
  `1 − C(n−c,k)/C(n,k)`, averaged over tasks.
- Errored trials count as **reward 0** and are *not* excluded (`raw/tbench/leaderboard__SUBMIT.md`).
- 95% CI uses the per-task standard-error formula
  `s² = (1/n²) Σ p_i(1−p_i)/(k_i−1)`.

### 4.2 Submission constraints (`leaderboard/SUBMIT.md`)

- Run the **leaderboard dataset, unmodified**, at default execution settings:
  `timeout_multiplier` unset/`None` or `1.0`, no agent or verifier timeout overrides, no
  resource overrides. "CI rejects anything else."
- **Cover every task, ≥5 trials each**; errored trials count as reward 0.
- Upload jobs so trials are publicly readable; every rewarded trial needs an ATIF
  `trajectory_path` (a judge audits for reward hacking); disqualified trials are written as
  reward 0.

Note: the current `SUBMIT.md` says "Community submissions are currently closed for
Terminal-Bench 4.0. Only submissions run by the maintainers will be added to the leaderboard at
this time" — so the rules above are the last-published form of the rule set, not a currently open
door for any version. We did not find a TB 2.0-specific variant of these rules.

---

## 5. Condition differences vs our run

Our conditions come from this repo, not from a publication:
`bench/HARBOR_SETUP.md` (versions), `bench/PREREGISTRATION_TB2.md` (model, gateway, limits),
`bench/run_arm.sh` (flags), `bench/tb2-tasks.json` (dataset).

| Condition | SpecPi [S1/S2] | cline [S3] | tbench.ai [S4] | Our run | Status |
|---|---|---|---|---|---|
| Model | `deepseek-v4.1-flash` | glm-5.2 / minimax-m3 / mimo-v2.5-pro / kimi-k2.7-code | many, no deepseek-v4.1-flash | `deepseek-v4.1-flash` | **same as SpecPi**, differs from cline |
| Endpoint / provider | "one endpoint" (identity not published for TB2; the repo's eval harness documents the OpenCode Go base `opencode.ai/zen/go/v1/chat/completions`) | OpenRouter routes | varies per submitter | local sub2api gateway (`localhost:8080`) via metering proxy | **differs** (different serving stack, rate limits, cache behaviour) |
| Wire | not published for TB2 (repo default is chat-completions) | provider-native | varies | chat-completions ([OI]) | **unknown** |
| Harness versions | Pi 0.87.0/0.87.1; SpecPi 0.33.0; DSH 0.1.5-rc.3; OMP/OpenCode/[CC] not published | Cline 3.0.29, OpenCode 1.17.9, Pi 0.73.1 | varies | LumenCortex A/A+/A+fix builds; OpenCode 1.19.4 | **differs** (our OpenCode is 1.19.4 vs SpecPi's unpublished and cline's 1.17.9) |
| Harbor version | not published | not published (source checkout) | — | **0.25.0** | **unknown** |
| Dataset | "Terminal-Bench 2.0", 20 tasks (13 widened); digest not published | `terminal-bench@2.0`, 89 tasks, git `69671fb` | `terminal-bench/terminal-bench-2`, version "latest" | `terminal-bench@2.0`, 89 tasks downloaded, 13 run | **likely same dataset, unverified digest** |
| Task set | 13 widened (+7 calibration) | 89 | 89 | 13 widened | same 13 names as SpecPi |
| Attempts per task | 3 per sitting, pooled over up to 5 sittings (Pi: 15) | 1 (pass@1) | ≥5 required | **2** (`-k 2` in `run_arm.sh`) | **differs** |
| Scoring | share of attempts at reward 1 (micro-average) | pass@1 | accuracy = successes/total; pass@k reported | reward 1.0 per trial, pass rate + per-task pairing | same metric family as SpecPi |
| Timeout multiplier | default; setup 3× default on 24 Sep git pair | **2.0** | must be 1.0 | **2** | differs from official rules; same as cline |
| Other timeout overrides | none published | `setup-command-timeout-sec=240`, `setup-retries=3` | none allowed | provider 3600 s / idle 600 s (agent-side) | differs |
| Concurrency | 6 per launch (12 in one raced slice) | **20** | — | **1** | **differs** (ours is slowest, least contention) |
| Sandbox / host | Docker, Windows host [INFERENCE from `F:/…` path] | **Modal** | varies | Docker Desktop 29.8.2, macOS arm64, linux/arm64 server | **differs** |
| Arch / emulation | not published | Modal x86_64 (implied, not stated) | varies | **all TB2 images are amd64 → emulated on arm64** | **differs / unknown** |
| Resources | not published | Modal defaults | no overrides allowed | 10 CPU / 3.83 GiB VM | **unknown vs SpecPi** |
| Reasoning / thinking effort | **not published** | explicit: `none`/`medium` (Cline), `medium` (Pi), config (OpenCode) | recorded per submission | **not set** (gateway default; `reasoning_content` echoed) | **unknown** |
| Context / turn caps | not published | not published | — | 600K context, 32,768 output, `LCX_MAX_STEPS=150` | **unknown** |
| Token accounting | Harbor `n_input_tokens`/`n_output_tokens`, OpenCode reasoning added back | harness-reported per trial | per trial | metering proxy (prompt/completion/cached per request) | comparable in kind |
| Cost basis | frozen price list (0.15 / 0.6 / 0.003 per MTok) | harness `cost_usd` at OpenRouter prices | per submission | computed from measured tokens; no rate table is fixed in this repo yet | **differs / unknown** |

### 5.1 The five differences that matter most

1. **Host architecture and emulation.** Every TB2 image is amd64; our Docker Desktop is macOS
   arm64, so every task runs under QEMU emulation on a 10-CPU / 3.83 GiB VM. SpecPi ran Docker
   on a Windows host — `[INFERENCE]` from the runs root `F:/Development/tb-bench/runs`; its arch
   is unpublished, so "no emulation" is inferred, not stated — and cline ran on Modal (x86_64
   implied, not stated).
   Wall time and timeout exposure are therefore not comparable, and our `timeout_multiplier 2`
   buys less real work than theirs.
2. **Model endpoint.** SpecPi used one provider endpoint whose identity is not published (the
   repo's eval harness documents the OpenCode Go base, but the TB2 run configs are not in the
   repo); cline used OpenRouter; we use a local sub2api gateway in front of DeepSeek through a
   metering proxy. Same model id, different serving path — token accounting, cache-hit rate and
   latency all move with it.
3. **Attempts and pooling.** SpecPi pools 3 attempts per task per sitting across up to five
   sittings (Pi: 15 attempts/task, 195 total) and trims its two extreme sittings; we plan 2
   attempts per task in one sitting. With a published Pi spread of 22/39–30/39 between
   identical sittings, our per-task cells will be far noisier than theirs.
4. **Concurrency.** 1 (ours) vs 6 (SpecPi) vs 20 (cline). Ours avoids contention but multiplies
   wall time; SpecPi's one raced slice ran at 12-way and its timeouts may owe something to that.
5. **Unknown reasoning effort.** SpecPi publishes no thinking/effort setting for any harness;
   cline pins `none`/`medium`/`thinking=medium`; we set none (gateway default). If SpecPi's
   harnesses ran at a non-default effort, part of any gap is an effort difference, not a harness
   difference — and nothing published lets us rule that out.

Two further caveats worth carrying into the comparison: SpecPi's headline rates are
micro-averages over pooled attempts from *different hours* (their own Pi range is 56–77%), and
`[CC]`'s cost/cache figures cover only 39 of its 77 widened attempts.

---

## 6. Explicitly not published (do not guess these)

- SpecPi: Harbor version, dataset digest, Docker host arch, reasoning/thinking effort, agent
  kwargs, timeouts per phase (except "3× default setup" for the git pair), context/turn caps,
  wall time, tool-call counts, per-attempt records, calibration task names, per-task × per-sitting
  cells, Oh My Pi / OpenCode / [CC] versions.
- cline: Harbor version, wall time in the CSV (only inside the archives), tool-call counts,
  `deepseek-v4.1-flash` results.
- tbench.ai: any `deepseek-v4.1-flash` row; per-trial data for display rows (`n_trials: 0`).

---

## 7. Files

Raw downloads (byte-for-byte) and their URL, fetch time (UTC) and sha256:
`bench/public/raw/MANIFEST.csv` (66 files). Highlights:

| File | Bytes | sha256 |
|---|---:|---|
| `raw/specpi-site/terminal-bench-2.json` | 132,076 | `052fb135166cefd66a5106764288ed0d9f7204b9faa18523907349bc6f843655` |
| `raw/specpi-site/evaluations-index.html` | 58,489 | `1bfa325a90a6f89d…` (see manifest) |
| `raw/specpi-repo/scripts_tb2-metrics.mjs` | 30,497 | `dff4ae0a4eedbf88…` |
| `raw/tbench/leaderboard-tb2.0.json` | 117,567 | `dd426eef8d24974e…` |
| `raw/cline/archives/cline-non__glm-5.2.zip` | 10,265,789 | `02f2bcc719a5b5d688914ed42d91f14fac5543eba834784547686effa817a2fd` |

Derived tables (regenerated from the raw JSON, no hand edits):
`tb2-widened-matrix.csv`, `tb2-sittings.csv`, `tb2-gitpair.csv`, `cline-13tasks.csv`.

Reproduce:

```sh
# SpecPi per-task matrix
python3 - <<'EOF'
import json, csv
d = json.load(open('bench/public/raw/specpi-site/terminal-bench-2.json'))
for t in d['tasks']:
    for h, b in t['byHarness'].items():
        print(t['task'], h, b['solved'], b['attempts'], b['inputTokens'], b['outputTokens'], b['cost'])
EOF

# cline per-task rows for the 13 tasks
python3 - <<'EOF'
import json, glob, os
mine = {'cobol-modernization','configure-git-webserver','extract-elf','fix-git','hf-model-inference',
        'multi-source-data-merger','prove-plus-comm','pytorch-model-cli','raman-fitting',
        'sanitize-git-repo','sqlite-with-gcov','tune-mjcf','vulnerable-secret'}
for f in sorted(glob.glob('bench/public/raw/cline/runs/*__trial-results.json')):
    for r in json.load(open(f)):
        if r['task'] in mine:
            print(os.path.basename(f), r['task'], r['reward'], r['n_input_tokens'], r['n_output_tokens'])
EOF
```
