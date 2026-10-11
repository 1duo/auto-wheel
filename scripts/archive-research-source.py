#!/usr/bin/env python3
"""Copy one acquired research directory and mount an owner-only read-only view.

Original bytes remain in a private backup. No financial request, journal
mutation, recursive deletion, source-proof rewrite, or credential handling.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import stat
import subprocess

BASE = Path("/home/yuduo/Projects/AutoWheel-research")
DRIVE = Path("/media/yuduo/Lexar1")
ARCHIVE = DRIVE / "AutoWheel-research-private-20261006"
BINDER_SHA256 = "5b76796da56b637f4e674e54ba0f1db7905e0f56cc74e18b078637c4834ab2e4"
NAME = re.compile(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,254}\Z")


def directory(path, mode=0o700):
    info = path.lstat()
    if (not path.is_absolute() or path.resolve() != path or not stat.S_ISDIR(info.st_mode)
            or info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != mode):
        raise ValueError("unsafe private directory")


def sync_dir(path):
    fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def verify_backing_mount():
    mounts = json.loads(subprocess.run(["findmnt", "-J", "-T", str(DRIVE), "-o", "TARGET,FSTYPE,OPTIONS"],
                                      capture_output=True, text=True, check=True).stdout)["filesystems"]
    if (len(mounts) != 1 or mounts[0]["target"] != str(DRIVE) or mounts[0]["fstype"] != "exfat"
            or not {"uid=" + str(os.getuid()), "fmask=0077", "dmask=0077"}.issubset(mounts[0]["options"].split(","))):
        raise ValueError("private exFAT backing mount differs")


def checked_hash(path, mode):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, "rb") as source:
        info = os.fstat(source.fileno())
        if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_nlink != 1
                or stat.S_IMODE(info.st_mode) != mode or info.st_size > 8 << 30):
            raise ValueError("unsafe source descriptor")
        digest = hashlib.file_digest(source, "sha256").hexdigest()
        after = os.fstat(source.fileno())
        if (info.st_dev, info.st_ino, info.st_size, info.st_mtime_ns, info.st_ctime_ns) != (
                after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns, after.st_ctime_ns):
            raise ValueError("source changed while reading")
        return digest, info.st_size


def mount_view(binary, lower, source):
    fd = os.open(binary, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, "rb") as executable:
        info = os.fstat(fd)
        if (not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o755
                or info.st_uid != os.getuid() or info.st_nlink != 1
                or hashlib.file_digest(executable, "sha256").hexdigest() != BINDER_SHA256):
            raise ValueError("unqualified executable descriptor")
        subprocess.run(["/proc/self/fd/" + str(fd), "--no-allow-other", "--perms=0600:u+D", "--realistic-permissions",
                        "--chmod-deny", "--chown-deny", "--chgrp-deny", "-r", "-o",
                        "nosuid,nodev,noexec,default_permissions,attr_timeout=0,entry_timeout=0,negative_timeout=0",
                        str(lower), str(source)], pass_fds=(fd,), check=True,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


def write_report(path, value):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, "w") as report:
        json.dump(value, report, indent=2)
        report.write("\n")
        report.flush()
        os.fsync(report.fileno())
    sync_dir(path.parent)


def verify_view(source, lower, evidence):
    directory(source)
    if {p.name for p in source.iterdir()} != set(evidence):
        raise ValueError("mounted source roster differs")
    for name, expected in evidence.items():
        if checked_hash(source / name, 0o600) != (expected["sha256"], expected["bytes"]):
            raise ValueError("mounted source differs")
    info = next(line.split() for line in Path("/proc/mounts").read_text().splitlines() if line.split()[1] == str(source))
    options = set(info[3].split(","))
    if (info[0] != str(lower) or info[2] != "fuse"
            or not {"ro", "nosuid", "nodev", "noexec", "default_permissions",
                    "user_id=" + str(os.getuid())}.issubset(options) or "allow_other" in options):
        raise ValueError("private readonly mount differs")


def unmount(source):
    helper = shutil.which("fusermount3") or shutil.which("fusermount")
    if helper is None:
        raise ValueError("unmount helper missing")
    subprocess.run([helper, "-u", str(source)], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", type=Path, required=True)
    parser.add_argument("--binder", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    source = args.source
    if (source.parent != BASE or not source.name.startswith("cycle") or "-retrieve-" not in source.name
            or not NAME.fullmatch(source.name) or args.output.exists()):
        raise ValueError("research-only source and new report required")
    for path in (BASE, DRIVE, source, args.output.parent):
        directory(path)
    if (not args.output.is_absolute() or not args.output.is_relative_to(BASE)
            or args.output.parent.resolve() != args.output.parent or not args.binder.is_relative_to(BASE)):
        raise ValueError("unsafe output")
    directory(args.binder.parent)
    binary = args.binder.lstat()
    if (args.binder.resolve() != args.binder or not stat.S_ISREG(binary.st_mode)
            or binary.st_uid != os.getuid() or binary.st_nlink != 1 or stat.S_IMODE(binary.st_mode) != 0o755):
        raise ValueError("unsafe binder")
    if checked_hash(args.binder, 0o755)[0] != BINDER_SHA256:
        raise ValueError("unqualified binder")
    verify_backing_mount()
    if source.is_mount():
        raise ValueError("source already mounted")
    members = sorted(source.iterdir())
    if not 4 <= len(members) <= 4096 or not {"receipt.json", "job-source.json", "files-source.json"}.issubset(p.name for p in members):
        raise ValueError("source roster differs")
    evidence = {}
    for member in members:
        if not NAME.fullmatch(member.name):
            raise ValueError("unsafe member")
        digest, size = checked_hash(member, 0o600)
        evidence[member.name] = dict(sha256=digest, bytes=size)
    total = sum(value["bytes"] for value in evidence.values())
    free = os.statvfs(DRIVE)
    if total > 64 << 30 or total + (1 << 30) > free.f_bavail * free.f_frsize:
        raise ValueError("archive capacity insufficient")
    # Refuse to relocate a source held open by any observable same-user process.
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
            if target == str(source) or target.startswith(str(source) + "/"):
                raise ValueError("source is in use")
    if not ARCHIVE.exists():
        ARCHIVE.mkdir(mode=0o700)
        sync_dir(DRIVE)
    directory(ARCHIVE)
    lower = ARCHIVE / source.name
    if lower.exists():
        # An offline storage retry may reuse an exact completed copy. Never
        # overwrite, complete, or accept a partial/different old directory.
        directory(lower)
        if {p.name for p in lower.iterdir()} != set(evidence):
            raise ValueError("existing archive roster differs")
        for name, expected in evidence.items():
            if checked_hash(lower / name, 0o700) != (expected["sha256"], expected["bytes"]):
                raise ValueError("existing archive differs")
    else:
        lower.mkdir(mode=0o700)
        for name, expected in evidence.items():
            fd = os.open(lower / name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
            with os.fdopen(fd, "wb") as output:
                infd = os.open(source / name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
                with os.fdopen(infd, "rb") as original:
                    info = os.fstat(original.fileno())
                    if (not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o600
                            or info.st_uid != os.getuid() or info.st_nlink != 1 or info.st_size != expected["bytes"]):
                        raise ValueError("copy source descriptor differs")
                    copied, copied_bytes = hashlib.sha256(), 0
                    for raw in iter(lambda: original.read(1 << 20), b""):
                        copied_bytes += len(raw)
                        if copied_bytes > expected["bytes"]:
                            raise ValueError("copy source grew")
                        copied.update(raw)
                        output.write(raw)
                output.flush()
                os.fsync(output.fileno())
            if copied.hexdigest() != expected["sha256"] or checked_hash(lower / name, 0o700) != (expected["sha256"], expected["bytes"]):
                raise ValueError("archive copy differs")
    sync_dir(lower)
    sync_dir(ARCHIVE)
    for name, expected in evidence.items():
        if checked_hash(source / name, 0o600) != (expected["sha256"], expected["bytes"]):
            raise ValueError("original source changed")
    backup = BASE / (source.name + ".preserved-cycle30")
    if backup.exists():
        raise ValueError("backup already exists")
    # The durable intent permits recovery if power fails after rename or mount.
    intent = dict(schema="AutoWheelResearchArchiveIntentV1", source_directory=str(source),
                  physical_archive_directory=str(lower), preserved_original_directory=str(backup),
                  files=evidence, binder_sha256=BINDER_SHA256)
    write_report(args.output.with_name(args.output.name + ".intent.json"), intent)
    os.rename(source, backup)
    mounted = False
    try:
        source.mkdir(mode=0o700)
        sync_dir(BASE)
        mount_view(args.binder, lower, source)
        mounted = True
        verify_view(source, lower, evidence)
        for name, expected in evidence.items():
            if checked_hash(backup / name, 0o600) != (expected["sha256"], expected["bytes"]):
                raise ValueError("preserved backup differs")
        result = dict(schema="AutoWheelResearchSourceArchiveV1", source_directory=str(source),
                      physical_archive_directory=str(lower), preserved_original_directory=str(backup),
                      files=evidence, total_bytes=total, binder_sha256=BINDER_SHA256,
                      original_paths_and_bytes_preserved=True, original_backup_retained=True,
                      read_only=True, source_guard_changed=False, source_qualified=False,
                      financial_requests=0, full_window_capacity_verified=False)
        write_report(args.output, result)
        print(json.dumps({"archive_complete": True, "files": len(evidence), "bytes": total,
                          "original_backup_retained": True, "source_qualified": False}))
    except Exception:
        if mounted or source.is_mount():
            unmount(source)
        if source.exists():
            source.rmdir()
        os.rename(backup, source)
        sync_dir(BASE)
        raise


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        import sys
        # These messages are constant validation labels, never source bodies.
        label = str(error) if isinstance(error, ValueError) else type(error).__name__
        print("research source archive failed: " + label, file=sys.stderr)
        raise SystemExit(1)
