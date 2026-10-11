#!/usr/bin/env python3
"""Seal or restore private external research data without copies or financial I/O."""
import argparse
import datetime
import fcntl
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import stat
import subprocess

BASE=Path('/home/yuduo/Projects/AutoWheel-research')
ROOT=BASE/'source-afa3075'
VIEW=BASE/'cycle73-bulk-storage-20261008'
PHYSICAL=Path('/media/yuduo/Lexar1/AutoWheel-research-private-20261006/cycle73-bulk-storage-20261008')
SEALED=PHYSICAL.parent/'cycle74-sealed-native-sources-20261008'
LIBRARY=ROOT/'scripts/archive-research-source.py'
LIBRARY_SHA='0f2b362622373d0c404607c45870001c266a2c0cb471c9a2ff1dcd9cafab5106'
VERIFIER=ROOT/'scripts/archive-research-artifacts-cycle73-v2.py'
VERIFIER_SHA='10eb572d5765a4c2b3edaac1f1ca6dbfcb5e26a6d2232d409ad1716e0b3e9a23'
BINDER=BASE/'cycle30-bindfs-runtime-20261006/runtime/usr/bin/bindfs'

def load(path,name,pin):
    if path.resolve()!=path or hashlib.sha256(path.read_bytes()).hexdigest()!=pin:
        raise ValueError('sealed storage library differs')
    spec=importlib.util.spec_from_file_location(name,path)
    value=importlib.util.module_from_spec(spec);spec.loader.exec_module(value)
    return value

def mount_record(path):
    found=[v.split() for v in Path('/proc/mounts').read_text().splitlines() if v.split()[1]==str(path)]
    return found[0] if len(found)==1 else None

def verify_parent():
    lib.verify_backing_mount()
    for path in (BASE,ROOT,lib.DRIVE,lib.ARCHIVE,PHYSICAL,VIEW):lib.directory(path)
    record=mount_record(VIEW)
    if record is None:
        raise ValueError('qualified external parent is not mounted')
    options=set(record[3].split(','))
    if (record[0]!=str(PHYSICAL) or record[2]!='fuse' or 'allow_other' in options or 'ro' in options
            or not {'rw','nosuid','nodev','noexec','default_permissions','user_id='+str(os.getuid())}.issubset(options)):
        raise ValueError('external parent protections differ')
    actual=os.statvfs(VIEW);drive=os.statvfs(lib.DRIVE)
    if (actual.f_blocks,actual.f_frsize)!=(drive.f_blocks,drive.f_frsize):
        raise ValueError('external capacity differs from physical Lexar')

def restore_parent():
    lib.verify_backing_mount()
    for path in (BASE,ROOT,lib.DRIVE,lib.ARCHIVE,PHYSICAL,VIEW):lib.directory(path)
    if not VIEW.is_mount():
        if list(VIEW.iterdir()):
            raise ValueError('unmounted external parent is not empty')
        verifier.quiescent(VIEW)
        fd=os.open(BINDER,os.O_RDONLY|os.O_NOFOLLOW|os.O_NONBLOCK)
        with os.fdopen(fd,'rb') as executable:
            info=os.fstat(fd)
            if (not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode)!=0o755
                    or info.st_uid!=os.getuid() or info.st_nlink!=1
                    or hashlib.file_digest(executable,'sha256').hexdigest()!=lib.BINDER_SHA256):
                raise ValueError('qualified binder descriptor differs')
            subprocess.run(['/proc/self/fd/'+str(fd),'--no-allow-other','--perms=0600:u+D',
                '--realistic-permissions','--chmod-deny','--chown-deny','--chgrp-deny','-o',
                'nosuid,nodev,noexec,default_permissions,attr_timeout=0,entry_timeout=0,negative_timeout=0',
                str(PHYSICAL),str(VIEW)],pass_fds=(fd,),check=True,
                stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
    verify_parent()

def source_scope(source):
    if (source.parent!=BASE or not source.name.startswith('cycle') or '-retrieve-' not in source.name
            or not lib.NAME.fullmatch(source.name) or source.resolve()!=source):
        raise ValueError('external raw source scope differs')
    return SEALED/source.name

def manifest_scope(intent):
    source=Path(intent['source_directory']);lower=source_scope(source)
    if (intent['schema']!='AutoWheelExternalRawSourceSealIntentV1'
            or intent['physical_archive_directory']!=str(lower)
            or intent['physical_staging_directory']!=str(PHYSICAL/source.name)
            or intent['download_directory']!=str(VIEW/source.name)
            or intent['binder_sha256']!=lib.BINDER_SHA256
            or intent['storage_library_sha256']!=LIBRARY_SHA
            or intent['storage_verifier_sha256']!=VERIFIER_SHA
            or intent['directories']!=[] or not 4<=len(intent['files'])<=4096
            or not {'receipt.json','job-source.json','files-source.json'}.issubset(intent['files'])):
        raise ValueError('external seal intent differs')
    total=0
    for name,member in intent['files'].items():
        if (not lib.NAME.fullmatch(name) or type(member['bytes']) is not int
                or not 0<=member['bytes']<=8<<30 or len(member['sha256'])!=64
                or any(c not in '0123456789abcdef' for c in member['sha256'])):
            raise ValueError('external seal member differs')
        total+=member['bytes']
    if total!=intent['total_bytes'] or total>64<<30:
        raise ValueError('external seal byte bound differs')
    return source,lower

def verify_seal(intent):
    source,lower=manifest_scope(intent)
    verifier.verify_mount(source,lower)
    verifier.verify_tree(source,intent,0o600)
    return source,lower

def seal(source,output):
    final=BASE/source.name
    lower=source_scope(final)
    if (source.parent!=VIEW or source.resolve()!=source or source.is_mount()
            or final.exists() or lower.exists()):
        raise ValueError('fresh external download and final source namespaces required')
    verifier.quiescent(VIEW)
    inventory=verifier.census(source)
    if (inventory['directories'] or not 4<=len(inventory['files'])<=4096
            or not {'receipt.json','job-source.json','files-source.json'}.issubset(inventory['files'])
            or inventory['bytes']>64<<30):
        raise ValueError('external raw source roster differs')
    receipt=json.loads((source/'receipt.json').read_bytes())
    if receipt.get('state')!='done' or receipt.get('all_verified') is not True:
        raise ValueError('unverified raw download cannot be sealed')
    files={}
    for name,entry in inventory['files'].items():
        digest,size=lib.checked_hash(source/name,0o600)
        if size!=entry['bytes'] or lib.checked_hash(PHYSICAL/source.name/name,0o700)!=(digest,size):
            raise ValueError('external raw physical/view bytes differ')
        files[name]=dict(sha256=digest,bytes=size)
    verifier.original_unchanged(source,inventory);verifier.quiescent(VIEW)
    intent=dict(schema='AutoWheelExternalRawSourceSealIntentV1',source_directory=str(final),download_directory=str(source),
        physical_archive_directory=str(lower),physical_staging_directory=str(PHYSICAL/source.name),
        files=files,directories=[],total_bytes=inventory['bytes'],binder_sha256=lib.BINDER_SHA256,
        storage_library_sha256=LIBRARY_SHA,storage_verifier_sha256=VERIFIER_SHA,
        source_qualified=False,financial_requests=0)
    lib.write_report(output.with_name(output.name+'.intent.json'),intent)
    os.rename(PHYSICAL/source.name,lower)
    lib.sync_dir(PHYSICAL);lib.sync_dir(SEALED)
    try:
        final.mkdir(mode=0o700);lib.sync_dir(BASE)
        lib.mount_view(BINDER,lower,final)
        verify_seal(intent)
    except Exception:
        if final.is_mount():lib.unmount(final)
        if final.exists():final.rmdir()
        os.rename(lower,PHYSICAL/source.name)
        lib.sync_dir(PHYSICAL);lib.sync_dir(SEALED)
        raise
    return intent

def restore(intent):
    source,lower=manifest_scope(intent)
    verifier.verify_tree(lower,intent,0o700)
    if not source.is_mount():
        if not source.exists():source.mkdir(mode=0o700)
        lib.directory(source)
        if list(source.iterdir()):raise ValueError('raw restoration mountpoint is not empty')
        lib.mount_view(BINDER,lower,source)
    verify_seal(intent)

def main():
    ap=argparse.ArgumentParser(description=__doc__)
    ap.add_argument('--phase',choices=('seal','verify','restore','restore-parent'),required=True)
    ap.add_argument('--source',type=Path)
    ap.add_argument('--intent',type=Path)
    ap.add_argument('--output',type=Path,required=True)
    args=ap.parse_args()
    global lib,verifier
    lib=load(LIBRARY,'sealed_archive_library',LIBRARY_SHA)
    verifier=load(VERIFIER,'sealed_storage_verifier',VERIFIER_SHA)
    verifier.lib=lib;verifier.ALLOWLIST=(VIEW.name,)
    if (not args.output.is_absolute() or not args.output.is_relative_to(BASE)
            or args.output.resolve()!=args.output or args.output.exists()):
        raise ValueError('new private report required')
    lib.directory(args.output.parent)
    lockfd=os.open(BASE/'.cycle74-bulk-storage-control.lock',os.O_WRONLY|os.O_CREAT|os.O_NOFOLLOW,0o600)
    info=os.fstat(lockfd)
    if (not stat.S_ISREG(info.st_mode) or info.st_uid!=os.getuid() or info.st_nlink!=1
            or stat.S_IMODE(info.st_mode)!=0o600):raise ValueError('unsafe storage control lock')
    fcntl.flock(lockfd,fcntl.LOCK_EX|fcntl.LOCK_NB)
    if args.phase=='restore-parent':
        if args.source is not None or args.intent is not None:raise ValueError('parent restoration takes no source')
        restore_parent();intent=None
    else:
        if args.phase=='seal':verify_parent()
        else:
            lib.verify_backing_mount()
            for path in (BASE,ROOT,lib.DRIVE,lib.ARCHIVE):lib.directory(path)
        if not SEALED.exists():SEALED.mkdir(mode=0o700);lib.sync_dir(SEALED.parent)
        lib.directory(SEALED)
        if args.phase=='seal':
            if args.source is None or args.intent is not None:raise ValueError('exact seal source required')
            intent=seal(args.source,args.output)
        else:
            if args.intent is None or args.source is not None:raise ValueError('exact restoration intent required')
            if not args.intent.is_relative_to(BASE) or args.intent.resolve()!=args.intent:
                raise ValueError('private seal intent required')
            lib.checked_hash(args.intent,0o600);intent=json.loads(args.intent.read_bytes())
            if args.phase=='restore':restore(intent)
            else:verify_seal(intent)
    fs=os.statvfs(lib.DRIVE)
    report=dict(schema='AutoWheelExternalRawSourceSealV1',phase=args.phase,
        observed_at=datetime.datetime.now(datetime.timezone.utc).isoformat(),
        source_directory=intent['source_directory'] if intent else None,
        physical_archive_directory=intent['physical_archive_directory'] if intent else str(PHYSICAL),
        files_verified=len(intent['files']) if intent else 0,bytes_verified=intent['total_bytes'] if intent else 0,
        source_bytes_preserved=True,final_source_path_preserved=True,
        final_path_established_before_native_proof=True,read_only=intent is not None,data_only_noexec=True,
        root_bulk_bytes_created=0,original_data_copies_deleted=0,journal_changed=False,
        source_guard_changed=False,financial_requests=0,source_qualified=False,free_bytes=fs.f_bavail*fs.f_frsize)
    lib.write_report(args.output,report);print(json.dumps(report));os.close(lockfd)

if __name__=='__main__':
    try:main()
    except Exception as error:
        import sys
        label=str(error) if isinstance(error,ValueError) else type(error).__name__
        print('external source sealing stopped: '+label,file=sys.stderr)
        raise SystemExit(1)
