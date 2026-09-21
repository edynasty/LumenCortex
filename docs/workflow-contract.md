# Workflow Contract Layer

LumenCortex separates three state domains:

```text
Workflow State     -> Facts / Action / Route / Gate / Outcome
Cognitive State    -> Evidence / Belief / Attention Light
Execution State    -> LLM / Tools / Shell / LSP / MCP / Sessions
```

The Workflow Contract Layer constrains progress without replacing model reasoning. The model remains free to analyze and choose an implementation strategy inside the current action.

## Contract file

```bash
lcx workflow validate examples/workflows/verified-code-fix.json
lcx agent "fix the failing tests" --workflow examples/workflows/verified-code-fix.json --yes
```

A workflow contains:

- `facts` — durable machine-verifiable state.
- `actions` — explicit stages.
- `allowedTools` — tools visible and executable in the current stage.
- `outcomes` — deterministic tool-result rules that write facts.
- `routes` — fact-driven transitions.
- `gates` — condition or human approvals.
- `terminal` + `completeWhen` — proof required before the Agent may finish.

Workflow state is stored inside normal Session metadata, so resume does not require a second database. The original definition is persisted with the Session; changing the contract during resume is rejected.

## Condition DSL

Conditions support `all`, `any`, `not`; selectors `fact`, `tool`, `ok`, `arg`, `result`; and comparators:

```text
equals  notEquals  exists  contains  matches  in
gt      gte        lt      lte
```

Example proving a real test process exited successfully:

```json
{
  "all": [
    { "tool": "shell" },
    { "arg": "command", "contains": "test" },
    { "result": "exitCode", "equals": 0 }
  ]
}
```

Tool execution success and command success are different. A shell tool can execute normally while the child process exits non-zero, so workflow outcomes inspect the parsed `exitCode`.

## Runtime enforcement

The workflow is not advisory prompt text.

- The provider receives only tools allowed by the current Action.
- Every tool call is checked again at execution time; if an earlier call transitions the workflow, later calls from the same model turn cannot use tools forbidden by the new Action.
- Tool Outcomes create facts with provenance in `factSources`.
- A model final answer is rejected until the current Action is terminal and all completion evidence/gates are satisfied.
- Human gates pause the Session as `waiting_gate`, avoiding extra LLM turns.

Human-gate flow:

```bash
lcx workflow status SESSION_ID
lcx workflow approve SESSION_ID GATE_ID --actor your-name
lcx agent --session SESSION_ID --yes
```

The Workflow Graph answers "what progress is legal and proven"; the Cognitive Graph answers "what evidence should the Agent remember and attend to".
