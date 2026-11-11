# README brand and architecture-image repair

Date: 2026-11-11

## User-visible defects

The GitHub README showed a large blank area where the system architecture image should render, and the project header had no brand mark.

## Root cause

All four first-party architecture `.webp` assets were incomplete. `dwebp` returned `NOT_ENOUGH_DATA` for every file, even though the paths existed in the repository and GitHub. The README's relative path was correct; the image payload itself was not decodable.

## Changes

- Replaced all four broken WebP assets with PNGs rendered directly from their Mermaid sources using Mermaid CLI 12.2.0, Node.js 24.20.0, and the local Chrome renderer.
- Changed the system architecture map to a compact left-to-right layout and updated it to show the Go production runtime, current Editing / DAP / Eval / Agent Hub capabilities, read-only Go Graph Governor boundary, optional Session Worktrees, and the not-yet-in-Go semantic/vector retrieval boundary.
- Changed the Agent execution flow source to a compact left-to-right layout.
- The README brand image is `docs/assets/brand/lumencortex-app-icon.png`, copied byte-for-byte from the Desktop production build source `LumenCortex-Desktop/build/appicon.png` (1024 × 1024 RGBA). Its SHA-256 is verified identical to the Desktop source. The earlier flat A1 indicator was superseded; the full glass app icon is used.
- Added the canonical Desktop app icon and a centered wordmark/tagline to both README languages.
- Updated all first-party documentation references from `.webp` to `.png` and updated the image maintenance guide.
- Corrected version wording: `208ad6b` is the Go runtime code baseline in Core `main`; the README does not pin a transient documentation HEAD hash.

## Rendered assets

| Asset | Dimensions |
|---|---:|
| `system-architecture.png` | 1568 × 450 |
| `agent-execution-flow.png` | 1568 × 214 |
| `workflow-cognitive-dual-plane.png` | 1568 × 294 |
| `persistence-data-flow.png` | 1568 × 784 |
| `lumencortex-app-icon.png` | 1024 × 1024 RGBA; byte-identical to Desktop `build/appicon.png` |

## Verification

- All four PNGs decoded and passed image integrity checks.
- The Desktop app icon PNG decodes successfully and is byte-identical to Desktop `build/appicon.png`.
- All first-party documentation image references point to existing PNG/SVG files.
- No first-party `.webp` references or architecture WebP files remain.
- Relative Markdown links in the updated README and documentation set resolve.
- `git diff --check`: PASS.
- No Go source, runtime behavior, database migration, public API, or Desktop implementation changed. Go tests were not rerun because this is a documentation and static-asset repair.
- The pre-existing `AGENTS.md` modification was intentionally excluded from this commit.
