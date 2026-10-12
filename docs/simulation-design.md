# Simulation design

AutoWheel research is offline and cannot bind a production account, OAuth grant, or coordinator. Inputs and generated account reports are supplied privately and are excluded from this repository. Older research notes describe their original configurations and evidence; use the input's declared spec when interpreting them.

## Causal quote replay

`cmd/autowheel-sim` consumes a JSONL `AutoWheelReplaySpecV1` followed by strictly increasing frames. The spec fixes dates, initial cash, fee stress, optional peer reservations, source, corporate-action audit, and strategy configuration. The current baseline uses the ten configured symbols, three managed slots, an $80,000 ceiling, and the legacy balanced put band; explicit named modes require their exact preset bands. Production `init` defaults to conservative.

A scheduled decision requires complete point-in-time chain coverage and all ranking-relevant inputs. The reducer retains receive/event/availability clocks, source digests, original contract identities, standard deliverables, quote sizes, and published open interest. A future, stale, missing, malformed, or unqualified observation blocks a historical run. Specialized omission/exclusion proofs must establish that omitted contracts cannot change selection or execution; they never invent missing bids or Greeks.

Selection chooses the nearest eligible expiry before liquidity ranking. Legacy calls prioritize delta, then time-value ROI; legacy conservative puts prioritize lower absolute delta. Named ROI presets rank by net quoted ROI per calendar day. Execution reselects through the production engine using the simulated snapshot.

The explicit `calendar_2024_2025_roi_v1` research variant accepts July–December
2024 or full 2025, the ten-symbol universe, one of the four frozen ROI presets,
three slots and a $150,000 offline ceiling. It permits the observed cash seed
and six-cent/$2 fee cases while preserving the production $80,000 validation
limit. It requires every native five-minute decision and session valuation;
declaring coverage alone produces no returns. This continuous execution model
does not include the weekly diagnostic's additional one-cent price allowance.

The replay models a GFD sell limit at the decision bid, one-second latency, and five seconds of sized bid dwell with observations no more than one second apart. It does not prove queue priority, broker acceptance, actual scan/review latency, or Robinhood fills. Explicit lifecycle and settlement events govern assignment, call-away, and cash/share reuse. The production ownership delay and unresolved-intent fence remain active.

NAV is economic cash plus stock at bid minus short-option liabilities at ask. Premiums alone are not profit. Fees, stock losses, and unsettled obligations remain in accounting. Drawdown is measured at session closes; no interest, taxes, or data-subscription costs are modeled.

## Output qualification

- `blocked`: reasons only; no partial performance, curve, trades, or end state.
- `fixture`: prominently synthetic; invented observations validate accounting and safety.
- `counterfactual`: completed declared observations under explicit modeling assumptions; never live broker performance.

`AccountingFixture` uses invented NVDA/AMD/IREN identities from the current universe. Its five-session path collects $400 premium, pays $2 fees, and loses $500 on stock call-away, ending $102 below its $80,000 starting cash. These prices and events are fabricated test inputs.

```sh
go run ./cmd/autowheel-sim --fixture --output /tmp/autowheel-fixture-new.json
python3 scripts/render-simulation.py --report /tmp/autowheel-fixture-new.json \
  --output /tmp/autowheel-fixture.html
```

Evidence output paths must be new. Historical sources require independent contract, corporate-action, quote, open-interest, exercise/dividend, and coverage qualification; an incomplete source must remain blocked.

## Weekly studies and shared cash

The month/symbol/two-month study commands are separate research models. The shared portfolio in `cmd/autowheel-two-month-study` accepts `--shared-portfolio`, `--starting-cash-cents`, and `--max-collateral-cents`; the last flag defaults to $150,000 for this offline study. A ceiling does not create cash or permit leverage.

The portfolio ranks eligible contracts by projected net ROI per calendar day using only decision-time information, freezes admissions before examining execution, and maintains one common cash ledger with up to three occupied slots. Owned stock continues to consume a slot. The first execution book and fixed limit determine the modeled fill; failed fills are not replaced with hindsight winners. Daily valuation includes cash and owned stock, exact dividend accounting, fees, assignment, and settlement under its disclosed assumptions.

Current account cash used to seed a past year is a counterfactual starting balance. A symbol universe selected later in 2026 introduces retrospective selection bias. Neither should be presented as a blind out-of-sample result. Base and fee/slippage stress can follow different trade paths, so their returns need not be monotonic.

Native market-data reducers, causal windows, American-option qualification, and source/acquisition proof tools live in the remaining packages and research scripts. Their historical notes describe evidence that is not bundled here. Vendor credentials, paid acquisition authorization, and licensed datasets must be supplied separately.
