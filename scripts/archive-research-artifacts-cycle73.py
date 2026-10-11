#!/usr/bin/env python3
"""Preserve inactive research trees on qualified Lexar storage and their read paths.

This research-only tool never reads credentials, changes financial reservations,
submits jobs, or changes a replay qualification gate. Its allowlist excludes live
staging, source code, binaries, runtime environments, and incomplete downloads.
"""
import argparse
import datetime
import fcntl
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import stat

BASE = Path('/home/yuduo/Projects/AutoWheel-research')
ROOT = BASE / 'source-afa3075'
HELPER = ROOT / 'scripts/archive-research-source.py'
HELPER_SHA = '0f2b362622373d0c404607c45870001c266a2c0cb471c9a2ff1dcd9cafab5106'
BINDER = BASE / 'cycle30-bindfs-runtime-20261006/runtime/usr/bin/bindfs'
RUN = BASE / 'cycle73-inactive-artifact-relocation-20261008'
LOWER = Path('/media/yuduo/Lexar1/AutoWheel-research-private-20261006/cycle73-artifacts-20261008')
Q = ROOT / 'research/simulation/2026-10-05/databento-qualification.json'
ALLOWLIST = (
    'cycle28-native-cmbp1-002-hardened-20261006',
    'cycle28-native-cmbp1-000-20261006',
    'cycle28-native-cmbp1-001-hardened-20261006',
    'cycle28-retrieve-Apr06-cmbp1-002-20261006',
    'cycle28-retrieve-Apr06-cmbp1-000-second-20261006',
    'cycle28-retrieve-Apr06-cmbp1-001-20261006',
    'cycle51-statistics-prior-scope-audit-20261007',
    'cycle53-full-statistics-OI-normalization-20261007',
    'cycle48-all-acquired-statistics-OI-audit-20261007',
    'cycle46-statistics-prefetch-20261007',
    'cycle48-OI-normalizer-equivalence-20261007',
    'cycle38-statistics-prefetch-final-20261007',
    'cycle46-OI-normalization-equivalence-20261007',
    'cycle45-full-statistics-OI-audit-20261007',
    'cycle51-statistics-next-scope-audit-20261007',
    'cycle43-full-statistics-OI-audit-20261007',
    'cycle41-acquired-statistics-audit-20261007',
    'cycle42-native-OI-equivalence-20261007',
    'full-all15-definitions-verified-20261006',
    'full-definition-identities-20261006',
    'cycle56-native-stock-audit-2026-04-07-20261007',
    'cycle56-native-stock-audit-2026-04-08-20261007',
    'cycle56-native-stock-audit-2026-04-09-20261007',
    'cycle56-native-stock-audit-2026-04-10-20261007',
    'cycle57-native-stock-audit-2026-04-13-20261007',
    'cycle57-native-stock-audit-2026-04-14-20261007',
    'cycle57-native-stock-audit-2026-04-15-20261007',
    'cycle28-native-stock-mbp1-fixed-20261006',
    'cycle70-native-stock-audit-2026-04-16-20261008',
)


def now():
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


def fingerprint(info):
    return [info.st_dev, info.st_ino, info.st_size, info.st_mtime_ns, info.st_ctime_ns]


def census(path, mode=0o600):
    """No links, mounts, special files, foreign ownership, or executable data."""
    lib.directory(path)
    files, directories = {}, []
    for current, dirs, names in os.walk(path, followlinks=False):
        parent = Path(current)
        lib.directory(parent)
        if parent != path and parent.is_mount():
            raise ValueError('nested mount prohibited')
        for name in sorted(dirs + names):
            if not lib.NAME.fullmatch(name):
                raise ValueError('unsafe member name')
            member = parent / name
            info = member.lstat()
            rel = str(member.relative_to(path))
            if stat.S_ISDIR(info.st_mode):
                lib.directory(member)
                directories.append(rel)
            elif (stat.S_ISREG(info.st_mode) and info.st_uid == os.getuid()
                  and info.st_nlink == 1 and stat.S_IMODE(info.st_mode) == mode
                  and 0 <= info.st_size <= 8 << 30 and member.resolve() == member):
                files[rel] = dict(bytes=info.st_size, fingerprint=fingerprint(info),
                                 allocated_bytes=info.st_blocks * 512)
            else:
                raise ValueError('unsafe artifact member')
            if len(files) + len(directories) > 100000:
                raise ValueError('artifact inventory exceeds bound')
    if not files:
        raise ValueError('empty artifact tree')
    return dict(files=dict(sorted(files.items())), directories=sorted(directories),
                bytes=sum(v['bytes'] for v in files.values()),
                allocated_bytes=sum(v['allocated_bytes'] for v in files.values()))


def quiescent(path):
    """Reject observable readers/writers, including cwd and mapped files."""
    for process in Path('/proc').iterdir():
        if not process.name.isdigit() or int(process.name) == os.getpid():
            continue
        try:
            if process.stat().st_uid != os.getuid():
                continue
            targets = []
            for name in ('cwd', 'root'):
                try:
                    targets.append(os.readlink(process / name))
                except FileNotFoundError:
                    pass
            for fd in (process / 'fd').iterdir():
                try:
                    targets.append(os.readlink(fd))
                except FileNotFoundError:
                    pass
            for line in (process / 'maps').read_text().splitlines():
                fields = line.split(maxsplit=5)
                if len(fields) == 6:
                    targets.append(fields[5])
            if any(t == str(path) or t.startswith(str(path) + '/') for t in targets):
                raise ValueError('artifact tree is in use')
        except (FileNotFoundError, ProcessLookupError):
            continue
        # Permission errors for a same-user process fail closed.


def original_unchanged(path, inventory):
    actual = census(path)
    if actual != inventory:
        raise ValueError('original artifact tree changed')


def verify_tree(path, manifest, mode):
    actual = census(path, mode)
    if (set(actual['files']) != set(manifest['files'])
            or actual['directories'] != manifest['directories']):
        raise ValueError('preserved tree roster differs')
    for rel, expected in manifest['files'].items():
        if lib.checked_hash(path / rel, mode) != (expected['sha256'], expected['bytes']):
            raise ValueError('preserved artifact hash differs')


def verify_mount(source, lower):
    lib.directory(source)
    records = [line.split() for line in Path('/proc/mounts').read_text().splitlines()]
    found = [v for v in records if v[1] == str(source)]
    if len(found) != 1:
        raise ValueError('archive mount missing')
    record = found[0]
    options = set(record[3].split(','))
    if (record[0] != str(lower) or record[2] != 'fuse' or 'allow_other' in options
            or not {'ro', 'nosuid', 'nodev', 'noexec', 'default_permissions',
                    'user_id=' + str(os.getuid())}.issubset(options)):
        raise ValueError('archive mount protections differ')


def free(path):
    v = os.statvfs(path)
    return v.f_bavail * v.f_frsize


def archive_one(name, inventory, known):
    source, lower = BASE / name, LOWER / name
    backup = BASE / (name + '.preserved-cycle73')
    if source.is_mount() or lower.exists() or backup.exists():
        raise ValueError('artifact relocation namespace already used')
    quiescent(source)
    original_unchanged(source, inventory)
    if free(lib.DRIVE) < inventory['bytes'] + (2 << 30):
        raise ValueError('qualified archive capacity insufficient')
    lower.mkdir(mode=0o700)
    for rel in sorted(inventory['directories'], key=lambda x: (x.count('/'), x)):
        (lower / rel).mkdir(mode=0o700)
    evidence = {}
    for rel, member in inventory['files'].items():
        fd = os.open(source / rel, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
        outfd = os.open(lower / rel, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
        digest, count = hashlib.sha256(), 0
        with os.fdopen(fd, 'rb') as original, os.fdopen(outfd, 'wb') as copied:
            before = os.fstat(original.fileno())
            if fingerprint(before) != member['fingerprint']:
                raise ValueError('copy source fingerprint differs')
            for block in iter(lambda: original.read(1 << 20), b''):
                count += len(block)
                if count > member['bytes']:
                    raise ValueError('copy source grew')
                digest.update(block)
                copied.write(block)
            copied.flush()
            os.fsync(copied.fileno())
            if fingerprint(os.fstat(original.fileno())) != member['fingerprint']:
                raise ValueError('copy source changed')
        hashed = digest.hexdigest()
        if count != member['bytes'] or (str(source / rel) in known and known[str(source / rel)] != hashed):
            raise ValueError('source differs from qualification binding')
        evidence[rel] = dict(bytes=count, sha256=hashed,
                             prior_qualification_binding=str(source / rel) in known)
    for rel in sorted(inventory['directories'], key=lambda x: x.count('/'), reverse=True):
        lib.sync_dir(lower / rel)
    lib.sync_dir(lower)
    lib.sync_dir(LOWER)
    manifest = dict(schema='AutoWheelInactiveArtifactRelocationIntentV1', source_directory=str(source),
                    physical_archive_directory=str(lower), preserved_original_directory=str(backup),
                    files=evidence, directories=inventory['directories'], total_bytes=inventory['bytes'],
                    binder_sha256=lib.BINDER_SHA256, archive_helper_sha256=HELPER_SHA,
                    created_at=now(), source_qualified=False, financial_requests=0)
    verify_tree(lower, manifest, 0o700)
    original_unchanged(source, inventory)
    quiescent(source)
    lib.write_report(RUN / (name + '.intent.json'), manifest)
    os.rename(source, backup)
    try:
        source.mkdir(mode=0o700)
        lib.sync_dir(BASE)
        lib.mount_view(BINDER, lower, source)
        verify_mount(source, lower)
        verify_tree(source, manifest, 0o600)
        verify_tree(backup, manifest, 0o600)
        quiescent(backup)
    except Exception:
        if source.is_mount():
            lib.unmount(source)
        source.rmdir()
        os.rename(backup, source)
        lib.sync_dir(BASE)
        raise
    lib.write_report(RUN / (name + '.verified.json'), dict(
        schema='AutoWheelInactiveArtifactVerifiedV1', verified_at=now(),
        source_directory=str(source), physical_archive_directory=str(lower),
        files_verified=len(evidence), bytes_verified=manifest['total_bytes'],
        original_paths_and_bytes_preserved=True, read_only=True,
        qualification_bound_files=sum(v['prior_qualification_binding'] for v in evidence.values()),
        source_qualified=False, financial_requests=0))
    # Release only the enumerated redundant root copy after three matching views.
    # The durable intent and verified receipt remain available for recovery.
    quiescent(backup)
    release = dict(source_directory=str(source), preserved_original_directory=str(backup),
                   total_bytes=manifest['total_bytes'], files=len(evidence), started_at=now())
    lib.write_report(RUN / (name + '.release-intent.json'), release)
    for rel in evidence:
        (backup / rel).unlink()
    for rel in sorted(manifest['directories'], key=lambda x: (x.count('/'), x), reverse=True):
        (backup / rel).rmdir()
    backup.rmdir()
    lib.sync_dir(BASE)
    result = dict(**release, complete=True, finished_at=now(), root_free_bytes=free(BASE),
                  archive_free_bytes=free(lib.DRIVE), original_backup_retained=False,
                  original_paths_and_bytes_preserved=True, financial_requests=0,
                  journal_changed=False, source_qualified=False)
    lib.write_report(RUN / (name + '.complete.json'), result)
    print(json.dumps(dict(artifact=name, complete=True, bytes_preserved=manifest['total_bytes'],
                          root_free_bytes=result['root_free_bytes'])), flush=True)
    return result


def restore(name):
    """Remount an existing sealed copy after reboot; never change its bytes."""
    intent_path = RUN / (name + '.intent.json')
    lib.checked_hash(intent_path, 0o600)
    intent = json.loads(intent_path.read_bytes())
    source, lower, backup = BASE / name, LOWER / name, BASE / (name + '.preserved-cycle73')
    if (intent['source_directory'] != str(source) or intent['physical_archive_directory'] != str(lower)
            or intent['preserved_original_directory'] != str(backup)
            or intent['binder_sha256'] != lib.BINDER_SHA256 or intent['archive_helper_sha256'] != HELPER_SHA):
        raise ValueError('restoration intent scope differs')
    if len(intent['files']) + len(intent['directories']) > 100000:
        raise ValueError('restoration inventory exceeds bound')
    for rel in list(intent['files']) + intent['directories']:
        if (Path(rel).is_absolute() or str(Path(rel)) != rel
                or any(not lib.NAME.fullmatch(part) for part in Path(rel).parts)):
            raise ValueError('unsafe restoration member')
    for entry in intent['files'].values():
        if (not isinstance(entry['bytes'], int) or not 0 <= entry['bytes'] <= 8 << 30
                or len(entry['sha256']) != 64 or any(c not in '0123456789abcdef' for c in entry['sha256'])):
            raise ValueError('invalid restoration binding')
    verify_tree(lower, intent, 0o700)
    if not source.exists() or not source.is_mount():
        if not source.exists():
            source.mkdir(mode=0o700)
        lib.directory(source)
        if list(source.iterdir()):
            raise ValueError('restoration source is not empty')
        lib.mount_view(BINDER, lower, source)
    verify_mount(source, lower)
    verify_tree(source, intent, 0o600)
    print(json.dumps(dict(restored=name, files_verified=len(intent['files']),
                          original_paths_and_bytes_preserved=True, financial_requests=0)), flush=True)


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument('--execute', action='store_true')
    ap.add_argument('--restore', choices=ALLOWLIST)
    args = ap.parse_args()
    if hashlib.sha256(HELPER.read_bytes()).hexdigest() != HELPER_SHA:
        raise ValueError('approved archive library changed')
    global lib
    spec = importlib.util.spec_from_file_location('approved_archive', HELPER)
    lib = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(lib)
    for path in (BASE, ROOT, lib.DRIVE, lib.ARCHIVE):
        lib.directory(path)
    lib.verify_backing_mount()
    for path in (RUN, LOWER):
        if not path.exists():
            path.mkdir(mode=0o700)
            lib.sync_dir(path.parent)
        lib.directory(path)
    lockfd = os.open(RUN / '.relocation.lock', os.O_WRONLY | os.O_CREAT | os.O_NOFOLLOW, 0o600)
    fcntl.flock(lockfd, fcntl.LOCK_EX | fcntl.LOCK_NB)
    if args.restore:
        restore(args.restore)
        return
    qraw = Q.read_bytes()
    q = json.loads(qraw)
    known = {}
    for row in q['source_files']:
        if row['path'] in known and known[row['path']] != row['sha256']:
            raise ValueError('conflicting qualification binding')
        known[row['path']] = row['sha256']
    if len(q['gates']) != 9 or any(v['status'] != 'pending' for v in q['gates'].values()):
        raise ValueError('qualification state changed; review required')
    inventories = {}
    for name in ALLOWLIST:
        source = BASE / name
        if source.is_mount():
            raise ValueError('allowlisted tree already mounted')
        quiescent(source)
        inventories[name] = census(source)
    plan = dict(schema='AutoWheelInactiveArtifactRelocationPlanV1', observed_at=now(),
                qualification_sha256=hashlib.sha256(qraw).hexdigest(), source_file_count=len(q['source_files']),
                directories=inventories, root_free_bytes_before=free(BASE),
                archive_free_bytes_before=free(lib.DRIVE), execute=args.execute,
                logical_bytes=sum(v['bytes'] for v in inventories.values()),
                root_file_allocated_bytes=sum(v['allocated_bytes'] for v in inventories.values()),
                tradepulse_required_root_bytes=46240432128, preferred_reclaimed_bytes=33000000000,
                financial_requests=0, journal_changed=False, source_qualified=False)
    if free(lib.DRIVE) < plan['logical_bytes'] + (2 << 30):
        raise ValueError('whole relocation archive capacity insufficient')
    lib.write_report(RUN / ('execute-plan.json' if args.execute else 'dry-plan.json'), plan)
    print(json.dumps({k:plan[k] for k in ('execute','logical_bytes','root_file_allocated_bytes',
                                          'root_free_bytes_before','archive_free_bytes_before')}), flush=True)
    if args.execute:
        results = [archive_one(name, inventories[name], known) for name in ALLOWLIST]
        if Q.read_bytes() != qraw:
            raise ValueError('qualification bindings changed during relocation')
        lib.write_report(RUN / 'summary.json', dict(
            schema='AutoWheelInactiveArtifactRelocationSummaryV1', complete=True,
            finished_at=now(), artifacts=results, bytes_preserved=plan['logical_bytes'],
            root_free_bytes=free(BASE), archive_free_bytes=free(lib.DRIVE),
            tradepulse_required_root_bytes=46240432128,
            tradepulse_capacity_met=free(BASE) >= 46240432128,
            qualification_unchanged=True, journal_changed=False, financial_requests=0,
            source_qualified=False, historical_performance_available=False))
    os.close(lockfd)


if __name__ == '__main__':
    try:
        main()
    except Exception as error:
        import sys
        label = str(error) if isinstance(error, ValueError) else type(error).__name__
        print('inactive artifact relocation stopped: ' + label, file=sys.stderr, flush=True)
        raise SystemExit(1)
