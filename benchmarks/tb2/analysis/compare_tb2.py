"""Paired comparison of two arms from analyze_tb2.py --json outputs (EVAL_PLAN_TB2_DUAL_BASELINE.md §6.3).

    python3 bench/analysis/compare_tb2.py a.json b.json [--exclude-task T] [--seed 0] [--resamples 10000]

Tasks are the unit: attempts of one task are not independent, so the difference in success rate is
the mean over shared tasks of (rate_b - rate_a), with a 95% interval from a bootstrap that resamples
tasks (and, inside each drawn task, the attempts of each arm). Only tasks both arms ran are compared;
the rest are listed. The per-task table keeps every attempt, INFRA included (raw view); --env-valid
drops INFRA_VERIFIER / INFRA_PROVIDER trials first, and says how many it dropped.
"""

import argparse
import collections
import json
import random

INFRA = ("INFRA_VERIFIER", "INFRA_PROVIDER")


def per_task(path, excluded, env_valid):
    trials = json.load(open(path))["trials"]
    out, dropped = collections.defaultdict(list), 0
    for t in trials:
        if t["task"] in excluded:
            continue
        if env_valid and t["class"] in INFRA:
            dropped += 1
            continue
        out[t["task"]].append(1 if t["solved"] else 0)
    return out, dropped


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("a")
    ap.add_argument("b")
    ap.add_argument("--exclude-task", action="append", default=[])
    ap.add_argument("--env-valid", action="store_true")
    ap.add_argument("--seed", type=int, default=0)
    ap.add_argument("--resamples", type=int, default=10_000)
    args = ap.parse_args()
    a, drop_a = per_task(args.a, set(args.exclude_task), args.env_valid)
    b, drop_b = per_task(args.b, set(args.exclude_task), args.env_valid)
    shared = sorted(t for t in a.keys() & b.keys() if a[t] and b[t])
    if not shared:
        raise SystemExit(f"no task was run by both arms (a: {sorted(a)}, b: {sorted(b)})")
    rate = lambda xs: sum(xs) / len(xs)
    diff = lambda ts, aa, bb: sum(rate(bb[t]) - rate(aa[t]) for t in ts) / len(ts)
    rng = random.Random(args.seed)
    boot = []
    for _ in range(args.resamples):
        ts = [rng.choice(shared) for _ in shared]
        aa = {t: [rng.choice(a[t]) for _ in a[t]] for t in set(ts)}
        bb = {t: [rng.choice(b[t]) for _ in b[t]] for t in set(ts)}
        boot.append(diff(ts, aa, bb))
    boot.sort()
    lo, hi = boot[int(0.025 * len(boot))], boot[int(0.975 * len(boot)) - 1]
    print(json.dumps({
        "a": args.a, "b": args.b, "view": "env-valid" if args.env_valid else "raw",
        "droppedAsInfra": {"a": drop_a, "b": drop_b},
        "sharedTasks": len(shared), "onlyA": sorted(a.keys() - b.keys()), "onlyB": sorted(b.keys() - a.keys()),
        "meanTaskRate": {"a": sum(rate(a[t]) for t in shared) / len(shared),
                         "b": sum(rate(b[t]) for t in shared) / len(shared)},
        "diffBminusA": diff(shared, a, b), "diffCI95": [lo, hi],
        "tasksBbetter": sum(rate(b[t]) > rate(a[t]) for t in shared),
        "tasksAbetter": sum(rate(a[t]) > rate(b[t]) for t in shared),
        "perTask": {t: {"a": f"{sum(a[t])}/{len(a[t])}", "b": f"{sum(b[t])}/{len(b[t])}"} for t in shared},
    }, indent=1))


if __name__ == "__main__":
    main()
