# Validation status

This document separates architecture claims from evidence.

## Automated core tests

Latest validated core suite:

- 136 core tests / 136 passed / 0 failed on Node.js 22; the Node.js 24 matrix job also passes
- Node.js 22 and 24 core matrix
- dedicated isolated Search / LSP / MCP / Subagent / TUI harness jobs
- MCP HTTP modern + legacy fallback + stdio transport coverage
- real overlapping Subagent concurrency assertion
- SQLite WAL / normalized Session / JSON migration coverage
- cross-process WAL Session concurrency with simultaneous reader coverage
- atomic multi-file `apply_patch` validation, ambiguity rejection and workspace-bound path coverage
- bounded multi-file `read_files` batching with traversal and duplicate-path rejection
- asynchronous shell execution, process-tree timeout termination and explicit abort coverage
- Agent cancellation propagation without retry, with interrupted Session persistence for resume
- live shell stdout/stderr streaming into Agent `tool.output` events before process completion
- standalone `doctor` provider diagnostics outside an initialized LumenCortex workspace
- SQLite status / integrity-check / checkpoint / journal maintenance coverage
- sparse Cognitive Git checkpoint/reopen coverage across 120+ commits
- explicit Repository/Runtime/SessionStore connection cleanup
- 100k-node SQLite FTS5/symbol search benchmark
- Workflow Contract validation for deterministic Facts/Action/Route/Outcome/Gate transitions
- Workflow Agent enforcement: per-Action tool visibility, execution-time recheck after mid-turn transitions, final-answer evidence gate, durable human-gate pause/approval/resume
- Workflow CLI validate/status/approve integration
- Cognitive control validation: algorithmic DecisionProvider, Jev/Laya-compatible `/v1/systemone` adapter shape, framework Think/effort routing, OpenRouter/Groq/DeepSeek effort mapping, ordered Category model chains, shared circuit breakers, provider-chain failover, failure signatures, and session model-latency telemetry
- Work Unit validation: dependency-cycle rejection, model/provider/category field rejection, required-evidence/verification completion gates, ordered activation, and Agent premature-final blocking
- Graph Governor validation: global analyzer candidates, model Curator JSON plans, reproduced-evidence archive protection, semantic plan validation, provenance-preserving canonicalization, branch preservation, global promotion, and reversible Cortex Epoch commits
- Storage-tier validation: SQLite schema-v2 additive backfill, indexed hot/warm/cold metadata, Attention access telemetry without graph-revision changes, protected evidence GC filtering, and derived-cache-only cold archive compaction
- Go cognitive-control parity validation: shared cognition-profile parsing, Algorithm Router, Jev/Laya-compatible System One HTTP signals, explicit-zero probability handling, ordered Category provider failover, shared circuit breakers, provider-specific reasoning effort, Think token-budget scaling, persistent Work Unit tools/evidence gates, and per-step provider/model traces
- Go Graph Governor validation: Node-parity Analyzer thresholds/formulas, deterministic safe plan, semantic Curator plan generation, deterministic validator, read-only shared SQLite graph snapshot, and no-mutation assertions

Covered behaviors include:

- cognitive graph integrity and reversible diffs,
- repository ingestion and source-change invalidation,
- Attention Light propagation and multi-light policies,
- manual Promotion and Active Promotion Controller,
- Cognitive Git commit/branch/merge/conflict/revert,
- Cognitive Git blame/cherry-pick/rebase,
- provider tool-call normalization,
- Agent Loop tool-call round trips,
- max-step protection,
- bounded Working-Set paging,
- a 20-tool-round / 21-reasoning-step long-loop runtime test,
- atomic cognitive worker commit/rollback,
- workspace path traversal protection.

## Long-task runtime test

The deterministic long-loop test intentionally separates runtime mechanics from model intelligence.

It performs:

```text
20 tool-call rounds
        +
21st LLM turn final answer
```

Assertions:

- Attention is recomputed on every reasoning turn.
- Full session history remains durable.
- Model working history contains only recent complete assistant/tool rounds.
- Tool results are not retained without the assistant tool-call message they answer.
- Working message count stays bounded while durable history grows.

This proves bounded-context loop mechanics. It does not prove that a weak model can solve a difficult coding task.

## Real local-model VM observations

Environment:

- GitHub-hosted Ubuntu VM
- local Ollama OpenAI-compatible endpoint
- Qwen tool-calling model

Observed with the earlier 1.7B ExpertTools model:

- endpoint health succeeded,
- LumenCortex provider compatibility succeeded,
- real `tool_calls` were received,
- `read_file` executed successfully,
- by step 3 the model received the correct hidden file content,
- the weak 1.7B model nevertheless repeated `read_file` and reached `max_steps`.

Interpretation:

The tool protocol and result feedback path worked. The observed failure was model policy/termination quality, not absence of the tool result.

A stronger Qwen3 4B VM validation is maintained in `.github/workflows/vm-real-agent-smoke.yml`.

A separate strict graph-memory proof in `.github/workflows/vm-long-memory-real-agent.yml` now passes on a GitHub-hosted Ubuntu VM with local Ollama and `qwen3:4b-instruct`:

- 7 real reasoning steps,
- step 1-4 each read exactly one distinct file,
- `recentRounds=1`, so the oldest tool result was no longer retained as ordinary recent chat history,
- the write step's Active Subgraph contained all 4 durable read Observation nodes,
- Active Promotion triggered after the fourth read,
- step 5 wrote the report from those values,
- step 6 ran an independent verifier,
- verifier printed `GRAPH_MEMORY_LONG_TASK_OK`,
- step 7 returned the final answer.

This is direct real-model evidence for the central design claim: paged-out tool evidence can remain durable in the Context Graph and be reactivated by the moving Attention Light when needed.


The standalone real coding workflow also completed successfully in run `35549612505` using local `qwen3:4b-instruct`:

- initial implementation tests failed 0/2 pass,
- the real model used `shell`, batch `read_files`, and `replace_in_file`,
- it repaired two independent implementation bugs,
- its own verification reached 2/2 passing tests,
- an independent second `npm test` also passed 2/2,
- test-file SHA256 hashes were unchanged,
- durable Session `session_e01a914ce26344bc` finished as `completed` with seven reasoning steps and persisted shell/edit evidence.

This closes the baseline real local-model coding gate. It does not imply parity with mature coding products on every repository or model.

## DeepSeek validation

Integration support exists for:

- official DeepSeek OpenAI-compatible API through the `deepseek` provider; its current default model is `deepseek-flash`, which DeepSeek currently serves as DeepSeek-V4.1-Flash,
- OpenRouter preset `openrouter-deepseek-free`, pinned to `deepseek/deepseek-v4-flash-0731:free` for the free V4 Flash 0731 route.

A public no-key community DeepSeek V4 Flash endpoint was tested from an Ubuntu VM and re-tested on 2026-09-20. Four consecutive health requests returned:

```text
HTTP 400
Bad Request: The endpoint is paused, ask a maintainer to restart it
```

Therefore there is currently **no claim of a successful real DeepSeek inference run without credentials**.

A credentialed real-model validation must use one of:

- `DEEPSEEK_API_KEY` with the official API, or
- `OPENROUTER_API_KEY` with a currently free DeepSeek V4 route.

The credentialed `.github/workflows/deepseek-v4-free-real-agent.yml` proof is manual-only and fail-closed: if `OPENROUTER_API_KEY` is missing, the workflow fails before the fixture is created. Therefore a green run from that workflow now means the real DeepSeek tool-call smoke and autonomous coding task both executed successfully; a missing credential can no longer produce a misleading green run.

## Benchmark snapshot

The current synthetic benchmark in CI uses 1,205 graph nodes:

```text
fullEstimatedTokens   = 393879
activeEstimatedTokens = 1989
selectedNodes         = 5
contextRatio          = 0.005
```

This is a routing/attention benchmark, not a model-quality benchmark.

## Coding-harness validation

Dedicated `harness-ci` runs isolated tests for:

- persistent SQLite FTS5/symbol retrieval and Attention Light seeding,
- Content-Length LSP JSON-RPC against a fake language server,
- MCP 2026 modern discovery and 2025 legacy fallback,
- dynamic MCP tool registration/calling,
- focused Subagents and parallel durable sessions,
- TUI frame/session/event rendering, including active-run cancellation controls and live stdout/stderr event formatting.

A separate 100k-node search benchmark is enforced by `search-benchmark`.

Latest successful synthetic run:

```text
nodes             = 100000
queries           = 50
index build       = 3317.38 ms
query p50         = 0.101 ms
query p95         = 0.189 ms
database size     = 79.61 MB
indexed terms     = 300010
indexed symbols   = 400000
single mutation   = 129.11 ms
```

The benchmark specifically stresses exact/symbol-heavy code lookup. It proves that this path no longer performs a full 100k-node scan per query; it does not substitute for semantic-retrieval quality evaluation.

A separate mutation performance gate fails if a single-node update on a 100k-node graph exceeds 500ms. The pre-optimization path measured about 2020ms. Recent successful CI runs observed 129.11ms, 207.87ms, and 212.90ms; the latest run measured 212.90ms. Treat these as runner-dependent measurements, not a fixed latency guarantee.

Index consistency is separately tested: graph mutations use optimistic SQLite revisions, stale graph writers are rejected, changed nodes enter a dirty queue, and Runtime incrementally synchronizes only stale FTS5/symbol rows before searching. SQLite WAL mode, normalized incremental Session persistence, shared Subagent SessionStore reuse, Cognitive Git reopen/reconstruction and one-time JSON migration are covered by dedicated storage tests.

## Still not validated / still planned

The following are not represented as complete features:

- embedding/vector semantic retrieval,
- cognitive branch <-> actual Git worktree transaction binding,
- transactional rollback of workspace edits,
- separate physical hot/warm/cold node stores or destructive graph GC beyond current indexed storage metadata/derived-cache compaction,
- full temporal validity querying,
- autonomous Governor scheduling and higher-order split/merge policy beyond the implemented Curator/validated semantic operations,
- live Jev/Laya provider validation and calibrated cognitive-routing benchmarks,
- reusable Skills layer,
- vision/browser tools,
- richer verifier plugins beyond the deterministic Workflow Contract condition/gate DSL.


- Associative Light validation: bounded Personalized PageRank neighborhood, structural-cut enforcement, hop/node/token limits, explicit Runtime mode, default weighted-mode preservation, and next-turn cognitive retrieval policy persistence across Agent resume.


- Retrieval-profile validation: default weighted/lexical equivalence, dependency and causal relation weighting, historical archived-seed access, unknown-mode fallback, and next-turn profile persistence.
- Context-diversity validation: default greedy preservation, optional bounded MMR replacement of redundant nodes, shared weighted/PPR selector behavior, token-budget enforcement, and invalid-parameter fallback.


- Embedding/Hybrid validation: OpenAI-compatible `/embeddings` wire shape, response-order normalization, persistent SQLite schema v3, content-hash incremental reuse, single-node re-embedding, model dimension protection, exact cosine ranking, deterministic RRF fusion, pure semantic candidate seeding into Attention, unconfigured-provider fallback, shared cognition-profile configuration, Agent next-turn async Hybrid routing, and CLI Hybrid search.
