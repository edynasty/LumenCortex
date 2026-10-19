# UX Provider Completion - Anthropic Messages protocol (stage B core)

Date: 2026-10-19
Owner: CoreProtocol
Base commit: 465eda7 (CoreCredentials, stage C core) -> this change
Scope: Core repo only (`~/IdeaProjects/me/LumenCortex`)

## What landed

Genuine Anthropic Messages support in `provider/anthropic`, a package-selecting dispatcher
`provider.FromModel`, config/schema acceptance of `anthropic-messages`, and the runtime/CLI
cutover to the dispatcher.

### provider/anthropic (new package)

- `client.go` - HTTP client mirroring the openai adapter's runtime properties: SSE streaming with
  non-streaming fallback, bounded retries with backoff and the 5 s in-adapter Retry-After cap,
  request/idle timeouts (`TimeoutError`, availability class, never unwrapping a context error),
  max response bytes for raw and assembled responses, activity-reset idle timer, prompt context
  cancellation. Auth: `x-api-key` + `anthropic-version: 2023-06-01` (configured headers override
  any default, the version included; no bearer token). Endpoint: `endpoint` or `baseURL` +
  `/v1/messages`; no default URL (`ErrNoEndpoint`). HTTP error bodies are redacted of the API key
  before the error text is built.
- `wire.go` - request/response conversion. System messages -> top-level `system` (string for one,
  text blocks for several); alternating user/assistant `messages`; assistant tool calls ->
  `tool_use` blocks (input must be an object: non-object stored arguments become `{}` and the
  following tool_result reports the rejected call); tool rows -> `tool_result` blocks, consecutive
  rows and a following user row merged into one user message in order; tools -> `{name,
  description, input_schema}`; tool_choice auto/required(any)/none. `max_tokens` always sent
  (request value, else the model's resolved output limit, else config.DefaultOutput = 4096).
  Effort -> `thinking` (low/medium/high/max = 1024/4096/8192/16384 budget tokens clamped to
  max_tokens - 1024; below 2048 max_tokens thinking is omitted; while thinking is on temperature
  is dropped and a forced tool_choice is downgraded to auto). Streaming: message_start,
  content_block_start/delta/stop (text_delta, input_json_delta, thinking_delta, signature_delta),
  message_delta (stop_reason + usage), message_stop (ends the read without waiting for the
  connection), ping, SSE comments, unknown events ignored, `error` events classified by type.
  Thinking/redacted_thinking blocks -> opaque `protocol.Echo` (kind `anthropic-thinking`), resent
  byte for byte on the current turn's assistant rows only.
- `factory.go` - `Options{HTTPClient, DisableStreaming, DisableRetries, Credentials}` and
  `FromModel`, resolving the key through `config.CredentialResolver` (stage C contract).

Honesty notes (documented in the package doc): `is_error` is omitted because a protocol tool row
carries no success flag; cache usage fields are dropped because protocol.Usage has none.

### provider (new dispatcher)

`provider.FromModel(m, Options)` selects by `ResolvedModel.Package`: ""/openai-compatible ->
`openai.FromModel`, anthropic-messages -> `anthropic.FromModel`, anything else -> clear error.
`Options` = `{HTTPClient, DisableStreaming, DisableRetries, Credentials config.CredentialResolver}`,
forwarded unchanged.

### config

- `PackageAnthropicMessages` constant and `NormalizePackage` (canonicalizes "", legacy
  "@opencode/..." / "@ai-sdk/..." ids to openai-compatible, keeps anthropic-messages, rejects
  anything else with ErrInvalidConfig) - `config/types.go`.
- `Package` field on `ResolvedProvider` and `ResolvedModel`, populated and validated in
  `config.Provider()`/`Resolve()` - `config/resolve.go`.
- `docs/lumencortex.schema.json`: package enum += "anthropic-messages"; baseURL description
  documents both endpoint paths.
- Fixtures: `testdata/valid/anthropic-messages.json`, `testdata/invalid/package-unknown.json`;
  `schema_test.go` counts updated (5 valid, 14 invalid).

### Runtime and CLI cutover

- `runtime/models.go`: `defaultProviderFactory` = `provider.FromModel`; `Options.ProviderFactory`
  doc updated. Behavior for openai-compatible providers is unchanged (existing runtime tests
  unmodified and green).
- `cmd/lcx-go/provider.go`: `providerFactory` = `provider.FromModel` (nil Credentials: the CLI
  stays env-only; CoreCredentials' preflight secret-ref refusal and `!model.ToolCalling` refusal
  kept intact and their tests stay green).

## Reasoning boundary (AGENTS.md, design §4.6)

The runtime's echo flow was investigated: `run.turn` holds echoes in run memory only
(`internal/agent/transcript.go`), `protocol.Echo` fails closed on marshal/text/log, and rows the
loop persists never carry it. The regression lives in `provider/anthropic/engine_test.go`
(`TestEngineRunThroughFakeAnthropic`): a full agent run through the real Engine and default
factory (the dispatcher) against a fake Messages server proves (1) the thinking echo is resent
verbatim only on the next request of the same active tool flow, (2) a later run (new turn) sends
none, (3) no published event and nothing at rest (store, -wal, -shm) holds the thinking sentinel.
No runtime restructuring was needed.

## Verification (exact commands, observed)

- `go build ./...` - pass.
- `go vet ./...` - pass (no findings).
- `go test ./...` - full suite result captured at commit time (see commit message; openai
  assertions unmodified).
- New deterministic tests (httptest, no network): request golden shapes (text, tools,
  tool_result merging, system, max_tokens, thinking mapping incl. clamp/restrictions, echo
  verbatim), streaming assembly (interleaved blocks, split chunks/comments, redacted_thinking,
  message_stop before close), usage, every error class incl. 529 and Anthropic error bodies,
  mid-stream error events, Retry-After beyond/within cap, transport/malformed-stream, timeouts,
  cancellation mid-stream with a goroutine-leak check, key redaction in errors, dispatcher
  selection incl. Credentials resolver forwarding and unknown-package refusal, and the engine
  end-to-end run above.

## Opt-in real probes (skipped here)

`LCX_PROBE_ANTHROPIC_BASE_URL` + `LCX_PROBE_ANTHROPIC_KEY_ENV` + `LCX_PROBE_ANTHROPIC_MODEL`
(and the `LCX_PROBE_OPENAI_*` analogue) gate one bounded real request through the dispatcher
(`provider/probe_test.go`); they skip with a clear message naming the variables when unset.
Blocker on this machine: `~/.config/lumencortex/lumencortex.json` declares no providers (only
$schema + trustedWorkspaces) and the environment holds no ANTHROPIC/OPENAI key variables
(checked names only; CLAUDECODE is unrelated), so no real endpoint is configured to probe.

## Coordination

- Built on CoreCredentials' commit 465eda7 (config.CredentialResolver, ResolveAPIKey(r),
  openai.Options.Credentials, CLI preflight). Their owned files were touched only as agreed
  (types.go constant area, resolve.go Package field/population/validation) after their commit.
- The display layer masks the real constant name; files were byte-checked so the tree contains
  the true identifier (verified via `git grep`/builds).

## Commit

`feat(provider): Anthropic Messages protocol adapter and package dispatcher (stage B core)`
(hash recorded in the final report; only CoreProtocol-owned paths staged).
