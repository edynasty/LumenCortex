#!/usr/bin/env python3
"""Compare two directories of rendered failure payloads (one JSON file per case) and classify the
differences. Used by verify.sh differential."""
import collections
import json
import os
import sys

before_dir, after_dir = sys.argv[1], sys.argv[2]
names = sorted(os.listdir(before_dir))
if names != sorted(os.listdir(after_dir)):
    sys.exit("the two directories hold different cases")

LIMIT = 4096
same, classes, examples, over = 0, collections.Counter(), {}, 0
for name in names:
    raw_before = open(os.path.join(before_dir, name), encoding="utf-8").read().rstrip("\n")
    raw_after = open(os.path.join(after_dir, name), encoding="utf-8").read().rstrip("\n")
    if len(raw_before.encode()) > LIMIT or len(raw_after.encode()) > LIMIT:
        over += 1
    if raw_before == raw_after:
        same += 1
        continue
    before, after = json.loads(raw_before), json.loads(raw_after)
    rows_before = [f["path"] for f in before.get("files", [])]
    rows_after = [f["path"] for f in after.get("files", [])]
    kinds = []
    if len(rows_after) > len(rows_before):
        kinds.append("more rows shown")
    if len(rows_after) < len(rows_before):
        kinds.append("FEWER ROWS SHOWN")
    if len(set(rows_before)) < len(rows_before):
        kinds.append("rows repeated one text before")
    if len(set(rows_after)) < len(rows_after):
        kinds.append("ROWS REPEAT ONE TEXT AFTER")
    if "path" in after and "path" not in before:
        kinds.append("refused path added")
    for key in sorted(set(before) | set(after)):
        if key in ("files", "filesOmitted", "filesOmittedByState", "path"):
            continue
        if before.get(key) != after.get(key):
            kinds.append("FIELD CHANGED: " + key)
    label = " + ".join(kinds) or "row paths differ"
    classes[label] += 1
    examples.setdefault(label, (name, len(rows_before), len(rows_after)))

print(f"cases={len(names)} identical={same} different={len(names) - same} over_{LIMIT}_bytes={over}")
for label, count in classes.most_common():
    name, n_before, n_after = examples[label]
    print(f"{count:4d}  {label}   e.g. {name}: rows {n_before} -> {n_after}")
