# Native open-interest observations

`internal/causaloi` is an offline reducer for the independently audited native
`StatMsgV1` vectors. It retains all seventeen original fields, including the
source index and nanosecond clocks. Quantity is `int32`: `INT32_MAX` is undefined,
while an observed zero stays zero. It is separate from the older JSON pilot.

Each input covers one UTC day and at most 2,000 requested raw option identities.
The reducer retains up to 64 latest publisher copies per identity. It consumes
every row in source order, including non-OI records and rows after the last cut.
Receive-time cuts are closed: all rows at the cut are included before emission.
Publisher copies are never summed. Unsupported updates, undefined quantities,
invalid event/reference clocks, disagreements, or unavailable captured
definitions withhold quantity. Missing identities stay in each snapshot;
missing reference timestamps stay null. No reporting date is inferred.

`cmd/autowheel-native-oi` reads a private scope JSON and private gzip vector. The
scope names the dataset, UTC day, native version, exact record count, compressed
and decoded vector hashes, requested bindings and cuts. Zero `InstrumentID` in
a binding means no mapping assertion; the independently audited native source
must still prove its original mappings. A definition does not establish terms
or tradability. Bindings use the Go field names `RawSymbol`, `InstrumentID` and
`Definition`; definitions use `InstrumentID`, `ReceiveAt` and `EventAt`.

The exporter rejects null integer fields, invalid widths, oversized lines,
incomplete/trailing gzip data, identity changes and capture-order regressions.
It stages output in a fresh private directory and publishes a report only after
complete EOF, count/hash checks and before/after source stability. Failed runs
discard their staged output. An existing output directory is preserved.
Successful output remains explicitly provisional, with the source gate and
historical-performance flags false.

`scripts/compare-native-oi-scope.py` binds the immutable 44-partition Python
audit, original definitions, queries, code and executable. It compares every
contract observation at every cut with the independently generated reference
and checks both gzip EOFs and output hashes. All licensed rows and quantities
stay on MSI; only aggregate results, paths and hashes are retained locally.

This component does not produce replay frames, qualify all 126 sessions,
establish causal reporting dates, or authorize live trading. The complete
source, contract-term, Greek, execution and lifecycle gates still apply.
