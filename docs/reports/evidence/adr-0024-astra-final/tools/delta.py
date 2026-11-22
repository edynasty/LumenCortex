"""Summarize the tested correction against the frozen Opus wire bytes; never generate inputs."""
from collections import Counter
import hashlib, json, pathlib, sys

root=pathlib.Path(sys.argv[1]); out=pathlib.Path(sys.argv[2])
failed=set("external_modification_detected partially_modified publication_uncertain rollback_failed published_post_error failed_before_write unsupported".split())
def read(v,group,n):
    b=(root/v/group/(n+".json")).read_bytes()
    if group=="corpus":b=b.removesuffix(b"\n")
    p=json.loads(b);fs=p.get("files",[])
    return hashlib.sha256(b).hexdigest(),len(b),len(fs),sum(f["publishState"] in failed for f in fs),p.get("filesOmitted",0),p.get("filesOmittedByState",{}),p.get("publishedFilesOmitted",0)
with out.open("w") as h:
    print("set","case","Opus:B/rows/failed/omitted/by-state/pub-omitted","Astra:B/rows/failed/omitted/by-state/pub-omitted",sep="\t",file=h)
    for group in ["corpus","matrix","sweep/out","extra","witness"]:
        a=root/"candidate"/group;b=root/"astra"/group; ns=sorted(p.stem for p in a.glob("*.json"))
        assert ns==sorted(p.stem for p in b.glob("*.json")); c=Counter()
        indir={"corpus":"in","matrix":"matrix-in","sweep/out":"sweep/in","extra":"extra-in","witness":"witness-in"}[group]
        aa=root/"candidate"/indir;bb=root/"astra"/indir
        assert sorted(p.name for p in aa.iterdir())==sorted(p.name for p in bb.iterdir())
        assert all((bb/p.name).read_bytes()==p.read_bytes() for p in aa.iterdir())
        for n in ns:
            old,new=read("candidate",group,n),read("astra",group,n)
            c["identical"]+=old[0]==new[0]
            for i,k in [(2,"Rows"),(3,"Failed")]:c["more"+k]+=new[i]>old[i];c["fewer"+k]+=new[i]<old[i]
            if old!=new:print(group,n,json.dumps(old[1:]),json.dumps(new[1:]),sep="\t",file=h)
        print(group,len(ns),c)
