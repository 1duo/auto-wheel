#!/usr/bin/env python3
"""Completion gate for the separately pinned 0.15 put-risk-cap study.

Reuse every historical coverage, financial, source and independent-review
requirement of the baseline checker; only the immutable study identity differs.
This command cannot complete the frozen baseline goal.
"""

import argparse
import importlib.util
import json
from pathlib import Path

VARIANT = "weekly_put_risk_cap_v1"
SPEC_PATH = "research/simulation/2026-10-05/put-risk-cap-015/spec.jsonl"
SPEC_SHA256 = "2e81d451c599706495c5316d9a3a2f7efe6d80125d8072ae9da6ba86da4a56f1"


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
    parser.add_argument("--report", type=Path, required=True)
    parser.add_argument("--qualification", type=Path, required=True)
    args = parser.parse_args()
    try:
        baseline = baseline_checker()
        gaps = baseline.check(
            baseline.read_json(args.report),
            baseline.read_json(args.qualification),
            Path.cwd(),
            frozen_spec_path=SPEC_PATH,
            frozen_spec_sha256=SPEC_SHA256,
            research_variant=VARIANT,
        )
    except (OSError, ValueError, TypeError, AttributeError) as error:
        gaps = ["completion evidence unavailable: " + type(error).__name__]
    print(json.dumps({"study": VARIANT, "complete": not gaps,
                      "units_remaining": len(gaps), "gaps": gaps}, allow_nan=False))
    raise SystemExit(1 if gaps else 0)


if __name__ == "__main__":
    main()
