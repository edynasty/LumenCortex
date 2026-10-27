# Evaluation plan — Terminal-Bench 2.0, dual external baseline (NOT RUN)

Written 2026-10-27. **Benchmark Status: NOT RUN.** No trial, replay or model request described here
has been executed. Every run needs the owner authorization in §8; the scripts refuse to start without
`EVAL_AUTHORIZED=1`.

Companion documents (facts are not repeated here):

| Document | Role |
|---|---|
| `bench/baselines/BASELINE_A_DEEPSEEK_OFFICIAL.md` | Baseline A: DeepSeek's own numbers, model identity, sampling/thinking/DSML semantics |
| `bench/baselines/BASELINE_B_SPECPI_HARNESS.md` | Baseline B: SpecPi's harness rows, fairness table, controlled-comparison recipe (§5) |
| `bench/public/PUBLIC_BASELINES.md` | Raw public data for the 13 tasks (SpecPi, cline, tbench.ai) |
| `bench/PREREGISTRATION_TB2_W13.md`, `bench/HARBOR_REMEDIATION_REPORT_2026-10-27.md` | LC history (W13A/B, reruns) |
| `bench/HARNESS_INVESTIGATION_2026-10-27.md` | Why the LC arm changes (fixes) and what the DSML evidence is |
| `bench/RESULTS_TEMPLATE_TB2_DUAL_BASELINE.md` | The report to fill in after the runs |

## 1. Questions

1. **Q1 — LC regression.** Do the harness fixes (`cog/harness-fix`, merged into `cog/r1a`; §3.2) change LC's outcomes, measured
   against the W13 binary in the same sittings?
2. **Q2 — Harness comparison.** Under one model, endpoint, task set, resource and scoring condition,
   how do LC, Pi, OpenCode and SpecPi compare?
3. **Q3 — DSML attribution.** Is the DSML-in-reasoning / reasoning-as-content pattern caused by the
   serving route, by sampling (top_p, effort, output cap), or by the model itself? (§7)

Not a question here: whether LC reaches DeepSeek's or SpecPi's published numbers. Those are reference
values with different conditions (Baseline A §5, Baseline B §3); they are never an acceptance
threshold.

## 2. Three result views, never mixed

| View | Content | Rule |
|---|---|---|
| **Published Reference** | Baseline A (DeepSeek TB 2.1 / TB 4.0 numbers), Baseline B (SpecPi widened rows: Pi 134/195, SpecPi 62/77, OMP 84/116, [CC] 50/77, OpenCode 29/39, DSH 21/38) | Quoted with their own conditions; no ranking against LC |
| **Controlled Comparison** | §3–§6 of this plan | The only view that supports a harness-vs-harness statement |
| **LC history** | W13A 11/24, W13B 7/15 (raw), reruns per the remediation report | Regression reference for LC only; not a third external baseline |

## 3. Arms and fixed conditions

### 3.1 Arms

| Arm | Agent import | Version pin | Requests attributed by |
|---|---|---|---|
| `LCpost` | `lcx_eval:LcxPost` | Core `50aa96d` (`cog/harness-fix`, merged into `cog/r1a`), binary `lcx-go-post` | `X-Exp-Tag: LCpost/<trial>` |
| `LCpre` | `lcx_eval:LcxPre` | Core `4968ea8`, the W13 binary `lcx-go-Aplus` | `X-Exp-Tag: LCpre/<trial>` |
| `pi` | Harbor built-in `pi` | Pi 0.87.1 (`--ak version=0.87.1`) | proxy tag file `pi/<job>` + time window |
| `opencode` | `opencode_local:OpenCodeLocal` | OpenCode 1.19.4 (offline binary, watcher off) | tag file + time window |
| `specpi` | `specpi_agent:SpecPi033` | SpecPi 0.33.0 on Pi 0.87.1 | tag file + time window |
| `omp` (optional) | — | OMP 18.2.5; needs its own adapter (Baseline B §5.4) | not planned in this round |

`LCpre` is re-run in the same sittings instead of reusing W13: the gateway and its account changed
behaviour between days (W13A had 4 burst failures, W13B one), so only a same-sitting control separates
the fixes from day-to-day provider drift. `[CC]` is excluded: it speaks Anthropic Messages only and the
route is Chat Completions (Baseline B §5.7 #10).

### 3.2 Building `lcx-go-post` (before gate G1)

From a clean tree at the exact commit, the same way as the W13 binaries (static linux/amd64):

```sh
cd /Users/tangxingpeng/IdeaProjects/me/LumenCortex
git -C LumenCortex-Core worktree add --detach /tmp/lcx-post 50aa96d
(cd /tmp/lcx-post && test -z "$(git status --porcelain)" && \
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o ../lcx-go-post ./cmd/lcx-go)
mv /tmp/lcx-go-post bench/build/bin/lcx-go-post && shasum -a 256 bench/build/bin/lcx-go-post
git -C LumenCortex-Core worktree remove /tmp/lcx-post
```

`50aa96d` is the last code commit of `cog/harness-fix` (`da9c5bd` cut streams, `ff19b89` output_limit,
`50aa96d` argumentless last call under output_limit). The branch was merged into `cog/r1a` with only
documentation and `benchmarks/tb2/` added after it, so the merge builds the same binary; if code changes
before the run, build from the new SHA and record it. The arm is defined by the commit, not by the branch name.

### 3.3 Fixed conditions (identical for every arm)

Taken from `run_w13.sh` / Baseline B §5.0; `bench/run_controlled.sh` encodes them.

| Condition | Value |
|---|---|
| Dataset | local export of `terminal-bench@2.0` (`./tb2/terminal-bench`), images pre-pulled and digest-checked |
| Tasks | the 13 in `bench/tb2-widened13.txt` |
| Model id / route | `deepseek-v4.1-flash` via metering proxy `127.0.0.1:18080` → local sub2api `:8080` (§3.4) |
| Wire | OpenAI Chat Completions, streaming |
| Concurrency | `-n 1` (Docker VM 3.83 GiB; tag-file attribution needs it) |
| Timeout | `--timeout-multiplier 2` |
| Network | in-container `http(s)_proxy=host.docker.internal:7897`, `no_proxy` for the proxy host |
| Harbor | 0.25.0 |
| Scoring | Harbor's verifier reward; solved = `reward >= 1` |

### 3.4 What is *not* equalized, and is recorded instead

* **Harness-owned settings** — prompts, tools, context handling, step/turn caps, output caps and
  reasoning effort stay at each harness's defaults (LC: 600K context / 32,768 output / 150 steps / no
  effort sent). Equalizing them would remove the differences being measured (Baseline B §5.8).
  The parameters each arm actually sent are read from the stored request bodies
  (`analyze_tb2.py` → `requestParams`), not assumed.
* **Serving route.** Gateway records (`usage_logs`, account 42) show every `deepseek-v4.1-flash`
  request of W13 left the local sub2api as `/v1/chat/completions` → upstream `/v1/responses` on
  `https://ai.edynasty.asia`; what serves it behind that host is not visible from here. The local
  bridge passes `temperature`/`top_p` through for non-GPT models
  (`apicompat/chatcompletions_to_responses.go:53-56`). The route is held fixed for all arms and
  disclosed; Q3 tests it.
* **Model checkpoint.** `deepseek-v4.1-flash` is a gateway alias, not a DeepSeek API id (Baseline A
  §2.1). Whether it resolves to -1017 is not observable from the client.

## 4. Metrics

| Metric | Definition | Source |
|---|---|---|
| Task Success Rate | solved / all trials, every trial in the denominator (raw view), Wilson 95% CI | Harbor `result.json` |
| Env-valid success | solved / trials not classed INFRA_VERIFIER / INFRA_PROVIDER; denominator and exclusions printed next to it | same + §6.1 classes |
| Pass@1 | mean over tasks of per-task solved/attempts | same |
| Pass@k | per task `1 - C(n-c,k)/C(n,k)` (unbiased estimator, Chen et al. 2021), averaged over tasks; k ≤ attempts per task | same |
| Tokens | prompt, cached, completion summed over 200 responses | proxy `usage` (header/tag-file matched); Harbor `agent_result` otherwise, labelled `tokenSource` |
| Total cost | tokens × deepseek-flash list price (peak and off-peak, Baseline A S14); gateway billing reported separately if available | `analyze_tb2.py --rates` |
| Wall time | trial `started_at`→`finished_at`; agent execution separately | Harbor `result.json` |
| Tool Failure Rate | non-zero tool results / tool calls | LC: Core state via `collect_w13.py`; other arms: n/a unless their own logs expose it (stated, not imputed) |
| Protocol Failure Rate | trials with ≥1 cut stream, `length` final, or DSML in content, over metered trials; cut streams / streams | proxy stream bodies |
| Infrastructure Failure Rate | trials classed INFRA_VERIFIER, INFRA_PROVIDER or HARBOR / all trials | §6.1 |

## 5. Execution

### 5.1 Gates before the first counted trial

| Gate | Check | Counts toward results? |
|---|---|---|
| G0 | Owner authorization (§8) recorded in a dated decision file | — |
| G1 | `lcx-go-post` built per §3.2, sha256 recorded; `lcx-go-Aplus` sha256 equals the W13 record | — |
| G2 | One install-and-run smoke per built-in arm on `fix-git` (`-k 1`, job `smoke-<arm>`): Pi and SpecPi install over the in-container proxy, SpecPi's two unverified installer behaviours (Baseline B §5.3) checked in the trial log, requests appear in `proxy.jsonl` under the tag file | No — smoke jobs are excluded by name |
| G3 | Oracle pass over the 13 tasks (`--agent oracle`, no model): every task reward 1; a task that fails here is an environment defect, fixed or removed **before** any arm runs | No |
| G4 | Pre-registration: this plan, `run_controlled.sh`, the adapters, both binaries and `analyze_tb2.py` hashed into `bench/PREREGISTRATION_TB2_CC.sha256` | — |

### 5.2 Size

k = 5 attempts per task per arm: 13 × 5 = 65 trials per arm, 325 for five arms.

Why 5: a same-data bootstrap on W13A (8 tasks × 3) gives a ±0.25 interval for a between-arm difference
(`compare_tb2.py`); scaling by tasks and attempts, 13 × 5 should give roughly ±0.15 [INFERENCE], enough
to see a large harness gap or a regression from the fixes, not a 5-point one. If a decision depends on
a difference inside that interval, extend only the arms involved to k = 10 (±0.11 [INFERENCE]) in
further sittings, pre-registered before they start.

### 5.3 Sittings

One sitting = each arm once over the 13 tasks (`-k 1`), so provider state on a given day hits all arms.
Five sittings, arm order rotated:

| Sitting | Order |
|---|---|
| 1 | LCpost LCpre pi opencode specpi |
| 2 | LCpre pi opencode specpi LCpost |
| 3 | pi opencode specpi LCpost LCpre |
| 4 | opencode specpi LCpost LCpre pi |
| 5 | specpi LCpost LCpre pi opencode |

```sh
cd bench
EVAL_AUTHORIZED=1 ./run_controlled.sh 1 LCpost LCpre pi opencode specpi
```

### 5.4 Infra rule and stop rules

* INFRA trials (§6.1) are re-run **once** each, in a separate job `cc-<arm>-s<n>-r`, after the sitting;
  the original trial stays in the raw view, the rerun only in the env-valid view (W13 rule).
* Pause the sitting (finish the running trial, start nothing new) when: the gateway returns non-200 on
  more than 20% of requests in the last 30 minutes; three consecutive trials are INFRA; or the Docker VM
  reports OOM. Resume only after the cause is logged in the execution log.
* Never edit tasks, verifiers, scorers or timeouts between sittings. A changed binary, adapter or
  route is a new arm with a new name.

### 5.5 Time and cost estimate

Planning figures from W13 (39 LC trials): 658K prompt (542K cached) and 24.7K completion tokens, and
8.7 minutes wall time, per trial; $0.068 per trial at deepseek-flash peak list price ($0.034
off-peak). Other harnesses' token use is unknown; the band below allows 0.5–2× LC's.

| Item | Trials / requests | Tokens (LC-like) | Cost at list price (peak) | Wall time (`-n 1`) |
|---|---:|---:|---:|---:|
| Controlled comparison, k = 5 | 325 trials | 214M prompt (176M cached), 8.0M completion | ≈ $22 (band $11–44) | ≈ 47 h (≈ 9.4 h per sitting) |
| Extension to k = 10 (two arms) | +130 | +86M / +3.2M | ≈ +$9 | ≈ +19 h |
| G2 smoke | 3 trials | — | < $0.3 | < 1 h |
| DSML stage 1 (§7) | 240 requests | 10.6M prompt, 1.7M completion | ≤ $5.25 | ≈ 2–4 h |
| DSML stage 2 (conditional) | 240 requests | 10.7M / 3.1M | ≤ $6.96 | ≈ 3–6 h |

The route bills through the owner's gateway, so list price is a comparable yardstick, not the invoice.

## 6. Analysis

### 6.1 Classes (one per trial; `analyze_tb2.py`)

| Class | Rule | Attribution |
|---|---|---|
| SOLVED | reward ≥ 1 | — |
| INFRA_VERIFIER | verifier ran no test and its log shows a download/DNS/502 failure | Harbor/network environment |
| INFRA_PROVIDER | agent exited 1 and its last 3 requests were non-200 | provider service |
| TIMEOUT | Harbor `AgentTimeoutError` | kept as failure; not excluded (could be harness or emulation speed) |
| HARBOR | any other Harbor exception | Harbor environment; inspected one by one |
| FAILED | reward < 1, none of the above | task failure; sub-attributed with the flags below |

Flags on any trial (metered streams only): `stream_cut` (provider service), `length_final` (model ran
out of output budget; with LCpost the run now ends `output_limit`), `dsml_in_content` (upstream
model/route, §7). A FAILED trial with none of the flags is a task failure. A FAILED trial where the
harness mishandled a well-formed response is a **harness** failure; it is identified by reading the
transcript, listed by name in the report, and never inferred from the flags alone.

### 6.2 Per-arm summary

```sh
python3 analysis/analyze_tb2.py --arm LCpost jobs/cc-LCpost-s{1,2,3,4,5} --rates peak \
  --exclude-task pytorch-model-cli --json out/LCpost.json
# same for LCpre, pi, opencode, specpi; LC arms first run collect_w13.py per job for tool metrics
```

The `all` view is the headline; `primary` (with `pytorch-model-cli` dropped as ENV, as in W13) is
shown next to it, never instead of it.

### 6.3 Comparisons

```sh
python3 analysis/compare_tb2.py out/LCpre.json out/LCpost.json             # Q1
python3 analysis/compare_tb2.py out/pi.json out/LCpost.json                # Q2, one pair at a time
python3 analysis/compare_tb2.py out/pi.json out/LCpost.json --env-valid    # secondary view
```

Task is the unit (attempts within a task are correlated): the difference is the mean over shared
tasks of the per-task rate difference, with a task-clustered bootstrap 95% interval. Report every
pair's interval; no ranking is stated where intervals overlap zero. Arms with different attempt counts
are compared only through per-task rates and are labelled as such.

## 7. DSML / temperature diagnostic (separate from §5; NOT RUN)

### 7.1 What is already established offline (no request sent)

From the 1,074 successful metered W13 streams of arm A+ (`bench/HARNESS_INVESTIGATION_2026-10-27.md`
§2): DSML markup appears in 20 responses, always in `reasoning_content`, never in `tool_calls`
arguments; the 8 with DSML in `content` have `content` byte-identical to `reasoning_content` (what
sub2api's reasoning-only fallback produces; which gateway hop applied it is not observable from here),
6 of them `finish_reason=length`, 2 `stop`. 11 of the 20 still
ended with a parsed tool call; 1 was a cut stream. Leaked special tokens (`<｜begin▁of▁sentence｜>`,
literal `</thinking>`, doubled `｜｜DSML｜｜`) occur in 14 of them.

### 7.2 Why there is no temperature matrix

DeepSeek documents `temperature` as having no effect in thinking mode, which is the default (Baseline A
§4.2: S12, S13). LC sends neither `temperature` nor `top_p` (stored request bodies:
`{"max_tokens": 32768, "model": "deepseek-v4.1-flash"}` in all 39 W13 trials) and has no config knob
for either. A temperature sweep in thinking mode would spend money to re-test a documented no-op, so it
is not planned. `top_p` *is* live in thinking mode (effective 0.95–1.0) and is in stage 2.

### 7.3 Design

Units: the 20 recorded DSML requests and 20 controls (same trial, closest prompt size, clean tool-call
response), replayed byte-for-byte except the fields a cell sets (`bench/analysis/dsml_replay.py`; dry
run prints the selection and cost). 3 repetitions per unit and cell, no retries.

| Stage | Cells | Requests | Decides |
|---|---|---:|---|
| 1 | G0 = current route as recorded; G1 = DeepSeek official API, `deepseek-flash`, same body | 240 | Route effect: does G1 reproduce DSML in reasoning, leaked tokens, and `content == reasoning_content`? |
| 2 (only if G1 shows DSML in reasoning on ≥ 3 of the 20 DSML units) | G1 × {top_p 0.95, effort max, both, effort max + max_tokens 65,536} | 240 | Whether a documented, live parameter reduces it |

Outcome measures per response: DSML in reasoning / content, `content == reasoning_content`, leaked
special tokens, finish reason, parsed tool calls, reasoning length.

Decision rules, fixed now:

* G0 shows the pattern and G1 does not (difference in DSML-unit reproduction rate ≥ 0.3) → route
  problem: the evaluation route needs the gateway fallback off or a CC-native path before §5 results
  are interpreted; LC unchanged.
* Both show it at similar rates → model behaviour under these prompts; stage 2 decides whether
  `top_p`/effort mitigate it; any such setting becomes per-model configuration, never a harness
  default.
* Neither reproduces it (< 3 of 20 on both) → not reproducible by replay; record as such and stop.

## 8. Authorization required (owner)

O-6, as cited by `bench/PREREGISTRATION_TB2_W13.md`, covers real-model runs of LC arm A+ (`4968ea8`);
that preregistration states the other harnesses are not re-run. Everything below is therefore outside
existing authority:

| Item | Scope | Budget at list price |
|---|---|---|
| A | Controlled comparison, 5 arms × 65 trials, incl. new LC SHA and Pi/OpenCode/SpecPi through the owner's gateway; G2 smoke | ≈ $22 (band to $44) |
| B | DSML stage 1, incl. a DeepSeek official API key for G1 | ≤ $5.25 |
| C | DSML stage 2, only if §7.3's trigger holds | ≤ $6.96 |
| D | Optional k = 10 extension, named arms only | ≈ +$9 |
