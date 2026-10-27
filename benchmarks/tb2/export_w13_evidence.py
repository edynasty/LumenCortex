#!/usr/bin/env python3
"""Offline evidence export for the W13 Harbor batches (PREREGISTRATION_TB2_W13.md).

Not an evaluation platform and not part of the Harbor execution path: it only reads job
directories that already exist and writes reporting artifacts. Nothing here launches, retries or
re-scores a trial.

    python3 bench/export_w13_evidence.py                       # discover tb2-w13a/b + tb2-w13r*
    python3 bench/export_w13_evidence.py --jobs-dir bench/jobs --out bench/evidence/harbor-remediation-20261027
    python3 bench/export_w13_evidence.py --job tb2-w13a --job tb2-w13b -v

Outputs (under --out, default bench/evidence/harbor-remediation-20261027):
    trials.csv                   one row per trial, every reported metric
    trials.json                  the same rows plus job/meta/ledger structure
    verifier_network_events.csv  verifier-output lines that look like network/bootstrap failures
    per_task_original.csv        per task, original batches only
    per_task_rerun.csv           per task, rerun batches only (empty header when no reruns yet)
    per_task_vs_baseline.csv     original / rerun / ledger-replaced vs the public CSV baselines
    ledger.csv, ledger.json      one replacement slot per original INFRA trial, with mapping
    report.md                    compact human-readable tables + the commands this file runs

Evidence boundary: this module never reads or prints model reasoning, assistant message content or
assistant final text. From the adapter's `agent/result.json` only whitelisted metadata keys are
taken (`status`, `error`, `sessionId`, usage counters); the `final` field is never loaded into a
value that can be serialized. From the Core state database only tool-call names/counters and step
metadata are read, exactly as `collect_w13.core_stats` already does.

Metric definitions are reused from `collect_w13.py` (`proxy_index`, `core_stats`, `secs`), so the
per-trial numbers here stay identical to the collector's.
"""

import argparse
import collections
import csv
import hashlib
import json
import re
import sys
from datetime import datetime, timezone
from pathlib import Path

BENCH = Path(__file__).resolve().parent
sys.path.insert(0, str(BENCH))

import collect_w13 as cw  # noqa: E402  (reuse of the frozen per-trial metric definitions)

DEFAULT_JOBS_DIR = BENCH / "jobs"
DEFAULT_OUT = BENCH / "evidence" / "harbor-remediation-20261027"
DEFAULT_BASELINES = BENCH / "public" / "tb2-widened-matrix.csv"

# Original batches plus any rerun batch named tb2-w13r*.
JOB_PATTERN = re.compile(r"^tb2-w13(a|b|r[^/]*)$")
ORIGINAL_JOBS = ("tb2-w13a", "tb2-w13b")

# Frozen exclusion: the task's verifier environment is broken under emulation (all oracle runs
# fail: jobs/oracle-w13/pytorch-model-cli__wXpmRut, jobs/oracle-w13b/pytorch-model-cli__LVUQEN2,
# jobs/oracle-w13-pytorch-retry/pytorch-model-cli__zaa6yfn each reward 0). PREREGISTRATION_TB2_W13
# §Rules: such a task is reported as ENV and excluded from the comparison.
ENV_EXCLUDED_TASKS = {
    "pytorch-model-cli": "oracle fails 3/3 under emulation (see jobs/oracle-w13*, EXECUTION_LOG_TB2_W13.md)",
}

BASELINE_HARNESSES = ("pi", "specpi", "omp", "claude-code", "opencode", "dsh")
BASELINE_LABEL = {"pi": "Pi", "specpi": "SpecPi", "omp": "OMP", "claude-code": "CC",
                  "opencode": "OpenCode", "dsh": "DSH"}

# Verifier-output signatures of a network/bootstrap failure. A "no tests" trial only counts as an
# environment failure when one of the network kinds matched; the tool-missing kind is context.
NETWORK_PATTERNS = (
    ("dns", re.compile(r"could not resolve|temporary failure in name resolution|name or service not known", re.I)),
    ("http5xx", re.compile(r"\b50[234]\b|bad gateway|service unavailable|gateway time-?out", re.I)),
    ("tls", re.compile(r"ssl_error|ssl_connect|gnutls_handshake|certificate verify failed", re.I)),
    ("download", re.compile(r"failed to download|failed to fetch|unable to fetch|could not fetch", re.I)),
    ("connect", re.compile(r"connection refused|network is unreachable|connection timed out|operation timed out", re.I)),
)
TOOL_MISSING = re.compile(r"\b(uvx|uv|curl|pip3?|wget|git): command not found", re.I)
NETWORK_KINDS = {name for name, _ in NETWORK_PATTERNS}

# Secret redaction for stderr / verifier lines that get copied into the evidence files.
SECRET_PATTERNS = (
    (re.compile(r"(?i)\bsk-[A-Za-z0-9_\-]{6,}"), "sk-***"),
    (re.compile(r"(?i)(bearer\s+)[A-Za-z0-9._\-]{6,}"), r"\1***"),
    (re.compile(r"(?i)((?:api[_-]?key|apikey|access[_-]?token|auth[_-]?token|token|secret|password)"
                r"\s*[:=]\s*)[\"']?[^\s\"',;}]{4,}"), r"\1***"),
)

AGENT_RESULT_KEYS = ("sessionId", "status", "error")  # never "final" or any message body


# --------------------------------------------------------------------------- helpers


def redact(text):
    if not text:
        return ""
    for pattern, repl in SECRET_PATTERNS:
        text = pattern.sub(repl, text)
    return text


def one_line(text, limit=300):
    text = redact(" ".join((text or "").split()))
    return text[:limit]


def sha256_file(path):
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def read_text_limited(path, limit=2_000_000):
    try:
        with open(path, "rb") as fh:
            return fh.read(limit).decode("utf-8", "replace")
    except OSError:
        return ""


def task_of(trial_name):
    return trial_name.split("__")[0]


def trial_started(trial_dir, result):
    started = result.get("started_at")
    if started:
        return started
    try:
        return datetime.fromtimestamp(trial_dir.stat().st_mtime, timezone.utc).isoformat()
    except OSError:
        return ""


def parse_reward(result):
    rewards = (result.get("verifier_result") or {}).get("rewards") or {}
    return rewards.get("reward")


# --------------------------------------------------------------------------- per-trial evidence


def proxy_stats(trial_name, proxy):
    """Same aggregation as collect_w13.main, plus ordering, cache share and proxy error counts."""
    # Exact join only: a tag must end with "/<trial_name>"; keys are "<arm>/<trial dir>".
    reqs = [q for tag, entries in proxy.items() if tag.endswith("/" + trial_name) for q in entries]
    tags = sorted({tag for tag in proxy if tag.endswith("/" + trial_name)})
    reqs.sort(key=lambda q: (q.get("start") or "", q.get("n") or 0))
    codes = [q.get("status") for q in reqs]
    ok = [q for q in reqs if q.get("status") == 200]
    usage = [q.get("usage") or {} for q in ok]
    prompt = sum(u.get("prompt_tokens") or 0 for u in usage)
    cached = sum((u.get("prompt_tokens_details") or {}).get("cached_tokens") or 0 for u in usage)
    completion = [u.get("completion_tokens") or 0 for u in usage]
    errs = collections.Counter(q.get("err") for q in reqs if q.get("err"))
    finishes = collections.Counter(q.get("finish") for q in reqs if q.get("finish"))
    return {
        "proxyTags": tags,
        "requests": len(reqs),
        "non200": len(reqs) - len(ok),
        "non200Codes": dict(collections.Counter(c for c in codes if c != 200)),
        "statusSequence": codes,
        "lastStatus": codes[-1] if codes else None,
        "promptTokens": prompt,
        "cachedTokens": cached,
        "cacheHitRate": round(cached / prompt, 4) if prompt else None,
        "completionTokens": sum(completion),
        "maxPrompt": max((u.get("prompt_tokens") or 0 for u in usage), default=0),
        "maxCompletion": max(completion, default=0),
        "upstreamSec": round(sum(q.get("totalMs") or 0 for q in reqs) / 1000, 1),
        "proxyFinish": dict(finishes),
        "proxyErrs": {str(k): v for k, v in errs.items()},
    }


def gateway_run_ending(stats, stderr_text):
    """A non-200 that ended the run: nothing successful after it, or never anything successful."""
    if stats["requests"] and stats["lastStatus"] != 200:
        return True
    if stats["requests"] and stats["non200"] == stats["requests"]:
        return True
    if not stats["requests"] and re.search(r"provider HTTP 5\d\d", stderr_text or ""):
        return True
    return False


def ctrf_evidence(trial_dir):
    path = trial_dir / "verifier" / "ctrf.json"
    base = {"ctrfPresent": False, "ctrfTool": None, "testsTotal": None, "testsPassed": None,
            "testsFailed": None, "testsSkipped": None, "testsPending": None, "testsOther": None,
            "tests": []}
    if not path.is_file():
        return base
    try:
        data = json.loads(path.read_text())
    except ValueError:
        base["ctrfTool"] = "unparsable"
        return base
    results = data.get("results") or {}
    summary = results.get("summary") or {}
    tests = []
    for t in results.get("tests") or []:
        tests.append({
            "name": one_line(str(t.get("name")), 140),
            "status": t.get("status"),
            "durationSec": round(t.get("duration") or 0, 3),
            "file": one_line(str(t.get("file_path")), 80),
        })
    base.update({
        "ctrfPresent": True,
        "ctrfTool": ((results.get("tool") or {}).get("name")),
        "testsTotal": summary.get("tests"),
        "testsPassed": summary.get("passed"),
        "testsFailed": summary.get("failed"),
        "testsSkipped": summary.get("skipped"),
        "testsPending": summary.get("pending"),
        "testsOther": summary.get("other"),
        "tests": tests,
    })
    return base


def verifier_network_scan(trial_dir):
    """Network/bootstrap failure signatures in the verifier's stdout, redacted and truncated."""
    text = read_text_limited(trial_dir / "verifier" / "test-stdout.txt")
    kinds, events, tool_missing = set(), [], False
    for lineno, line in enumerate(text.splitlines(), 1):
        if not line.strip():
            continue
        hit = False
        for name, pattern in NETWORK_PATTERNS:
            if pattern.search(line):
                kinds.add(name)
                hit = True
        if TOOL_MISSING.search(line):
            tool_missing = True
            hit = True
        if hit and len(events) < 8:
            events.append({"line": lineno, "kind": ",".join(sorted(kinds)) or "tool-missing",
                           "text": one_line(line, 200)})
    return {"kinds": sorted(kinds), "events": events, "toolMissing": tool_missing,
            "networkBootstrap": bool(kinds & NETWORK_KINDS)}


def agent_stderr(trial_dir):
    text = read_text_limited(trial_dir / "agent" / "stderr.txt", 8192)
    codes = re.findall(r"provider HTTP (\d{3})", text)
    return {
        "error": one_line(text, 300),
        "providerCodes": [int(c) for c in codes],
    }

def _skip_json_value(text, index):
    """Return the end of one JSON value without decoding its contents."""
    length = len(text)
    while index < length and text[index].isspace():
        index += 1
    if index >= length:
        return index
    if text[index] == '"':
        index += 1
        escaped = False
        while index < length:
            char = text[index]
            index += 1
            if escaped:
                escaped = False
            elif char == "\\":
                escaped = True
            elif char == '"':
                return index
        return index
    if text[index] in "[{":
        opener = text[index]
        closer = "]" if opener == "[" else "}"
        depth = 0
        in_string = False
        escaped = False
        while index < length:
            char = text[index]
            index += 1
            if in_string:
                if escaped:
                    escaped = False
                elif char == "\\":
                    escaped = True
                elif char == '"':
                    in_string = False
            elif char == '"':
                in_string = True
            elif char == opener:
                depth += 1
            elif char == closer:
                depth -= 1
                if depth == 0:
                    return index
        return index
    while index < length and text[index] not in ",}":
        index += 1
    return index


def _top_level_json_fields(raw, wanted):
    """Decode selected metadata fields and only detect presence of message fields."""
    decoder = json.JSONDecoder()
    fields = {}
    index = 0
    while index < len(raw) and raw[index].isspace():
        index += 1
    if index >= len(raw) or raw[index] != "{":
        raise ValueError("metadata is not a JSON object")
    index += 1
    while index < len(raw):
        while index < len(raw) and (raw[index].isspace() or raw[index] == ","):
            index += 1
        if index >= len(raw) or raw[index] == "}":
            break
        key, index = decoder.raw_decode(raw, index)
        while index < len(raw) and raw[index].isspace():
            index += 1
        if index >= len(raw) or raw[index] != ":":
            raise ValueError("metadata member missing colon")
        index += 1
        value_start = index
        value_end = _skip_json_value(raw, index)
        if key in wanted:
            if key == "final":
                fields[key] = True
            else:
                fields[key] = json.loads(raw[value_start:value_end])
        index = value_end
    return fields


def agent_result_meta(trial_dir):
    """Read adapter metadata without decoding assistant message bodies."""
    path = trial_dir / "agent" / "result.json"
    out = {"lcStatus": None, "lcError": None, "lcSessionId": None, "agentFinalPresent": False,
           "agentResultUnparsable": False}
    if not path.is_file():
        return out
    raw = read_text_limited(path, 4_000_000)
    if not raw.strip():
        out["agentResultUnparsable"] = True
        return out
    try:
        parsed = _top_level_json_fields(raw, {"final", "status", "error", "sessionId"})
    except (TypeError, ValueError, json.JSONDecodeError):
        out["agentResultUnparsable"] = True
        return out
    out["agentFinalPresent"] = "final" in parsed
    out["lcStatus"] = parsed.get("status")
    err = parsed.get("error")
    out["lcError"] = one_line(str(err), 300) if err else None
    out["lcSessionId"] = parsed.get("sessionId")
    return out


def collect_trial(trial_dir, job, kind, proxy):
    result = json.loads((trial_dir / "result.json").read_text())
    name = result.get("trial_name") or trial_dir.name
    task = result.get("task_name") or task_of(name)
    agent_dir = trial_dir / "agent"
    exit_file = agent_dir / "exit.txt"
    stderr = agent_stderr(trial_dir)
    stats = proxy_stats(name, proxy)
    ctrf = ctrf_evidence(trial_dir)
    net = verifier_network_scan(trial_dir)
    agent_meta = agent_result_meta(trial_dir)
    core = cw.core_stats(agent_dir)
    agent_result = result.get("agent_result") or {}
    usage_meta = ((agent_result.get("metadata") or {}).get("usage") or {})
    exception = result.get("exception_info") or {}

    reward = parse_reward(result)
    row = {
        "job": job,
        "jobKind": kind,
        "arm": ((agent_result.get("metadata") or {}).get("arm")
                or (stats["proxyTags"][0].split("/")[0] if stats["proxyTags"] else None)),
        "trial": name,
        "task": task,
        "taskChecksum": result.get("task_checksum"),
        "taskPath": (result.get("task_id") or {}).get("path"),
        "agent": (result.get("agent_info") or {}).get("name"),
        "agentVersion": (result.get("agent_info") or {}).get("version"),
        "trialStartedAt": result.get("started_at"),
        "trialFinishedAt": result.get("finished_at"),
        "reward": reward,
        "exceptionType": exception.get("exception_type"),
        "exceptionMessage": one_line(exception.get("exception_message"), 240),
        "envSetupSec": cw.secs(result.get("environment_setup")),
        "agentSetupSec": cw.secs(result.get("agent_setup")),
        "agentExecSec": cw.secs(result.get("agent_execution")),
        "verifierSec": cw.secs(result.get("verifier")),
        "totalSec": cw.secs({"started_at": result.get("started_at"),
                             "finished_at": result.get("finished_at")}),
        "exit": (exit_file.read_text().strip() if exit_file.is_file() else None),
        "exitMissing": not exit_file.is_file(),
        "harborInputTokens": agent_result.get("n_input_tokens"),
        "harborCacheTokens": agent_result.get("n_cache_tokens"),
        "harborOutputTokens": agent_result.get("n_output_tokens"),
        "agentUsageRequests": usage_meta.get("requests"),
        "coreDB": core.get("coreDB"),
        "steps": core.get("steps"),
        "toolCalls": core.get("toolCalls"),
        "toolsByName": core.get("toolsByName"),
        "toolNonZero": core.get("toolNonZero"),
        "sessionStatus": core.get("sessionStatus"),
        "finishReasons": core.get("finishReasons"),
        "ctrfPresent": ctrf["ctrfPresent"],
        "ctrfTool": ctrf["ctrfTool"],
        "testsTotal": ctrf["testsTotal"],
        "testsPassed": ctrf["testsPassed"],
        "testsFailed": ctrf["testsFailed"],
        "testsSkipped": ctrf["testsSkipped"],
        "testsPending": ctrf["testsPending"],
        "testsOther": ctrf["testsOther"],
        "tests": ctrf["tests"],
        "agentStderrError": stderr["error"],
        "agentStderrProviderCodes": stderr["providerCodes"],
        "verifierNetworkKinds": net["kinds"],
        "verifierNetworkEvents": net["events"],
        "verifierToolMissing": net["toolMissing"],
        "lcStatus": agent_meta["lcStatus"],
        "lcError": agent_meta["lcError"],
        "lcSessionId": agent_meta["lcSessionId"],
        "agentFinalPresent": agent_meta["agentFinalPresent"],
        "agentResultUnparsable": agent_meta["agentResultUnparsable"],
    }
    row.update({k: stats[k] for k in (
        "proxyTags", "requests", "non200", "non200Codes", "statusSequence", "lastStatus",
        "promptTokens", "cachedTokens", "cacheHitRate", "completionTokens", "maxPrompt",
        "maxCompletion", "upstreamSec", "proxyFinish", "proxyErrs")})
    row.update(classify(row, net))
    row["comparisonEligible"] = (
        row["classification"] in ("solved", "MODEL")
        and not row["envExclusion"]
        and row["reward"] is not None
    )
    return row


# --------------------------------------------------------------------------- frozen classification


def classify(row, net):
    """PREREGISTRATION_TB2_W13.md §Metrics/§Rules, as frozen for this remediation.

    solved first (verifier reward 1.0, including trials whose agent timed out); then INFRA when a
    gateway non-200 ended the run, or when no test ran and the verifier failed to bootstrap over
    the network; otherwise MODEL. Truncation, protocol anomalies, agent timeouts and the pytorch
    ENV exclusion are flags, never alternative classes. Raw rewards are never rewritten.
    """
    finish = row.get("finishReasons") or {}
    truncation = bool(finish.get("length")) or (row.get("maxCompletion") or 0) >= 32768
    protocol = bool(re.search(r"neither tool calls nor final content", row.get("agentStderrError") or "", re.I)) or \
        bool(re.search(r"neither tool calls nor final content", row.get("lcError") or "", re.I))
    timeout = row.get("exceptionType") == "AgentTimeoutError" or (row.get("agentExecSec") or 0) >= 1800
    env_reason = ENV_EXCLUDED_TASKS.get(row["task"])
    no_tests = (not row["ctrfPresent"]) or (row["testsTotal"] in (0, None))
    run_ending = gateway_run_ending(
        {"requests": row["requests"], "non200": row["non200"], "lastStatus": row["lastStatus"]},
        row.get("agentStderrError") or "")
    network_bootstrap = net["networkBootstrap"]
    reward = row.get("reward")

    if reward is not None and float(reward) >= 1.0:
        classification, reason = "solved", "verifier reward=1.0"
    elif run_ending:
        codes = row.get("non200Codes") or {}
        detail = ", ".join(f"{c}x{n}" for c, n in sorted(codes.items(), key=lambda kv: str(kv[0])))
        classification = "INFRA"
        reason = f"run-ending gateway non-200 ({detail or 'last request non-200'})"
    elif no_tests and network_bootstrap:
        classification = "INFRA"
        reason = ("no CTRF/tests and verifier bootstrap failed over the network ("
                  + ", ".join(net["kinds"]) + ")")
    else:
        classification = "MODEL"
        reason = (f"verifier ran {row['testsTotal']} test(s)={row['testsPassed']} passed"
                  if row["ctrfPresent"] else "no test evidence and no infra signature in the verifier log")

    return {
        "classification": classification,
        "classReason": reason,
        "gatewayRunEnding": run_ending,
        "gatewayEvents": bool(row["non200"]),
        "noTests": no_tests,
        "networkBootstrap": network_bootstrap,
        "truncation": truncation,
        "protocolAnomaly": protocol,
        "agentTimeout": timeout,
        "artifactCollectionError": bool(timeout and row.get("exitMissing")),
        "envExclusion": bool(env_reason),
        "envExclusionReason": env_reason,
        "rewardMissing": reward is None,
    }


# --------------------------------------------------------------------------- aggregation


def per_task(trials):
    rows = []
    by_task = collections.defaultdict(list)
    for t in trials:
        by_task[t["task"]].append(t)
    for task in sorted(by_task):
        ts = by_task[task]
        attempts = [t for t in ts if t["reward"] is not None]
        solved = sum(1 for t in attempts if float(t["reward"]) >= 1.0)
        classes = collections.Counter(t["classification"] for t in ts)
        rows.append({
            "task": task,
            "attempts": len(attempts),
            "observed": len(ts),
            "solved": solved,
            "rate": round(solved / len(attempts), 4) if attempts else None,
            "solvedClass": classes.get("solved", 0),
            "infra": classes.get("INFRA", 0),
            "model": classes.get("MODEL", 0),
            "meanPromptTokens": round(sum(t["promptTokens"] or 0 for t in attempts) / len(attempts)) if attempts else None,
            "meanCompletionTokens": round(sum(t["completionTokens"] or 0 for t in attempts) / len(attempts)) if attempts else None,
            "meanRequests": round(sum(t["requests"] or 0 for t in attempts) / len(attempts), 1) if attempts else None,
            "meanToolCalls": round(sum(t["toolCalls"] or 0 for t in attempts) / len(attempts), 1) if attempts else None,
            "meanSteps": round(sum(t["steps"] or 0 for t in attempts) / len(attempts), 1) if attempts else None,
            "meanAgentSec": round(sum(t["agentExecSec"] or 0 for t in attempts) / len(attempts), 1) if attempts else None,
            "jobs": sorted({t["job"] for t in ts}),
            "trials": [t["trial"] for t in ts],
        })
    return rows


def load_baselines(path):
    """bench/public/tb2-widened-matrix.csv: task,harness,solved,attempts,rate,..."""
    out = collections.defaultdict(dict)
    if not path.is_file():
        return out
    with open(path, newline="") as fh:
        for row in csv.DictReader(fh):
            if row["task"] not in out[row["harness"]]:
                out[row["harness"]][row["task"]] = {
                    "solved": int(row["solved"]), "attempts": int(row["attempts"]),
                }
    return out


def build_ledger(originals, reruns):
    """One replacement slot per original INFRA trial; assignments are positional, never by score.

    Matching rule: slots in (job, task, trial) order; each required slot consumes the next unused
    rerun trial of the same task in (started_at, trial) order. Extra reruns are listed as unused so
    no attempt can be dropped or picked by outcome.
    """
    infra = [t for t in originals if t["classification"] == "INFRA"]
    infra.sort(key=lambda t: (t["job"], t["task"], t["trial"]))
    pool = collections.defaultdict(list)
    for r in sorted(reruns, key=lambda r: (r["trialStartedAt"] or "", r["trial"])):
        pool[r["task"]].append(r)
    used = set()
    slots = []
    for index, orig in enumerate(infra, 1):
        required = not orig["envExclusion"]
        slot = {
            "slot": f"S{index:02d}",
            "originalJob": orig["job"],
            "originalTrial": orig["trial"],
            "task": orig["task"],
            "originalReward": orig["reward"],
            "originalClass": orig["classification"],
            "originalReason": orig["classReason"],
            "replacementRequired": required,
            "exclusion": orig["envExclusionReason"] or "",
            "rerunJob": None,
            "rerunTrial": None,
            "rerunReward": None,
            "rerunClass": None,
            "counted": False,
            "finalReward": orig["reward"],
            "status": "pending" if required else "env-excluded",
            "note": "",
        }
        candidate = next((r for r in pool[orig["task"]] if r["trial"] not in used), None)
        if candidate is not None:
            used.add(candidate["trial"])
            slot.update({
                "rerunJob": candidate["job"],
                "rerunTrial": candidate["trial"],
                "rerunReward": candidate["reward"],
                "rerunClass": candidate["classification"],
                "counted": required,
                "finalReward": candidate["reward"] if required else orig["reward"],
                "status": "filled" if required else "env-excluded",
            })
            if required and candidate["classification"] == "INFRA":
                slot["note"] = "replacement itself INFRA; kept, no second rerun (prereg one-rerun rule)"
                slot["status"] = "filled-infra"
        elif required and any(r["task"] == orig["task"] for r in reruns):
            slot["note"] = "all reruns of this task already assigned; slot stays open"
        elif not required:
            slot["note"] = "ENV exclusion: no replacement counted, original reward kept as-is"
        slots.append(slot)

    by_task = collections.defaultdict(lambda: {"infraOriginals": 0, "slotsRequired": 0,
                                               "rerunsObserved": 0, "filled": 0, "excluded": 0})
    for s in slots:
        b = by_task[s["task"]]
        b["infraOriginals"] += 1
        if s["replacementRequired"]:
            b["slotsRequired"] += 1
            if s["status"].startswith("filled"):
                b["filled"] += 1
        else:
            b["excluded"] += 1
    for r in reruns:
        by_task[r["task"]]["rerunsObserved"] += 1
    counts = [{"task": task, **vals} for task, vals in sorted(by_task.items())]
    unused = [r for r in reruns if r["trial"] not in used]
    required = sum(1 for s in slots if s["replacementRequired"])
    filled = sum(1 for s in slots if s["replacementRequired"] and s["status"].startswith("filled"))
    if not reruns:
        overall = "awaiting-reruns"
    elif filled < required:
        overall = "incomplete"
    elif unused:
        overall = "complete-with-extra-reruns"
    else:
        overall = "complete"
    return {
        "slots": slots,
        "counts": counts,
        "unusedReruns": [{"job": r["job"], "trial": r["trial"], "task": r["task"],
                          "reward": r["reward"], "classification": r["classification"]} for r in unused],
        "summary": {"infraOriginals": len(slots), "slotsRequired": required, "filled": filled,
                    "envExcluded": len(slots) - required, "rerunsObserved": len(reruns),
                    "status": overall},
    }


def ledger_final(original_rows, ledger, base_rows):
    """Per-task outcome after replacing one INFRA trial per slot; no other attempt changes."""
    out = {}
    by_task_trials = collections.defaultdict(list)
    for t in original_rows:
        by_task_trials[t["task"]].append(t)
    slots_by_task = collections.defaultdict(list)
    for s in ledger["slots"]:
        slots_by_task[s["task"]].append(s)
    for task in sorted(set(list(by_task_trials) + list(base_rows))):
        trials = by_task_trials.get(task, [])
        rewards = []
        replaced = 0
        blocked = 0
        excluded = 0
        for t in trials:
            if t["classification"] != "INFRA":
                rewards.append(t["reward"])
                continue
            slot = next((s for s in slots_by_task.get(task, [])
                         if s["originalTrial"] == t["trial"]), None)
            if slot and slot["replacementRequired"] and slot["counted"]:
                rewards.append(slot["rerunReward"])
                replaced += 1
            elif slot and not slot["replacementRequired"]:
                rewards.append(t["reward"])
                excluded += 1
            else:
                rewards.append(t["reward"])
                blocked += 1
        out[task] = {
            "attempts": len(rewards),
            "solved": sum(1 for r in rewards if r is not None and float(r) >= 1.0),
            "replaced": replaced,
            "pendingInfra": blocked,
            "envExcluded": excluded,
            "complete": blocked == 0 and len(trials) == 3,
        }
    return out


# --------------------------------------------------------------------------- writers


CSV_FIELDS = [
    "job", "jobKind", "arm", "task", "trial", "taskChecksum", "taskPath", "agent", "agentVersion",
    "trialStartedAt", "trialFinishedAt", "reward", "exceptionType", "exceptionMessage",
    "envSetupSec", "agentSetupSec", "agentExecSec", "verifierSec", "totalSec", "exit", "exitMissing",
    "lcStatus", "lcError", "lcSessionId", "agentFinalPresent", "agentResultUnparsable",
    "agentStderrError", "agentStderrProviderCodes",
    "requests", "non200", "non200Codes", "lastStatus", "statusSequence", "proxyTags", "proxyFinish",
    "proxyErrs", "promptTokens", "cachedTokens", "cacheHitRate", "completionTokens", "maxPrompt",
    "maxCompletion", "upstreamSec", "harborInputTokens", "harborCacheTokens", "harborOutputTokens",
    "agentUsageRequests", "coreDB", "steps", "toolCalls", "toolsByName", "toolNonZero",
    "sessionStatus", "finishReasons",
    "ctrfPresent", "ctrfTool", "testsTotal", "testsPassed", "testsFailed", "testsSkipped",
    "testsPending", "testsOther", "tests",
    "verifierNetworkKinds", "verifierNetworkEvents", "verifierToolMissing",
    "classification", "classReason", "gatewayRunEnding", "gatewayEvents", "noTests",
    "networkBootstrap", "truncation", "protocolAnomaly", "agentTimeout", "artifactCollectionError",
    "envExclusion", "envExclusionReason", "rewardMissing", "comparisonEligible",
]


def csv_value(value):
    if isinstance(value, (dict, list)):
        return json.dumps(value, sort_keys=True, ensure_ascii=False)
    if value is None:
        return ""
    return value


def write_csv(path, rows, fields):
    with open(path, "w", newline="") as fh:
        writer = csv.DictWriter(fh, fieldnames=fields, extrasaction="ignore")
        writer.writeheader()
        for row in rows:
            writer.writerow({f: csv_value(row.get(f)) for f in fields})


def fmt_ratio(solved, attempts):
    return f"{solved}/{attempts}" if attempts is not None else "—"


def _mapping_check(ledger):
    """Explicit statement when the rerun supply does not match the required replacement slots."""
    summary = ledger["summary"]
    required, observed = summary["slotsRequired"], summary["rerunsObserved"]
    open_tasks = [c["task"] for c in ledger["counts"] if c["slotsRequired"] > c["filled"]]
    extra = len(ledger["unusedReruns"])
    if observed == required and not extra:
        return ("Mapping check: rerun supply matches the required replacement slots exactly "
                f"({observed}/{required}).")
    parts = [f"Mapping check: {observed} rerun trial(s) observed for {required} required replacement "
             f"slot(s)"]
    if required > summary["filled"]:
        parts.append(f"{required - summary['filled']} slot(s) still open on "
                     + ", ".join(f"`{t}`" for t in open_tasks))
    if extra:
        parts.append(f"{extra} rerun trial(s) extra, listed above and never substituted for a "
                     "different attempt")
    if observed < required:
        parts.append("reruns must be launched for the open tasks only, one attempt per slot, before "
                     "the ledger can be closed")
    return "; ".join(parts) + "."


def md_cell(value):
    if value is None:
        return ""
    return str(value).replace("|", "\\|").replace("\n", " ")


def md_table(header, rows):
    out = ["| " + " | ".join(header) + " |",
           "|" + "|".join("---" for _ in header) + "|"]
    for row in rows:
        out.append("| " + " | ".join(md_cell(c) for c in row) + " |")
    return "\n".join(out)


def write_report(path, ctx):
    trials = ctx["trials"]
    ledger = ctx["ledger"]
    metric_notes = """
Sources per trial, all read-only: Harbor `result.json` (reward, exception, timings, task checksum),
the adapter's `agent/{result.json,stderr.txt,exit.txt}` (whitelisted metadata only), the Core state
database under `agent/state` (tool names, step counts, finish reasons), `verifier/ctrf.json`,
`verifier/test-stdout.txt`, and the shared metering log `jobs/metering/proxy.jsonl` joined on the
exact tag suffix `<arm>/<trial dir>`. Per-trial token/tool/time figures are internal diagnostics:
the public baselines publish no per-attempt time, tool calls or trajectories, so they must not be
compared item by item.
"""
    sections = []
    sections.append("# W13 evidence export — 2026-10-27\n")
    sections.append("Machine-readable companion to `bench/HARBOR_REMEDIATION_REPORT_2026-10-27.md`. Generated by\n"
                    "`bench/export_w13_evidence.py`; offline reporting only, no Harbor run, retry or re-scoring is\n"
                    "triggered here. Raw rewards are exported as observed and are never rewritten.\n")
    sections.append("## 1. Commands\n\n```sh\n" + ctx["commands"] + "\n```\n")
    sections.append("## 2. Inputs and integrity\n\n" + md_table(
        ["input", "path", "sha256 / value"],
        [["metering proxy log", ctx["proxyPath"], ctx["proxySha"]],
         ["public baselines", ctx["baselinePath"], ctx["baselineSha"] or "(missing)"],
         ["preregistration", ctx["preregPath"], ctx["preregSha"] or "(missing)"],
         ["jobs scanned", ", ".join(ctx["jobs"]), ctx["jobSummary"]]]) + "\n")
    sections.append("## 3. Frozen classification as implemented\n")
    sections.append("""- `solved`: verifier reward `1.0`, including trials whose agent timed out (prereg §Metrics; raw
  reward keeps its authority).
- `INFRA`: an unresolved trial whose run was ended by a gateway non-200 (last request non-200, no
  successful request at all, or no request with a provider 5xx in agent stderr), **or** an
  unresolved trial with no CTRF/no tests whose verifier failed to bootstrap over the network
  (DNS, HTTP 5xx, TLS, download or connection failure in `verifier/test-stdout.txt`).
- `MODEL`: every other unresolved trial; the verifier ran against a live environment.
- Flags, never classes: `truncation` (`finishReason=length` or a 32,768-token completion),
  `protocolAnomaly` (Core/provider abort: "model returned neither tool calls nor final content"),
  `agentTimeout` (Harbor `AgentTimeoutError` or ≥1800 s agent execution),
  `artifactCollectionError` (timeout plus missing `exit.txt`), `envExclusion` (task whose oracle
  fails under emulation — `pytorch-model-cli`; excluded from the comparison per prereg §Rules),
  `rewardMissing` (no verifier reward recorded), `comparisonEligible`.
- INFRA trials are never re-scored. A rerun appears only as the single ledger replacement of an
  original INFRA slot; the original row keeps its raw reward and class.
""")
    sections.append("## 4. Batch state\n\n" + md_table(
        ["job", "kind", "trials observed", "scored", "finished_at", "Harbor stats"],
        ctx["jobRows"]) + "\n")
    sections.append("## 5. Classification summary\n\n" + md_table(
        ["job", "solved", "INFRA", "MODEL", "truncation", "protocol", "timeout", "env-excluded", "attempts"],
        ctx["classRows"]) + "\n")

    sections.append("## 6. Per-trial evidence (original batches)\n")
    for job in ctx["jobs"]:
        rows = [t for t in trials if t["job"] == job]
        if not rows:
            continue
        table = []
        for t in sorted(rows, key=lambda x: (x["task"], x["trial"])):
            flags = ",".join(k for k in ("truncation", "protocolAnomaly", "agentTimeout",
                                         "artifactCollectionError", "envExclusion", "gatewayEvents")
                             if t.get(k))
            tests = f"{t['testsPassed']}/{t['testsFailed']}" if t["ctrfPresent"] else "no ctrf"
            table.append([t["task"], t["trial"], t["reward"], t["classification"], tests,
                          f"{t['requests']}/{t['non200']}",
                          f"{t['promptTokens']}/{t['cachedTokens']}/{t['completionTokens']}",
                          t["toolCalls"], t["steps"], t["agentExecSec"], flags, t["classReason"]])
        sections.append(f"### {job}\n\n" + md_table(
            ["task", "trial", "reward", "class", "tests P/F", "req/non200",
             "prompt/cached/out tok", "tools", "steps", "agent s", "flags", "reason"],
            table) + "\n")

    if ctx["rerunRows"]:
        sections.append("## 7. Rerun batches (separate accounting)\n")
        for job in ctx["rerunJobs"]:
            rows = [t for t in trials if t["job"] == job]
            sections.append(f"### {job}\n\n" + md_table(
                ["task", "trial", "reward", "class", "tests P/F", "req/non200", "reason"],
                [[t["task"], t["trial"], t["reward"], t["classification"],
                  (f"{t['testsPassed']}/{t['testsFailed']}" if t["ctrfPresent"] else "no ctrf"),
                  f"{t['requests']}/{t['non200']}", t["classReason"]]
                 for t in sorted(rows, key=lambda t: (t["task"], t["trial"]))]) + "\n")

    sections.append("## 8. Rerun ledger — one replacement per original INFRA trial\n")
    sections.append(f"Status: **{ledger['summary']['status']}** — INFRA originals "
                    f"{ledger['summary']['infraOriginals']}, slots required "
                    f"{ledger['summary']['slotsRequired']}, filled {ledger['summary']['filled']}, "
                    f"ENV-excluded {ledger['summary']['envExcluded']}, reruns observed "
                    f"{ledger['summary']['rerunsObserved']}.\n")
    sections.append(md_table(
        ["slot", "original trial", "task", "orig reward", "orig class", "rerun trial", "rerun reward",
         "rerun class", "counted", "status", "note"],
        [[s["slot"], s["originalTrial"], s["task"], s["originalReward"], s["originalClass"],
          s["rerunTrial"] or "(slot open)", s["rerunReward"] if s["rerunTrial"] else "",
          s["rerunClass"] or "", "yes" if s["counted"] else "no", s["status"], s["note"]]
         for s in ledger["slots"]]) + "\n")
    sections.append("Task counts (one replacement per original INFRA, no extra attempts):\n\n" + md_table(
        ["task", "INFRA originals", "slots required", "reruns observed", "filled", "ENV-excluded"],
        [[c["task"], c["infraOriginals"], c["slotsRequired"], c["rerunsObserved"], c["filled"],
          c["excluded"]] for c in ledger["counts"]]) + "\n")
    if ledger["unusedReruns"]:
        sections.append("Rerun trials not used as a replacement (never dropped silently, never scored\n"
                        "in place of another attempt):\n\n" + md_table(
            ["job", "trial", "task", "reward", "class"],
            [[u["job"], u["trial"], u["task"], u["reward"], u["classification"]]
             for u in ledger["unusedReruns"]]) + "\n")
    sections.append(_mapping_check(ledger) + "\n")

    sections.append("## 9. Per task: original, rerun, ledger-replaced, public baselines\n")
    sections.append("Original and rerun batches are counted separately; the `ledger` column applies exactly the\n"
                    "replacement slots above (an INFRA trial is swapped for its single rerun, nothing else\n"
                    "changes). A `partial` suffix appears only for an unfinished original batch or an open\n"
                    "INFRA replacement slot; completed evidence has no suffix.\n")
    sections.append(md_table(
        ["task", "LC original", "LC rerun", "LC ledger", "Pi", "SpecPi", "OMP", "CC", "OpenCode", "DSH"],
        ctx["vsBaselineRows"]) + "\n")
    sections.append("Baselines are the public SpecPi widened slice (`bench/public/tb2-widened-matrix.csv`); they\n"
                    "are a historical comparison, not a same-environment A/B (see PUBLIC_BASELINES.md §5 and\n"
                    "the prereg's disclosed condition differences).\n")

    sections.append("## 10. Verifier network failures and agent stderr\n\n" + md_table(
        ["task", "trial", "class", "verifier kinds", "first evidence line"],
        [[t["task"], t["trial"], t["classification"],
          ",".join(t["verifierNetworkKinds"]) or "—",
          (t["verifierNetworkEvents"][0]["text"] if t["verifierNetworkEvents"] else "—")]
         for t in sorted(trials, key=lambda t: (t["task"], t["trial"]))
         if t["verifierNetworkKinds"]]) + "\n")
    sections.append("A solved trial can still show a recovered gateway or verifier retry here (for example\n"
                    "`hf-model-inference__DQbvfBw`, 503×3 then success); the class column keeps the outcome\n"
                    "authority.\n")
    stderr_rows = [[t["task"], t["trial"], t["agentStderrError"]]
                   for t in sorted(trials, key=lambda t: (t["task"], t["trial"]))
                   if t["agentStderrError"]]
    if stderr_rows:
        sections.append("Agent stderr (credentials redacted):\n\n" + md_table(
            ["task", "trial", "stderr"], stderr_rows) + "\n")

    sections.append("## 11. Boundaries and limits\n\n" + metric_notes + """
- No model reasoning, assistant message content or assistant final text is read into these files.
  `agent/result.json` is parsed only for `status`, `error` and `sessionId`; the presence of a
  `final` key is recorded as a boolean and its value is discarded, so assistant text cannot reach
  an output file. Plain-text DSML markup that leaks into a final message is therefore **not**
  detected here; that evidence stays in the metering response bodies addressed by `reqSha256`,
  outside this export.
- A batch without `finished_at` is reported as observed; per-task `attempts` count only trials that
  have a `result.json`, so an in-flight batch shows fewer than 3 attempts on its pending tasks.
- The metering log is shared across runs; joins use the exact trial-directory suffix, and the
  matched tags are exported per trial (`proxyTags`) so cross-batch attribution can be audited.
- `pytorch-model-cli` is ENV-excluded (oracle fails 3/3 under emulation); its raw trials keep their
  rewards and classes but no replacement is counted for it.
""")
    counter = 0
    rendered = []
    for line in "\n".join(sections).splitlines():
        match = re.match(r"^## (?:\d+\.\s*)?(.+)$", line)
        if match:
            counter += 1
            rendered.append(f"## {counter}. {match.group(1)}")
        else:
            rendered.append(line)
    Path(path).write_text("\n".join(rendered))


# --------------------------------------------------------------------------- main


def discover_jobs(jobs_dir, explicit):
    if explicit:
        return list(explicit)
    names = []
    for path in sorted(p for p in jobs_dir.iterdir() if p.is_dir()):
        if not JOB_PATTERN.match(path.name):
            continue
        has_job_result = (path / "result.json").is_file()
        has_trial = any((t / "result.json").is_file() for t in path.iterdir() if t.is_dir())
        if has_job_result or has_trial:
            names.append(path.name)
    return names


def job_meta(job_dir):
    path = job_dir / "result.json"
    meta = {"nTotal": None, "nCompleted": None, "nRunning": None, "nPending": None,
            "nErrored": None, "finishedAt": None}
    if not path.is_file():
        return meta
    try:
        data = json.loads(path.read_text())
    except ValueError:
        return meta
    stats = data.get("stats") or {}
    meta.update({
        "nTotal": data.get("n_total_trials"),
        "nCompleted": stats.get("n_completed_trials"),
        "nRunning": stats.get("n_running_trials"),
        "nPending": stats.get("n_pending_trials"),
        "nErrored": stats.get("n_errored_trials"),
        "finishedAt": data.get("finished_at"),
    })
    return meta


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--jobs-dir", type=Path, default=DEFAULT_JOBS_DIR)
    parser.add_argument("--out", type=Path, default=DEFAULT_OUT)
    parser.add_argument("--job", action="append", help="job name to scan (default: tb2-w13a, tb2-w13b, tb2-w13r*)")
    parser.add_argument("--baselines", type=Path, default=DEFAULT_BASELINES)
    parser.add_argument("--proxy-log", type=Path, default=None)
    parser.add_argument("-v", "--verbose", action="store_true")
    args = parser.parse_args(argv)

    if args.proxy_log:
        cw.PROXY_LOG = args.proxy_log
    proxy = cw.proxy_index()
    jobs = discover_jobs(args.jobs_dir, args.job)
    out = args.out
    out.mkdir(parents=True, exist_ok=True)

    trials, job_metas, pending = [], [], []
    for job in jobs:
        job_dir = args.jobs_dir / job
        kind = "original" if job in ORIGINAL_JOBS else "rerun"
        meta = job_meta(job_dir)
        job_metas.append({"job": job, "kind": kind, **meta})
        for trial_dir in sorted(p for p in job_dir.iterdir() if p.is_dir()):
            if (trial_dir / "result.json").is_file():
                trials.append(collect_trial(trial_dir, job, kind, proxy))
                if args.verbose:
                    print(trials[-1]["job"], trials[-1]["trial"], trials[-1]["classification"],
                          trials[-1]["reward"], file=sys.stderr)
            elif trial_dir.name not in ("agent", "verifier", "artifacts"):
                pending.append({"job": job, "trial": trial_dir.name, "state": "no result.json yet"})

    trials.sort(key=lambda t: (t["job"], t["task"], t["trial"]))
    originals = [t for t in trials if t["jobKind"] == "original"]
    reruns = [t for t in trials if t["jobKind"] == "rerun"]
    baseline = load_baselines(args.baselines)
    ledger = build_ledger(originals, reruns)
    final = ledger_final(originals, ledger, baseline)

    orig_by_task = {r["task"]: r for r in per_task(originals)}
    rerun_by_task = {r["task"]: r for r in per_task(reruns)}

    vs_rows = []
    for task in sorted(set(list(orig_by_task) + list(rerun_by_task) + list(baseline.get("pi", {})))):
        o = orig_by_task.get(task)
        r = rerun_by_task.get(task)
        f = final.get(task) or {"attempts": 0, "solved": 0, "complete": False}
        ledger_cell = fmt_ratio(f["solved"], f["attempts"])
        if f.get("pendingInfra"):
            ledger_cell += f" {f['pendingInfra']} pending"
        if f.get("envExcluded"):
            ledger_cell += f" {f['envExcluded']} env-excl"
        if not f.get("complete") and not (f.get("pendingInfra") or f.get("envExcluded")):
            ledger_cell += " partial"
        row = [task, fmt_ratio(o["solved"], o["attempts"]) if o else "—",
               fmt_ratio(r["solved"], r["attempts"]) if r else "—", ledger_cell]
        for harness in BASELINE_HARNESSES:
            cell = baseline.get(harness, {}).get(task)
            row.append(fmt_ratio(cell["solved"], cell["attempts"]) if cell else "—")
        vs_rows.append(row)

    # ---- CSVs
    write_csv(out / "trials.csv", trials, CSV_FIELDS)
    write_csv(out / "verifier_network_events.csv",
              [{"job": t["job"], "trial": t["trial"], "task": t["task"], "kind": e["kind"],
                "line": e["line"], "text": e["text"]}
               for t in trials for e in t["verifierNetworkEvents"]],
              ["job", "trial", "task", "kind", "line", "text"])
    pt_fields = ["task", "attempts", "observed", "solved", "rate", "solvedClass", "infra", "model",
                 "meanPromptTokens", "meanCompletionTokens", "meanRequests", "meanToolCalls",
                 "meanSteps", "meanAgentSec", "jobs", "trials"]
    write_csv(out / "per_task_original.csv", sorted(orig_by_task.values(), key=lambda r: r["task"]), pt_fields)
    write_csv(out / "per_task_rerun.csv", sorted(rerun_by_task.values(), key=lambda r: r["task"]), pt_fields)
    vs_fields = ["task", "lcOriginal", "lcRerun", "lcLedger"] + [BASELINE_LABEL[h] for h in BASELINE_HARNESSES]
    write_csv(out / "per_task_vs_baseline.csv",
              [dict(zip(vs_fields, row)) for row in vs_rows], vs_fields)
    write_csv(out / "ledger.csv", ledger["slots"],
              ["slot", "originalJob", "originalTrial", "task", "originalReward", "originalClass",
               "originalReason", "replacementRequired", "exclusion", "rerunJob", "rerunTrial",
               "rerunReward", "rerunClass", "counted", "finalReward", "status", "note"])

    commands = "\n".join([
        "# regenerate this evidence directory (offline; reads existing job dirs only)",
        "cd /Users/tangxingpeng/IdeaProjects/me/LumenCortex",
        "python3 bench/export_w13_evidence.py",
        "",
        "# explicit inputs / alternate job set",
        "python3 bench/export_w13_evidence.py \\",
        "  --jobs-dir bench/jobs --out bench/evidence/harbor-remediation-20261027 \\",
        "  --job tb2-w13a --job tb2-w13b --job tb2-w13r-<timestamp> \\",
        "  --baselines bench/public/tb2-widened-matrix.csv -v",
        "",
        "# unchanged per-trial collector this module reuses (metric definitions)",
        "python3 bench/collect_w13.py bench/jobs/tb2-w13a > bench/jobs/tb2-w13a/trials.jsonl",
    ])
    proxy_path = cw.PROXY_LOG
    ctx = {
        "trials": trials,
        "jobs": [j["job"] for j in job_metas],
        "rerunJobs": [j["job"] for j in job_metas if j["kind"] == "rerun"],
        "rerunRows": bool(reruns),
        "ledger": ledger,
        "commands": commands,
        "proxyPath": str(proxy_path.relative_to(BENCH.parent)) if str(proxy_path).startswith(str(BENCH.parent)) else str(proxy_path),
        "proxySha": sha256_file(proxy_path) if proxy_path.is_file() else "(missing)",
        "baselinePath": str(args.baselines.relative_to(BENCH.parent)),
        "baselineSha": sha256_file(args.baselines) if args.baselines.is_file() else "",
        "preregPath": "bench/PREREGISTRATION_TB2_W13.md",
        "preregSha": sha256_file(BENCH / "PREREGISTRATION_TB2_W13.md")
        if (BENCH / "PREREGISTRATION_TB2_W13.md").is_file() else "",
        "jobSummary": "; ".join(
            f"{j['job']}: total={j['nTotal']} completed={j['nCompleted']} running={j['nRunning']} "
            f"pending={j['nPending']} errored={j['nErrored']} finished_at={j['finishedAt']}"
            for j in job_metas),
        "jobRows": [[j["job"], j["kind"], sum(1 for t in trials if t["job"] == j["job"]),
                     sum(1 for t in trials if t["job"] == j["job"] and t["reward"] is not None),
                     j["finishedAt"] or "(running)",
                     f"total={j['nTotal']} completed={j['nCompleted']} running={j['nRunning']} "
                     f"pending={j['nPending']} errored={j['nErrored']}"] for j in job_metas],
        "classRows": [],
        "vsBaselineRows": vs_rows,
    }
    for j in job_metas:
        rows = [t for t in trials if t["job"] == j["job"]]
        ctx["classRows"].append([
            j["job"],
            sum(1 for t in rows if t["classification"] == "solved"),
            sum(1 for t in rows if t["classification"] == "INFRA"),
            sum(1 for t in rows if t["classification"] == "MODEL"),
            sum(1 for t in rows if t["truncation"]),
            sum(1 for t in rows if t["protocolAnomaly"]),
            sum(1 for t in rows if t["agentTimeout"]),
            sum(1 for t in rows if t["envExclusion"]),
            len(rows),
        ])

    payload = {
        "meta": {
            "generatedAt": datetime.now(timezone.utc).isoformat(timespec="seconds"),
            "utility": "bench/export_w13_evidence.py",
            "scope": "offline evidence export of existing Harbor job directories",
            "jobsDir": str(args.jobs_dir),
            "inputs": {"proxyLog": ctx["proxyPath"], "proxySha256": ctx["proxySha"],
                       "baselines": ctx["baselinePath"], "baselinesSha256": ctx["baselineSha"],
                       "preregistration": ctx["preregPath"], "preregistrationSha256": ctx["preregSha"]},
            "classification": "solved | INFRA | MODEL; flags truncation, protocolAnomaly, agentTimeout, "
                              "artifactCollectionError, envExclusion, rewardMissing",
            "envExcludedTasks": sorted(ENV_EXCLUDED_TASKS),
            "reasoningPolicy": "no reasoning_content, assistant content or final text is read",
            "commands": commands.splitlines(),
        },
        "jobs": job_metas,
        "pendingTrials": pending,
        "trials": trials,
        "perTask": {"original": sorted(orig_by_task.values(), key=lambda r: r["task"]),
                    "rerun": sorted(rerun_by_task.values(), key=lambda r: r["task"])},
        "ledger": ledger,
        "baseline": {h: dict(sorted(v.items())) for h, v in baseline.items()},
    }
    (out / "trials.json").write_text(json.dumps(payload, indent=1, sort_keys=False) + "\n")
    (out / "ledger.json").write_text(json.dumps(ledger, indent=1) + "\n")
    write_report(out / "report.md", ctx)

    manifest = []
    for name in ("trials.csv", "trials.json", "verifier_network_events.csv", "per_task_original.csv",
                 "per_task_rerun.csv", "per_task_vs_baseline.csv", "ledger.csv", "ledger.json",
                 "report.md"):
        p = out / name
        manifest.append({"file": name, "bytes": p.stat().st_size, "sha256": sha256_file(p)})
    (out / "manifest.json").write_text(json.dumps(
        {"generatedAt": payload["meta"]["generatedAt"], "files": manifest}, indent=1) + "\n")

    print(json.dumps({
        "out": str(out),
        "jobs": ctx["jobs"],
        "trials": len(trials),
        "solved": sum(1 for t in trials if t["classification"] == "solved"),
        "infra": sum(1 for t in trials if t["classification"] == "INFRA"),
        "model": sum(1 for t in trials if t["classification"] == "MODEL"),
        "ledger": ledger["summary"],
        "pending": len(pending),
    }, indent=1))
    return 0


if __name__ == "__main__":
    sys.exit(main())