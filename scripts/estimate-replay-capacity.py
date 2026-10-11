"""Free-only uncompressed billable-size metadata for frozen daily queries."""
import argparse
import base64
import concurrent.futures as cf
import datetime as dt
import hashlib
import http.client
import json
import os
import re
import ssl
import stat
import sys
import threading
import time
import urllib.parse
from pathlib import Path
from batch_acquisition_proof import canonical_query
BASE=Path("/home/yuduo/Projects/AutoWheel-research")
ROOT=BASE/"source-afa3075"
PLAN24=BASE/"cycle24-selective-acquisition-queries-20261006/manifest.json"
PLAN27=BASE/"cycle27-stock-daily-acquisition-queries-20261006/manifest.json"
SHA24="ccf48c229ce29db62e0d4aaa20cb06c733fdf4e528cf2d79f4da0e71a00adb2b"
SHA27="25056b1d6d41d3a97e9ffb7700cf157b3f195a1eff085b1bc8f2f5a4ba9b5333"
KEYF=Path("/home/yuduo/.config/autowheel/databento-pilot.key")
HOST="hist.databento.com"
ENDP="/v0/metadata.get_billable_size"
MAXB=4096
LIM=4<<20
PAIRS={("OPRA.PILLAR","statistics"),("OPRA.PILLAR","cmbp-1"),("EQUS.MINI","mbp-1")}
NAMERE=re.compile(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,254}")
HASHRE=re.compile(r"[0-9a-f]{64}")
DAYRE=re.compile(r"\d{4}-\d{2}-\d{2}T00:00:00Z")
MAXI=(1<<63)-1
def uniq(pairs):
    o={}
    for k,v in pairs:
        if k in o: raise ValueError("duplicate key")
        o[k]=v
    return o
def priv_dir(p):
    p=Path(p)
    s=os.lstat(p)
    if not p.is_absolute() or p.resolve()!=p or not stat.S_ISDIR(s.st_mode) or s.st_uid!=os.getuid() or stat.S_IMODE(s.st_mode)!=0o700: raise ValueError("private dir")
def read_priv(p,lim):
    if not os.path.isabs(str(p)): raise ValueError("absolute path")
    if os.path.islink(p): raise ValueError("symlink")
    if Path(p).resolve()!=Path(p): raise ValueError("resolved path")
    fd=os.open(p,os.O_RDONLY|os.O_NOFOLLOW|os.O_NONBLOCK)
    try:
        s=os.fstat(fd)
        if not stat.S_ISREG(s.st_mode) or s.st_uid!=os.getuid() or s.st_nlink!=1 or stat.S_IMODE(s.st_mode)!=0o600 or s.st_size>lim: raise ValueError("private file")
        d=b""
        while True:
            c=os.read(fd,1<<20)
            if not c: break
            d+=c
            if len(d)>lim: raise ValueError("size")
        if len(d)!=s.st_size: raise ValueError("size change")
        a=os.fstat(fd)
        if (s.st_dev,s.st_ino,s.st_size,s.st_mtime_ns,s.st_ctime_ns)!=(a.st_dev,a.st_ino,a.st_size,a.st_mtime_ns,a.st_ctime_ns): raise ValueError("file changed")
        return d
    finally: os.close(fd)
def load_key():
    priv_dir(str(KEYF.parent))
    raw=read_priv(str(KEYF),128)
    key=raw.decode("ascii").rstrip("\r\n")
    if len(key)!=32 or not key.startswith("db-") or ":" in key or "\n" in key: raise ValueError("credential")
    for c in key[3:]:
        if not (c.isalnum() or c in "_-"): raise ValueError("credential")
    return key
def parse_day(s):
    if DAYRE.fullmatch(s) is None: raise ValueError("day")
    return dt.date.fromisoformat(s[:10])
def load_plan(path,sha,stock):
    path=Path(path)
    priv_dir(str(path.parent))
    raw=read_priv(str(path),LIM)
    if hashlib.sha256(raw).hexdigest()!=sha: raise ValueError("plan hash")
    m=json.loads(raw.decode(),object_pairs_hook=uniq)
    return raw,m
def main():
    ap=argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--output-directory",type=Path,required=True)
    ar=ap.parse_args()
    out=ar.output_directory
    if not out.is_absolute(): raise ValueError("output absolute")
    if str(out)!=os.path.normpath(str(out)): raise ValueError("output strict")
    if out.parent!=BASE or out.resolve()!=out: raise ValueError("output base")
    nm=out.name
    if not nm.startswith("cycle33") or NAMERE.fullmatch(nm) is None: raise ValueError("output name")
    if os.path.lexists(out): raise ValueError("output fresh")
    priv_dir(str(BASE))
    priv_dir(str(ROOT))
    if not out.parent.exists(): raise ValueError("output parent")
    priv_dir(str(out.parent))
    raw24,man24=load_plan(str(PLAN24),SHA24,False)
    raw27,man27=load_plan(str(PLAN27),SHA27,True)
    qrows=list(man24.get("queries",[]))+list(man27.get("queries",[]))
    if len(man24.get("queries",[]))!=1086 or len(man27.get("queries",[]))!=126 or len(qrows)!=1212: raise ValueError("count")
    infos=[];seen=set();dates=set();counts={"statistics":0,"cmbp-1":0,"mbp-1":0};before={}
    before[str(PLAN24)]=hashlib.sha256(raw24).hexdigest()
    before[str(PLAN27)]=hashlib.sha256(raw27).hexdigest()
    for row,plan in [(r,0) for r in man24["queries"]]+[(r,1) for r in man27["queries"]]:
        if stock:=plan==1:
            if set(row)!={"filename","sha256","bytes","date","symbols"}: raise ValueError("row keys")
        elif set(row)!= {"filename","sha256","bytes","date","symbols","schema"}: raise ValueError("row keys")
        fn,hs,nb,dat,ct=row["filename"],row["sha256"],row["bytes"],row["date"],row["symbols"]
        if NAMERE.fullmatch(fn) is None or not fn.endswith(".json"): raise ValueError("row file")
        if HASHRE.fullmatch(hs) is None or type(nb) is not int or type(ct) is not int: raise ValueError("row meta")
        if not re.fullmatch(r"\d{4}-\d{2}-\d{2}",dat): raise ValueError("row date")
        base=PLAN24.parent if plan==0 else PLAN27.parent
        fp=base/fn
        raw=read_priv(str(fp),LIM)
        if len(raw)!=nb or hashlib.sha256(raw).hexdigest()!=hs: raise ValueError("row binding")
        q=json.loads(raw.decode(),object_pairs_hook=uniq)
        if set(q)!={"dataset","schema","symbols","stype_in","start","end"}: raise ValueError("query keys")
        if q["stype_in"]!="raw_symbol" or (q["dataset"],q["schema"]) not in PAIRS: raise ValueError("scope")
        if stock and q["schema"]!="mbp-1": raise ValueError("stock schema")
        if not stock and row["schema"]!=q["schema"]: raise ValueError("schema binding")
        sd,ed=parse_day(q["start"]),parse_day(q["end"])
        if (ed-sd).days!=1 or sd.isoformat()!=dat: raise ValueError("day binding")
        if not (dt.date(2026,4,6)<=sd and ed<=dt.date(2026,10,3)): raise ValueError("range")
        if len(q["symbols"])!=ct: raise ValueError("count binding")
        canon=canonical_query(q)
        ident=hashlib.sha256(canon).hexdigest()
        if ident in seen: raise ValueError("duplicate")
        seen.add(ident)
        dates.add(dat)
        counts[q["schema"]]=counts.get(q["schema"],0)+1
        before[str(fp)]=hs
        infos.append({"filename":fn,"date":dat,"dataset":q["dataset"],"schema":q["schema"],"query_sha256":hs,"symbol_count":ct,"query":q,"ident":ident})
    if len(infos)!=1212 or len(dates)!=126 or counts!= {"statistics":543,"cmbp-1":543,"mbp-1":126}: raise ValueError("coverage")
    code_files=[Path(__file__).resolve(),ROOT/'scripts/batch_acquisition_proof.py']
    code_before={str(p):hashlib.sha256(p.read_bytes()).hexdigest() for p in code_files}
    key=load_key()
    os.mkdir(out,0o700)
    rowsf=out/"rows.jsonl"
    rfd=os.open(rowsf,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
    directory_fd=os.open(out,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW)
    try: os.fsync(directory_fd)
    finally: os.close(directory_fd)
    ctx=ssl.create_default_context()
    auth="Basic "+base64.b64encode((key+":").encode()).decode()
    del key
    lock=threading.Lock();last=[0.0]
    def fetch(it):
        with lock:
            n=time.monotonic();w=0.1-(n-last[0])
            if w>0: time.sleep(w)
            last[0]=time.monotonic()
        base={"filename":it["filename"],"date":it["date"],"dataset":it["dataset"],"schema":it["schema"],"query_sha256":it["query_sha256"],"symbol_count":it["symbol_count"]}
        try:
            form=urllib.parse.urlencode({"dataset":it["query"]["dataset"],"schema":it["query"]["schema"],"stype_in":it["query"]["stype_in"],"start":it["query"]["start"],"end":it["query"]["end"],"symbols":",".join(it["query"]["symbols"])}).encode()
            cn=http.client.HTTPSConnection(HOST,443,context=ctx,timeout=60)
            try:
                cn.request("POST",ENDP,body=form,headers={"Authorization":auth,"Content-Type":"application/x-www-form-urlencoded","Accept":"application/json"})
                rp=cn.getresponse();st=rp.status;body=rp.read(MAXB+1)
            finally: cn.close()
        except Exception: return {**base,"error_class":"transport","http_status":None,"response_sha256":None,"response_bytes":0}
        n=len(body);response_complete=n<=MAXB
        response={"http_status":st,"response_sha256":hashlib.sha256(body).hexdigest() if response_complete else None,"response_bytes":n,"response_complete":response_complete}
        if st!=200: return {**base,"error_class":"http",**response}
        try:
            if len(body)>MAXB: raise ValueError("bound")
            v=json.loads(body.decode(),object_pairs_hook=uniq) if isinstance(body,(bytes,bytearray)) else None
            if type(v) is not int or not 0<=v<=MAXI: raise ValueError("int")
        except Exception: return {**base,"error_class":"response",**response}
        return {**base,**response,"uncompressed_bytes":v}
    done=[]
    with os.fdopen(rfd,"wb") as sink, cf.ThreadPoolExecutor(max_workers=3) as ex:
        futs=[ex.submit(fetch,it) for it in infos]
        for f in cf.as_completed(futs):
            r=f.result();done.append(r)
            line=(json.dumps(r,separators=(",",":"))+ "\n").encode()
            sink.write(line);sink.flush();os.fsync(sink.fileno())
    ok=[r for r in done if "uncompressed_bytes" in r];bad=[r for r in done if "uncompressed_bytes" not in r]
    unchanged=True
    try:
        for p,h in before.items():
            if hashlib.sha256(read_priv(p,LIM)).hexdigest()!=h: unchanged=False
        if any(hashlib.sha256(Path(p).read_bytes()).hexdigest()!=h for p,h in code_before.items()): unchanged=False
    except Exception: unchanged=False
    complete=len(ok)==1212 and not bad
    rep={"schema_version":"AutoWheelReplayCapacityEstimateV1","status":"complete" if complete else "partial","observed_at":dt.datetime.now(dt.timezone.utc).isoformat(),"script_sha256":code_before[str(Path(__file__).resolve())],"code_sha256":code_before,"plan_sha256":{"cycle24":SHA24,"cycle27":SHA27},"query_count":1212,"successful":len(ok),"failed":len(bad),"complete":complete,"source_unchanged":unchanged,"accepted":bool(unchanged and complete),"rows_sha256":hashlib.sha256(read_priv(rowsf,4<<20)).hexdigest(),"metadata_requests":len(done),"financial_requests":0,"compressed_capacity_verified":False,"source_qualified":False}
    if complete:
        tot={};total_bytes=0
        for s in ("statistics","cmbp-1","mbp-1"):
            t=sum(r["uncompressed_bytes"] for r in ok if r["schema"]==s);tot[s]=t;total_bytes+=t
        rep["totals_by_schema"]=tot;rep["total_uncompressed_bytes"]=total_bytes
    raw=(json.dumps(rep,indent=2,sort_keys=True)+"\n").encode()
    fd=os.open(out/"report.json",os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
    with os.fdopen(fd,"wb") as s: s.write(raw);s.flush();os.fsync(s.fileno())
    dfd=os.open(out,os.O_RDONLY)
    try: os.fsync(dfd)
    finally: os.close(dfd)
    safe={"successful":len(ok),"failed":len(bad),"complete":complete,"accepted":rep["accepted"],"observed_at":rep["observed_at"]}
    if complete: safe["total_uncompressed_bytes"]=rep["total_uncompressed_bytes"];safe["totals_by_schema"]=rep["totals_by_schema"]
    print(json.dumps(safe,separators=(",",":")))
if __name__=="__main__":
    try: main()
    except Exception: print("replay capacity estimate failed",file=sys.stderr);sys.exit(1)
