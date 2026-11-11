# README and Go-only documentation baseline refresh

Date: 2026-11-11

## Why this update was required

Core Harness commit `208ad6b` had been pushed to `origin/main`, but the README refresh was still an uncommitted local working-tree change. The remote README therefore remained at its previous content.

The local draft also contained stale baseline references to Core `937e161`, described Node.js as the default/production runtime, and retained an incremental Node/Go parity plan. Those statements conflicted with the current Core baseline and the Owner's Go-only production decision.

## Current facts reflected in the docs

- Core `main`: `208ad6b`.
- Latest Core tag in this checkout: `v0.1.4` at `42ee234`, older than current main.
- Go Engine identity: `0.1.4-preview`.
- Desktop `main`: `4a6c2f3`.
- Desktop Core dependency: `v0.1.4`; exact upgrade remains pending a new Core SemVer release.
- Go is the sole production Runtime.
- Node.js source/package/test material is historical read-only reference, not a second production runtime or a parity target.
- Cognitive Core P0 remains unmerged and is not represented as a current mainline capability.

## Updated documents

1. `README.md`
2. `README.zh-CN.md`
3. `docs/README.md`
4. `docs/go-runtime-migration.md`
5. `docs/standalone-readiness.md`
6. `docs/validation.md`
7. `docs/cognitive-control-plane.md`
8. `docs/system-overview.md`
9. `docs/architecture.md`
10. `docs/adr/0001-go-runtime-migration.md` (original ADR retained; a superseding Owner decision note was added)

## Verification

- Relative Markdown links in all 10 updated documents: 0 missing.
- `git diff --check`: PASS.
- No Go source, tests, Migration, public API, Desktop files, or runtime behavior changed.
- No Go tests rerun because this change is documentation-only.
- Existing uncommitted `AGENTS.md` modification was intentionally excluded from the commit.

## Release and Git boundary

This documentation update does not create a Core SemVer tag or Desktop release. It records the current main baseline and the still-pending Desktop dependency upgrade and Cognitive Core P0 integration.
