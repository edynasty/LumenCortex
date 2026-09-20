# Architecture diagrams

Solid boxes are implemented/tested core capabilities. Dashed boxes are planned/partial capabilities.

## System architecture

```mermaid
flowchart TB
    classDef done fill:#e8f5e9,stroke:#2e7d32,stroke-width:2px,color:#102510
    classDef core fill:#e3f2fd,stroke:#1565c0,stroke-width:2px,color:#0d2340
    classDef git fill:#fff3e0,stroke:#ef6c00,stroke-width:2px,color:#3d2100
    classDef plan fill:#fafafa,stroke:#777,stroke-width:1.5px,stroke-dasharray:6 4,color:#444
    classDef store fill:#f3e5f5,stroke:#7b1fa2,stroke-width:2px,color:#2b1232

    subgraph ENTRY["Entry"]
      CLI["CLI agent/chat/context/git"]:::done
      OC["OpenCode tools"]:::done
      API["API / MCP / Web UI"]:::plan
    end

    subgraph RUNTIME["Agent Runtime"]
      GOAL["Goal / Intent"]:::core
      LOOP["Agent Loop"]:::core
      WS["Bounded Working-Set Pager"]:::done
      SESS["Full Durable Session"]:::store
      EXEC["Tool Executor"]:::done
      VERIFY["Verifier / Test Loop"]:::done
      GOAL --> LOOP
      SESS --> WS --> LOOP
      LOOP --> EXEC --> VERIFY --> LOOP
    end

    subgraph CONTROL["Cognitive Control"]
      RET["Hybrid Retrieval: BM25 + Symbol + Embedding + Graph"]:::plan
      LIGHT["Moving Attention Light"]:::core
      ACTIVE["Active Subgraph / Token Budget"]:::core
      PROMOTE["Active Promotion Controller"]:::done
      DRILL["Drill-down"]:::done
      RET -.-> LIGHT --> ACTIVE --> PROMOTE --> DRILL --> LIGHT
    end

    subgraph GRAPH["Persistent Cognitive Graph"]
      REAL["Reality"]:::done --> EVID["Evidence"]:::done --> BELIEF["Belief / Negative"]:::done
      BELIEF --> CG["Context Graph"]:::core
      ABS["Abstraction / Parent"]:::done --> CG
      MUT["Propagate / Prune / Graft / Promote"]:::done
      AUTO["Auto Split / Merge / Canonicalize"]:::plan
      CG --> MUT --> CG
      AUTO -.-> CG
    end

    subgraph GIT["Cognitive Git"]
      C["Commit / Diff"]:::git --> B["Branch / Checkout"]:::git --> M["Merge / Conflict / Revert"]:::git --> A["Blame / Cherry-pick / Rebase"]:::git
      W["Real Git Worktree Binding"]:::plan -.-> C
    end

    subgraph PROVIDER["LLM Providers"]
      DS["DeepSeek official"]:::done
      OR["OpenRouter"]:::done
      GQ["Groq"]:::done
      LOCAL["Ollama / vLLM / generic OpenAI-compatible"]:::done
    end

    ENTRY --> GOAL
    PROVIDER <--> LOOP
    LOOP --> LIGHT
    CG --> LIGHT
    ACTIVE --> WS
    VERIFY --> REAL
    EVID -. "source changed => stale" .-> BELIEF
    PROMOTE --> ABS
    MUT --> C
    GIT --> CG
```

## Working principle

```mermaid
flowchart LR
    U["Goal"] --> OBS["Observe Reality"]
    OBS --> EV["Evidence"]
    EV --> G["Cognitive Graph"]
    G --> L["Recompute Light every step"]
    R["Recent tool evidence"] --> L
    L --> AS["Active Subgraph"]
    AS --> Q{"Granularity suitable?"}
    Q -- no --> P["Active Promotion: parent abstraction, children preserved"] --> G
    Q -- need detail --> D["Drill-down"] --> L
    Q -- yes --> W["Bounded Working Set"]
    H["Full durable Session"] --> W
    W --> M["LLM"]
    M --> T{"tool_calls?"}
    T -- yes --> X["Execute tools"] --> V["Verify"]
    V --> C{"Reality changed?"}
    C -- yes --> RI["Re-ingest: changed evidence -> stale beliefs"] --> G
    C -- no --> L
    T -- final --> DF["Graph Diff / optional Cognitive Commit"] --> G
```

Core invariant:

> Durable history may keep growing; the active model attention must remain finite.
