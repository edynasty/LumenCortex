# Final Governance & Finding Ledger — Core PRE-01 / PRE-02 / Runtime Integration (2026-11-24)

- **Owner Option B Approval Record (immutable in `main`):** `docs/adr/governance/pre01-pre02-owner-approval-2026-11-24.md` (Fail-Closed Option B, development `main` integration only; no production release, tag, or TB2 authorization).
- **Verified `M2` Regression SHA:** `d0cb171d0319bd0d523bbc19241343b9d1c3e60c` (PR #12 `cfef043083a37b8fb6f67a96002b0d9b8e22a9f9` + PR #13 `d0cb171d0319bd0d523bbc19241343b9d1c3e60c`).

## Core Pillar Dispositions

| Pillar | Status |
|---|---|
| `PRE-01` | `VERIFIED_WITHIN_SUPPORTED_SCOPE` |
| `PRE-02` | `VERIFIED_WITH_LIMITATIONS` |
| `INT-PRE-REG-01` | `INDEPENDENTLY_VERIFIED` |

## Complete Finding Ledger

| Finding | Final Disposition | Evidence / Notes |
|---|---|---|
| `INT-PRE-REG-01` | `INDEPENDENTLY_VERIFIED` | Runtime test contract remediation (`316e99b90fad5d6e22b6f9a9b01f240be2b55370`, `runtime/context_space_t3_test.go` only; zero production Go changes), evidence (`2852b56023088c3c33b2678af0c8dd7debff06e4` in `bench/evidence/runtime-int-pre-reg-01-carbon-20261124/`), PR #13 merge `d0cb171d0319bd0d523bbc19241343b9d1c3e60c`. Independently verified by Claude Opus 5.5 (`PASS_WITH_LIMITATIONS`) on `2852b56` (`temporary/opus-intpre-review-20261124/REPORT.md`, SHA256 `ddf70aaaf8f772a5fc6c124ffdd720727fa3cb8092959279fec4c45ff57a52f5`) and on `M2` (`postmerge-regression-m2/OPUS_POSTMERGE_ACCEPTANCE.md`), with full `M2` Gates 1–6 PASS. |
| `SOL-PRE-L1-01` | Independently verified fixed | `315dc74`; review archived at `history/editing-pre01-pre02-sol-l1-opus-verify-20261123`. |
| `SOL-PRE-L1-02` | Independently verified fixed | `4c79bc0`; review archived at `history/editing-pre01-pre02-sol-l1-opus-verify-20261123`. |
| `OPUS-V-01` | Independently reviewed, platform limitation documented | `9ad5f2aa`; archived at `history/editing-opus-v01-platform-scope-20261123`. |
| `SOL-OV01-01` | Independently verified fixed | `93a2855`; fresh-context Opus review of `5131660` archived at `temporary/opus-final-review/ORIGINAL_FINAL_RESPONSE.md`. |
| `SOL-OV01-02` | Independently verified fixed | `5131660`; archived at `history/editing-sol-ov01-final-p3-20261123` and `temporary/opus-final-review/ORIGINAL_FINAL_RESPONSE.md`. |
| `SOL-OV01-EVIDENCE-01` | Non-blocking evidence provenance limitation (`OPEN P3`) | Preserved unchanged without reconstructing missing UID/command lines (`INDEX.md`, `history/editing-opus-v01-platform-scope-20261123`). |
| `OPUS-INTPRE-EV-01` | Evidence location/provenance limitation (`P3`, preserved explicitly) | `docs/reports/runtime-int-pre-reg-01-carbon-2026-11-24.md` absent from `2852b56`; equivalent technical content present in `bench/evidence/runtime-int-pre-reg-01-carbon-20261124/README.md`, and original `/private/tmp/opus-intpre-review.fkO0cN/_logs/` review (`REPORT.md` SHA256 `ddf70aaa...`) + Opus `M2` post-merge acceptance are preserved byte-for-byte in this archive. |
| `L2` | `OPEN` | Non-cooperative external writes through pre-opened file descriptors after final verification check. |
| `L3` | `OPEN P3` | `SIGKILL` during staging may leave `.lcx-*` private staging files. |
| `Late hard-link race` | `OPEN` limitation | Hard-link creation between final link-count check and atomic exchange. |
| `Native ext4` | `UNVERIFIED` | Supported in contract where required Linux atomic exchange works (Docker overlayfs/tmpfs verified); native Linux ext4 remains unverified. |
| `Windows/FreeBSD native` | `UNVERIFIED` | Existing-file updates unsupported (`unsupported_filesystem`, fail closed); FreeBSD deletion unsupported; native Windows/FreeBSD unverified. |
| `NFS/FUSE` | Outside scope / excluded | Excluded from supported filesystem contract. |
| `Binary export boundary` | Inherited open boundary | Retains its separate inherited boundary; outside PRE-01/PRE-02 scope. |
| Pre-existing `gofmt` legacy files | `OPEN P3` (pre-existing, out of scope) | `postmerge-regression-m2/01b-gofmt-all-audit.stdout` lists 16 untouched pre-existing non-evidence `.go` files inherited before `7aeb160`; `01a-gofmt-changed.stdout` is empty (`0` modified Go files unformatted). |
