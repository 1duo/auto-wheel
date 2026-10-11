"""Bind daily closed captures to every accepted frozen option query.

This outer interface preserves the reviewed calendar and reducers. It does
not establish terms, eligibility, continuous execution, Greeks or performance.
"""
from datetime import date, datetime, timedelta, timezone
import hashlib
import importlib.util
import json
from pathlib import Path
from zoneinfo import ZoneInfo

BASE = Path('/home/yuduo/Projects/AutoWheel-research')
MANIFEST = BASE / 'cycle24-selective-acquisition-queries-20261006/manifest.json'
MANIFEST_SHA = 'ccf48c229ce29db62e0d4aaa20cb06c733fdf4e528cf2d79f4da0e71a00adb2b'
CALENDAR_INTERFACE_SHA = 'b472c8203ab3081468beec7d9d810f6e51483af07a3024e948bbf412741255a9'
NANO = 1_000_000_000
SCHEMA = 'AutoWheelFrozenStudyOptionCaptureCutsV1'

def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()

def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(',', ':'), allow_nan=False)

def calendar_interface(root):
    path = root / 'scripts/frozen-study-stock-cuts.py'
    if path.is_symlink() or digest(path) != CALENDAR_INTERFACE_SHA:
        raise ValueError('reviewed full-study calendar interface differs')
    spec = importlib.util.spec_from_file_location('option_capture_calendar', path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module

def query_binding(query, root):
    calendar = calendar_interface(root)
    sessions, binding = calendar.scope(root)
    if (not isinstance(query, dict)
            or set(query) != {'dataset', 'schema', 'stype_in', 'symbols', 'start', 'end'}
            or query['dataset'] != 'OPRA.PILLAR' or query['schema'] != 'cmbp-1'
            or query['stype_in'] != 'raw_symbol'
            or not isinstance(query['start'], str) or not isinstance(query['end'], str)
            or not isinstance(query['symbols'], list) or not 1 <= len(query['symbols']) <= 2000
            or any(not isinstance(symbol, str) for symbol in query['symbols'])):
        raise ValueError('frozen option query shape differs')
    day = date.fromisoformat(query['start'][:10])
    if (day not in sessions or query['start'] != day.isoformat() + 'T00:00:00Z'
            or query['end'] != (day + timedelta(days=1)).isoformat() + 'T00:00:00Z'):
        raise ValueError('option UTC session outside frozen scope')
    if MANIFEST.is_symlink() or digest(MANIFEST) != MANIFEST_SHA:
        raise ValueError('accepted full option acquisition manifest differs')
    manifest = json.loads(MANIFEST.read_bytes())
    books = [item for item in manifest['queries'] if item['schema'] == 'cmbp-1']
    if (manifest['daily_partitions'] != 126 or len(books) != 543
            or {item['date'] for item in books} != {session.isoformat() for session in sessions}):
        raise ValueError('full option acquisition scope differs')
    matches = []
    for item in books:
        if item['date'] != day.isoformat():
            continue
        path = MANIFEST.parent / item['filename']
        if path.is_symlink() or path.parent != MANIFEST.parent or digest(path) != item['sha256']:
            raise ValueError('accepted option query file differs')
        original = json.loads(path.read_bytes())
        if canonical(original) == canonical(query):
            matches.append(dict(path=str(path), sha256=item['sha256']))
    if len(matches) != 1:
        raise ValueError('option query is not an exact accepted partition')
    binding += [dict(path=str(MANIFEST), sha256=MANIFEST_SHA), matches[0],
                dict(path='scripts/frozen-study-option-cuts.py',
                     sha256=digest(root / 'scripts/frozen-study-option-cuts.py'))]
    return day, calendar, binding

def make(query, root):
    day, calendar, binding = query_binding(query, root)
    document = calendar.make(day.isoformat(), root)
    document['schema'] = SCHEMA
    document['source_files'] = binding
    return document

def frozen_cuts(document, query, start, end, root):
    expected = make(query, root)
    stamp = datetime.strptime(query['start'], '%Y-%m-%dT%H:%M:%SZ').replace(tzinfo=timezone.utc)
    if (canonical(document) != canonical(expected) or type(start) is not int or type(end) is not int
            or start != int(stamp.timestamp()) * NANO or end != start + 86400 * NANO
            or any(type(value) is not int or not start <= value < end for value in document['cuts_ns'])):
        raise ValueError('frozen full-study option capture cuts differ')
    return document['cuts_ns'], [row['path'] for row in expected['source_files']]

def verify_frozen_cuts(document, query, start, end, root):
    """Derive timestamps independently, sharing only the immutable bindings."""
    day, _, binding = query_binding(query, root)
    stamp = datetime.combine(day, datetime.min.time(), tzinfo=timezone.utc)
    opening = datetime.combine(day, datetime.min.time(), tzinfo=ZoneInfo('America/New_York'))
    first = int((opening + timedelta(hours=9, minutes=35)).timestamp()) * NANO
    close = int((opening + timedelta(hours=16)).timestamp()) * NANO
    expected = [first + index * 300 * NANO for index in range(74)] + [close]
    keys = {'schema', 'query_start', 'query_end', 'cuts_ns', 'max_age_nanos', 'decision_cuts',
            'closing_valuation_cuts', 'timezone', 'first_day_only', 'study_sessions',
            'study_decision_cuts', 'study_closing_valuation_cuts', 'source_files',
            'full_source_qualification'}
    if (set(document) != keys or document['schema'] != SCHEMA
            or document['query_start'] != query['start'] or document['query_end'] != query['end']
            or document['cuts_ns'] != expected or document['source_files'] != binding
            or document['timezone'] != 'America/New_York' or document['first_day_only'] is not False
            or document['full_source_qualification'] is not False
            or type(start) is not int or type(end) is not int
            or start != int(stamp.timestamp()) * NANO or end - start != 86400 * NANO
            or any(type(document[key]) is not int or document[key] != value for key, value in
                   (('decision_cuts', 74), ('closing_valuation_cuts', 1), ('study_sessions', 126),
                    ('study_decision_cuts', 9324), ('study_closing_valuation_cuts', 126),
                    ('max_age_nanos', 30 * NANO)))
            or any(type(value) is not int or not start <= value < end for value in document['cuts_ns'])):
        raise ValueError('independent full-study option cuts differ')
    return expected, [row['path'] for row in binding]
