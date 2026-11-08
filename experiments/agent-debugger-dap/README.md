# Agent Debugger DAP P0 experiment

This is an isolated, stdlib-only DAP client and deterministic C fixture. It does not modify the Core runtime, Agent tool catalog, Desktop, configuration or database.

## Run on macOS with Xcode Command Line Tools

```bash
clang -g -O0 -fno-omit-frame-pointer -o experiments/agent-debugger-dap/fixture \\
  experiments/agent-debugger-dap/fixture.c
DAP="$(xcrun --find lldb-dap)"
python3 experiments/agent-debugger-dap/probe.py "$DAP" \\
  experiments/agent-debugger-dap/fixture
```

The fixture intentionally computes `count - 3` where the expected multiplier is `count`. It prints `total=7 expected=43` and exits 2. The client verifies a source breakpoint, stack frames, local variables, two line steps, continue, process exit and termination. `evidence.json` is the captured successful run.

## Scope and limitations

- Proves real Xcode `lldb-dap` stdio interoperability on macOS.
- Exercises initialize, launch, initialized, setBreakpoints, configurationDone, stopped, stackTrace, scopes, variables, next, continue, exited and terminated.
- Does not yet prove attach, pause request, step-in/out, conditional/function breakpoints, evaluate, multi-thread races, host-crash cleanup, or Go/Python/JS adapters.
- This is an experiment harness, not production Core code. It omits robust concurrent request correlation, complete DAP header validation, process-group ownership and Core permission checks.
- No model was invoked. This demonstrates additional runtime facts available to an Agent, not a measured improvement in LLM task success, tokens or time.
