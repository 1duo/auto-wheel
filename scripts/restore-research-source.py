#!/usr/bin/env python3
"""Restore a private read-only source view from a durable archive intent."""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import re
import stat


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--intent", type=Path, required=True)
    parser.add_argument("--binder", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--archive-only", action="store_true",
                        help="recover absent/empty view from verified external copy without requiring original backup")
    args = parser.parse_args()
    spec = importlib.util.spec_from_file_location("archive", Path(__file__).with_name("archive-research-source.py"))
    archive = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(archive)
    if (not args.intent.is_absolute() or not args.intent.is_relative_to(archive.BASE)
            or not args.output.is_absolute() or not args.output.is_relative_to(archive.BASE)
            or not args.binder.is_absolute() or not args.binder.is_relative_to(archive.BASE) or args.output.exists()
            or any(path.resolve() != path for path in (args.intent, args.output, args.binder))):
        raise ValueError("private research paths required")
    for path in (archive.BASE, args.intent.parent, args.output.parent, args.binder.parent):
        archive.directory(path)
    if archive.checked_hash(args.binder, 0o755)[0] != archive.BINDER_SHA256:
        raise ValueError("unqualified binder")
    fd = os.open(args.intent, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, "rb") as opened:
        info = os.fstat(fd)
        if (not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o600
                or info.st_uid != os.getuid() or info.st_nlink != 1 or info.st_size > 1 << 20):
            raise ValueError("private bounded intent required")
        intent = json.load(opened)
    source, lower, backup = (Path(intent[k]) for k in ("source_directory", "physical_archive_directory", "preserved_original_directory"))
    evidence = intent["files"]
    if (intent["schema"] != "AutoWheelResearchArchiveIntentV1" or intent["binder_sha256"] != archive.BINDER_SHA256
            or source.parent != archive.BASE or not archive.NAME.fullmatch(source.name) or "-retrieve-" not in source.name
            or lower != archive.ARCHIVE / source.name or backup != archive.BASE / (source.name + ".preserved-cycle30")
            or not isinstance(evidence, dict) or not 4 <= len(evidence) <= 4096):
        raise ValueError("archive intent binding differs")
    for name, item in evidence.items():
        if (not archive.NAME.fullmatch(name) or not isinstance(item, dict)
                or not isinstance(item.get("sha256"), str) or not re.fullmatch("[0-9a-f]{64}", item["sha256"])
                or type(item.get("bytes")) is not int or not 0 <= item["bytes"] <= 8 << 30):
            raise ValueError("archive intent member differs")
    if sum(item["bytes"] for item in evidence.values()) > 64 << 30:
        raise ValueError("archive intent capacity differs")
    for directory in (archive.DRIVE, archive.ARCHIVE, lower):
        archive.directory(directory)
    archive.verify_backing_mount()
    if {p.name for p in lower.iterdir()} != set(evidence):
        raise ValueError("backing archive roster differs")
    for name, expected in evidence.items():
        if archive.checked_hash(lower / name, 0o700) != (expected["sha256"], expected["bytes"]):
            raise ValueError("backing archive differs")
    # A live correct view is idempotent. A stale/disconnected view fails closed
    # and must be unmounted explicitly before recovery; never force an unmount.
    mounted = any(line.split()[1] == str(source) for line in Path("/proc/mounts").read_text().splitlines())
    if args.archive_only:
        # Archive-only recovery for released-backup acquisition: full V1
        # intent/member/copy/pinned-bindfs/source/drive checks above unchanged.
        # Missing backup allowed; partial backup left untouched; only an
        # absent/empty owner-700 target is recovered from the verified copy.
        if mounted:
            backup_present = backup.exists()
        else:
            if source.exists():
                archive.directory(source)
                if list(source.iterdir()):
                    raise ValueError("unmounted source directory must be empty")
            else:
                source.mkdir(mode=0o700)
                archive.sync_dir(archive.BASE)
            archive.directory(source)
            if list(source.iterdir()):
                raise ValueError("unmounted source directory must be empty")
            backup_present = backup.exists()
            archive.sync_dir(archive.BASE)
            archive.mount_view(args.binder, lower, source)
        archive.verify_view(source, lower, evidence)
        archive.write_report(args.output, dict(schema="AutoWheelResearchArchiveRestoreV1", source_directory=str(source),
                                              files_verified=len(evidence), bytes_verified=sum(x["bytes"] for x in evidence.values()),
                                              view_previously_mounted=mounted, archive_only=True,
                                              archive_only_recovery=not mounted, original_backup_present=backup_present,
                                              original_backup_verified=False,
                                              original_proof_paths_preserved=True, journal_changed=False, source_qualified=False))
        print(json.dumps(dict(restore_verified=True, files=len(evidence), archive_only=True,
                              archive_only_recovery=not mounted, original_backup_present=backup_present,
                              source_qualified=False)))
        return
    if not mounted:
        if not backup.exists():
            archive.directory(source)
            if {p.name for p in source.iterdir()} != set(evidence):
                raise ValueError("prepared original roster differs")
            for name, expected in evidence.items():
                if archive.checked_hash(source / name, 0o600) != (expected["sha256"], expected["bytes"]):
                    raise ValueError("prepared original differs")
            os.rename(source, backup)
        archive.directory(backup)
        for name, expected in evidence.items():
            if archive.checked_hash(backup / name, 0o600) != (expected["sha256"], expected["bytes"]):
                raise ValueError("preserved original differs")
        if not source.exists():
            source.mkdir(mode=0o700)
        archive.directory(source)
        if list(source.iterdir()):
            raise ValueError("unmounted source directory must be empty")
        archive.sync_dir(archive.BASE)
        archive.mount_view(args.binder, lower, source)
    archive.verify_view(source, lower, evidence)
    archive.write_report(args.output, dict(schema="AutoWheelResearchArchiveRestoreV1", source_directory=str(source),
                                          files_verified=len(evidence), bytes_verified=sum(x["bytes"] for x in evidence.values()),
                                          view_previously_mounted=mounted, original_proof_paths_preserved=True,
                                          journal_changed=False, source_qualified=False))
    print(json.dumps(dict(restore_verified=True, files=len(evidence), source_qualified=False)))


if __name__ == "__main__":
    try:
        main()
    except Exception:
        import sys
        print("research source restore failed closed", file=sys.stderr)
        raise SystemExit(1)
