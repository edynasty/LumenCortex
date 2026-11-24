# PRE-01 / PRE-02 Owner approval — 2026-11-24

Source: Owner's explicit Final Integration, Evidence Preservation & Git Cleanup instruction in this session. This supersedes historical PENDING_OWNER_APPROVAL labels prospectively; historical records remain unchanged.

## Approved Option B — fail closed

Ordinary supported files use verified atomic replacement. Multiple hard links, ACL/required metadata that cannot be preserved, and filesystems lacking the required atomic primitive are refused explicitly with `unsupported_filesystem`. No destructive in-place fallback or overwrite-style rename fallback.

Approved for development-main integration and normal GitHub PR merge commit, publication of main, regression, evidence preservation and safe cleanup only. No production release, tags, formal TB2, or paid experiment authorized.

| Platform | Contract and evidence |
|---|---|
| macOS APFS | Supported; native verification completed |
| Linux local filesystem | Supported where atomic exchange works; Docker overlayfs/tmpfs verified; native ext4 UNVERIFIED |
| Windows | Existing-file updates unsupported, fail closed; native UNVERIFIED; NTFS implementation deferred |
| FreeBSD/other unsupported Unix | Existing-file updates unsupported, fail closed; native UNVERIFIED; FreeBSD deletion unsupported |

NFS/FUSE outside scope. No universal external-process locking guarantee.

PRE-01: **VERIFIED_WITHIN_SUPPORTED_SCOPE**. Failed staging preserves original contents; unsupported hard-link/metadata/filesystem updates refuse.

PRE-02: **VERIFIED_WITH_LIMITATIONS**. Same-process LC writes serialize; detected external changes cause conflict; supported delete CAS quarantines safely; uncertain outcomes cannot claim verified success.

OPEN: L2 non-cooperative old-FD writes after final check; L3 SIGKILL `.lcx-*` residue (P3); late hard-link creation between final link check and exchange. Binary artifact export retains its separate inherited boundary. Native ext4 and Windows/FreeBSD remain UNVERIFIED.

## Finding ledger

| Finding | Treatment / evidence |
|---|---|
| SOL-PRE-L1-01 | Independently verified fixed; 315dc74, review history/editing-pre01-pre02-sol-l1-opus-verify-20261123 |
| SOL-PRE-L1-02 | Independently verified fixed; 4c79bc0, same review |
| OPUS-V-01 | Independently reviewed; platform limitations documented; 9ad5f2aa |
| SOL-OV01-01 | Independently verified fixed; 93a2855; fresh-context Opus review of 5131660 |
| SOL-OV01-02 | Independently verified fixed; 5131660; same review |
| SOL-OV01-EVIDENCE-01 | OPEN P3, non-blocking provenance limitation: cited Linux raw log lacks UID and actual command lines. Do not reconstruct them. Stress failure occurred in implementer's baseline focused simulation, not historical full simulation; historical full run remains FAIL / exit 1 |
| L2 / late hard-link race | OPEN limitations |
| L3 | OPEN P3 |
| PLATFORM-WIN-NTFS-001 | Deferred |
| Native Linux ext4 | UNVERIFIED |

Review provenance: final fresh-context reviewer reported actual model `local-anthropic/combo/claude-opus-5-5`; it was dispatched by the implementing session, not a separately initiated top-level session. This limitation was disclosed before the Owner's present explicit integration authorization. No post-merge independent acceptance is implied.

Canonical archive: `bench/evidence/core-final-integration-20261124/INDEX.md`.
