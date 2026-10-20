# Go providers

The Go preview runtime includes two wire adapters and a package dispatcher that selects one per
catalog provider:

```text
github.com/edynasty/LumenCortex/provider          # dispatcher: FromModel, Options
github.com/edynasty/LumenCortex/provider/openai    # OpenAI-compatible chat completions
github.com/edynasty/LumenCortex/provider/anthropic # Anthropic Messages
```

`provider.FromModel(m, provider.Options{})` is the Engine's default `ProviderFactory`: it reads the
provider's `package` from `lumencortex.json` and builds the matching adapter. `openai-compatible`
(absent, empty and the legacy ids included) selects the chat-completions client for OpenAI-compatible
gateways, local vLLM servers and compatible providers; `anthropic-messages` selects the Anthropic
Messages client. Any other package value is refused by `config` (schema and `NormalizePackage`)
and by the dispatcher.

## Runtime properties

The provider is designed for long-running agent sessions:

- reuses a caller-supplied or shared `http.Client`,
- uses SSE streaming by default,
- bounds the raw/assembled response size,
- supports cancellation through `context.Context`,
- performs bounded retries for transient HTTP/network errors,
- honors `Retry-After` when it is larger than the local backoff,
- assembles streamed function/tool calls by index,
- can disable streaming for compatibility testing.

Provider response size is bounded independently from Session history. Session history remains SQLite-backed and the Agent Loop only loads its recent working window.

## Anthropic Messages adapter

`package: "anthropic-messages"` selects `provider/anthropic`. Differences from the chat-completions
adapter, all inside the adapter:

- authentication is `x-api-key` plus `anthropic-version: 2023-06-01`; configured `headers` override
  any default, the version included, and no `Authorization` bearer is ever sent;
- the endpoint is `endpoint`, else `baseURL` + `/v1/messages`;
- system messages become the top-level `system`; tool rows become `tool_result` blocks of one user
  message (consecutive results and a following user row merge, in order); assistant tool calls
  become `tool_use` blocks. A protocol tool row carries no success flag, so `is_error` is omitted
  rather than guessed - the result text states failures;
- `max_tokens` is required by the API, so it is always sent: the run's value, else the model's
  `limit.output`, else 4096;
- reasoning effort maps to the `thinking` parameter (low/medium/high/max = 1024/4096/8192/16384
  budget tokens, clamped to `max_tokens` - 1024; none omits it; a `max_tokens` below 2048 leaves
  no legal budget and thinking is omitted). While thinking is on, `temperature` is dropped and a
  forced `tool_choice` is downgraded to `auto`, as the API requires;
- thinking and redacted_thinking blocks come back as an opaque `protocol.Echo` (kind
  `anthropic-thinking`) and are resent byte for byte on the current turn's assistant rows, only
  within the active tool flow. They are run memory: never a row, step record, event, log line or
  recovery state (design §4.6);
- prompt caching usage (`cache_creation_input_tokens`, `cache_read_input_tokens`) has no protocol
  `Usage` field and is dropped;
- errors map to the same classes as the chat-completions adapter: 401/403 auth, 400 with a capacity
  marker or 413 capacity, 408/409/429/5xx/529 availability, anything else bad request; a mid-stream
  `error` event classifies by its error type; the API key never appears in an error text.


## Go API

```go
engine, err := runtime.Open(runtime.Options{
    Workspace: workspace,
    // Optional: build the client of each resolved catalog model yourself. The default is
    // provider.FromModel, which selects the adapter by the provider's package.
    ProviderFactory: func(m config.ResolvedModel) (protocol.Provider, error) { /* ... */ },
})

result, err := engine.RunAgent(ctx, sessionID, runtime.AgentOptions{
    Policy:   "workspace",
    ModelRef: "local/m", // optional; empty keeps the session's pinned model
    Effort:   "auto",    // optional; none|low|medium|high|max|auto
})
```

`Engine.RunAgent` and `RunSupervisor.Start` take no provider. The model comes from the configuration (`lumencortex.json`):

| Rule logged | When | Effect on `sessions.model_ref` |
| --- | --- | --- |
| `model.user` | `AgentOptions.ModelRef` differs from the pinned model | written |
| `model.session` | the session is pinned and no different model was requested | none |
| `model.default` | the session has no pin (a new session, or one created before migration 0002) | the configured `model` is written |

Changing the global `model` therefore never moves an existing session. `failover` never writes the pin; a read-only child starts on the first model of the `explore` agent's `models` list, an execution task on the first of `worker`'s, else on its parent's pin (never the model that last answered the parent). The Start builds a client for the session model and every `failover` model before it writes anything (`ErrUnknownModel`, `ErrModelWithoutToolCalling`, `ErrInvalidEffort`, a missing key variable or a missing `baseURL`/`endpoint` all leave the session as it was).

Each step record in `agent_steps.json` carries a `decisions` list (`model.*`, `model.candidates`, `effort.*`, `failover.<class>`); every entry holds the inputs its rule read and recomputes to its output with `agent.ReplayDecision`. `effort.auto` is the exception: its classifier input (the user and assistant text) is deliberately not persisted, so replay checks the decision's invariants (a known source, a concrete level at or below the ceiling, a non-negative input length) instead of re-running the classification.

### Auto effort

`effort.default: "auto"` or `AgentOptions.Effort: "auto"` resolves one concrete level per Start (one user turn), under the run lock and before the first request. An explicit concrete override wins; `auto` is an override value like any other. The resolved level is that run's default, so `effort.escalation` still raises it while the run is stuck (up to `escalation.max`) and `ClampEffort` still fits it to each candidate's `reasoning.efforts`.

Configuration:

```json
{
  "effort": {
    "default": "auto",
    "auto": {
      "model": "local/judge",   // optional; absent → heuristic only
      "max": "high",            // optional; default high; classifier result is capped here
      "timeoutSeconds": 8       // optional; default 8, 1..60
    }
  }
}
```

The classifier reads the turn's new input plus the previous substantive user request and the last assistant reply, each truncated to 2000 characters, so follow-ups such as "continue" are judged in context. With a judge model, one non-streaming, tool-less request goes through the normal provider factory (both wire protocols); it asks for a short JSON answer `{"level":"none|low|medium|high|max"}`, tolerates a fenced or bare level, and is bounded by `timeoutSeconds` and the run context. The judge call is not a transcript row, not a step, and writes no message; only the `effort.auto` decision is recorded. A cancelled run cancels the judge and writes nothing. A resumed turn with no new input reuses the level its earlier run recorded (`reused: true`) instead of calling the judge again.

A judge failure, timeout, unusable answer or absent model falls back to the deterministic heuristic. The decision records `source` (`judge` or `heuristic`) and `fallbackReason` (a class only: `no_model`, `timeout`, `unparsable`, `error`, or the adapter's error class); it never records the input text, the assistant text or the judge's reply.

Heuristic rules (deterministic; score → level):

| Signal | Contribution |
| --- | --- |
| length ≥ 600 characters | +1 |
| length ≥ 1500 characters | +2 instead of +1 |
| a fenced code block (```` ``` ````) | +1 |
| a stack trace / compiler-error marker (`panic:`, `Traceback`, `goroutine N [`, `FAIL`, `error TS…`, `at …(file:line:col)`) | +1 |
| a file reference (`path/to/file.go`, `*.ts`, …) | +1 |
| numbered or bulleted items | +1 step |
| a sequence word (`then`, `after that`, `first`, …) | +1 step |
| ≥ 2 distinct imperative verbs (`fix`, `refactor`, `migrate`, `run`, …) | +1 step |
| ≥ 4 distinct imperative verbs | +2 steps instead of +1 |
| 3 or more steps | +2 |
| ≥ 3 distinct imperative verbs | +2 |
| question-only (ends with `?`, ≤ 240 characters, no code/stack/file/list/imperative signal) | −1 |
| short continuation (`continue`, `go on`, `proceed`, …) | the previous request's level |

`score ≥ 3` → `high`, `score ≥ 1` → `medium`, otherwise `low`; the heuristic never returns `none` or `max`. A ceiling of `none` or `low` is therefore honored conservatively.

Per candidate, the capped level is fitted with `ClampEffort`. If the candidate's non-empty `reasoning.efforts` list has no level at or below `effort.auto.max`, no reasoning parameter is sent to it and the step logs rule `effort.auto_unavailable`. A model with no `efforts` list keeps the level (the provider decides). The ceiling bounds the classifier's result; an escalated level may still be fitted to the model's nearest supported level, as before.

The decision log carries `effort.auto` in the first step record of the run (inputs: source, fallback class, input length, ceiling, judge model ref, `reused`; output: the resolved level) and, per request, `effort.default`/`effort.override`/`effort.escalated`/`effort.capped` or `effort.auto_unavailable`.

Provider errors are classified by the adapter as `protocol.ProviderError{Class, RetryAfter}`: `auth` and `bad_request` end the run as `failed` and never fail over; `capacity` fails over only to a candidate with a larger context; `availability` (HTTP 408/409/429/5xx, transport errors, request and idle timeouts) fails over to the next candidate and marks the model unavailable in the Engine's in-memory registry for `Retry-After` or 30 seconds. The adapter waits at most five seconds for a `Retry-After`. A cancelled run is `interrupted`, never failed over. `max_tokens` is sent only when `AgentOptions.MaxTokens` or the model's `limit.output` is set
(chat-completions; the Anthropic adapter always sends one because the API requires it).

### Run ownership and host messages

A session run has one owner across processes. `RunSupervisor.Start` and `Engine.RunAgent` take `<state>/runs/<session-id>.lock` (an advisory kernel lock released when the owner exits, `kill -9` included) and return `runtime.ErrRunHeld`, having written nothing, when another Engine in this or another process holds it. Session deletion (`Engine.DeleteSession`) and `Engine.ApproveWorkflowGate` take the same lock. `status = running` is not evidence of a live run: `runtime.Open` reconciles a running session only after it has taken that session's free lock.

Concurrency (ADR-0008): `RunSupervisor.Start` takes one agent slot of the Engine after the lock and lease and before anything is written; a start beyond global `concurrency.maxAgents` (default 4, 1–16) returns `runtime.ErrAgentLimitExceeded` with the running count and the limit. `spawn_subagent` and `start_task` first refuse with `runtime.ErrSubagentLimitExceeded` once the parent's running children of both kinds, in any process, reach `concurrency.maxChildrenPerParent` (default 3, 1–8); the tool result tells the model to let a child finish and retry. Both limits are read from the configuration at every start: a saved change applies to the next start, a lower limit never stops a running agent, and nothing is queued. `Options.Budget.MaxAgents > 0` fixes the process limit for that Engine instead. `Engine.Health().Budget.MaxAgents` reports the limit in force.

At every Start, after the lock and before anything is appended, core reconciles the transcript: unanswered tool calls at the tail get a runtime tool row (`origin: tool_interrupted`), and a session still marked `running` becomes `interrupted` with kind `stale_run_recovered`.

Hosts add transcript rows only through `SessionOptions.Input` (the first row, written when the session is created; the goal when empty) and `AgentOptions.Input` (a user message appended by Start after Reconcile). `ApproveWorkflowGate` changes workflow state only; call Start afterwards to continue.

Rows the runtime writes carry `origin` in their stored JSON (`tool_interrupted`, `completion_blocked`, `resume`, `turn`). Provider adapters never send it.

A turn never spans runs or models (design §4.6). A Start without `Input` whose newest row is not a user row appends a `resume` user row in the Start transaction. Before each request the loop appends a `turn` user row, and logs a `turn.boundary` decision, when an assistant row after the last user row was not produced by the target model in this run (failover mid-turn).

Reasoning echo: with `reasoning.echo: reasoning_content` (the default for `reasoning.format: deepseek`, otherwise `none`) the OpenAI-compatible adapter returns the response's `reasoning_content` as an opaque `protocol.Echo` and sends it back on the current turn's assistant messages, only to the model that produced it. Echo lives in run memory only: it has no JSON, text or log form, is never written to a row, step record, event or checkpoint, and is gone when the run returns. A resumed run therefore starts a new turn and sends none.

For non-standard providers, prefer setting `Endpoint` to the exact chat-completions URL. `BaseURL` is also supported and appends `/chat/completions`.

## Preview CLI

The preview CLI can now run the Go harness directly:

The CLI takes its provider and model from `lumencortex.json` (see [Configuration](#configuration)):

```bash
export DEEPSEEK_API_KEY='...'   # the variable the config's {env:DEEPSEEK_API_KEY} names
export LCX_POLICY='workspace'

go run ./cmd/lcx-go agent "inspect this repository and fix the failing test"
```

Resume an existing Session:

```bash
go run ./cmd/lcx-go resume session_xxx
```

Use a Workflow Contract:

```bash
export LCX_WORKFLOW=examples/workflows/verified-code-fix.json
go run ./cmd/lcx-go agent "repair the failing test and prove it passes"
```

Human gate approval:

```bash
go run ./cmd/lcx-go approve session_xxx gate-id
go run ./cmd/lcx-go resume session_xxx
```

Set `LCX_EVENTS=true` to emit the bounded runtime event stream as JSON lines on stderr.

### Useful preview environment variables

`LCX_MODEL`, `LCX_ENDPOINT`, `LCX_BASE_URL`, `LCX_API_KEY` and `LCX_REASONING_FORMAT` are no longer read (M2.2). When one is set, the CLI prints the configuration key that replaces it: `model` and `providers.<provider>.models.<key>.modelID`, `providers.<provider>.settings.endpoint`, `.baseURL`, `.apiKey` (`"{env:NAME}"`) and `providers.<provider>.models.<key>.reasoning.format`.

| Variable | Purpose | Default |
| --- | --- | --- |
| `LCX_POLICY` | `read-only`, `workspace`, or `full`; the agent's ceiling can only narrow it | `read-only` |
| `LCX_AGENT` | catalog agent of this run; re-pins the session's agent (ADR-0009); `lcx-go agents` lists the merged catalog | the session's agent, else `agent` |
| `LCX_WORKFLOW` | Workflow Contract JSON path | none |
| `LCX_MAX_STEPS` | maximum agent turns for this run | 24 |
| `LCX_MAX_TOOL_CALLS_PER_STEP` | bounded tool fanout per turn | 8 |
| `LCX_MAX_TOKENS` | provider output-token request limit | provider default |
| `LCX_EVENTS` | print runtime events to stderr | false |
| `LCX_DISABLE_STREAMING` | use one JSON response instead of SSE | false |
| `LCX_DISABLE_RETRIES` | disable transient provider retries | false |

## Configuration

One schema, [`lumencortex.schema.json`](lumencortex.schema.json), describes both configuration files. Package `config` (`github.com/edynasty/LumenCortex/config`) is the only reader and writer:

| File | Location |
| --- | --- |
| global `lumencortex.json`, `mcp.json`, `skills/` | `GlobalDir`: `$XDG_CONFIG_HOME/lumencortex` when `XDG_CONFIG_HOME` is an absolute path, else `~/.config/lumencortex`, on every OS |
| workspace `lumencortex.json` | `<workspace>/lumencortex.json` |

- `config.Load(workspace)` merges workspace over global: `model`, `failover` and `effort` replace; `providers` merge by id, field by field, with `headers` replacing the global set (an explicit `"headers": {}` clears it); `checks` merge by id; `agents` merge by id (below). Unknown keys are errors.
- Trust: only a workspace listed (absolute path, compared after symlink resolution) in the **global** `trustedWorkspaces` may define `providers`, `checks` or agents. Any other workspace may set only `model`, `failover`, `effort` and the `models`/`effort` of an agent that already exists, and only to models of global providers. `effort.auto.model` is a model reference too: an untrusted workspace may name only a global provider's model; otherwise the judge model is dropped with a warning and the heuristic classifier applies. Everything else a workspace sets is dropped and reported in `Effective().Warnings` (code `untrusted_override`, `errors.Is(w.Err, config.ErrUntrustedOverride)`), with the `trustedWorkspaces` entry to add. The CLI prints these warnings to stderr.
- Agents (ADR-0009): `agents` is the agent catalog. Built-ins `ask` (conversation, read-only, no delegation), `agent` (conversation, no cap), `explore` (read-only children) and `worker` (execution tasks) always exist; a file may set only their `instructions`, `effort` and `models`. Up to 16 custom agents with `name`, `description`, `use` (`conversation`/`delegation`/`both`), `policy` and `delegation` ceilings, `instructions` (≤16 KiB), `effort` and an ordered `models` list (≤8, unique, each `{model, effort?}`). A delegation agent must cap `policy` at `read-only` or `workspace` and describe itself. Every listed model must be in the merged catalog and the first must call tools. A trusted workspace replaces a custom agent whole and overrides a built-in field by field. `Effective().Agents` lists the merged catalog (built-ins first) with each agent's `Source` and `Digest`. An agent without its own list pins `model` on a new session and falls back through `failover`, as before agents had lists.
- Agents at Start (ADR-0009 A2): `AgentOptions.Agent` names the thread's agent and `SessionInfo.Agent` reports it. A session pins its agent like its model (rules `agent.user`/`agent.session`/`agent.default`); switching agent re-pins the model to the new agent's first model (`model.agent`) unless the same Start sets `ModelRef`. An agent that is not in the catalog, a delegation-only agent requested for a thread, and a session whose pinned agent was removed are refused before anything is written (`ErrUnknownAgent`, `ErrAgentNotForConversation`); a removed agent never falls back to another one. Candidates are the pinned model, then the agent's list (a child whose agent has none uses its parent agent's list, else `failover`); each candidate's effort is its list entry, else the agent's `effort`, else `effort.default`, and `AgentOptions.Effort` overrides all of them for that thread only: children do not inherit it. The agent's `policy`/`delegation` ceilings cap the host's grant (one `agent` decision records grant, ceilings and result); its instructions are appended after the default and skills prompts and logged as digest and length only (`instructions.appended`). `AgentOptions.Instructions` was removed: instructions are configuration. The CLI reads `LCX_AGENT`.
- Agents for children (ADR-0009 A3): `spawn_subagent {goal, agent?}` offers the read-only delegation agents, `start_task {objective, agent?}` the workspace ones, as a closed enum plus `- id: description` lines; omitted, they run `explore`/`worker`. The child pins the agent at its first Start. A proposal records the agent and the proposing run's bounds, and `StartProposedTask` applies them. An agent that is no longer eligible is refused (`ErrUnknownAgent`, `ErrAgentUseMismatch`) before anything is created. `SubagentNode.agent` and `TaskResult.agent` name it.
- `subagentModel` is retired: a file that sets it fails to load with `config.ErrSubagentModelRetired`, which names the replacement. `config.MigrateSubagentModel(path)` rewrites such a file (the model becomes the `explore` and `worker` list unless the file already gives one) through `Save` with the read digest.
- Global only: `concurrency` bounds the machine, so it is read from the global file alone; a workspace file that sets it, trusted or not, is reported with code `global_only` and ignored. `Effective().Concurrency` carries the global values with their defaults applied.
- Credentials: `apiKey` must be `{env:NAME}`; literal keys are rejected on load and in `config.Save`. A workspace that changes a global provider's `baseURL` or `endpoint` must give its own `apiKey` (`config.ErrCredentialRedirect`), and a redirected provider never inherits the global `headers`. The variable is read only when the client is built (`ResolvedModel.ResolveAPIKey`).
- `config.Save(path, file, expectedDigest)` validates against the schema, takes a per-file lock, compares the file's digest with `expectedDigest` (`config.ErrConflict`), writes a temporary file and renames it. The digest comes from `Document.Digest` of `ReadDocument`/`Config.Global()`/`Config.Workspace()`; `""` means the file did not exist.
- Files in directories earlier builds used (`os.UserConfigDir()/lumencortex`, `~/.config/lumencortex` when not `GlobalDir`) are reported in `Effective().Warnings` (code `legacy_global_path`) with the path to move them to. They are not read, moved or deleted.

Consumed by the runtime: `model`, `failover`, the first model of `agents.explore.models`/`agents.worker.models` (child sessions), `effort.default` (including `auto`), `effort.auto` (`model`, `max`, `timeoutSeconds`), `concurrency` (`maxAgents`, `maxChildrenPerParent`), the provider's `baseURL`/`endpoint` (one of them is required: there is no default URL), `apiKey`, `headers`, `timeoutSeconds` (whole-request cap), `idleTimeoutSeconds`, `effort.escalation`, per model `modelID`, `limit`, `toolCalling` and `reasoning.format`/`efforts`/`echo`, and `checks` (the evaluator: completion gate, work-unit checks and the evidence rule; an untrusted workspace's checks are dropped with a `checks.<id>` warning). The rest of the agent catalog (thread agents, ceilings, per-candidate effort, choosing an agent for a child) is consumed from A2/A3 of ADR-0009.

## Security

Do not put API keys in repository configuration or Session metadata. Configuration holds `{env:NAME}` references only; the CLI reads the key from the process environment and the provider only uses it to build the Authorization header.
