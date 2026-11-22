#!/usr/bin/env python3
"""Categorize per-case differences between two renderer output trees (Astra vs remediation)."""
import json, pathlib, sys, collections
a, b = map(pathlib.Path, sys.argv[1:3])
F = set("external_modification_detected partially_modified publication_uncertain rollback_failed published_post_error failed_before_write unsupported".split())
def load(p):
    raw = p.read_bytes().removesuffix(b"\n")
    return raw, json.loads(raw)
def norm(d, drop):
    return {k: v for k, v in d.items() if k not in drop}
stats = collections.defaultdict(collections.Counter); examples = collections.defaultdict(list)
for pa in sorted(a.rglob("*.json")):
    rel = pa.relative_to(a); pb = b / rel
    if not pb.exists():
        continue
    suite = rel.parts[0] if len(rel.parts) > 1 else "."
    ra, da = load(pa); rb, db = load(pb)
    fa = [f["path"] for f in da.get("files", [])]; fb = [f["path"] for f in db.get("files", [])]
    fla = sum(f["publishState"] in F for f in da.get("files", [])); flb = sum(f["publishState"] in F for f in db.get("files", []))
    if ra == rb:
        cat = "identical"
    elif norm(da, {"pathNotation"}) == norm(db, {"pathNotation"}):
        cat = "legend-only"
    elif norm(da, {"pathNotation", "truncated"}) == norm(db, {"pathNotation", "truncated"}):
        cat = "legend+truncated-flag"
    elif flb > fla:
        cat = "more-failed-rows"
    elif flb < fla:
        cat = "fewer-failed-rows"
    elif len(fb) > len(fa):
        cat = "more-lower-class-rows"
    elif len(fb) < len(fa):
        cat = "fewer-lower-class-rows"
    else:
        cat = "same-counts-other-change"
    stats[suite][cat] += 1
    if cat not in ("identical", "legend-only") and len(examples[(suite, cat)]) < 3:
        examples[(suite, cat)].append((rel.stem, len(ra), len(rb), fla, flb, len(fa), len(fb), "truncated" in da, "truncated" in db))
for suite in sorted(stats):
    print(suite, dict(stats[suite]))
for (suite, cat), ex in sorted(examples.items()):
    print("  ", suite, cat, ex)
