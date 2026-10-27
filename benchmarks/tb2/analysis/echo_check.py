"""Does LC echo each assistant turn's reasoning_content in the next request? (offline, metering logs)

For every pair of consecutive requests of one trial (proxy.jsonl order), compare the reasoning the
response of request i streamed with the reasoning_content the request i+1 carries on its last
assistant message. Also count, per response, reasoning/content/tool-call shapes.

    python3 bench/analysis/echo_check.py
"""

import collections
import json
from pathlib import Path

M = Path(__file__).resolve().parent.parent / "jobs" / "metering"


def stream(path):
    rc, c, tools, fin = [], [], 0, None
    for line in path.open(encoding="utf-8", errors="replace"):
        if not line.startswith("data: {"):
            continue
        try:
            d = json.loads(line[6:])
        except ValueError:
            continue
        for ch in d.get("choices", []):
            de = ch.get("delta", {})
            rc.append(de.get("reasoning_content") or "")
            c.append(de.get("content") or "")
            tools += bool(de.get("tool_calls"))
            fin = ch.get("finish_reason") or fin
    return "".join(rc), "".join(c), tools, fin


rows = [json.loads(l) for l in (M / "proxy.jsonl").open()]
by_tag = collections.defaultdict(list)
for r in rows:
    if r.get("tag", "").startswith("A+/") and r.get("status") == 200:
        by_tag[r["tag"]].append(r["n"])

pairs = collections.Counter()
shapes = collections.Counter()
examples = collections.defaultdict(list)
for tag, ns in by_tag.items():
    for i, n in enumerate(ns):
        resp = M / "bodies" / f"{n:06d}.resp.txt"
        if not resp.is_file():
            continue
        r, c, tools, fin = stream(resp)
        dup = bool(c) and c == r
        shapes[(fin, bool(r), bool(c), tools > 0, dup)] += 1
        if i + 1 < len(ns):
            req = M / "bodies" / f"{ns[i + 1]:06d}.req.json"
            if not req.is_file():
                continue
            body = json.loads(req.read_text())
            asst = [m for m in body["messages"] if m.get("role") == "assistant"]
            echoed = (asst[-1].get("reasoning_content") or "") if asst else None
            key = ("hadReasoning" if r else "noReasoning",
                   "echoMissing" if not echoed else ("echoEqual" if echoed == r else "echoDiffers"))
            pairs[key] += 1
            if key == ("hadReasoning", "echoMissing") and len(examples[key]) < 5:
                examples[key].append((tag, n, ns[i + 1], fin, tools))

print("response shapes (finish, hasReasoning, hasContent, hasToolCalls, contentDuplicatesReasoning):")
for k, v in sorted(shapes.items(), key=lambda kv: -kv[1]):
    print(f"  {v:4d}  {k}")
print("echo of reasoning in the next request:")
for k, v in sorted(pairs.items()):
    print(f"  {v:4d}  {k}")
for k, ex in examples.items():
    print("examples", k, ex)
