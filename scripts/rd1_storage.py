"""Private, create-only RD1 artifacts on the verified Lexar view (Linux only)."""
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import subprocess

PARENT = Path('/home/yuduo/Projects/AutoWheel-research/theta-campaign-r6-20261013-1757')
PHYSICAL = Path('/media/yuduo/Lexar1/AutoWheel-research-private-20261006') / PARENT.name
PROTOCOL_SHA256 = '87c07e6d9b5bf6ccbf6c7299d47d09c0a50195e3d38e7c76ef1250c04707188f'
AUTHORIZATION_SHA256 = {'rd1-261016-2037': 'a7bf0428b528cf3872e1ce6d185cc6dbeb80bc0179b89c4b910efd9f57597352'}


def require(value, message):
    if not value:
        raise ValueError(message)


def canonical(value):
    return (json.dumps(value, sort_keys=True, separators=(',', ':'), allow_nan=False)+'\n').encode()


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def verify_storage():
    for path in (PARENT, PHYSICAL):
        info = path.lstat()
        require(stat.S_ISDIR(info.st_mode) and info.st_uid == os.getuid()
                and stat.S_IMODE(info.st_mode) & 0o077 == 0, 'private campaign storage changed')
    def mount(path):
        return json.loads(subprocess.check_output(['findmnt', '-J', '-T', str(path),
            '-o', 'SOURCE,FSTYPE,OPTIONS,UUID'], timeout=3))['filesystems'][0]
    physical, logical = mount(PHYSICAL), mount(PARENT)
    require(physical['uuid'] == '62F9-12AF' and physical['fstype'] == 'exfat'
            and 'rw' in physical['options'].split(','), 'Lexar physical identity changed')
    require(logical['fstype'] == 'fuse' and logical['source'] == str(PHYSICAL)
            and {'rw', 'nosuid', 'nodev', 'noexec', 'default_permissions',
                 'user_id='+str(os.getuid())} <= set(logical['options'].split(','))
            and 'allow_other' not in logical['options'].split(','), 'private bind changed')
    free = min(os.statvfs(p).f_bavail * os.statvfs(p).f_frsize for p in (PARENT, PHYSICAL))
    require(free >= (20 << 30) + (256 << 20), 'Lexar headroom insufficient')
    return free


def root(run_id, *, create=False):
    require(re.fullmatch(r'rd1-[0-9]{6}-[0-9]{4}', run_id) is not None, 'invalid RD1 run ID')
    verify_storage()
    namespace = PARENT / 'reduced-data-rd1'
    if create:
        namespace.mkdir(mode=0o700, exist_ok=True)
        info = namespace.lstat()
        require(stat.S_ISDIR(info.st_mode) and not namespace.is_symlink() and info.st_uid == os.getuid()
                and stat.S_IMODE(info.st_mode) & 0o077 == 0, 'RD1 namespace lost privacy before creation')
    path = namespace / run_id
    if create:
        path.mkdir(mode=0o700)
    for p in (namespace, path):
        info = p.lstat()
        require(stat.S_ISDIR(info.st_mode) and not p.is_symlink() and info.st_uid == os.getuid()
                and stat.S_IMODE(info.st_mode) & 0o077 == 0, 'RD1 root lost privacy')
    return path


def write_bytes(path, raw):
    verify_storage()
    require(path.is_relative_to(PARENT / 'reduced-data-rd1') and path.resolve() == path,
            'RD1 output escaped namespace')
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, 'wb') as f:
        f.write(raw); f.flush(); os.fsync(f.fileno())
    physical = PHYSICAL / path.relative_to(PARENT)
    require(path.stat().st_mode & 0o077 == 0 and physical.stat().st_size == len(raw)
            and sha(physical.read_bytes()) == sha(raw), 'physical Lexar output differs')
    directory = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        os.fsync(directory)
    finally:
        os.close(directory)
    return dict(file=str(path), sha256=sha(raw), bytes=len(raw))


def write(path, value):
    return write_bytes(path, canonical(value))


def budget(run, observation, timeout):
    auth_raw = (run/'authorization.json').read_bytes()
    require(sha(auth_raw) == AUTHORIZATION_SHA256.get(run.name), 'RD1 authorization hash differs')
    auth = json.loads(auth_raw)
    require(auth['authorization_text'] == 'Start RD1' and auth['protocol_sha256'] == PROTOCOL_SHA256,
            'RD1 authorization differs')
    goal = observation['goal']
    age = (datetime.now(timezone.utc)-datetime.fromisoformat(observation['recorded_at'])).total_seconds()
    used = goal['timeUsedSeconds'] - auth['active_seconds_baseline']
    require(goal['status'] == 'active' and goal['threadId'] == auth['native_goal_observation']['threadId']
            and 0 <= age <= 900 and 0 <= used and 0 < timeout <= 900
            and used+age+timeout < auth['admission_cutoff_seconds'], 'RD1 admission/budget fence')
    return dict(active_seconds_used=used, observation_age_seconds=age)
