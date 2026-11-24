# Safe Git Worktree & Branch Cleanup Report — Core Final Integration (2026-11-24)

- **Final `origin/main` SHA (`M3`):** `83304dc163ee54bdd8bfa254339a7cfc0ade0dea` (PR #14 normal merge of archived evidence, findings ledger, `M2` regression suite, and Opus independent reviews)
- **Verified `M2` Regression SHA:** `d0cb171d0319bd0d523bbc19241343b9d1c3e60c` (PR #13 normal merge of `INT-PRE-REG-01`)
- **PR #12 Evidence Preservation SHA (`M1`):** `cfef043083a37b8fb6f67a96002b0d9b8e22a9f9` (PR #12 normal merge of `35409ca0e2015c631d3dd620a354124436a27d2d`)
- **Pre-Integration Baseline `main`:** `ef10ca7c8e8031e75fdf27f449924b8254f0e2bb`
- **Summary Counts:**
  - Worktrees safely removed (`git worktree remove`, without `--force`): **24**
  - Local branches safely deleted (`git branch -d`, without `-D`): **22**
  - Remote branches safely deleted (`git push origin --delete`): **21** (`fix/editing-pre01-pre02-macos-20261123` had no remote branch)
  - In-scope worktrees explicitly retained: **3** (`LumenCortex-Core`, `wt/core-editing-pre01-pre02-opus-20261123`, `wt/core-final-merged-verification-20261124`)
  - Local tags (`v0.1.0`..`v0.1.4`) and all unrelated branches/worktrees preserved untouched.

## 1. Evidence Preservation Verification Against Exact `M3` (`83304dc163ee54bdd8bfa254339a7cfc0ade0dea`)

Before any worktree or branch deletion, an archive extraction of `M3` verified all manifests:
- `bench/evidence/core-final-integration-20261124/SHA256SUMS`: **525/525 OK** (unchanged)
- `bench/evidence/core-final-integration-20261124/ADDENDUM_SHA256SUMS`: **62/62 OK**
- `bench/evidence/core-final-integration-20261124/temporary/opus-intpre-review-20261124/SHA256SUMS`: **13/13 OK** (`REPORT.md` SHA256 `ddf70aaaf8f772a5fc6c124ffdd720727fa3cb8092959279fec4c45ff57a52f5`)
- `bench/evidence/core-final-integration-20261124/postmerge-regression-m2/SHA256SUMS` + `OPUS_POSTMERGE_ACCEPTANCE.sha256`: **43/43 + 1/1 OK**
- `bench/evidence/core-final-integration-postmerge-20261124/SHA256SUMS`: **35/35 OK** (`04-test-all.stdout` SHA256 `eda4550cfe8a4b6c33e34b2dc6ea1b27920850f57e8e1d73743b9da5566cc961`)
- `docs/reports/core-final-integration-2026-11-24.md`: preserved unchanged (SHA256 `00eec92b1313c9e93723ff2912060c6d9abb2dad4128b39b519c7308bcbb518f`)
- `bench/evidence/runtime-int-pre-reg-01-carbon-20261124/SHA256SUMS`: **11/11 OK**

## 2. Deleted Worktrees (24), Local Branches (22), and Remote Branches (21)

Every deleted target satisfied all 8 safety criteria: zero unique commits vs `M3`, clean `status --porcelain --untracked-files=all --ignored`, zero open PRs (`[]`), zero active `lsof -d cwd` holders, and archived evidence verified in `M3`. Full structured per-entry records live in `CLEANUP_LEDGER.json`.

| # | Worktree Path | Local Branch Deleted | Remote Deleted | Verified HEAD SHA |
|---|---|---|---|---|
| 1 | `wt/core-adr-0024-context-space-20261118` | `feat/adr-0024-context-space-20261118` | Yes | `a8b478617fe15e8726d4009f94454b027b1a9ef9` |
| 2 | `wt/core-adr-0024-final-merge-evidence-20261122` | `docs/adr-0024-final-merge-evidence` | Yes | `daa5cb8edb7e729d017a0f1588c4bc97d387f005` |
| 3 | `wt/core-adr-0024-integration-20261122` | `fix/editing-pre01-pre02-macos-20261123` | N/A (local only) | `aa9281fe1618acd87ffb322559d4ccd643b37e8a` |
| 4 | `wt/core-adr-0024-merged-verification-20261122` | (detached) | N/A | `7aeb1607ffa588727c6fe04873f7b58a1dc16f15` |
| 5 | `wt/core-adr-0024-opus-ov-remediation-20261122` | `fix/adr-0024-opus-ov-remediation-20261122` | Yes | `d0f114be20ab578388e2333f2c6c71392f509de7` |
| 6 | `wt/core-adr-0024-r2-20261119` | `fix/adr-0024-r2-remediation-20261119` | Yes | `2ba79ae4aedd35d597604168894c3d65167e9cde` |
| 7 | `wt/core-adr-0024-r3-20261119` | `fix/adr-0024-r3-remediation-20261119` | Yes | `aeb2e5014697fa544ee855c4d6277be4c074fc16` |
| 8 | `wt/core-adr-0024-r4-20261120` | `fix/adr-0024-r4-remediation-20261120` | Yes | `0b3b8e721163890ebb664abb4b5c9fc3ef39f4f1` |
| 9 | `wt/core-adr-0024-r5-rollback-safety-20261120` | `fix/adr-0024-r5-rollback-safety-20261120` | Yes | `bfc493634d819bc3be7d30d669bbb2fbef30212e` |
| 10 | `wt/core-adr-0024-r6-safety-20261120` | `fix/adr-0024-r6-safety-20261120` | Yes | `4aaf8e7168743d6c8ae8fa6fe87a350ca69f9af2` |
| 11 | `wt/core-adr-0024-r7-partial-commit-20261120` | `fix/adr-0024-r7-partial-commit-20261120` | Yes | `31172bc274c3a4c54243a72e554862d539d7759d` |
| 12 | `wt/core-adr-0024-r7r2-20261121` | `fix/adr-0024-r7r2-remediation-20261121` | Yes | `fc1c4368507bea3f4e13099c62ca35936a6fba48` |
| 13 | `wt/core-adr-0024-r7r2-carbon-p3-20261121` | `fix/adr-0024-r7r2-carbon-p3-20261121` | Yes | `f3f77e35c98de4647243019568ed5ec3093cf072` |
| 14 | `wt/core-adr-0024-r7r2-opus-final-20261121` | `fix/adr-0024-r7r2-opus-final-20261121` | Yes | `10f135d1f904ab88c6a086319ca5820ea6b9e09c` |
| 15 | `wt/core-adr-0024-sol-01-02-remediation-20261122` | `fix/adr-0024-sol-01-02-remediation-20261122` | Yes | `3d85d06e31ae8b424b2ed3d0b9b94be70627e5ac` |
| 16 | `wt/core-adr-0024-sol-rev-01-astra-final-20261122` | `fix/adr-0024-sol-rev-01-astra-final-20261122` | Yes | `a5a1ec6be1081bad4e706944d655a9a9d97f4f70` |
| 17 | `wt/core-adr-0024-sol-rev-01-opus-20261122` | `fix/adr-0024-sol-rev-01-opus-20261122` | Yes | `52ac7d7df7d582bc2cb95e36618381cf30f7ab80` |
| 18 | `wt/core-editing-opus-v01-20261123` | `fix/editing-opus-v01-platform-scope-20261123` | Yes | `9ad5f2aa1c4ace22829be574b562ea41b1dfe127` |
| 19 | `wt/core-editing-pre01-pre02-sol-l1-20261123` | `fix/editing-pre01-pre02-sol-l1-remediation-20261123` | Yes | `b03c91e5db6969f0345a2034ecb172cf826bc792` |
| 20 | `wt/core-editing-sol-ov01-final-p3-20261123` | `fix/editing-sol-ov01-final-p3-20261123` | Yes | `51316603974d916295bc6f0560d46e7772f304e5` |
| 21 | `wt/core-final-evidence-archive-20261124` | `docs/core-final-integration-archive-20261124` | Yes | `1df145857edd11c6c66d61a44ffd5c9ae734a130` |
| 22 | `wt/core-final-integration-20261124` | `integ/pre01-pre02-final-20261124` | Yes | `35409ca0e2015c631d3dd620a354124436a27d2d` |
| 23 | `wt/core-final-merged-regression-20261124` | (detached) | N/A | `d0cb171d0319bd0d523bbc19241343b9d1c3e60c` |
| 24 | `wt/core-runtime-int-pre-reg-01-carbon-20261124` | `fix/runtime-int-pre-reg-01-carbon-remediation-20261124` | Yes | `2852b56023088c3c33b2678af0c8dd7debff06e4` |

## 3. Retained Items & Exact Reasons

### Retained In-Scope Worktrees & Branches
1. **`/Users/tangxingpeng/IdeaProjects/me/LumenCortex/LumenCortex-Core`** (`main`, checkout left untouched at `ef10ca7c8e8031e75fdf27f449924b8254f0e2bb`; live `origin/main` updated via PRs #12/#13/#14 to `83304dc163ee54bdd8bfa254339a7cfc0ade0dea`):
   - Retained untouched; holds Owner-owned ` M AGENTS.md` with exact SHA256 `84bb06cd73eb37997966e2d2e3cb8e8b181b27f97ff367d209f327bee818d1a0`.
2. **`wt/core-editing-pre01-pre02-opus-20261123`** (`fix/editing-pre01-pre02-opus-remediation-20261123`, local + remote `71cce56acb06b457dad2a4fec3a2c3db56537fd6` retained):
   - Retained because it contains 12 ignored local `.history/`, `.lumencortex/`, and `12.eval.log` files (`CLEANUP-PREFLIGHT.json`).
3. **`wt/core-final-merged-verification-20261124`** (detached `ef10ca7c8e8031e75fdf27f449924b8254f0e2bb`):
   - Retained because it contains ignored local `.history/`, `.lumencortex/`, and `12.eval.log` files.

### Retained Out-of-Scope Branches, Worktrees, Tags & Workspace Artifacts
- All unrelated branches and worktrees (`cog/*`, `rt/*`, `fix/rt-*`, `tb2-*`, `od11a-*`, `verifier-*`, `wp12-*`, `cognitive-core-*`, `unified-execution-runtime*`, `feature/*`, `hardening/*`, `docs/architecture-handbook`, `docs/generated-architecture-images`, `archive/merged/*`, `backup/*`) were left untouched.
- Local tags `v0.1.0`, `v0.1.1`, `v0.1.2`, `v0.1.3`, `v0.1.4` remain intact.
- Workspace evidence directories (`/Users/tangxingpeng/IdeaProjects/me/LumenCortex/bench/evidence/*`) remain preserved untouched.
