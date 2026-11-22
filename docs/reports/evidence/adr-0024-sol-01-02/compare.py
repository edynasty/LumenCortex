#!/usr/bin/env python3
"""Check the frozen corpus by original file identity, then emit per-case and aggregate measurements."""

from collections import Counter
from functools import lru_cache
import csv
import json
import math
from pathlib import Path
import statistics
import sys


def load(path):
    raw = path.read_bytes().removesuffix(b"\n")
    data = json.loads(raw.decode("utf-8", errors="strict"))
    assert len(raw) <= 4096, path
    return raw, data


def states(rows):
    return Counter(r["publishState"] for r in rows)


def failed_rows(rows):
    return sum(r["publishState"] not in {"published_verified", "not_attempted"} for r in rows)


def distinct(rows):
    return len({r["path"] for r in rows})


def matches(label, full):
    """Interpret the renderer's literal components, directory gaps and elided component starts."""
    wanted, actual = label.split("/"), full.split("/")
    if wanted[-1] != actual[-1]:
        return False

    @lru_cache(None)
    def walk(i, j):
        if i == len(wanted):
            return j == len(actual)
        if wanted[i] == "…":
            return any(walk(i + 1, end) for end in range(j, len(actual)))
        if j == len(actual):
            return False
        if wanted[i].startswith("…"):
            return actual[j].endswith(wanted[i][1:]) and walk(i + 1, j + 1)
        return wanted[i] == actual[j] and walk(i + 1, j + 1)

    return walk(0, 0)


def verify_candidate(name, payload, input_path):
    files, published = payload.get("files", []), payload.get("publishedFiles", [])
    if not input_path.is_file():
        assert "_refusal_" in name and not files and not published and payload.get("path"), name
        return
    inputs = json.loads(input_path.read_bytes())
    receipts = {row["Path"]: row for row in inputs["receipt"]["Files"]}
    results = {row["path"]: row for row in inputs["result"]["files"]}
    assert receipts.keys() == results.keys(), name
    grouped = {}
    for path in receipts:
        grouped.setdefault(path.rsplit("/", 1)[-1], []).append(path)

    aliases = {}

    def resolve(label):
        if label not in aliases:
            if label in receipts:
                aliases[label] = label
            else:
                paths = [p for p in grouped.get(label.rsplit("/", 1)[-1], ()) if matches(label, p)]
                assert len(paths) == 1, (name, label, paths)
                aliases[label] = paths[0]
        return aliases[label]

    resolved = [resolve(row["path"]) for row in files]
    assert len(set(resolved)) == distinct(files) == len(files), name
    expected_order = [p for p, r in receipts.items()
                      if r["Error"] or r["PublishState"] not in {"published_verified", "not_attempted"}]
    expected_order += [p for p, r in receipts.items() if r["PublishState"] == "published_verified"]
    expected_order += [p for p, r in receipts.items() if r["PublishState"] == "not_attempted"]
    assert resolved == expected_order[:len(files)], (name, resolved)
    for path, row in zip(resolved, files, strict=True):
        r, f = receipts[path], results[path]
        for field in ("publishState", "published", "verified", "wrote", "created", "deleted", "rolledBack"):
            assert row.get(field, False) == r[field[0].upper() + field[1:]], (name, row, field)
        if "newRev" in row:
            assert r["Verified"] and row["newRev"] == f["newRev"], (name, row)
        if "sha256" in row:
            assert r["Verified"] and row["sha256"] == r["NewSHA256"], (name, row)
    omitted = Counter(receipts[p]["PublishState"] for p in expected_order[len(files):])
    assert omitted == Counter(payload.get("filesOmittedByState", {})), name
    assert len(receipts) - len(files) == payload.get("filesOmitted", 0), name
    unique_published = {resolve(row["path"]) for row in published}
    assert len(unique_published) == distinct(published) == len(published), name
    for row in published:
        result = results[resolve(row["path"])]
        assert result["published"], (name, row)
        assert all(row.get(f) == result.get(f) for f in result.keys() - {"path", "error"}), (name, row)
    assert sum(f.get("published", False) for f in results.values()) - len(published) == payload.get("publishedFilesOmitted", 0), name


def distribution(values):
    values = sorted(values)
    return {
        "min": values[0], "p50": statistics.median(values),
        "p95": values[math.ceil(0.95 * len(values)) - 1], "max": values[-1],
        "mean": round(statistics.mean(values), 2),
    }


def main():
    carbon, opus, sol, inputs, csv_path = map(Path, sys.argv[1:])
    dirs = [carbon, opus, sol]
    names = sorted(p.name for p in sol.glob("*.json"))
    assert len(names) == 684 and all(names == sorted(p.name for p in d.glob("*.json")) for d in dirs)
    labels = ["carbon", "opus", "sol"]
    measurements, sizes, comparisons = [], {v: [] for v in labels}, {}
    for name in names:
        data = [load(d / name) for d in dirs]
        summary = {}
        for v, (raw, p) in zip(labels, data, strict=True):
            rows = p.get("files", [])
            omitted_states = p.get("filesOmittedByState", {})
            assert sum(omitted_states.values()) == p.get("filesOmitted", 0), (v, name)
            assert all(data[0][1].get(k) == p.get(k) for k in
                       ("code", "commitState", "dirtyFailure", "partialPublication")), (v, name)
            assert states(rows) + Counter(omitted_states) == states(data[0][1].get("files", [])) + Counter(data[0][1].get("filesOmittedByState", {})), (v, name)
            sizes[v].append(len(raw))
            summary[v] = {
                "bytes": len(raw), "rows": len(rows), "failed": failed_rows(rows),
                "unique": distinct(rows), "duplicate": len(rows) - distinct(rows),
                "omitted": p.get("filesOmitted", 0),
                "omitted_by_state": json.dumps(omitted_states, sort_keys=True, separators=(",", ":")),
                "published_duplicate": len(p.get("publishedFiles", [])) - distinct(p.get("publishedFiles", [])),
            }
        verify_candidate(name, data[2][1], inputs / name)
        measurements.append({"case": name, **{f"{v}_{k}": n for v in labels for k, n in summary[v].items()}})
        for i, j in ((0, 1), (0, 2), (1, 2)):
            before, after = summary[labels[i]], summary[labels[j]]
            m = comparisons.setdefault(f"{labels[i]} -> {labels[j]}", Counter())
            m["identical"] += data[i][0] == data[j][0]
            for key in ("rows", "failed"):
                m[f"more_{key}"] += after[key] > before[key]
                m[f"fewer_{key}"] += after[key] < before[key]
            for key in ("duplicate", "published_duplicate"):
                m[f"fewer_{key}"] += after[key] < before[key]
                m[f"new_{key}"] += before[key] == 0 and after[key] > 0
    with csv_path.open("x", encoding="utf-8", newline="") as f:
        writer = csv.DictWriter(f, fieldnames=measurements[0], lineterminator="\n")
        writer.writeheader()
        writer.writerows(measurements)
    print(f"cases={len(names)} invalid_utf8=0 invalid_json=0 over_4096=0")
    for v in labels:
        print(v, distribution(sizes[v]),
              "duplicate files:", sum(row[f"{v}_duplicate"] for row in measurements),
              "duplicate publishedFiles:", sum(row[f"{v}_published_duplicate"] for row in measurements))
    for k, m in comparisons.items():
        print(k, dict(m))
    for name in ("deep_f32_v00_u00_eio.json", "samename_head_f20_v00_u00_eio.json", "samename_head_f32_v00_u00_eio.json"):
        print(next(row for row in measurements if row["case"] == name))


if __name__ == "__main__":
    main()
