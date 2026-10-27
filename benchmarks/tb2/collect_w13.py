"""Per-trial metrics for a Harbor job of the LumenCortex adapter (PREREGISTRATION_TB2_W13.md).

    python3 bench/collect_w13.py bench/jobs/<job> > bench/jobs/<job>/trials.jsonl

Sources, per trial directory: Harbor `result.json` (reward, exception, timings), the adapter's
`agent/{result.json,exit.txt,stderr.txt}`, the Core state database copied to `agent/state`
(assistant tool calls by tool name, tool errors, steps), and the metering proxy log
(`jobs/metering/proxy.jsonl`, requests whose tag is `<arm>/<trial dir>`: count, non-200, prompt,
cached, completion tokens, upstream ms).
"""

import collections
import json
import shutil
import sqlite3
import sys
import tempfile
from datetime import datetime
from pathlib import Path

BENCH = Path(__file__).resolve().parent
PROXY_LOG = BENCH / "jobs" / "metering" / "proxy.jsonl"


def ts(s):
    return datetime.fromisoformat(s.replace("Z", "+00:00")) if s else None


def secs(span):
    if not span:
        return None
    a, b = ts(span.get("started_at")), ts(span.get("finished_at"))
    return round((b - a).total_seconds(), 1) if a and b else None


def proxy_index():
    by_tag = collections.defaultdict(list)
    if PROXY_LOG.is_file():
        for line in PROXY_LOG.open():
            try:
                r = json.loads(line)
            except ValueError:
                continue
            by_tag[r.get("tag", "")].append(r)
    return by_tag


def core_stats(agent_dir):
    dbs = list((agent_dir / "state").rglob("lumencortex.db"))
    if not dbs:
        return {"coreDB": False}
    with tempfile.TemporaryDirectory() as tmp:
        # Copy with the WAL so the read sees the last committed rows; never touch the original.
        for f in dbs[0].parent.glob("lumencortex.db*"):
            shutil.copy2(f, tmp)
        con = sqlite3.connect(f"file:{tmp}/lumencortex.db?mode=ro", uri=True)
        tools = collections.Counter()
        errors = 0
        for (raw,) in con.execute("select json from session_messages where role='assistant'"):
            for call in json.loads(raw).get("tool_calls") or []:
                tools[call.get("name", "?")] += 1
        for (raw,) in con.execute("select json from session_messages where role='tool'"):
            m = json.loads(raw)
            try:
                c = json.loads(m.get("content") or "{}")
            except ValueError:
                c = {}
            if isinstance(c, dict) and (c.get("error") or (c.get("exitCode") not in (None, 0))):
                errors += 1
        steps = con.execute("select count(*) from agent_steps").fetchone()[0]
        status = [r[0] for r in con.execute("select status from sessions")]
        finish = collections.Counter(
            str(json.loads(r[0]).get("finishReason")) for r in con.execute("select json from agent_steps"))
        con.close()
    return {"coreDB": True, "steps": steps, "toolCalls": sum(tools.values()), "toolsByName": dict(tools),
            "toolNonZero": errors, "sessionStatus": status, "finishReasons": dict(finish)}


def main(job):
    job = Path(job)
    proxy = proxy_index()
    for trial in sorted(p for p in job.iterdir() if (p / "result.json").is_file()):
        r = json.loads((trial / "result.json").read_text())
        agent = trial / "agent"
        task = trial.name.split("__")[0]
        row = {
            "trial": trial.name, "task": task,
            "reward": ((r.get("verifier_result") or {}).get("rewards") or {}).get("reward"),
            "exception": (r.get("exception_info") or {}).get("exception_type"),
            "agentSec": secs(r.get("agent_execution")), "setupSec": secs(r.get("agent_setup")),
            "verifierSec": secs(r.get("verifier")),
        }
        exit_file = agent / "exit.txt"
        row["exit"] = exit_file.read_text().strip() if exit_file.is_file() else None
        res = agent / "result.json"
        if res.is_file():
            try:
                parsed = json.loads(res.read_text())
                row["lcStatus"] = parsed.get("status")
                row["lcError"] = (parsed.get("error") or None) and str(parsed.get("error"))[:300]
            except ValueError:
                row["lcStatus"] = "unparsable"
        row.update(core_stats(agent))
        reqs = [q for tag, qs in proxy.items() if tag.endswith("/" + trial.name) for q in qs]
        ok = [q for q in reqs if q.get("status") == 200]
        usage = [q.get("usage") or {} for q in ok]
        row.update({
            "requests": len(reqs), "non200": len(reqs) - len(ok),
            "non200Codes": dict(collections.Counter(q.get("status") for q in reqs if q.get("status") != 200)),
            "promptTokens": sum(u.get("prompt_tokens") or 0 for u in usage),
            "cachedTokens": sum((u.get("prompt_tokens_details") or {}).get("cached_tokens") or 0 for u in usage),
            "completionTokens": sum(u.get("completion_tokens") or 0 for u in usage),
            "maxPrompt": max((u.get("prompt_tokens") or 0 for u in usage), default=0),
            "upstreamSec": round(sum(q.get("totalMs") or 0 for q in reqs) / 1000, 1),
        })
        print(json.dumps(row, sort_keys=True))


if __name__ == "__main__":
    main(sys.argv[1])
