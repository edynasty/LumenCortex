# Go OpenAI-compatible provider

The Go preview runtime includes a bounded OpenAI-compatible Chat Completions provider at:

```text
github.com/edynasty/LumenCortex/provider/openai
```

It is intended for OpenAI-compatible gateways, local vLLM servers, and providers that expose compatible chat-completions requests.

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

## Go API

```go
engine, err := runtime.Open(runtime.Options{
    Workspace: workspace,
    // Optional: build the client of each resolved catalog model yourself. The default is
    // openai.FromModel, which reads the model's apiKey variable and requires a baseURL or endpoint.
    ProviderFactory: func(m config.ResolvedModel) (protocol.Provider, error) { /* ... */ },
})

result, err := engine.RunAgent(ctx, sessionID, runtime.AgentOptions{
    Policy:   "workspace",
    ModelRef: "local/m", // optional; empty keeps the session's pinned model
    Effort:   "high",    // optional; none|low|medium|high|max
})
```

`Engine.RunAgent` and `RunSupervisor.Start` take no provider. The model comes from the configuration (`lumencortex.json`):

| Rule logged | When | Effect on `sessions.model_ref` |
| --- | --- | --- |
| `model.user` | `AgentOptions.ModelRef` differs from the pinned model | written |
| `model.session` | the session is pinned and no different model was requested | none |
| `model.default` | the session has no pin (a new session, or one created before migration 0002) | the configured `model` is written |

Changing the global `model` therefore never moves an existing session. `failover` never writes the pin; a child session starts on `subagentModel`, else its parent's pin (never the model that last answered the parent). The Start builds a client for the session model and every `failover` model before it writes anything (`ErrUnknownModel`, `ErrModelWithoutToolCalling`, `ErrInvalidEffort`, a missing key variable or a missing `baseURL`/`endpoint` all leave the session as it was).

Each step record in `agent_steps.json` carries a `decisions` list (`model.*`, `model.candidates`, `effort.*`, `failover.<class>`); every entry holds the inputs its rule read and recomputes to its output with `agent.ReplayDecision`.

Provider errors are classified by the adapter as `protocol.ProviderError{Class, RetryAfter}`: `auth` and `bad_request` end the run as `failed` and never fail over; `capacity` fails over only to a candidate with a larger context; `availability` (HTTP 408/409/429/5xx, transport errors, request and idle timeouts) fails over to the next candidate and marks the model unavailable in the Engine's in-memory registry for `Retry-After` or 30 seconds. The adapter waits at most five seconds for a `Retry-After`. A cancelled run is `interrupted`, never failed over. `max_tokens` is sent only when `AgentOptions.MaxTokens` or the model's `limit.output` is set.

### Run ownership and host messages

A session run has one owner across processes. `RunSupervisor.Start` and `Engine.RunAgent` take `<state>/runs/<session-id>.lock` (an advisory kernel lock released when the owner exits, `kill -9` included) and return `runtime.ErrRunHeld`, having written nothing, when another Engine in this or another process holds it. Session deletion (`Engine.DeleteSession`) and `Engine.ApproveWorkflowGate` take the same lock. `status = running` is not evidence of a live run: `runtime.Open` reconciles a running session only after it has taken that session's free lock.

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
| `LCX_POLICY` | `read-only`, `workspace`, or `full` | `read-only` |
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

- `config.Load(workspace)` merges workspace over global: `model`, `subagentModel`, `failover` and `effort` replace; `providers` merge by id, field by field, with `headers` replacing the global set (an explicit `"headers": {}` clears it); `checks` merge by id. Unknown keys are errors.
- Trust: only a workspace listed (absolute path, compared after symlink resolution) in the **global** `trustedWorkspaces` may define `providers` or `checks`. Any other workspace may set only `model`, `failover`, `subagentModel` and `effort`, and only to models of global providers. Everything else it sets is dropped and reported in `Effective().Warnings` (code `untrusted_override`, `errors.Is(w.Err, config.ErrUntrustedOverride)`), with the `trustedWorkspaces` entry to add. The CLI prints these warnings to stderr.
- Credentials: `apiKey` must be `{env:NAME}`; literal keys are rejected on load and in `config.Save`. A workspace that changes a global provider's `baseURL` or `endpoint` must give its own `apiKey` (`config.ErrCredentialRedirect`), and a redirected provider never inherits the global `headers`. The variable is read only when the client is built (`ResolvedModel.ResolveAPIKey`).
- `config.Save(path, file, expectedDigest)` validates against the schema, takes a per-file lock, compares the file's digest with `expectedDigest` (`config.ErrConflict`), writes a temporary file and renames it. The digest comes from `Document.Digest` of `ReadDocument`/`Config.Global()`/`Config.Workspace()`; `""` means the file did not exist.
- Files in directories earlier builds used (`os.UserConfigDir()/lumencortex`, `~/.config/lumencortex` when not `GlobalDir`) are reported in `Effective().Warnings` (code `legacy_global_path`) with the path to move them to. They are not read, moved or deleted.

Consumed by the runtime: `model`, `failover`, `subagentModel`, `effort.default`, the provider's `baseURL`/`endpoint` (one of them is required: there is no default URL), `apiKey`, `headers`, `timeoutSeconds` (whole-request cap), `idleTimeoutSeconds`, `effort.escalation`, per model `modelID`, `limit`, `toolCalling` and `reasoning.format`/`efforts`/`echo`, and `checks` (the evaluator: completion gate, work-unit checks and the evidence rule; an untrusted workspace's checks are dropped with a `checks.<id>` warning).

## Security

Do not put API keys in repository configuration or Session metadata. Configuration holds `{env:NAME}` references only; the CLI reads the key from the process environment and the provider only uses it to build the Authorization header.
