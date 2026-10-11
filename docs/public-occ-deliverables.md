# Public OCC deliverable research

The frozen study still has no qualified corporate-action or deliverable corpus.
The undefined Databento multiplier and quantity fields remain undefined.

Two dated primary memos provide positive Netflix terms before the study:
[OCC 57556](https://infomemo.theocc.com/infomemos?number=57556), dated
November 4, 2025, and [OCC 57651](https://infomemo.theocc.com/infomemos?number=57651),
dated November 14, 2025. They describe a split effective November 17, 2025.
The NFLX option root remains NFLX, the premium/strike-dollar multiplier becomes
100, and the stated deliverable is 100 NFLX common shares. The contract-count
multiplier is 10 and the strike divisor is 10. These are different quantities;
the contract-count multiplier must not become the replay's premium multiplier.

These memos establish terms for the described adjustment. They do not prove
the terms of every newly listed April–October series, absence of later
adjustments, contemporaneous tradability, a complete roster, minimum tick,
weekly classification, or first listing time. No replay field is filled from
a generic standard-contract default or from a negative web-search result.

The initial direct download returned HTTP403. Later Safari acquisition retained
11 original PDFs and 16 individual keyword CSV exports. The recorded file
hashes and sizes are in
the original receipts (local evidence omitted from this source repository)
and the search exports (local evidence omitted from this source repository).
The exports contain 707 distinct memo index rows. The failed combined OR
search missed the known Netflix memos and supplies no completeness proof.

Previously reviewed positive split candidates also include NVDA effective
June 10, 2024 (54623), AVGO effective July 15, 2024 (54716), and NOW effective
December 18, 2025 (57868). These describe the referenced adjustments only;
they cannot establish terms for all newly listed April–October contracts.

`scripts/audit-occ-research-index.py` checks the retained CSV scopes, counts,
overlapping rows, Netflix positive control, and original PDF bytes against
their recorded receipts. It binds the previously reviewed candidate facts
without claiming to re-parse tables, prove an exhaustive search, or qualify
per-series terms. All historical source gates remain pending.

The next source step is a reproducible OCC memo index/search across all 15
roots, including announcements before April 6 that remain relevant during the
study. Preserve posted and effective dates, revisions, affected roots/series,
deliverable terms and original-document hashes when available. A complete
negative-result audit is required before claiming no intervening change;
issuer records and exchange contract rules must corroborate relevant facts.
Ordinary Friday eligibility does not need a weekly flag. Holiday Thursday put
eligibility does. A captured definition supplies an observation time, not an
automatic proof of first activation or tradability.

Cycle39 acquired an unrestricted posted-date index for January 1, 2024 through
October 6, 2026. The broad search showed 5,975 results but its CSV contained
only 2,500 rows. Three yearly exports contain 1,937, 2,243 and 1,795 rows; their
disjoint union matches the observed broad count, all 707 retained keyword
rows and all 11 retained original memo IDs. The receipts and the reproducible
`scripts/audit-occ-unrestricted-index.py` audit retain the truncation explicitly.
This verifies index enumeration within the observed scope. It does not prove
memo content, affected roots, historical revisions, first availability or
pre-2024 terms that remain relevant.

The free [OCC ESS batch endpoint](https://www.theocc.com/market-data/market-data-reports/other-market-data-info/batch-processing/equity-special-settlements-reports-batch-processing)
returned dated special-settlement reports for all 126 frozen sessions and
October 5 and 6 boundary dates. The original report layout PDF was saved in
Safari and visually checked. `scripts/audit-occ-ess-reports.py` reconciles
128 originals and 66,385 records against their dates, page headers and printed
totals. It records 1,062 incomplete component group-days rather than filling
missing components; none references a study symbol under exact product or
delivery-security matching. Two public HTTP520 responses and their successful
single GET recoveries remain retained. These are public read-only requests,
separate from the financial Databento acquisition journal.

[OCC's FAQ](https://www.optionseducation.org/referencelibrary/faq/general-information)
describes the ESS file as covering nonstandard products eligible for clearing
on the previous day, with an effective-date exclusion. Reconcile that lag,
adjustment memos and exact series identities before treating absence as
evidence of standard settlement. Current OCC standard-equity specifications
state a 100-share unit, a $100 premium point and American exercise, but do not
identify the historical terms of an arbitrary series. Report/process dates
and printed times remain source assertions; `known_at` is still unknown.

The new originals and audits are retained under
`research/simulation/2026-10-05/cycle39-public-primary/`. Seven narrow malformed
input controls reject wrong dates, totals, widths, missing footers, duplicate
CSV identities, wrong headers and out-of-scope dates. All source gates and
per-series qualification remain pending. The next step is to join exact
definition roots with daily ESS identities and the complete memo index,
review relevant adjustment contents and prove the applicable standard-rule
history without replacing undefined fields with defaults.

Cycle40 retains three dated OCC originals: the June 2024 options disclosure
document, [OSI memo 26853](https://infomemo.theocc.com/infomemos?number=26853)
and [T+1 memo 54580](https://infomemo.theocc.com/infomemos?number=54580).
The relevant pages were rendered and visually reviewed. The disclosure
document describes 100 shares as the general stock-option unit and explicitly
allows exceptions and adjustments. Memo 26853 states that rare nonstandard
contracts may lack a numeric suffix; cash mergers can also change a
deliverable without changing the root. Root spelling alone cannot classify
a contract's terms. Memo 54580 and the June amendment date the regular
physical stock-option exercise settlement transition to May 28, 2024.
These dated general rules are retained evidence, with first availability
unknown. They do not supply a qualified term record for every study series.

The complete index title census finds 79 exact study-ticker mentions; five
titles name a study option root and match the retained positive split memos.
The other 74 often refer to ETFs with different option roots. This separates
title candidates without certifying other memos irrelevant.

The private evidence join retains all 955,003 requested contract-days across
126 sessions. Captured definitions precede the first decision cut for
954,940 subjects. The remaining 63 are preserved with unknown definitions
and terms. The independent query/gap reconciliation separates 54 missing
DTE1–8 records from nine same-day records; it checks the complete query
universe and the prior mapping audit. All term fields remain null, next-day
ESS observations remain retrospective, and qualification is still pending.
The reproducible join and audit are `scripts/join-occ-contract-term-evidence.py`
and `scripts/audit-occ-contract-term-join.py`. Licensed rows remain on MSI;
only aggregate counts, source paths and hashes are copied to the workspace.
