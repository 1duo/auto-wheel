# Corporate-action revision sourcing for the full replay

The historical-data spending cap is removed. The sole MSI statistics writer
continues separately. This research prepares a request; it does not buy a
subscription, allocate identifiers, fetch account records or qualify a replay.

The original public responses, official SDK at commit
`25eef4ee8e66559e2370166a57c20925ce244d49`, and the offline request audit are retained
under `research/simulation/2026-10-05/cycle49-reference-*`. Anonymous provider
metadata returned 60 event codes and 235 enumeration groups. These catalogs
describe fields and cancellation states; they do not establish historical
coverage, account entitlement, current price or an event's absence.

The SDK's `pit` argument controls client processing and is absent from the
server request. Its default sorts by `ts_record`, groups by
`event_unique_id` and aggregates with `last`. That discards older revisions
and can combine nullable fields from different revisions. Retain the raw JSONL
versions and their original nulls and cancellation records instead.

The prepared request uses `index=ts_record`, the frozen 15 symbols with
`stype_in=nasdaq_symbol`, and an exclusive end of October 3, 2026 UTC. This
retains revisions already recorded during the study for events with later
ex-dates. Post-study revisions are unnecessary for decisions ending October 2.
The January 1, 1970 lower bound requests available seed history; it makes no
claim about when provider coverage begins. Identity matching and complete
pre-April 6 seed state remain acceptance requirements. Do not filter the
request by ex-date, event-date or a guessed security-type code.

`allocate_isins=False` omits events requiring a new identifier allocation;
such a response cannot establish a complete history or no-dividend state.
The prepared raw request uses `allocate_isins=True` but is explicitly
non-executing. Before dispatch, confirm the reference plan, actual price,
allocation behavior and complete provider coverage. A new recurring or annual
subscription has not been enrolled. Wait for the owned financial writer to
finish before any financial or allocation request.

For each decision, use intact revisions with confirmed per-version
availability at or before the cut. Confirm the meanings of `ts_created` and
`ts_record`; calendar announcement dates do not supply publication times.
Resolve action codes and reject cancelled, deleted, approximate, TBA,
unsupported or ambiguous benefits. Require verified cash currency, amount,
identity and explicit ex-date. Do not derive ex-dates from record dates or
treat current data as historical evidence. Stock security-master lot size does
not establish an option's deliverable or premium multiplier.

No source gate changes as a result of this work. Causal Greeks, complete books,
dated option terms and lifecycle validation remain pending.

Primary references:
[official SDK](https://github.com/databento/databento-python/blob/25eef4ee8e66559e2370166a57c20925ce244d49/databento/reference/api/corporate.py),
[provider schema](https://databento.com/docs/schemas-and-data-formats/corporate-actions),
[reference product](https://databento.com/corporate-actions),
[FINRA distribution dates](https://www.finra.org/rules-guidance/rulebooks/finra-rules/11140).
