# Core final integration evidence index — 2026-11-24

Candidate: 51316603974d916295bc6f0560d46e7772f304e5. Original main: 7aeb1607ffa588727c6fe04873f7b58a1dc16f15. Integration branch: integ/pre01-pre02-final-20261124.

## Archive map

- `history/adr-0024-*`: architecture, R2–R7 remediation/review evidence, original merge/integration, Carbon independent post-merge acceptance and GOFLAGS full-suite recovery. Original architecture and versioned reports also remain reachable in main history and `docs/adr/0024-*`, `docs/reports/evidence/adr-0024-*`.
- `history/editing-pre01-pre02-macos-20261123`: Carbon original remediation, RED/GREEN and raw command evidence (aa9281f).
- `history/editing-pre01-pre02-opus-20261123`: Opus F1–F5 remediation and fail-closed followup (2c950cf, 71cce56).
- `history/editing-pre01-pre02-sol-l1-20261123`: Sol L1 fixes and 17-entry manifest (b03c91e).
- `history/editing-pre01-pre02-sol-l1-opus-verify-20261123`: original Opus independent L1 verification report/logs.
- `history/editing-opus-v01-platform-scope-20261123`: original 26-entry manifest, platform correction, historical full simulation FAIL / exit 1, 152 PASS / 6 FAIL / 24 SKIP (9ad5f2aa).
- `history/editing-sol-ov01-final-p3-20261123`: final 40-entry evidence set, source probes, raw logs, prior independent report and handoff (5131660).
- `temporary/sol-opusv01-review/REPORT.md`: bytewise preserved original Sol independent OPUS-V-01 review; supporting temporary logs retained.
- `temporary/opus-final-review/ORIGINAL_FINAL_RESPONSE.md`: original Opus final review text extracted verbatim from the assistant text content in native session JSONL; source was a conversation result, not a standalone repository report. No reasoning/private scratchpads were extracted. Additional raw test/mutant outputs copied from /tmp/lc-ov01-indep.91wlpA. Original source session: ~/.omp/agent/sessions/-IdeaProjects-me-LumenCortex/2026-11-23T05-07-22-159Z_01a206cd-37af-772b-b4f4-a0399462c58e/OV01FinalIndependentReview.jsonl. Full private transcript is not archived.
- `REPORT-PROVENANCE.json`: source worktree, immutable commit URL and file SHA256 for historical reports; duplicates referenced rather than recopied.
- `WORKTREE-INVENTORY.json`, `WORKTREES-BEFORE.txt`, `REFS-BEFORE.txt`: pre-cleanup preservation inventory. Unrelated worktrees are not deletion candidates.
- Governance: `docs/adr/governance/pre01-pre02-owner-approval-2026-11-24.md`.

443 copied historical files were compared byte-for-byte with workspace originals. Historical manifests retained unchanged. All 16 SHA256SUMS/ARCHIVE_SHA256SUMS manifests verify. `ORIGINALS.sha256` initially failed when checked from its parent: its paths are relative to `private-r6-originals/`, not the manifest directory. Correct-root verification passes all 22 entries; both results retained (`HISTORICAL-MANIFEST-VERIFICATION.txt`, `R7-ORIGINALS-CORRECT-ROOT.txt`). This is a path-root correction, not altered evidence.

Temporary report discovery: original final Opus review had no standalone Markdown file; extracted original response provenance above. No missing UID/command evidence was reconstructed. Linux raw log lacks UID and actual command lines. Stress failure belongs to implementer's baseline focused simulation, not historical full-package simulation. Historical reports remain unchanged.

PRE-01 VERIFIED_WITHIN_SUPPORTED_SCOPE; PRE-02 VERIFIED_WITH_LIMITATIONS. L2, L3, late hard-link race, binary export boundary, native ext4/Windows/FreeBSD gaps remain open. Option B formally approved now; historical pending labels are preserved as historical facts. No production release/TB2 authorization.

New `SHA256SUMS` covers archived file bytes. Post-merge regression/cleanup results will be preserved separately against their exact merged SHA. No cleanup before all required gates pass.
