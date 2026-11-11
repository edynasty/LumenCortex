# Architecture visual assets

This directory contains the reader-facing architecture diagrams used by the README and technical documentation.

## Files

- `system-architecture.png` — Go production runtime, cognitive plane, editing/execution, integrations, and persistence.
- `agent-execution-flow.png` — Agent lifecycle from goal/resume through tools, workflow gates, persistence, and completion.
- `workflow-cognitive-dual-plane.png` — Workflow legality/proof responsibilities versus Cognitive memory/attention responsibilities.
- `persistence-data-flow.png` — durable runtime/cognitive state, SQLite WAL tables, and restart/resume flow.

## Maintenance model

The corresponding `docs/diagrams/*.mmd` files are the maintainable logical/topology sources. PNGs are rendered from those sources with Mermaid CLI 12.2.0 and Chromium, then reviewed for labels, boundaries, implemented/planned distinction, and readability.

When architecture semantics change:

1. update the relevant `.mmd`,
2. render the corresponding PNG from the Mermaid source,
3. decode and visually inspect the PNG (not just its file signature),
4. review labels and arrows against current code,
5. update canonical documentation and validation/readiness claims where applicable,
6. commit source and presentation asset together.

Do not infer implementation status from visual polish. `docs/standalone-readiness.md` and `docs/validation.md` remain authoritative for capability claims.
