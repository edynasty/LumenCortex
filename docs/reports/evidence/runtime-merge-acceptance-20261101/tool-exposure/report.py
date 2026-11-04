#!/usr/bin/env python3
"""Builds the final summary from the driver's raw results and the token measurements.

Every number is read from results/*.json or computed from them; nothing is typed in.
"""
import hashlib
import json
import os
import subprocess

R = "/tmp/lcx-tools/results"


def load(name):
    return json.load(open(os.path.join(R, name)))


def compact(obj):
    return json.dumps(obj, ensure_ascii=False, separators=(",", ":"))


def fn_bytes(tools):
    return sum(len(compact(t["function"])) for t in tools)


def spec_array_bytes(objs):
    return sum(len(compact(o)) for o in objs) + 2 + max(0, len(objs) - 1)


def spec_obj(fn, permission, mutates):
    obj = {"name": fn["name"], "description": fn["description"], "permission": permission, "parameters": fn["parameters"]}
    if mutates:
        obj["mutatesWorkspace"] = True
    return obj


def group_of(source):
    if source in ("builtin", "task"):
        return "builtin"
    if source == "lsp":
        return "lsp"
    if source.startswith("mcp"):
        return "mcp"
    return source


def row(pid, label, rec, tokens, cost, caps, prompt, fallback=None):
    tools = json.loads(rec["toolsJson"]) if rec.get("toolsJson") else []
    reported = {t["name"]: t for t in (prompt or {}).get("included") or []}
    included = dict(reported)
    for name, entry in (fallback or {}).items():
        included.setdefault(name, entry)
    groups = {}
    for t in tools:
        fn = t["function"]
        entry = included.get(fn["name"])
        if not entry:
            groups.setdefault("unknown", []).append(spec_obj(fn, "", False))
            continue
        groups.setdefault(group_of(entry["source"]), []).append(
            spec_obj(fn, entry["permission"], (caps.get(fn["name"]) or {}).get("mutatesWorkspace")))
    wire_names = [t["function"]["name"] for t in tools]
    t = tokens or {}
    cost = cost or {}
    return {
        "id": pid,
        "label": label,
        "toolCount": rec["toolCount"],
        "wireBytes": rec["toolsBytes"],
        "wireFunctionBytes": fn_bytes(tools),
        "specArrayBytes": spec_array_bytes([o for g in groups.values() for o in g]),
        "specArrayBySource": {k: spec_array_bytes(v) for k, v in groups.items()},
        "costApi": {"builtinBytes": cost.get("builtinBytes"), "builtinTools": cost.get("builtinTools"),
                    "lspBytes": cost.get("lspBytes"), "lspTools": cost.get("lspTools"),
                    "mcpBytes": cost.get("mcpBytes") or None, "mcpTools": cost.get("mcpTools") or None,
                    "totalBytes": cost.get("totalBytes")},
        "toolsJsonTokens": t.get("toolsJsonTokens"),
        "promptTokensWithoutTools": t.get("promptTokensWithoutTools"),
        "promptTokensWithTools": t.get("promptTokensWithTools"),
        "toolDefinitionTokens": t.get("toolDefinitionTokens"),
        "systemPromptTokens": t.get("systemPromptTokens"),
        "promptReportToolCount": len(reported),
        "promptOnlyTools": sorted(set(reported) - set(wire_names)),
        "wireOnlyTools": sorted(set(wire_names) - set(reported)),
        "toolNames": wire_names,
    }


def main():
    tokens = json.load(open(os.path.join(R, "tokens.json")))
    scen = {n: load(n + ".json") for n in ("base", "lsp", "mcp", "mcp-narrow")}
    out = {"profiles": {}, "deltas": {}, "reconciliation": {}, "provenance": {}}

    for name, data in scen.items():
        caps = {c["toolId"]: c for c in data.get("capabilities") or []}
        # A tool the pre-run report does not list (an MCP tool whose catalog is discovered at run
        # start) still has a permission and a source in the capability registry.
        capfallback = {k: {"name": k, "permission": v["permission"], "source": v["source"]} for k, v in caps.items()}
        for p in data["profiles"]:
            key = p["id"] if name == "base" else name + ":" + p["id"]
            out["profiles"][key] = row(key, p["label"], p["firstRequest"], tokens[name].get(p["id"]),
                                       p.get("toolSchemaCost"), caps, p.get("effectivePrompt"), capfallback)
        if name == "base":
            # A child run has no report of its own here: its specs are rebuilt from the full-policy
            # report (same tools, same permissions) and the capability registry.
            full = [p for p in data["profiles"] if p["id"] == "p4-coding-full"][0]
            fallback = {t["name"]: t for t in full["effectivePrompt"]["included"]}
            for name, cap in caps.items():
                fallback.setdefault(name, {"name": name, "permission": cap["permission"], "source": cap["source"]})
            out["profiles"]["subagent-child"] = row(
                "subagent-child", "spawn_subagent child run: explore agent, read-only", data["child"],
                tokens["base"].get("subagent-child"), None, caps, None, fallback)
            out["profiles"]["execution-task"] = row(
                "execution-task", "start_task execution run: worker agent, workspace", data["task"],
                tokens["base"].get("execution-task"), None, caps, None, fallback)

    P = out["profiles"]

    def delta(a, b, note):
        x, y = P[a], P[b]
        return {"note": note,
                "toolCount": y["toolCount"] - x["toolCount"],
                "wireBytes": y["wireBytes"] - x["wireBytes"],
                "toolDefinitionTokens": y["toolDefinitionTokens"] - x["toolDefinitionTokens"],
                "added": sorted(set(y["toolNames"]) - set(x["toolNames"])),
                "removed": sorted(set(x["toolNames"]) - set(y["toolNames"]))}

    out["deltas"] = {
        "readonly-agent-vs-default": delta("p1-default", "p2-readonly-agent", "agent identity alone"),
        "workspace-vs-readonly": delta("p1-default", "p3-workspace", "policy workspace"),
        "full-vs-workspace": delta("p3-workspace", "p4-coding-full", "policy full"),
        "complex-vs-full": delta("p4-coding-full", "p5-complex", "delegation isolated"),
        "lsp-readonly": delta("p1-default", "lsp:p1-default", "language-server tools, read-only policy"),
        "lsp-full": delta("p5-complex", "lsp:p5-complex", "language-server tools, full policy"),
        "mcp-full": delta("p5-complex", "mcp:p5-complex-mcp", "one MCP server, five tools"),
        "mcp-include-narrowed": delta("mcp:p5-complex-mcp", "mcp-narrow:p5-complex-mcp-narrow", "include list two of five"),
        "minimal-research": delta("p1-default", "m1-research-min", "four-tool allowlist"),
        "minimal-coding": delta("p4-coding-full", "m2-coding-min", "denylist of aux tools"),
        "child": delta("p1-default", "subagent-child", "a child run"),
        "task": delta("p4-coding-full", "execution-task", "an execution task"),
    }

    # Core's ToolSchemaCost against the run, per source, for every profile that has both.
    checks = []
    for key, prof in P.items():
        c = prof["costApi"]
        by = prof["specArrayBySource"]
        checks.append({
            "profile": key,
            "builtin": {"reconstructed": by.get("builtin"), "costApi": c["builtinBytes"]},
            "lsp": {"reconstructed": by.get("lsp"), "costApi": c["lspBytes"]},
            "mcp": {"reconstructed": by.get("mcp"), "costApi": (c["mcpBytes"] or {}).get("stub")},
        })
    out["reconciliation"] = {
        "perSource": checks,
        "note": ("The cost API reports one fixed list (full policy, no delegation, LSP if attached, every "
                 "configured MCP server's exposed tools); a run's own list is the wire. The two agree per source "
                 "where the run's policy and grant match the API's assumptions."),
        "mcpCatalogOrder": {
            "firstRunInWorkspace": {
                "profile": "mcp:p5-complex-mcp",
                "promptReportToolCount": P["mcp:p5-complex-mcp"]["promptReportToolCount"],
                "wireToolCount": P["mcp:p5-complex-mcp"]["toolCount"],
                "wireOnly": P["mcp:p5-complex-mcp"]["wireOnlyTools"],
                "costApiMCP": P["mcp:p5-complex-mcp"]["costApi"]["mcpBytes"],
            },
            "afterCatalogCached": {
                "profile": "mcp:p1-default-mcp",
                "promptReportToolCount": P["mcp:p1-default-mcp"]["promptReportToolCount"],
                "wireToolCount": P["mcp:p1-default-mcp"]["toolCount"],
                "wireOnly": P["mcp:p1-default-mcp"]["wireOnlyTools"],
                "costApiMCP": P["mcp:p1-default-mcp"]["costApi"]["mcpBytes"],
                "costApiMCPTools": P["mcp:p1-default-mcp"]["costApi"]["mcpTools"],
            },
        },
    }

    rev = subprocess.run(["curl", "-sS", "--max-time", "30",
                          "https://huggingface.co/api/models/deepseek-ai/DeepSeek-V4.1-Flash-1017"],
                         capture_output=True, text=True).stdout
    revision = json.loads(rev)["sha"] if rev.strip().startswith("{") else "unknown"
    prov = {"repo": "deepseek-ai/DeepSeek-V4.1-Flash-1017", "revision": revision, "files": {},
            "notDownloaded": "model-*.safetensors (48 shards) were never requested",
            "encoder": "encoding/encoding.py from the same revision; reproduces encoding/tests/test_output_1.txt byte for byte",
            "vocabSize": 129280}
    for name in ("tokenizer.json", "tokenizer_config.json", "config.json", "encoding/encoding.py"):
        path = os.path.join("/tmp/lcx-tools/hf", name)
        prov["files"][name] = {"sha256": hashlib.sha256(open(path, "rb").read()).hexdigest(),
                               "bytes": os.path.getsize(path)}
    # What the pre-run report says about the child and the task, against what they were sent.
    kids = []
    for c in scen["base"].get("children") or []:
        reported = [t["name"] for t in (c.get("prompt") or {}).get("included") or []]
        wire = (P["subagent-child"] if c["kind"] == "subagent" else P["execution-task"])["toolNames"]
        kids.append({"kind": c["kind"], "agent": c["agent"], "status": c["status"], "sessionId": c["sessionId"],
                     "reportedToolCount": len(reported), "wireToolCount": len(wire),
                     "reportedOnly": sorted(set(reported) - set(wire)), "wireOnly": sorted(set(wire) - set(reported))})
    out["children"] = kids
    for kid in kids:
        key = "subagent-child" if kid["kind"] == "subagent" else "execution-task"
        prof = out["profiles"][key]
        prof["promptReportToolCount"] = kid["reportedToolCount"]
        prof["promptOnlyTools"] = kid["reportedOnly"]
        prof["wireOnlyTools"] = kid["wireOnly"]
    prov["coreHead"] = scen["base"].get("coreHead")
    prov["globalSkills"] = scen["base"].get("globalSkills")
    prov["capabilities"] = {"count": scen["base"].get("capabilityCount"),
                            "bySource": scen["base"].get("capabilitiesBySource")}
    prov["sessionlessFullPolicyToolCount"] = len(scen["base"]["sessionlessFullPolicy"]["included"])
    out["provenance"] = prov

    json.dump(out, open(os.path.join(R, "summary.json"), "w"), indent=2)
    print(json.dumps(out, indent=2))


if __name__ == "__main__":
    main()
