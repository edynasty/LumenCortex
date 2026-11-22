#!/usr/bin/env python3
"""Compare renderer outputs of a3f19d9 (old: per-result legend) and the follow-up (new: contract in
tool descriptions, inline fallback only for elided labels). Byte counts are of the exact serialized
payload (trailing newline removed); notation bytes are the exact `"pathNotation":"..."` member plus
one separating comma."""
import json, pathlib, re, sys, collections
root = pathlib.Path(sys.argv[1])
old, new = root / "old", root / (sys.argv[2] if len(sys.argv) > 2 else "new")
FAILED = set("external_modification_detected partially_modified publication_uncertain rollback_failed "
             "published_post_error failed_before_write unsupported".split())
MEMBER = re.compile(rb',?"pathNotation":"(?:[^"\\]|\\.)*"')
def load(p):
    raw = p.read_bytes().removesuffix(b"\n")
    return raw, json.loads(raw)
def notation_bytes(raw):
    m = MEMBER.search(raw)
    if not m:
        return 0
    n = len(m.group(0))
    return n if m.group(0).startswith(b",") else n + 1  # first member: count the comma after it
suites = collections.defaultdict(lambda: collections.Counter())
deltas = collections.defaultdict(collections.Counter)
nb = collections.defaultdict(lambda: [0, 0, 0])  # payloads with notation old, new, sum saved
saved_nonoverflow = collections.defaultdict(list)
over = collections.defaultdict(lambda: [0, 0, 0, 0])  # overflow payloads, failed old, failed new, rows old/new diff
maxb = collections.defaultdict(int)
examples = collections.defaultdict(list)
for po in sorted(old.rglob("*.json")):
    rel = po.relative_to(old)
    if rel.parts[0] in ("in",) or po.name.endswith("-costs.json"):
        continue
    pn = new / rel
    suite = rel.parts[0]
    ro, do = load(po); rn, dn = load(pn)
    fo = sum(f.get("publishState") in FAILED for f in do.get("files", []))
    fn = sum(f.get("publishState") in FAILED for f in dn.get("files", []))
    ao, an = len(do.get("files", [])), len(dn.get("files", []))
    s = suites[suite]; s["payloads"] += 1
    s["identical"] += ro == rn
    s["failedOld"] += fo; s["failedNew"] += fn; s["rowsOld"] += ao; s["rowsNew"] += an
    deltas[suite][fn - fo] += 1
    if fn < fo: s["fewerFailed"] += 1
    if fn > fo: s["moreFailed"] += 1
    if an < ao: s["fewerRows"] += 1
    if an > ao: s["moreRows"] += 1
    bo, bn = notation_bytes(ro), notation_bytes(rn)
    nb[suite][0] += bo > 0; nb[suite][1] += bn > 0; nb[suite][2] += bo - bn
    if (do.get("files") == dn.get("files") and fo == fn):
        saved_nonoverflow[suite].append(len(ro) - len(rn))
    if do.get("overflow") or dn.get("overflow"):
        s["overflow"] += 1
    maxb[suite] = max(maxb[suite], len(rn))
    if "truncated" in do and "truncated" not in dn or "truncated" not in do and "truncated" in dn:
        s["truncatedFlip"] += 1
    if fn != fo and len(examples[suite]) < 3:
        examples[suite].append((rel.stem, fo, fn, len(ro), len(rn), bo, bn))
tot = collections.Counter()
print("suite payloads identical failedOld->failedNew (delta) rowsOld->rowsNew moreFailed fewerFailed fewerRows overflow notationPayloads(old/new) notationBytesSaved maxBytesNew truncatedFlip")
for suite in sorted(suites):
    s = suites[suite]; tot.update(s)
    print(suite, s["payloads"], s["identical"], f'{s["failedOld"]}->{s["failedNew"]} ({s["failedNew"]-s["failedOld"]:+d})',
          f'{s["rowsOld"]}->{s["rowsNew"]}', s["moreFailed"], s["fewerFailed"], s["fewerRows"], s["overflow"],
          f'{nb[suite][0]}/{nb[suite][1]}', nb[suite][2], maxb[suite], s["truncatedFlip"])
print("TOTAL", dict(tot))
print("failed-row delta histogram per suite:")
for suite in sorted(deltas):
    print(" ", suite, dict(sorted(deltas[suite].items())))
print("byte savings where shown rows are unchanged (old-new bytes): suite count min median max sum")
for suite in sorted(saved_nonoverflow):
    v = sorted(saved_nonoverflow[suite])
    if v:
        print(" ", suite, len(v), v[0], v[len(v)//2], v[-1], sum(v))
print("examples (name, failedOld, failedNew, bytesOld, bytesNew, notationOld, notationNew):")
for suite in sorted(examples):
    for e in examples[suite]:
        print(" ", suite, e)
