# Documentation guide

This document defines how LumenCortex documentation should be written and maintained.

The main objective is to prevent three common failures:

1. README becoming an unstructured architecture dump,
2. implementation claims getting ahead of evidence,
3. the same behavior being described differently in multiple files.

## Documentation layers

### Layer 1 — README

The root `README.md` should answer:

- What is LumenCortex?
- Why does it exist?
- How do I install/run it?
- What are the main capabilities?
- Where is the detailed documentation?

Do not put deep architecture proofs, long data-model details, or benchmark history in README.

### Layer 2 — System documentation

Canonical documents:

- `system-overview.md`,
- `architecture.md`,
- `architecture-diagrams.md`,
- `execution-flow.md`.

These explain how the system fits together.

### Layer 3 — Component/reference documentation

Examples:

- `agent-runtime.md`,
- `workflow-contract.md`,
- `data-model.md`,
- `security-and-trust.md`,
- `attention-light-algorithm.md`,
- `cognitive-control-plane.md`.

These explain exact behavior and contracts.

### Layer 4 — Evidence documentation

- `validation.md`,
- `standalone-readiness.md`.

These answer a different question:

> What has actually been proven?

Never mix target architecture and validation evidence without labeling the distinction.

### Layer 5 — Decision history

Use `docs/adr/` for architectural decisions that would otherwise be repeatedly re-litigated.

## One canonical document per question

Prefer one source of truth.

| Question | Canonical place |
|---|---|
| What is the system? | `system-overview.md` |
| Why graph + attention? | `architecture.md` |
| How does Attention Light retrieve, propagate, rank, and budget context? | `attention-light-algorithm.md` |
| How are Decision Layer judgments, Category model chains, Think effort, and Graph governance separated? | `cognitive-control-plane.md` |
| What happens during a run? | `execution-flow.md` |
| How does Workflow work? | `workflow-contract.md` |
| How are things persisted? | `data-model.md` |
| What are the security boundaries? | `security-and-trust.md` |
| What is actually proven? | `validation.md` |
| What is still missing? | `standalone-readiness.md` |

Other documents should link rather than restate large sections.

## Status language

Use these terms precisely.

### Implemented

Code exists.

### Validated

Evidence exercises the same execution path.

### Partial

Some meaningful part is missing.

### Planned

Design only.

### Evidence gap

Implementation exists, but required real-world proof is missing.

Avoid claims such as:

- “fully safe,”
- “production ready,”
- “100% replacement,”
- “complete parity,”
- fixed performance guarantees based on one CI runner.

## Architecture diagrams

Use two layers:

- `docs/diagrams/*.mmd` — maintainable logical/topology source,
- `docs/assets/architecture/*.webp` — curated presentation visuals used by README and architecture documents.

The static visual is the primary reader-facing artifact when one exists. The Mermaid source is the maintainable structural reference, not a requirement that the WebP be a pixel-for-pixel renderer output.

Rules:

1. each diagram answers one architectural question,
2. keep the logical topology source in `docs/diagrams/*.mmd`,
3. keep the reviewed presentation asset in `docs/assets/architecture/*.webp`,
4. use consistent semantic colors by architectural role,
5. planned components remain visually distinct, preferably dashed/gray,
6. never hide known product gaps in a presentation image,
7. when topology changes, update both the `.mmd` source and the corresponding static visual in the same change,
8. README and overview documents should reference the static visual rather than embedding a large Mermaid graph,
9. verify text labels in generated visuals manually before merging.

Recommended semantic palette:

| Role | Color family |
|---|---|
| Entry/UI | dark/navy |
| Workflow/control | indigo |
| Cognitive | cyan |
| Execution | green |
| External/runtime | orange |
| Persistence | purple |
| Planned | gray dashed |

## Change-to-document matrix

| Code change | Documents that normally change |
|---|---|
| Agent Loop lifecycle | `execution-flow.md`, `agent-runtime.md`, `validation.md` |
| Workflow schema/semantics | `workflow-contract.md`, `execution-flow.md`, `data-model.md`, tests |
| Tool permission/scope | `security-and-trust.md`, `agent-runtime.md`, tests |
| Cognitive Graph/Light/Promotion | `architecture.md`, `attention-light-algorithm.md`, diagrams, `validation.md` |
| Decision Layer / Category routing / Think effort / Graph Governor | `cognitive-control-plane.md`, `architecture.md`, ADR, readiness, validation |
| SQLite schema/persistence | `data-model.md`, `architecture.md`, migration tests |
| LSP/MCP/Subagent | `agent-runtime.md`, readiness, validation |
| Provider behavior | README quick start, `agent-runtime.md`, validation |
| Performance optimization | `validation.md`; architecture only if design changed |
| New known gap closed | `standalone-readiness.md` and validation evidence |
| New architecture decision | add ADR |

## Documentation Definition of Done

A change that affects documented behavior is not finished until:

- [ ] canonical behavior document is updated,
- [ ] diagram is updated if component/data flow changed,
- [ ] validation evidence is updated when a claim changes,
- [ ] readiness matrix is updated if a gap opens or closes,
- [ ] README changes only if user-facing entry behavior changed,
- [ ] planned features are not written as implemented,
- [ ] examples use commands/schema supported by current code.

## ADR policy

Create an ADR when a decision:

- changes a major subsystem boundary,
- introduces a new persistence model,
- changes the trust/security model,
- changes Workflow vs Cognitive responsibilities,
- replaces a core provider/tool protocol,
- creates a long-lived compatibility constraint.

Do not use ADRs for ordinary bug fixes.

## Suggested ADR topics for future work

When implemented, these deserve ADRs:

1. transactional Git worktree isolation strategy,
2. whole-run rollback semantics,
3. optional embedding/vector retrieval architecture,
4. Skills packaging and discovery model,
5. browser/vision trust boundary,
6. hot/warm/cold cognitive storage.

## Writing style

Prefer:

- concrete invariants,
- exact state transitions,
- tables for responsibilities,
- examples tied to current code,
- explicit limitations,
- links to evidence.

Avoid:

- marketing adjectives,
- vague “AI memory” language,
- unexplained diagrams,
- duplicating the same command reference across many files,
- claiming a benchmark as a universal latency guarantee.
