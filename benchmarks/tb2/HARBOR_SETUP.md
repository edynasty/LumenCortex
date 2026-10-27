# Harbor + Terminal-Bench 2.0 — installation and validation

Work dir: `/Users/tangxingpeng/IdeaProjects/me/LumenCortex/bench/`
Status: harness installed, dataset downloaded, oracle grading validated, container→host
connectivity verified. **No real model calls were made** (oracle only).

All commands below were executed on this machine unless explicitly marked
`NOT EXECUTED`.

---

## 1. Installed versions

| Component | Version | How it was determined |
|---|---|---|
| macOS | 27.0.1 (build 26A434), arm64 | `sw_vers`, `uname -m` |
| Harbor (`harbor`, `hb`, `hr`) | **0.25.0** | `harbor --version`; `uv tool list` → `harbor v0.25.0` |
| Harbor Python | CPython 3.12.12 (uv-managed) | `uv tool install harbor --python 3.12` |
| Docker CLI / Engine | 29.8.2, server `linux/arm64` | `docker version --format ...` |
| Docker Compose | v5.6.0 (`~/.docker/cli-plugins/docker-compose`) | `docker compose version` |
| Docker Desktop VM | 10 CPUs, 4,106,346,496 B RAM (~3.83 GiB) | `docker info --format 'CPUs=… Mem=…'` |
| Dataset | `terminal-bench@2.0`, **89 tasks** | `harbor download terminal-bench@2.0` |

Harbor package root used for all source citations below:

```
/Users/tangxingpeng/.local/share/uv/tools/harbor/lib/python3.12/site-packages/harbor/
```

(abbreviated `$HARBOR` in the citations).

---

## 2. Host prerequisites and quirks that had to be worked around

These are environment facts, not Harbor bugs, but every one of them breaks a run if
ignored.

1. **`all_proxy=socks5://127.0.0.1:7897` breaks Harbor at import/HTTP time.**
   The shell exports `ALL_PROXY`/`all_proxy` (Clash, SOCKS5) plus
   `HTTP(S)_PROXY=http://127.0.0.1:7897`. Harbor's `httpx` usage (litellm model-cost
   map, registry client, supabase auth) then raises
   `ImportError: Using SOCKS proxy, but the 'socksio' package is not installed.`
   Fix used for every command in this document:

   ```sh
   env -u all_proxy -u ALL_PROXY harbor …
   ```

   (`HTTP_PROXY`/`HTTPS_PROXY` are left in place; they are http:// proxies and work.)
2. **litellm fetches its model-cost map at import time** (`harbor/llms/litellm_config.py:7`
   → `import litellm`). A network outage at import produces a confusing traceback.
3. **Docker Hub pulls are slow and retry-heavy here** (Docker Desktop uses the system
   HTTP proxy plus registry mirrors from `~/.docker/daemon.json`). Measured pull times
   for single-arch `alexgshaw/*:20251031` images directly from Docker Hub:
   **6m15s – 26min** per image, with repeated `Retrying in Ns` layer retries. This is the
   single largest source of wall time in this setup and it *exceeded Harbor's default
   600 s `environment.build_timeout_sec`* on the first attempt (see §5, attempt 1).

   **Workaround (fast):** one of the mirrors already configured in `~/.docker/daemon.json`
   serves these images at ~15× the speed. Pull through the mirror and retag:

   ```sh
   docker pull docker.1ms.run/alexgshaw/<task>:20251031
   docker tag  docker.1ms.run/alexgshaw/<task>:20251031 alexgshaw/<task>:20251031
   docker rmi  docker.1ms.run/alexgshaw/<task>:20251031   # optional cleanup
   ```

   Measured: `prove-plus-comm` (1.29 GB) **1m04s** via `docker.1ms.run` vs **>26min**
   direct; the remaining six images 8–110 s each. Note `edynasty.asia:38082` (also in the
   mirror list) answered the manifest but failed with `unknown blob` on every retry, so
   use `docker.1ms.run`.
   Harbor then finds the image locally and does not pull at all.
4. **Container → github.com is intermittently reset** (`SSL_ERROR_SYSCALL` on
   `github.com:443`). It made one oracle verifier fail (§5, attempt 1). A retry
   succeeded; the host Clash proxy is reachable from containers as a fallback (§9).
5. Docker Desktop settings were **not** changed. Existing production containers
   (sub2api, sub2api-postgres, sub2api-redis, wechat-sort, codebridge-local-client,
   sub2api-usage-sync) were left untouched.

---

## 3. Exact install and dataset commands

```sh
mkdir -p /Users/tangxingpeng/IdeaProjects/me/LumenCortex/bench/jobs
cd /Users/tangxingpeng/IdeaProjects/me/LumenCortex/bench

# Harbor (uv tool, isolated venv, exposes harbor/hb/hr)
uv tool install harbor --python 3.12

harbor --version            # 0.25.0
harbor --help
harbor run --help
harbor agent list           # built-in agents (oracle, nop, opencode, codex, …)
harbor agent auth opencode  # connection contract for opencode
harbor agent auth codex     # connection contract for codex
harbor agent schema opencode

# Terminal-Bench 2.0 (legacy registry dataset name)
env -u all_proxy -u ALL_PROXY harbor download terminal-bench@2.0 --export -o ./tb2
# → 89 tasks, exported to ./tb2/terminal-bench/<task-name>/
```

The registry is the default one:

```
$HARBOR/constants.py:9  DEFAULT_REGISTRY_URL = "https://raw.githubusercontent.com/laude-institute/harbor/main/registry.json"
```

Full captured output of `harbor --version`, `harbor --help`, `harbor run --help` and
`harbor agent list` is saved verbatim in `bench/harbor-help.txt` (318 lines). The flags
that matter for this project, from `harbor run --help`:

| Flag | Meaning |
|---|---|
| `--agent/-a <name\|module.path:Class>` | built-in agent, custom import path, or `acp:<agent>` |
| `--model/-m` (repeatable) | model name |
| `--base-url` / `--api-key-name` / `--api-format` | inference endpoint override (`--api-format` ∈ `openai_chat_completions`, `openai_responses`, `anthropic_messages`, `google_generate_content`, `bedrock_converse`) |
| `--ak/--agent-kwarg key=value` | agent kwargs (`harbor agent schema <name>`) |
| `--ae/--agent-env KEY=VALUE` | env vars injected into the agent's exec |
| `--ve/--verifier-env KEY=VALUE` | env vars for verification |
| `--allow-agent-host` / `--allow-verifier-host` | extend network allowlists |
| `--path/-p`, `-i/--include-task-name`, `-x/--exclude-task-name`, `-l/--n-tasks` | dataset/task selection |
| `--n-concurrent/-n`, `--n-concurrent-agents` | concurrency |
| `--timeout-multiplier` + `--agent-`/`--verifier-`/`--agent-setup-`/`--environment-build-timeout-multiplier` | timeout scaling |
| `-o/--jobs-dir`, `--job-name`, `-k/--n-attempts` | output location and repeats |
| `--override-cpus` / `--override-memory` / `--cpus` / `--memory` | resource policy (`auto\|limit\|request\|guarantee\|ignore`) |
| `--print-config`, `--dry-run`, `--diff` | inspect before running |

`harbor dataset list` only prints a pointer to <https://hub.harborframework.com/datasets>;
there is no offline catalogue. Dataset resolution for `terminal-bench@2.0` goes through
that `registry.json`.

Layout of a downloaded task (`tb2/terminal-bench/<name>/`): `task.toml`,
`instruction.md`, `environment/`, `solution/`, `tests/`.

### Dataset facts (from `bench/tb2-tasks.json`)

* 89 tasks; difficulty: 4 easy, 55 medium, 30 hard.
* Every task declares `storage = "10G"`, `build_timeout_sec = 600.0`, and a
  prebuilt `docker_image = "alexgshaw/<task>:20251031"` (no Dockerfile build).
* Memory: 71 × `2G`, 16 × `4G`, 2 × `8G`. CPUs: 84 × 1, 3 × 2, 2 × 4.
* Agent/verifier timeouts: 600, 750, 900 (×99 declarations), 1200, 1800, 2400, 3600,
  7200, 12000 s.
* **No task declares a platform/architecture.** All images are single-arch
  `linux/amd64` (`docker manifest inspect` returns a v2 manifest, not an index;
  `docker image inspect … Architecture` = `amd64`). On this arm64 host every task runs
  under x86-64 emulation.
* Categories: software-engineering 26, system-administration 9, scientific-computing 8,
  security 8, data-science 8, debugging 5, file-operations 5, model-training 4,
  mathematics 4, data-processing 4, machine-learning 3, plus 5 singletons.

---

## 4. Resource policy used here (4 GB RAM, 10 CPUs)

Harbor maps task resources straight onto Docker Compose service limits, e.g. for a
2 GB/1 CPU task (`<tmp>/…-docker-compose-resources.json`, generated per trial):

```json
{"services": {"main": {"cpus": 1.0, "mem_limit": "2048m"}}}
```

Consequences:

* `--n-concurrent 1` is mandatory for 4 GB tasks (the VM only has ~3.8 GiB, so a single
  4 GB task already over-commits; see §5 `overfull-hbox`).
* `--n-concurrent 2` is only safe for 2 GB tasks and even then leaves ~0.5 GB for the
  host-side Harbor process, Docker Desktop, and the verifier.
* Emulation means a 1-CPU task is materially slower than on x86-64; TB2's own timeouts
  (900 s typical) are the binding constraint, not the machine.

---

## 5. Oracle validation

Oracle = Harbor's `oracle` agent, which copies the task's reference `solution/` into the
container and runs it. It exercises environment start, image pull, the agent phase, and
the real TB2 verifier — i.e. everything except model I/O. A reward of `1.0` proves the
task's grading is wired correctly on this host.

Command template (exact):

```sh
cd /Users/tangxingpeng/IdeaProjects/me/LumenCortex/bench
env -u all_proxy -u ALL_PROXY harbor run \
  --path ./tb2/terminal-bench \
  -i '<task-name>' [-i '<task-name>' ...] \
  --agent oracle -n 1 -o ./jobs --job-name <job-name> -y
```

Attempt history (chronological — includes the two failures):

| # | Task | Oracle reward | Trial wall time | Image pull (pre-pulled) | Notes |
|---|---|---|---|---|---|
| 1 | `cobol-modernization` | **0.0** | 34 s | 8m03s (`docker pull`, retries) | First attempt. Agent phase OK (COBOL compiled + ran under emulation). Verifier failed: `/tests/test.sh` downloads `uv` from `github.com` → `curl: (35) OpenSSL SSL_connect: SSL_ERROR_SYSCALL in connection to github.com:443`, then `uvx: command not found`. Network, not harness. |
| 2 | `cobol-modernization` | **1.0** | 40 s | – (image cached) | Same command, rerun. Full pass. |
| 3 | `fix-git` | **1.0** | 31 s | 16m33s | Pass first time. |
| 4 | `overfull-hbox` | **1.0** | 401.6 s | 6m15s | 2 CPU / 4 GB task on a 3.83 GiB VM — passes, but ~10× slower than the 2 GB tasks (LaTeX + emulation). |
| 5 | `oracle-smoke-3` (job) | – | cancelled at 8m31s | – | First attempt at the 3-task job. Hung in `docker compose up` pulling the image; killed manually after 8m31s (Harbor's 600 s environment-build timeout had already been exceeded). Recorded as `CancelledError` in `jobs/oracle-smoke-3/result.json`. |

### 5.1 Full results (10 distinct tasks, all 1.0)

Job `oracle-7more` (7 tasks, `-n 1`, 9m53s total) plus the earlier runs:

| Task | Oracle reward | Trial wall time | Image size | Notes |
|---|---|---|---|---|
| `cobol-modernization` | **1.0** | 40.2 s | 424 MB | Pass (2nd attempt; 1st was 0.0 from a GitHub TLS failure). |
| `fix-git` | **1.0** | 30.7 s | 310 MB | |
| `overfull-hbox` | **1.0** | 401.6 s | 312 MB | 2 CPU / 4 GB — slowest of the set (LaTeX under emulation). |
| `cancel-async-tasks` | **1.0** | 48.6 s | 120 MB | |
| `configure-git-webserver` | **1.0** | 284.7 s | 143 MB | |
| `fix-code-vulnerability` | **1.0** | 13.2 s | 460 MB | Fastest. |
| `git-multibranch` | **1.0** | 107.8 s | 618 MB | |
| `prove-plus-comm` | **1.0** | 78.3 s | 1.29 GB | Largest image; Coq proof under emulation. |
| `sanitize-git-repo` | **1.0** | 32.0 s | 360 MB | |
| `vulnerable-secret` | **1.0** | 29.0 s | 372 MB | |

**10/10 oracle passes, 0 exceptions, mean reward 1.000.**

Non-scoring control runs:

| Run | Outcome | Meaning |
|---|---|---|
| `probe-e2e` (`fix-git`, custom `probe_agent:ProbeAgent`) | reward 0.0, 27.8 s | Correct: the probe agent does nothing. Proves the custom-adapter path executes and that `AgentContext` reaches `result.json` (`n_input_tokens: 11`, `n_output_tokens: 7`, `cost_usd: 0.0`, `metadata.arch: "x86_64"`). |

Failures observed and their causes:

1. `oracle-smoke-3` — hung in `docker compose up` while pulling `cobol-modernization`
   (8m31s, killed). Cause: Docker Hub pull throughput, not Harbor. Harbor's own 600 s
   `environment.build_timeout_sec` would have aborted it.
2. `oracle-cobol` — reward `0.0`. Cause: the verifier downloads `uv` from GitHub and hit
   `curl: (35) OpenSSL SSL_connect: SSL_ERROR_SYSCALL in connection to github.com:443`,
   then `uvx: command not found`. Network, not harness; identical rerun passed.
3. No amd64-emulation failures, no OOM kills, no timeouts on the 10 tasks above.

### 5.2 What the oracle runs proved

* Image pull → compose start → agent exec → verifier exec → reward collection works on
  arm64 Docker Desktop with amd64 images under emulation (10/10 tasks, mean 1.000).
* The verifier grades correctly: the one failing attempt produced `0.0` with the
  verifier's own output preserved in
  `jobs/oracle-cobol/cobol-modernization__MEi3LQz/verifier/test-stdout.txt`, and the
  identical rerun produced `1.0`.
* Emulation works: `uname -m` inside a task container returns `x86_64` (captured by the
  probe agent), and CPU-bound tasks (`overfull-hbox` 402 s, `configure-git-webserver`
  285 s) still finish inside TB2's own timeouts.
* Oracle wall time per task is **13–402 s** (median ~40 s) **once the image is local**;
  cold image pull dominates everything else (see §2.3 for the mirror workaround).

---

## 6. Plugging in a custom agent (LumenCortex adapter contract)

### 6.1 Entry points

```sh
harbor run --agent <module.path>:<ClassName> …       # --agent accepts an import path
# or in a JobConfig YAML:
# agents:
#   - import_path: lumen_agent:LumenCortex
```

* CLI flag: `harbor run --agent/-a` accepts "a custom agent import path
  (`module.path:ClassName`)" — `harbor run --help`, *Agent* section.
* Resolution: `$HARBOR/agents/factory.py:25` `_import_agent_class` →
  `$HARBOR/utils/import_path.py:7` `import_symbol` uses plain
  `importlib.import_module`, so the module must be importable by the **Harbor process**
  (`PYTHONPATH=/abs/path/to/adapters harbor run …`, or install into the tool venv).
  **Verified:** `PYTHONPATH=$PWD/adapters harbor run --agent probe_agent:ProbeAgent
  --dry-run` → `Dry run OK — 1 trial(s)` (`bench/adapters/probe_agent.py`).
* Config model: `$HARBOR/models/trial/config.py:65` `AgentConfig` holds `name`,
  `import_path`, `model_name`, `base_url`, `model_api_key_name`, `model_api_format`,
  `skip_model_parsing`, `n_concurrent`, `skills`, `override_timeout_sec`,
  `override_setup_timeout_sec`, `max_timeout_sec`, `kwargs`, `mcp_servers`.

### 6.2 BaseAgent vs BaseInstalledAgent

`$HARBOR/agents/base.py:34` `class BaseAgent(ABC)` — the minimum surface:

| Member | Location | Requirement |
|---|---|---|
| `__init__(logs_dir, model_name=None, logger=None, mcp_servers=None, skills_dir=None, *, extra_env, base_url, model_api_key_name, model_api_format, skip_model_parsing, load_trajectory, environment_logs_dir, **kwargs)` | `base.py:144` | Call `super().__init__(…)`; `kwargs` becomes `self.options` via `parse_options`. |
| `name() -> str` (staticmethod, abstract) | `base.py:655` | Any string; built-ins use the `AgentName` enum (`models/agent/name.py:4`). A custom agent may return a plain string (the probe returns `"probe"`). |
| `version() -> str \| None` (abstract) | `base.py:659` | Reported in `result.json` → `agent_info.version`. |
| `async setup(environment)` (abstract) | `base.py:670` | Install the CLI, write config, register MCP/skills. Runs as `environment.default_user`; wrapped in `asyncio.wait_for(..., self._agent_setup_timeout_sec)` (`trial/trial.py:1889`). |
| `async run(instruction, environment, context)` (abstract) | `base.py:686` | Execute the agent; **populate `context` as you go** (comment at `base.py:693`: "populate the context as the agent executes in case of a timeout"). |
| `populate_context_post_run(context)` | `base.py:739` | Optional backfill after logs are synced to the host (used by opencode/codex to parse their JSON streams). |
| `import_path()` (classmethod) | `base.py:663` | `f"{cls.__module__}:{cls.__name__}"` — what you pass to `--agent`. |
| `capabilities: AgentCapabilities` | `agents/capabilities.py:8` | Declares `base_url`, `atif`, `resume`, `native_config`, `skills`, `mcp_servers`, … Anything you do not declare is refused by the framework (e.g. `config=` raises unless `native_config` is set, `installed/base.py:671`). |
| `MODEL_CONNECTION: ModelConnectionSpec \| None` | `agents/agent_auth_resolution.py:365` | **If `None`, passing `--base-url/--api-key-name/--api-format` raises** `"Agent '…' does not support inference connection overrides"` (`base.py:183`). Set it to get free env injection. |

`$HARBOR/agents/installed/base.py:354` `class BaseInstalledAgent(BaseAgent, ABC)` adds
what a CLI-in-a-container agent needs:

| Member | Location | Purpose |
|---|---|---|
| `async install(environment)` (abstract) | `installed/base.py:1117` | "Use `exec_as_root` for system packages and `exec_as_agent` for user-level installs." |
| `setup()` concrete impl | `installed/base.py:1126` | Creates `/installed-agent`, calls `install()`, then best-effort `get_version_command()`/`parse_version()`. |
| `_exec / exec_as_root / exec_as_agent` | `installed/base.py:999`, `:1059`, `:1072` | Wraps `environment.exec` with `set -o pipefail`, redaction, and error classification. |
| `ensure_system_dependencies(env, ("curl","bash","git",…))` | `installed/base.py:772` | Installs only what is missing; keys come from `SYSTEM_PACKAGES` (`installed/base.py:371`). |
| `_upload_config_text(...)` | `installed/base.py:1085` | Upload a rendered config, `chown` to the agent user, `chmod 600`. |
| `render_instruction()` / `prompt_template_path` | `installed/base.py:1110` | Optional Jinja2 wrapper; must contain `{{ instruction }}`. |
| `CLI_FLAGS` / `ENV_VARS` / `options_model` | `installed/base.py:368`, `:369`, `:370` | Two ways to expose `--ak key=value` kwargs. `options_model` (pydantic `InstalledAgentOptions`, `agents/options.py:68`) is the modern one; `Cli`/`Env` annotations compile fields to CLI flags / env vars. |
| `ERROR_PATTERNS` | `installed/base.py:495` | Regex → exception mapping (rate limit, auth, context window, safety refusal, network). Reuse it so failures are classified instead of surfacing as raw exit codes. |

There is **no install-script template file**. `install()` is plain Python that issues
`exec_as_agent(...)` shell commands; the "template" is whatever the agent writes to
`/installed-agent` or `~/.local/bin`. The opencode and codex installers are the two
reference implementations to copy (`opencode.py:410`, `codex.py:653`).

### 6.3 How the agent command runs in the container

There is no generic command wrapper: each agent builds its own shell string and hands it
to `exec_as_agent`, which runs `set -o pipefail; <command>` as the task's agent user
(`installed/base.py:1033`). Reference shapes:

* opencode (`opencode.py:908-926`):
  `opencode --model=<provider/model> run --format=json --thinking --dangerously-skip-permissions -- <instruction> 2>&1 </dev/null | stdbuf -oL tee /logs/agent/opencode.txt`
  with `env = dict(self.model_connection.env)` plus `OPENCODE_FAKE_VCS=git`,
  `XDG_DATA_HOME`, `XDG_STATE_HOME`.
* codex (`codex.py:2088-2105`):
  `codex exec --dangerously-bypass-approvals-and-sandbox --skip-git-repo-check --model <m> --json --enable unified_exec -- <instruction> 2>&1 </dev/null | tee /logs/agent/codex.txt`
  with `CODEX_HOME` and a generated `$CODEX_HOME/config.toml`.

Filesystem contract (`models/trial/paths.py:19` `EnvironmentPaths`):

```
/logs/agent/          <- host trial_dir/agent/  (write your output here)
/logs/verifier/
/logs/artifacts/
/tests/               <- copied in by the verifier
/solution/            <- copied in by the oracle agent only
```

Anything you write into `/logs/agent/` is synced back to the host trial directory, which
is why both agents `tee` their stdout there and parse it afterwards.

### 6.4 Token usage / cost reporting

`$HARBOR/models/agent/context.py:21` `class AgentContext(BaseModel)`:

```python
n_input_tokens:  int | None   # includes cache
n_cache_tokens:  int | None
n_output_tokens: int | None
cost_usd:        float | None
model_usage:     dict[str, ModelUsage] | None   # per-model breakdown, incl. subagents
rollout_details: list[RolloutDetail] | None
metadata:        dict[str, Any] | None
```

* Set these fields inside `run()` (best-effort, before returning) and/or in
  `populate_context_post_run()`.
* `model_usage` is backfilled from the ATIF trajectory when the agent does not populate
  it (`context.py:37-43`). `ModelUsage` (`context.py:8`) has the same token fields per
  model name plus `cost_usd`.
* opencode's implementation (`opencode.py:766-798`) is the pattern to copy: it parses
  each `step_finish` event, sums `tokens.input + tokens.cache.read`, `tokens.output`,
  `tokens.cache.read`, and `cost`, then writes `context.cost_usd`,
  `context.n_input_tokens`, `context.n_output_tokens`, `context.n_cache_tokens`.
* They land in `result.json` → `agent_result` (verified shape:
  `{"n_input_tokens": null, "n_cache_tokens": null, "n_output_tokens": null,
  "cost_usd": null, "model_usage": null, "rollout_details": null, "metadata": null}`
  for the oracle) and are aggregated into `result.json` → `stats`.

### 6.5 Timeouts

* Per-task agent/verifier timeouts come from `task.toml`
  (`[agent] timeout_sec`, `[verifier] timeout_sec`), scaled by
  `--timeout-multiplier` / `--agent-timeout-multiplier` / `--verifier-timeout-multiplier`
  / `--environment-build-timeout-multiplier`
  (`trial/trial.py:1694` `_compute_agent_timeout_sec`, `:225`).
* Agent setup timeout: `BaseAgent.DEFAULT_SETUP_TIMEOUT_SEC = 360.0`
  (`agents/base.py:65`), overridable with `--ak override_setup_timeout_sec=…` or
  `agent.override_setup_timeout_sec`.
* Hard caps: `agent.max_timeout_sec`, `verifier.max_timeout_sec`
  (`models/trial/config.py:116-118`).
* Exceeding them raises `AgentTimeoutError` / `AgentSetupTimeoutError` /
  `EnvironmentStartTimeoutError` (`trial/single_step.py:79`, `trial/trial.py:1855`).

### 6.6 Network policy

* Default for a task is `NetworkMode.PUBLIC` (allow-all) — `environments/docker/docker.py:208`.
  Nothing has to be opened for the agent to reach `host.docker.internal:<port>`.
* If a task declares a stricter policy, Harbor enforces it through an egress-control
  sidecar (`docker.py:1448` `_apply_network_policy`) and the trial plan in
  `trial/network_policy.py:130` `resolve_trial_network_plan`.
* `--allow-agent-host <host-or-cidr>` (repeatable) extends the *agent phase* allowlist;
  `--allow-verifier-host` does the same for verification; `--allow-environment-access`
  is the startup baseline. Use these instead of disabling egress control.

---

## 7. OpenCode with a custom [OI]-compatible endpoint

`$HARBOR/agents/installed/opencode.py:279` `class OpenCode(BaseInstalledAgent, ACPAgentMixin)`.

### 7.1 Install and model string

* Install (`opencode.py:410`): `ensure_system_dependencies(("curl","bash","coreutils","nodejs","npm"))`
  then `npm i -g opencode-ai@<version|latest>`; on musl/Alpine it uses the distro Node,
  otherwise it bootstraps Node via nvm (`nvm_node_install_snippet()`). Version pin via
  `--ak version=1.x.y`.
* Model selector: `--model provider/model-id` → passed to the CLI as
  `--model=<provider>/<model>` (`opencode.py:917`). `_execution_model_name()`
  (`opencode.py:396`) keeps the provider prefix.
* Env vars it uses: `OPENAI_API_KEY` (credential candidate list,
  `opencode.py:324`), `OPENAI_BASE_URL` / `OPENAI_API_BASE` (base-URL candidates),
  `OPENCODE_FAKE_VCS=git`, `XDG_DATA_HOME`, `XDG_STATE_HOME`, and
  `OPENCODE_CONFIG_CONTENT` in ACP mode (`opencode.py:444-448`).
* Supported API formats (`opencode.py:327`): `openai_chat_completions`,
  `anthropic_messages`.

### 7.2 Pointing it at a custom base URL

Two supported routes.

**(a) Flags — verified output.** With
`--model oi/deepseek-v4.1-flash --base-url http://host.docker.internal:18080/v1
--api-key-name OI_API_KEY --api-format openai_chat_completions`, Harbor's own venv
produced this command (`_build_register_config_command()`, `opencode.py:809`):

```sh
mkdir -p ~/.config/opencode && echo '{
  "provider": {
    "oi": {
      "models": { "deepseek-v4.1-flash": {} },
      "options": {
        "baseURL": "http://host.docker.internal:18080/v1",
        "apiKey": "{env:OPENAI_API_KEY}"
      },
      "npm": "@ai-sdk/openai-compatible"
    }
  }
}' > ~/.config/opencode/opencode.json
```

Notes:

* `--api-format openai_chat_completions` is what makes Harbor emit
  `"npm": "@ai-sdk/openai-compatible"`. **Without it the `npm` key is absent**
  (`opencode.py:238-246`) and OpenCode only knows the provider if its id is in
  OpenCode's built-in registry — so a made-up provider id such as `oi` requires the
  explicit `--api-format`.
* `--api-key-name OI_API_KEY` names the **host** variable Harbor reads; the value is
  injected into the container as `OPENAI_API_KEY` and referenced as
  `{env:OPENAI_API_KEY}` (empirically confirmed: `model_connection.env ==
  {'OPENAI_API_KEY': 'sk-test'}`). Passing `--api-key-name OPENAI_API_KEY` gives the
  same result with a less surprising name.
* The provider/model selector must contain a `/` (or the agent config must define a
  default provider), otherwise the generated block is skipped — see the guard at
  `opencode.py:229-235` and the `ValueError` at `opencode.py:100-104`.

**(b) `opencode_config` kwarg** (`OpenCodeOptions.opencode_config`, `opencode.py:274`)
— a dict merged over the generated config (`opencode.py:860-866`), e.g.:

```yaml
agents:
  - name: opencode
    model_name: oi/deepseek-v4.1-flash
    kwargs:
      opencode_config:
        provider:
          oi:
            npm: "@ai-sdk/openai-compatible"
            options:
              baseURL: http://host.docker.internal:18080/v1
              apiKey: "{env:OPENAI_API_KEY}"
```

### 7.3 Usage extraction

`opencode.py:766` `populate_context_post_run` → `_convert_events_to_trajectory`
(`opencode.py:551`) parses `opencode.txt` (the `--format=json` line stream) into ATIF and
maps `step_finish` events (`opencode.py:663-690`) into
`context.cost_usd / n_input_tokens / n_output_tokens / n_cache_tokens`. Error events are
turned into a `NonZeroAgentExitCodeError` (`opencode.py:924`).

Caveat for a metering proxy: OpenCode's `step_finish.cost` is computed by OpenCode from
its own model pricing table, so for a custom provider it will usually be `0`. Token
counts still flow through. If cost matters, take it from the proxy logs, not
`cost_usd`.

---

## 8. Codex with a custom endpoint

`$HARBOR/agents/installed/codex.py:119` `class Codex(BaseInstalledAgent, ACPAgentMixin)`.

* **Wire API:** `MODEL_CONNECTION.supported_api_formats = [ModelApiFormat.OPENAI_RESPONSES]`
  (`codex.py:151`). Passing `--api-format openai_chat_completions` is rejected
  (verified):

  ```
  ValueError: Unsupported API format 'openai_chat_completions'; this agent supports:
  openai_responses. Native default for provider 'openai': openai_responses.
  Run `harbor agent auth codex` to see supported formats and defaults.
  ```

  The check lives at `agents/agent_auth_resolution.py:632`.
* Credentials: `OPENAI_API_KEY`, `CODEX_API_KEY` (`codex.py:145`), injected as
  `OPENAI_API_KEY` (`codex.py:148`); or `CODEX_AUTH_JSON_PATH` /
  `CODEX_FORCE_AUTH_JSON` to use an `auth.json` (`codex.py:159`).
* Base URL: `--base-url` → `OPENAI_BASE_URL` (`codex.py:150`), and Harbor also writes
  `openai_base_url` into `$CODEX_HOME/config.toml` because "codex 0.118.0 only honors
  openai_base_url from config.toml, not the env var" (`codex.py:2028-2030`,
  `_build_effective_config` at `codex.py:1845`). Verified effective config for
  `--base-url http://host.docker.internal:18080/v1 --api-format openai_responses`:

  ```json
  {"model_provider": "openai", "openai_base_url": "http://host.docker.internal:18080/v1"}
  ```
* Command (`codex.py:2080`): `codex exec --dangerously-bypass-approvals-and-sandbox
  --skip-git-repo-check --model <m> --json --enable unified_exec -- <instruction>`.
* Usage: codex reports token counts in its `--json` event stream; `populate_context_post_run`
  parses `codex.txt`/sessions (`codex.py:1932+`).

### Can Codex use an [OI]-compatible chat-completions endpoint?

**Not through `--api-format`** — Harbor refuses it, and the Codex CLI's built-in
`openai` provider speaks the **Responses** wire API.

**But yes through a native `config`**: Codex's `config.toml` supports
`model_providers.<id>.wire_api = "chat"`, and Harbor passes a native config through
untouched when `--api-format` is *not* given (the `wire_api = "responses"` rewrite at
`codex.py:1872-1874` only fires when `self._model_api_format is not None`). Verified with
Harbor's own venv:

```python
Codex(logs_dir=…, model_name="deepseek-v4.1-flash",
      config={"model_provider": "oi",
              "model_providers": {"oi": {"name": "OI gateway",
                                         "base_url": "http://host.docker.internal:18080/v1",
                                         "wire_api": "chat",
                                         "env_key": "OPENAI_API_KEY"}}},
      model_api_key_name="OPENAI_API_KEY")
→ _build_effective_config(...) ==
  {'model_provider': 'oi',
   'model_providers': {'oi': {'name': 'OI gateway',
                              'base_url': 'http://host.docker.internal:18080/v1',
                              'wire_api': 'chat',
                              'env_key': 'OPENAI_API_KEY'}}}
```

So the two candidates are not symmetric:

| Agent | Custom endpoint via flags | Custom endpoint via native config | Wire API needed |
|---|---|---|---|
| `opencode` | **Yes** (`--base-url` + `--api-format openai_chat_completions`) | `opencode_config` overlay | chat-completions (or anthropic-messages) |
| `codex` | Only with an **OpenAI-Responses** endpoint | `config` with `model_providers.<id>.wire_api = "chat"` | Responses by default; `chat` only via native config |

If the OI gateway exposes only `/v1/chat/completions`, `opencode` is the zero-friction
choice and `codex` needs the native-config path (and should be validated with a `nop`/manual
smoke call before being used for a scored comparison).

---

## 9. Container → host connectivity (`host.docker.internal`)

Question: can a container reach a host port that is bound to **127.0.0.1 only** (as the
future metering proxy on `127.0.0.1:18080` will be)?

Test performed:

```sh
python3 -m http.server 18099 --bind 127.0.0.1        # host, loopback-only
curl -sS -o /dev/null -w '%{http_code}' http://127.0.0.1:18099/          # host → 200
docker run --rm curlimages/curl:latest \
  -sS -m 15 -o /dev/null -w '%{http_code}' \
  http://host.docker.internal:18099/                                    # → 200
```

Result: **works, HTTP 200.** Docker Desktop forwards `host.docker.internal` to the host's
loopback interface, so a `127.0.0.1`-bound proxy is reachable from containers without
rebinding it to `0.0.0.0` and without changing Docker Desktop settings. The test server
was stopped afterwards.

Corollary checks (same method):

* `https://github.com` from a container: 200 (but intermittently
  `SSL_ERROR_SYSCALL`, see §2.4).
* The host Clash proxy itself, `http://host.docker.internal:7897`, is reachable from a
  container: `HTTPS_PROXY=http://host.docker.internal:7897 docker run … curl https://github.com`
  → **200**. This is the fallback if a task's verifier needs GitHub and the direct path
  flakes:

  ```sh
  harbor run … --ve HTTPS_PROXY=http://host.docker.internal:7897 \
                 --ve HTTP_PROXY=http://host.docker.internal:7897
  ```

  (Use `--ae` instead of `--ve` to give the agent the same proxy.)

**Recommendation for the metering proxy:** keep it bound to `127.0.0.1:18080` on the
host and configure agents with `--base-url http://host.docker.internal:18080/v1`. No
`--allow-agent-host` entry is needed at the default `PUBLIC` network policy; add
`--allow-agent-host host.docker.internal` only if a task ships a restrictive policy.

---

## 10. Suggested configuration for the LumenCortex vs OpenCode comparison

Fixed inputs (same for both runs): `terminal-bench@2.0` subset, same `--model`, same
`--base-url`, same `n_concurrent`, same `--timeout-multiplier`.

`bench/tb2-subset.txt` holds the 10 tasks validated in §5 (one name per line). Harbor's
`-i/--include-task-name` is repeatable but takes a glob per flag, not a comma list, so
build the flags with shell expansion:

```sh
cd /Users/tangxingpeng/IdeaProjects/me/LumenCortex/bench
TASKS=$(sed 's/^/-i /' tb2-subset.txt | tr '\n' ' ')   # "-i a -i b …"
```

```sh
cd /Users/tangxingpeng/IdeaProjects/me/LumenCortex/bench

# 1. Pre-pull every image through the fast mirror (see §2.3).
#    Direct Docker Hub pulls take 6–26 min per image and blow the 600 s build timeout.
for t in $(cat tb2-subset.txt); do
  docker pull docker.1ms.run/alexgshaw/$t:20251031 &&
  docker tag  docker.1ms.run/alexgshaw/$t:20251031 alexgshaw/$t:20251031
done

# 2. OpenCode, custom OI endpoint through the metering proxy
env -u all_proxy -u ALL_PROXY harbor run \
  --path ./tb2/terminal-bench \
  -i task-a -i task-b -i task-c \
  --agent opencode \
  -m oi/deepseek-v4.1-flash \
  --base-url http://host.docker.internal:18080/v1 \
  --api-format openai_chat_completions \
  --api-key-name OPENAI_API_KEY \
  --ae OPENAI_API_KEY=sk-… \
  -n 1 --timeout-multiplier 2 \
  -o ./jobs --job-name oc-<model>-tb2

# 3. LumenCortex (custom adapter, see §6)
PYTHONPATH=$PWD/adapters env -u all_proxy -u ALL_PROXY harbor run \
  --path ./tb2/terminal-bench -i task-a -i task-b -i task-c \
  --agent lumen_adapter:LumenCortexAgent \
  -m deepseek-v4.1-flash \
  --base-url http://host.docker.internal:18080/v1 \
  --api-format openai_chat_completions \
  --ae OPENAI_API_KEY=sk-… \
  -n 1 --timeout-multiplier 2 \
  -o ./jobs --job-name lc-<model>-tb2
```

Notes:

* `--timeout-multiplier 2` is worth using: emulated x86-64 runs are slower than TB2's
  authors assumed, and a timeout scores the same as a wrong answer.
* `n_concurrent 1` for 2 GB tasks is the safe default here; 2 is the ceiling.
* `--n-attempts/-k` > 1 if you want variance; the reward aggregation is per-attempt.
* `harbor view jobs` renders the local viewer; `harbor analyze` inspects trajectories.

---

## 11. Blockers / open issues

| # | Issue | Impact | Status |
|---|---|---|---|
| 1 | Docker Hub pull throughput: 6m15s–26min per ~300 MB–1.3 GB image directly from Docker Hub, with layer retries; exceeds Harbor's default 600 s `build_timeout_sec` on a cold cache | First trial of every task fails/hangs unless the image is pre-pulled or `--environment-build-timeout-multiplier` is raised | **Resolved** via the already-configured `docker.1ms.run` mirror (8 s – 1m04s per image, §2.3). No Docker Desktop setting was changed. |
| 2 | Container → `github.com` intermittently `SSL_ERROR_SYSCALL` | TB2 verifiers that download tools (`uv` in `cobol-modernization`) score `0.0` spuriously | **Workaround**: retry, or `--ve HTTPS_PROXY=http://host.docker.internal:7897`. |
| 3 | Host `all_proxy=socks5://…` breaks Harbor's `httpx`/litellm imports | Every `harbor` invocation fails with a socksio ImportError | **Workaround**: `env -u all_proxy -u ALL_PROXY`. Could be removed permanently with `uv pip install --python $(uv tool dir)/harbor/bin/python socksio` (NOT EXECUTED). |
| 4 | 4 GB tasks (`overfull-hbox`, `rstan-to-pystan`, `mcmc-sampling-stan`, …) exceed the 3.83 GiB VM | OOM / very slow / timeout | **Unresolved** — requires more VM RAM (out of scope: Docker Desktop settings must not change). Exclude 4 GB/8 GB tasks from the comparison subset, or accept failures. |
| 5 | All TB2 images are `linux/amd64` only | Runs on arm64 under emulation: slower, and any task with architecture-sensitive timing/benchmarks is skewed | **Accepted**. Document it next to any score comparison. |
| 6 | OpenCode `step_finish.cost` is OpenCode's own estimate and is `0` for custom providers | `cost_usd` unusable for a custom gateway | **Workaround**: take cost from the metering proxy. |
| 7 | `codex` cannot take a chat-completions endpoint via `--api-format` | Not usable against a chat-only OI gateway without a native `config` | **Workaround documented** (§8); not exercised end-to-end (no model calls allowed in this task). |
