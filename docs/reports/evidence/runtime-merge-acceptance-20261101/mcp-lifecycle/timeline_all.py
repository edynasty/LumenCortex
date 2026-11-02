#!/usr/bin/env python3
"""Render /tmp/lcx-mcp/evidence/*.jsonl into the timeline tables the acceptance report quotes."""
import json
import os

ROOT = "/tmp/lcx-mcp/evidence"

KEEP = {
    "idle": ["server.pid", "run.end", "sample:after-run", "sample:t+300s", "mcp.stopped",
             "sample:after-idle-stop", "orphans.after-idle", "engine.close.begin", "orphans.after-close"],
    "restart": ["mcp.start#1", "sample:running#1", "after-disable", "mcp.start#2", "sample:running#2",
                "engine.close.begin", "orphans.after-close"],
    "crash": ["kill", "survivors@5s", "survivors@30s", "run.end", "after-failed-call", "restarted",
              "survivors@restart", "orphans.after-close"],
    "cancel": ["sample:before-cancel", "cancel.inflight", "run.cancelled", "after-cancel",
               "sample:after-close", "after-close"],
    "close": ["sample:before-close", "engine.close.begin", "engine.close.done", "sample:after-close"],
    "policy": ["run.end", "readonly.run.end", "capabilities:disabled", "capabilities:enabled",
               "computer-use.mcptools", "orphans.after-close"],
    "prefix-close": ["sample:before-close", "engine.close.begin", "engine.close.done",
                     "sample:after-close", "after-close"],
}

DROP = {"what", "t", "elapsed", "config", "final", "lastToolResult", "mcpToolsOffered", "tree",
        "orphans", "survivors", "survivorRoots", "rows", "label", "pid", "descendants"}


def table(mode):
    path = os.path.join(ROOT, mode + ".jsonl")
    if not os.path.exists(path):
        return f"## {mode}\n\n(no evidence)\n"
    rows = []
    for line in open(path):
        rec = json.loads(line)
        if rec.get("what") not in KEEP.get(mode, []):
            continue
        kv = " ".join(f"{k}={v}" for k, v in sorted(rec.items()) if k not in DROP)
        rows.append(f"| {rec['elapsed']} | {rec['what']} | {kv[:320]} |")
    return f"## {mode}\n\n| t | mark | detail |\n|---|---|---|\n" + "\n".join(rows) + "\n"


print("# Browser (Playwright MCP) lifecycle — timeline tables\n")
for mode in ["prefix-close", "idle", "restart", "crash", "cancel", "close", "policy"]:
    print(table(mode))
