# Architecture visual assets

This directory contains the curated reader-facing architecture visuals used by the README and technical documentation.

## Files

- `system-architecture.webp` — system-level component/layer map.
- `agent-execution-flow.webp` — autonomous Agent lifecycle.
- `workflow-cognitive-dual-plane.webp` — Workflow vs Cognitive control responsibilities.
- `persistence-data-flow.webp` — durable state and restart flow.

## Maintenance model

The corresponding `docs/diagrams/*.mmd` files are the maintainable logical/topology sources. These WebP files are curated presentation assets generated from the same architecture specification and reviewed for labels, boundaries, implemented/planned distinction, and readability.

When architecture semantics change:

1. update the relevant `.mmd`,
2. regenerate or redraw the presentation visual,
3. review labels and arrows against current code,
4. update canonical documentation and validation/readiness claims where applicable,
5. commit source and presentation asset together.

Do not infer implementation status from visual polish. `docs/standalone-readiness.md` and `docs/validation.md` remain authoritative for capability claims.
