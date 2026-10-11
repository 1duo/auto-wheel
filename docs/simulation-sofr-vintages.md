# Daily historical SOFR archive validation

On October 7, 2026, AutoWheel acquired 128 anonymous public ALFRED CSV vintages
for the frozen April 6–October 2 study. They cover the previous calendar day of
each of the 126 study sessions, plus April 2 and October 6 controls. The complete
offline audit compared 8,130 archived numeric observations. Every session's
selected observation, value and complete candidate rate record matches the
previously reviewed [SOFR candidate](public-sofr-research.md).

The final audit requires the complete set of observations expected to have
been published by each vintage date. Missing older values cannot pass merely
because the most recent value is present. It verifies the requested URL, dated
CSV header, original response and receipt hashes, reviewed acquisition version,
frozen study and candidate hashes, unique dates, and complete source stability.
Eight mutations are rejected, including a missing available value and a wrong
session rate. April 2 contains one available observation; October 6 contains
all 128. No precise intraday publication time is inferred from a daily vintage.

The previous-calendar-day archive supports the candidate for all 9,324 decision
cuts and 126 closing valuations. The archive corroborates its historical date
and value selections. It does not turn the disclosed flat future-rate proxy
into an observed OIS forward curve, qualify the American Greek model, or
produce replay frames or returns. Those acceptance and integration requirements
remain open. Production configuration is unchanged.

Run the complete offline comparison from the checkout with a fresh output:

```sh
python3 scripts/audit-alfred-sofr-vintages-complete.py \
  --source research/simulation/2026-10-05/cycle48-alfred-sofr-vintages-default-client \
  --output research/simulation/2026-10-05/NEW-sofr-vintage-audit.json
```

The MSI audit (local evidence omitted from this source repository)
contains the source bindings, all session selections and eight rejected
controls. Original CSVs and individual HTTP receipts are retained beside the
acquisition aggregate. Earlier audit outputs and source versions are preserved
as evidence of their original runs.

Initial public POST and custom-client requests timed out without accepted
data. The default-client GET run retained 81 successful originals before a
transport failure. Its resume verified those original bytes and receipts,
then fetched the remaining 47. These requests use neither a Databento
credential nor its financial journal. Public read-only transport retries are
bounded separately from the paid acquisition policy.

For a future fresh acquisition of this frozen study, use
`scripts/acquire-alfred-sofr-vintages-v2.py`. Its failure receipts use fresh
UUID names, so repeated failures for one vintage preserve separate evidence.
Each new receipt records its acquisition source version; the aggregate records
its finalizing version and explicitly identifies any resumed legacy receipts
whose original version was not recorded. Four offline file-handling controls
passed, including preservation of existing sources and a forced UUID collision.
This V2 format is separate from the completed V1 acquisition. The complete
auditor intentionally pins and accepts the reviewed V1 source for this study;
a future V2 dataset requires its own reviewed acceptance.

Primary references:

- [ALFRED historical download](https://alfred.stlouisfed.org/series/downloaddata?seid=SOFR)
- [ALFRED download formats](https://alfred.stlouisfed.org/help/downloaddata)
- [ALFRED vintage methodology](https://alfred.stlouisfed.org/help)
- [New York Fed publication and revision policy](https://www.newyorkfed.org/markets/reference-rates/additional-information-about-reference-rates)
