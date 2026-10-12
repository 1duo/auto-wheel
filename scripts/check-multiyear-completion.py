#!/usr/bin/env python3
"""Fail-closed completion predicate for the separately frozen 2024–2025 goal."""
import argparse
import hashlib
import json
from pathlib import Path

SYMBOLS = ["NVDA", "IREN", "NOW", "AMD", "AMZN", "GOOGL", "META", "MSFT", "PLTR", "AVGO"]
GATES = ("point_in_time_chains", "quote_sizes_and_clocks", "causal_open_interest_and_greeks",
         "raw_stock_and_official_closes", "corporate_actions_and_deliverables",
         "continuous_execution", "american_exercise_and_settlement", "independent_accounting_review")


def inspect(root):
    gaps = []
    manifest_path = root / "completion-manifest.json"
    if not manifest_path.is_file():
        return ["completion_manifest", "qualified_2024_replay", "qualified_2025_replay", "frozen_selection", "held_out_acceptance", "symbol_presets", "source_publication"]
    manifest = json.loads(manifest_path.read_bytes())
    if manifest.get("schema") != "AutoWheelMultiyearCompletionV1":
        return ["invalid_manifest_schema"]
    pinned = manifest.get("artifact_sha256", {})
    verified = set()
    for name, digest in pinned.items():
        path = root / name
        if path.is_file() and not path.is_symlink() and path.resolve().is_relative_to(root.resolve()):
            with path.open("rb") as stream:
                if hashlib.file_digest(stream, "sha256").hexdigest() == digest:
                    verified.add(name)
    def document(key):
        name = manifest.get(key)
        if name not in verified:
            gaps.append(key)
            return {}
        return json.loads((root / name).read_bytes())
    for year in (2024, 2025):
        expected_start, expected_months = ("2024-07-01", 6) if year == 2024 else ("2025-01-02", 12)
        replay = document(f"replay_{year}")
        qualification = document(f"qualification_{year}")
        if not replay:
            continue
        if replay.get("year") != year or replay.get("symbols") != SYMBOLS or replay.get("historical_performance_qualified") is not True:
            gaps.append(f"qualified_{year}_replay")
        if replay.get("start") != expected_start or replay.get("end") != f"{year}-12-31":
            gaps.append(f"coverage_period_{year}")
        if replay.get("max_managed_symbols") != 3 or replay.get("max_contracts_per_symbol") != 1 or replay.get("max_collateral_cents") != 15_000_000:
            gaps.append(f"scope_{year}")
        if qualification.get("replay_sha256") != pinned.get(manifest.get(f"replay_{year}")):
            gaps.append(f"bound_qualification_{year}")
        for gate in GATES:
            if qualification.get("gates", {}).get(gate) is not True:
                gaps.append(f"{year}_{gate}")
        for result in replay.get("results", []):
            performance = result.get("performance", {})
            if result.get("cash_ledger_reconciled") is not True or result.get("pnl_reconciled") is not True or performance.get("daily_valuation_complete") is not True or len(performance.get("end_of_month", [])) != expected_months:
                gaps.append(f"complete_accounting_{year}")
                break
        if not replay.get("results"):
            gaps.append(f"performance_{year}")
    selection = document("frozen_selection")
    if selection.get("development_year") != 2024 or selection.get("held_out_year") != 2025 or selection.get("protocol") != "2024-h2-stressed-pnl-minus-drawdown-v1":
        gaps.append("development_holdout_separation")
    acceptance = document("held_out_acceptance")
    if acceptance.get("passed") is not True or acceptance.get("selection_sha256") != pinned.get(manifest.get("frozen_selection")):
        gaps.append("held_out_acceptance")
    presets = document("symbol_presets")
    if presets.get("symbols") != SYMBOLS or presets.get("paused_default") is not True or presets.get("switch_and_rollback_verified") is not True:
        gaps.append("symbol_preset_readiness")
    publication = document("source_publication")
    if publication.get("repository") != "1duo/auto-wheel" or publication.get("verified_remote_head") is not True or publication.get("credential_scan_passed") is not True:
        gaps.append("source_publication")
    return sorted(set(gaps))


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--root", required=True, type=Path)
    args = p.parse_args()
    try:
        gaps = inspect(args.root)
    except (OSError, ValueError, TypeError, KeyError):
        gaps = ["invalid_or_unreadable_completion_evidence"]
    print(json.dumps({"complete": not gaps, "units_remaining": len(gaps), "gaps": gaps}, sort_keys=True))
    return bool(gaps)


if __name__ == "__main__":
    raise SystemExit(main())
