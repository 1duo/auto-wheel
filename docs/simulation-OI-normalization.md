# OI candidate normalization

`internal/replaynormalize.OpenInterest` converts an independently attested
closed OI observation into a candidate quantity and publication clock. It
does not create a complete market quote, replay frame, fill or performance
report. Books, terms, tradability, Greeks and the other source gates still apply.

The fixed `captured_OI_receive_availability_v1` policy uses the literal native
receive time as a capture-availability bound. A later observation cut cannot
refresh it. The original event clock and all seventeen chosen native fields
remain intact. Missing economic reference time stays null; a present reference
is copied literally and may predate the capture day. No calendar date is inferred.
See [the primary clock review](simulation-open-interest-clocks.md) for limits on
earliest publication, broker availability and historical revision continuity.

The helper requires matching nonzero source hashes, UTC partition, raw identity
and daily instrument ID. It rejects withheld statuses, absent records or
quantities, inconsistent copies, unsupported native/update/flag fields, nonzero
OPRA sending deltas, and unrepresentable or future clocks. Native quantity stays
within its signed `int32` domain; its sentinel remains unknown and an observed
zero stays zero. Publisher copies are never summed. Every error returns a zero
candidate. Caller labels and hashes alone establish no source truth.

The offline CLI under `research/simulation/2026-10-05/cycle46-OI-normalizer/`
consumes the complete audited seventeen-field gzip vector through the existing
`causaloi.Reduce`. Its private candidate packets retain every requested identity,
original row, status and rejection reason at each cut. Output is staged until
count, compressed/plain hashes, complete EOF and before/after stability pass.
A late failure removes only the invocation's fresh output; existing output is
preserved. The cached native exporter and frozen replay engine are unchanged.

`scripts/audit-OI-replay-normalizer.py` accepts the sealed all-acquired audit,
binds its original sources and frozen 543-query/126-session plan, and reconstructs
the latest publisher copies directly from the original vectors. It compares
every candidate field at all 75 frozen cuts for every attested snapshot member,
including null rejections, original record fields and exact nanosecond clocks.
It consumes and hashes all rows after the last cut too. Licensed rows and
quantities stay on MSI; local artifacts contain aggregate evidence only.

The comparison independently checks the candidate projection. Original native
admission, mappings and acquisition proofs remain bound to the accepted upstream
audit. Invalid event/reference clocks produce a withheld reducer status before
conversion; a clock discrepancy on an admitted 2026 observation fails comparison.
The generic helper's direct-caller clock rejections are covered separately.

Only `^TestOpenInterest` was run on Linux, and only the offline CLI was built.
Five runtime controls verified late count/hash failure, truncated gzip, explicit
null quantity rejection and preservation of an existing output directory. The
full snapshot conversion has a separate evidence artifact; this document alone
does not establish its completion. All qualification, frame and performance
flags remain false until the complete historical study is qualified.
