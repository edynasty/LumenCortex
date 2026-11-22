"""Freeze exact selected inputs/outputs and the complete per-case wire digests without rerendering."""
import gzip, hashlib, io, pathlib, sys, tarfile

scratch, evidence=map(pathlib.Path,sys.argv[1:3])
vs=sys.argv[3:]
def put(tar,relative,data):
    info=tarfile.TarInfo(relative);info.size=len(data);info.mode=0o644
    tar.addfile(info,io.BytesIO(data))
name="-".join(vs)
with (evidence/(name+"-witnesses.tar.gz")).open("wb") as sink,gzip.GzipFile(filename="",fileobj=sink,mode="wb",mtime=0) as gz,tarfile.open(fileobj=gz,mode="w") as tar,(evidence/(name+"-wire.sha256")).open("w") as h:
    for v in vs:
        base=scratch/v
        for kind,inputs in [("corpus","in"),("matrix","matrix-in"),("sweep/out","sweep/in"),("extra","extra-in"),("witness","witness-in")]:
            paths=sorted((base/kind).glob("*.json"))
            for p in paths:
                b=p.read_bytes();payload=b[:-1] if kind=="corpus" else b
                print(hashlib.sha256(payload).hexdigest(),f"{v}/{kind}/{p.name}",file=h)
                if (kind in ("extra","witness") or p.stem in {"deep_f32_v00_u00_eio","J5_literal_ellipsis_shown","B_repeated_dirs_40_pressure","K_three_1400B_basenames","sweep_end_k070","sweep_end_k100","sweep_end_k240","sweep_start_k240"}):
                    put(tar,f"{v}/{kind}/{p.name}",b)
                    if v==vs[-1]:
                        put(tar,f"inputs/{inputs}/{p.name}",(base/inputs/p.name).read_bytes())
                        if kind=="corpus":put(tar,"inputs/in/"+p.stem+".meta",(base/"in"/(p.stem+".meta")).read_bytes())
        for f in sorted((base/"compact").glob("*.json")):put(tar,v+"/compact/"+f.name,f.read_bytes())
