"""DSML / sampling diagnostic: replay recorded requests under controlled cells
(EVAL_PLAN_TB2_DUAL_BASELINE.md §7). Default is a dry run that sends nothing.

    python3 bench/analysis/dsml_replay.py                      # selection, cells, request and cost estimate
    python3 bench/analysis/dsml_replay.py --stage 2            # stage-2 cells
    EVAL_AUTHORIZED=1 python3 bench/analysis/dsml_replay.py --execute --stage 1 \
        --g0-url http://127.0.0.1:18080/v1 --g1-url https://api.deepseek.com --g1-key-env DEEPSEEK_API_KEY

Units are recorded W13 requests (bench/jobs/metering/bodies/NNNNNN.req.json, exact bytes LC sent):
every 200 response that carried DeepSeek tool markup (｜DSML｜) anywhere, plus one control per such
request — the request of the same trial, closest in prompt size, whose response was a clean tool call.
A replay changes only the fields a cell names; the history (including the echoed reasoning_content
DeepSeek requires) is untouched. Each replay is sent once per repetition, never retried: a failure is a
result. Responses are stored raw; the API key is read from the named environment variable and never
written or printed.
"""

import argparse
import json
import os
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path

METERING = Path(__file__).resolve().parent.parent / "jobs" / "metering"
DSML = "｜DSML｜"
LEAKS = ("<｜begin▁of▁sentence｜>", "</thinking>", "｜｜DSML｜｜")
# deepseek-flash list price, peak, USD per 1M tokens (archived ds-quick_start_pricing.html). Replays are
# priced as cache misses: an upper bound, since repetitions of one prompt can hit DeepSeek's prefix cache.
PEAK = {"miss": 0.3, "out": 1.2}

# G0: the chain W13 ran on (metering proxy -> local sub2api -> ai.edynasty.asia /v1/responses).
# G1: DeepSeek's own API, Chat Completions, documented model id (Baseline A §2.1).
CELLS = {
    1: {
        "G0-default": {"route": "g0", "set": {}},
        "G1-default": {"route": "g1", "set": {"model": "deepseek-flash"}},
    },
    # Stage 2 runs only if stage 1 leaves DSML-in-reasoning on G1 (plan §7.3). Defaults are effort high,
    # top_p 1.0; temperature is absent on purpose: DeepSeek documents it as a no-op in thinking mode.
    2: {
        "G1-topp095": {"route": "g1", "set": {"model": "deepseek-flash", "top_p": 0.95}},
        "G1-max": {"route": "g1", "set": {"model": "deepseek-flash", "reasoning_effort": "max"}},
        "G1-topp095-max": {"route": "g1", "set": {"model": "deepseek-flash", "top_p": 0.95, "reasoning_effort": "max"}},
        "G1-max-64k": {"route": "g1", "set": {"model": "deepseek-flash", "reasoning_effort": "max", "max_tokens": 65536}},
    },
}
STAGE_UNITS = {1: ("dsml", "control"), 2: ("dsml",)}


def shape(text):
    reasoning, content, tools, fin, done, error = [], [], 0, None, False, False
    for line in text.splitlines():
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
        error = error or bool(event.get("error"))
        for ch in event.get("choices") or []:
            fin = ch.get("finish_reason") or fin
            delta = ch.get("delta") or {}
            reasoning.append(delta.get("reasoning_content") or "")
            content.append(delta.get("content") or "")
            tools += len(delta.get("tool_calls") or [])
    r, c = "".join(reasoning), "".join(content)
    return {"finish": fin, "ended": bool(fin) or done, "error": error, "toolCallDeltas": tools,
            "dsmlReasoning": DSML in r, "dsmlContent": DSML in c, "contentIsReasoning": bool(c) and c == r,
            "leak": [s for s in LEAKS if s in r or s in c], "reasoningChars": len(r), "contentChars": len(c)}


def select(arm):
    rows = []
    for line in (METERING / "proxy.jsonl").open():
        r = json.loads(line)
        body = METERING / "bodies" / f"{r['n']:06d}.resp.txt"
        if r.get("status") == 200 and r.get("tag", "").startswith(arm + "/") and body.is_file():
            r["shape"] = shape(body.read_text(encoding="utf-8", errors="replace"))
            rows.append(r)
    dsml = [r for r in rows if r["shape"]["dsmlReasoning"] or r["shape"]["dsmlContent"]]
    used = {r["n"] for r in dsml}
    controls = []
    for d in dsml:
        size = (d.get("usage") or {}).get("prompt_tokens", 0)
        pool = [r for r in rows if r["tag"] == d["tag"] and r["n"] not in used
                and r["shape"]["finish"] == "tool_calls" and not r["shape"]["leak"]]
        if pool:
            best = min(pool, key=lambda r: abs((r.get("usage") or {}).get("prompt_tokens", 0) - size))
            used.add(best["n"])
            controls.append(best)
    return {"dsml": dsml, "control": controls}


def estimate(units, cells, reps):
    n = sum(len(units[k]) for k in units) * len(cells) * reps
    prompt = sum((r.get("usage") or {}).get("prompt_tokens", 0) for k in units for r in units[k]) * len(cells) * reps
    out = sum((r.get("usage") or {}).get("completion_tokens", 0) for k in units for r in units[k]) * len(cells) * reps
    return n, prompt, out, (prompt * PEAK["miss"] + out * PEAK["out"]) / 1e6


def send(url, key, body, timeout):
    req = urllib.request.Request(url.rstrip("/") + "/chat/completions", data=json.dumps(body).encode(),
                                 headers={"Content-Type": "application/json", "Authorization": f"Bearer {key}",
                                          "X-Exp-Tag": "DSMLDIAG/replay"})
    start = time.monotonic()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return resp.status, resp.read().decode("utf-8", "replace"), time.monotonic() - start
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode("utf-8", "replace")[:2000], time.monotonic() - start
    except (urllib.error.URLError, TimeoutError, OSError) as e:
        return None, f"{type(e).__name__}: {e}", time.monotonic() - start


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--stage", type=int, choices=(1, 2), default=1)
    ap.add_argument("--arm", default="A+", help="proxy tag arm whose recorded requests are replayed")
    ap.add_argument("--reps", type=int, default=3)
    ap.add_argument("--execute", action="store_true")
    ap.add_argument("--g0-url", default="http://127.0.0.1:18080/v1")
    ap.add_argument("--g0-key-env", default="EXP_DUMMY_KEY", help="the metering proxy injects the real key")
    ap.add_argument("--g1-url", default="https://api.deepseek.com")
    ap.add_argument("--g1-key-env", default="DEEPSEEK_API_KEY")
    ap.add_argument("--out", default=str(METERING.parent / "dsml-diagnostic"))
    ap.add_argument("--timeout", type=int, default=3600)
    args = ap.parse_args()

    sel = select(args.arm)
    units = {k: sel[k] for k in STAGE_UNITS[args.stage]}
    cells = CELLS[args.stage]
    for kind, rs in units.items():
        print(f"{kind}: {len(rs)} requests")
        for r in rs:
            u, s = r.get("usage") or {}, r["shape"]
            print(f"  n={r['n']:<5} {r['tag']:<42} prompt={u.get('prompt_tokens', 0):<7} "
                  f"completion={u.get('completion_tokens', 0):<6} finish={s['finish']} "
                  f"dsml(r/c)={int(s['dsmlReasoning'])}/{int(s['dsmlContent'])} content==reasoning={int(s['contentIsReasoning'])} "
                  f"leak={','.join(s['leak']) or '-'}")
    n, prompt, out, usd = estimate(units, cells, args.reps)
    print(f"stage {args.stage}: cells {list(cells)} x reps {args.reps} -> {n} requests, "
          f"~{prompt / 1e6:.2f}M prompt + ~{out / 1e6:.2f}M completion tokens, "
          f"<= ${usd:.2f} at deepseek-flash peak list price (cache misses; G0 is billed by the gateway)")
    if not args.execute:
        print("dry run: nothing sent (Benchmark Status NOT RUN)")
        return
    if os.environ.get("EVAL_AUTHORIZED") != "1":
        sys.exit("refusing: a diagnostic run needs owner authorization (plan §8); set EVAL_AUTHORIZED=1")
    routes = {"g0": (args.g0_url, os.environ.get(args.g0_key_env, "dummy-exp-key")),
              "g1": (args.g1_url, os.environ.get(args.g1_key_env))}
    if any(cells[c]["route"] == "g1" for c in cells) and not routes["g1"][1]:
        sys.exit(f"{args.g1_key_env} is not set")
    out_dir = Path(args.out) / f"stage{args.stage}"
    out_dir.mkdir(parents=True, exist_ok=True)
    index = (out_dir / "index.jsonl").open("a")
    for rep in range(1, args.reps + 1):
        for kind, rs in units.items():
            for r in rs:
                body = json.loads((METERING / "bodies" / f"{r['n']:06d}.req.json").read_text())
                for cell, spec in cells.items():
                    url, key = routes[spec["route"]]
                    status, text, secs = send(url, key, {**body, **spec["set"]}, args.timeout)
                    raw = out_dir / f"{cell}__{r['n']:06d}__r{rep}.txt"
                    raw.write_text(text)
                    rec = {"cell": cell, "kind": kind, "n": r["n"], "rep": rep, "status": status,
                           "sec": round(secs, 1), "raw": raw.name, "set": spec["set"],
                           "shape": shape(text) if status == 200 else None}
                    index.write(json.dumps(rec) + "\n")
                    index.flush()
                    print(json.dumps({k: rec[k] for k in ("cell", "kind", "n", "rep", "status", "sec")}))


if __name__ == "__main__":
    main()
