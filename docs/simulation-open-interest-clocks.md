# Open-interest clocks in the replay

An unknown economic reference date is distinct from unknown availability.
The current replay requires `open_interest_published_at`; it has no field or
validation rule requiring an OI economic reference date. The native reducer
already keeps an observed quantity when `ts_ref` is undefined, provided its
other checks pass. This finding does not qualify incomplete source coverage.

Databento's [OPRA dataset documentation](https://databento.com/docs/venues-and-datasets/opra-pillar)
describes `ts_recv` as the capture server's receive time and `ts_event` as the
OPRA consolidator's processing time. For an offline replay, receive time can
bound when that capture knew the observation. It does not establish the
statistic's economic date, earliest publication, Robinhood delivery time or
first availability to a particular trading account.

The [statistics schema](https://databento.com/docs/schemas-and-data-formats/statistics)
describes `ts_ref` separately as a reference timestamp and documents typed
missing-value sentinels. Its current quantity width is not the width of the
retained AS_IS `StatMsgV1` records: the audited native reducer must continue to
use its pinned `int32` width and `INT32_MAX` sentinel.

The retained [OPRA recipient specification](https://cdn.opraplan.com/documents/OPRA_Pillar_Output_Specification.pdf),
version 6.4b dated August 25, 2026, shows an OI message layout without a separate
economic reference-date field (printed page 28). Printed page 45 describes the
start/end transmission controls and participant-coded OI messages; page 60
defines OI volume. These pages and the cover were rendered and visually
checked. This current revision does not prove earlier revision continuity for
the entire April–October study.

The two downloaded Databento HTML bodies are identical application shells;
they do not contain the field descriptions. Their receipts preserve this
limitation. The descriptions above were inspected through the rendered web
source, rather than asserted from those shell files. HTTP modification and
download times do not prove historical first publication.

The existing `internal/causaloi` component remains appropriate: use closed
receive-time cuts, preserve the original event clock, keep a missing reference
null, retain observed zero, and withhold unsupported updates, publisher
conflicts or unavailable definitions. Every staged observation stays
provisional until full source checks finish. Publisher copies are not summed,
and a filename or scheduled dissemination time cannot supply a missing date.

A future replay adapter must declare its capture-availability interpretation
explicitly and retain the literal receive time. No adapter or qualification
gate is changed by this review. Complete statistics coverage, contract terms,
tradability, execution books, causal Greek inputs and lifecycle qualification
remain required before either mode has a historical performance report.

Original public files and receipts are under
`research/simulation/2026-10-05/cycle45-OI-primary/`. Licensed market rows remain
private on MSI. The frozen replay engine, strategy and production state are
unchanged.
