#!/usr/bin/env python3
"""Read-only Go overlays using the pinned worktree's original corpus source and case constructors."""
import argparse, hashlib, json, os, pathlib, subprocess, sys

p = argparse.ArgumentParser()
p.add_argument("scratch",type=pathlib.Path); p.add_argument("evidence_tree",type=pathlib.Path)
p.add_argument("worktree",type=pathlib.Path); p.add_argument("variant"); p.add_argument("sha")
p.add_argument("mode",choices=["render","oracle","bench","scale","witness","selected"])
a=p.parse_args(); tmp=a.scratch.resolve(); wt=a.worktree.resolve()
def git(*args): return subprocess.check_output(["git",*args],cwd=wt).decode().strip()
assert git("rev-parse","HEAD")==a.sha
probes=a.evidence_tree/"docs/reports/evidence/adr-0024-sol-rev-01/inputs/probes-v2"
out=tmp/a.variant; out.mkdir(exist_ok=True)
for child in ["in","workspace"]: (out/child).mkdir(exist_ok=True)
keys=["adv_cases","adv_common","sweep","corpus_helpers","corpus_dump","bench","boundary"]
if a.mode=="oracle": keys+=["adv_check","corpus_oracle"]
m={str(wt/"internal/toolset"/("zz_review_"+k+"_test.go")):str(probes/("zz_review_"+k+"_test.go.txt")) for k in keys}
m[str(wt/"internal/toolset/zz_carbon_audit_test.go")]=str(pathlib.Path(__file__).resolve().with_name("probe.go.txt"))
if a.mode in ("witness","selected"):
    leaf={"witness":"witness","selected":"compact"}[a.mode]
    m[str(wt/("internal/toolset/zz_astra_"+leaf+"_test.go"))]=str(pathlib.Path(__file__).resolve().with_name(leaf+".go.txt"))
overlay=out/("overlay-"+a.mode+".json"); overlay.write_text(json.dumps({"Replace":m}))
env=dict(os.environ, GOMAXPROCS="2", CARBON_CAPTURE=str(out), REVIEW_ADV_WS=str(out/"workspace"),
         WITNESS=str(out),
         REVIEW_INPUT_DIR=str(out/"in"),REVIEW_DUMP_DIR=str(out/"corpus"),REVIEW_SWEEP_OUT=str(out/"sweep"),
         REVIEW_VARIANT="out",REVIEW_ADV_DIR=str(out/"oracle"),REVIEW_CORPUS_OUT=str(out/"corpus"),
         REVIEW_CORPUS_IN=str(tmp/"candidate/in"))
base=["go","test","-overlay",str(overlay),"-timeout=30m"]
extra={"render":["-count=1","-v","-run","^(TestCarbonAuditCapture|TestReviewCorpusDump|TestReviewBasenameSweep|TestReviewBoundary)$"],
       "witness":["-count=1","-v","-run","^TestAstraWitness$"],
       "selected":["-count=1","-v","-run","^TestAstraSelectedLabels$"],
       "oracle":["-count=1","-v","-run","^(TestReviewAdversarialMatrix|TestReviewCorpusOracle)$"],
       "bench":["-count=3","-run","^$","-bench","^BenchmarkReviewCorpus$","-benchtime=1x","-benchmem"],
       "scale":["-count=2","-run","^$","-bench","^BenchmarkReviewScale$/(longname_middle_n(100|1000|50000)|longname_(end|start)_n1000)$","-benchtime=1x","-benchmem"]}[a.mode]
cmd=base+extra+["./internal/toolset"]
with (out/(a.mode+".log")).open("w") as f:
    print("HEAD",a.sha,"editing.go",git("rev-parse","HEAD:internal/toolset/editing.go"),"GOMAXPROCS=2",file=f)
    print(subprocess.list2cmdline(cmd),file=f,flush=True)
    status=subprocess.run(cmd,cwd=wt,env=env,stdout=f,stderr=subprocess.STDOUT).returncode
    print("exit",status,file=f)
print(a.variant,a.mode,status,flush=True)
sys.exit(status)
