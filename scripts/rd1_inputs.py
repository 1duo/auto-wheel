"""Exact RD1 source decoding and price-independent contract choice.

Bucket timestamps and one-lot capacity are model assumptions. This adapter does
not turn a sampled book into causal receipt evidence or an actual broker fill.
"""
from datetime import date, datetime, timedelta
from decimal import Decimal
from fractions import Fraction
import json

from rd1_storage import require, sha

MAX_MONEY = 100_000_000_000


def direct_common_title(value):
    """Recognize class text despite missing XBRL word-separator whitespace.

    This classifies a supplied title only; issuer, joint context, exchange and
    dated source checks remain separate. Preserve the original field as evidence.
    """
    require(type(value) is str, 'security class title must be text')
    compact = ''.join(value.casefold().split())
    return any(x in compact for x in ('commonstock','commonshares','ordinaryshares')) and not any(
        x in compact for x in ('preferred','preference','deposit','notes','debenture',
                              'warrant','purchaseright','beneficialinterest'))


def decode(raw):
    def pairs(items):
        result = {}
        for key, value in items:
            require(key not in result, 'duplicate JSON key')
            result[key] = value
        return result
    return json.loads(raw, parse_float=Decimal, object_pairs_hook=pairs,
                      parse_constant=lambda _: (_ for _ in ()).throw(ValueError('nonfinite JSON')))


def cents(value):
    require(type(value) in (int, Decimal), 'source price must be an exact JSON number')
    exact = Fraction(value)*100
    require(exact.denominator == 1 and 0 <= exact <= MAX_MONEY, 'fractional-cent or excessive price')
    return exact.numerator


def receipt_rows(path):
    require(path.is_file() and not path.is_symlink() and path.stat().st_size <= 1 << 20,
            'unbounded or linked source receipt')
    receipt = decode(path.read_bytes())
    raw_path = path.with_name(path.name.replace('.receipt.json', '.raw'))
    require(raw_path.is_file() and not raw_path.is_symlink() and raw_path.stat().st_size <= 64 << 20,
            'unbounded or linked raw source')
    raw = raw_path.read_bytes()
    require(receipt['status'] == 'captured' and receipt['complete'] is True
            and receipt['http_status'] == 200 and len(raw) == receipt['bytes']
            and sha(raw) == receipt['sha256'], 'incomplete or modified source')
    payload = decode(raw)
    require(type(payload) is dict and set(payload) == {'response'}
            and type(payload['response']) is list, 'unexpected source envelope')
    return receipt, payload['response']


def session_days(calendar, first, last):
    closures = {row['date']:row for row in calendar}
    require(len(closures) == len(calendar), 'duplicate calendar date')
    result = []
    day = date.fromisoformat(first)
    while day <= date.fromisoformat(last):
        closure = closures.get(str(day), {})
        if day.weekday() < 5 and closure.get('type') != 'full_close':
            result.append(dict(date=str(day), close=closure.get('close') or '16:00:00'))
        day += timedelta(days=1)
    return result


def book(row, expected):
    stamp = datetime.fromisoformat(row['timestamp'])
    require(stamp.tzinfo is None and stamp.isoformat(timespec='milliseconds') == row['timestamp']
            and stamp == expected, 'quote bucket differs or has an ambiguous timezone')
    bid, ask = cents(row['bid']), cents(row['ask'])
    require(ask > 0 and ask >= bid, 'invalid/crossed book')
    return dict(bid_cents=bid, ask_cents=ask, source_timestamp=row['timestamp'])


def option_mark(row, expected):
    # RD1 requires a positive short-liability ask. Preserve a crossed bid as an
    # entry/cost-shock defect; do not erase the separately observed ask or call
    # the two-sided book executable. No half-spread is inferred from a cross.
    stamp = datetime.fromisoformat(row['timestamp'])
    require(stamp.tzinfo is None and stamp == expected
            and stamp.isoformat(timespec='milliseconds') == row['timestamp'], 'option bucket differs')
    bid, ask = cents(row['bid']), cents(row['ask'])
    require(ask > 0, 'missing positive short-liability ask')
    return dict(bid_cents=bid, ask_cents=ask, source_timestamp=row['timestamp'],
                crossed=bid > ask, entry_book_valid=bid > 0 and ask >= bid,
                half_spread_cost_defined=ask >= bid)


def contract(value):
    require(set(value) == {'symbol','expiration','strike','right'}
            and value['right'] in ('PUT','CALL'), 'ambiguous contract fields')
    expiry = date.fromisoformat(value['expiration'])
    strike = cents(value['strike'])
    require(strike > 0 and strike <= MAX_MONEY//100, 'invalid contract strike')
    return dict(symbol=value['symbol'], expiry=str(expiry), kind=value['right'].lower(),
                strike_cents=strike,
                id=f"{value['symbol']:<6}{expiry:%y%m%d}{value['right'][0]}{strike*10:08d}")


def choose(menu, day, symbol, kind, spot_twice, target_num, target_den):
    """Choose identity before looking at side validity/premium. Never replace it."""
    candidates = []
    for item in menu:
        c = contract(item)
        dte = (date.fromisoformat(c['expiry'])-date.fromisoformat(day)).days
        if c['symbol'] != symbol or c['kind'] != kind or not 21 <= dte <= 45:
            continue
        k2 = c['strike_cents']*2
        if not spot_twice*4 <= k2*5 or not k2*5 <= spot_twice*6:
            continue
        candidates.append(c)
    if not candidates:
        return None
    expiry = min({c['expiry'] for c in candidates},
                 key=lambda e:(abs((date.fromisoformat(e)-date.fromisoformat(day)).days-30),e))
    candidates = [c for c in candidates if c['expiry'] == expiry
                  and ((c['strike_cents']*2*target_den <= spot_twice*target_num) if kind=='put'
                       else (c['strike_cents']*2*target_den >= spot_twice*target_num))]
    return min(candidates, key=lambda c:((-c['strike_cents'] if kind=='put' else c['strike_cents']),c['id'])) if candidates else None


def stock_samples(capture_paths):
    result = {}
    for path in capture_paths:
        receipt, rows = receipt_rows(path)
        symbol = receipt['query']['params']['symbol']
        for row in rows:
            key = symbol, row['timestamp']
            require(key not in result or result[key] == row, 'conflicting same-bucket stock rows')
            result[key] = row
    return result
