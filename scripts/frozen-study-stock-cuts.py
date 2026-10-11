"""Source-bound daily capture cuts for the entire frozen stock study.

Only the reviewed Apr6–Oct2,2026 calendar and unchanged15-symbol study are
accepted. This outer interface does not reduce books or qualify replay data.
"""
from datetime import date, datetime, timedelta, timezone
import hashlib
import json
from pathlib import Path
import re
from zoneinfo import ZoneInfo

BASE = Path('/home/yuduo/Projects/AutoWheel-research')
MANIFEST = BASE / 'cycle27-stock-daily-acquisition-queries-20261006/manifest.json'
MANIFEST_SHA = '25056b1d6d41d3a97e9ffb7700cf157b3f195a1eff085b1bc8f2f5a4ba9b5333'
FIRST, LAST = date(2026, 4, 6), date(2026, 10, 2)
NANO = 1_000_000_000
SYMBOLS = ['NFLX','TSLA','NVDA','NBIS','IREN','NOW','AMD','AMZN','GOOGL','META','MSFT','PLTR','UBER','AVGO','ORCL']
PINS = {
    'research/simulation/2026-10-05/spec.jsonl': '2ff4ae3765f4102a67382d645164ae4dfb6e036cc7ea3b113eab6dc24f4eac98',
    'research/simulation/2026-10-05/mode-conservative-05015/spec.jsonl': '55fbfe17b47d9b85d98a3010ae2b9d744a55bde2937018619a742d19b6fa77b6',
    'research/simulation/2026-10-05/mode-balanced-1535/spec.jsonl': '73dcb1ff6f9f81be71f419540c3fca19f169d86c5108b34581a125c1c53698ab',
    'internal/simulation/replay.go': '1e3a75597569b7bfdb2a169d93a58a6baeba85897c9c32ef0fb1b21848cb8023',
    'internal/wheel/strategy.go': 'be2af36f7a23158b7c72dd095ebee2390c53335883320082a940ddd882ac7b61',
    'internal/wheel/calendar.go': 'ad1ec4d912d091fa89dea0d958e9b9643a026c596554fa1edac04879618d814e',
}

def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()

def scope(root):
    binding = []
    for name, expected in PINS.items():
        path = root / name
        if path.is_symlink() or digest(path) != expected:
            raise ValueError('frozen stock study source differs')
        binding.append(dict(path=name, sha256=expected))
    for name in list(PINS)[:3]:
        config = json.loads((root / name).read_bytes())
        if (config['start_date'] != FIRST.isoformat() or config['end_date'] != LAST.isoformat()
                or config['strategy_config']['symbols'] != SYMBOLS
                or config['strategy_config']['poll_seconds'] != 300
                or config['strategy_config']['max_quote_age_seconds'] != 30):
            raise ValueError('frozen stock study configuration differs')
    calendar = (root / 'internal/wheel/calendar.go').read_text()
    def dates(variable):
        matches = re.findall(r'var ' + variable + r' = map\[string\]struct\{\}\{(.*?)\n\}', calendar, re.S)
        if len(matches) != 1:
            raise ValueError('reviewed calendar declaration differs')
        return {date.fromisoformat(value) for value in re.findall(r'"(\d{4}-\d{2}-\d{2})"', matches[0])}
    holidays, early = dates('exchangeHolidays'), dates('earlyCloses')
    sessions = [FIRST + timedelta(days=i) for i in range((LAST-FIRST).days+1)
                if (FIRST + timedelta(days=i)).weekday() < 5 and FIRST + timedelta(days=i) not in holidays]
    if len(sessions) != 126 or set(sessions) & early:
        raise ValueError('frozen session count or ordinary-close scope differs')
    if MANIFEST.is_symlink() or digest(MANIFEST) != MANIFEST_SHA:
        raise ValueError('accepted full stock acquisition manifest differs')
    manifest = json.loads(MANIFEST.read_bytes())
    if (manifest['daily_partitions'] != 126 or manifest['universe'] != SYMBOLS
            or [item['date'] for item in manifest['queries']] != [day.isoformat() for day in sessions]):
        raise ValueError('full stock acquisition sessions differ')
    binding.append(dict(path=str(MANIFEST), sha256=MANIFEST_SHA))
    binding.append(dict(path='scripts/frozen-study-stock-cuts.py', sha256=digest(root / 'scripts/frozen-study-stock-cuts.py')))
    return sessions, binding

def make(day, root):
    sessions, binding = scope(root)
    day = date.fromisoformat(day)
    if day not in sessions:
        raise ValueError('stock session outside frozen scope')
    origin = datetime(day.year, day.month, day.day, tzinfo=ZoneInfo('America/New_York'))
    cuts = [int((origin + timedelta(minutes=minute)).timestamp()) * NANO
            for minute in [*range(575,945,5),960]]
    return dict(schema='AutoWheelFrozenStudyStockCaptureCutsV1',
        query_start=day.isoformat()+'T00:00:00Z', query_end=(day+timedelta(days=1)).isoformat()+'T00:00:00Z',
        cuts_ns=cuts, max_age_nanos=30*NANO, decision_cuts=74, closing_valuation_cuts=1,
        timezone='America/New_York', first_day_only=False, study_sessions=126,
        study_decision_cuts=9324, study_closing_valuation_cuts=126, source_files=binding,
        full_source_qualification=False)

def frozen_cuts(document, query, start, end, root):
    if (query['dataset'] != 'EQUS.MINI' or query['schema'] != 'mbp-1'
            or query['stype_in'] != 'raw_symbol' or query['symbols'] != SYMBOLS
            or query['start'][-10:] != 'T00:00:00Z'):
        raise ValueError('frozen full stock query differs')
    expected = make(query['start'][:10], root)
    canonical = lambda value: json.dumps(value, sort_keys=True, separators=(',', ':'), allow_nan=False)
    if canonical(document) != canonical(expected) or query['end'] != expected['query_end']:
        raise ValueError('frozen full-study capture cuts differ')
    utc_day = datetime.strptime(query['start'], '%Y-%m-%dT%H:%M:%SZ').replace(tzinfo=timezone.utc)
    if (type(start) is not int or type(end) is not int or start != int(utc_day.timestamp())*NANO
            or end != start+86400*NANO or type(document['max_age_nanos']) is not int
            or any(type(value) is not int or not start <= value < end for value in document['cuts_ns'])):
        raise ValueError('stock UTC partition or numeric cut shape differs')
    return document['cuts_ns'], [row['path'] for row in expected['source_files']]

def verify_frozen_cuts(document, query, start, end, root):
    """Independent outer timestamp arithmetic, sharing only immutable bindings."""
    sessions, binding = scope(root)
    stamp = datetime.strptime(query['start'], '%Y-%m-%dT%H:%M:%SZ').replace(tzinfo=timezone.utc)
    day = stamp.date()
    if (day not in sessions or query['start'] != day.isoformat()+'T00:00:00Z'
            or query['end'] != (day+timedelta(days=1)).isoformat()+'T00:00:00Z'
            or query['dataset'] != 'EQUS.MINI' or query['schema'] != 'mbp-1'
            or query['stype_in'] != 'raw_symbol' or query['symbols'] != SYMBOLS
            or type(start) is not int or type(end) is not int
            or start != int(stamp.timestamp())*NANO or end-start != 86400*NANO):
        raise ValueError('independent frozen stock query differs')
    # Derive the local calendar opening and close directly as UTC seconds.
    opening = datetime.combine(day, datetime.min.time(), tzinfo=ZoneInfo('America/New_York')) + timedelta(hours=9, minutes=35)
    closing = opening + timedelta(hours=6, minutes=25)
    first, close = int(opening.timestamp())*NANO, int(closing.timestamp())*NANO
    expected = [first + k*300*NANO for k in range(74)] + [close]
    keys = {'schema','query_start','query_end','cuts_ns','max_age_nanos','decision_cuts',
            'closing_valuation_cuts','timezone','first_day_only','study_sessions','study_decision_cuts',
            'study_closing_valuation_cuts','source_files','full_source_qualification'}
    if (set(document) != keys or document['schema'] != 'AutoWheelFrozenStudyStockCaptureCutsV1'
            or document['query_start'] != query['start'] or document['query_end'] != query['end']
            or document['cuts_ns'] != expected or document['source_files'] != binding
            or document['timezone'] != 'America/New_York' or document['first_day_only'] is not False
            or document['full_source_qualification'] is not False
            or any(type(document[key]) is not int or document[key] != value for key,value in
                   (('decision_cuts',74),('closing_valuation_cuts',1),('study_sessions',126),
                    ('study_decision_cuts',9324),('study_closing_valuation_cuts',126),('max_age_nanos',30*NANO)))
            or any(type(value) is not int or not start <= value < end for value in document['cuts_ns'])):
        raise ValueError('independent full-study cuts differ')
    return expected, [row['path'] for row in binding]
