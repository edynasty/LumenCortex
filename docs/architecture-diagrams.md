# Architecture diagrams

This file is the visual architecture index.

Reusable Mermaid sources are stored under `docs/diagrams/`.

## Legend

- navy: entry/user surface,
- indigo: Workflow and policy control,
- cyan: cognitive retrieval/attention,
- green: execution,
- orange: host/external integration,
- purple: durable persistence,
- dashed gray: planned/incomplete.

## System architecture

Question answered:

> What are the major runtime layers, and how do they interact?

```mermaid
flowchart TB
  classDef entry fill:#0f172a,stroke:#38bdf8,color:#f8fafc,stroke-width:1.5px
  classDef control fill:#eef2ff,stroke:#6366f1,color:#1e1b4b,stroke-width:1.5px
  classDef cognitive fill:#ecfeff,stroke:#0891b2,color:#164e63,stroke-width:1.5px
  classDef exec fill:#f0fdf4,stroke:#16a34a,color:#14532d,stroke-width:1.5px
  classDef integ fill:#fff7ed,stroke:#ea580c,color:#7c2d12,stroke-width:1.5px
  classDef store fill:#faf5ff,stroke:#9333ea,color:#581c87,stroke-width:1.5px
  classDef planned fill:#f8fafc,stroke:#94a3b8,color:#475569,stroke-dasharray:5 4

  subgraph ENTRY["Entry Surface"]
    TUI["TUI"]:::entry
    CLI["CLI / Chat"]:::entry
    PAR["Parallel Sessions"]:::entry
  end

  subgraph ORCH["Orchestration & Control"]
    AGENT["Agent Loop"]:::exec
    WF["Workflow Contract<br/>Facts · Action · Route · Outcome · Gate"]:::control
    POLICY["Permission Policy<br/>read-only · workspace · full"]:::control
    SESSION["Durable Session<br/>Messages · Steps · Usage · Status"]:::store
  end

  subgraph COG["Cognitive Plane"]
    SEARCH["FTS5 + Symbol Retrieval"]:::cognitive
    LIGHT["Attention Light"]:::cognitive
    ACTIVE["Active Subgraph"]:::cognitive
    PROMOTE["Active Promotion"]:::cognitive
    GRAPH["Persistent Context Graph<br/>Evidence · Belief · Task · Abstraction"]:::store
    CGIT["Cognitive Git"]:::store
  end

  subgraph EXEC["Execution Plane"]
    LLM["LLM Provider"]:::exec
    TOOLSET["Per-step Tool Working Set"]:::exec
    TOOLS["Workspace Tools"]:::exec
    SUB["Subagents"]:::exec
  end

  subgraph INTEG["External / Runtime Integration"]
    SHELL["Shell / Tests"]:::integ
    LSP["LSP"]:::integ
    MCP["MCP"]:::integ
    REPO["Workspace / Repository"]:::integ
  end

  subgraph STORE["Persistence"]
    SQLITE["SQLite WAL<br/>Graph · Sessions · Journal · Search · Cognitive Git"]:::store
    WORKTREE["Transactional Git Worktree Isolation"]:::planned
    VECTOR["Embedding / Vector Index"]:::planned
  end

  TUI --> AGENT
  CLI --> AGENT
  PAR --> AGENT
  SESSION <--> AGENT
  WF --> AGENT
  POLICY --> TOOLSET
  AGENT --> SEARCH --> LIGHT --> ACTIVE
  GRAPH --> LIGHT
  ACTIVE --> AGENT
  ACTIVE --> PROMOTE --> GRAPH
  GRAPH <--> CGIT
  AGENT <--> LLM
  AGENT --> TOOLSET --> TOOLS
  AGENT --> SUB
  TOOLS --> SHELL
  TOOLS --> LSP
  TOOLS --> MCP
  TOOLS --> REPO
  SHELL --> REPO
  REPO --> GRAPH
  GRAPH --> SQLITE
  SESSION --> SQLITE
  CGIT --> SQLITE
  SEARCH --> SQLITE
  VECTOR -.-> SEARCH
  WORKTREE -.-> REPO
```

Source: `docs/diagrams/system-architecture.mmd`.

## Agent execution logic

Question answered:

> What exactly happens from a user goal until completion, pause, or resume?

```mermaid
flowchart TD
  classDef start fill:#0f172a,stroke:#38bdf8,color:#f8fafc
  classDef decision fill:#fff7ed,stroke:#f97316,color:#7c2d12
  classDef control fill:#eef2ff,stroke:#6366f1,color:#312e81
  classDef cognitive fill:#ecfeff,stroke:#0891b2,color:#164e63
  classDef action fill:#f0fdf4,stroke:#16a34a,color:#14532d
  classDef stop fill:#fef2f2,stroke:#dc2626,color:#7f1d1d

  START["User Goal / Resume Session"]:::start --> LOAD["Load durable Session<br/>+ optional Workflow Contract"]:::control
  LOAD --> GATE0{"Human gate waiting?"}:::decision
  GATE0 -- yes --> PAUSE["Persist waiting_gate<br/>Return without another LLM call"]:::stop
  GATE0 -- no --> FOCUS["Derive step focus"]:::cognitive
  FOCUS --> RETRIEVE["Indexed retrieval<br/>FTS5 · symbols · recent evidence"]:::cognitive
  RETRIEVE --> LIGHT["Move Attention Light"]:::cognitive
  LIGHT --> ACTIVE["Build finite Active Subgraph<br/>under token budget"]:::cognitive
  ACTIVE --> WORK["Build bounded working messages<br/>system + workflow + active graph + recent rounds"]:::control
  WORK --> TOOLS["Compute current Action tool allowlist"]:::control
  TOOLS --> LLM["LLM reasoning"]:::action
  LLM --> KIND{"Tool calls or final?"}:::decision

  KIND -- tool calls --> CHECK["Execution-time tool boundary check"]:::control
  CHECK --> EXEC["Execute Tool<br/>read · edit · shell · LSP · MCP"]:::action
  EXEC --> OBS["Record tool observation<br/>and provenance"]:::cognitive
  OBS --> OUT["Evaluate Workflow Outcomes<br/>write Facts"]:::control
  OUT --> ROUTE["Evaluate Route / Gate"]:::control
  ROUTE --> MUT{"Workspace changed?"}:::decision
  MUT -- yes --> INGEST["Re-ingest reality<br/>invalidate stale dependent cognition"]:::cognitive
  MUT -- no --> SAVE
  INGEST --> SAVE["Persist Session step + workflow state"]:::control
  SAVE --> GATE1{"Human gate reached?"}:::decision
  GATE1 -- yes --> PAUSE
  GATE1 -- no --> FOCUS

  KIND -- final --> FINAL{"Terminal Action and<br/>completion evidence satisfied?"}:::decision
  FINAL -- no --> REJECT["Reject premature completion<br/>persist corrective turn"]:::stop
  REJECT --> FOCUS
  FINAL -- yes --> DONE["Persist completed Session<br/>optional Cognitive Commit"]:::start
```

Source: `docs/diagrams/agent-execution-flow.mmd`.

See [Execution flow](execution-flow.md) for step-by-step semantics.

## Workflow and Cognitive Graph dual control plane

Question answered:

> Why are Workflow Contract and Context Graph separate instead of being one graph?

```mermaid
flowchart LR
  classDef wf fill:#eef2ff,stroke:#6366f1,color:#312e81,stroke-width:1.5px
  classDef cog fill:#ecfeff,stroke:#0891b2,color:#164e63,stroke-width:1.5px
  classDef exec fill:#f0fdf4,stroke:#16a34a,color:#14532d,stroke-width:1.5px
  classDef store fill:#faf5ff,stroke:#9333ea,color:#581c87,stroke-width:1.5px

  GOAL["Goal"]:::exec --> AGENT["Agent Loop"]:::exec

  subgraph WF["Workflow Control Plane — what is legal / proven?"]
    FACTS["Facts"]:::wf --> ACTION["Current Action"]:::wf
    ACTION --> ROUTE["Route"]:::wf
    ACTION --> ALLOW["Allowed Tools"]:::wf
    OUT["Tool Outcomes"]:::wf --> FACTS
    GATE["Condition / Human Gate"]:::wf --> ACTION
  end

  subgraph COG["Cognitive Control Plane — what should be remembered / attended?"]
    EVID["Evidence"]:::cog --> GRAPH["Context Graph"]:::store
    BELIEF["Belief / Hypothesis"]:::cog --> GRAPH
    GRAPH --> RET["Retrieval"]:::cog
    RET --> LIGHT["Attention Light"]:::cog
    LIGHT --> ACTIVE["Active Subgraph"]:::cog
    ACTIVE --> PROMOTE["Promotion / Drill-down"]:::cog
    PROMOTE --> GRAPH
  end

  AGENT --> ACTION
  ACTION --> AGENT
  AGENT --> ACTIVE
  ACTIVE --> AGENT
  ALLOW --> EXEC["Tool Execution"]:::exec
  AGENT --> EXEC
  EXEC --> OUT
  EXEC --> EVID
  GATE --> AGENT

  NOTE["Workflow never replaces cognition.<br/>Cognition never overrides workflow legality."]:::store
  WF --- NOTE --- COG
```

Source: `docs/diagrams/workflow-cognitive-dual-plane.mmd`.

The invariant is:

```text
Workflow: what is legal / proven
Cognition: what is remembered / attended
Execution: what actually happens
```

## Persistence and data flow

Question answered:

> What survives process restart and where is it stored?

```mermaid
flowchart TB
  classDef db fill:#faf5ff,stroke:#9333ea,color:#581c87
  classDef runtime fill:#f0fdf4,stroke:#16a34a,color:#14532d
  classDef cog fill:#ecfeff,stroke:#0891b2,color:#164e63
  classDef ctrl fill:#eef2ff,stroke:#6366f1,color:#312e81
  classDef planned fill:#f8fafc,stroke:#94a3b8,color:#475569,stroke-dasharray:5 4

  subgraph RUNTIME["Runtime State"]
    SESSION["Session<br/>goal · status · usage"]:::runtime
    MSG["Messages"]:::runtime
    STEP["Agent Steps"]:::runtime
    WF["Workflow Snapshot<br/>definition · facts · factSources · history"]:::ctrl
    JOURNAL["Runtime Journal"]:::runtime
  end

  subgraph COG["Cognitive State"]
    NODE["Graph Nodes"]:::cog
    EDGE["Graph Edges"]:::cog
    COMMIT["Cognitive Commits / Refs"]:::cog
    SYMBOL["Symbols / FTS5"]:::cog
  end

  subgraph SQLITE[".lumencortex/lumencortex.db — WAL"]
    SESS_T["sessions"]:::db
    MSG_T["session_messages"]:::db
    STEP_T["agent_steps"]:::db
    NODE_T["graph_nodes"]:::db
    EDGE_T["graph_edges"]:::db
    CGIT_T["cognitive_commits / cognitive_refs"]:::db
    SEARCH_T["symbols / search_documents / node_fts"]:::db
    JR_T["journal"]:::db
  end

  SESSION --> SESS_T
  WF --> SESS_T
  MSG --> MSG_T
  STEP --> STEP_T
  JOURNAL --> JR_T
  NODE --> NODE_T
  EDGE --> EDGE_T
  COMMIT --> CGIT_T
  SYMBOL --> SEARCH_T

  RESTART["Process restart / resume"]:::ctrl --> SESS_T
  SESS_T --> SESSION
  SESS_T --> WF
  MSG_T --> MSG
  STEP_T --> STEP

  VECTOR["Vector / embedding index"]:::planned -.-> SEARCH_T
  WORKTREE["Real Git worktree transaction metadata"]:::planned -.-> SESS_T
```

Source: `docs/diagrams/persistence-data-flow.mmd`.

## Long-task invariant

```text
Durable Session + Context Graph may grow
                 │
                 ▼
      retrieval narrows candidates
                 │
                 ▼
       Attention Light moves
                 │
                 ▼
        finite Active Subgraph
                 │
                 ▼
       bounded recent tool rounds
                 │
                 ▼
             finite LLM input
```

LumenCortex does not require the entire durable history to be sent to the model every turn.

## Implementation vs target

The diagrams intentionally show planned components as dashed gray.

Currently important planned/incomplete items include:

- transactional real Git worktree isolation,
- vector/embedding retrieval,
- whole-run rollback for arbitrary workspace mutations,
- richer Skills/vision/browser layers.

For claim status, use [Standalone readiness](standalone-readiness.md), not the diagram alone.
