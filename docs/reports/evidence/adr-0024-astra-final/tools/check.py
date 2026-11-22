#!/usr/bin/env python3
"""Compare actual Go payload bytes and resolve labels against the union of receipt and result paths."""
import argparse, collections, csv, functools, hashlib, json, pathlib, re
from collections import Counter

P=argparse.ArgumentParser(); P.add_argument("scratch",type=pathlib.Path);P.add_argument("evidence_tree",type=pathlib.Path)
P.add_argument("variants",nargs="+");args=P.parse_args()
root=args.scratch; evidence=args.evidence_tree/"docs/reports/evidence/adr-0024-sol-rev-01"
fail=set("external_modification_detected partially_modified publication_uncertain rollback_failed published_post_error failed_before_write unsupported".split())
def rank(r):
    return 0 if r["PublishState"] in fail or r["Error"] else (1 if r["Verified"] and r["Published"] and r["PublishState"]=="published_verified" else 2)
def display(s): return "<outside-workspace>/"+s.rsplit("/",1)[-1] if s.startswith("../") else s
def bytesgo(d):
    s=json.dumps(d,ensure_ascii=False,separators=(",",":"))
    for c in "<>&\u2028\u2029": s=s.replace(c,"\\u%04x"%ord(c))
    return s.encode()
def sha(b): return hashlib.sha256(b).hexdigest()
def framed(parts):
    h=hashlib.sha256()
    for n,b in sorted(parts): h.update(n.encode()+b"\0"+hashlib.sha256(b).digest())
    return h.hexdigest()

@functools.lru_cache(24000)
def rx(l,loose,typed):
    if typed and l.startswith("./"): return re.compile(re.escape(l[2:])+r"\Z",re.S)
    if typed and l.startswith("…/./"): return re.compile(r"(?:[^/]+/)+"+re.escape(l[4:])+r"\Z",re.S)
    if l.startswith("<outside-workspace>/") and typed: return re.compile(re.escape(l)+r"\Z",re.S)
    a=[]
    for i,c in enumerate(l.split("/")):
        last=i==l.count("/")
        n=re.fullmatch(r"…\{([1-9][0-9]{0,3})\}",c) if typed and i and not last else None
        if n:
            a.append("(?:[^/]+/){%d}"%int(n[1])); continue
        if not loose and c=="…" and not last: a.append("(?:[^/]+/)+"); continue
        a.append((".+" if loose else "[^/]+").join(map(re.escape,c.split("…"))))
        if not last:a.append("/")
    return re.compile("".join(a)+r"\Z",re.S)
@functools.lru_cache(45000)
def targets(l,raws,loose,typed):
    pattern=rx(l,loose,typed)
    return tuple(raw for raw in raws if pattern.match(display(raw)))
def compact_max(payload,receipts,labels):
    def make(k):
        d=dict(payload)
        rows=[dict(path=label,publishState=r["PublishState"],published=r["Published"],verified=r["Verified"],wrote=r["Wrote"]) for label,r in zip(labels[:k],receipts)]
        assert all(r["Wrote"] and r["Published"] and not r["Verified"] for r in receipts[:k])
        d["files"]=rows; om=Counter(r["PublishState"] for r in receipts[k:])
        for field in ["filesOmitted","filesOmittedByState"]: d.pop(field,None)
        if om: d.update(filesOmitted=sum(om.values()),filesOmittedByState=om)
        return len(bytesgo(d))
    sizes=[make(k) for k in range(1+len(labels))]
    k=max(i for i,n in enumerate(sizes) if n<=4096)
    return k,sizes[k],sizes[k+1:k+2]
def case_input(base,kind,n):
    if kind=="corpus":
        path=base/"in"/(n+".json");meta=json.loads((base/"in"/(n+".meta")).read_bytes())
        if not path.exists(): return [],[],meta
        inp=json.loads(path.read_bytes());return inp["receipt"]["Files"],inp["result"]["files"],meta
    inp=json.loads((base/({"sweep":"sweep/in"}.get(kind,kind+"-in"))/(n+".json")).read_bytes())
    return inp["receipts"],inp["results"],inp
def analyse(v,kind,name,b,rec,res,meta):
    body=json.loads(b.decode("utf-8")); assert len(b)<=4096 and not b.endswith(b"\n")
    states=Counter(r["PublishState"] for r in rec); files=body.get("files",[]);pub=body.get("publishedFiles",[])
    omissions=body.get("filesOmittedByState",{}); omitted=body.get("filesOmitted",0)
    assert len(rec)==len(files)+omitted and sum(omissions.values())==omitted
    assert Counter(f["publishState"] for f in files)+Counter(omissions)==states
    assert sum(f.get("published",False) for f in res)==len(pub)+body.get("publishedFilesOmitted",0)
    byraw={}; resmap={r["path"]:r for r in res}; seen=set()
    for r in sorted(rec,key=rank): byraw.setdefault(r["Path"],r)
    raws=list(dict.fromkeys([r["Path"] for r in rec]+list(resmap)))
    er=meta.get("editErr") or {}; errpath=er.get("Path",meta.get("path",""))
    if errpath and errpath not in raws: raws.append(errpath)
    raws=tuple(raws); typed=v=="astra"; ct=Counter()
    expected=[k for k in byraw if sum(display(q)==display(k) for q in raws)==1]
    if typed and body.get("overflow"):
        selected=json.loads((root/v/"selected"/kind/(name+".json")).read_bytes())
        env=dict(body);env["files"]=[]
        env.pop("filesOmitted",None);env.pop("filesOmittedByState",None)
        def row(raw):
            r=byraw[raw]
            out=dict(path=selected[raw],publishState=r["PublishState"],published=r["Published"],verified=r["Verified"],wrote=r["Wrote"])
            for k in ["Created","Deleted","RolledBack"]:
                if r[k]:out[k[0].lower()+k[1:]]=True
            return out
        floor=[k for k in expected if rank(byraw[k])==0] or expected[:1]
        minimum=len(bytesgo(env))
        expected=[k for k in floor if selected[k] and minimum+len(bytesgo(row(k)))<=4096]
        best=-1
        for k in range(len(expected)+1):
            prefix=expected[:k]; d=dict(env,files=[row(w) for w in prefix])
            om=states-Counter(byraw[w]["PublishState"] for w in prefix)
            if om:d.update(filesOmitted=len(rec)-k, filesOmittedByState=om)
            if len(bytesgo(d))<=4096:best=k
        assert best==len(files),(kind,name,"prefix",best,len(files))
        ct["maximalPrefix"]+=1
    for view,rows in [("file",files),("pub",pub)]:
        for i,f in enumerate(rows):
            l=f["path"]; a=targets(l,raws,False,typed); z=targets(l,raws,True,typed)
            ct[view+"ComponentNotOne"]+=len(a)!=1;ct[view+"LooseNotOne"]+=len(z)!=1
            ct[view+"Labels"]+=1; ct[view+"Duplicates"]+=sum(row["path"]==l for row in rows[:i])>0
            if v=="old" or not rec or len(a)!=1 or len(z)!=1:continue
            assert a==z,(v,name,l,a,z); raw=a[0]
            if view=="file":
                assert i<len(expected) and raw==expected[i],(v,name,i,l,raw,expected[i:i+1])
                r=byraw[raw]; assert f["publishState"]==r["PublishState"]
                for k in ["wrote","verified","published","created","deleted","rolledBack"]:
                    assert f.get(k,False)==r[k[0].upper()+k[1:]],(name,k)
                if "newRev" in f: assert r["Verified"] and f["newRev"]==resmap[raw]["newRev"]
                if "sha256" in f: assert r["Verified"] and f["sha256"]==r["NewSHA256"]
            else: assert resmap[raw].get("published",False) or name.startswith("J1_")
    if not rec and body.get("path"):
        assert errpath and targets(body["path"],raws,True,typed)==(errpath,)
    return (len(b),len(files),sum(f["publishState"] in fail for f in files),omitted,dict(ct),sha(b))

for kind,count in [("corpus",684),("matrix",1180),("sweep",33),("extra",8)]:
    directory={"sweep":"sweep/out"}.get(kind,kind); data={}; anchor=None; inputhash=None
    table={}
    if kind!="extra":
        with (evidence/"tables"/(kind+"-per-case.tsv")).open() as h:table={r["case"]:r for r in csv.DictReader(h,delimiter="\t")}
    for v in args.variants:
        base=root/v; paths=sorted((base/directory).glob("*.json")); ids={p.stem for p in paths}
        assert len(ids)==count and (not table or set(table)==ids)
        if anchor is None:anchor=ids
        assert ids==anchor
        inpdir=base/({"corpus":"in","sweep":"sweep/in"}.get(kind,kind+"-in"))
        inputfiles=list(inpdir.glob("*")); digest=framed((f.name,f.read_bytes()) for f in inputfiles)
        if inputhash is None:inputhash=digest
        assert digest==inputhash and len(inputfiles)==(1350 if kind=="corpus" else count)
        stats={}; totals=Counter()
        for path in paths:
            name=path.stem; rec,res,meta=case_input(base,kind,name); raw=path.read_bytes()
            if kind=="corpus": assert raw[-1:]==b"\n";raw=raw[:-1]
            s=analyse(v,kind,name,raw,rec,res,meta); stats[name]=s; totals.update(s[4])
            key={"candidate":"new","old":"opus","sol":"sol"}.get(v)
            if table and key: assert tuple(int(table[name][key+k]) for k in ["_bytes","_rows","_failed","_omitted"])==s[:4],(kind,v,name,s[:4])
        data[v]=stats
        print(kind,v,"N",len(stats),"input-set",digest,"output-set",framed((k,bytes.fromhex(s[5])) for k,s in stats.items()),"maxB",max(s[0] for s in stats.values()),"omitted",sum(s[3] for s in stats.values()),dict(totals),flush=True)
    for a,b in zip(args.variants,args.variants[1:]):
        stats=Counter()
        for n,x in data[a].items():
            y=data[b][n];stats["identical"]+=x[5]==y[5]
            for k,i in [("Rows",1),("Failed",2)]:stats["more"+k]+=y[i]>x[i];stats["fewer"+k]+=y[i]<x[i]
            if x[2]!=y[2] and (kind!="corpus" or y[2]<x[2]):print("delta",kind,n,a,x[:4],b,y[:4])
        print("COMPARE",kind,a,b,dict(stats),flush=True)
    for n in (["sweep_end_k070","sweep_end_k100","sweep_end_k240","sweep_start_k240"] if kind=="sweep" else ["K_three_1400B_basenames","B_repeated_dirs_40_pressure","J5_literal_ellipsis_shown"] if kind=="matrix" else ["deep_f32_v00_u00_eio"] if kind=="corpus" else sorted(anchor)):
        print("SAMPLE",kind,n,[(v,data[v][n][:4],data[v][n][4]["fileLooseNotOne"]) for v in data])

for v in [v for v in args.variants if (root/v/"witness").is_dir()]:
    for n in ["B_repeated_dirs_40_pressure","escaped-joint","multiple-literal","expensive-literal","blocked-row"]:
        base=root/v; rec,res,case=case_input(base,"witness",n)
        raw=(base/"witness"/(n+".json")).read_bytes(); p=json.loads(raw)
        compact=json.loads((base/"compact"/(n+".json")).read_bytes())
        ans=analyse(v,"witness",n,raw,rec,res,case)
        if n in ("multiple-literal","expensive-literal","blocked-row"): print("LITERAL",v,n,[f["path"] for f in p["files"]],ans[:5]);continue
        names=tuple(r["Path"] for r in rec); num=sum(rank(r)==0 for r in rec)
        alt=[]
        for i,s in enumerate(names[:num]):
            options=[compact[i]]
            if n=="escaped-joint":
                name=s.split("/")[-1]; options.append("pkg/"+name[:13]+"…"+name[-4:])
            else:
                parts=s.split("/")
                for tail in (1,2):
                    gap=len(parts)-1-tail
                    cand="/".join([parts[0],f"…{{{gap}}}",*parts[-tail:]])
                    if all(targets(cand,names,m,True)==(s,) for m in (True,False)): options.append(cand)
            assert all(all(targets(q,names,m,True)==(s,) for m in (True,False)) for q in options)
            alt.append(min(options,key=lambda q:len(bytesgo(q))))
        print("WITNESS",v,n,"actual",ans[:4],"compact",compact_max(p,rec,compact[:num]),"alternative",compact_max(p,rec,alt),"compact path costs",list(map(lambda l:len(bytesgo(l)),compact[:4])),"alternative labels",alt[:4])
