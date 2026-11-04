"""Offline analysis of Harbor TB2 jobs (EVAL_PLAN_TB2_DUAL_BASELINE.md §6). Reads files only; never
sends a request.

    python3 bench/analysis/analyze_tb2.py --arm A+ bench/jobs/tb2-w13a bench/jobs/tb2-w13b \
        [--rates offpeak|peak] [--exclude-task pytorch-model-cli] [--json out.json]

Inputs per trial directory: Harbor `result.json` (reward, exception, agent_result tokens, timings),
the verifier log, the metering proxy log `bench/jobs/metering/proxy.jsonl` with the raw stream bodies
under `metering/bodies/`, and — for LC arms — the job's `trials.jsonl` written by
`bench/collect_w13.py` (tool calls and non-zero tool results from the Core state database).
Proxy requests are matched to a trial in one of two ways:
  header   LC sends `X-Exp-Tag: <arm>/<trial>`: tag suffix == trial directory name;
  tagfile  arms that cannot send a header run with the proxy tag file set to `<arm>/<job>`
           (run_controlled.sh); with `-n 1` a request belongs to the trial whose agent execution
           window contains its start time.
A trial with neither keeps Harbor's own token fields (`tokenSource: harbor`) and no protocol flags.

Classes (one primary class per trial, first match wins; every trial stays in the raw denominator):
  INFRA_VERIFIER  verifier ran no test and its log shows a network/install failure
  INFRA_PROVIDER  the run ended on provider HTTP errors (non-200 in the last 3 requests, run exit 1)
  TIMEOUT         Harbor AgentTimeoutError
  HARBOR          any other Harbor exception
  SOLVED          reward >= 1
  FAILED          reward < 1, none of the above (task failure; protocol flags below say why it may be
                  the harness's or the model's)
Per-trial protocol flags (from the raw streams of tagged arms):
  stream_cut      a 200 stream with no finish_reason and no [DONE] (often an in-band error event)
  length_final    the run's last reply finished `length` with no tool call
  dsml_in_content DeepSeek tool markup (｜DSML｜) in `content` of a reply with no parsed tool call
"""

import argparse
import collections
from datetime import datetime
import json
import math
import re
from pathlib import Path

BENCH = Path(__file__).resolve().parent.parent
METERING = BENCH / "jobs" / "metering"
INFRA_VERIFIER = re.compile(r"uvx: command not found|failed to download|Could not resolve|Bad Gateway|"
                            r"Temporary failure resolving|curl: command not found")
# A verifier ran tests when pytest printed a per-test result line or its summary line.
TESTS_RAN = re.compile(r"^(PASSED|FAILED|ERROR) |^=+ .*\b\d+ (passed|failed|error)", re.M)
DSML = "｜DSML｜"
# DeepSeek list prices for deepseek-flash, USD per 1M tokens (api-docs.deepseek.com/quick_start/pricing,
# archived bench/baselines/raw/deepseek/ds-quick_start_pricing.html, fetched 2026-10-27).
RATES = {"offpeak": {"hit": 0.003, "miss": 0.15, "out": 0.6},
         "peak": {"hit": 0.006, "miss": 0.3, "out": 1.2}}


def stream_shape(path):
    """finish reason, whether the stream ended, tool-call presence and DSML in content."""
    fin, done, tools, content = None, False, False, []
    for line in path.open(encoding="utf-8", errors="replace"):
        if not line.startswith("data:"):
            continue
        payload = line[5:].strip()
        if payload == "[DONE]":
            done = True
            continue
        try:
            event = json.loads(payload)
        except ValueError:
            continue
        for ch in event.get("choices") or []:
            fin = ch.get("finish_reason") or fin
            delta = ch.get("delta") or {}
            tools = tools or bool(delta.get("tool_calls"))
            content.append(delta.get("content") or "")
    return {"finish": fin, "ended": bool(fin) or done, "tools": tools, "dsml": DSML in "".join(content)}


def load_proxy(arm):
    out = collections.defaultdict(list)
    log = METERING / "proxy.jsonl"
    if log.is_file():
        for line in log.open():
            try:
                r = json.loads(line)
            except ValueError:
                continue
            tag = r.get("tag", "")
            if tag.startswith(arm + "/"):
                out[tag.split("/", 1)[1]].append(r)
    return out


def trial_requests(proxy, job, trial):
    if trial.name in proxy:
        return proxy[trial.name], "header"
    ae = json.loads((trial / "result.json").read_text()).get("agent_execution") or {}
    lo, hi = ae.get("started_at"), ae.get("finished_at")
    if not (lo and hi) or job.name not in proxy:
        return [], None
    # Harbor and the proxy both write ISO-8601 UTC; trim to whole microseconds for a string compare.
    lo, hi = lo.replace("Z", "")[:26], hi.replace("Z", "")[:26]
    reqs = [q for q in proxy[job.name] if lo <= q.get("start", "").replace("Z", "")[:26] <= hi]
    return reqs, "tagfile" if reqs else None


# The request fields that set sampling and thinking; absent means the endpoint's default applies.
PARAMS = ("model", "temperature", "top_p", "reasoning_effort", "thinking", "reasoning", "max_tokens",
          "max_completion_tokens")


def request_params(reqs):
    """The sampling/thinking fields the harness actually sent, as one sorted set over the trial."""
    seen = set()
    for q in reqs:
        body = METERING / "bodies" / f"{q['n']:06d}.req.json"
        if body.is_file():
            sent = json.loads(body.read_text())
            seen.add(json.dumps({k: sent[k] for k in PARAMS if k in sent}, sort_keys=True))
    return sorted(seen)



def classify(trial, reqs):
    r = json.loads((trial / "result.json").read_text())
    reward = ((r.get("verifier_result") or {}).get("rewards") or {}).get("reward")
    exc = (r.get("exception_info") or {}).get("exception_type")
    vlog = trial / "verifier" / "test-stdout.txt"
    vtext = vlog.read_text(errors="replace") if vlog.is_file() else ""
    tests_run = len(TESTS_RAN.findall(vtext))
    exit_file = trial / "agent" / "exit.txt"
    exit_code = exit_file.read_text().strip() if exit_file.is_file() else None
    flags = collections.Counter()
    shapes = []
    for q in reqs:
        body = METERING / "bodies" / f"{q['n']:06d}.resp.txt"
        if q.get("status") == 200 and body.is_file():
            s = stream_shape(body)
            shapes.append(s)
            flags["stream_cut"] += not s["ended"]
            flags["dsml_in_content"] += s["dsml"] and not s["tools"]
    if shapes and shapes[-1]["finish"] == "length" and not shapes[-1]["tools"]:
        flags["length_final"] += 1
    tail = reqs[-3:]
    if reward is not None and reward >= 1:
        cls = "SOLVED"
    elif tests_run == 0 and INFRA_VERIFIER.search(vtext):
        cls = "INFRA_VERIFIER"
    elif exit_code == "exit=1" and tail and all(q.get("status") != 200 for q in tail):
        cls = "INFRA_PROVIDER"
    elif exc == "AgentTimeoutError":
        cls = "TIMEOUT"
    elif exc:
        cls = "HARBOR"
    else:
        cls = "FAILED"
    ar = r.get("agent_result") or {}
    ok = [q for q in reqs if q.get("status") == 200]
    usage = [q.get("usage") or {} for q in ok]
    prompt = sum(u.get("prompt_tokens") or 0 for u in usage) if reqs else (ar.get("n_input_tokens") or 0)
    cached = sum((u.get("prompt_tokens_details") or {}).get("cached_tokens") or 0 for u in usage) if reqs \
        else (ar.get("n_cache_tokens") or 0)
    completion = sum(u.get("completion_tokens") or 0 for u in usage) if reqs else (ar.get("n_output_tokens") or 0)
    ae = r.get("agent_execution") or {}
    return {
        "trial": trial.name, "task": trial.name.split("__")[0], "reward": reward, "class": cls,
        "solved": bool(reward is not None and reward >= 1), "exception": exc, "testsRun": tests_run,
        "requests": len(reqs), "non200": len(reqs) - len(ok), "tokenSource": "proxy" if reqs else "harbor",
        "promptTokens": prompt, "cachedTokens": cached, "completionTokens": completion,
        "trialSec": seconds(r.get("started_at"), r.get("finished_at")),
        "agentSec": seconds(ae.get("started_at"), ae.get("finished_at")),
        "streams": len(shapes), "params": request_params(reqs), **dict(flags),
    }


def seconds(start, end):
    if not (start and end):
        return None
    parse = lambda s: datetime.fromisoformat(s.replace("Z", "+00:00"))
    return round((parse(end) - parse(start)).total_seconds(), 1)


def wilson(k, n, z=1.96):
    if n == 0:
        return (float("nan"), float("nan"))
    p = k / n
    d = 1 + z * z / n
    c = (p + z * z / (2 * n)) / d
    h = z * math.sqrt(p * (1 - p) / n + z * z / (4 * n * n)) / d
    return (max(0.0, c - h), min(1.0, c + h))


def pass_at_k(n, c, k):
    """Unbiased pass@k for one task: 1 - C(n-c, k)/C(n, k) (Chen et al. 2021)."""
    if n - c < k:
        return 1.0
    return 1.0 - math.comb(n - c, k) / math.comb(n, k)


def summarize(rows, rates, excluded):
    rows = [r for r in rows if r["task"] not in excluded]
    n = len(rows)
    solved = sum(r["solved"] for r in rows)
    by_task = collections.defaultdict(list)
    for r in rows:
        by_task[r["task"]].append(r["solved"])
    valid = [r for r in rows if r["class"] not in ("INFRA_VERIFIER", "INFRA_PROVIDER")]
    metered = [r for r in rows if r["tokenSource"] == "proxy"]
    tooled = [r for r in rows if r.get("toolCalls") is not None]
    tool_calls = sum(r["toolCalls"] for r in tooled)
    wall = sorted(r["trialSec"] for r in rows if r["trialSec"] is not None)
    out = {
        "trials": n, "tasks": len(by_task), "solved": solved,
        "successRate": solved / n if n else None, "successRateCI95": wilson(solved, n),
        "classes": dict(collections.Counter(r["class"] for r in rows)),
        "infraFailureRate": sum(r["class"] in ("INFRA_VERIFIER", "INFRA_PROVIDER", "HARBOR") for r in rows) / n
        if n else None,
        "envValid": {"trials": len(valid), "solved": sum(r["solved"] for r in valid),
                     "excludedAsInfra": n - len(valid),
                     "successRate": sum(r["solved"] for r in valid) / len(valid) if valid else None},
        "perTask": {t: f"{sum(v)}/{len(v)}" for t, v in sorted(by_task.items())},
        # Trials with at least one such event, over trials whose streams were metered.
        "protocol": {"meteredTrials": len(metered),
                     **{k: sum(r.get(k, 0) > 0 for r in metered) for k in ("stream_cut", "length_final", "dsml_in_content")},
                     "streams": sum(r["streams"] for r in metered),
                     "cutStreams": sum(r.get("stream_cut", 0) for r in metered)},
        "tools": {"trials": len(tooled), "calls": tool_calls, "nonZero": sum(r["toolNonZero"] for r in tooled),
                  "failureRate": sum(r["toolNonZero"] for r in tooled) / tool_calls if tool_calls else None},
        "wallSec": {"sum": round(sum(wall), 1), "median": wall[len(wall) // 2] if wall else None,
                    "max": wall[-1] if wall else None},
        "tokens": {k: sum(r[k] for r in rows) for k in ("promptTokens", "cachedTokens", "completionTokens")},
        "tokenSources": dict(collections.Counter(r["tokenSource"] for r in rows)),
        # Distinct parameter sets sent, with the number of trials that sent each.
        "requestParams": dict(collections.Counter(p for r in rows for p in r["params"])),
        "requests": sum(r["requests"] for r in rows), "non200": sum(r["non200"] for r in rows),
    }
    ks = sorted({len(v) for v in by_task.values()})
    for k in range(1, (min(ks) if ks else 0) + 1):
        out[f"pass@{k}"] = sum(pass_at_k(len(v), sum(v), k) for v in by_task.values()) / len(by_task)
    t = out["tokens"]
    if rates:
        rate = RATES[rates]
        out["costUSD"] = round(((t["promptTokens"] - t["cachedTokens"]) * rate["miss"] + t["cachedTokens"] * rate["hit"]
                                + t["completionTokens"] * rate["out"]) / 1e6, 4)
        out["costRates"] = f"deepseek-flash list price, {rates}"
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("jobs", nargs="+")
    ap.add_argument("--arm", default="", help="proxy tag arm prefix; empty: Harbor fields only")
    ap.add_argument("--rates", choices=sorted(RATES))
    ap.add_argument("--exclude-task", action="append", default=[])
    ap.add_argument("--json")
    args = ap.parse_args()
    proxy = load_proxy(args.arm) if args.arm else {}
    rows = []
    for job in map(Path, args.jobs):
        core = {}
        if (job / "trials.jsonl").is_file():
            for line in (job / "trials.jsonl").open():
                t = json.loads(line)
                core[t["trial"]] = {k: t.get(k) for k in ("toolCalls", "toolNonZero")}
        for trial in sorted(p for p in job.iterdir() if (p / "result.json").is_file() and "__" in p.name):
            reqs, how = trial_requests(proxy, job, trial)
            row = classify(trial, reqs)
            row["job"], row["match"] = job.name, how
            row.update(core.get(trial.name, {"toolCalls": None, "toolNonZero": None}))
            rows.append(row)
    summary = {"all": summarize(rows, args.rates, set()),
               "excluding": args.exclude_task, "primary": summarize(rows, args.rates, set(args.exclude_task))}
    print(json.dumps(summary, indent=1, default=str))
    if args.json:
        Path(args.json).write_text(json.dumps({"summary": summary, "trials": rows}, indent=1, default=str))


if __name__ == "__main__":
    main()
