#!/usr/bin/env python3
"""Read-only free price estimate for frozen 669 book ops (metadata.get_cost only)."""
import argparse, base64, concurrent.futures, hashlib, importlib.util, json, os, re, stat, sys
from datetime import datetime, timezone
from decimal import Decimal, ROUND_CEILING, InvalidOperation, localcontext
from pathlib import Path
import urllib.error, urllib.parse, urllib.request

SEALED_RUNNER_SHA = "4529d7ed56bcaf4187af653c01567ac85c424e04892566ee605dbc058e8fd984"
OPTION_MANIFEST_SHA = "ccf48c229ce29db62e0d4aaa20cb06c733fdf4e528cf2d79f4da0e71a00adb2b"
STOCK_MANIFEST_SHA = "25056b1d6d41d3a97e9ffb7700cf157b3f195a1eff085b1bc8f2f5a4ba9b5333"
BOOK_BASE = Path("/home/yuduo/Projects/AutoWheel-research")
SOURCE_ROOT = BOOK_BASE / "source-afa3075"
OPTION_MANIFEST = BOOK_BASE / "cycle24-selective-acquisition-queries-20261006/manifest.json"
STOCK_MANIFEST = BOOK_BASE / "cycle27-stock-daily-acquisition-queries-20261006/manifest.json"
KEY_FILE = Path("/home/yuduo/.config/autowheel/databento-pilot.key")
JOURNAL = Path("/home/yuduo/.local/share/autowheel-research/databento-budget.json")
COST_URL = "https://hist.databento.com/v0/metadata.get_cost"
COST_MODE = "historical"
MAX_INT64 = 2 ** 63 - 1
NAME_RE = re.compile(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,254}\Z")


def unique(pairs):
    out = {}
    for k, v in pairs:
        if k in out:
            raise ValueError("duplicate JSON key")
        out[k] = v
    return out


def encode(value):
    return (json.dumps(value, indent=2, sort_keys=True) + "\n").encode()


def save_raw(path, raw):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, "wb") as f:
        f.write(raw)
        f.flush()
        os.fsync(f.fileno())
    fd = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


def parse_cost(body):
    try:
        val = json.loads(body, parse_float=Decimal, parse_int=Decimal, parse_constant=lambda _: (_ for _ in ()).throw(ValueError("nonfinite cost")))
    except (ValueError, InvalidOperation):
        raise ValueError("metadata cost invalid")
    if isinstance(val, bool):
        raise ValueError("metadata cost invalid")
    if isinstance(val, Decimal):
        raw, cost = str(val), val
    elif isinstance(val, int):
        raw, cost = str(val), Decimal(val)
    elif isinstance(val, str):
        try:
            raw, cost = val, Decimal(val)
        except (ValueError, InvalidOperation):
            raise ValueError("metadata cost invalid") from None
    else:
        raise ValueError("metadata cost invalid")
    if not cost.is_finite() or cost < 0:
        raise ValueError("metadata cost invalid")
    return raw, cost


def cost_nano(cost):
    if not cost.is_finite() or cost < 0 or cost > Decimal(MAX_INT64).scaleb(-9):
        raise ValueError("metadata cost exceeds representable bound")
    if cost.is_zero():
        return 0
    if cost < Decimal("0.000000001"):
        return 1
    with localcontext() as context:
        context.prec = max(28, len(cost.as_tuple().digits) + 10)
        nano = int(cost.scaleb(9).to_integral_value(rounding=ROUND_CEILING))
    if not 0 <= nano <= MAX_INT64:
        raise ValueError("metadata cost exceeds representable bound")
    return nano


def sealed_scope():
    runner = Path(__file__).with_name("acquire-prefetched-book-plan.py")
    if runner.is_symlink() or hashlib.sha256(runner.read_bytes()).hexdigest() != SEALED_RUNNER_SHA:
        raise ValueError("pinned sealed book runner differs")
    spec = importlib.util.spec_from_file_location("sealed_book_plan", runner)
    mod = importlib.util.module_from_spec(spec)
    old_path = sys.path[:]
    try:
        sys.path.insert(0, str(runner.resolve().parent))
        spec.loader.exec_module(mod)
    finally:
        sys.path[:] = old_path
    old = mod.load_old_controller()
    return mod, old.load_preflight()


def main():
    ap = argparse.ArgumentParser(description="Free read-only quote of the frozen 669 book plan.")
    ap.add_argument("--base", type=Path, required=True)
    ap.add_argument("--source-root", type=Path, required=True)
    ap.add_argument("--output", type=Path, required=True)
    a = ap.parse_args()
    for p in (a.base, a.source_root, a.output):
        if not p.is_absolute() or p != Path(os.path.normpath(p)):
            raise ValueError("absolute clean paths required")
    if a.base != BOOK_BASE or a.source_root != SOURCE_ROOT:
        raise ValueError("research base directories required")
    if a.output.parent != a.base or not a.output.name.startswith("cycle") or NAME_RE.fullmatch(a.output.name) is None or a.output.exists() or a.output.resolve() != a.output:
        raise ValueError("new cycle output namespace within base required")
    mod, preflight = sealed_scope()
    for d in (a.base, a.source_root):
        li = d.lstat()
        if d.resolve() != d or not stat.S_ISDIR(li.st_mode) or li.st_uid != os.getuid() or stat.S_IMODE(li.st_mode) != 0o700:
            raise ValueError("private owned directory required")
    key = preflight.private(KEY_FILE).decode("ascii").strip()
    if not key or ":" in key or "\n" in key:
        raise ValueError("credential invalid")
    journal_before = preflight.private(JOURNAL)
    jb = json.loads(journal_before, object_pairs_hook=unique)
    if jb.get("version") != 2 or jb.get("authorization_mode") != "uncapped" or jb.get("lifetime_nano") != 0:
        raise ValueError("uncapped v2 journal required")
    rows = jb.get("reservations", [])
    if len({r["operation_id"] for r in rows}) != len(rows):
        raise ValueError("duplicate financial operation")
    by_op = {r["operation_id"]: r for r in rows}
    book_items, _, _ = mod.collect_book_operations(preflight, OPTION_MANIFEST, STOCK_MANIFEST)
    if len(book_items) != 669:
        raise ValueError("full book scope differs")
    batch_go = a.source_root / "internal/databento/batch.go"
    batch_sha = hashlib.sha256(batch_go.read_bytes()).hexdigest()
    kind_of, acquired, fresh, unknown = {}, [], [], []
    for kind, item, path, op, qh in book_items:
        kind_of[op] = (kind, item, path, qh)
        r = by_op.get(op)
        if r is None:
            fresh.append(op)
        elif r.get("status") == "acquired":
            acquired.append(op)
        else:
            unknown.append(op)
    book_items.sort(key=lambda e: (e[1]["date"], e[0] != "stock-mbp-1", e[1]["filename"]))
    a.output.mkdir(mode=0o700)
    save_raw(a.output / "journal-before.private.json", journal_before)
    auth = "Basic " + base64.b64encode((key + ":").encode()).decode()
    del key
    def one(entry):
        kind, item, path, op, qh = entry
        stem = item["filename"].removesuffix(".json")
        receipt_p = a.output / (stem + ".cost.json")
        body_p = a.output / (stem + ".response.private.json")
        if receipt_p.exists():
            rc = json.loads(preflight.private(receipt_p), object_pairs_hook=unique)
            bd = preflight.private(body_p)
            if rc.get("query_file_sha256") != item["sha256"] or rc.get("operation_id") != op or rc.get("canonical_query_sha256") != qh or rc.get("http_status") != 200 or hashlib.sha256(bd).hexdigest() != rc.get("response_sha256") or rc.get("informational_not_issued_purchase_quote") is not True:
                raise ValueError("existing metadata receipt differs")
            return {"ok": True, "reused": True, "kind": kind, "item": item, "op": op, "nano": int(rc["cost_nanodollars_rounded_up"]), "cost": Decimal(rc["raw_cost_usd"]), "receipt": rc}
        if body_p.exists():
            raise ValueError("prior incomplete metadata response requires explicit disposition")
        q = preflight.read_json(path)
        form = {"dataset": q["dataset"], "schema": q["schema"], "symbols": ",".join(q["symbols"]), "stype_in": q["stype_in"], "start": q["start"], "end": q["end"], "mode": COST_MODE}
        req = urllib.request.Request(COST_URL, data=urllib.parse.urlencode(form).encode(), headers={"Authorization": auth, "Content-Type": "application/x-www-form-urlencoded"}, method="POST")
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
        try:
            try:
                resp = opener.open(req, timeout=45)
            except urllib.error.HTTPError as e:
                resp = e
            with resp:
                st, body = resp.status, resp.read(8193)
        except (urllib.error.URLError, TimeoutError, OSError):
            return {"ok": False, "kind": kind, "item": item, "op": op, "failure": "transport_failure_no_response"}
        if len(body) > 8192:
            raise ValueError("metadata response exceeds bound")
        save_raw(body_p, body)
        bh = hashlib.sha256(body).hexdigest()
        if st != 200:
            return {"ok": False, "kind": kind, "item": item, "op": op, "failure": "http_failure", "http_status": st, "response_sha256": bh, "response_bytes": len(body)}
        try:
            raw, cost = parse_cost(body)
            nano = cost_nano(cost)
        except ValueError:
            return {"ok": False, "kind": kind, "item": item, "op": op, "failure": "cost_parse_failure", "response_sha256": bh, "response_bytes": len(body)}
        rc = {"query_file": item["filename"], "query_file_sha256": item["sha256"], "query_bytes": item["bytes"], "canonical_query_sha256": qh, "operation_id": op, "kind": kind, "http_status": st, "endpoint": "/metadata.get_cost", "raw_cost_usd": raw, "cost_nanodollars_rounded_up": nano, "response_sha256": bh, "response_bytes": len(body), "observed_at": datetime.now(timezone.utc).isoformat(), "informational_not_issued_purchase_quote": True, "actual_invoice_cost_verified": False}
        save_raw(receipt_p, encode(rc))
        return {"ok": True, "reused": False, "kind": kind, "item": item, "op": op, "nano": nano, "cost": cost, "receipt": rc}
    done, failed, reused, total_all, total_fresh = [], [], 0, Decimal(0), Decimal(0)
    nano_all, nano_fresh = 0, 0
    per_schema_all, per_schema_fresh = {}, {}
    with concurrent.futures.ThreadPoolExecutor(max_workers=3) as pool:
        futs = {pool.submit(one, e): e for e in book_items}
        n = 0
        for f in concurrent.futures.as_completed(futs):
            r = f.result()
            n += 1
            if r["ok"]:
                if r["reused"]:
                    reused += 1
                done.append(r)
                total_all += r["cost"]
                nano_all += r["nano"]
                if nano_all > MAX_INT64:
                    raise ValueError("aggregated estimate exceeds bound")
                s = "mbp-1" if r["kind"] == "stock-mbp-1" else "cmbp-1"
                g = per_schema_all.setdefault(s, {"queries": 0, "usd": Decimal(0), "nano": 0})
                g["queries"] += 1
                g["usd"] += r["cost"]
                g["nano"] += r["nano"]
                if r["op"] in fresh:
                    total_fresh += r["cost"]
                    nano_fresh += r["nano"]
                    h = per_schema_fresh.setdefault(s, {"queries": 0, "usd": Decimal(0), "nano": 0})
                    h["queries"] += 1
                    h["usd"] += r["cost"]
                    h["nano"] += r["nano"]
            else:
                failed.append({k: r[k] for k in ("kind", "item", "op", "failure", "http_status", "response_sha256", "response_bytes") if k in r} | {"query_file": r["item"]["filename"]})
            if n % 25 == 0 or n == len(book_items):
                print(json.dumps({"quoted_completions": n, "required": 669, "priced_ok": len(done), "failed": len(failed)}), flush=True)
    rebind_ok = True
    for kind, item, path, op, qh in book_items:
        raw = preflight.private(path)
        if len(raw) != item["bytes"] or hashlib.sha256(raw).hexdigest() != item["sha256"]:
            rebind_ok = False
    journal_after = preflight.private(JOURNAL)
    journal_equal = journal_after == journal_before
    complete = len(done) == 669 and not failed and rebind_ok
    # Display the checked integer sums; each query is rounded up by <1 nanodollar.
    for g in (*per_schema_all.values(), *per_schema_fresh.values()):
        g["usd"] = str(Decimal(g["nano"]).scaleb(-9))
    total_all = Decimal(nano_all).scaleb(-9)
    total_fresh = Decimal(nano_fresh).scaleb(-9)
    summary = {"schema": "AutoWheelFullBookPricingReadOnlyV1", "option_manifest_sha256": OPTION_MANIFEST_SHA, "stock_manifest_sha256": STOCK_MANIFEST_SHA, "sealed_runner_sha256": SEALED_RUNNER_SHA, "batch_go_sha256": batch_sha, "cost_mode": COST_MODE, "required_queries": 669, "priced_queries": len(done), "complete_estimate": complete, "existing_responses_reused": reused, "quoted_total_or_subtotal_usd": str(total_all), "quoted_total_nanodollars_ceiling": nano_all, "remaining_fresh_usd": str(total_fresh), "remaining_fresh_nanodollars_ceiling": nano_fresh, "per_schema_full": per_schema_all, "per_schema_remaining_fresh": per_schema_fresh, "already_acquired": len(acquired), "never_attempted_fresh": len(fresh), "reserved_unknown": len(unknown), "journal_before_sha256": hashlib.sha256(journal_before).hexdigest(), "journal_after_sha256": hashlib.sha256(journal_after).hexdigest(), "journal_bytes_equal": journal_equal, "journal_mutated_by_this_tool": False, "query_rebind_ok": rebind_ok, "failures": failed, "actual_invoice_cost_verified": False, "credit_balance_verified": False, "source_qualified": False, "historical_performance_available": False, "paid_requests": 0, "financial_writes": 0, "untouched_files": [str(OPTION_MANIFEST), str(STOCK_MANIFEST), str(JOURNAL), str(KEY_FILE)], "limitations": "Informational metadata.get_cost estimate only; not an issued Go quote or purchase approval; partial sums are subtotals, never complete estimates; invoice and credit balance unverified; no source qualification or P&L.", "observed_at": datetime.now(timezone.utc).isoformat()}
    save_raw(a.output / "summary.json", encode(summary))
    pub = {k: v for k, v in summary.items() if k != "failures"}
    pub["failure_count"] = len(failed)
    pub["failure_kinds"] = sorted({f["failure"] for f in failed})
    print(json.dumps(pub), flush=True)


if __name__ == "__main__":
    try:
        main()
    except Exception as e:
        print(json.dumps({"status": "stopped", "error_type": type(e).__name__, "inspect_private_evidence": True}), flush=True)
        sys.exit(1)
