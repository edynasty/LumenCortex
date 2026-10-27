# TB2 dual-baseline results — <date> (template)

Fill every `<…>` from command output; paste the command next to each table. A cell that was not run
says `NOT RUN`; a value that could not be measured says `n/a` with the reason. Plan:
`bench/EVAL_PLAN_TB2_DUAL_BASELINE.md`; pre-registration hash file:
`bench/PREREGISTRATION_TB2_CC.sha256` (verified after the runs: `<shasum -c output>`).

## 0. Status

| Item | Value |
|---|---|
| Owner authorization | `<decision file, date, items A–D>` |
| Benchmark status | `<RUN / PARTIAL (which sittings) / NOT RUN>` |
| Sittings completed | `<n of 5>`; extension `<none / arms, k>` |
| Deviations from the plan | `<none / each one, with the reason and the affected trials>` |

## 1. Environment record

| Item | Value |
|---|---|
| Harbor / Docker / host | `<harbor --version>` / `<docker version>` / `<uname -a, VM memory>` |
| Task images | `<digest list file>` |
| Route | proxy `127.0.0.1:18080` → sub2api `<image tag>` → `<upstream endpoint from usage_logs>` |
| Binaries | `lcx-go-post <sha256> (Core <SHA>)`, `lcx-go-Aplus <sha256> (Core 4968ea8)` |
| Built-in versions | Pi `<pi --version>`, OpenCode `<version>`, SpecPi `<version>` (from the G2 smoke logs) |
| Parameters actually sent, per arm | `<analyze_tb2.py requestParams>` |
| Gates G1–G4 | `<result and evidence per gate>` |

## 2. Controlled Comparison — per arm (raw view first)

Command: `python3 analysis/analyze_tb2.py --arm <arm> jobs/cc-<arm>-s{1..5} --rates peak --exclude-task pytorch-model-cli --json out/<arm>.json`

| Arm | Trials | Solved | Success (95% CI) | Pass@1 | Pass@3 | Pass@5 | Env-valid success (n, excluded) | Infra rate | Timeouts |
|---|---:|---:|---|---:|---:|---:|---|---:|---:|
| LCpost | | | | | | | | | |
| LCpre | | | | | | | | | |
| pi | | | | | | | | | |
| opencode | | | | | | | | | |
| specpi | | | | | | | | | |

Same table with `pytorch-model-cli` excluded (`primary`): `<…>`.

| Arm | Prompt tok | Cached tok | Completion tok | Token source | Cost peak / off-peak (USD) | Wall time sum / median / max | Tool failure rate | Trials with cut stream / length final / DSML in content |
|---|---:|---:|---:|---|---:|---|---:|---|
| LCpost | | | | | | | | |
| LCpre | | | | | | | | |
| pi | | | | | | | n/a (`<reason>`) | |
| opencode | | | | | | | n/a (`<reason>`) | |
| specpi | | | | | | | n/a (`<reason>`) | |

## 3. Per task

| Task | LCpost | LCpre | pi | opencode | specpi | Published: Pi / SpecPi / OMP / OpenCode (Baseline B, other conditions) |
|---|---|---|---|---|---|---|
| `<task>` | `<s/n>` | | | | | `<from PUBLIC_BASELINES.md §1.5>` |

## 4. Paired comparisons

Command: `python3 analysis/compare_tb2.py out/<a>.json out/<b>.json [--env-valid]`

| Pair (b − a) | Shared tasks | Mean task rate a / b | Difference | 95% CI (task bootstrap) | Tasks b better / a better |
|---|---:|---|---:|---|---|
| LCpost − LCpre (Q1) | | | | | |
| LCpost − pi | | | | | |
| LCpost − opencode | | | | | |
| LCpost − specpi | | | | | |

Statement rule: a difference whose interval contains 0 is reported as "not distinguishable at this
sample size", never as a ranking.

## 5. Failure attribution

| Arm | SOLVED | FAILED (task) | FAILED (harness, named) | Model/upstream output (flags) | Provider service (INFRA_PROVIDER, cut streams) | Harbor/environment (INFRA_VERIFIER, HARBOR) | TIMEOUT |
|---|---:|---:|---:|---:|---:|---:|---:|
| `<arm>` | | | | | | | |

Harness-attributed failures, one line each: `<trial> — <what the harness did with which response> — <evidence path>`.
INFRA reruns: `<trial> → <rerun trial>: <reward>`.

## 6. DSML diagnostic (plan §7)

| Stage | Cell | Units × reps | DSML in reasoning (DSML units / controls) | content == reasoning | Leaked tokens | Tool call parsed | Finish length |
|---|---|---|---|---|---|---|---|
| 1 | G0 | | | | | | |
| 1 | G1 | | | | | | |
| 2 | `<cell>` | `<or NOT RUN: trigger not met>` | | | | | |

Decision taken under the plan's fixed rules: `<route / model / not reproducible>` — `<evidence>`.

## 7. Published Reference (unchanged conditions; not a ranking)

Baseline A: `<DeepSeek numbers quoted with condition ledger, Baseline A §3.3>`.
Baseline B: `<SpecPi rows with attempts, sittings, versions>`.
Differences that remain between the controlled run and each published row: `<list>`.

## 8. Conclusions and limits

* Q1: `<…>`  · Q2: `<…>`  · Q3: `<…>`
* What this run cannot show: `<…>`
* Checks run for this report: `<commands>`; checks not run: `<…>`.
