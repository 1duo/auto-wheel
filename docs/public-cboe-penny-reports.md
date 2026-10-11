# Historical Cboe penny-report evidence

The research retains all 126 Cboe Options daily penny reports for April 6–October 2,
2026. The [official report index](https://www.cboe.com/markets/us/options/market-statistics/penny-tick-types/cone)
links dated CSV files. The separate public-series and underlying reference files
describe current data; they have not been substituted for historical terms.

`scripts/acquire-cboe-penny-reports.py` binds the exact frozen calendar, retains
original responses and receipts, and uses serial public GETs. It makes no
Databento financial request. Failed captures stop without automatic retry or
original replacement. `scripts/audit-cboe-penny-reports.py` independently checks
every date, URL, original hash, receipt, row shape and root uniqueness.

The actual audit covers 229,644 public rows and all 1,890 requested root/date
subjects. Fourteen study roots have `Pennies to 3.00` on all 126 dates. NOW has
that positive classification on 66 dates from July 1; its 60 earlier missing
observations remain unknown. Absence from a penny report does not establish a
non-penny tick, tradability or standard deliverable. The 154 non-study binary
rows with another tick label are retained and counted without interpretation.

Seven HTTP modification clocks fall after the corresponding first decision.
Those clocks are retrieval metadata, not proof of first publication, complete
revision history or historical availability. Every `known_at` remains null.
Tick-rule thresholds, exchange/routing applicability and publication policy
still need primary corroboration. No replay minimum tick, premium multiplier,
deliverable, currency or weekly flag has been filled from these reports.

The public acquisition, audit and originals are under
`research/simulation/2026-10-05/cycle44-public-primary/`; nine final boundary
controls include a poisoned HTTP200 manual-resume case. The original capture
code is retained and hash-bound after tightening the resume prefix check.
A direct HTML download of the current report
index returned HTTP403 before body retention; that failed access is recorded,
and no other planned HTML requests were sent by that helper.

The [OCC data-sales page](https://www.theocc.com/market-data/market-data-reports/other-market-data-info/data-sales)
offers a daily product/series feed, but the published offer does not establish
availability of archived intraday or historical revisions for this study. No
subscription or historical archive was purchased. Full per-series terms and
all nine source gates remain pending; the research has no replay performance.
