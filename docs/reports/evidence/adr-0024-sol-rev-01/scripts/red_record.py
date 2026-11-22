#!/usr/bin/env python3
"""Per-case record for the five SOL-REV-01 cases (attempted, failed, retained, omissions, bytes, labels)."""
import json, re, sys
from pathlib import Path
FAILED = {"partially_modified", "published_post_error", "external_modification_detected", "publication_uncertain",
          "rollback_failed", "failed_before_write", "unsupported"}
CASES = [("sweep_end_k070", "sweep"), ("sweep_end_k100", "sweep"), ("sweep_end_k240", "sweep"),
         ("sweep_start_k240", "sweep"), ("K_three_1400B_basenames", "adv")]
def loose(label, paths):
    rx = re.compile("^" + ".+".join(re.escape(x) for x in label.split("…")) + "$", re.S)
    return [p for p in paths if p == label or ("…" in label and rx.match(p))]
def short(l): return l if len(l) <= 44 else f"{l[:18]}⟨{len(l.encode())}B⟩{l[-18:]}"
def record(base, variant, name, kind):
    c = json.loads((base / kind / "in" / f"{name}.json").read_text())
    raw = (base / kind / variant / f"{name}.json").read_bytes(); p = json.loads(raw)
    paths = [r["Path"] for r in c["receipts"]]; files = p.get("files", [])
    labels = [f["path"] for f in files]
    return dict(case=name, variant=variant, attempted=len(c["receipts"]),
                failed=sum(r["PublishState"] in FAILED or bool(r["Error"]) for r in c["receipts"]),
                retained=len(files), failed_retained=sum(f["publishState"] in FAILED for f in files),
                omitted=p.get("filesOmitted", 0), omitted_by_state=p.get("filesOmittedByState", {}),
                bytes=len(raw), duplicate_labels=len(labels) - len(set(labels)),
                unique_identities=sum(len(loose(l, paths)) == 1 for l in labels),
                overflow=p.get("overflow", False), error=p.get("error"), commitState=p.get("commitState"),
                labels=[short(l) for l in labels[:2]] + (["…"] if len(labels) > 3 else []) + ([short(l) for l in labels[-1:]] if len(labels) > 2 else []))
if __name__ == "__main__":
    bases = {v: Path(b) for v, b in (a.split("=", 1) for a in sys.argv[1:])}
    for name, kind in CASES:
        for v, b in bases.items():
            print(json.dumps(record(b, v, name, kind), ensure_ascii=False))
