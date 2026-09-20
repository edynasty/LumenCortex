# Architecture diagrams

These diagrams distinguish the **target architecture** from the **current implementation**.

Legend:

- solid green/blue/orange: implemented and covered by automated tests;
- dashed gray: planned or only partially represented;
- "attention cut" means removing nodes from the **current working set**, not deleting durable graph memory.

## Target system architecture

```mermaid
flowchart TB
    classDef done fill:#e8f5e9,stroke:#2e7d32,stroke-width:2px,color:#102510
    classDef core fill:#e3f2fd,stroke:#1565c0,stroke-width:2px,color:#0d2340
    classDef git fill:#fff3e0,stroke:#ef6c00,stroke-width:2px,color:#3d2100
    classDef plan fill:#fafafa,stroke:#777,stroke-width:1.5px,stroke-dasharray:6 4,color:#444
    classDef store fill:#f3e5f5,stroke:#7b1fa2,stroke-width:2px,color:#2b1232

    subgraph ENTRY["Entry / Harness"]
      TUI["Full-screen TUI / Session Switch"]:::done
      CLI["Standalone CLI: agent / chat / git / light"]:::done
      MCP["MCP stdio/HTTP"]:::done
      API["API / Web UI"]:::plan
    end

    subgraph EXEC["Ephemeral Agent Execution"]
      GOAL["Goal / Intent"]:::core
      LOOP["Agent Loop"]:::done
      WS["Bounded Working-Set Pager"]:::done
      SESSION["Full Durable Session"]:::store
      TWS["Tool Working Set / Schema Allowlist"]:::done
      TOOLS["Tool Registry + Permission Gate\nread_file + read_files + apply_patch"]:::done
      VERIFY["Async Test / Shell / Reality Verification\nlive stdout/stderr + cancel"]:::done
      TASKDAG["Focused Subagent / Parallel Sessions"]:::done
      GOAL --> LOOP
      SESSION --> WS --> LOOP
      LOOP --> TWS --> TOOLS --> VERIFY --> LOOP
      LOOP --> TASKDAG
      LOOP --> TUI
    end

    subgraph RETRIEVAL["Candidate Retrieval"]
      LEX["Fallback lexical scan"]:::done
      SYM["Symbol index + LSP"]:::done
      FTS["SQLite FTS5 lexical index"]:::done
      EMB["Embedding index"]:::plan
      HIST["Recent evidence / history seeds"]:::done
      CAND["Candidate Set"]:::core
      LEX --> CAND
      HIST --> CAND
      SYM --> CAND
      FTS --> CAND
      EMB -.-> CAND
    end

    subgraph ATTENTION["Cognitive Control / Light"]
      LIGHT["Moving Attention Light"]:::done
      PROP["Activation Propagation over typed edges"]:::done
      CUT["Ephemeral Attention Cut: omit low-utility nodes under budget"]:::done
      SCUT["Structural Cut / Restore: disable propagation, retain memory"]:::done
      ACTIVE["Finite Active Subgraph"]:::core
      PROMOTE["Active Promotion Controller"]:::done
      DRILL["Drill-down by reseeding child/detail nodes"]:::core
      LIGHT --> PROP --> CUT --> ACTIVE
      SCUT --> PROP
      ACTIVE --> PROMOTE
      PROMOTE --> DRILL --> LIGHT
    end

    subgraph STORAGE["SQLite WAL Storage"]
      DB["lumencortex.db"]:::store
      GN["graph_nodes / graph_edges"]:::done
      SS["sessions / messages / steps"]:::done
      CJ["cognitive_commits / refs"]:::done
      JR["journal"]:::done
      SI["symbols / node_fts"]:::done
      DB --> GN
      DB --> SS
      DB --> CJ
      DB --> JR
      DB --> SI
    end

    subgraph MEMORY["Persistent Cognitive Memory"]
      REAL["Reality / Repository / Runtime"]:::done
      EVID["Evidence"]:::done
      BELIEF["Belief / Negative / Hypothesis"]:::done
      GRAPH["Context Graph"]:::store
      ABS["Abstraction Parent; child detail preserved"]:::done
      OBS["Tool Observations"]:::done
      SGRAFT["Structural Graft Edge"]:::done
      CANON["Canonicalization / GC / hot-warm-cold"]:::plan
      REAL --> EVID --> GRAPH
      BELIEF --> GRAPH
      ABS --> GRAPH
      OBS --> GRAPH
      SGRAFT --> GRAPH
      CANON -.-> GRAPH
    end

    subgraph CGIT["Cognitive Git"]
      COMMIT["Commit / Diff"]:::git
      BRANCH["Branch / Checkout"]:::git
      MERGE["Merge / Conflict / Revert"]:::git
      GRAFT["Graft: Cherry-pick"]:::git
      BLAME["Blame / Rebase"]:::git
      WORKTREE["Transactional real Git worktree binding"]:::plan
      COMMIT --> BRANCH --> MERGE --> GRAFT --> BLAME
      WORKTREE -.-> COMMIT
    end

    subgraph PROVIDERS["LLM Providers"]
      LOCAL["Ollama / vLLM / Generic OpenAI-compatible"]:::done
      OR["OpenRouter"]:::done
      GROQ["Groq"]:::done
      DS["DeepSeek official adapter"]:::done
      DSREAL["Real DeepSeek V4 validation"]:::plan
      DS --> DSREAL
    end

    ENTRY --> GOAL
    TUI --> GOAL
    MCP --> TOOLS
    PROVIDERS <--> LOOP
    LOOP --> CAND --> LIGHT
    GRAPH --> LIGHT
    ACTIVE --> WS
    VERIFY --> REAL
    TOOLS --> OBS
    EVID -. "source changed => dependent cognition stale" .-> BELIEF
    PROMOTE --> ABS
    GRAPH --> COMMIT
    CGIT --> GRAPH
    GRAPH --> GN
    SESSION --> SS
    COMMIT --> CJ
    OBS --> JR
    CAND --> SI
```

## Working principle

```mermaid
flowchart LR
    U["User Goal"] --> R["Candidate Retrieval"]
    R --> L["Move the Light"]
    L --> P["Propagate Activation"]
    P --> C["Attention Cut under token budget"]
    C --> AS["Active Subgraph"]

    AS --> Q{"Granularity still suitable?"}
    Q -- "too dense / repeatedly activated" --> UP["Active Promotion"]
    UP --> PAR["Create small parent abstraction"]
    PAR --> KEEP["Keep all detailed children intact"]
    KEEP --> G["Persistent Context Graph"]
    Q -- "need detail" --> DOWN["Drill down / reseed child"]
    DOWN --> L
    Q -- yes --> W["Bounded LLM Working Set"]

    S["Durable Session History"] --> W
    G --> L
    W --> M["LLM"]
    M --> TC{"Tool calls?"}

    TC -- yes --> X["Read / Search / Edit / Shell / Test"]
    X --> O["Tool Observation Evidence"]
    O --> G
    X --> CH{"Workspace changed?"}
    CH -- yes --> ING["Re-ingest Reality"]
    ING --> ST["Changed Evidence marks dependent cognition stale"]
    ST --> G
    CH -- no --> L

    TC -- final --> DONE["Final Answer / Task Result"]
    DONE --> CG["Optional Cognitive Commit"]

    B["Alternative hypothesis / task branch"] --> MER["Merge or Cherry-pick = Graft"]
    MER --> CG
    CG --> G
```

## The four key operations

```text
1. Propagation  传导
   Seed nodes -> typed edges -> decayed activation -> candidate attention

2. Cut / Amputation  截肢
   Ephemeral cut: candidate -> token budget -> omit from this working set.
   Structural cut: edge cut -> propagation stops while node/edge/history remain restorable.
   IMPORTANT: neither form requires deleting durable cognitive memory.

3. Graft  嫁接
   Structural graft: add an explicit typed graph edge between existing cognition.
   Branch graft: merge/cherry-pick another cognition branch into current history.

4. Promotion  升格
   Dense/reused detail -> parent abstraction
   children remain intact -> future drill-down is possible
```

## Long-task invariant

```text
Persistent graph/session may grow without bound
                |
                v
        Light moves every step
                |
                v
      finite Active Subgraph
                |
                v
       finite Working Set
                |
                v
              LLM

The model never needs the entire durable history in every request.
```
