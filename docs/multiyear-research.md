# 2024–2025 strategy research

The active research goal uses the current ten symbols, one common observed cash
seed, one contract per symbol, three managed slots and a $150,000 offline
collateral ceiling. Development covers July 1–December 31, 2024; evaluation
covers January 2–December 31, 2025. July 1 is the verified start of the official
raw-close source, and the user authorized omitting the unavailable first half.
The same seed starts each independent period. It is a
counterfactual using current cash, not a reconstruction of the account in 2024.

The second half of 2024 is development data. Freeze the selected candidate and its source/code
digests before evaluating 2025 once. Never use 2025 returns to revise parameters
or choose individual tickers. The universe was selected in 2026 and remains
retrospective even with a held-out parameter evaluation.

## Frozen candidates

| Preset | Put delta | Maximum option spread | Covered calls |
| --- | --- | --- | --- |
| conservative-roi-v1 | 0.05–0.15 | 20% | 0.50–0.85 delta, below cost allowed |
| balanced-roi-v1 | 0.15–0.35 | 20% | 0.50–0.85 delta, below cost allowed |
| liquid-roi-v1 | 0.05–0.15 | 10% | 0.50–0.85 delta, below cost allowed |
| cost-protected-roi-v1 | 0.05–0.15 | 20% | 0.50–0.85 delta, strike at or above cost |

All four apply to NVDA, IREN, NOW, AMD, AMZN, GOOGL, META, MSFT, PLTR and AVGO.
They rank eligible nearest-expiry contracts by net quoted ROI per calendar day.
Calls use signed extrinsic credit less the fee; puts use credit less the fee
over secured strike collateral. A quoted ROI is not a forecast of realized
return. Accounts reserve cash and slots before execution. An unfilled selection
is not replaced using knowledge of later fills.

The research loop has a ceiling of 25 experiments, including rejected trials.
Evaluate the four frozen candidates before expanding the search. The development
metric is stressed net P&L minus maximum daily drawdown, in integer cents.
Guards require complete daily valuations, exact reconciled accounting, no
negative secured cash, no limit violations, at least 12 filled sales and
drawdown no more than baseline plus 1% of starting cash. Rank ties by preset ID.
The baseline is conservative-roi-v1. Keep it if no candidate clears the guards.
Compare the frozen winner and baseline in 2025 at both cost cases; a failed
held-out check prevents promotion and does not start another tuning round.

## Running offline

```sh
go build -o bin/autowheel-study ./cmd/autowheel-two-month-study
bin/autowheel-study --shared-portfolio --calendar-year 2024 --plan-only --output /private/plan-2024.json
bin/autowheel-study --shared-portfolio --calendar-year 2025 --plan-only --output /private/plan-2025.json
bin/autowheel-study --shared-portfolio --calendar-year 2024 --input /private/2024 --starting-cash-cents CASH_CENTS --output /private/results-2024.json
python3 scripts/select-multiyear-strategy.py --development-report /private/results-2024.json --output /private/frozen-selection.json
bin/autowheel-study --shared-portfolio --calendar-year 2025 --input /private/2025 --starting-cash-cents CASH_CENTS --frozen-selection /private/frozen-selection.json --output /private/results-2025.json
```

The selector needs Python 3.11+. The Go evaluation independently reconstructs
its development metric and rejects a changed winner or cash seed. It evaluates
only that candidate and the baseline in 2025. It never promotes a live strategy.

Every symbol needs `inputs/SYMBOL.json` using
`AutoWheelCalendarYearEndpointInputV1`, all scheduled sessions, correctly timed
13:00/16:00 closes, raw official closing references, decision books received
before 09:35 and execution observations at least one second afterward.
`corporate-actions.json` uses `AutoWheelCalendarYearActionsV1`, covers every
symbol and the complete study period, and supplies source-pinned `actions` and
`dividends`. Every referenced source is hashed before replay. Six cents per
contract and $2 plus a one-cent price allowance are separate cost cases.

The calendar includes the January 9, 2025 mourning closure and each historical
early close. A held unsupported corporate action stops the run. Do not erase
inventory, resize option exposure without OCC evidence, or combine adjusted
stock prices with raw option strikes. Broadcom's July 2024 and ServiceNow's
December 2025 splits require explicit coverage. NVIDIA's June 2024 split precedes
the flat July start; historical prices must use the correct post-split basis.

Outputs include cash, stock and short-option liability ledgers, total return,
drawdown, cash utilization and each month's closing cash and equity. Missing
marks remain null. The endpoint simulator leaves historical performance and
production qualification false: it does not prove complete chains, causal OI,
American early exercise, settlement delays or continuous pending-order fills.
Do not represent its output as qualified historical performance or a completed
strategy promotion.

Continuous confirmation uses `cmd/autowheel-sim` with the explicit spec variant
`calendar_2024_2025_roi_v1`. It accepts the same exact periods, ten symbols,
named presets, three slots, observed cash seed and $150,000 offline ceiling.
It requires the native five-minute decision schedule, complete chains, causal
OI/Greeks and continuous pending-order observations. Its fee cases are six cents
and $2; it does not apply the weekly endpoint diagnostic's extra one-cent
allowance. Treat this as a separate execution model, not the frozen development
metric. Full-2025 input requires the same frozen 2024 winner proof and accepts
only that winner or the baseline. Bind it with
`--frozen-selection /private/frozen-selection.json` when invoking
`go run ./cmd/autowheel-sim --input /private/2025.jsonl --output /private/continuous-2025.json`.
The CLI embeds the exact selection and development-report bytes before replay;
the report hashes both the original and effective streams. Direct library
inputs require that same embedded evidence. Changed evidence, seed, calendar,
universe, limits or an unselected preset blocks evaluation. Development cannot
consume the selection. Evidence is bounded to 1 MiB for selection and 16 MiB
for the report and does not itself qualify prices or accounting declarations.
Empty or incomplete input produces no performance. Reports pin the
preset source, engine source and executing binary. The declared source hashes
still require independent verification against the built executable and data.

## Preparing a switch

The implementation supports these presets, but they are research candidates
until the historical evidence and held-out checks pass. Existing production
collateral limits remain $80,000. The $150,000 limit belongs to offline research.

An authenticated local `POST /control/strategy/PRESET` with
`X-AutoWheel-Control: confirm` first persists HALT and revokes LIVE.json. It saves
the new policy to owner-only `selection.json`, clears cached candidates and
readiness, and blocks resume/live until restart. Verify the loaded preset after
restart, reconcile broker ownership and pass fresh readiness before deliberate
activation. The activation is bound to the executable and configuration digest.
For rollback, restore the prior private configuration and selection policy while
paused, restart, and repeat readiness. A prior activation record is never reused.

Research does not authorize deployment, activation, peer liquidation or use of
TradePulse inventory. Provider credentials, account seeds, licensed captures,
experiment logs and performance artifacts stay outside Git.

## Primary references

- [NYSE 2024 calendar](https://www.nyse.com/publicdocs/ICE_NYSE_2024_Yearly_Trading_Calendar.pdf)
- [NYSE 2025 calendar](https://www.nyse.com/publicdocs/ICE_NYSE_2025_Yearly_Trading_Calendar.pdf)
- [January 9, 2025 closure](https://www.nyse.com/publicdocs/nyse/markets/american-options/rule-interpretations/2025/National_Day_of_Mourning_20250102.pdf)
- [NVIDIA OCC adjustment](https://infomemo.theocc.com/infomemos?number=54623)
- [Broadcom 2024 split announcement](https://investors.broadcom.com/static-files/df9ff86d-bc30-4901-9884-299b17619d46)
- [ServiceNow split approval](https://investor.servicenow.com/news/news-details/2025/ServiceNow-Shareholders-Approve-5-for-1-Stock-Split/default.aspx)
- [Databento official raw closes](https://databento.com/docs/examples/equities/closing-prices)

Provider metadata checked on October 11, 2026 covers both years for OPRA CMBP-1
and EQUS.MINI quotes. EQUS.SUMMARY official closes start July 1, 2024. The study
starts there and does not substitute EQUS.MINI trade bars for official closes.
