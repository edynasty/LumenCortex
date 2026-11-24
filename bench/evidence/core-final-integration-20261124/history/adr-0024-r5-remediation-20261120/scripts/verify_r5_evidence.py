#!/usr/bin/env python3
"""Verify ADR-0024 R5 commit references, report SHA256 rows, and R3/R4/R5 SHA256SUMS manifests."""

from pathlib import Path
import hashlib
import json
import re
import subprocess
import sys

WS = Path("/Users/tangxingpeng/IdeaProjects/me/LumenCortex")
REPO = WS / "wt/core-adr-0024-r5-20261120"
REPORT = REPO / "docs/reports/adr-0024-context-space-implementation-2026-11-18.md"
DOCS = [
    REPORT,
    REPO / "docs/adr/0024-candidate-context-projection-and-budget-guards.md",
    REPO / "docs/adr/0024-candidate-context-projection-and-budget-guards.zh-CN.md",
]


def sha256_file(p: Path) -> str:
    return hashlib.sha256(p.read_bytes()).hexdigest()


def main() -> int:
    # 1. Verify every backticked commit reference resolves to a commit object in REPO.
    refs = set()
    for doc in DOCS:
        text = doc.read_text()
        refs.update(re.findall(r"`([0-9a-f]{7,40})`", text))
        refs.update(re.findall(r"`[^`\n]+@([0-9a-f]{7,40})`", text))

    invalid = []
    for ref in sorted(refs):
        check = subprocess.run(
            ["git", "cat-file", "-e", ref + "^{commit}"],
            cwd=REPO,
            capture_output=True,
            text=True,
        )
        if check.returncode != 0:
            invalid.append({"sha": ref, "error": check.stderr.strip()})
    print(json.dumps({"commit_references": len(refs), "invalid": invalid}))

    # 2. Verify every SHA256 table row in REPORT matches the actual file on disk.
    rows = []
    for line in REPORT.read_text().splitlines():
        cells = line.split("|")
        if len(cells) < 5:
            continue
        mm = re.fullmatch(r"\s*`([^`]+)`\s*", cells[1])
        hm = re.fullmatch(r"\s*`([0-9a-f]{64})`\s*", cells[-2])
        if not mm or not hm:
            continue
        rel = mm.group(1)
        if rel.endswith(".log") and "/" not in rel:
            p = WS / "bench/evidence/adr-0024-r2-remediation-20261119" / rel
        elif (REPO / rel).exists():
            p = REPO / rel
        else:
            p = WS / rel
        rows.append(
            {
                "path": rel,
                "exists": p.is_file(),
                "matches": p.is_file() and sha256_file(p) == hm.group(1),
            }
        )
    bad_rows = [r for r in rows if not r["matches"]]
    print(json.dumps({"report_hash_rows": len(rows), "bad": bad_rows}))

    # 3. Verify all four SHA256SUMS manifests (R3 review, R3 remediation, R4 remediation, R5 remediation).
    manifest_errors = 0
    for name in [
        "adr-0024-r3-review-20261119",
        "adr-0024-r3-remediation-20261119",
        "adr-0024-r4-remediation-20261120",
        "adr-0024-r5-remediation-20261120",
    ]:
        root = WS / "bench/evidence" / name
        manifest = root / "SHA256SUMS"
        if not manifest.is_file():
            print(json.dumps({"manifest": name, "error": "missing SHA256SUMS"}))
            manifest_errors += 1
            continue
        listed = set()
        bad = []
        for line in manifest.read_text().splitlines():
            if not line.strip():
                continue
            expected, rel = line.split(None, 1)
            rel = rel.strip().removeprefix("*").removeprefix("./")
            listed.add(rel)
            p = root / rel
            if not p.is_file() or sha256_file(p) != expected:
                bad.append(rel)
        actual = {
            str(p.relative_to(root))
            for p in root.rglob("*")
            if p.is_file() and p != manifest
        }
        unlisted = sorted(actual - listed)
        missing = sorted(listed - actual)
        if bad or unlisted or missing:
            manifest_errors += 1
        print(
            json.dumps(
                {
                    "manifest": name,
                    "entries": len(listed),
                    "bad": bad,
                    "unlisted": unlisted,
                    "missing": missing,
                    "sha256": sha256_file(manifest),
                }
            )
        )

    if invalid or bad_rows or manifest_errors:
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
