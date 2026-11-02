import json, sys, os
keep = {
 'restart': ['mcp.start#1','sample:running#1','after-disable','mcp.start#2','sample:running#2','engine.close.begin','orphans.after-close'],
 'crash': ['kill','survivors@5s','survivors@30s','run.end','after-failed-call','restarted','engine.close.begin','orphans.after-close'],
 'cancel': ['sample:before-cancel','cancel.inflight','run.cancelled','after-cancel','sample:after-close'],
 'close': ['sample:before-close','engine.close.begin','engine.close.done','sample:after-close'],
 'policy': ['run.end','readonly.run.end','capabilities:disabled','capabilities:enabled'],
 'idle': ['server.pid','sample:after-run','sample:t+300s','mcp.stopped','sample:after-idle-stop','orphans.after-idle','orphans.after-close'],
}
mode = sys.argv[1]
path = f'evidence/{mode}.jsonl'
for line in open(path):
    r = json.loads(line)
    if r.get('what') not in keep.get(mode, []): continue
    drop = {'what','t','elapsed','config','final','lastToolResult','mcpToolsOffered','tree','orphans','survivors','survivorRoots','rows'}
    kv = ' '.join(f'{k}={v}' for k,v in sorted(r.items()) if k not in drop)
    print(f"| {r['elapsed']} | {r['what']} | {kv[:300]} |")
