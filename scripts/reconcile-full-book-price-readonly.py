#!/usr/bin/env python3
"""Reconcile frozen 669 full-book price; reuse ok, missing-only metadata.get_cost."""
import argparse, base64, hashlib, importlib.util, json, os, stat, sys
from datetime import datetime, timezone
from decimal import Decimal
from pathlib import Path
import urllib.error, urllib.parse, urllib.request
HELPER_SHA = "cc049922db7595660fa21e7d5289c02ab5adf2657a834d7b9325203f17b96fdc"
PARENT_NAME = "cycle51-full-book-price-readonly-20261007"
SCHEMA = "AutoWheelFullBookPricingReadOnlyV1"
def unique(pairs):
    o = {}
    for k, v in pairs:
        if k in o:
            raise ValueError("dup key")
        o[k] = v
    return o
def load_helper():
    p = Path(__file__).with_name("quote-full-book-plan-readonly.py")
    if p.is_symlink() or hashlib.sha256(p.read_bytes()).hexdigest() != HELPER_SHA:
        raise ValueError("pinned helper differs")
    s = importlib.util.spec_from_file_location("qfb_helper", p)
    m = importlib.util.module_from_spec(s)
    old = sys.path[:]
    try:
        sys.path.insert(0, str(p.resolve().parent))
        s.loader.exec_module(m)
    finally:
        sys.path[:] = old
    return m
def req_dir(d):
    li = d.lstat()
    if d.resolve() != d or not stat.S_ISDIR(li.st_mode) or li.st_uid != os.getuid() or stat.S_IMODE(li.st_mode) != 0o700:
        raise ValueError("private owned dir required")
def main():
    ap = argparse.ArgumentParser(description="Reconcile 669 book price read-only.")
    ap.add_argument("--output", type=Path, required=True)
    a = ap.parse_args()
    H = load_helper()
    if Path(__file__).resolve() != H.SOURCE_ROOT / 'scripts/reconcile-full-book-price-readonly.py':
        raise ValueError('fixed MSI script location required')
    if not a.output.is_absolute() or a.output != Path(os.path.normpath(a.output)):
        raise ValueError("absolute clean output required")
    if a.output.parent != H.BOOK_BASE or not a.output.name.startswith("cycle") or H.NAME_RE.fullmatch(a.output.name) is None or a.output.exists() or a.output.resolve() != a.output:
        raise ValueError("fresh cycle output in base required")
    for d in (H.BOOK_BASE, H.SOURCE_ROOT):
        req_dir(d)
    mod, pre = H.sealed_scope()
    parent = H.BOOK_BASE / PARENT_NAME
    req_dir(parent)
    if not parent.is_dir() or not (parent / "summary.json").is_file():
        raise ValueError("parent summary missing; first run not terminal")
    psum_raw = pre.private(parent / 'summary.json')
    psum = json.loads(psum_raw, object_pairs_hook=unique)
    anchors = {parent / 'summary.json': hashlib.sha256(psum_raw).hexdigest()}
    def anchor(p, raw=None):
        raw = pre.private(p) if raw is None else raw
        digest = hashlib.sha256(raw).hexdigest()
        if p in anchors and anchors[p] != digest:
            raise ValueError('original input changed')
        anchors[p] = digest
    if psum.get("schema") != SCHEMA or psum.get("required_queries") != 669 or psum.get("cost_mode") != H.COST_MODE:
        raise ValueError("parent schema/required/mode differs")
    if psum.get("option_manifest_sha256") != H.OPTION_MANIFEST_SHA or psum.get("stock_manifest_sha256") != H.STOCK_MANIFEST_SHA or psum.get("sealed_runner_sha256") != H.SEALED_RUNNER_SHA:
        raise ValueError("parent manifest pins differ")
    items, _, _ = mod.collect_book_operations(pre, H.OPTION_MANIFEST, H.STOCK_MANIFEST)
    if len(items) != 669:
        raise ValueError("book scope differs")
    ns = sum(1 for k, _, _, _, _ in items if k == "stock-mbp-1")
    if ns != 126 or (669 - ns) != 543:
        raise ValueError("669 composition differs (543 cmbp-1 + 126 mbp-1)")
    items.sort(key=lambda e: (e[1]["date"], e[0] != "stock-mbp-1", e[1]["filename"]))
    jb_raw = pre.private(parent / "journal-before.private.json")
    anchor(parent / 'journal-before.private.json', jb_raw)
    if hashlib.sha256(jb_raw).hexdigest() != psum.get('journal_before_sha256'):
        raise ValueError('original journal snapshot binding differs')
    jb = json.loads(jb_raw, object_pairs_hook=unique)
    ja_raw = pre.private(H.JOURNAL)
    ja = json.loads(ja_raw, object_pairs_hook=unique)
    for j in (jb, ja):
        if j.get('version') != 2 or j.get('authorization_mode') != 'uncapped' or j.get('lifetime_nano') != 0:
            raise ValueError('uncapped v2 journal required')
        rs = j.get("reservations", [])
        if len({r["operation_id"] for r in rs}) != len(rs):
            raise ValueError("dup financial op")
    b_by = {r["operation_id"]: r for r in jb.get("reservations", [])}
    c_by = {r["operation_id"]: r for r in ja.get("reservations", [])}
    acq = [op for _, _, _, op, _ in items if b_by.get(op, {}).get("status") == "acquired"]
    fre = [op for _, _, _, op, _ in items if op not in b_by]
    unk = [op for _, _, _, op, _ in items if op in b_by and b_by[op].get("status") != "acquired"]
    if len(acq) != 4 or len(fre) != 665 or unk:
        raise ValueError("parent book classes differ (4 acquired/665 fresh expected)")
    rows_same = all(b_by.get(op) == c_by.get(op) for _, _, _, op, _ in items)
    whole_eq = (ja_raw == jb_raw)
    parent_failures = psum.get('failures', [])
    pfail_ops = {f['op'] for f in parent_failures}
    if len(pfail_ops) != len(parent_failures):
        raise ValueError('duplicate parent failed operation')
    ok, miss = {}, []
    for kind, it, pa, op, qh in items:
        raw = pre.private(pa)
        anchor(pa, raw)
        if len(raw) != it["bytes"] or hashlib.sha256(raw).hexdigest() != it["sha256"]:
            raise ValueError("query rebind differs")
        stem = it["filename"].removesuffix(".json")
        rp, bp = parent / (stem + ".cost.json"), parent / (stem + ".response.private.json")
        good = False
        rraw = braw = None
        rc = None
        if rp.is_file() and bp.is_file():
            rraw = pre.private(rp)
            braw = pre.private(bp)
            anchor(rp, rraw)
            anchor(bp, braw)
            rc = json.loads(rraw, object_pairs_hook=unique)
            if rc.get("query_file_sha256") == it["sha256"] and rc.get("operation_id") == op and rc.get("canonical_query_sha256") == qh and rc.get("http_status") == 200 and hashlib.sha256(braw).hexdigest() == rc.get("response_sha256") and rc.get("response_bytes") == len(braw) and rc.get("informational_not_issued_purchase_quote") is True:
                if (rc.get('query_bytes') != it['bytes'] or rc.get('query_file') != it['filename']
                    or rc.get('kind') != kind or rc.get('endpoint') != '/metadata.get_cost'):
                    good = False
                else:
                    rd, co = H.parse_cost(braw)
                    if rd == rc.get("raw_cost_usd") and H.cost_nano(co) == rc.get("cost_nanodollars_rounded_up"):
                        good = True
        if good:
            ok[op] = (kind, it, rc, rraw, braw)
        else:
            miss.append((kind, it, pa, op, qh))
    if {o for _, _, _, o, _ in miss} != pfail_ops:
        raise ValueError("missing set differs from parent failures")
    if len(ok) + len(miss) != 669 or len(ok) != psum.get("priced_queries"):
        raise ValueError("counts differ; preserve originals")
    a.output.mkdir(mode=0o700)
    for op, (kind, it, rc, rraw, braw) in ok.items():
        stem = it["filename"].removesuffix(".json")
        H.save_raw(a.output / (stem + ".response.private.json"), braw)
        H.save_raw(a.output / (stem + ".cost.json"), rraw)
    new_ok, new_fail = {}, []
    if miss:
        key = pre.private(H.KEY_FILE).decode("ascii").strip()
        if not key or ":" in key or "\n" in key:
            raise ValueError("credential invalid")
        auth = "Basic " + base64.b64encode((key + ":").encode()).decode()
        del key
        for kind, it, pa, op, qh in miss:
            stem = it["filename"].removesuffix(".json")
            bp, rp = a.output / (stem + ".response.private.json"), a.output / (stem + ".cost.json")
            q = pre.read_json(pa)
            form = {"dataset": q["dataset"], "schema": q["schema"], "symbols": ",".join(q["symbols"]), "stype_in": q["stype_in"], "start": q["start"], "end": q["end"], "mode": H.COST_MODE}
            req = urllib.request.Request(H.COST_URL, data=urllib.parse.urlencode(form).encode(), headers={"Authorization": auth, "Content-Type": "application/x-www-form-urlencoded"}, method="POST")
            opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), H.NoRedirect())
            try:
                try:
                    resp = opener.open(req, timeout=45)
                except urllib.error.HTTPError as e:
                    resp = e
                with resp:
                    st, body = resp.status, resp.read(8193)
            except (urllib.error.URLError, TimeoutError, OSError):
                new_fail.append({"kind": kind, "item": it, "op": op, "query_file": it["filename"], "failure": "transport_failure_no_response"})
                continue
            if len(body) > 8192:
                raise ValueError("response bound exceeded")
            H.save_raw(bp, body)
            bh = hashlib.sha256(body).hexdigest()
            if st != 200:
                new_fail.append({"kind": kind, "item": it, "op": op, "query_file": it["filename"], "failure": "http_failure", "http_status": st, "response_sha256": bh, "response_bytes": len(body)})
                continue
            try:
                rd, co = H.parse_cost(body)
                nn = H.cost_nano(co)
            except ValueError:
                new_fail.append({"kind": kind, "item": it, "op": op, "query_file": it["filename"], "failure": "cost_parse_failure", "response_sha256": bh, "response_bytes": len(body)})
                continue
            rc = {"query_file": it["filename"], "query_file_sha256": it["sha256"], "query_bytes": it["bytes"], "canonical_query_sha256": qh, "operation_id": op, "kind": kind, "http_status": st, "endpoint": "/metadata.get_cost", "raw_cost_usd": rd, "cost_nanodollars_rounded_up": nn, "response_sha256": bh, "response_bytes": len(body), "observed_at": datetime.now(timezone.utc).isoformat(), "informational_not_issued_purchase_quote": True, "actual_invoice_cost_verified": False}
            H.save_raw(rp, H.encode(rc))
            new_ok[op] = (kind, it, rc)
    for p, digest in anchors.items():
        if hashlib.sha256(pre.private(p)).hexdigest() != digest:
            raise ValueError('original input changed during reconciliation')
    ja_raw = pre.private(H.JOURNAL)
    ja = json.loads(ja_raw, object_pairs_hook=unique)
    if len({r['operation_id'] for r in ja['reservations']}) != len(ja['reservations']):
        raise ValueError('duplicate current financial operation')
    c_by = {r['operation_id']: r for r in ja['reservations']}
    rows_same = rows_same and all(b_by.get(op) == c_by.get(op) for _, _, _, op, _ in items)
    whole_eq = ja_raw == jb_raw
    rebind = rows_same
    for kind, it, pa, op, qh in items:
        raw = pre.private(pa)
        if len(raw) != it["bytes"] or hashlib.sha256(raw).hexdigest() != it["sha256"]:
            rebind = False
    for op, (kind, it, rc, _, _) in ok.items():
        stem = it["filename"].removesuffix(".json")
        nr = json.loads(pre.private(a.output / (stem + ".cost.json")), object_pairs_hook=unique)
        nb = pre.private(a.output / (stem + ".response.private.json"))
        if nr != rc or nb != pre.private(parent / (stem + ".response.private.json")):
            rebind = False
    allp = dict(ok)
    allp.update({o: (k, i, r) for o, (k, i, r) in new_ok.items()})
    nano_all, nano_fre = 0, 0
    ps_all, ps_fre = {}, {}
    fre_set = set(fre)
    for op, (kind, it, rc, *_) in [(o, v) for o, v in ok.items()] + [(o, (*v, b"")) for o, v in new_ok.items()]:
        nn = int(rc["cost_nanodollars_rounded_up"])
        nano_all += nn
        if nano_all > H.MAX_INT64:
            raise ValueError("aggregate bound exceeded")
        s = "mbp-1" if kind == "stock-mbp-1" else "cmbp-1"
        g = ps_all.setdefault(s, {"queries": 0, "nano": 0})
        g["queries"] += 1
        g["nano"] += nn
        if op in fre_set:
            nano_fre += nn
            h = ps_fre.setdefault(s, {"queries": 0, "nano": 0})
            h["queries"] += 1
            h["nano"] += nn
    for g in (*ps_all.values(), *ps_fre.values()):
        g["usd"] = str(Decimal(g["nano"]).scaleb(-9))
    priced = len(ok) + len(new_ok)
    complete = priced == 669 and not new_fail and rebind and rows_same
    batch_sha = hashlib.sha256((H.SOURCE_ROOT / "internal/databento/batch.go").read_bytes()).hexdigest()
    ps_sha = hashlib.sha256(pre.private(parent / "summary.json")).hexdigest()
    summary = {"schema": SCHEMA, "parent_run": PARENT_NAME, "parent_summary_sha256": ps_sha, "option_manifest_sha256": H.OPTION_MANIFEST_SHA, "stock_manifest_sha256": H.STOCK_MANIFEST_SHA, "sealed_runner_sha256": H.SEALED_RUNNER_SHA, "batch_go_sha256": batch_sha, "cost_mode": H.COST_MODE, "required_queries": 669, "priced_queries": priced, "complete_estimate": complete, "existing_responses_reused": len(ok), "newly_priced": len(new_ok), "new_failures": len(new_fail), "quoted_total_or_subtotal_usd": str(Decimal(nano_all).scaleb(-9)), "quoted_total_nanodollars_ceiling": nano_all, "remaining_fresh_usd": str(Decimal(nano_fre).scaleb(-9)), "remaining_fresh_nanodollars_ceiling": nano_fre, "per_schema_full": ps_all, "per_schema_remaining_fresh": ps_fre, "already_acquired": 4, "never_attempted_fresh": 665, "journal_before_sha256": hashlib.sha256(jb_raw).hexdigest(), "journal_current_sha256": hashlib.sha256(ja_raw).hexdigest(), "journal_bytes_equal": whole_eq, "book_rows_unchanged": rows_same, "journal_mutated_by_this_tool": False, "query_rebind_ok": rebind, "failures": new_fail, "actual_invoice_cost_verified": False, "credit_balance_verified": False, "source_qualified": False, "historical_performance_available": False, "paid_requests": 0, "financial_writes": 0, "untouched_files": [str(H.OPTION_MANIFEST), str(H.STOCK_MANIFEST), str(H.JOURNAL), str(H.KEY_FILE)], "limitations": "Informational metadata.get_cost only; subtotal unless 669 rebound with zero new failures; invoice/balance/qualification/P&L unverified; never invent missing cost.", "observed_at": datetime.now(timezone.utc).isoformat()}
    H.save_raw(a.output / "summary.json", H.encode(summary))
    pub = {"required": 669, "priced": priced, "reused": len(ok), "new_ok": len(new_ok), "new_fail": len(new_fail), "complete": complete, "total_usd": str(Decimal(nano_all).scaleb(-9)), "total_nano": nano_all, "fresh_usd": str(Decimal(nano_fre).scaleb(-9)), "fresh_nano": nano_fre, "rebind_ok": rebind, "rows_same": rows_same, "parent_summary_sha256": ps_sha, "summary_sha256": hashlib.sha256(H.encode(summary)).hexdigest(), "helper_sha256": HELPER_SHA}
    print(json.dumps(pub, sort_keys=True), flush=True)
if __name__ == "__main__":
    try:
        main()
    except Exception as e:
        print(json.dumps({"status": "stopped", "error_type": type(e).__name__, "inspect_private_evidence": True}), flush=True)
        sys.exit(1)
