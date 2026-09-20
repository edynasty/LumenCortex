# Agent runtime

ModelWeave v0.3 is both a cognitive context runtime and a standalone coding agent.

## Loop

```text
Goal + recent tool evidence
  -> recompute Attention Light every reasoning step
  -> Active Promotion when granularity is too dense
  -> Active Subgraph
  -> bounded Working-Set Pager
  -> LLM
  -> tool_calls
  -> permission gate
  -> execute tools
  -> tool results
  -> LLM
  -> edit / inspect / test
  -> refresh evidence graph after mutations
  -> continue until final or max_steps
```

The loop is provider-independent. OpenRouter, Groq, DeepSeek official and any OpenAI-compatible local/API endpoint use the same runtime. Full session history is persisted, while only the current Active Subgraph plus recent complete tool rounds are sent back to the model.

## Built-in tools

- `read_file`
- `list_dir`
- `search_text`
- `write_file`
- `replace_in_file`
- `shell`
- `modelweave_context`
- `modelweave_ingest`

Tools declare a permission class: `read`, `write`, or `exec`. The CLI prompts before write/exec by default; `--yes` enables unattended coding runs.

All file tools are workspace-scoped and reject path traversal. Shell execution always starts with the workspace as cwd.

## Sessions

Agent conversations are stored under:

```text
.modelweave/sessions/<session-id>.json
```

A session stores messages, tool steps, provider/model, token usage, final status and timestamps. Use `--session ID` to resume.

## Provider configuration

### OpenRouter free router

```bash
export OPENROUTER_API_KEY=...
modelweave agent "inspect this project and run its tests" \
  --provider openrouter \
  --model openrouter/free \
  --yes
```

### Groq free tier

```bash
export GROQ_API_KEY=...
modelweave agent "find and explain the failing test" \
  --provider groq \
  --model openai/gpt-oss-120b \
  --yes
```

### DeepSeek official

```bash
export DEEPSEEK_API_KEY=...
modelweave agent "run tests, fix all failures and verify the result" \
  --provider deepseek \
  --model deepseek-flash \
  --yes
```

### DeepSeek V4 Flash free model through OpenRouter

```bash
export OPENROUTER_API_KEY=...
modelweave agent "run tests, fix all failures and verify the result" \
  --provider openrouter \
  --model deepseek/deepseek-v4-flash-0731:free \
  --yes
```

### Generic / local OpenAI-compatible server

```bash
export MODELWEAVE_BASE_URL=http://127.0.0.1:8000/v1
export MODELWEAVE_MODEL=Qwen/Qwen2.5-32B-Instruct-AWQ
export MODELWEAVE_API_KEY=dummy
modelweave agent "run the smoke tests" --provider generic --yes
```

For an unauthenticated local endpoint:

```bash
export MODELWEAVE_REQUIRE_API_KEY=false
```

## Real-model smoke test

The smoke test creates a temporary repository, ingests it, asks the selected LLM to locate a value through tools, and verifies the final answer.

```bash
OPENROUTER_API_KEY=... npm run smoke:free

# or
MODELWEAVE_PROVIDER=groq GROQ_API_KEY=... npm run smoke:free
```
