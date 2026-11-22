#!/usr/bin/env python3
"""Three-way comparison (Opus, Sol, new) of rendered failure payloads for one case set.

usage: compare3.py CASE_INPUT_DIR OPUS_DIR SOL_DIR NEW_DIR [--corpus]
Corpus inputs are {"result","receipt"} captures plus .meta; matrix inputs are rvCase JSON."""
import json, math, statistics, sys
from collections import Counter
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
import labelgrammar as g

FAILED = {"partially_modified", "published_post_error", "external_modification_detected", "publication_uncertain",
          "rollback_failed", "failed_before_write", "unsupported"}
VERS = ("opus", "sol", "new")


def rank(r):
    if r["Error"] or r["PublishState"] in FAILED:
        return 0
    return 1 if r["Verified"] and r["Published"] and r["PublishState"] == "published_verified" else 2


def load_case(path, corpus):
    d = json.loads(path.read_text())
    if corpus:
        return d["receipt"]["Files"], d["result"]["files"]
    return d["receipts"], d["results"]


def measure(raw, receipts, results, check_order=True):
    p = json.loads(raw.decode("utf-8", errors="strict"))
    files = p.get("files", [])
    paths = list(dict.fromkeys(g.display(r["Path"]) for r in receipts))
    labels = [f["path"] for f in files]
    m = dict(bytes=len(raw), rows=len(files), failed=sum(f["publishState"] in FAILED for f in files),
             omitted=p.get("filesOmitted", 0), dup=len(labels) - len(set(labels)), problems=[])
    for mode in ("strict", "lenient", "loose"):
        m[mode] = sum(len(g.resolve(l, paths, mode)) != 1 for l in labels)
    pub = [f["path"] for f in p.get("publishedFiles", [])]
    m["pub_ambiguous"] = sum(len(g.resolve(l, paths, "loose")) != 1 for l in pub) + (len(pub) - len(set(pub)))
    by = p.get("filesOmittedByState", {})
    if sum(by.values()) != m["omitted"] or m["rows"] + m["omitted"] != len(receipts):
        m["problems"].append("omission totals")
    if Counter(f["publishState"] for f in files) + Counter(by) != Counter(r["PublishState"] for r in receipts):
        m["problems"].append("state multiset")
    if len(raw) > 4096:
        m["problems"].append("over 4096")
    # Distinct attempted files that share one display path (a redaction collision) have no identity;
    # the renderer omits them, so they are excluded from the expected order and the field check.
    shared = Counter(g.display(r["Path"]) for r in {r["Path"]: r for r in receipts}.values())
    order = [g.display(r["Path"]) for r in sorted(receipts, key=rank) if shared[g.display(r["Path"])] == 1]
    order = list(dict.fromkeys(order))
    got = [g.resolve(l, paths, "lenient") for l in labels]
    if check_order and all(len(x) == 1 for x in got) and [x[0] for x in got] != order[:len(got)]:
        m["problems"].append("not a priority prefix")
    byp = {}
    for r in receipts:  # identical raw paths: the renderer keeps the first row and omits the rest
        if shared[g.display(r["Path"])] == 1:
            byp.setdefault(g.display(r["Path"]), r)
    for f, x in zip(files, got):
        if len(x) != 1 or x[0] not in byp:
            continue
        r = byp[x[0]]
        if any(f.get(k, False) != r[k[0].upper() + k[1:]] for k in ("publishState", "published", "verified", "wrote", "created", "deleted", "rolledBack")):
            m["problems"].append("row fields")
        if ("newRev" in f or "sha256" in f) and not r["Verified"]:
            m["problems"].append("fabricated revision")
    return m


def dist(v):
    v = sorted(v)
    return dict(min=v[0], p50=statistics.median(v), p95=v[math.ceil(.95 * len(v)) - 1], max=v[-1], mean=round(statistics.mean(v), 2))


def main():
    inputs, dirs, corpus = Path(sys.argv[1]), dict(zip(VERS, map(Path, sys.argv[2:5]))), "--corpus" in sys.argv
    names = sorted(p.stem for p in dirs["new"].glob("*.json"))
    assert all(sorted(p.stem for p in d.glob("*.json")) == names for d in dirs.values()), "case sets differ"
    per, sizes, problems = {}, {v: [] for v in VERS}, []
    for n in names:
        ip = inputs / f"{n}.json"
        receipts, results = load_case(ip, corpus) if ip.exists() else ([], [])
        per[n] = {}
        for v in VERS:
            raw = dirs[v].joinpath(f"{n}.json").read_bytes().rstrip(b"\n")
            if not receipts:
                p = json.loads(raw); per[n][v] = dict(bytes=len(raw), rows=0, failed=0, omitted=0, dup=0, strict=0, lenient=0, loose=0, pub_ambiguous=0, problems=[] if p.get("path") else ["refusal without path"], raw=raw)
            else:
                # Opus ranks failed_before_write/unsupported rows without error text after verified rows.
                per[n][v] = measure(raw, receipts, results, check_order=v != "opus") | {"raw": raw}
            sizes[v].append(per[n][v]["bytes"])
            problems += [(v, n, x) for x in per[n][v]["problems"]]
    print(f"cases={len(names)} problems={len(problems)}")
    for x in problems[:20]:
        print("  problem", x)
    for v in VERS:
        tot = Counter()
        for n in names:
            for k in ("dup", "strict", "lenient", "loose", "pub_ambiguous"):
                tot[k + "_rows"] += per[n][v][k]
                tot[k + "_cases"] += per[n][v][k] > 0
        print(v, "sizes", dist(sizes[v]), dict(tot))
    for a, b in (("opus", "sol"), ("opus", "new"), ("sol", "new")):
        c = Counter(); fewer = []
        for n in names:
            x, y = per[n][a], per[n][b]
            c["identical"] += x["raw"] == y["raw"]
            for k in ("rows", "failed"):
                c[f"more_{k}"] += y[k] > x[k]; c[f"fewer_{k}"] += y[k] < x[k]
            c["new_ambiguity_loose"] += x["loose"] == 0 and y["loose"] > 0
            c["new_ambiguity_lenient"] += x["lenient"] == 0 and y["lenient"] > 0
            c["less_ambiguity_loose"] += y["loose"] < x["loose"]
            if y["failed"] < x["failed"]:
                fewer.append((n, x["failed"], y["failed"], x["loose"], y["loose"]))
        print(f"{a} -> {b}", dict(c))
        for f in fewer[:30]:
            print(f"   fewer failed rows: {f[0]} {a}={f[1]} (ambiguous {f[3]}) {b}={f[2]} (ambiguous {f[4]})")
    out = Path(sys.argv[1]).parent / ("compare3-per-case.tsv")
    if "--tsv" in sys.argv:
        with open(sys.argv[sys.argv.index("--tsv") + 1], "w") as fh:
            fh.write("case\t" + "\t".join(f"{v}_{k}" for v in VERS for k in ("bytes", "rows", "failed", "omitted", "dup", "lenient", "loose")) + "\n")
            for n in names:
                fh.write(n + "\t" + "\t".join(str(per[n][v][k]) for v in VERS for k in ("bytes", "rows", "failed", "omitted", "dup", "lenient", "loose")) + "\n")


if __name__ == "__main__":
    main()
