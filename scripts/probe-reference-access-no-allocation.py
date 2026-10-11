#!/usr/bin/env python3
"""Research-only Corporate Actions access probe, allocate_isins=false.
Mirrors accepted SDK cycle49-reference-sdk/corporate.py get_range raw wire
(SHA 9ec588b95db94297e870faf4a8f31f1392dc0d322c8375de08e9900574d53eb3):
form allocate_isins=false drops rows needing new ISINs. Single fixed endpoint.
Never requests new allocations, subscription changes, trading or journal writes.
"""
import argparse
import base64
import hashlib
import json
import os
import ssl
import stat
import time
from datetime import datetime, timezone
from pathlib import Path
import urllib.error
import urllib.parse
import urllib.request
ENDPOINT = "https://hist.databento.com/v0/corporate_actions.get_range"
ROOT = Path("/home/yuduo/Projects/AutoWheel-research/source-afa3075")
BASE = Path("/home/yuduo/Projects/AutoWheel-research")
KEY_FILE = Path("/home/yuduo/.config/autowheel/databento-pilot.key")
SDK_REL = Path("research/simulation/2026-10-05/cycle49-reference-sdk/corporate.py")
SDK_SHA = "9ec588b95db94297e870faf4a8f31f1392dc0d322c8375de08e9900574d53eb3"
START = "1970-01-01T00:00:00Z"
END = "2026-10-03T00:00:00Z"
SYMBOLS = ["NFLX","TSLA","NVDA","NBIS","IREN","NOW","AMD","AMZN","GOOGL","META","MSFT","PLTR","UBER","AVGO","ORCL"]
TIMEOUT = 45
LIMIT = 8 << 20
class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *a, **k):
        return None
def priv_dir(p):
    s = p.lstat()
    if not stat.S_ISDIR(s.st_mode) or stat.S_IMODE(s.st_mode) != 0o700 or s.st_uid != os.getuid() or p.resolve() != p:
        raise ValueError("private 700 owned canonical directory required: " + str(p))
def save_raw(p, raw):
    fd = os.open(p, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, "wb") as f:
        f.write(raw)
        f.flush()
        os.fsync(f.fileno())
    dfd = os.open(p.parent, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(dfd)
    finally:
        os.close(dfd)
def main():
    ap = argparse.ArgumentParser(description="Research-only no-alloc corporate-actions probe.")
    ap.add_argument("--output", type=Path, required=True)
    a = ap.parse_args()
    out = a.output
    if not out.is_absolute() or out != Path(os.path.normpath(out)) or out.resolve() != out:
        raise ValueError("absolute canonical --output required")
    if out.parent != BASE or not out.name.startswith("cycle"):
        raise ValueError("fresh cycle directory under BASE required")
    # Fixed MSI roots only; no bypass via env/argv/cwd.
    for d in (ROOT, BASE):
        priv_dir(d)
    me = Path(__file__).resolve()
    try:
        me.relative_to(ROOT)
    except ValueError:
        raise ValueError("script runnable only from fixed ROOT")
    sdk = ROOT / SDK_REL
    if sdk.is_symlink() or not sdk.is_file():
        raise ValueError("pinned SDK original required")
    if hashlib.sha256(sdk.read_bytes()).hexdigest() != SDK_SHA:
        raise ValueError("pinned SDK SHA differs")
    st = KEY_FILE.lstat()
    if not stat.S_ISREG(st.st_mode) or stat.S_IMODE(st.st_mode) != 0o600 or st.st_uid != os.getuid() or st.st_nlink != 1 or KEY_FILE.resolve() != KEY_FILE:
        raise ValueError("600 single-link owned key file required")
    priv_dir(KEY_FILE.parent)
    key = KEY_FILE.read_bytes().decode("ascii").strip()
    if not key or ":" in key or "\n" in key or "\r" in key:
        raise ValueError("credential invalid")
    if out.exists() or out.is_symlink():
        raise ValueError("fresh --output required")
    out.mkdir(mode=0o700)
    priv_dir(out)
    form = {"start": START, "end": END, "index": "ts_record", "symbols": ",".join(SYMBOLS), "stype_in": "nasdaq_symbol", "allocate_isins": "false", "compression": "zstd"}
    body = urllib.parse.urlencode(form).encode("ascii")
    auth = "Basic " + base64.b64encode((key + ":").encode("ascii")).decode("ascii")
    req = urllib.request.Request(ENDPOINT, data=body, headers={"Authorization": auth, "Content-Type": "application/x-www-form-urlencoded"}, method="POST")
    del auth
    ctx = ssl.create_default_context()
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), urllib.request.HTTPSHandler(context=ctx), NoRedirect())
    t0 = time.monotonic()
    ts = datetime.now(timezone.utc).isoformat()
    status = None
    raw = None
    failure = None
    try:
        try:
            resp = opener.open(req, timeout=TIMEOUT)
        except urllib.error.HTTPError as e:
            resp = e
        with resp:
            status = int(getattr(resp, "status", resp.code))
            raw = resp.read(LIMIT + 1)
    except (urllib.error.URLError, TimeoutError, OSError) as e:
        if isinstance(e, urllib.error.HTTPError):
            raise
        failure = "transport_failure_incomplete_response" if status is not None else "transport_failure_no_response"
        raw = None
    elapsed = time.monotonic() - t0
    own_sha = hashlib.sha256(Path(__file__).read_bytes()).hexdigest()
    if failure is not None:
        receipt = {"schema": "AutoWheelReferenceAccessProbeNoAllocV1", "endpoint": ENDPOINT, "method": "POST", "form": form, "expected_symbols_requested": 15, "symbols": SYMBOLS, "sdk_corporate_sha256": SDK_SHA, "http_status": status, "failure": failure, "response_saved": False, "response_complete": False, "observed_at": ts, "elapsed_s": elapsed, "code_sha256": own_sha}
        save_raw(out / "receipt.json", (json.dumps(receipt, indent=2, sort_keys=True) + "\n").encode())
        summary = dict(receipt, allocation_disabled_by_request=True, new_allocation_not_proved_from_billing=True, subscription_enrolled=False, journal_writes=0, historical_performance_available=False, source_qualified=False, full_gates=False, access_probe_only=True, actual_invoice_cost_verified=False, credential_literal_matches=0, financial_requests=0, limits={"timeout_s": TIMEOUT, "response_bound_bytes": LIMIT, "retries": 0, "redirects": "deny", "single_fixed_endpoint": True})
        save_raw(out / "summary.json", (json.dumps(summary, indent=2, sort_keys=True) + "\n").encode())
        print(json.dumps({"http_status": status, "failure": failure, "expected_symbols_requested": 15, "access_probe_only": True}))
        return
    save_raw(out / "response.private.zstd", raw)
    cred_matches = 1 if key.encode("ascii") in raw else 0
    del key
    if cred_matches != 0:
        raise ValueError("credential reflected in response")
    rsha = hashlib.sha256(raw).hexdigest()
    receipt = {"schema": "AutoWheelReferenceAccessProbeNoAllocV1", "endpoint": ENDPOINT, "method": "POST", "form": form, "expected_symbols_requested": 15, "symbols": SYMBOLS, "sdk_corporate_sha256": SDK_SHA, "http_status": status, "response_bytes": len(raw), "response_sha256": rsha, "response_path": str(out / "response.private.zstd"), "observed_at": ts, "elapsed_s": elapsed, "code_sha256": own_sha}
    save_raw(out / "receipt.json", (json.dumps(receipt, indent=2, sort_keys=True) + "\n").encode())
    summary = dict(receipt, allocation_disabled_by_request=True, new_allocation_not_proved_from_billing=True, subscription_enrolled=False, journal_writes=0, historical_performance_available=False, source_qualified=False, full_gates=False, access_probe_only=True, actual_invoice_cost_verified=False, credential_literal_matches=0, financial_requests=1, limitations="Status 200 proves transport only; not entitlement/completeness/absence nor availability clocks; allocate_isins=false may drop every unallocated symbol.")
    summary.update(financial_requests=0, record_requests=1, paid_submissions=0,
        response_complete=len(raw)<=LIMIT, response_truncated=len(raw)>LIMIT)
    summary["limits"] = {"timeout_s": TIMEOUT, "response_bound_bytes": LIMIT, "retries": 0, "redirects": "deny", "single_fixed_endpoint": True}
    save_raw(out / "summary.json", (json.dumps(summary, indent=2, sort_keys=True) + "\n").encode())
    pub = {k: summary[k] for k in ("http_status", "response_bytes", "response_sha256", "expected_symbols_requested", "allocation_disabled_by_request", "subscription_enrolled", "journal_writes", "access_probe_only", "actual_invoice_cost_verified", "credential_literal_matches", "sdk_corporate_sha256", "code_sha256", "observed_at")}
    print(json.dumps(pub, sort_keys=True))
if __name__ == "__main__":
    main()
