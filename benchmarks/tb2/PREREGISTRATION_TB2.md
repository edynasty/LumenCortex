# TB2 subset run — pre-registration

Written 2026-10-26 before any counted trial. Smoke trials (`jobs/smoke-*`) are not counted.

## Question

Does LumenCortex Core, as built at three revisions, complete real terminal tasks as well as an
established open agent (OpenCode) on the same model and gateway, and do the R1a changes (A → A+)
and the CHECKS-002 / provider-capacity fixes (A+ → A+fix) regress general task success, cost or
reliability?

Out of scope: cognition benefit across sessions. Every TB2 trial is a single fresh session in a fresh
container with no prior state, so it cannot show cross-session learning (`COGNITIVE_BENEFIT_PROVEN`
stays NO whatever the result). TB2 tasks ship no LumenCortex check set, so CHECKS-002 is not exercised
here; A+ vs A+fix measures only that the fixes do not regress ordinary runs.

## Arms

| Arm | Agent | Core revision | Binary SHA-256 (linux/amd64, static, `bench/build/bin`) |
|---|---|---|---|
| A | `lcx_agent:LcxA` | `3e1c3cc` (main: T0-S1 merged) | `f039fb72bdd776c42f6ac65124fa02cd7d961d567706f14a039716360c1e6d23` |
| A+ | `lcx_agent:LcxAplus` | `4968ea8` (cog/r1a, G-R1a reviewed) | `c33ff097cf9449a6b7c6195ee46c57045ab81b2a5e1ddc5609fe8f24c4b03435` |
| A+fix | `lcx_agent:LcxAfix` | `f2046d5` (cog/r1a-fix: CHECKS-002 rev3 + capacity marker) | `06009c6dffa424f4f80c7783655c17f4ebccdee8a25f57e727017e7a4e24a926` |
| OpenCode | `opencode_local:OpenCodeLocal` (Harbor `opencode` with an offline install) | opencode-ai 1.19.4, official `opencode-linux-x64` binary uploaded at setup | `538fcb1b6408c8d4608822acee19f7ff5b3a3eb3383c0633c4f06170857312a7` |

Core binaries: built from `git archive <rev>` in `golang:1.23-alpine` (linux/amd64), `CGO_ENABLED=1`,
`-trimpath -ldflags '-s -w -linkmode external -extldflags -static'`, tag `sqlite_omit_load_extension`.

OpenCode arm (amended before its first counted trial; the arm runs last): offline install of the
official binary and, for every exec, `OPENCODE_EXPERIMENTAL_DISABLE_FILEWATCHER`,
`OPENCODE_DISABLE_AUTOUPDATE`, `OPENCODE_DISABLE_MODELS_FETCH`, `OPENCODE_DISABLE_LSP_DOWNLOAD` = 1.
Under amd64 emulation 1.19.4 hung with the watcher on (smoke-opencode-fixgit-3: AgentTimeout after
one request; a direct debug run finished the task in 13 s and then never exited); with these off it
exits 0 in 22 s. They change no tool, prompt or model. One more Harbor smoke must pass before the
OpenCode job starts.

A+fix note: the counted A+fix job runs `f2046d5`. `4e35447` (committed while that job ran) changes
only how check commands are parsed into inputs, which no TB2 trial reaches (no task ships a check
set), so the A+fix binary is not rebuilt mid-experiment.

LumenCortex arms are identical except for the binary: global config under `XDG_CONFIG_HOME` (nothing
written into the task workspace), state under `XDG_STATE_HOME`, `LCX_POLICY=full`,
`LCX_MAX_STEPS=150`, model limit context 600,000 / output 32,768, reasoning echo
`reasoning_content`, provider timeouts 3600 s / idle 600 s. The 600K context is below the measured
gateway ceiling (649,063 passed, ~674K refused) so A and A+ are not penalised for lacking the
capacity marker; A+fix's marker is therefore not expected to fire.

## Model, gateway, metering

- Model `deepseek-v4.1-flash` via the local sub2api gateway (`localhost:8080`), reached from the
  containers through the metering proxy `127.0.0.1:18080` (`host.docker.internal`), which injects the
  real key (credential `omp-local`) and logs every request (`jobs/metering/proxy.jsonl`, bodies under
  `jobs/metering/bodies`). Containers hold only a dummy key.
- LumenCortex requests carry `X-Exp-Tag: <arm>/<trial>`; OpenCode requests are attributed by the proxy
  tag file, set to `OpenCode/<job>` for the OpenCode job only. Arms run as separate sequential jobs.
- Egress (amended before any counted trial, after the OpenCode smoke could not `apt-get` directly):
  every arm gets the same agent env `http_proxy`/`https_proxy`/`HTTP_PROXY`/`HTTPS_PROXY` =
  `http://host.docker.internal:7897` (host Clash) and `no_proxy`/`NO_PROXY` =
  `host.docker.internal,localhost,127.0.0.1`, so installs and in-task downloads have the same path for
  all arms and model traffic still goes to the metering proxy directly.

## Tasks

The 10 tasks of `tb2-subset.txt` (oracle 10/10 at reward 1.0 on this host, `HARBOR_SETUP.md`):
cobol-modernization, fix-git, cancel-async-tasks, sanitize-git-repo, vulnerable-secret,
fix-code-vulnerability, prove-plus-comm, git-multibranch, configure-git-webserver, overfull-hbox.
All images are amd64, run emulated on arm64.

## Protocol

- `harbor run --path ./tb2/terminal-bench -i <each task> -n 1 --timeout-multiplier 2 -k 2`: 2 attempts
  per task per arm, one trial at a time (the 3.83 GiB VM does not fit two 2 GB tasks reliably).
  4 arms × 10 tasks × 2 attempts = 80 trials.
- Order: A+fix, A+, A, OpenCode (one job each). No trial is re-run because of its score.
- INFRA: a trial whose failure is caused by infrastructure — gateway non-200 that ends the run, image
  pull/build timeout, a verifier download failure (`SSL_ERROR_SYSCALL` to GitHub), Docker error — is
  marked INFRA from its logs, may be re-run once (suffix noted), and is excluded from the pass rate if
  it is INFRA again. Classification is made from logs before looking at the arm comparison.

## Measures

Primary: Harbor reward per trial; pass rate per arm (reward 1.0), with per-task pairing.
Secondary: requests, prompt / completion / cached tokens (proxy), wall time per trial, exceptions,
Core run status (`completed`, `failed`, `max_steps`, …), CLI exit code.

## Decision rules

- With 20 trials per arm, a difference of fewer than 4 passed trials is reported as no detectable
  difference; no significance claim is made.
- A regression flag for A+ or A+fix vs A: ≥ 3 more failed trials, or any Core-caused crash / non-zero
  CLI exit not seen in A, or median prompt tokens per trial ≥ 1.5× A's on tasks both pass.
- LumenCortex vs OpenCode is reported as measured, with the same noise caveat.
