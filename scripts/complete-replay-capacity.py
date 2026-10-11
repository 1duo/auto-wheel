"""Explicitly repeat only failed FREE size metadata reads; no purchase endpoint."""
import argparse
import base64
import datetime
import hashlib
import http.client
import importlib.util
import json
import os
from pathlib import Path
import ssl
import sys
import urllib.parse

spec=importlib.util.spec_from_file_location('capacity',Path(__file__).with_name('estimate-replay-capacity.py'))
m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)

def write(path,value):
    with os.fdopen(os.open(path,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600),'w') as f:
        json.dump(value,f,indent=2);f.write('\n');f.flush();os.fsync(f.fileno())

def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--previous-directory',type=Path,required=True)
    parser.add_argument('--output-directory',type=Path,required=True)
    args=parser.parse_args();old=args.previous_directory;out=args.output_directory
    for p in [old,out]:
        if p.parent!=m.BASE or p.resolve()!=p or not p.name.startswith('cycle33-') or not m.NAMERE.fullmatch(p.name):raise ValueError('path')
    m.priv_dir(m.BASE);m.priv_dir(old)
    if os.path.lexists(out):raise ValueError('fresh output')
    report_raw=m.read_priv(old/'report.json',m.LIM);rows_raw=m.read_priv(old/'rows.jsonl',m.LIM)
    report=json.loads(report_raw,object_pairs_hook=m.uniq)
    if report.get('schema_version')!='AutoWheelReplayCapacityEstimateV1' or not report.get('source_unchanged') or report.get('complete') or report.get('query_count')!=1212 or report.get('rows_sha256')!=hashlib.sha256(rows_raw).hexdigest():raise ValueError('previous evidence')
    code_before={p:hashlib.sha256(Path(p).read_bytes()).hexdigest() for p in report['code_sha256']}
    if code_before!=report['code_sha256']:raise ValueError('previous code')
    code_before[str(Path(__file__).resolve())]=hashlib.sha256(Path(__file__).read_bytes()).hexdigest()
    original=[json.loads(line,object_pairs_hook=m.uniq) for line in rows_raw.splitlines()]
    prior={x['query_sha256']:x for x in original}
    if len(original)!=1212 or len(prior)!=1212:raise ValueError('previous uniqueness')
    before={};requests=[];expected=set()
    for manifest,sha in [(m.PLAN24,m.SHA24),(m.PLAN27,m.SHA27)]:
        raw,plan=m.load_plan(manifest,sha,False);before[str(manifest)]=sha
        for row in plan['queries']:
            path=manifest.parent/row['filename'];raw=m.read_priv(path,m.LIM)
            if hashlib.sha256(raw).hexdigest()!=row['sha256']:raise ValueError('query hash')
            q=json.loads(raw,object_pairs_hook=m.uniq);m.canonical_query(q)
            if q['stype_in']!='raw_symbol' or (q['dataset'],q['schema']) not in m.PAIRS:raise ValueError('query scope')
            previous=prior[row['sha256']]
            identity={'filename':row['filename'],'date':row['date'],'dataset':q['dataset'],'schema':q['schema'],'query_sha256':row['sha256'],'symbol_count':row['symbols']}
            if any(previous.get(k)!=v for k,v in identity.items()):raise ValueError('cached query identity')
            expected.add(row['sha256']);before[str(path)]=row['sha256']
            if 'uncompressed_bytes' in previous:
                if (type(previous['uncompressed_bytes']) is not int or not 0<=previous['uncompressed_bytes']<=m.MAXI or previous.get('http_status')!=200 or previous.get('response_complete') is not True or not m.HASHRE.fullmatch(previous.get('response_sha256',''))):raise ValueError('cached response')
            elif previous.get('error_class') in ['http','transport','response']:requests.append((identity,q))
            else:raise ValueError('failure evidence')
    if expected!=set(prior) or not 1<=len(requests)<=32 or len(requests)!=report['failed']:raise ValueError('bounded missing set')
    key=m.load_key();auth='Basic '+base64.b64encode((key+':').encode()).decode();del key
    out.mkdir(mode=0o700);context=ssl.create_default_context();context.minimum_version=ssl.TLSVersion.TLSv1_2
    results=[]
    for identity,q in requests:
        body=None;status=None
        connection=http.client.HTTPSConnection('hist.databento.com',443,context=context,timeout=60)
        try:
            form=urllib.parse.urlencode({**{k:q[k] for k in ['dataset','schema','stype_in','start','end']},'symbols':','.join(q['symbols'])}).encode()
            connection.request('POST','/v0/metadata.get_billable_size',body=form,headers={'Authorization':auth,'Content-Type':'application/x-www-form-urlencoded','Accept':'application/json'})
            response=connection.getresponse();status=response.status;body=response.read(m.MAXB+1)
        except Exception:result={**identity,'error_class':'transport','http_status':None,'response_sha256':None,'response_bytes':0}
        else:
            result={**identity,'http_status':status,'response_bytes':len(body),'response_complete':len(body)<=m.MAXB,'response_sha256':hashlib.sha256(body).hexdigest() if len(body)<=m.MAXB else None}
            try:
                value=json.loads(body)
                if status!=200 or len(body)>m.MAXB or type(value) is not int or not 0<=value<=m.MAXI:raise ValueError('response')
            except Exception:result['error_class']='http' if status!=200 else 'response'
            else:result['uncompressed_bytes']=value
        finally:connection.close()
        results.append(result);write(out/('response-'+str(len(results))+'.json'),result)
    combined=dict(prior)
    for result in results:combined[result['query_sha256']]=result
    valid=[r for r in combined.values() if 'uncompressed_bytes' in r];complete=len(valid)==1212
    unchanged=all(hashlib.sha256(m.read_priv(p,m.LIM)).hexdigest()==h for p,h in before.items()) and all(hashlib.sha256(Path(p).read_bytes()).hexdigest()==h for p,h in code_before.items()) and m.read_priv(old/'report.json',m.LIM)==report_raw and m.read_priv(old/'rows.jsonl',m.LIM)==rows_raw
    rows=[combined[x['query_sha256']] for x in original];write(out/'combined-results.json',rows)
    final={'schema_version':'AutoWheelReplayCapacityCompletionV1','observed_at':datetime.datetime.now(datetime.timezone.utc).isoformat(),'previous_report_sha256':hashlib.sha256(report_raw).hexdigest(),'previous_rows_sha256':hashlib.sha256(rows_raw).hexdigest(),'code_sha256':code_before,'metadata_requests_this_pass':len(results),'previous_successes_reused':1212-len(requests),'successful':len(valid),'failed':1212-len(valid),'source_unchanged':unchanged,'complete':complete,'accepted':complete and unchanged,'financial_requests':0,'compressed_capacity_verified':False,'source_qualified':False,'combined_results_sha256':hashlib.sha256(m.read_priv(out/'combined-results.json',m.LIM)).hexdigest()}
    if complete:
        final['totals_by_schema']={s:sum(r['uncompressed_bytes'] for r in valid if r['schema']==s) for s in ['statistics','cmbp-1','mbp-1']}
        final['total_uncompressed_bytes']=sum(final['totals_by_schema'].values())
    write(out/'report.json',final)
    descriptor=os.open(out,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW)
    try:os.fsync(descriptor)
    finally:os.close(descriptor)
    print(json.dumps({k:v for k,v in final.items() if k in ['metadata_requests_this_pass','successful','failed','complete','accepted','total_uncompressed_bytes','totals_by_schema','financial_requests']}))

if __name__=='__main__':
    try:main()
    except Exception:print('capacity metadata completion failed closed',file=sys.stderr);sys.exit(1)
