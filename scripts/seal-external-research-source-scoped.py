#!/usr/bin/env python3
"""Seal one inactive download using the unchanged, pinned storage implementation."""
import argparse
import datetime
import fcntl
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import stat

import research_source_quiescence as guard

ORIGINAL_SHA = 'ac894a4ec6f3d3db9c8638150a02c8f1104072eac0b63954f0278dd709dbb578'


class ScopedVerifier:
    """Delegate every original check except the two seal quiescence calls."""
    def __init__(self, original, source):
        self.original, self.source = original, source

    def __getattr__(self, name):
        return getattr(self.original, name)

    def quiescent(self, path):
        if path != guard.VIEW:
            raise ValueError('original seal requested a different process scope')
        guard.quiescent(self.source)


def load_original():
    path = Path(__file__).with_name('seal-external-research-source-cycle74-v2.py')
    if path.resolve() != path or hashlib.sha256(path.read_bytes()).hexdigest() != ORIGINAL_SHA:
        raise ValueError('original source sealer differs')
    spec = importlib.util.spec_from_file_location('unchanged_external_source_sealer', path)
    original = importlib.util.module_from_spec(spec);spec.loader.exec_module(original)
    return original


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--source', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    logical, physical = guard.source_paths(args.source)
    original = load_original()
    original.lib = original.load(original.LIBRARY, 'scoped_seal_library', original.LIBRARY_SHA)
    verifier = original.load(original.VERIFIER, 'scoped_seal_verifier', original.VERIFIER_SHA)
    verifier.lib = original.lib
    original.verifier = ScopedVerifier(verifier, args.source)
    lib = original.lib
    if (not args.output.is_absolute() or not args.output.is_relative_to(original.BASE)
            or args.output.resolve() != args.output or args.output.exists()
            or args.output.with_name(args.output.name + '.intent.json').exists()
            or args.output.is_relative_to(logical)):
        raise ValueError('new private seal report outside download required')
    lib.directory(args.output.parent)
    lockfd = os.open(original.BASE / '.cycle74-bulk-storage-control.lock',
                    os.O_WRONLY | os.O_CREAT | os.O_NOFOLLOW, 0o600)
    try:
        info = os.fstat(lockfd)
        if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid()
                or info.st_nlink != 1 or stat.S_IMODE(info.st_mode) != 0o600):
            raise ValueError('unsafe storage control lock')
        fcntl.flock(lockfd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        original.verify_parent()
        if not original.SEALED.exists():
            original.SEALED.mkdir(mode=0o700);lib.sync_dir(original.SEALED.parent)
        lib.directory(original.SEALED)
        intent = original.seal(args.source, args.output)
        fs = os.statvfs(lib.DRIVE)
        report = dict(schema='AutoWheelScopedExternalRawSourceSealV1',
            observed_at=datetime.datetime.now(datetime.timezone.utc).isoformat(),
            phase='seal', source_directory=intent['source_directory'],
            physical_archive_directory=intent['physical_archive_directory'],
            files_verified=len(intent['files']), bytes_verified=intent['total_bytes'],
            source_bytes_preserved=True, final_source_path_preserved=True,
            final_path_established_before_native_proof=True, read_only=True,
            data_only_noexec=True, root_bulk_bytes_created=0, original_data_copies_deleted=0,
            journal_changed=False, financial_requests=0, source_qualified=False,
            original_source_sealer_sha256=ORIGINAL_SHA, original_source_guards_unchanged=True,
            process_scope=[str(logical), str(physical)],
            process_guard='selected-logical-and-physical-download',
            whole_parent_restore_guard_unchanged=True, free_bytes=fs.f_bavail * fs.f_frsize)
        lib.write_report(args.output, report);print(json.dumps(report))
    finally:
        os.close(lockfd)


if __name__ == '__main__':
    try:
        main()
    except Exception as error:
        import sys
        label = str(error) if isinstance(error, ValueError) else type(error).__name__
        print('scoped external source sealing stopped: ' + label, file=sys.stderr)
        raise SystemExit(1)
