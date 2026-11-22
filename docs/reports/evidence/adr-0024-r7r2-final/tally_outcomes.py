#!/usr/bin/env python3
"""Sum the per-step outcome tallies that TestR201TwoOSProcessesCreateRace logs with -v.
Usage: tally_outcomes.py LOG"""
import collections
import re
import sys

steps, total, runs = collections.OrderedDict(), collections.Counter(), 0
for line in open(sys.argv[1], encoding="utf-8"):
    if line.startswith("--- PASS: TestR201TwoOSProcessesCreateRace ("):
        runs += 1
    m = re.search(r"stress \(Create=(\w+)\) (\w+) released (\w+): rounds=(\d+)(.*)", line)
    if not m:
        continue
    key = f"Create={m.group(1):5s} {m.group(2):28s} released {m.group(3):17s}"
    counts = steps.setdefault(key, collections.Counter())
    for name, n in re.findall(r"(\w+)=(\d+)", m.group(5)):
        counts[name] += int(n)
        total[name] += int(n)

print(f"passing runs of the whole test: {runs}")
for key, counts in steps.items():
    parts = " ".join(f"{k}={v}" for k, v in sorted(counts.items(), key=lambda kv: -kv[1]))
    print(f"{key} rounds={sum(counts.values()):5d}: {parts}")
print(f"all stress rounds: {sum(total.values())}")
for name, n in sorted(total.items(), key=lambda kv: -kv[1]):
    print(f"  {name:36s} {n:6d}")
