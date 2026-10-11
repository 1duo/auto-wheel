"""Read-only, fail-closed process census for one external download namespace."""
import json
import os
from pathlib import Path
import re
import subprocess

BASE = Path('/home/yuduo/Projects/AutoWheel-research')
VIEW = BASE / 'cycle73-bulk-storage-20261008'
PHYSICAL = Path('/media/yuduo/Lexar1/AutoWheel-research-private-20261006/cycle73-bulk-storage-20261008')
NAME = re.compile(r'cycle[0-9]+[A-Za-z0-9._-]*-retrieve-[A-Za-z0-9._-]+')

# The privileged subprocess reads only /proc metadata. It emits no file contents,
# commands, credentials, or complete descriptor paths.
SCANNER = r'''
import json,os,re,sys
from pathlib import Path
watched=json.loads(sys.argv[1]);uid=int(sys.argv[2]);excluded=int(sys.argv[3])
proc=Path(sys.argv[4])
def hit(value):
 if value.endswith(' (deleted)'):value=value[:-10]
 return next((i for i,p in enumerate(watched) if value==p or value.startswith(p+'/')),None)
def inspect(value,pid,kind):
 scope=hit(value)
 if scope is not None:
  print(json.dumps(dict(status='in_use',pid=pid,kind=kind,scope_index=scope)))
  sys.exit(2)
try:
 for process in proc.iterdir():
  if not process.name.isdigit() or int(process.name)==excluded:continue
  try:
   if process.stat().st_uid!=uid:continue
   for name in ('cwd','root'):
    try:inspect(os.readlink(process/name),int(process.name),name)
    except FileNotFoundError:pass
   for fd in (process/'fd').iterdir():
    try:inspect(os.readlink(fd),int(process.name),'fd')
    except FileNotFoundError:pass
   for line in (process/'maps').read_text().splitlines():
    fields=line.split(maxsplit=5)
    if len(fields)==6:
     value=re.sub(r'\\(040|011|012|134)',lambda m:chr(int(m[1],8)),fields[5])
     inspect(value,int(process.name),'map')
  except (FileNotFoundError,ProcessLookupError):
   # A process may disappear, but a surviving process with an unreadable or
   # missing census surface cannot be silently omitted.
   if process.exists():raise
 print(json.dumps(dict(status='quiescent')))
except (OSError,ValueError):
 print(json.dumps(dict(status='census_unavailable')))
 sys.exit(3)
'''


def source_paths(source, view=VIEW, physical=PHYSICAL):
    if (not source.is_absolute() or source.parent != view or not NAME.fullmatch(source.name)
            or source.resolve() != source or view.resolve() != view
            or physical.resolve() != physical):
        raise ValueError('selected download quiescence scope differs')
    lower = physical / source.name
    if lower.resolve() != lower:
        raise ValueError('physical download quiescence scope differs')
    return source, lower


def census(paths, *, privileged=True, proc=Path('/proc'), executable='python3'):
    if len(paths) != 2 or any(not p.is_absolute() for p in paths):
        raise ValueError('logical and physical process scopes required')
    command = (['sudo', '-n'] if privileged else []) + [executable, '-c', SCANNER,
        json.dumps([str(p) for p in paths]), str(os.getuid()), str(os.getpid()), str(proc)]
    result = subprocess.run(command, capture_output=True, timeout=30)
    if len(result.stdout) > 4096:
        raise ValueError('process census output exceeds bound')
    try:
        value = json.loads(result.stdout)
    except (ValueError, UnicodeError) as error:
        raise ValueError('selected download process census unavailable') from error
    expected = {'quiescent': 0, 'in_use': 2, 'census_unavailable': 3}
    if value.get('status') not in expected or result.returncode != expected[value['status']]:
        raise ValueError('selected download process census unavailable')
    return value


def quiescent(source):
    result = census(source_paths(source))
    if result['status'] != 'quiescent':
        raise ValueError('selected download ' + result['status'])
    return result
