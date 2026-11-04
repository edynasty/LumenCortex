# Baseline A — DeepSeek's own published numbers and documented semantics (V4.1-Flash / -1017)

Compiled 2026-10-27. Research/documentation only: nothing here was produced by running Harbor, any
benchmark, or any model request. Every number below is quoted from a file downloaded byte-for-byte
under `bench/baselines/raw/deepseek/` (index: `bench/baselines/raw/deepseek/MANIFEST.csv`, with URL,
UTC fetch time and sha256 per file). Where a figure does not exist in any public source it is written
**not published**, together with where it was looked for.

This document answers three questions for the LC Terminal-Bench 2.0 evaluation:

1. What does DeepSeek publish about the model our adapter calls (`deepseek-v4.1-flash`), and which
   checkpoint does that id correspond to?
2. What does DeepSeek document about sampling (temperature / top_p) in thinking mode, given that our
   adapter sends neither?
3. Can DeepSeek's own Terminal-Bench number be reproduced with our setup, and what exactly blocks it?

DeepSeek publishes **no Terminal-Bench 2.0 number at all** for any V4.x model (see §3.4), so Baseline
A is by construction a reference point about the model, not a peer measurement on our task set.

---

## 1. Sources

Everything in this document traces to one of these. `raw/…` paths are relative to
`bench/baselines/raw/deepseek/`; hashes and byte counts are in `MANIFEST.csv`.

| # | What it is | URL | Revision | Raw file |
|---|---|---|---|---|
| S1 | Model card, DeepSeek-V4.1-Flash-1017 | `https://huggingface.co/deepseek-ai/DeepSeek-V4.1-Flash-1017` (raw: `…/raw/1bae0e1088993b9b82de08f59de5be5a34e42170/README.md`) | `1bae0e1088993b9b82de08f59de5be5a34e42170` (repo `lastModified` 2026-10-17T05:46:55Z) | `raw/README.md` |
| S2 | HF model metadata (sha, file list, config summary) | `https://huggingface.co/api/models/deepseek-ai/DeepSeek-V4.1-Flash-1017` | same sha | `raw/hf-api-model.json` |
| S3 | HF commit history for the -1017 repo | `…/api/models/deepseek-ai/DeepSeek-V4.1-Flash-1017/commits/main` | 4 commits | `raw/hf-api-commits.json` |
| S4 | `config.json` (text_config, quantisation, max_position_embeddings) | `…/raw/1bae0e10…/config.json` | same sha | `raw/config.json` |
| S5 | `tokenizer_config.json` (no `chat_template` key) | `…/raw/1bae0e10…/tokenizer_config.json` | same sha | `raw/tokenizer_config.json` |
| S6 | Prompt-format reference README (DSML, thinking, effort, tool calls) | `…/raw/1bae0e10…/encoding/README.md` | same sha | `raw/encoding_README.md` |
| S7 | `encoding.py`, the reference encoder/parser | `…/raw/1bae0e10…/encoding/encoding.py` | same sha | `raw/encoding.py` |
| S8 | Model card of the **base** DeepSeek-V4.1-Flash (TB 2.1 tables) | `https://huggingface.co/deepseek-ai/DeepSeek-V4.1-Flash` | `314113f8a7dadb388215396ba5fbee2c610a04d6` (lastModified 2026-10-16T09:27:28Z) | `raw/v41_README.md` |
| S9 | DeepSeek-V4.1-Flash technical report (PDF, Sep 2026) | `…/DeepSeek-V4.1-Flash/resolve/314113f8…/DeepSeek_V41_Tech_Report.pdf` | same sha | `raw/DeepSeek_V41_Tech_Report.pdf` |
| S10 | HF commit history, base repo | `…/api/models/deepseek-ai/DeepSeek-V4.1-Flash/commits/main` | 17 commits | `raw/hf-api-commits-v41flash.json` |
| S11 | HF model listing for org `deepseek-ai` | `https://huggingface.co/api/models?author=deepseek-ai&limit=100` | live at fetch | `raw/hf-api-author-models.json` |
| S12 | API docs — Thinking Mode | `https://api-docs.deepseek.com/guides/thinking_mode` | live at fetch | `raw/ds-guides_thinking_mode.html` |
| S13 | API docs — Chat Completions reference (parameter table) | `https://api-docs.deepseek.com/api/create-chat-completion` | live at fetch | `raw/ds-api_create-chat-completion.html` |
| S14 | API docs — Models & Pricing (model ids, context, max output) | `https://api-docs.deepseek.com/quick_start/pricing` | live at fetch | `raw/ds-quick_start_pricing.html` |
| S15 | API docs — Change Log (2026-09-10, 2026-10-17 entries) | `https://api-docs.deepseek.com/updates` | live at fetch | `raw/ds-updates.html` |
| S16 | API docs — release notes 2026-10-17 | `https://api-docs.deepseek.com/news/news261017` | live at fetch | `raw/ds-news_news261017.html` |
| S17 | API docs — release notes 2026-09-10 (V4.1-Flash launch) | `https://api-docs.deepseek.com/news/news260910` | live at fetch | `raw/ds-news_news260910.html` |
| S18 | API docs — Tool Calls (incl. `strict` mode) | `https://api-docs.deepseek.com/guides/tool_calls` | live at fetch | `raw/ds-guides_tool_calls.html` |
| S19 | API docs — Multi-turn conversation (`reasoning_content` handling) | `https://api-docs.deepseek.com/guides/multi_round_chat` | live at fetch | `raw/ds-guides_multi_round_chat.html` |
| S20 | `deepseek-recipe` README (max_tokens defaults, accepted model names) | `https://raw.githubusercontent.com/deepseek-ai/deepseek-recipe/main/README.md` | `main` at fetch | `raw/deepseek-recipe-README.md` |
| S21 | Terminal-Bench 2.0 leaderboard (supabase `leaderboard-read`) | `POST …/leaderboard-read {"package":"terminal-bench/terminal-bench-2","name":"2-0"}` | live at fetch | `raw/tbench-leaderboard-tb2.0.json` |
| S22 | Terminal-Bench 2.1 leaderboard (same endpoint) | `POST … {"package":"terminal-bench/terminal-bench-2-1","name":"main"}` | live at fetch | `raw/tbench-leaderboard-tb2.1.json` |

Fetch times are in `MANIFEST.csv` (`fetched_utc`); all fetches happened between 2026-10-27T02:35:47Z
and 2026-10-27T02:45:07Z (the last one is `raw/v41_base_config.json`).

Note on S21/S22: the live payloads are byte-identical (same sha256 `dd426eef8d24974e…` /
`a03472372fd4cbf3…`) to the copies already archived at `bench/public/raw/tbench/leaderboard-tb2.0.json`
and `…-tb2.1.json` on 2026-10-26T13:45Z, so the leaderboards did not change between the two days.

---

## 2. Model identity

### 2.1 Exact identifiers

| Layer | Identifier | Source |
|---|---|---|
| HF repository (this release) | `deepseek-ai/DeepSeek-V4.1-Flash-1017` | S1, S2 |
| HF revision | `1bae0e1088993b9b82de08f59de5be5a34e42170` | S2 |
| HF repository (previous release) | `deepseek-ai/DeepSeek-V4.1-Flash`, rev `314113f8a7dadb388215396ba5fbee2c610a04d6` | S8, S10 |
| Official API model name | **`deepseek-flash`** | S14, S13 (`model` enum is `[deepseek-flash, deepseek-v4-pro]`) |
| What `deepseek-flash` currently serves | `MODEL VERSION: DeepSeek-V4.1-Flash-1017` | S14 |
| Legacy names still accepted | `deepseek-v4-flash`, `deepseek-v4-flash-vision-exp` — "the corresponding models have been retired, their requests are served by the DeepSeek-V4.1-Flash-1017 model" | S14, S15 |
| Second live model | `deepseek-v4-pro` → `DeepSeek-V4-Pro-0813` | S14 |

Sibling repositories: the first page of the org listing (S11, `limit=100`) contains `DeepSeek-V4.1-Flash` and
`DeepSeek-V4.1-Flash-1017` and no other V4.1 checkpoint — there is **no** `DeepSeek-V4.1`,
`DeepSeek-V4.1-Pro`, `DeepSeek-V4.1-Flash-Pro`, `DeepSeek-V4.1-Base` or `DeepSeek-V4` repo
(`https://huggingface.co/api/models/deepseek-ai/{name}` returned `{"error":"Invalid username or
password."}` for those five probes; the author listing is the authoritative inventory). So there is
no separate "V4.1 Pro" whose numbers could be confused with V4.1-Flash.

`deepseek-v4.1-flash` — the name our adapter sends — is **not a documented DeepSeek API model id**.
It does not appear in the pricing table (S14), nor in the `model` enum of the Chat Completions
reference (S13), nor in the 2026-09-10 / 2026-10-17 changelog entries (S15), nor in the release notes
(S16, S17). Our `bench/adapters/lcx_agent.py` puts that string on a local sub2api gateway, so whatever
checkpoint it selects is a gateway-side alias decision, not something DeepSeek documents. The string
does appear in third-party material (e.g. the OpenCode Go provider naming in a harness bug report at
`https://github.com/can1357/oh-my-pi/issues/15178`, `"provider": "opencode-go", "model":
"deepseek-v4.1-flash"`), which is evidence a gateway alias of that shape is in use elsewhere, not
evidence that DeepSeek serves it.

### 2.2 What "-1017" means, and whether SpecPi's runs used it

"-1017" is the release date, **17 October 2026**, and this is verified from commit timestamps rather
than inferred from the name:

- S3, `/commits/main`, oldest-first: `50aa60c7c756 initial commit` at `2026-10-17T02:40:38Z`, then
  `73e1d8e9… "Add files using upload-large-folder tool"` at `03:35:48Z`, `dfbc4a7d… "Update
  gitattributes"` at `05:44:31Z`, and `1bae0e10… "Release DeepSeek-V4.1-Flash-1017"` at
  `05:46:55Z` — the tip, which is also the revision holding the weights and README.
- S15 change log: `### Date: 2026-10-17` … "DeepSeek V4.1 Flash has been upgraded to
  DeepSeek-V4.1-Flash-1017. … The API calling method and pricing remain unchanged — simply use
  `deepseek-flash` to access the latest model."
- S16 (release note) is headed `DeepSeek-V4.1-Flash-1017 Release 2026/10/17`.

The previous checkpoint, `DeepSeek-V4.1-Flash` (no suffix), was released **2026-09-10**: S15
`### Date: 2026-09-10` "Today, we officially release the DeepSeek-V4.1-Flash model", and the earliest
commits on its repo are dated 2026-09-10 (S10: `53e70b17d9ac "Upload DeepSeek_V41_Tech_Report.pdf"`
at `2026-09-10T05:51:21Z`).

**Consequence for SpecPi.** SpecPi's Terminal-Bench 2.0 data was generated on **2026-09-24** (SpecPi
`CHANGELOG.md` 0.34.0: "A 24 Sep Terminal-Bench 2.0 sitting ran the published 0.33.0 …"; also "Add a
second 24 Sep sitting for SpecPi 0.33.0"), i.e. **23 days before -1017 existed in any form**. Any
`deepseek-v4.1-flash` traffic SpecPi sent on 24 Sep can therefore only have reached the 0910
V4.1-Flash checkpoint (or whatever its provider served under that alias then); it cannot have reached
V4.1-Flash-1017. The same holds for the 21 Sep sittings listed in
`bench/public/PUBLIC_BASELINES.md` sitting table (e.g. "Pi (base) | 21 Sep (s1) | 30/39"). Stated
positively: for the specpi rows we
compare against, the model version is **V4.1-Flash (2026-09-10), not -1017**, and SpecPi did not
publish which checkpoint its endpoint served (see the "endpoint unpublished" disclosure in
`bench/PREREGISTRATION_TB2_W13.md`).

### 2.3 Attachment-visible configuration differences between the two checkpoints

`config.json` is **byte-identical between the two checkpoints**: the 1017 file (S4) has
sha256 `8be45ce0476004a3…` and the file served from the base repo at rev `314113f8…`
(`raw/v41_base_config.json`) has the same sha256, so a JSON-level comparison finds no differing key
(40 layers, 384 routed experts, 1M `max_position_embeddings`, fp8 weights, fp4 experts in both). Nothing in the public files states what changed
inside the weights between 0910 and 1017; the model card only says it "is a minor update … delivering
substantial improvements across reasoning, coding, agentic tasks, and visual benchmarks" (S1). The
training-difference detail for -1017 is **not published** (checked: S1, S16, S15, S9 — the technical
report predates -1017 and describes the 0910 model).

---

## 3. Official benchmark numbers

### 3.1 What the -1017 card publishes

Table from S1 (`raw/README.md`, section "Introduction"), with the card's own footnote: "*† denotes the
text-only subset of HLE. We use the Claude Code harness for Terminal-Bench 4.0 and the Mini-SWE-V2
harness for DeepSWE v1.1.*"

| Benchmark | DS-V4.1-Flash-1017 | DS-V4.1-Flash (0910) |
|---|---|---|
| HLE w/o tools | 42.6 (44.0†) | 36.8 (39.1†) |
| **Terminal-Bench 4.0** | **42.3** | 31.2 |
| DeepSWE v1.1 | 77.0 | 74.2 |
| ProgramBench | 29.1 | 20.3 |
| NL2Repo-Bench | 68.2 | 65.4 |
| Automation-Bench v1.0.6 | 60.5 | 54.8 |
| Agents' Last Exam | 35.4 | 31.8 |
| Apex Bench v1.1 | 72.1 | 64.6 |
| Chartography (w/tools) | 83.0 | 78.9 |
| BabyVision (w/tools) | 92.5 | 89.6 |
| ZeroBench-main (w/tools) | 51.0 | 49.0 |

The same 1017 numbers are repeated in the API changelog (S15, `### Date: 2026-10-17`) with one extra
sentence of harness attribution: "Terminal-Bench 4.0 was tested with the Claude Code framework, and
DeepSWE with the Mini-SWE-V2 framework. ZeroBench-main was evaluated with Pass@5."

**The -1017 card publishes no Terminal-Bench 2.0 and no Terminal-Bench 2.1 number.** Checked S1
(full README), S16 (release note; it shows only a DeepSWE v1.1 cost/performance chart), S15 (change
log entry) and S2 (no `model-index`; `"model-index": null`).

### 3.2 What the previous checkpoint (0910) publishes for Terminal-Bench 2.1

S8 (`raw/v41_README.md`) carries the only official Terminal-Bench 2.x figure in this family, and it
belongs to the **0910** checkpoint, not to -1017:

| Benchmark (Metric) | DS-V4.1-Flash |
|---|---|
| Terminal-Bench 2.1 (Pass@1) | **90.6** |
| Terminal-Bench 3.0 (Pass@1) | 30.0 |
| Terminal-Bench 4.0 (Pass@1) | 31.2 |
| DeepSWE v1.1 (Resolved) | 74.2 |

The conditions are published in unusual detail. From S8 ("Instruct Model" preamble): "All instruct
results below use the maximum effort setting (`reasoning_effort=100`). Evaluations use
`temperature=1.0, top_p=0.95`." … "For code agent benchmarks (Terminal-Bench 2.1/3.0/4.0, DeepSWE
v1.1, NL2Repo-Bench, ProgramBench), the model is evaluated with the Minimal mode of DeepSeek Harness
and a 1M-token context window. … All agentic evaluations use `temperature=1.0, top_p=0.95`."

Cross-scaffold table (S8, "Performance across agent scaffolds", also S9 Table 4):

| Benchmark | Claude Code | Codex | OpenCode | Pi | mini-SWE | DSH Minimal | DSH Standard | DSH PTC |
|---|---|---|---|---|---|---|---|---|
| Terminal-Bench 2.1 (Pass@1) | 88.0 | 84.1 | 85.0 | 86.1 | 90.3 | **90.6** | 85.8 | 85.8 |
| DeepSWE v1.1 (Resolved) | 69.8 | 65.6 | 65.5 | 66.2 | 74.2 | 72.6 | 70.5 | 67.6 |

Conditions, verbatim (S8): "All scaffolds use N=8 samples per task on DeepSWE v1.1 and N=3 on
Terminal-Bench 2.1, with Linux containers, `temperature=1.0`, `top_p=0.95`, a 1M-token context limit,
and max_steps=500 per agent. Terminal-Bench 2.1 is evaluated without network access."

Version pinning of the harnesses (S9, Appendix B.1): "Claude Code (v2.1.105, v2.1.238, v2.1.251,
v2.1.259). We use the Claude Agent SDK with each version's native tool interface. Table 4 reports
v2.1.251"; "Codex (v0.147.0)"; "OpenCode (v1.18.15)", "build agent with shell and file tools, and
native task delegation"; "Pi (v0.84.2)", "RPC mode with file and shell tools plus a search extension";
"mini-SWE. We use the mini_swe_v2 port with a single bash tool"; "DeepSeek Harness (DSH). We use
Minimal with a single bash tool; Standard with the full sdk profile and 26 initial function tools …
and PTC with run_code". Also: "All scaffolds run in Linux task containers using the shared evaluation
settings in Table 4. Each run starts from the benchmark task description and uses the prompts, task
templates, and tool definitions supplied by the scaffold's runtime or evaluation integration. **We add
no experimental system prompt.**" And on integrity: "we restrict internet access and strip Git
histories from the environment. Additionally, we automatically purge transient build and package
caches".

Reasoning-effort sensitivity on the same benchmarks (S9, §5.3.2): "Raising the effort from 25 to 100
improves the average Pass@1 on eight reasoning-intensive benchmarks from 67.1% to 76.3%, on DeepSWE
v1.1 from 66.0% to 74.2%, and on Terminal-Bench 2.1 from 82.4% to 90.6%, at the cost of roughly 2.5×
more output tokens. … the 60–80 range already recovers most of the accuracy of the maximum setting at
less than half of its token budget".

### 3.3 Condition ledger

For each published number, what is published and what is not.

| Condition | Terminal-Bench 2.1 (0910, 90.6) | Terminal-Bench 4.0 (-1017, 42.3) |
|---|---|---|
| Benchmark version | TB 2.1 — published (S8, S9) | TB 4.0 — published (S1, S15) |
| Dataset ref / task count | **not published** (checked S8, S9 §5.3.1, B.1; no commit or dataset tag is named) | **not published** (checked S1, S15, S16) |
| Harness | DSH Minimal, plus 7 others in the scaffold table — published (S8, S9 Table 4) | Claude Code — published (S1 footnote, S15); version **not published** |
| Harness version | published (S9 B.1: Claude Code v2.1.251; Codex v0.147.0; OpenCode v1.18.15; Pi v0.84.2; mini-swe-agent commit 04d809ceab9d; DSH v0.1.1+ custom.202609011522) | **not published** |
| reasoning_effort | Max = 100 — published (S8) | **not published** (likely max, not stated) |
| temperature / top_p | 1.0 / 0.95 — published (S8) | **not published** |
| context window | 1M — published (S8, S9) | **not published** |
| max_steps | 500 — published (S8, S9 Table 4 note) | **not published** |
| max output tokens | **not published** (only the API default/cap in §4.4) | **not published** |
| trials / averaging | N=3 samples per task; metric "Pass@1"; no CI or pooling rule published (S8, S9) | **not published** |
| network | disabled — "evaluated without network access" (S8, S9) | **not published** |
| system prompt | none added — "We add no experimental system prompt" (S9 B.1) | **not published** |
| date | model released 2026-09-10 (S15, S17); evaluation date **not published** | model released 2026-10-17 (S3, S15); evaluation date **not published** |

### 3.4 Terminal-Bench 2.0 and SWE-bench Verified

- **Terminal-Bench 2.0: not published** for any DeepSeek V4.x model. Checked: the -1017 card (S1), the
  0910 card (S8), the technical report's benchmark list (S9 §5.3.1: "Terminal-Bench 2.1 … 3.0 … 4.0",
  no 2.0), the API change log back to 2026-08-13 (S15, plus `raw/ds-news_news260813.html`,
  `raw/ds-news_news260821.html`), and the TB 2.0 leaderboard itself (S21).
- **SWE-bench Verified: not published** for either checkpoint. Checked: S1, S8, S9 (the benchmark list
  in §5.3.1 and Table 3 contain no SWE-bench entry), S15. (The SWE-bench Verified results in
  `bench/public/raw/specpi-repo/CHANGELOG.md` are SpecPi's own 12-task slice, not DeepSeek's.)
- **The Terminal-Bench 2.0 leaderboard has exactly one DeepSeek row, and it is not a V4.x model** (S21):
  `rank 86 | Terminus 2 (agent_version 2.0.0) | model DeepSeek-V3.2 | accuracy 39.5505617978 %
  (display "**39.6%** ± 2.8") | date 2025-12-01 | verified false | integration_method "API" |
  n_trials 0`. The TB 2.1 leaderboard (S22, 22 rows) contains **no** DeepSeek row at all. No V4.1-Flash
  or V4.1-Flash-1017 row exists on either leaderboard, with any harness.

---

## 4. Sampling, thinking mode, and output limits (what DeepSeek documents)

### 4.1 Thinking mode toggle and effort

S12 (`guides/thinking_mode`), table "Thinking Mode Toggle and Effort Control": thinking mode is
enabled with `{"thinking": {"type": "enabled/disabled"}}` (OpenAI format) or `reasoning_effort`
`low/high/max`; the Anthropic format uses `{"reasoning": {"effort": "none/low/high/max"}}`. Footnote:
"**(1) Thinking mode is enabled by default, with the default effort being high**". The effort mapping
table is: `minimal → low`, `low → low`, `medium → high`, `high → high`, `xhigh → high`, `max → max`,
`ultra → max`.

The raw numeric scale behind those aliases is published in the model files, not the API docs: S6
("Reasoning effort is a numeric budget (1–100) … `"low"` → 50, `"high"` → 75, `"max"` → 100. The
default is `"high"` (75)") and S7 (`REASONING_EFFORT_MAPPINGS = {"low": 50, "high": 75, "max": 100}`,
`DEFAULT_REASONING_EFFORT = "high"`). S9 §5.3.3 states the same: "In our public API service, we expose
three preset effort levels that map onto this scale: low, high, and max correspond to effort values of
50, 75, and 100, respectively."

Prompt-side rendering (S6): the effort prefix is a single `<｜System｜>` block at the start of the
conversation, `Reasoning Effort: {budget} (range 1-100, the higher the value, the more thorough the
reasoning)`, and "The effort prefix is only rendered in `thinking_mode="thinking"` and only at the
beginning of the conversation (index 0)."

### 4.2 temperature and top_p — **the answer to the acceptance question**

**Does setting `temperature` have an effect in thinking mode, per official docs? No.** Two independent
statements, both from DeepSeek's own API documentation:

1. S12 (`guides/thinking_mode`), section "Input and Output Parameters": "**Thinking mode does not
   support the temperature, presence_penalty, or frequency_penalty parameters. Please note that, for
   compatibility with existing software, setting these parameters will not trigger an error but will
   also have no effect.**"
2. S13 (Chat Completions reference), `temperature` field: "Default value: 1 … **Has no effect in
   thinking mode.**"

`top_p` behaves differently and *is* live in thinking mode. S13, `top_p` field, verbatim: "Default
value: 1 … It only takes effect in thinking mode, where the effective range is 0.95–1.0: values below
0.95 are treated as 0.95. In non-thinking mode it is fixed at 1.0 and the value you pass is ignored."
S12 gives the same rule with the model split: "top_p behavior depends on the model: — deepseek-flash:
only takes effect in thinking mode, where the effective range is 0.95–1.0; values below 0.95 are
treated as 0.95. In non-thinking mode it is fixed at 1.0 and your value is ignored. — deepseek-v4-pro:
in thinking mode your value is ignored (fixed at 0.95 when tools is present, otherwise 1.0)."

Penalties are gone entirely. S12: presence/frequency penalties not supported in thinking mode. S13
marks both fields `deprecated`: "This parameter is no longer supported. It will not take effect if you
pass it to the API."

**What that means for LC.** Our adapter sends no `temperature` and no `top_p`
(`bench/PREREGISTRATION_TB2_W13.md`: "No `reasoning_effort` is sent (upstream default)"). Because
thinking mode is on by default (S12) — and our adapter declares `"format": "reasoning-effort"` with
`echo: reasoning_content` (`bench/adapters/lcx_agent.py`) — the effective sampling is DeepSeek's
defaults: temperature 1.0 with no effect, top_p 1.0 (inside the 0.95–1.0 band, so no clamping).
The official benchmark condition `temperature=1.0, top_p=0.95` (S8) is therefore *equivalent in kind*
to ours on temperature (both no-ops) but differs in top_p (0.95 vs 1.0 default). Per S9 §5.3.2 and
Figure 11/12, the accuracy knob that actually moves Terminal-Bench 2.1 is effort, not temperature:
82.4 at effort 25 → 90.6 at effort 100.

### 4.3 Defaults and recommended values

| Parameter | Default (API) | Recommended (model card) | Used in published evals |
|---|---|---|---|
| `temperature` | 1, no effect in thinking mode (S13) | 1.0 (S1, S8) | 1.0 (S8, S9) |
| `top_p` | 1 (S13); effective 0.95–1.0 in thinking mode (S12) | 1.0 (S1); "0.95 or 1.0" (S8) | 0.95 (S8, S9) |
| `max_tokens` | 8K non-thinking, 64K thinking, 128K at `reasoning_effort=max` (S13) | "≥ 256K" (S1); "≥ 256K" (S8) | **not published** |
| context window | 1M (S14) | 1M (S1, S8) | 1M (S8, S9) |
| `presence_penalty` / `frequency_penalty` | not supported, ignored (S12, S13) | — | — |

`generation_config.json` — **does not exist in the repository**. S2's file list has no
`generation_config.json`; the only config artefacts are `config.json`, `inference/config.json` and
`tokenizer_config.json`. So there is no model-side default sampling config to disagree with the API
defaults above. Likewise there is **no Jinja chat template**: S5's `tokenizer_config.json` has no
`chat_template` key, and both cards state "This release does not include a Jinja-format chat template"
(S1, S8).

### 4.4 Output limits

S14 (Models & Pricing) for `deepseek-flash`: "CONTEXT LENGTH 1M", "MAX OUTPUT MAXIMUM: 384K".
S13 (`max_tokens`): "The value must be between 1 and 384K (393216). When not set, the default is 8K in
non-thinking mode, 64K in thinking mode (128K with `reasoning_effort` set to `max`)." S20 corroborates
from the reference implementation side: `resolve_max_tokens` "supplies the `max_tokens` default when
the request sets none: 8192 outside thinking mode, 65536 in thinking mode, and 131072 in thinking mode
with `max` effort."

Our adapter's 32,768 output cap is therefore below the thinking-mode default of 65,536 that an
unmodified `deepseek-flash` request would get, and far below the ≥256K the model card recommends for
agentic work.

### 4.5 `reasoning_content` in multi-turn tool use

S12: "In thinking mode, the chain-of-thought content is returned via the `reasoning_content`
parameter, at the same level as `content`. In subsequent requests, whether `reasoning_content` should
be passed back and whether it will be concatenated into the context depends on whether the request
carries the `tools` parameter: — If the request carries the `tools` parameter: the `reasoning_content`
of all previous turns should be passed back to the API and will be concatenated into the context. … —
If the request does not carry the `tools` parameter: `reasoning_content` does not need to be passed
back; even if passed to the API, it will be ignored". S19 repeats it from the multi-turn angle: "If
the request does not carry the `tools` parameter, the CoT content from previous turns will not be
concatenated into the context in the next turn", and its sample code comments the second-turn append
with "# The reasoning_content will be ignored by the API".

And, in the same section, the hard requirement: "for requests carrying the `tools` parameter, the
`reasoning_content` must be fully passed back to the API in all subsequent requests — even for turns
where the model did not perform a tool call. **If your code does not correctly pass back
`reasoning_content`, the API will return a 400 error.**"

The model-file side says the same thing in encoder terms (S6): "**With a tools declaration**:
`drop_thinking` is automatically disabled. All turns retain their reasoning, because tool-calling
conversations require full context for the model to track multi-step reasoning across tool calls."

### 4.6 Tool calls in thinking mode, `strict` mode, and DSML

- Tool use in thinking mode is supported (S18: "From DeepSeek-V3.2, the API supports tool use in the
  thinking mode.").
- `tool_choice` is restricted: S13 — "required and named tool choices are not supported in thinking
  mode; the API returns a 400 error. Disable thinking mode first to use them."
- `strict` mode exists and is a Beta feature: S18 — "In strict mode, the model strictly adheres to the
  format requirements of the Function's JSON schema when outputting a tool call … It is supported by
  both thinking and non-thinking mode." To use it: "Use `base_url="https://api.deepseek.com/beta"` to
  enable Beta features" and set `"strict": true` on every function; the server validates the JSON
  Schema. S13's `strict` field: "Default value: false … This is a Beta feature".
- **DSML**: the tool-call markup format is documented in the model files, **not** in the API docs. S6
  has a section headed "Tool calling (DSML format)", which says tools are declared on the system
  message through the `tools` field and that the model "can invoke tools by writing a
  `<｜DSML｜ calls>` block" wrapping one `<｜DSML｜ invoke name="TOOL_NAME">` element per call, each holding
  `<｜DSML｜ parameter name="ARG_NAME" string="true|false">` elements. S7 fixes the names in code:
  `dsml_token = "｜DSML｜"`, `tool_calls_block_name = " calls"`, `tool_call_tag_name = " invoke"`,
  `tool_parameter_tag_name = " parameter"` (`raw/encoding.py:33,428-430`).

  The detail that matters for a harness parser: **every V4.1 tag name carries a leading space**, and
  S6 lists this as the first V4.1-vs-V4 prompt-format change — in S6's words, tool calls are wrapped in
  a block whose tag name is written with a space before `calls`, the per-call tag name with a space
  before `invoke`, and the parameter tag name with a space before `parameter`; "The V4 format used
  `tool_calls` without a space." S7 encodes the same as `" calls"`, `" invoke"`, `" parameter"`.

  Parameter typing (S6): `string="true"` means "the parameter value is a raw string";
  `string="false"` means "the parameter value is JSON (number, boolean, array, object)". Tool results
  come back as a `tool_result` block inside the following user message; S6 also notes
  `merge_tool_messages()` sorts multiple results by the order of the corresponding calls in the
  preceding assistant message. Deferred tool loading (Messages API) and client-executed tool search
  (Responses API) are V4.1 features named in S20.

  "DSML" appears **nowhere** on `api-docs.deepseek.com`: a case-insensitive search over the 15 API
  documentation pages archived here (`raw/ds-*.html`, including the full sitemap `raw/apidocs_sitemap.xml`)
  returns no match. The only official documentation of the format is S6/S7 (HF repo) and S20.

---

## 5. Reproducibility in Harbor with our setup

Short answer: **no.** DeepSeek's Terminal-Bench numbers cannot be reproduced by our run, and for
Terminal-Bench 2.0 there is no DeepSeek number to reproduce in the first place.

### 5.1 There is no official TB 2.0 number to compare 13 tasks against

§3.4: no V4.x model has a TB 2.0 figure anywhere public, and the only TB 2.0 leaderboard row from
DeepSeek is V3.2 + Terminus 2 at 39.6% ± 2.8 (S21) — a different generation, a different harness, and
a full-dataset number. The 90.6 figure in S8 is **TB 2.1** and belongs to the **0910** checkpoint; the
-1017 checkpoint has no TB 2.1 number published, only TB 4.0 = 42.3 with Claude Code.

### 5.2 Blocking differences, one by one

| # | Dimension | Official (S8/S9 unless noted) | Ours (`bench/PREREGISTRATION_TB2_W13.md`, `bench/adapters/lcx_agent.py`) | Blocks reproduction? |
|---|---|---|---|---|
| 1 | Benchmark version | TB 2.1 (90.6) or TB 4.0 (42.3, S1) | TB **2.0** | **Yes** — different task sets; scores are not transferable across versions |
| 2 | Task count / subset | full published dataset (TB 2.0 has 89 tasks per `bench/public/PUBLIC_BASELINES.md`, not per DeepSeek; TB 2.1 count and dataset ref not published) | 13 "widened" tasks, `tb2-widened13.txt` | **Yes** — 13/89 subset, chosen for discriminative power, has no published reference value |
| 3 | Harness | DSH Minimal for the 90.6 headline; Claude Code (v2.1.251) for 88.0; TB 4.0 42.3 uses Claude Code | LumenCortex A+ (`cog/r1a@4968ea8`) | **Yes** — harness is the second-largest factor (88.0 → 90.6 across scaffolds on TB 2.1) |
| 4 | reasoning_effort | 100 (`max`) | none sent → API default `high` = 75 (S12, S6, S7) | **Yes, materially** — effort 25 → 100 moves TB 2.1 from 82.4 to 90.6 (S9 §5.3.2); no official value is published at 75, and S9 states the 60–80 range "already recovers most of the accuracy of the maximum setting" |
| 5 | temperature / top_p | 1.0 / 0.95 | neither sent; effective temperature 1.0 (no-op), top_p 1.0 | Partly — temperature is a documented no-op in thinking mode, so this difference is inert; top_p 1.0 vs 0.95 is a real (small, undocumented in magnitude) difference |
| 6 | max_tokens | not published; card recommends ≥256K; API default 64K in thinking mode / 128K at max effort | 32,768 | **Yes** — cutting output tokens truncates long agent turns; the effort-vs-length curve in S9 §5.3.2 is the mechanism by which effort buys accuracy |
| 7 | context window | 1M | 600K | Possibly — 1M is the card's recommended window (S1, S8); a 600K cap can force different compaction behaviour |
| 8 | max_steps | 500 | 150 (`LCX_MAX_STEPS=150`) | **Yes** — a third of the step budget |
| 9 | trials / averaging | N=3 per task (TB 2.1); no CI, no pooling rule published | 3 attempts per task, one sitting, concurrency 1 | Comparable count, different pooling; official has no published variance |
| 10 | timeout | not published | Harbor 0.25.0, `--timeout-multiplier 2` | Unknown direction — cannot be quantified against an unpublished baseline |
| 11 | network | "evaluated without network access" (S8, S9) | not restricted by the adapter; connectivity follows the task image | Unknown — network access can only help a solver that is allowed to use it |
| 12 | system prompt / tools | official scaffold's own, and "We add no experimental system prompt" (S9 B.1) | LC's own policy/tool set, `LCX_POLICY=full` | **Yes** — not separable from harness effect |
| 13 | host / runtime | Linux containers, x86 (S9 B.1 "Linux task containers"); host arch not published | macOS arm64 host, amd64 images emulated, 3.8 GiB Docker VM shared with the gateway | **Yes** — wall-clock and timeout behaviour differ; disclosed in the pre-registration |
| 14 | model checkpoint availability | 0910 for the TB 2.1 numbers; -1017 for TB 4.0 | whatever `deepseek-v4.1-flash` resolves to on the local gateway (undocumented alias; §2.1) | **Yes** — unless the gateway pins -1017, even the checkpoint differs |
| 15 | endpoint | DeepSeek's own serving stack | local sub2api gateway → DeepSeek | Unknown — gateway may re-render prompts, drop fields, or clamp parameters |

### 5.3 What a comparison can legitimately claim

- Our 13-task number is a measurement of **LC × deepseek-v4.1-flash(gateway) × TB 2.0/13 tasks**.
  DeepSeek has published nothing on any of those axes except the model.
- The only defensible uses of the official figures are (a) ordering models or harnesses on TB 2.1 to
  calibrate what "good" looks like for this model family, and (b) sanity-checking that LC's
  requirements — 1M context, ≥256K output, full `reasoning_content` echo, thinking mode on, numeric
  effort 1–100 — are reachable at all.
- Any claim of the form "LC is X points below DeepSeek's official number" is invalid: the datasets
  differ, the harnesses differ, the step budgets differ by 3.3×, and for TB 2.0 no official number
  exists.

---

## 6. Status of these numbers for LC

**The official numbers in §3 are a reference, not an acceptance threshold.** Nothing here defines a
pass/fail bar for the LC run, and no threshold should be derived from them:

- They are measured on a different benchmark version (2.1 or 4.0, never 2.0), a different harness, a
  different step/context/output budget, and — for TB 2.1 — a different checkpoint than the one our
  gateway may serve.
- The only official TB 2.0 figure from DeepSeek is V3.2 + Terminus 2 (39.6% ± 2.8, S21), which is a
  generation older than ours and a harness we do not run.
- The pre-registration already fixes the acceptance logic for the LC run (per-task pooled verifier
  reward at 3 attempts, plus the ≤900 s timeout-sensitivity view). Those are the criteria; §3 numbers
  are context for interpreting them.

---

## 7. Gaps register — everything not published, and where it was checked

| Missing datum | Where it was looked for | Result |
|---|---|---|
| TB 2.0 score for any V4.x model | S1, S8, S9 §5.3.1/Table 3, S15, S16, `raw/ds-news_news260813.html`, `raw/ds-news_news260821.html`, S21 | not published |
| TB 2.1 score for V4.1-Flash-1017 | S1 (card), S15 (2026-10-17 entry), S16 (release note), S22 | not published (only the 0910 checkpoint has 90.6) |
| TB 4.0 score for 0910 with Claude Code | S8, S9 | not published for Claude Code specifically; 31.2 is the DSH-Minimal protocol (S8), and the -1017 card's comparison column repeats 31.2 for "DS-V4.1-Flash" |
| SWE-bench Verified for any V4.x checkpoint | S1, S8, S9, S15 | not published |
| Dataset ref/tag for TB 2.1 and TB 4.0 evaluations | S8, S9 §5.3.1, B.1, S1, S15 | not published (no commit hash or dataset tag named) |
| temperature / top_p / effort / trials used for TB 4.0 = 42.3 | S1, S15, S16 | not published |
| Claude Code version used for TB 4.0 = 42.3 | S1, S15, S16 | not published (v2.1.251 applies to the TB 2.1 table, S9 B.1) |
| max output tokens used in any official agentic evaluation | S8, S9 | not published (only the ≥256K recommendation and the API default/cap in §4.4) |
| TB 2.1 task count | S8, S9 | not published (TB 2.0's 89 tasks is from `bench/public/PUBLIC_BASELINES.md`, not from DeepSeek) |
| What changed in the weights between 0910 and -1017 | S1, S15, S16, S9 | not published (technical report predates -1017) |
| Any documentation of `deepseek-v4.1-flash` as an API model id | S13, S14, S15, S16, S17, S20 | not published (official ids are `deepseek-flash` and `deepseek-v4-pro`) |
| `generation_config.json` | S2 file list, S4, S5 | file does not exist in the repo |
| Jinja chat template | S5, S1, S8 | not included ("This release does not include a Jinja-format chat template") |
| DSML markup on api-docs.deepseek.com | 16 archived API doc pages + `raw/apidocs_sitemap.xml` | not documented there; only in S6/S7/S20 |

---

## 8. Facts that matter most for the LC evaluation

1. **temperature is a documented no-op in thinking mode**, and thinking mode is the default. S12:
   "Thinking mode does not support the temperature, presence_penalty, or frequency_penalty parameters.
   … setting these parameters will not trigger an error but will also have no effect." S13, on
   `temperature`: "Has no effect in thinking mode." For `deepseek-flash`, `top_p` *is* live in thinking
   mode with an effective range of 0.95–1.0 (S12, S13). Our adapter's silence on both therefore costs
   nothing on temperature and leaves top_p at 1.0.
2. **The accuracy knob that exists is `reasoning_effort` (1–100, aliases low=50 / high=75 / max=100,
   default high).** Official agentic runs used the maximum (100) (S8); ours sends nothing, so it runs
   at 75. S9 §5.3.2 quantifies the gap on TB 2.1: 82.4 at effort 25 → 90.6 at 100, with most of the
   gain by 60–80.
3. **SpecPi's 24 Sep TB 2.0 data used the 0910 checkpoint, not -1017.** -1017's first commit is
   2026-10-17T02:40:38Z and the API switched on 2026-10-17 (S3, S15) — 23 days after SpecPi's sitting.
   Any SpecPi row for `deepseek-v4.1-flash` is a row for V4.1-Flash (2026-09-10), whose TB 2.1 score is
   90.6 with DSH Minimal.
4. **-1017 publishes only TB 4.0 (42.3, Claude Code) — no TB 2.0, no TB 2.1, no SWE-bench Verified.**
   The 42.3 is not comparable to a TB 2.0 result, and its sampling conditions (effort, trials, dataset
   ref, Claude Code version) are unpublished (S1, S15, S16).
5. **Tool-call protocol is DSML with leading-space tag names, and `reasoning_content` must be echoed
   verbatim when tools are present.** S12: "If your code does not correctly pass back
   `reasoning_content`, the API will return a 400 error." S6: tag names are " calls", " invoke",
   " parameter" (V4 used `tool_calls` with no space). Any LC-side parser or history rewrite that drops
   reasoning, renames the tags, or fails to close them breaks against this model — the failure mode is
   visible in the wild as malformed DSML arriving as plain text
   (`https://github.com/can1357/oh-my-pi/issues/15178`).
