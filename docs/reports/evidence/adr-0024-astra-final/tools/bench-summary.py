import pathlib, re, statistics, sys

root=pathlib.Path(sys.argv[1])
for v in sys.argv[2:]:
    rows=[l for l in (root/v/"bench.log").read_text().splitlines() if l.startswith("BenchmarkReviewCorpus-2")]
    metric=lambda unit:statistics.median(float(re.search(r"([\d.]+)\s+"+re.escape(unit),l)[1]) for l in rows)
    assert len(rows)==3
    print(v,"CPU ms/payload %.3f, MiB/payload %.3f, allocations/payload %.1f"%(metric("cpu-ns/payload")/1e6, metric("B/op")/(684*2**20),metric("allocs/op")/684))
    s=root/v/"scale.log"
    if not s.exists():continue
    vals={}
    for k,ns,n in re.findall(r"BenchmarkReviewScale/(\w+)-2\s+\d+\s+(\d+) ns/op\s+\d+ B/op\s+(\d+) allocs/op",s.read_text()):
        vals.setdefault(k,[]).append((int(ns)/1e6,int(n)))
    for k,pairs in vals.items(): print(v,k,"wall ms/render %.2f, allocs/render %.0f"%tuple(statistics.median(x) for x in zip(*pairs)))
