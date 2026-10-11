#!/usr/bin/env python3
"""Prepare and verify private data-only bulk storage; do not acquire any data."""
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import stat
import subprocess

BASE=Path('/home/yuduo/Projects/AutoWheel-research')
ROOT=BASE/'source-afa3075'
HELPER=ROOT/'scripts/archive-research-source.py'
HELPER_SHA='0f2b362622373d0c404607c45870001c266a2c0cb471c9a2ff1dcd9cafab5106'
BINDER=BASE/'cycle30-bindfs-runtime-20261006/runtime/usr/bin/bindfs'
VIEW=BASE/'cycle73-bulk-storage-20261008'
LOWER=Path('/media/yuduo/Lexar1/AutoWheel-research-private-20261006/cycle73-bulk-storage-20261008')
OUT=ROOT/'research/simulation/2026-10-05/cycle73-bulk-storage-preparation.json'

def main():
    assert hashlib.sha256(HELPER.read_bytes()).hexdigest()==HELPER_SHA
    spec=importlib.util.spec_from_file_location('approved_archive',HELPER)
    lib=importlib.util.module_from_spec(spec);spec.loader.exec_module(lib)
    for p in (BASE,ROOT,lib.DRIVE,lib.ARCHIVE,OUT.parent):lib.directory(p)
    lib.verify_backing_mount()
    if OUT.exists() or VIEW.exists() or LOWER.exists():
        raise ValueError('bulk storage namespace already exists; inspect before reuse')
    if lib.checked_hash(BINDER,0o755)[0]!=lib.BINDER_SHA256:
        raise ValueError('qualified binder differs')
    LOWER.mkdir(mode=0o700);VIEW.mkdir(mode=0o700)
    lib.sync_dir(LOWER.parent);lib.sync_dir(BASE)
    fd=os.open(BINDER,os.O_RDONLY|os.O_NOFOLLOW|os.O_NONBLOCK)
    with os.fdopen(fd,'rb') as executable:
        info=os.fstat(executable.fileno())
        if (not stat.S_ISREG(info.st_mode) or info.st_uid!=os.getuid() or info.st_nlink!=1
                or stat.S_IMODE(info.st_mode)!=0o755
                or hashlib.file_digest(executable,'sha256').hexdigest()!=lib.BINDER_SHA256):
            raise ValueError('qualified executable descriptor differs')
        subprocess.run(['/proc/self/fd/'+str(fd),'--no-allow-other','--perms=0600:u+D',
            '--realistic-permissions','--chmod-deny','--chown-deny','--chgrp-deny','-o',
            'nosuid,nodev,noexec,default_permissions,attr_timeout=0,entry_timeout=0,negative_timeout=0',
            str(LOWER),str(VIEW)],pass_fds=(fd,),check=True,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
    lib.directory(VIEW)
    records=[line.split() for line in Path('/proc/mounts').read_text().splitlines()]
    mount=next(v for v in records if v[1]==str(VIEW));options=set(mount[3].split(','))
    if (mount[0]!=str(LOWER) or mount[2]!='fuse' or 'allow_other' in options or 'ro' in options
            or not {'rw','nosuid','nodev','noexec','default_permissions','user_id='+str(os.getuid())}.issubset(options)):
        raise ValueError('private writable data mount differs')
    probe=VIEW/'storage-probe';probe.mkdir(mode=0o700);lib.directory(probe)
    path=probe/'original.json';raw=b'{"private-storage-probe":true}\n'
    f=os.open(path,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
    with os.fdopen(f,'wb') as output:output.write(raw);output.flush();os.fsync(output.fileno())
    expected=(hashlib.sha256(raw).hexdigest(),len(raw))
    if lib.checked_hash(path,0o600)!=expected or lib.checked_hash(LOWER/'storage-probe/original.json',0o700)!=expected:
        raise ValueError('private data write did not reach physical Lexar')
    replacement=probe/'renamed.json';os.rename(path,replacement);lib.sync_dir(probe)
    if lib.checked_hash(replacement,0o600)!=expected or not (LOWER/'storage-probe/renamed.json').is_file():
        raise ValueError('private rename did not reach Lexar')
    replacement.unlink();probe.rmdir();lib.sync_dir(LOWER);lib.sync_dir(VIEW)
    if list(VIEW.iterdir()) or list(LOWER.iterdir()):
        raise ValueError('storage probe cleanup differs')
    v=os.statvfs(VIEW);drive=os.statvfs(lib.DRIVE)
    if (v.f_frsize,v.f_blocks)!=(drive.f_frsize,drive.f_blocks):
        raise ValueError('bulk capacity is not physical Lexar capacity')
    report=dict(schema='AutoWheelBulkStoragePreparationV1',logical_data_parent=str(VIEW),
        physical_data_parent=str(LOWER),binder_sha256=lib.BINDER_SHA256,
        archive_helper_sha256=HELPER_SHA,private_owner_only=True,data_only_noexec=True,
        create_write_hash_rename_unlink_controls_passed=True,free_bytes=v.f_bavail*v.f_frsize,
        future_writers_must_use_logical_data_parent=True,acquisition_controller_routed=False,
        raw_source_sealing_adapter_pending=True,financial_requests=0,source_qualified=False,
        whole_window_capacity_verified=False,production_changed=False)
    lib.write_report(OUT,report);print(json.dumps(report))

if __name__=='__main__':
    try:main()
    except Exception as error:
        import sys
        print('bulk storage preparation stopped: '+(str(error) if isinstance(error,ValueError) else type(error).__name__),file=sys.stderr)
        raise SystemExit(1)
