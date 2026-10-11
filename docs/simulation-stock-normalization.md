# Stock candidate normalization

The offline `internal/replaynormalize` component converts independently
attested closed MBP stock observations into candidate `simulation.Stock`
values. It is outside the frozen replay engine and has no broker interface.
No historical source gate, replay frame, fill or performance follows from a
successful conversion.

The explicit `raw_book_receive_with_event_age_v1` policy uses the original
BOOK receive timestamp as `Stock.At`. It retains the event timestamp, event
completion time and observation cut separately. Completion or a newer cut
cannot refresh an old quote. Both receive and event ages must satisfy the
snapshot's bound, which is at most the frozen 30 seconds. A zero, undefined,
leading, future or unrepresentable clock fails. Neither clock is calibrated
to a Robinhood broker timestamp.

The helper rejects missing, pending, ambiguous, reset, stale, unknown-depth or
invalid stock books, unsupported publisher/schema fields, crossed quotes and
zero sizes. The source adapter owns the complete native digest, acquisition,
metadata, instrument mapping and closed-capture evidence. An identity label
or caller-supplied hash alone supplies no historical qualification.

Native signed prices use exactly 10,000,000 native units per cent. Only
positive exact-cent values can be represented by the frozen integer-cent
replay. Sentinels, nonpositive and subcent prices fail without rounding. A
rejection returns a zero value and stays explicit; a trade or previous quote
cannot substitute for it.

The targeted Linux tests passed for exact timestamps and integer boundaries,
inclusive freshness, event/receive separation, future completion, source
identity, unknown depth, incomplete events, invalid books and subcent prices.
Only this package was tested; only the offline research CLI was built.

The actual April 6 pass consumed all 9,786,851 native stock records. All 75
closed cuts and 1,125 stock observations matched an independent conversion
from the previously verified capture reference. It accepted 1,122 candidates
and retained three book rejections. Native, compressed and candidate digests,
gzip/decompressor EOF and before/after source/code stability matched. No
price was rounded or timestamp changed. Licensed output stays private on MSI.

The complete first-day option-definition classification probe inspected
50,202 records. Every CFI and settlement-currency field was empty; security
type, unit of measure, currency and underlying fields were populated. This
presence audit supplies no field semantics, quantity, standard-deliverable,
tradability or all-session proof. The empty CFI cannot close the term gap.

The next adapter still needs dated per-series terms and complete causal
rosters, option normalization and Greeks, rates/dividends, full pending-order
and GFD execution coverage, lifecycle assumptions, and 125 remaining sessions.
Both full 15-symbol selection-mode reports remain the final deliverable.
