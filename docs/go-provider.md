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
provider, err := openai.New(openai.Config{
    Endpoint: "https://example.invalid/v1/chat/completions",
    APIKey:   os.Getenv("API_KEY"),
    Model:    "model-name",
})
if err != nil {
    return err
}

result, err := engine.RunAgent(ctx, sessionID, provider, runtime.AgentOptions{
    Policy: "workspace",
})
```

For non-standard providers, prefer setting `Endpoint` to the exact chat-completions URL. `BaseURL` is also supported and appends `/chat/completions`.

## Preview CLI

The preview CLI can now run the Go harness directly:

```bash
export LCX_ENDPOINT='https://your-provider.example/v1/chat/completions'
export LCX_API_KEY='...'
export LCX_MODEL='your-model'
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

| Variable | Purpose | Default |
| --- | --- | --- |
| `LCX_MODEL` | model name sent to provider | required |
| `LCX_ENDPOINT` | exact chat-completions endpoint | provider default when empty |
| `LCX_BASE_URL` | API base URL; appends `/chat/completions` | OpenAI-compatible default |
| `LCX_API_KEY` | bearer token | empty |
| `LCX_POLICY` | `read-only`, `workspace`, or `full` | `read-only` |
| `LCX_WORKFLOW` | Workflow Contract JSON path | none |
| `LCX_MAX_STEPS` | maximum agent turns for this run | 24 |
| `LCX_RECENT_MESSAGES` | recent Session messages exposed per request | 8 |
| `LCX_MAX_TOOL_CALLS_PER_STEP` | bounded tool fanout per turn | 8 |
| `LCX_MAX_TOKENS` | provider output-token request limit | provider default |
| `LCX_EVENTS` | print runtime events to stderr | false |
| `LCX_DISABLE_STREAMING` | use one JSON response instead of SSE | false |
| `LCX_DISABLE_RETRIES` | disable transient provider retries | false |

## Security

Do not put API keys in repository configuration or Session metadata. The preview CLI reads the key from the process environment and the provider only uses it to build the Authorization header.
