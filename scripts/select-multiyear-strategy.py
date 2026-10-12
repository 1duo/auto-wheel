#!/usr/bin/env python3
"""Freeze a research candidate using 2024 alone; never consume 2025 outcomes."""
import argparse
import datetime as dt
import hashlib
import json
import os
from pathlib import Path

PRESETS = ("conservative-roi-v1", "balanced-roi-v1", "liquid-roi-v1", "cost-protected-roi-v1")
BASELINE = PRESETS[0]
PROTOCOL = "2024-h2-stressed-pnl-minus-drawdown-v1"


def sha(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def integer(value):
    return type(value) is int


def metrics(row, seed, ceiling):
    points = row["daily_equity"]
    peak, drawdown = seed, 0
    for point in points:
        equity = point["equity_cents"]
        if not integer(equity):
            raise ValueError("incomplete daily valuation cannot score a candidate")
        peak = max(peak, equity)
        drawdown = max(drawdown, peak-equity)
        if not (0 <= point["available_cash_cents"] <= point["cash_cents"] and
                0 <= point["committed_cents"] <= ceiling and
                0 <= point["active_symbols"] <= 3):
            raise ValueError("cash, collateral or slot violation")
    pnl = points[-1]["equity_cents"] - seed
    if row["net_pnl_cents"] != pnl or row["cash_ledger_reconciled"] is not True or row["pnl_reconciled"] is not True:
        raise ValueError("accounting does not reconcile")
    if row["performance"]["max_drawdown_cents"] != drawdown:
        raise ValueError("reported drawdown differs from complete daily path")
    sales = sum(t["event"].startswith("modeled_sell_") for t in row["trade_ledger"])
    return dict(net_pnl_cents=pnl, max_drawdown_cents=drawdown, sales=sales, score_cents=pnl-drawdown)


def select(report):
    if report.get("schema") != "AutoWheelCalendarYearPortfolioDiagnosticV1" or report.get("year") != 2024:
        raise ValueError("selection accepts the frozen July–December 2024 development report only")
    if report.get("start") != "2024-07-01" or report.get("end") != "2024-12-31":
        raise ValueError("development report has the wrong coverage period")
    seed, ceiling = report["starting_cash_cents"], report["max_collateral_cents"]
    if not integer(seed) or seed <= 0 or ceiling != 15_000_000:
        raise ValueError("invalid frozen cash or collateral scope")
    rows, measured = {}, {}
    for row in report["results"]:
        key = row["strategy"], row["fee_per_contract_cents"], row["price_allowance_cents"]
        if key in rows or key[0] not in PRESETS or key[1:] not in ((6, 0), (200, 1)):
            raise ValueError("duplicate or unexpected experiment")
        dates = [p["date"] for p in row["daily_equity"]]
        if len(dates) != 128 or dates[0] != "2024-07-01" or dates[-1] != "2024-12-31" or dates != sorted(set(dates)):
            raise ValueError("incomplete development calendar")
        if row["starting_cash_cents"] != seed:
            raise ValueError("candidates did not use the same starting cash")
        rows[key] = row
        measured[key] = metrics(row, seed, ceiling)
    expected = {(p, fee, allowance) for p in PRESETS for fee, allowance in ((6, 0), (200, 1))}
    if set(rows) != expected:
        raise ValueError("the frozen four-candidate cost grid is incomplete")
    evaluated = []
    for preset in PRESETS:
        guards = all(measured[(preset,) + cost]["sales"] >= 12 and
                     measured[(preset,) + cost]["max_drawdown_cents"] <= measured[(BASELINE,) + cost]["max_drawdown_cents"] + seed//100
                     for cost in ((6, 0), (200, 1)))
        stressed = measured[preset, 200, 1]
        evaluated.append(dict(preset=preset, guard_passed=guards, **stressed))
    eligible = [r for r in evaluated if r["guard_passed"]]
    winner = min(eligible, key=lambda r: (-r["score_cents"], r["preset"]))["preset"] if eligible else BASELINE
    return winner, evaluated


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--development-report", required=True, type=Path)
    p.add_argument("--output", required=True, type=Path)
    args = p.parse_args()
    report = json.loads(args.development_report.read_bytes())
    winner, experiments = select(report)
    output = dict(schema="AutoWheelFrozenMultiyearSelectionV1", protocol=PROTOCOL,
                  development_year=2024, held_out_year=2025, candidate=winner, baseline=BASELINE,
                  development_start="2024-07-01", development_end="2024-12-31", held_out_start="2025-01-02", held_out_end="2025-12-31",
                  development_report=str(args.development_report.resolve()), development_report_sha256=sha(args.development_report),
                  selector_sha256=sha(Path(__file__)), frozen_at=dt.datetime.now(dt.timezone.utc).isoformat(),
                  experiments=experiments, experiments_completed=4, max_experiments=25,
                  production_qualified=False, promotion_status="research_candidate_only",
                  historical_performance_qualified=report.get("historical_performance_qualified") is True)
    os.umask(0o077)
    with args.output.open("x") as f:
        json.dump(output, f, indent=2, allow_nan=False)
        f.write("\n")
    print(json.dumps({"candidate": winner, "experiments": 4, "promotion_status": output["promotion_status"]}))


if __name__ == "__main__":
    main()
