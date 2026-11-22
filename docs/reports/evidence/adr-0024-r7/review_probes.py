#!/usr/bin/env python3
"""Replay Sol's independent scheduling probes using a transient Go overlay.

Usage from a checkout: python3 path/to/review_probes.py {r6,r7} SOURCE_DIR
SOURCE_DIR contains the original final_{gap_compare,json_probe,path_probe}_test.go
and (for R6 only) r6_engine_gap.go. No files in the checkout are written.
"""
import hashlib
import json
from pathlib import Path
import subprocess
import sys
import tempfile

mode, src = sys.argv[1:]
if mode not in ('r6', 'r7'):
    raise SystemExit('expected r6 or r7')
src = Path(src).resolve()
root = Path(subprocess.check_output(['git', 'rev-parse', '--show-toplevel'], text=True).strip()).resolve()
head = subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=root, text=True).strip()
if mode == 'r6' and head != '4aaf8e7168743d6c8ae8fa6fe87a350ca69f9af2':
    raise SystemExit('run r6 probes from the frozen R6 source')

def original(name, digest):
    file = src / name
    if not file.exists():
        file = src / (name + '.txt')
    if hashlib.sha256(file.read_bytes()).hexdigest() != digest:
        raise SystemExit('original probe mismatch: ' + name)
    return file

with tempfile.TemporaryDirectory(prefix='lc-r7-review-') as tmp:
    temp = Path(tmp)
    engine = root / 'internal/editing/engine.go'
    if mode == 'r6':
        source = original('r6_engine_gap.go', 'cf9a16e2ccfda1da23c711b8582541bf04a1aabb24e69f843003b096d0b4d077')
    else:
        # The pre-shrink instruction no longer exists. Schedule exactly the original child
        # after B fails, just before read-only observation of the previously verified A.
        anchor = '\t\tif p.verified {\n\t\t\tif err := e.verifyActual(host, p); err != nil {'
        text = engine.read_text()
        if text.count(anchor) != 1:
            raise SystemExit('read-only settlement anchor differs; review before porting the probe')
        hook = ('\t\tif firstErr != nil && p.verified && auditAfterRestoreBoundary != nil {\n'
                '\t\t\tif err := auditAfterRestoreBoundary(p.abs); err != nil { return dirty, err }\n\t\t}\n')
        source = temp / 'engine.go'
        source.write_text(text.replace(anchor, hook + anchor))
    cases = [
        ('gap', {engine: source, root / 'internal/editing/zz_final_gap_test.go': original('final_gap_compare_test.go', '9f747a0daee5a6e77ae2e277bf73befcf7f7470f4fe3958ee748512a022cdc5e')}, ['^TestAuditFinalGapCommit$', './internal/editing']),
        ('jsonpath', {root / 'internal/agent/zz_final_json_probe_test.go': original('final_json_probe_test.go', '0f9548358448fdd9dfc1f397106af3bd710b6231b88845fd203cf3d9d9395329'), root / 'internal/toolset/zz_final_path_probe_test.go': original('final_path_probe_test.go', '335ccfeba6ab7f5ac4eef236af7605f06a07da9a3387bc623775708bce30b3bb')}, ['^TestAuditFinal', './internal/agent', './internal/toolset']),
    ]
    if mode == 'r6' and sys.platform == 'darwin':
        # This original oracle passes when it observes a partial destination.
        fallback = original('lc-adr0024-r6-fallback-probe.go', '8b21ec00ea4fa175f179e7a4ef8ff9bbdd11e61f158160ec133694f250e064a2')
        cases.append(('fallback', {root / 'internal/editing/zz_fallback_probe_test.go': fallback}, ['^TestR6FallbackDeletedRestorePartialWriteProbe$', './internal/editing']))
    print('mode', mode, 'head', head, flush=True)
    failed = False
    for name, files, args in cases:
        overlay = temp / (name + '.json')
        overlay.write_text(json.dumps({'Replace': {str(k): str(v) for k, v in files.items()}}))
        cmd = ['go', 'test', '-count=1', '-race', '-v', '-overlay=' + str(overlay), '-run', *args]
        print(' '.join(cmd), flush=True)
        proc = subprocess.run(cmd, cwd=root, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
        print(proc.stdout, end='')
        print(name, 'exit', proc.returncode, flush=True)
        if mode == 'r7':
            failed |= proc.returncode != 0 or '--- PASS: TestAuditFinal' not in proc.stdout
        elif name == 'gap':
            failed |= proc.returncode != 1 or 'external append lost' not in proc.stdout
        elif name == 'fallback':
            failed |= proc.returncode != 0 or '--- PASS: TestR6FallbackDeletedRestorePartialWriteProbe' not in proc.stdout
        else:
            failed |= proc.returncode != 1 or not all(s in proc.stdout for s in ('8192-byte limit exceeded', 'outside directory tail leaked', 'new LSP error leaks absolute host path'))
    sys.exit(int(failed))
