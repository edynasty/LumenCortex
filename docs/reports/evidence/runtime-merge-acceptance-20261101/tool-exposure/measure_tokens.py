#!/usr/bin/env python3
"""Token cost of the tool definitions Core sends, in the real deepseek-v4.1-flash-1017 encoding.

Two numbers per profile:
  * toolsJsonTokens: the tokenizer's count of the `tools` value exactly as sent on the wire. Label:
    tokenizer count of the wire JSON.
  * renderedPrompt / toolDefinitionTokens: the official chat encoding (encoding/encoding.py from the
    model repository) rendered over the request's own messages, with and without the tools section.
    toolDefinitionTokens = with - without, i.e. what the tool definitions cost in the rendered prompt.

No model call: only the tokenizer and the reference encoder run.
"""
import json
import sys
import os

sys.path.insert(0, "/tmp/lcx-tools/hf/encoding")
from tokenizers import Tokenizer  # noqa: E402
import encoding as ds  # noqa: E402

TOKENIZER = "/tmp/lcx-tools/hf/tokenizer.json"
tok = Tokenizer.from_file(TOKENIZER)


def oi_messages(messages_json):
    """The request's messages as the [OI] encoder wants them: role + string content."""
    msgs = json.loads(messages_json)
    out = []
    for m in msgs:
        content = m.get("content")
        if not isinstance(content, str):
            content = json.dumps(content, ensure_ascii=False) if content is not None else ""
        out.append({"role": m["role"], "content": content})
    return out


def render(msgs, tools):
    msgs = json.loads(json.dumps(msgs))
    if tools:
        msgs[0]["tools"] = tools
    prompt = ds.encode_messages(msgs, thinking_mode="thinking", reasoning_effort=75)
    return prompt, len(tok.encode(prompt).ids)


def measure(rec):
    if not rec or not rec.get("messagesJson") or not rec.get("toolsJson"):
        return None
    tools = json.loads(rec["toolsJson"]) if rec.get("toolsJson") else []
    msgs = oi_messages(rec["messagesJson"])
    with_prompt, with_tokens = render(msgs, tools)
    _, without_tokens = render(msgs, [])
    tools_json_tokens = len(tok.encode(rec["toolsJson"]).ids) if rec.get("toolsJson") else 0
    system_tokens = len(tok.encode(msgs[0]["content"]).ids) if msgs else 0
    return {
        "toolCount": rec["toolCount"],
        "wireBytes": rec["toolsBytes"],
        "toolsJsonTokens": tools_json_tokens,
        "systemPromptTokens": system_tokens,
        "promptTokensWithoutTools": without_tokens,
        "promptTokensWithTools": with_tokens,
        "toolDefinitionTokens": with_tokens - without_tokens,
        "renderedPromptBytes": len(with_prompt.encode("utf-8")),
        "messages": len(msgs),
    }


def main(paths):
    out = {}
    for path in paths:
        data = json.load(open(path))
        scenario = data["scenario"]
        rows = {}
        for p in data["profiles"]:
            m = measure(p.get("firstRequest"))
            if m:
                rows[p["id"]] = m
        for key, label in (("child", "subagent-child"), ("task", "execution-task")):
            m = measure(data.get(key))
            if m:
                rows[label] = m
        out[scenario] = rows
    print(json.dumps(out, indent=2))
    return out


if __name__ == "__main__":
    main(sys.argv[1:])
