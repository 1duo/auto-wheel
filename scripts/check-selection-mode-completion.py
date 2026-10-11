#!/usr/bin/env python3
"""Completion gate for explicit Conservative/Balanced offline studies.

Reuse every historical coverage, financial, source and independent-review
requirement of the baseline checker; only the immutable study identity differs
(distinct directory, spec digest and explicit selection_mode config).
This command cannot complete the frozen baseline goal or the zero-floor
put-risk-cap variant: both lack the required explicit selection_mode.
"""

import argparse
import importlib.util
import json
from pathlib import Path

MODE_SPECS = {
    "conservative": {
        "spec_path": "research/simulation/2026-10-05/mode-conservative-05015/spec.jsonl",
        "spec_sha256": "55fbfe17b47d9b85d98a3010ae2b9d744a55bde2937018619a742d19b6fa77b6",
        "study": "selection_conservative",
    },
    "balanced": {
        "spec_path": "research/simulation/2026-10-05/mode-balanced-1535/spec.jsonl",
        "spec_sha256": "73dcb1ff6f9f81be71f419540c3fca19f169d86c5108b34581a125c1c53698ab",
        "study": "selection_balanced",
    },
}


def baseline_checker():
    path = Path(__file__).resolve().with_name("check-simulation-completion.py")
    spec = importlib.util.spec_from_file_location("autowheel_completion", path)
    if spec is None or spec.loader is None:
        raise ValueError("baseline completion checker unavailable")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--mode", required=True, choices=sorted(MODE_SPECS))
    parser.add_argument("--report", type=Path, required=True)
    parser.add_argument("--qualification", type=Path, required=True)
    args = parser.parse_args()
    entry = MODE_SPECS[args.mode]
    try:
        baseline = baseline_checker()
        report = baseline.read_json(args.report)
        qualification = baseline.read_json(args.qualification)
        gaps = baseline.check(
            report,
            qualification,
            Path.cwd(),
            frozen_spec_path=entry["spec_path"],
            frozen_spec_sha256=entry["spec_sha256"],
            research_variant="",
        )
        try:
            actual = report.get("spec", {}).get("strategy_config", {}).get("selection_mode")
        except AttributeError:
            actual = None
        if actual != args.mode:
            gaps = gaps + ["selection mode mismatch: expected " + args.mode]
    except (OSError, ValueError, TypeError, AttributeError) as error:
        gaps = ["completion evidence unavailable: " + type(error).__name__]
    print(json.dumps({"study": entry["study"], "mode": args.mode, "complete": not gaps, "units_remaining": len(gaps), "gaps": gaps}, allow_nan=False))
    raise SystemExit(1 if gaps else 0)


if __name__ == "__main__":
    main()
