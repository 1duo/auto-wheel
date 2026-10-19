#!/usr/bin/env python3
"""Independent RD1 money/journal verifier.

Scope: independent financial cash/NAV/PnL reconciliation only.  This is not
full research completion and does not qualify original source units or source
publication chronology, actual broker fees or fills, option deliverables,
scheduled decisions or strategy rules, or settlement/availability timing.
It reads hash-verified Record JSON lines and the final ReplayResult JSON,
reconstructs cash, fees, receivables, 100-share lots and one short per symbol
from Effect actions with exact bounded integers, and never writes files.
"""

import argparse
import hashlib
import json
import os
import re
import stat
from datetime import date, datetime, timedelta, timezone

SEED_CENTS = 8_404_717
MAX_MONEY = 100_000_000_000
MAX_PRICE = MAX_MONEY // 100
MAX_FEE = 10_000
MAX_JOURNAL_BYTES = 32 * 1024 * 1024
MAX_RESULT_BYTES = 4 * 1024 * 1024
MAX_LINE_BYTES = 64 * 1024
MAX_RECORDS = 100_000
MAX_ERRORS = 40
MAX_BOOKS = 256
MAX_DIAGNOSTICS = 128

SCOPE = ("independent financial cash/NAV/PnL reconciliation only; "
         "not full research completion")
UNVERIFIED = [
    "original source units and source-publication chronology",
    "actual broker fees, fills and execution quality",
    "option deliverables and contract terms beyond journal identity fields",
    "scheduled decisions, session calendars and strategy rules",
    "settlement, cash-availability and share-availability timing",
]
_ACTIONS = {"sell_put", "sell_call", "buy_stock", "close_short", "resolve",
            "dividend", "pay_dividend", "mark"}
_RECORD_KEYS = {"sequence", "effect", "cash_delta_cents", "cash_after_cents",
                "receivable_delta_cents", "fees_after_cents", "mark"}
_EFFECT_KEYS = {"ref", "action", "symbol", "at", "signal_at", "profile",
                "contract", "price_cents", "fee_cents",
                "source_packet_sha256", "outcome", "notice_at", "payment_at",
                "available_at", "dividend_id"}
_CONTRACT_KEYS = {"id", "symbol", "expiry", "kind", "strike_cents"}
_SHA_RE = re.compile(r"^[0-9a-f]{64}$")
_SYMBOL_RE = re.compile(r"^[A-Z]{1,5}$")
_TIME_RE = re.compile(
    r"^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})"
    r"(?:\.(\d{1,9}))?(Z|[+-]\d{2}:\d{2})$")
_DATE_RE = re.compile(r"^(\d{4})-(\d{2})-(\d{2})$")
_MISSING = object()


class _Fail(Exception):
    pass


class _Errors:
    def __init__(self):
        self.items = []
        self.count = 0

    def add(self, message):
        self.count += 1
        if len(self.items) < MAX_ERRORS:
            self.items.append(str(message)[:240])


def _parse_time(value):
    if not isinstance(value, str):
        return None
    match = _TIME_RE.match(value)
    if not match:
        return None
    year, month, day, hour, minute, second = (
        int(match.group(i)) for i in range(1, 7))
    fraction = match.group(7) or ""
    if fraction[6:].strip("0"):
        return None  # Do not collapse distinct nanosecond clocks to one instant.
    frac = fraction[:6].ljust(6, "0")
    zone = match.group(8)
    try:
        if zone == "Z":
            tz = timezone.utc
        else:
            sign = 1 if zone[0] == "+" else -1
            tz = timezone(sign * timedelta(hours=int(zone[1:3]),
                                           minutes=int(zone[4:6])))
        return datetime(year, month, day, hour, minute, second,
                        int(frac or 0), tzinfo=tz)
    except ValueError:
        return None


def _valid_date(value):
    if not isinstance(value, str):
        return False
    match = _DATE_RE.match(value)
    if not match:
        return False
    try:
        date(int(match.group(1)), int(match.group(2)), int(match.group(3)))
    except ValueError:
        return False
    return True


def _int_field(obj, key, low, high, errors, label, default=_MISSING):
    if key not in obj:
        if default is _MISSING:
            errors.add("%s: missing %s" % (label, key))
            return 0
        return default
    value = obj[key]
    if type(value) is not int or not low <= value <= high:
        errors.add("%s: %s outside bounded integer range" % (label, key))
        return value if type(value) is int else 0
    return value


def _checked(value, errors, label):
    if value < -MAX_MONEY or value > MAX_MONEY:
        errors.add("%s: checked money envelope exceeded" % label)
    return value


def _read_bounded(path, limit, label):
    try:
        path = os.fspath(path)
    except TypeError:
        raise _Fail("%s path is invalid" % label)
    if not isinstance(path, str):
        raise _Fail("%s path is invalid" % label)
    try:
        info = os.lstat(path)
    except OSError as exc:
        raise _Fail("%s unreadable: %s" % (label, exc.strerror or "error"))
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
        raise _Fail("%s is not a regular non-symlink file" % label)
    if info.st_size > limit:
        raise _Fail("%s exceeds size bound" % label)
    flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0)
    try:
        with os.fdopen(os.open(path, flags), "rb") as handle:
            data = handle.read(limit + 1)
    except OSError as exc:
        raise _Fail("%s unreadable: %s" % (label, exc.strerror or "error"))
    if len(data) > limit:
        raise _Fail("%s exceeds size bound" % label)
    return data


def _check_hash(data, expected, label):
    if not isinstance(expected, str) or not _SHA_RE.match(expected):
        raise _Fail("%s expected SHA256 is malformed" % label)
    if hashlib.sha256(data).hexdigest() != expected:
        raise _Fail("%s SHA256 mismatch" % label)


def _no_dup(pairs):
    out = {}
    for key, value in pairs:
        if key in out:
            raise ValueError("duplicate key %r" % key)
        out[key] = value
    return out


def _bad_number(value):
    raise ValueError("float/nonfinite number rejected: %s" % value)


def _parse_int(text):
    if len(text.lstrip("-")) > 18:
        raise ValueError("integer literal too large")
    return int(text)


def _loads(text):
    return json.loads(text, object_pairs_hook=_no_dup,
                      parse_float=_bad_number, parse_constant=_bad_number,
                      parse_int=_parse_int)


def _parse_journal(data):
    try:
        text = data.decode("utf-8")
    except UnicodeDecodeError:
        raise _Fail("journal is not UTF-8")
    if "\x00" in text:
        raise _Fail("journal contains NUL")
    lines = text.splitlines()
    if not lines or len(lines) > MAX_RECORDS:
        raise _Fail("journal record count outside bound")
    records = []
    for number, line in enumerate(lines, 1):
        if not line.strip() or len(line.encode("utf-8")) > MAX_LINE_BYTES:
            raise _Fail("journal line %d outside bound" % number)
        try:
            record = _loads(line)
        except (ValueError, TypeError) as exc:
            raise _Fail("journal line %d invalid JSON: %s" % (number, exc))
        if not isinstance(record, dict):
            raise _Fail("journal line %d is not an object" % number)
        records.append(record)
    return records


def _parse_result(data):
    try:
        text = data.decode("utf-8")
    except UnicodeDecodeError:
        raise _Fail("result is not UTF-8")
    if "\x00" in text or not text.strip():
        raise _Fail("result is empty or contains NUL")
    try:
        result = _loads(text)
    except (ValueError, TypeError) as exc:
        raise _Fail("result invalid JSON: %s" % exc)
    if not isinstance(result, dict):
        raise _Fail("result is not an object")
    return result


def _contract(raw, errors, label):
    if not isinstance(raw, dict) or set(raw) - _CONTRACT_KEYS:
        errors.add("%s: malformed contract" % label)
        return None
    strike = _int_field(raw, "strike_cents", 1, MAX_PRICE, errors, label)
    out = {"id": raw.get("id"), "symbol": raw.get("symbol"),
           "expiry": raw.get("expiry"), "kind": raw.get("kind"),
           "strike_cents": strike}
    if (not isinstance(out["id"], str) or not 1 <= len(out["id"]) <= 128
            or not isinstance(out["symbol"], str)
            or not _SYMBOL_RE.match(out["symbol"])
            or not _valid_date(out["expiry"])
            or out["kind"] not in ("put", "call")):
        errors.add("%s: malformed contract identity" % label)
        return None
    return out


def _profile(raw, errors, label):
    if not isinstance(raw, dict) or set(raw) - {"mode", "exit"}:
        errors.add("%s: malformed profile" % label)
        return None
    out = {"mode": raw.get("mode"), "exit": raw.get("exit")}
    if (out["mode"] not in ("conservative", "balanced", "aggressive")
            or out["exit"] not in ("hold", "tp50", "tp75", "tp90", "puttp50", "puttp75", "puttp90")):
        errors.add("%s: malformed profile" % label)
        return None
    return out


def _book(raw):
    if not isinstance(raw, dict) or set(raw) - {"bid_cents", "ask_cents"}:
        return None
    bid, ask = raw.get("bid_cents"), raw.get("ask_cents")
    if type(bid) is not int or type(ask) is not int:
        return None
    if not 0 <= bid <= MAX_PRICE or not 0 <= ask <= MAX_PRICE:
        return None
    return bid, ask


def _sale_view(raw, errors, label):
    if not isinstance(raw, dict):
        errors.add("%s: malformed option sale" % label)
        return None
    resolved = raw.get("resolved_at")
    if resolved is not None and _parse_time(resolved) is None:
        errors.add("%s: malformed resolution clock" % label)
    return {
        "contract": _contract(raw.get("contract"), errors, label),
        "opening_ref": raw.get("opening_ref"),
        "opening_net_credit_cents": _int_field(
            raw, "opening_net_credit_cents", -MAX_MONEY, MAX_MONEY,
            errors, label),
        "opening_fee_cents": _int_field(
            raw, "opening_fee_cents", 0, MAX_FEE, errors, label),
        "opened_at": _parse_time(raw.get("opened_at")),
        "resolved_at": _parse_time(resolved) if resolved is not None else None,
        "outcome": raw.get("outcome", ""),
        "close_cost_cents": _int_field(
            raw, "close_cost_cents", 0, MAX_MONEY, errors, label),
    }


def _ratio(numerator, denominator, num_key, den_key, zero_reason):
    return {num_key: numerator, den_key: denominator,
            "ratio": ("%d/%d" % (numerator, denominator)) if denominator else None,
            "reason": None if denominator else zero_reason}


def _pending_intents(result, cutoff, used, positions, sales, errors):
    """Unexecuted future plans have no cash, fee, position or PnL effect.

    Exact next-session clocks and source-time decisions need the packet audits;
    monetary framing alone cannot establish those facts.
    """
    pending=result.get('pending_attempts')
    if pending is None or pending==[]:return 0
    if (not isinstance(pending,list) or len(pending)>13
            or result.get('sensitivity')!='execution_next_session' or cutoff is None):
        errors.add('nonempty pending intents require the separately bound next-session model');return 0
    allowed={'ref','action','symbol','at','signal_at','profile','contract',
             'price_cents','fee_cents','source_packet_sha256'}
    seen=set();symbols=set()
    for item in pending:
        if not isinstance(item,dict) or set(item)-allowed:
            errors.add('pending intent contains an unsupported effect or lifecycle field');continue
        ref=item.get('ref');symbol=item.get('symbol');action=item.get('action')
        signal=_parse_time(item.get('signal_at'));future=_parse_time(item.get('at'))
        if (not isinstance(ref,str) or not 1<=len(ref)<=128 or ref in used or ref in seen
                or not isinstance(symbol,str) or not _SYMBOL_RE.fullmatch(symbol) or symbol in symbols
                or action not in ('sell_put','sell_call','buy_stock','close_short')
                or signal is None or future is None or not signal<=cutoff<future
                or not isinstance(item.get('source_packet_sha256'),str)
                or not _SHA_RE.fullmatch(item['source_packet_sha256'])):
            errors.add('pending intent is duplicated, already used, malformed or not beyond the observed cutoff');continue
        seen.add(ref);symbols.add(symbol)
        if (_int_field(item,'price_cents',0,MAX_PRICE,errors,'pending intent')!=0
                or _int_field(item,'fee_cents',0,MAX_FEE,errors,'pending intent')!=0):
            errors.add('unexecuted pending intent carries a financial price or fee')
        contract=_contract(item.get('contract'),errors,'pending intent')
        profile=_profile(item.get('profile'),errors,'pending intent');p=positions.get(symbol)
        if contract is None or profile is None:continue
        if contract['symbol']!=symbol:errors.add('pending intent contract symbol differs')
        if action=='sell_put':
            if p is not None or contract['kind']!='put' or profile['mode']=='aggressive':
                errors.add('pending put is incompatible with the observed lifecycle')
        elif action=='buy_stock':
            if p is not None or contract['kind']!='call' or profile['mode']!='aggressive':
                errors.add('pending stock entry is incompatible with the observed lifecycle')
        elif action=='sell_call':
            if not p or p['shares']!=100 or p['short_ref'] is not None or profile!=p['profile'] or contract['kind']!='call':
                errors.add('pending call is incompatible with owned stock')
        elif (not p or p['short_ref'] is None or sales[p['short_ref']]['contract']!=contract
              or profile!=p['profile'] or profile['exit']=='hold'):
            errors.add('pending close has no matching managed held short')
    return len(pending)


def _verify(journal_path, expected_journal_sha256, result_path,
           expected_result_sha256):
    """Verify RD1 financial reconciliation; return a compact report dict."""
    errors = _Errors()
    report = {"scope": SCOPE, "unverified": list(UNVERIFIED),
              "status": "failed", "qualified": False,
              "financial_checks_passed": False,
              "journal_sha256_verified": False,
              "result_sha256_verified": False,
              "result_claim_relied_upon": False,
              "records_checked": 0, "marks_checked": 0,
              "near_close_marks_checked": 0, "metrics": None,
              "result_status": None, "result_diagnostic_counts": {},
              "result_independent_financial_verification_claim": None,
              "errors": [], "error_count": 0, "errors_truncated": False}
    fatal = False
    try:
        journal_data = _read_bounded(journal_path, MAX_JOURNAL_BYTES, "journal")
        _check_hash(journal_data, expected_journal_sha256, "journal")
        report["journal_sha256_verified"] = True
        result_data = _read_bounded(result_path, MAX_RESULT_BYTES, "result")
        _check_hash(result_data, expected_result_sha256, "result")
        report["result_sha256_verified"] = True
        records = _parse_journal(journal_data)
        result = _parse_result(result_data)
    except _Fail as exc:
        errors.add(str(exc))
        fatal = True
        records, result = [], {}
    except Exception as exc:  # fail closed, with bounded diagnostics
        errors.add("verifier input failure: %s" % type(exc).__name__)
        fatal = True
        records, result = [], {}

    cash, fees, receivables = SEED_CENTS, 0, 0
    realized_stock, received_dividends = 0, 0
    positions, sales, dividends = {}, {}, {}
    refs = set()
    last_at = None
    marks_checked = near_close_checked = 0
    last_near_close = None
    mark_infos = []

    if not fatal:
        for index, record in enumerate(records, 1):
            label = "record %d" % index
            if set(record) - _RECORD_KEYS:
                errors.add("%s: unknown record fields" % label)
            sequence = _int_field(record, "sequence", 1, MAX_RECORDS,
                                  errors, label)
            if sequence != index:
                errors.add("%s: sequence is not contiguous from 1" % label)
            effect = record.get("effect")
            if not isinstance(effect, dict):
                errors.add("%s: effect is not an object" % label)
                continue
            if set(effect) - _EFFECT_KEYS:
                errors.add("%s: unknown effect fields" % label)
            ref = effect.get("ref")
            if not isinstance(ref, str) or not 1 <= len(ref) <= 128:
                errors.add("%s: malformed ref" % label)
                ref = ""
            if ref in refs:
                errors.add("%s: duplicate ref" % label)
            refs.add(ref)
            action = effect.get("action")
            if action not in _ACTIONS:
                errors.add("%s: unsupported action" % label)
                action = ""
            at = _parse_time(effect.get("at"))
            if at is None:
                errors.add("%s: effect.at is not an aware timestamp" % label)
            else:
                if last_at is not None and at < last_at:
                    errors.add("%s: effect.at is not monotonic" % label)
                last_at = at
            source = effect.get("source_packet_sha256")
            if not isinstance(source, str) or not _SHA_RE.match(source):
                errors.add("%s: malformed source SHA" % label)
            symbol = effect.get("symbol", "")
            price = _int_field(effect, "price_cents", 0, MAX_PRICE,
                               errors, label)
            fee = _int_field(effect, "fee_cents", 0, MAX_FEE,
                             errors, label)
            delta = receivable_delta = 0

            if action == "mark":
                marks_checked += 1
                if symbol or price or fee:
                    errors.add("%s: mark effect carries money fields" % label)
                mark = record.get("mark")
                nav = None
                if not isinstance(mark, dict):
                    errors.add("%s: mark sample missing" % label)
                else:
                    stocks = mark.get("stock_books")
                    options = mark.get("option_books")
                    # Go encodes a nil map as null. Empty accounts need no
                    # market books; a held position still requires its exact key.
                    if stocks is None:
                        stocks = {}
                    if options is None:
                        options = {}
                    if (not isinstance(stocks, dict)
                            or not isinstance(options, dict)
                            or len(stocks) > MAX_BOOKS
                            or len(options) > MAX_BOOKS):
                        errors.add("%s: mark books outside bound" % label)
                        stocks, options = {}, {}
                    stock_bids, option_asks = {}, {}
                    nav = _checked(cash + receivables, errors, label)
                    for sym in sorted(positions):
                        pos = positions[sym]
                        if pos["shares"]:
                            book = _book(stocks.get(sym))
                            if book is None or book[0] <= 0 or book[1] < book[0]:
                                errors.add("%s: held stock book missing or "
                                           "not valid noncrossed positive "
                                           "sides for %s" % (label, sym))
                                nav = None
                            else:
                                stock_bids[sym] = book[0]
                                if nav is not None:
                                    nav = _checked(nav + 100 * book[0],
                                                   errors, label)
                        if pos["short_ref"] is not None:
                            sale = sales[pos["short_ref"]]
                            cid = sale["contract"]["id"]
                            book = _book(options.get(cid))
                            # A positive crossed ask remains a liability.
                            if book is None or book[1] <= 0:
                                errors.add("%s: held option ask missing or "
                                           "not positive for %s" % (label, cid))
                                nav = None
                            else:
                                option_asks[cid] = book[1]
                                if nav is not None:
                                    nav = _checked(nav - 100 * book[1],
                                                   errors, label)
                    stated_nav = _int_field(mark, "NAV_cents", -MAX_MONEY,
                                            MAX_MONEY, errors, label, None)
                    if nav is not None and stated_nav != nav:
                        errors.add("%s: mark NAV differs from independent "
                                   "valuation" % label)
                    mark_infos.append({
                        "index": index, "at": at, "nav": nav, "ref": ref,
                        "stock_bids": stock_bids, "option_asks": option_asks})
            else:
                if not isinstance(symbol, str) or not _SYMBOL_RE.match(symbol or ""):
                    errors.add("%s: malformed symbol" % label)
                    symbol = ""
                pos = positions.get(symbol)
                if action in ("sell_put", "sell_call"):
                    contract = _contract(effect.get("contract"), errors, label)
                    kind = "put" if action == "sell_put" else "call"
                    profile = _profile(effect.get("profile"), errors, label)
                    if contract is not None and (
                            contract["symbol"] != symbol
                            or contract["kind"] != kind):
                        errors.add("%s: contract does not match sale" % label)
                        contract = None
                    if price <= 0 or 100 * price <= fee:
                        errors.add("%s: opening credit not positive" % label)
                        contract = None
                    if action == "sell_put" and pos is not None:
                        errors.add("%s: put sale is not flat" % label)
                        contract = None
                    if action == "sell_call" and (
                            pos is None or pos["shares"] != 100
                            or pos["short_ref"] is not None):
                        errors.add("%s: call sale lacks owned uncovered "
                                   "100 shares" % label)
                        contract = None
                    if contract is not None and profile is not None:
                        delta = _checked(100 * price - fee, errors, label)
                        sales[ref] = {
                            "contract": contract, "opening_ref": ref,
                            "opening_net_credit_cents": delta,
                            "opening_fee_cents": fee, "opened_at": at,
                            "resolved_at": None, "outcome": "",
                            "close_cost_cents": 0}
                        if action == "sell_put":
                            pos = {"shares": 0, "stock_cost_cents": 0,
                                   "profile": profile, "short_ref": ref}
                            positions[symbol] = pos
                        else:
                            pos["short_ref"] = ref
                        if len(positions) > 3:
                            errors.add("%s: more than three managed symbols"
                                       % label)
                elif action == "buy_stock":
                    profile = _profile(effect.get("profile"), errors, label)
                    if pos is not None or price <= 0 or profile is None:
                        errors.add("%s: stock buy is not a flat priced entry"
                                   % label)
                    else:
                        cost = _checked(100 * price + fee, errors, label)
                        delta = -cost
                        positions[symbol] = {
                            "shares": 100, "stock_cost_cents": cost,
                            "profile": profile, "short_ref": None}
                        if len(positions) > 3:
                            errors.add("%s: more than three managed symbols"
                                       % label)
                elif action in ("close_short", "resolve"):
                    contract = _contract(effect.get("contract"), errors, label)
                    sale = None
                    if pos is None or pos["short_ref"] is None:
                        errors.add("%s: no open short to resolve" % label)
                    else:
                        sale = sales[pos["short_ref"]]
                        if contract != sale["contract"]:
                            errors.add("%s: contract does not match open "
                                       "short" % label)
                            sale = None
                    if sale is not None and action == "close_short":
                        if price <= 0:
                            errors.add("%s: close ask not positive" % label)
                        else:
                            delta = _checked(-(100 * price + fee),
                                             errors, label)
                            sale["resolved_at"], sale["outcome"] = at, "managed_close"
                            sale["close_cost_cents"] = -delta
                            pos["short_ref"] = None
                            if pos["shares"] == 0:
                                del positions[symbol]
                    elif sale is not None:
                        outcome = effect.get("outcome")
                        strike = sale["contract"]["strike_cents"]
                        if outcome == "expired":
                            if fee != 0 or price != 0:
                                errors.add("%s: expired short must have "
                                           "zero cash and fee" % label)
                            sale["resolved_at"], sale["outcome"] = at, outcome
                            pos["short_ref"] = None
                            if pos["shares"] == 0:
                                del positions[symbol]
                        elif outcome == "assigned":
                            if (sale["contract"]["kind"] != "put"
                                    or pos["shares"] != 0):
                                errors.add("%s: assignment lacks flat put"
                                           % label)
                            else:
                                delta = _checked(-(100 * strike + fee),
                                                 errors, label)
                                pos["shares"] = 100
                                pos["stock_cost_cents"] = -delta
                                sale["resolved_at"], sale["outcome"] = at, outcome
                                pos["short_ref"] = None
                        elif outcome == "called_away":
                            if (sale["contract"]["kind"] != "call"
                                    or pos["shares"] != 100):
                                errors.add("%s: call-away lacks 100 owned "
                                           "shares" % label)
                            else:
                                delta = _checked(100 * strike - fee,
                                                 errors, label)
                                realized_stock = _checked(
                                    realized_stock + delta
                                    - pos["stock_cost_cents"], errors, label)
                                pos["shares"] = 0
                                pos["stock_cost_cents"] = 0
                                sale["resolved_at"], sale["outcome"] = at, outcome
                                pos["short_ref"] = None
                                del positions[symbol]
                        else:
                            errors.add("%s: unsupported resolve outcome"
                                       % label)
                elif action == "dividend":
                    did = effect.get("dividend_id")
                    pay_at = _parse_time(effect.get("payment_at"))
                    avail_at = _parse_time(effect.get("available_at"))
                    if (pos is None or pos["shares"] != 100 or price <= 0
                            or fee != 0 or not isinstance(did, str)
                            or not 1 <= len(did) <= 128 or did in dividends
                            or pay_at is None or avail_at is None
                            or at is None or pay_at < at or avail_at < pay_at):
                        errors.add("%s: dividend entitlement is not exact "
                                   "for 100 owned shares" % label)
                    else:
                        receivable_delta = price
                        dividends[did] = {
                            "symbol": symbol, "amount_cents": price,
                            "payment_at": pay_at, "available_at": avail_at,
                            "paid": False}
                elif action == "pay_dividend":
                    did = effect.get("dividend_id")
                    div = dividends.get(did) if isinstance(did, str) else None
                    if (div is None or div["paid"] or div["symbol"] != symbol
                            or fee != 0 or price != div["amount_cents"]
                            or at != div["payment_at"]):
                        errors.add("%s: dividend payment does not transfer "
                                   "the exact entitlement" % label)
                    else:
                        delta = div["amount_cents"]
                        receivable_delta = -delta
                        div["paid"] = True
                        received_dividends = _checked(
                            received_dividends + delta, errors, label)

            cash = _checked(cash + delta, errors, label)
            if cash < 0:
                errors.add("%s: reconstructed cash is negative" % label)
            receivables = _checked(receivables + receivable_delta,
                                   errors, label)
            if receivables < 0:
                errors.add("%s: reconstructed receivables are negative" % label)
            fees = _checked(fees + fee, errors, label)
            if _int_field(record, "cash_delta_cents", -MAX_MONEY, MAX_MONEY,
                          errors, label) != delta:
                errors.add("%s: cash_delta differs from reconstruction" % label)
            if _int_field(record, "cash_after_cents", -MAX_MONEY, MAX_MONEY,
                          errors, label) != cash:
                errors.add("%s: cash_after differs from reconstruction" % label)
            if _int_field(record, "receivable_delta_cents", -MAX_MONEY,
                          MAX_MONEY, errors, label) != receivable_delta:
                errors.add("%s: receivable_delta differs from reconstruction"
                           % label)
            if _int_field(record, "fees_after_cents", 0, MAX_MONEY,
                          errors, label) != fees:
                errors.add("%s: fees_after differs from reconstruction" % label)
            if action != "mark" and record.get("mark") is not None:
                errors.add("%s: non-mark record carries a mark" % label)

    if not fatal:
        # RD1 primary replay explicitly labels near-close marks. An arbitrary
        # last mark of a day cannot qualify an omitted scheduled NAV sample.
        nav_marks = [m for m in mark_infos if m["ref"].startswith("NAV-")]
        nav_days = [m["ref"] for m in nav_marks]
        if len(nav_days) != len(set(nav_days)):
            errors.add("duplicate near-close session label")
        near_close_checked = len(nav_marks)
        if nav_marks:
            last_near_close = nav_marks[-1]

    report["records_checked"] = len(records) if not fatal else 0
    report["marks_checked"] = marks_checked
    report["near_close_marks_checked"] = near_close_checked

    result_status = result.get("status") if isinstance(result, dict) else None
    report["result_status"] = result_status if isinstance(result_status, str) else None
    claim = result.get("independent_financial_verification_complete")
    report["result_independent_financial_verification_claim"] = (
        claim if type(claim) is bool else None)
    raw_counts = result.get("diagnostic_counts")
    if isinstance(raw_counts, dict) and len(raw_counts) <= MAX_DIAGNOSTICS:
        counts = {}
        for key in sorted(raw_counts):
            value = raw_counts[key]
            if (isinstance(key, str) and len(key) <= 128
                    and type(value) is int and 0 <= value <= MAX_MONEY):
                counts[key] = value
            else:
                errors.add("result diagnostic_counts are malformed")
                break
        else:
            report["result_diagnostic_counts"] = counts
    elif raw_counts is not None:
        errors.add("result diagnostic_counts are malformed")

    final_nav = None
    if not fatal:
        if last_near_close is None or last_near_close["nav"] is None:
            errors.add("journal has no independently valued final near-close mark")
        else:
            final_nav = last_near_close["nav"]
            if last_near_close["index"] != len(records):
                errors.add("effects or marks occur after the final near-close cutoff")
        state = result.get("checkpoint_state")
        if not isinstance(state, dict):
            errors.add("result checkpoint_state is missing")
            state = {}
        if _int_field(state, "seed_cents", 0, MAX_MONEY, errors,
                      "final state") != SEED_CENTS:
            errors.add("final state seed differs from 8404717")
        if _int_field(state, "economic_cash_cents", -MAX_MONEY, MAX_MONEY,
                      errors, "final state") != cash:
            errors.add("final state cash differs from reconstruction")
        if _int_field(state, "fees_cents", 0, MAX_MONEY, errors,
                      "final state") != fees:
            errors.add("final state fees differ from reconstruction")
        if _int_field(state, "realized_stock_pnl_cents", -MAX_MONEY, MAX_MONEY,
                      errors, "final state") != realized_stock:
            errors.add("final state stock PnL differs from reconstruction")
        if _int_field(state, "sequence", 0, MAX_RECORDS, errors,
                      "final state") != len(records):
            errors.add("final state sequence differs from journal")
        state_positions = state.get("positions")
        if not isinstance(state_positions, dict) or len(state_positions) > 3:
            errors.add("final state positions outside bound")
            state_positions = {}
        if set(state_positions) != set(positions):
            errors.add("final state position symbols differ from reconstruction")
        for sym in sorted(set(positions) & set(state_positions)):
            raw_pos, pos = state_positions[sym], positions[sym]
            if not isinstance(raw_pos, dict):
                errors.add("final position %s is malformed" % sym)
                continue
            if (_int_field(raw_pos, "shares", 0, 100, errors,
                           "final position") != pos["shares"]
                    or pos["shares"] not in (0, 100)
                    or _int_field(raw_pos, "stock_acquisition_cents", 0,
                                  MAX_MONEY, errors, "final position")
                    != pos["stock_cost_cents"]
                    or _profile(raw_pos.get("opening_profile"), errors,
                                "final position") != pos["profile"]):
                errors.add("final position %s differs from reconstruction" % sym)
            raw_short = raw_pos.get("short")
            if pos["short_ref"] is None:
                if raw_short is not None:
                    errors.add("final position %s has an unexpected short" % sym)
            elif _sale_view(raw_short, errors, "final position short") != sales[pos["short_ref"]]:
                errors.add("final position %s short differs from reconstruction" % sym)
        state_sales = state.get("option_sales")
        if not isinstance(state_sales, dict) or set(state_sales) != set(sales):
            errors.add("final option sales differ from reconstruction")
            state_sales = {}
        for opening_ref in sorted(set(sales) & set(state_sales)):
            if _sale_view(state_sales[opening_ref], errors,
                          "final sale") != sales[opening_ref]:
                errors.add("final sale %s differs from reconstruction" % opening_ref)
        state_divs = state.get("dividends")
        if not isinstance(state_divs, dict) or set(state_divs) != set(dividends):
            errors.add("final dividends differ from reconstruction")
            state_divs = {}
        for did in sorted(set(dividends) & set(state_divs)):
            raw_div = state_divs[did]
            if not isinstance(raw_div, dict):
                errors.add("final dividend %s is malformed" % did)
                continue
            view = {"symbol": raw_div.get("symbol"),
                    "amount_cents": _int_field(raw_div, "amount_cents", 0,
                                               MAX_MONEY, errors,
                                               "final dividend"),
                    "payment_at": _parse_time(raw_div.get("payment_at")),
                    "available_at": _parse_time(raw_div.get("available_at")),
                    "paid": raw_div.get("paid")}
            if view != dividends[did] or type(view["paid"]) is not bool:
                errors.add("final dividend %s differs from reconstruction" % did)
        used = state.get("used_refs")
        if (not isinstance(used, dict)
                                 or set(used) != refs
                                 or any(value is not True for value in used.values())):
            errors.add("final used_refs differ from journal refs")

        sessions = _int_field(result, "sessions_marked", 0, MAX_RECORDS,
                              errors, "result")
        if sessions != near_close_checked or sessions <= 0:
            errors.add("result sessions_marked differs from near-close marks")
        stated_nav = result.get("last_near_close_NAV_cents")
        if type(stated_nav) is not int or stated_nav != final_nav:
            errors.add("result last near-close NAV differs from final mark")
        stated_at = _parse_time(result.get("last_near_close_at"))
        final_at = last_near_close["at"] if last_near_close else None
        if stated_at is None or stated_at != final_at:
            errors.add("result last near-close time differs from final mark")
        if _parse_time(state.get("at")) is None or _parse_time(state.get("at")) != final_at:
            errors.add("final state at does not equal its final near-close mark")
        report['censored_pending_attempts_count']=_pending_intents(result,final_at,refs,positions,sales,errors)
        report['pending_intents_have_no_financial_effect']=True
        report['pending_calendar_and_signal_qualification_performed']=False
        if result_status == "complete":
            profit = result.get("net_profit_cents")
            if (type(profit) is not int or final_nav is None
                    or profit != final_nav - SEED_CENTS):
                errors.add("complete result net profit differs from NAV minus seed")

    metrics = None
    if (not fatal and errors.count == 0 and result_status == "complete"
            and final_nav is not None and last_near_close is not None
            and last_near_close["index"] == len(records)):
        resolved = [s for s in sales.values() if s["outcome"]]
        resolved_net = sum(s["opening_net_credit_cents"] - s["close_cost_cents"]
                           for s in resolved)
        capture_cohort = [s for s in resolved
                          if s["outcome"] in ("managed_close", "expired")]
        capture_num = sum(s["opening_net_credit_cents"] - s["close_cost_cents"]
                          for s in capture_cohort)
        capture_den = sum(s["opening_net_credit_cents"] for s in capture_cohort)
        owned_mark = sum(100 * last_near_close["stock_bids"][sym]
                         - pos["stock_cost_cents"]
                         for sym, pos in positions.items() if pos["shares"])
        open_net = sum(
            sales[pos["short_ref"]]["opening_net_credit_cents"]
            - 100 * last_near_close["option_asks"][
                sales[pos["short_ref"]]["contract"]["id"]]
            for pos in positions.values() if pos["short_ref"] is not None)
        components = {
            "realized_stock_pnl_cents": realized_stock,
            "resolved_option_net_cents": resolved_net,
            "received_dividends_cents": received_dividends,
            "unpaid_receivables_cents": receivables,
            "owned_stock_mark_minus_cost_cents": owned_mark,
            "open_option_net_credit_minus_liability_cents": open_net,
        }
        identity = sum(components.values())
        profit = final_nav - SEED_CENTS
        if identity != profit:
            errors.add("PnL identity differs from NAV minus seed")
        puts = [s for s in resolved if s["contract"]["kind"] == "put"]
        calls = [s for s in resolved if s["contract"]["kind"] == "call"]
        realized_net = realized_stock + resolved_net + received_dividends
        metrics = {
            "seed_cents": SEED_CENTS,
            "final_near_close_nav_cents": final_nav,
            "net_profit_cents": profit,
            "pnl_identity_total_cents": identity,
            "pnl_identity_difference_cents": identity - profit,
            "components_cents": components,
            "cash_cents": cash, "fees_cents": fees,
            "receivables_cents": receivables,
            "credit_capture": _ratio(
                capture_num, capture_den, "numerator_cents",
                "denominator_cents",
                "no managed-closed or worthless-expired option sales"),
            "assignment_rate": _ratio(
                sum(1 for s in puts if s["outcome"] == "assigned"), len(puts),
                "numerator", "denominator", "no resolved put sales"),
            "callaway_rate": _ratio(
                sum(1 for s in calls if s["outcome"] == "called_away"),
                len(calls), "numerator", "denominator",
                "no resolved call sales"),
            "net_realized_share": _ratio(
                realized_net, profit if profit > 0 else 0,
                "numerator_cents", "denominator_cents",
                "total net profit is not positive"),
        }
        if profit <= 0:
            metrics["net_realized_share"]["denominator_cents"] = profit
    report["metrics"] = metrics
    report["metrics_unavailable_reason"] = (None if metrics is not None else
        "declared path incomplete or independent financial checks failed")

    report["financial_checks_passed"] = bool(
        not fatal and errors.count == 0 and final_nav is not None)
    if fatal:
        report["status"] = "failed"
    elif result_status != "complete":
        report["status"] = "insufficient"
    elif errors.count:
        report["status"] = "failed"
    else:
        report["status"] = "verified"
    report["qualified"] = report["status"] == "verified"
    if not report["qualified"]:
        report["metrics"] = None
        report["metrics_unavailable_reason"] = (
            "declared path incomplete or independent financial checks failed")
    report["errors"] = errors.items
    report["error_count"] = errors.count
    report["errors_truncated"] = errors.count > len(errors.items)
    return report


def verify(journal_path, expected_journal_sha256, result_path,
           expected_result_sha256):
    try:
        return _verify(journal_path, expected_journal_sha256, result_path,
                       expected_result_sha256)
    except Exception as exc:
        # Malformed nested structures must not escape as an unstructured crash,
        # publish partial metrics or inherit an upstream verification claim.
        return {"scope": SCOPE, "unverified": list(UNVERIFIED),
                "status": "failed", "qualified": False,
                "financial_checks_passed": False, "metrics": None,
                "metrics_unavailable_reason": "malformed verifier input",
                "errors": ["verifier rejected malformed input: "+type(exc).__name__],
                "error_count": 1, "errors_truncated": False}


def main(argv=None):
    parser = argparse.ArgumentParser(
        description="Independent RD1 financial cash/NAV/PnL reconciliation "
                    "only; not full research completion.")
    parser.add_argument("--journal", required=True)
    parser.add_argument("--journal-sha256", required=True)
    parser.add_argument("--result", required=True)
    parser.add_argument("--result-sha256", required=True)
    args = parser.parse_args(argv)
    report = verify(args.journal, args.journal_sha256, args.result,
                    args.result_sha256)
    print(json.dumps(report, sort_keys=True, separators=(",", ":")))
    return 0 if report["qualified"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
