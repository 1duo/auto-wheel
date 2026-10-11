"""Join verified first-day compact captures privately; no replay frames or fills."""
import argparse
import gzip
import hashlib
import importlib.metadata
import importlib.util
import json
import os
from pathlib import Path
import stat
import subprocess
import sys
import tempfile
from datetime import datetime,timezone
from batch_acquisition_proof import file_hash,read_json

BASE=Path('/home/yuduo/Projects/AutoWheel-research')
ROOT=Path(__file__).resolve().parents[1]

def module(name,filename):
    spec=importlib.util.spec_from_file_location(name,ROOT/'scripts'/filename)
    m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m);return m

def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary',type=Path,required=True)
    parser.add_argument('--output-directory',type=Path,required=True)
    args=parser.parse_args();output=args.output_directory
    adapter=module('adapter','audit-native-book-batch.py');producer=module('producer','produce-native-causal-observations.py')
    adapter.private_directory(BASE)
    if output.parent!=BASE or output.resolve()!=output or output.exists():raise ValueError('fresh output')
    if importlib.metadata.version('databento-dbn')!='0.71.0':raise ValueError('decoder')
    import databento_dbn as dbn
    if args.binary.resolve()!=args.binary or not args.binary.is_relative_to(BASE):raise ValueError('binary path')
    adapter.private_directory(args.binary.parent);info=args.binary.lstat()
    if not stat.S_ISREG(info.st_mode) or info.st_uid!=os.getuid() or info.st_nlink!=1 or stat.S_IMODE(info.st_mode)!=0o700:raise ValueError('binary')
    ledger=Path('/home/yuduo/.local/share/autowheel-research/databento-budget.json')
    cuts_path=BASE/'cycle31-causal-observations-tools-20261006/cuts.json';adapter.private_file(cuts_path)
    cuts_doc=read_json(cuts_path);relevant={cuts_path,ledger,args.binary,Path(__file__).resolve(),ROOT/'internal/capturejoin/join.go',ROOT/'research/simulation/2026-10-05/cycle33-capture-join-reference/main.go'}
    entries=[];fds=[];seen=set();expected_observations=0;cuts=None
    for name,audit_name in [('cmbp1-000','cycle28-native-cmbp1-000-20261006'),('cmbp1-001','cycle28-native-cmbp1-001-hardened-20261006'),('cmbp1-002','cycle28-native-cmbp1-002-hardened-20261006'),('stock-mbp1','cycle28-native-stock-mbp1-fixed-20261006')]:
        proof_path=BASE/'cycle28-acquired-proofs-20261006'/(name+'-proof.json')
        proof,manifest,start,end=adapter.acquired_source(proof_path,ledger,ROOT);query=proof['query']
        day_cuts,binding=producer.frozen_cuts(cuts_doc,query,start,end,ROOT)
        if cuts is not None and cuts!=day_cuts:raise ValueError('cuts')
        cuts=day_cuts;relevant.update(ROOT/p for p in binding)
        relevant.update(Path(p) if Path(p).is_absolute() else ROOT/p for p in manifest)
        audit_dir=BASE/audit_name;audit_path=audit_dir/'audit.json';native_ref=audit_dir/'reference.json'
        directory=BASE/('cycle31-causal-stock-observations-final-20261006' if name=='stock-mbp1' else 'cycle31-causal-'+name+'-observations-20261006')
        report_path=directory/'report.json';ref_path=directory/'reference.json';snapshot=directory/'snapshots.jsonl.gz'
        for p in [audit_path,native_ref,report_path,ref_path,snapshot]:adapter.private_file(p)
        audit=read_json(audit_path);ref=read_json(ref_path);report=read_json(report_path);member=audit['files'][0]
        if (ref['schema_version']!='AutoWheelNativeCausalObservationVerificationV1' or ref['decoder_version']!='0.71.0' or ref['upgrade_policy']!='AS_IS' or ref['producer_snapshots_match'] is not True or ref['producer_counters_match'] is not True or ref['retimed_rows']!=0 or ref['snapshots_compared']!=75 or ref['records_reconstructed']!=proof['native']['records'] or ref['producer_report_sha256']!=file_hash(report_path) or ref['acquisition_proof_sha256']!=file_hash(proof_path) or ref['audit_sha256']!=file_hash(audit_path) or ref['independent_native_reference_sha256']!=file_hash(native_ref) or ref['cuts_sha256']!=file_hash(cuts_path) or ref['reference_script_sha256']!=file_hash(ROOT/'scripts/verify-native-causal-observations.py') or report['snapshot_compressed_sha256']!=ref['snapshot_compressed_sha256'] or report['snapshot_jsonl_sha256']!=ref['snapshot_jsonl_sha256'] or ref['full_source_qualification'] is not False):raise ValueError('independent reference')
        for key,code in [('reducer_sha256','internal/causalbook/observe.go'),('producer_sha256','scripts/produce-native-causal-observations.py')]:
            if ref[key]!=file_hash(ROOT/code):raise ValueError('reference code')
            relevant.add(ROOT/code)
        if ref['binary_sha256']!=file_hash(BASE/'cycle31-causal-observations-tools-20261006/causal-observation-producer'):raise ValueError('source binary')
        source=Path(proof['source_directory'])/member['source_filename'];fd=os.open(source,os.O_RDONLY|os.O_NOFOLLOW|os.O_NONBLOCK)
        try:
            adapter.private_file(source)
            stream=subprocess.Popen(['/usr/bin/zstd','-q','-f','-d','-c','--','/dev/fd/'+str(fd)],pass_fds=(fd,),stdout=subprocess.PIPE,stderr=subprocess.DEVNULL)
            try:
                prefix=stream.stdout.read(8)
                if len(prefix)!=8 or prefix[:3]!=b'DBN' or prefix[3] not in (1,2,3):raise ValueError('header')
                length=int.from_bytes(prefix[4:],'little')
                if not 100<=length<=16*1024*1024-8:raise ValueError('header bound')
                header=prefix+stream.stdout.read(length)
                decoder=dbn.DBNDecoder(upgrade_policy=dbn.VersionUpgradePolicy.AS_IS,compression=dbn.Compression.NONE)
                decoded=decoder.write_and_decode(header)
                if len(header)!=length+8 or decoder.buffer() or len(decoded)!=1 or not isinstance(decoded[0],dbn.Metadata):raise ValueError('metadata')
                meta=decoded[0]
                if meta.dataset!=query['dataset'] or str(meta.schema)!=query['schema'] or meta.start!=start or meta.end!=end or meta.version!=member['native_version'] or not adapter.metadata_symbols_match(list(meta.symbols),query['symbols']) or str(meta.stype_in)!='raw_symbol' or str(meta.stype_out)!='instrument_id' or meta.limit not in (None,0):raise ValueError('metadata scope')
                _,mapping=adapter.active_mappings(meta,datetime.fromtimestamp(start//1000000000,timezone.utc).date(),set(query['symbols']))
                if hashlib.sha256(header).hexdigest()!=report['metadata_sha256']:raise ValueError('metadata hash')
            finally:
                # Header identity only; full native EOF/count/hash was qualified
                # by the separate acquisition and complete reference passes.
                stream.stdout.close();stream.kill();stream.wait()
        finally:os.close(fd)
        for instrument in mapping:
            identity=(query['dataset'],instrument)
            if identity in seen:raise ValueError('overlap')
            seen.add(identity)
        if ref['observations_compared']!=len(mapping)*75 or any(ref['query_'+k]!=query[k] for k in ['dataset','schema','start','end']):raise ValueError('universe')
        expected_observations+=ref['observations_compared']
        relevant.update([proof_path,audit_path,native_ref,report_path,ref_path,snapshot,source])
        entries.append(dict(scope=dict(Dataset=query['dataset'],UTCStart=start,Instruments=sorted(mapping)),schema=query['schema'],mappings=mapping,compressed_bytes=ref['snapshot_compressed_bytes'],compressed_sha256=ref['snapshot_compressed_sha256'],plain_sha256=ref['snapshot_jsonl_sha256'],path=snapshot))
    relevant.update([ROOT/'scripts/audit-native-book-batch.py',ROOT/'scripts/produce-native-causal-observations.py',ROOT/'scripts/verify-native-causal-observations.py',ROOT/'internal/nativebook/stream.go',BASE/'cycle31-causal-observations-tools-20261006/causal-observation-producer'])
    before={p:file_hash(p) for p in relevant};output.mkdir(mode=0o700);partial=output/'captures.jsonl.gz.partial';stage=output/'captures.jsonl.gz';committed=False
    try:
        for entry in entries:
            fd=os.open(entry.pop('path'),os.O_RDONLY|os.O_NOFOLLOW|os.O_NONBLOCK);fds.append(fd);entry['fd']=fd
            with os.fdopen(os.dup(fd),'rb') as f:
                info=os.fstat(fd)
                if not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode)!=0o600 or info.st_uid!=os.getuid() or info.st_nlink!=1 or info.st_size!=entry['compressed_bytes'] or hashlib.file_digest(f,'sha256').hexdigest()!=entry['compressed_sha256']:raise ValueError('snapshot fd')
                f.seek(0)
        executable=os.open(args.binary,os.O_RDONLY|os.O_NOFOLLOW|os.O_NONBLOCK);fds.append(executable)
        with os.fdopen(os.dup(executable),'rb') as f:
            info=os.fstat(executable)
            if not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode)!=0o700 or info.st_uid!=os.getuid() or info.st_nlink!=1 or hashlib.file_digest(f,'sha256').hexdigest()!=before[args.binary]:raise ValueError('binary fd')
        out_fd=os.open(partial,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600);fds.append(out_fd)
        with tempfile.TemporaryDirectory(prefix='join-descriptor-',dir=output) as temp:
            descriptor=Path(temp)/'expected.json';adapter.write_json(descriptor,dict(inputs=entries,cuts=cuts,max_age_nanos=cuts_doc['max_age_nanos']))
            p=subprocess.run(['/proc/self/fd/'+str(executable),str(descriptor),str(out_fd)],pass_fds=tuple(fds),stdout=subprocess.PIPE,stderr=subprocess.PIPE)
        if p.returncode or len(p.stdout)>16384:raise ValueError('complete join')
        result=json.loads(p.stdout)
        if result['schema']!='AutoWheelRawCaptureJoinV1' or result['capture_sets']!=75 or result['source_snapshots']!=300 or result['observations']!=expected_observations or result['full_source_qualification'] is not False:raise ValueError('join totals')
        # Independent structural comparison against every original compact row,
        # retaining BOOK/TRADE vectors, unsigned clocks, statuses and all columns.
        source_paths=[BASE/('cycle31-causal-stock-observations-final-20261006' if name=='stock-mbp1' else 'cycle31-causal-'+name+'-observations-20261006')/'snapshots.jsonl.gz' for name in ['cmbp1-000','cmbp1-001','cmbp1-002','stock-mbp1']]
        readers=[gzip.open(p,'rb') for p in source_paths];whole=hashlib.sha256()
        try:
            with gzip.open(partial,'rb') as joined:
                for cut in cuts:
                    line=joined.readline(16<<20);whole.update(line)
                    actual=json.loads(line);expected=[json.loads(r.readline(4<<20)) for r in readers]
                    if actual!=[cut,expected]:raise ValueError('independent capture rows')
                if joined.read(1) or any(r.read(1) for r in readers):raise ValueError('capture EOF')
        finally:
            for r in readers:r.close()
        if whole.hexdigest()!=result['joined_jsonl_sha256'] or any(file_hash(p)!=h for p,h in before.items()):raise ValueError('final stability')
        os.rename(partial,stage)
        result.update(observed_at=datetime.now(timezone.utc).isoformat(),independent_compact_rows_match=True,independent_comparison_observations=expected_observations,source_manifest=[dict(path=str(p),sha256=h) for p,h in before.items()],joined_compressed_sha256=file_hash(stage),joined_compressed_bytes=stage.stat().st_size,snapshot_file=stage.name)
        adapter.write_json(output/'report.json',result);directory=os.open(output,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW)
        try:os.fsync(directory)
        finally:os.close(directory)
        committed=True;print(json.dumps({k:v for k,v in result.items() if k not in ['source_manifest','snapshot_file']}))
    finally:
        for fd in fds:os.close(fd)
        if not committed:
            for p in [partial,stage,output/'report.json']:
                if p.exists():p.unlink()

if __name__=='__main__':
    try:main()
    except Exception:print('native capture join failed closed',file=sys.stderr);raise SystemExit(1)
