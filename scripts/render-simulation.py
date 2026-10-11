#!/usr/bin/env python3
"""Standalone renderer for AutoWheel simulation Report JSON.

Reads internal/simulation.Report JSON (see internal/simulation/types.go) and
writes a self-contained, offline-readable HTML report. Python3 standard
library only. No network, no external scripts, no external stylesheets.

Strict display rules (never fabricate returns):
  - status == "blocked" OR performance missing -> every metric shows "N/A"
    and no equity/return chart is emitted.
  - status == "fixture" -> banner prominently says SYNTHETIC FIXTURE and the
    page states it is not six-month performance, even if metrics exist.
  - status == "counterfactual" with performance -> full counterfactual view:
    net P&L / return / drawdown, premiums / fees / stock realized /
    dividends, fill rate / assignment / call-away, end inventory, inline SVG
    equity curve, and daily / cash / exposure table.

Usage:
  python3 scripts/render-simulation.py --report input.json --output report.html
"""
import argparse
import html
import json
import math
import sys
from pathlib import Path

ALLOWED_STATUSES = ("blocked", "fixture", "counterfactual")
DEFAULT_START = "2026-04-06"
DEFAULT_END = "2026-10-02"
DEFAULT_INITIAL_CASH_CENTS = 8000000  # $80,000 all-cash hypothetical start.


def esc(value):
    if value is None:
        return ""
    return html.escape(str(value), quote=True)


def fmt_money(cents):
    if isinstance(cents, bool):
        return "N/A"
    if not isinstance(cents, int):
        return "N/A"
    try:
        c = int(cents)
    except Exception:
        return "N/A"
    sign = "-" if c < 0 else ""
    a = abs(c)
    dollars = a // 100
    rem = a % 100
    # thousands separator for dollars
    d_str = f"{dollars:,}"
    return f"{sign}${d_str}.{rem:02d}"


def fmt_pct(value):
    if isinstance(value, bool):
        return "N/A"
    if not isinstance(value, (int, float)) or not math.isfinite(value):
        return "N/A"
    return f"{float(value):.2f}%"


def fmt_int(value):
    if isinstance(value, bool):
        return "N/A"
    if not isinstance(value, int):
        # JSON may decode ints as int; floats for counts are invalid.
        return "N/A"
    return f"{value:,}"


def short_date(value):
    if not isinstance(value, str) or not value:
        return ""
    # Keep full timestamp for title, show date part in tables/SVG.
    return value[:10] if len(value) >= 10 else value


def load_report(path):
    try:
        with open(path, "r", encoding="utf-8") as f:
            data = json.load(f)
    except FileNotFoundError:
        sys.stderr.write(f"error: report file not found: {path}\n")
        sys.exit(2)
    except json.JSONDecodeError as e:
        sys.stderr.write(f"error: invalid JSON in {path}: {e}\n")
        sys.exit(2)
    if not isinstance(data, dict):
        sys.stderr.write("error: report JSON must be an object\n")
        sys.exit(2)
    return data


def equity_svg(points):
    """Build an inline SVG equity curve. Caller guarantees non-empty."""
    W, H = 760, 240
    PAD_L, PAD_R, PAD_T, PAD_B = 64, 16, 16, 34
    iw = W - PAD_L - PAD_R
    ih = H - PAD_T - PAD_B
    equities = []
    labels = []
    for p in points:
        try:
            equities.append(p["equity_cents"])
        except Exception:
            raise ValueError("invalid equity point; cannot invent a zero mark")
        at = p.get("at", "")
        labels.append(str(at) if isinstance(at, str) else "")
    n = len(equities)
    mn = min(equities)
    mx = max(equities)
    if mx == mn:
        mx = mn + 100
        mn = mn - 100
    def x_at(i):
        if n == 1:
            return PAD_L + iw / 2
        return PAD_L + (iw * i / (n - 1))
    def y_at(v):
        frac = (v - mn) / (mx - mn)
        return PAD_T + ih * (1.0 - frac)
    pts = [(x_at(i), y_at(v)) for i, v in enumerate(equities)]
    if n == 1:
        line = f'<circle cx="{pts[0][0]:.1f}" cy="{pts[0][1]:.1f}" r="4" fill="#225ad4" />'
    else:
        coord = " ".join(f"{x:.1f},{y:.1f}" for x, y in pts)
        # area fill under line
        area = (
            f"M {pts[0][0]:.1f},{(PAD_T+ih):.1f} "
            + " ".join(f"L {x:.1f},{y:.1f}" for x, y in pts)
            + f" L {pts[-1][0]:.1f},{(PAD_T+ih):.1f} Z"
        )
        line = (
            f'<path d="{area}" fill="#225ad4" opacity="0.12" />'
            f'<polyline points="{esc(coord)}" fill="none" stroke="#225ad4" stroke-width="2" '
            f'stroke-linejoin="round" stroke-linecap="round" />'
        )
    # y gridlines: min / mid / max
    grid = ""
    for frac, val in ((0.0, mx), (0.5, (mx + mn) / 2), (1.0, mn)):
        y = PAD_T + ih * frac
        grid += (
            f'<line x1="{PAD_L}" y1="{y:.1f}" x2="{W-PAD_R}" y2="{y:.1f}" '
            f'stroke="#dce3ec" stroke-width="1" />'
            f'<text x="{PAD_L-8}" y="{y+4:.1f}" text-anchor="end" font-size="11" fill="#526276">'
            f'{esc(fmt_money(int(val)))}</text>'
        )
    # x labels: first / middle / last dates
    first = esc(short_date(labels[0])) if labels else ""
    last = esc(short_date(labels[-1])) if labels else ""
    mid = esc(short_date(labels[n // 2])) if labels else ""
    xlabels = (
        f'<text x="{PAD_L}" y="{H-10}" font-size="11" fill="#526276">{first}</text>'
        f'<text x="{PAD_L+iw/2}" y="{H-10}" text-anchor="middle" font-size="11" fill="#526276">{mid}</text>'
        f'<text x="{W-PAD_R}" y="{H-10}" text-anchor="end" font-size="11" fill="#526276">{last}</text>'
    )
    title = f"Equity curve, {n} point{'s' if n != 1 else ''}, {fmt_money(min(equities))} to {fmt_money(max(equities))}."
    return (
        f'<svg viewBox="0 0 {W} {H}" role="img" aria-labelledby="eqTitle eqDesc" '
        f'preserveAspectRatio="xMidYMid meet" style="width:100%;height:auto;display:block">'
        f'<title id="eqTitle">Equity curve</title>'
        f'<desc id="eqDesc">{esc(title)} Marking: stock at bid, short options at ask. Synthetic fixtures are not performance.</desc>'
        f'{grid}{line}{xlabels}</svg>'
    )


CSS = """
:root{color-scheme:light dark;--ink:#162437;--muted:#526276;--paper:#f5f7fa;--card:#ffffff;--line:#dce3ec;--blue:#225ad4;--amber:#8a5a00;--amberbg:#fff6e0;--red:#a4262c;--redbg:#fdecec;--green:#1a6b3c;--greenbg:#e8f5ec}
*{box-sizing:border-box}body{margin:0;background:var(--paper);color:var(--ink);font:16px/1.65 -apple-system,BlinkMacSystemFont,"Segoe UI",Helvetica,Arial,sans-serif}
main{max-width:960px;margin:auto;padding:48px 28px 80px}
h1{font-size:34px;letter-spacing:-0.5px;line-height:1.2;margin:10px 0 16px}
h2{font-size:21px;margin:34px 0 10px}h3{font-size:16px;margin:0 0 8px}
p{margin:0 0 12px}.eyebrow{font-size:12px;letter-spacing:2px;color:var(--blue);font-weight:700}
.card{padding:20px;border:1px solid var(--line);border-radius:14px;background:var(--card);margin:16px 0;overflow-x:auto}
.banner{padding:18px 20px;border-radius:14px;margin:16px 0;border:1px solid var(--line)}
.banner.blocked{background:var(--redbg);border-left:6px solid var(--red)}
.banner.fixture{background:var(--amberbg);border-left:6px solid #c07916}
.banner.counterfactual{background:var(--greenbg);border-left:6px solid var(--green)}
.banner strong{font-size:17px;letter-spacing:0.2px}
.kv{display:grid;grid-template-columns:220px 1fr;gap:6px 14px;font-size:14px}
.kv dt{color:var(--muted)}.kv dd{margin:0;overflow-wrap:anywhere}
table{width:100%;border-collapse:collapse;font-size:14px;margin:10px 0}
caption{text-align:left;font-weight:700;padding:6px 0;color:var(--ink)}
td,th{text-align:left;vertical-align:top;padding:10px 8px;border-bottom:1px solid var(--line)}
th[scope=row]{color:var(--muted);font-weight:600;width:42%}
.num{text-align:right;font-variant-numeric:tabular-nums;white-space:nowrap}
.muted,.small{color:var(--muted)}ul,ol{padding-left:22px}
code{font:13px/1.6 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;overflow-wrap:anywhere}
pre{white-space:pre-wrap;overflow-wrap:anywhere;background:var(--paper);padding:14px;border-radius:8px;font-size:12.5px}
details{margin:14px 0}a{color:var(--blue)}
.badge{display:inline-block;padding:3px 10px;border:1px solid var(--line);border-radius:999px;font-size:12.5px;font-weight:700;letter-spacing:0.4px}
@media(max-width:640px){main{padding:28px 16px 60px}h1{font-size:27px}.kv{grid-template-columns:1fr}}
@media(prefers-color-scheme:dark){:root{--ink:#e0e7f2;--muted:#a5b3c7;--paper:#111824;--card:#192332;--line:#304156;--blue:#85aaff;--amberbg:#2e250f;--redbg:#33191b;--greenbg:#122b1c}}
"""


def render(report):
    raw_status = report.get("status", "")
    status = raw_status if raw_status in ALLOWED_STATUSES else "blocked"
    status_invalid = raw_status not in ALLOWED_STATUSES
    spec = report.get("spec") if isinstance(report.get("spec"), dict) else {}
    perf = report.get("performance")
    has_perf = isinstance(perf, dict)
    # Strict rule: blocked status OR missing performance -> N/A, no chart.
    show_metrics = (status in ("fixture", "counterfactual")) and has_perf
    equity = report.get("equity_curve")
    if not isinstance(equity, list):
        equity = []
    # Only dict points with equity_cents survive for chart/table.
    equity_points = [p for p in equity if isinstance(p, dict)]
    show_chart = bool(show_metrics and len(equity_points) > 0)
    trades = report.get("trades")
    if not isinstance(trades, list):
        trades = []
    trade_rows = [t for t in trades if isinstance(t, dict)]
    end_state = report.get("end_state") if isinstance(report.get("end_state"), dict) else None
    blockers = report.get("blockers") if isinstance(report.get("blockers"), list) else []
    assumptions = report.get("assumptions") if isinstance(report.get("assumptions"), list) else []

    # A hand-edited or damaged report must not turn incomplete source coverage
    # into a plausible-looking curve. This is a format/integrity check, not a
    # claim that the renderer independently verified vendor observations.
    money_keys = ("ending_equity_cents", "net_pnl_cents", "premiums_cents", "fees_cents", "stock_realized_cents", "dividends_cents")
    count_keys = ("submissions", "fills", "expired_orders", "assignments", "call_aways", "blocked_cycles")
    finite_keys = ("return_pct", "max_drawdown_pct")
    number = lambda value: type(value) in (int, float) and math.isfinite(value)
    valid_perf = has_perf and all(type(perf.get(k)) is int for k in money_keys + count_keys) and all(number(perf.get(k)) for k in finite_keys)
    valid_curve = bool(equity_points) and all(type(p.get("equity_cents")) is int and isinstance(p.get("at"), str) for p in equity_points)
    valid_coverage = all(type(report.get(k)) is int for k in ("expected_decisions", "observed_decisions", "expected_valuations", "observed_valuations")) and all(report[k] > 0 for k in ("expected_decisions", "expected_valuations")) and report.get("expected_decisions") == report.get("observed_decisions") and report.get("expected_valuations") == report.get("observed_valuations") and len(equity_points) == report.get("expected_valuations")
    valid_mode = (status == "fixture" and spec.get("mode") == "fixture") or (status == "counterfactual" and spec.get("mode") == "historical_quote_replay")
    valid_integrity = report.get("schema_version") == "AutoWheelReplayReportV1" and valid_mode and valid_perf and valid_curve and valid_coverage and spec.get("corporate_actions_audited") is True and not blockers
    if status == "counterfactual":
        valid_integrity = valid_integrity and all(isinstance(report.get(k), str) and len(report[k]) == 64 and all(c in "0123456789abcdef" for c in report[k]) for k in ("input_sha256", "strategy_sha256", "simulation_engine_sha256"))
    if valid_integrity:
        initial = spec.get("initial_cash_cents")
        valid_integrity = type(initial) is int and initial > 0 and perf["ending_equity_cents"] == equity_points[-1]["equity_cents"] and perf["net_pnl_cents"] == perf["ending_equity_cents"] - initial and abs(perf["return_pct"] - perf["net_pnl_cents"] / initial * 100) < 1e-8
    if show_metrics and not valid_integrity:
        blockers = blockers + ["Report format, mode, valuation coverage or P&L reconciliation failed; performance suppressed."]
        if status != "fixture":
            status = "blocked"
    show_metrics = show_metrics and valid_integrity
    show_chart = show_metrics and valid_curve
    if not show_metrics:
        trade_rows, end_state = [], None

    spec_mode = spec.get("mode", "")
    start = spec.get("start_date") or DEFAULT_START
    end = spec.get("end_date") or DEFAULT_END
    start_missing = not spec.get("start_date")
    end_missing = not spec.get("end_date")
    initial_cash = spec.get("initial_cash_cents")
    if not isinstance(initial_cash, int):
        initial_cash = DEFAULT_INITIAL_CASH_CENTS
        initial_cash_note = " (default $80,000 hypothetical; spec value missing)"
    else:
        initial_cash_note = ""
    peer_reserve = spec.get("peer_reserved_cash_cents")
    peer_symbols = spec.get("peer_reserved_symbols") if isinstance(spec.get("peer_reserved_symbols"), list) else []
    source = spec.get("source", "")
    corp_audited = spec.get("corporate_actions_audited")
    input_sha = report.get("input_sha256", "")
    strat_sha = report.get("strategy_sha256", "")
    exp_dec = report.get("expected_decisions")
    obs_dec = report.get("observed_decisions")
    exp_val = report.get("expected_valuations")
    obs_val = report.get("observed_valuations")

    if status == "blocked":
        h1 = "Simulation blocked &mdash; no measured performance"
        banner_cls = "blocked"
        banner_title = "BLOCKED &mdash; NO MEASURED PERFORMANCE"
        banner_body = ("This input did not qualify for a counterfactual. Every performance "
                       "metric below reads <strong>N/A</strong> and no equity/return chart is shown. "
                       "The renderer never fabricates returns.")
    elif status == "fixture":
        h1 = "Synthetic fixture &mdash; not six-month performance"
        banner_cls = "fixture"
        banner_title = "SYNTHETIC FIXTURE &mdash; NOT SIX-MONTH PERFORMANCE"
        banner_body = ("This report uses invented fixture data to validate replay accounting and reporting. "
                       "Any numbers below are synthetic and must not be read as the April&ndash;October 2026 "
                       "historical counterfactual or as any live result.")
    else:
        h1 = "Historical counterfactual (quote replay)"
        banner_cls = "counterfactual"
        banner_title = "COUNTERFACTUAL &mdash; HYPOTHETICAL, NOT ACTUAL FILLS"
        banner_body = ("A hypothetical quote-replay over the stated window. Executions, fills, assignments, "
                       "and call-aways are modeled assumptions, not historical broker fills or receipts. "
                       "There is no measured six-month return until the required point-in-time dataset and "
                       "data-quality gates pass; a blocked report would show N/A instead.")

    # Performance rows
    def perf_row(label, value_str):
        return f"<tr><th scope=\"row\">{esc(label)}</th><td class=\"num\">{esc(value_str)}</td></tr>"

    if show_metrics:
        submissions = perf.get("submissions")
        fills = perf.get("fills")
        if isinstance(submissions, int) and isinstance(fills, int) and submissions > 0:
            fill_rate = f"{fills:,}/{submissions:,} ({(fills/submissions*100):.1f}%)"
        elif isinstance(submissions, int) and isinstance(fills, int):
            fill_rate = f"{fills:,}/{submissions:,} (no submissions)"
        else:
            fill_rate = "N/A"
        perf_table = "\n".join([
            perf_row("Ending equity", fmt_money(perf.get("ending_equity_cents"))),
            perf_row(f"Net P&L (ending equity minus {fmt_money(initial_cash)} starting capital)", fmt_money(perf.get("net_pnl_cents"))),
            perf_row("Return on hypothetical start", fmt_pct(perf.get("return_pct"))),
            perf_row("Max drawdown at session closes", fmt_pct(perf.get("max_drawdown_pct"))),
            perf_row("Premiums collected (gross, not profit)", fmt_money(perf.get("premiums_cents"))),
            perf_row("Fees paid", fmt_money(perf.get("fees_cents"))),
            perf_row("Stock realized gains / losses", fmt_money(perf.get("stock_realized_cents"))),
            perf_row("Dividends credited", fmt_money(perf.get("dividends_cents"))),
            perf_row("Submissions", fmt_int(submissions)),
            perf_row("Fills", fmt_int(fills)),
            perf_row("Fill rate", fill_rate),
            perf_row("Expired (unfilled day-limit) orders", fmt_int(perf.get("expired_orders"))),
            perf_row("Assignments (modeled inference, not receipts)", fmt_int(perf.get("assignments"))),
            perf_row("Call-aways (modeled)", fmt_int(perf.get("call_aways"))),
            perf_row("Blocked cycles", fmt_int(perf.get("blocked_cycles"))),
        ])
        perf_note = ("Premiums are gross credits before fees, assignment losses, call-away losses, "
                     "and drawdown. Premium &ne; profit.")
        if status == "fixture":
            perf_note = ("Synthetic fixture values for accounting validation only. " + perf_note)
    else:
        na_labels = ["Ending equity", "Net P&L", "Return on hypothetical start",
                     "Max drawdown", "Premiums collected", "Fees paid",
                     "Stock realized gains / losses", "Dividends credited",
                     "Submissions", "Fills", "Fill rate", "Expired orders",
                     "Assignments", "Call-aways", "Blocked cycles"]
        perf_table = "\n".join(perf_row(l, "N/A") for l in na_labels)
        if status == "blocked":
            perf_note = ("Blocked or unqualified input: the renderer shows N/A and emits no "
                         "equity/return chart. No return has been measured.")
        else:
            perf_note = ("Performance object missing: the renderer shows N/A and emits no "
                         "equity/return chart. No return has been measured or implied.")

    # Equity chart + daily table
    if show_chart:
        chart_html = equity_svg(equity_points)
        if status == "fixture":
            chart_caption = "Synthetic equity curve (fixture only &mdash; not six-month performance)."
        else:
            chart_caption = "Hypothetical equity curve. Stock marked at bid; short options at ask."
        daily_rows = []
        for p in equity_points:
            daily_rows.append(
                "<tr>"
                f"<td>{esc(p.get('at',''))}</td>"
                f"<td class=\"num\">{esc(fmt_money(p.get('equity_cents')))}</td>"
                f"<td class=\"num\">{esc(fmt_money(p.get('economic_cash_cents')))}</td>"
                f"<td class=\"num\">{esc(fmt_money(p.get('available_cash_cents')))}</td>"
                f"<td class=\"num\">{esc(fmt_money(p.get('stock_value_cents')))}</td>"
                f"<td class=\"num\">{esc(fmt_money(p.get('option_liability_cents')))}</td>"
                f"<td class=\"num\">{esc(fmt_money(p.get('committed_capital_cents')))}</td>"
                f"<td class=\"num\">{esc(p.get('active_symbols',''))}</td>"
                "</tr>"
            )
        daily_html = "\n".join(daily_rows)
        equity_section = f"""
<h2>Equity curve and daily cash / exposure</h2>
<div class="card"><p><span class="badge">{esc('SYNTHETIC' if status=='fixture' else 'COUNTERFACTUAL')}</span></p>
{chart_html}
<p class="muted small">{chart_caption} Economic cash is unsettled-inclusive; available cash is tradable. Committed capital counts share cost plus short/pending put collateral against the capital ceiling.</p></div>
<div class="card"><table><caption>Daily valuation points ({len(equity_points)} rows)</caption>
<thead><tr><th scope="col">Time</th><th scope="col" class="num">Equity</th><th scope="col" class="num">Economic cash</th><th scope="col" class="num">Available cash</th><th scope="col" class="num">Stock value (bid)</th><th scope="col" class="num">Option liability (ask)</th><th scope="col" class="num">Committed</th><th scope="col" class="num">Active</th></tr></thead>
<tbody>{daily_html}</tbody></table></div>
"""
    else:
        equity_section = """
<h2>Equity curve and daily cash / exposure</h2>
<div class="card"><p class="muted">No equity/return chart. Blocked inputs and reports without a performance object never produce a chart; the renderer does not interpolate or fabricate one.</p></div>
"""

    # Trades
    if trade_rows:
        tr_html = []
        for t in trade_rows:
            tr_html.append(
                "<tr>"
                f"<td>{esc(t.get('at',''))}</td>"
                f"<td>{esc(t.get('symbol',''))}</td>"
                f"<td><code>{esc(t.get('option_id',''))}</code></td>"
                f"<td>{esc(t.get('action',''))}</td>"
                f"<td class=\"num\">{esc(fmt_money(t.get('price_cents')))}</td>"
                f"<td class=\"num\">{esc(fmt_money(t.get('cash_flow_cents')))}</td>"
                f"<td class=\"num\">{esc(fmt_money(t.get('fee_cents')))}</td>"
                f"<td>{esc(t.get('assumption',''))}</td>"
                "</tr>"
            )
        trades_section = f"""
<h2>Counterfactual executions ({len(trade_rows)})</h2>
<div class="card"><p class="muted small">{"Synthetic fixture executions for template testing only." if status=="fixture" else "Modeled day-limit sell-to-open executions under the latency / sustained-bid fill model. Not actual broker fills."} GFD; no reprice, roll, buyback, or stop.</p>
<table><caption>{"Synthetic trades" if status=="fixture" else "Hypothetical trades"}</caption>
<thead><tr><th scope="col">Time</th><th scope="col">Symbol</th><th scope="col">Option</th><th scope="col">Action</th><th scope="col" class="num">Price</th><th scope="col" class="num">Cash flow</th><th scope="col" class="num">Fee</th><th scope="col">Assumption</th></tr></thead>
<tbody>{"".join(tr_html)}</tbody></table></div>
"""
    else:
        trades_section = """
<h2>Counterfactual executions</h2>
<div class="card"><p class="muted">No executions recorded in this report.</p></div>
"""

    # End inventory
    if end_state is not None:
        positions = end_state.get("positions")
        if not isinstance(positions, dict):
            positions = {}
        intents = end_state.get("intents")
        if not isinstance(intents, dict):
            intents = {}
        pos_rows = []
        for sym in sorted(positions.keys()):
            p = positions[sym] if isinstance(positions[sym], dict) else {}
            pos_rows.append(
                "<tr>"
                f"<td>{esc(sym)}</td>"
                f"<td class=\"num\">{esc(p.get('owned_shares',''))}</td>"
                f"<td class=\"num\">{esc(fmt_money(p.get('cost_cents')))}</td>"
                f"<td><code>{esc(p.get('put_id',''))}</code></td>"
                f"<td class=\"num\">{esc(fmt_money(p.get('put_strike_cents')) if p.get('put_strike_cents') else '—')}</td>"
                f"<td><code>{esc(p.get('call_id',''))}</code></td>"
                f"<td>{esc(p.get('provenance',''))}</td>"
                "</tr>"
            )
        intent_rows = []
        for sym in sorted(intents.keys()):
            it = intents[sym] if isinstance(intents[sym], dict) else {}
            cand = it.get("candidate") if isinstance(it.get("candidate"), dict) else {}
            intent_rows.append(
                "<tr>"
                f"<td>{esc(sym)}</td>"
                f"<td><code>{esc(it.get('ref_id',''))}</code></td>"
                f"<td><code>{esc(it.get('order_id',''))}</code></td>"
                f"<td><code>{esc(cand.get('id',''))}</code></td>"
                f"<td>{esc(it.get('at',''))}</td>"
                "</tr>"
            )
        inv_html = ""
        if pos_rows:
            inv_html += (f"<table><caption>End inventory &mdash; positions ({len(pos_rows)})</caption>"
                         "<thead><tr><th scope=\"col\">Symbol</th><th scope=\"col\" class=\"num\">Owned shares</th>"
                         "<th scope=\"col\" class=\"num\">Cost / share</th><th scope=\"col\">Put ID</th>"
                         "<th scope=\"col\" class=\"num\">Put strike</th><th scope=\"col\">Call ID</th><th scope=\"col\">Provenance</th></tr></thead>"
                         f"<tbody>{''.join(pos_rows)}</tbody></table>")
        else:
            inv_html += "<p class=\"muted\">No positions in end state.</p>"
        if intent_rows:
            inv_html += (f"<table><caption>Unresolved intents at end ({len(intent_rows)}) &mdash; new risk frozen while any intent is open</caption>"
                         "<thead><tr><th scope=\"col\">Symbol</th><th scope=\"col\">ref_id</th><th scope=\"col\">Order ID</th><th scope=\"col\">Candidate</th><th scope=\"col\">At</th></tr></thead>"
                         f"<tbody>{''.join(intent_rows)}</tbody></table>")
        else:
            inv_html += "<p class=\"muted small\">No unresolved intents at end of window.</p>"
        inventory_section = f"<h2>End inventory</h2><div class=\"card\">{inv_html}</div>"
    else:
        inventory_section = "<h2>End inventory</h2><div class=\"card\"><p class=\"muted\">No end-state snapshot in this report.</p></div>"

    # Blockers / assumptions
    if blockers:
        blocker_html = "<ul>" + "".join(f"<li>{esc(b)}</li>" for b in blockers if isinstance(b, str)) + "</ul>"
    else:
        blocker_html = "<p class=\"muted\">No blockers listed.</p>" if status != "blocked" else "<p class=\"muted\">Blocked with no enumerated reason; treat as unqualified input.</p>"
    if assumptions:
        assumption_html = "<ol>" + "".join(f"<li>{esc(a)}</li>" for a in assumptions if isinstance(a, str)) + "</ol>"
    else:
        assumption_html = "<p class=\"muted\">No assumptions listed.</p>"

    # Spec facts
    peer_sym_str = ", ".join(esc(s) for s in peer_symbols if isinstance(s, str)) if peer_symbols else "—"
    date_note = ""
    if start_missing or end_missing:
        date_note = f" (spec dates missing; showing default window {esc(DEFAULT_START)} to {esc(DEFAULT_END)})"
    corp_str = "audited" if corp_audited is True else ("not audited" if corp_audited is False else "unknown")
    exp_obs = f"{esc(exp_dec)} expected / {esc(obs_dec)} observed decisions; {esc(exp_val)} expected / {esc(obs_val)} observed valuations"

    # Safe embedded JSON: escape script-closing.
    raw_json = json.dumps(report, indent=2, sort_keys=True, default=str)
    raw_json_escaped = raw_json.replace("<", "\\u003c").replace(">", "\\u003e").replace("&", "\\u0026")
    raw_pre = html.escape(raw_json, quote=True)

    if status_invalid:
        invalid_banner = "<div class=\"card\"><p><strong>Invalid status value:</strong> " + esc(raw_status) + " is not blocked, fixture, or counterfactual; rendered as blocked with all metrics N/A.</p></div>"
    else:
        invalid_banner = ""
    title_text = "Blocked simulation" if status == "blocked" else ("Synthetic fixture" if status == "fixture" else "Historical counterfactual")
    doc_title = f"AutoWheel simulation — {title_text}"

    html_doc = f"""<!doctype html>
<html lang="en">
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>{esc(doc_title)}</title>
<style>{CSS}</style>
<main>
<div class="eyebrow">AUTOWHEEL / SIMULATION</div>
<h1>{h1}</h1>
<div class="banner {banner_cls}" role="status"><strong>{banner_title}</strong><br>{banner_body}</div>
{invalid_banner}
<div class="card"><h3>Window and ledger basis</h3>
<dl class="kv">
<dt>Mode (spec)</dt><dd>{esc(spec_mode) if spec_mode else "—"}</dd>
<dt>Window</dt><dd>{esc(start)} to {esc(end)}{esc(date_note) if date_note else ""} <span class="muted">Default six-month window is 2026-04-06 to 2026-10-02 when spec dates are absent.</span></dd>
<dt>Hypothetical start</dt><dd>{esc(fmt_money(initial_cash))} all-cash{esc(initial_cash_note)}</dd>
<dt>Peer reservation (scenario)</dt><dd>{esc(fmt_money(peer_reserve)) if isinstance(peer_reserve,int) and not isinstance(peer_reserve,bool) else "—"} <span class="muted">Symbols: {peer_sym_str}. A fixed cash-reservation scenario; not a historical TradePulse result.</span></dd>
<dt>Source</dt><dd>{esc(source) if source else "—"}</dd>
<dt>Corporate actions</dt><dd>{esc(corp_str)}</dd>
<dt>Coverage check</dt><dd>{esc(exp_obs)}</dd>
<dt>Input SHA-256</dt><dd><code>{esc(input_sha) if input_sha else "—"}</code></dd>
<dt>Strategy SHA-256</dt><dd><code>{esc(strat_sha) if strat_sha else "—"}</code></dd>
<dt>Replay SHA-256</dt><dd><code>{esc(report.get('simulation_engine_sha256','—'))}</code></dd>
</dl>
<p class="muted small">Schema: <code>internal/simulation/types.go</code> Report. Rules: <code>internal/wheel/strategy.go</code> and <code>engine.go</code>; guide: <code>docs/strategy.html</code>. Marking for every valuation: stock at bid, short options at ask.</p></div>
<h2>Data blockers</h2><div class="card">{blocker_html}</div>
<h2>Strategy being simulated</h2>
<div class="card"><p>Scan the fixed 15 symbols; own at most three wheel symbols with one contract each. Sell the nearest eligible weekly put at absolute delta 0.15–0.35, ranked by net premium per collateral dollar per day. After a confirmed assignment and the 24-hour ownership hold, sell the earliest eligible call at delta 0.50–0.85. Highest call delta comes first, including strikes below assignment cost. Calls precede new puts; one submission per five-minute cycle.</p><p class="muted small">No same-day expiration, rolling or repricing. The $80,000 commitment ceiling shares cash capacity with peer reservations. Funding and ownership come from the replay ledger; capital release remains subject to settlement.</p></div>
<h2>Performance</h2>
<div class="card"><table><caption>{"Synthetic fixture metrics — not six-month performance" if status=="fixture" and show_metrics else ("Hypothetical counterfactual metrics" if show_metrics else "No measured performance — all metrics N/A")}</caption>
<tbody>{perf_table}</tbody></table>
<p class="muted small">{esc(perf_note)}</p></div>
{equity_section}
{trades_section}
{inventory_section}
<h2>How to read this report</h2>
<div class="card"><ul>
<li><strong>Premium &ne; profit.</strong> Net P&amp;L includes stock gains/losses, option gains/losses, fees and dividends. Open inventory and short options are marked to market. Drawdown measures the decline in total equity; it is not a separate expense. A high-premium week can still lose money.</li>
<li><strong>Current universe, applied retrospectively.</strong> The 15-symbol scan universe is the current configuration. Running it over April&ndash;October 2026 introduces retrospective bias: symbols liquid today may not have been liquid then, and delisted or illiquid names from that window are absent.</li>
<li><strong>Conservative marking.</strong> Every valuation marks stock inventory at the bid and short-option liability at the ask, so reported equity is pessimistic versus mid-mark.</li>
<li><strong>{esc(fmt_money(initial_cash))} all-cash hypothetical start{esc(initial_cash_note)}.</strong> No margin or deposits. Funding uses settled funds; economic equity includes unsettled receivables. Peer-reserved cash is subtracted from tradable cash in that scenario only.</li>
<li><strong>Peer scenario &ne; TradePulse history.</strong> A fixed peer cash reservation (for example $40,000) and reserved symbols model shared-account contention. It is not TradePulse&rsquo;s actual INTC / MU / AAPL result and must not be quoted as one.</li>
<li><strong>Counterfactual executions are not actual fills.</strong> Fills, 24-hour assignment/call-away inferences, expirations, and T+1 reuse are modeled assumptions with a 1-second latency plus sustained 5-second sized-bid crossing on &le;1-second observations (GFD, no reprice or roll). No broker order was sent.</li>
<li><strong>No fabricated returns.</strong> Blocked status or a missing performance object forces every metric to N/A with no chart. Fixture pages are labeled synthetic and are never six-month performance.</li>
</ul></div>
<h2>Assumptions</h2><div class="card">{assumption_html}</div>
<h2>Data and integrity</h2>
<div class="card"><p class="muted small">A counterfactual is only valid with the full point-in-time dataset described in <code>docs/simulation-design.md</code>: expired-contract listing / weekly / deliverables / tick, NBBO with size, delta and open interest each with publication timestamps, raw stock bid/ask, and audited corporate actions. Missing or ambiguous data blocks the run instead of interpolating. Expected vs observed decision/valuation counts gate completeness; any shortfall must block or be listed above.</p>
<p class="muted small">Vendor entitlement probes and normalized-input audits are separate from simulated returns. Input hashes identify the supplied files; they do not prove broker execution or independently establish chain completeness.</p></div>
<details><summary>Raw report JSON (audit copy)</summary><pre>{raw_pre}</pre></details>
<script type="application/json" id="report-data">{raw_json_escaped}</script>
<p class="muted small">Offline file: no external scripts, stylesheets, images, or network calls. Open locally with any browser. Typography follows the neutral OptionZero-like guide used in <code>docs/strategy.html</code>.</p>
</main>
</html>"""
    return html_doc


def main():
    ap = argparse.ArgumentParser(description="Render AutoWheel simulation Report JSON to offline HTML (stdlib only).")
    ap.add_argument("--report", required=True, help="Input Report JSON path")
    ap.add_argument("--output", required=True, help="Output HTML path")
    args = ap.parse_args()
    report = load_report(args.report)
    out = render(report)
    out_path = Path(args.output)
    out_path.parent.mkdir(parents=True, exist_ok=True)
    out_path.write_text(out, encoding="utf-8")
    print(f"wrote {out_path}")


if __name__ == "__main__":
    main()
