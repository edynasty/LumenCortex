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
- Added `docs/assets/brand/lumencortex-mark.svg`, reusing the A1 flat mark from the Desktop brand component. The Desktop source identifies this A1 mark as Owner-approved; the separate app-icon candidate was not copied or represented as approved.
- Added the mark and a centered wordmark/tagline to both README languages.
- Updated all first-party documentation references from `.webp` to `.png` and updated the image maintenance guide.
- Corrected version wording: `208ad6b` is the Go runtime code baseline in Core `main`; the README does not pin a transient documentation HEAD hash.

## Rendered assets

| Asset | Dimensions |
|---|---:|
| `system-architecture.png` | 1568 × 450 |
| `agent-execution-flow.png` | 1568 × 214 |
| `workflow-cognitive-dual-plane.png` | 1568 × 294 |
| `persistence-data-flow.png` | 1568 × 784 |
| `lumencortex-mark.svg` | 337 × 297 viewBox |

## Verification

- All four PNGs decoded and passed image integrity checks.
- The SVG brand mark rendered successfully through librsvg.
- All first-party documentation image references point to existing PNG/SVG files.
- No first-party `.webp` references or architecture WebP files remain.
- Relative Markdown links in the updated README and documentation set resolve.
- `git diff --check`: PASS.
- No Go source, runtime behavior, database migration, public API, or Desktop implementation changed. Go tests were not rerun because this is a documentation and static-asset repair.
- The pre-existing `AGENTS.md` modification was intentionally excluded from this commit.
