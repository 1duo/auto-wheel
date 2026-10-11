#!/usr/bin/env python3
"""Release the redundant preserved original after archive verification.

Validates the historical archive report, pinned copy, and live readonly
view, then unlinks only enumerated flat backup files. No recursive
deletion, mounts, production writes, secrets, journal, or copy mutation.
"""
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import stat


def fingerprint(info):
    return (info.st_dev, info.st_ino, info.st_mode, info.st_uid, info.st_nlink,
            info.st_size, info.st_mtime_ns, info.st_ctime_ns)


def bounded_json(path):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, "rb") as opened:
        info = os.fstat(fd)
        if (not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o600
                or info.st_uid != os.getuid() or info.st_nlink != 1 or info.st_size > 1 << 20):
            raise ValueError("private bounded report required")
        raw = opened.read((1 << 20) + 1)
        if len(raw) > 1 << 20:
            raise ValueError("private bounded report required")
        value = json.loads(raw)
        after = os.fstat(fd)
        if fingerprint(info) != fingerprint(after):
            raise ValueError("report changed while reading")
        return value, hashlib.sha256(raw).hexdigest(), fingerprint(info)


def quiescent(paths, lower, binder_sha256):
    # Observable same-user descriptor scan only; not a global reader lock.
    # Main runs this helper at a quiescent boundary.
    for process in Path("/proc").iterdir():
        if not process.name.isdigit():
            continue
        try:
            descriptors = list((process / "fd").iterdir())
        except (PermissionError, FileNotFoundError):
            continue
        for descriptor in descriptors:
            try:
                target = os.readlink(descriptor)
            except OSError:
                continue
            for watched in paths:
                if target == str(watched) or target.startswith(str(watched) + "/"):
                    # bindfs holds its lower directory for the lifetime of the
                    # verified view. Permit only that directory descriptor from
                    # the pinned executable; open archive members still block.
                    if target == str(lower):
                        try:
                            with (process / "exe").open("rb") as executable:
                                info = os.fstat(executable.fileno())
                                raw = executable.read((8 << 20) + 1)
                            if (stat.S_ISREG(info.st_mode) and info.st_uid == os.getuid()
                                    and len(raw) <= 8 << 20
                                    and hashlib.sha256(raw).hexdigest() == binder_sha256):
                                continue
                        except OSError:
                            pass
                    raise ValueError("research directory in use")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--intent", type=Path, required=True)
    parser.add_argument("--archive-report", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    spec = importlib.util.spec_from_file_location("archive", Path(__file__).with_name("archive-research-source.py"))
    archive = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(archive)
    for path in (args.intent, args.archive_report, args.output):
        if not path.is_absolute() or not path.is_relative_to(archive.BASE) or path.resolve() != path:
            raise ValueError("private research paths required")
    if args.output.exists():
        raise ValueError("fresh output required")
    for path in (archive.BASE, args.intent.parent, args.archive_report.parent, args.output.parent):
        archive.directory(path)
    intent, intent_digest, intent_info = bounded_json(args.intent)
    report, report_digest, report_info = bounded_json(args.archive_report)
    source, lower, backup = (Path(intent[k]) for k in ("source_directory", "physical_archive_directory", "preserved_original_directory"))
    evidence = intent["files"]
    if (intent["schema"] != "AutoWheelResearchArchiveIntentV1"
            or report.get("schema") != "AutoWheelResearchSourceArchiveV1"
            or intent["binder_sha256"] != archive.BINDER_SHA256 or report.get("binder_sha256") != archive.BINDER_SHA256
            or source.parent != archive.BASE or not archive.NAME.fullmatch(source.name) or "-retrieve-" not in source.name
            or lower != archive.ARCHIVE / source.name or backup != archive.BASE / (source.name + ".preserved-cycle30")
            or not isinstance(evidence, dict) or not 4 <= len(evidence) <= 4096
            or not source.name.startswith("cycle")
            or report.get("read_only") is not True or report.get("original_paths_and_bytes_preserved") is not True):
        raise ValueError("archive binding differs")
    if not (report.get("source_directory") == str(source) and report.get("physical_archive_directory") == str(lower)
            and report.get("preserved_original_directory") == str(backup) and report.get("files") == evidence):
        raise ValueError("archive report binding differs")
    for name, item in evidence.items():
        if (not archive.NAME.fullmatch(name) or not isinstance(item, dict)
                or not isinstance(item.get("sha256"), str) or not re.fullmatch("[0-9a-f]{64}", item["sha256"])
                or type(item.get("bytes")) is not int or not 0 <= item["bytes"] <= 8 << 30):
            raise ValueError("archive member differs")
    total = sum(item["bytes"] for item in evidence.values())
    if total > 64 << 30:
        raise ValueError("archive capacity differs")
    for directory in (archive.DRIVE, archive.ARCHIVE, lower):
        archive.directory(directory)
    archive.verify_backing_mount()
    if {p.name for p in lower.iterdir()} != set(evidence):
        raise ValueError("backing archive roster differs")
    for name, expected in evidence.items():
        if archive.checked_hash(lower / name, 0o700) != (expected["sha256"], expected["bytes"]):
            raise ValueError("backing archive differs")
    archive.verify_view(source, lower, evidence)
    if (fingerprint(args.intent.lstat()) != intent_info
            or fingerprint(args.archive_report.lstat()) != report_info):
        raise ValueError("archive evidence changed")
    # Enumerate remaining backup members; unknown/different fails before any unlink.
    if not backup.exists():
        remaining = []
    else:
        archive.directory(backup)
        if backup.is_mount():
            raise ValueError("backup is mounted")
        members = sorted(p.name for p in backup.iterdir())
        remaining = []
        for name in members:
            expected = evidence.get(name)
            if expected is None:
                raise ValueError("unknown backup member")
            if archive.checked_hash(backup / name, 0o600) != (expected["sha256"], expected["bytes"]):
                raise ValueError("backup member differs")
            remaining.append(name)
    watched = [source, lower] + ([backup] if backup.exists() else [])
    quiescent(watched, lower, archive.BINDER_SHA256)
    # Durable release intent before first unlink; historical archive report untouched.
    release_intent = dict(schema="AutoWheelResearchBackupReleaseIntentV1", source_directory=str(source),
                          physical_archive_directory=str(lower), preserved_original_directory=str(backup),
                          archive_intent_sha256=intent_digest, archive_report_sha256=report_digest,
                          files_intended=sorted(remaining), bytes_intended=sum(evidence[n]["bytes"] for n in remaining))
    release_intent_path = args.output.with_name(args.output.name + ".intent.json")
    if release_intent_path.exists():
        raise ValueError("fresh release intent required")
    archive.write_report(release_intent_path, release_intent)
    # Recheck full copy/view and every remaining member immediately before deletion.
    if {p.name for p in lower.iterdir()} != set(evidence):
        raise ValueError("backing archive roster differs")
    for name, expected in evidence.items():
        if archive.checked_hash(lower / name, 0o700) != (expected["sha256"], expected["bytes"]):
            raise ValueError("backing archive differs")
    archive.verify_view(source, lower, evidence)
    if backup.exists():
        archive.directory(backup)
        current = sorted(p.name for p in backup.iterdir())
        if current != remaining:
            raise ValueError("backup roster changed")
        member_info = {}
        for name in current:
            if archive.checked_hash(backup / name, 0o600) != (evidence[name]["sha256"], evidence[name]["bytes"]):
                raise ValueError("backup member differs")
            member_info[name] = fingerprint((backup / name).lstat())
        quiescent(watched, lower, archive.BINDER_SHA256)
        descriptor = os.open(backup, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
        try:
            if os.fstat(descriptor).st_ino != backup.lstat().st_ino or os.fstat(descriptor).st_dev != backup.lstat().st_dev:
                raise ValueError("backup directory changed")
            for name in current:
                if fingerprint(os.stat(name, dir_fd=descriptor, follow_symlinks=False)) != member_info[name]:
                    raise ValueError("backup member changed")
                os.unlink(name, dir_fd=descriptor)
            os.fsync(descriptor)
        finally:
            os.close(descriptor)
        archive.sync_dir(backup)
        if list(backup.iterdir()):
            raise ValueError("backup not empty")
        if backup.is_mount():
            raise ValueError("backup is mounted")
        backup.rmdir()
        archive.sync_dir(archive.BASE)
    if backup.exists():
        raise ValueError("backup still present")
    archive.verify_view(source, lower, evidence)
    if {p.name for p in lower.iterdir()} != set(evidence):
        raise ValueError("backing archive roster differs")
    for name, expected in evidence.items():
        if archive.checked_hash(lower / name, 0o700) != (expected["sha256"], expected["bytes"]):
            raise ValueError("backing archive differs")
    archive.write_report(args.output, dict(schema="AutoWheelResearchBackupReleaseV1", source_directory=str(source),
                                           physical_archive_directory=str(lower),
                                           preserved_original_directory=str(backup),
                                           files_released=len(remaining), bytes_released=sum(evidence[n]["bytes"] for n in remaining),
                                           original_backup_retained=False, source_qualified=False,
                                           full_window_capacity_verified=False))
    print(json.dumps(dict(release_complete=True, files=len(remaining),
                          bytes=sum(evidence[n]["bytes"] for n in remaining),
                          original_backup_retained=False, source_qualified=False)))


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        import sys
        print("research backup release failed closed", file=sys.stderr)
        raise SystemExit(1)
