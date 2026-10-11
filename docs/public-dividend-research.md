# Public dividend declaration evidence

The offline audit `scripts/audit-public-dividend-declarations.py` checks five
hash-pinned issuer pages captured on October 6, 2026. It verifies positive
common-share cash declarations for NVDA and ORCL. It records exact cash cents,
the displayed announcement date, record date and payment date. It emits no
pricing dividend schedule, ex-date or historical known-at timestamp.

The two NVDA releases display May 20 and August 26, 2026 and declare $0.25 per
common share. The three ORCL releases display March 10, June 10 and September
10, 2026 and declare $0.50 per common share. Sources, hashes and failed/successful
public HTTP receipts are retained in
`research/simulation/2026-10-05/public-inputs/dividend-declarations/`.

The Oracle investor site returned HTTP 403 to the source-capture tool. The
equivalent official Oracle newsroom pages returned HTTP 200 and were saved;
the failed attempts remain in the receipts. No request was paid or authenticated.

Record dates have not been reinterpreted as ex-dates. A board-action date
does not establish public availability. The current issuer-page capture does
not establish a historical vintage or precise publication time. Nothing here
establishes that these are all declarations for either issuer, that any of
the other thirteen symbols had no dividend, or that future declared amounts
were known before announcement. Expected-dividend completeness, causal source
availability, corporate actions and independently qualified American Greeks
remain pending. Both six-month studies remain incomplete.

The audit rejects changed source bytes, missing/ambiguous evidence, oversized
sources, symlinks and non-regular files. Ten controls use the actual five saved
sources; four additional checks reject symlinks, FIFOs, ambiguous dates and
missing declarations. An independent bounded review passed and isolated Linux
output is byte-identical to local output. The validation and code-review files
record this evidence preparation; they do not qualify historical inputs.

Five matching SEC 8-K accession records are now bound to raw SEC submissions
JSON captured on October 6. Their acceptance timestamps agree with the filing
index clocks after conversion from New York time to UTC. The corresponding
SEC filing or furnished exhibit contains the same common-share amount, record
date and payment date as the saved issuer source. The acceptance pack is
`edgar-capture-20261006/acceptance-evidence-20261006.json` beneath the evidence
directory. Original archive HTML capture failed; document comparison uses
explicitly recorded primary SEC rendered-text inspection.

Acceptance time is not a proved public-availability upper bound. The
[SEC's timestamp and dissemination guidance](https://www.sec.gov/about/webmaster-frequently-asked-questions)
says the usual delay can increase and is not guaranteed; it exposes no first
public timestamp. Consequently the pack emits acceptance evidence while
keeping `known_at_utc`, ex-dates and source qualification unset. A fixed delay
or next-day availability rule would be a separately accepted modeling
assumption. Neither is silently applied here.
