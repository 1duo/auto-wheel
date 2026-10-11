# Next causal observation component

Status: raw reducer implemented; targeted controls and complete first-day Go
passes succeed. Complete stock and all three option as-of comparisons succeed;
whole-source qualification remains pending. This component is
separate from the frozen replay engine.
The native decoder and complete reference comparisons establish integrity.
They do not establish decision-time market availability or performance.

Build a small component outside the frozen replay engine. It should consume
verified `nativebook.Record` values, retain raw prices and unsigned clocks,
and reduce them to per-instrument observation state. Bound its instrument
universe explicitly from the attested query/mapping; reject unknown identities
and capacity overflow. Key identities by dataset, UTC partition, and native
instrument ID. Daily mapping identifies records but supplies no causal listing,
weekly status, deliverable, multiplier, tick, open interest, or Greeks.

Keep `TsRecv`, `TsEvent`, and optional `TsOut` separate, including undefined
sentinels. Never cast undefined or out-of-range nanoseconds to signed time,
subtract a clamped sending delta to invent availability, or retime an event.
Capture order governs availability. Databento documents per-symbol receive
monotonicity; that is not a proof of global file order. Verify global receive
order before accepting any bounded k-way merge. An observed regression must
invalidate that provisional merge rather than trigger a hidden sort. Preserve
event-clock regressions as source facts; the later frame producer must resolve
them against the replay's quote-time constraints or fail qualification.

Apply the documented action and flag semantics through a fixed reviewed
dataset policy, not an injected caller table. Trades do not refresh quote
timestamps or replace the resting book. Clear/reset invalidates carried state.
Book updates use the supplied aggregate levels, not order-level arithmetic.
Track incomplete events and publish only at a verified event boundary; check
`F_LAST` as a bit. Bad-clock, gap, snapshot, or publisher-specific flags require
explicit provenance rules and cannot silently become a fresh usable book.
Undefined, empty, locked, or crossed levels remain explicit states.
Depth identifies the native update's originating level; it is not necessarily
zero just because the record supplies one BBO level. Preserve nonzero values.
Resets still invalidate the book, trades still cannot refresh it, and nonzero
BOOK depth stays `book_depth_unqualified` until its semantics are qualified.

Retain original file indexes for equal capture times. A decision may use its
closed capture boundary only after every relevant input is complete through
that boundary. Disjoint contract query chunks may be merged after order and
identity checks; overlapping subjects require a separate reviewed policy.
OPRA and EQUS.MINI retain distinct provenance and no implicit clock correction.

Callbacks and staged outputs remain provisional until every input passes
native count/hash/EOF, acquisition and compressed-source checks, decompressor
exit status, and before/after stability. Use bounded reduction and staged files,
never a buffer containing all records. No observation can become a replay
`Quote`, `ExecutionCoverage`, or `NoRecentBook` without its additional causal
evidence. In particular, missing Greeks cannot prove absence of book updates.
`Quote.At` and `ObservedAt` remain distinct; the execution interval producer
must preserve every intermediate minimum and quiet-period age.

The smallest meaningful acceptance matrix covers: undefined/overflow clocks;
trade-only periods; reset and incomplete events; quality flags; receive-order
regressions; equal-time closed boundaries; cross-dataset identity collisions
and overlapping chunks; universe bounds; and final EOF/hash/cancellation
failure discarding all provisional output. Validate these against synthetic
controls and then the acquired original streams before integration. Terms,
rates/dividends, IV identification, Greeks, and lifecycle qualification remain
separate unresolved inputs.

The cycle-30 worker supplied interface leads only. Its event-time ordering,
event-monotonicity assumption, unsized map, and possible whole-stream memory
staging were rejected. The main review checked the actual record types,
replay time constraints, and primary provider conventions.

Cycle 31 implements `internal/causalbook` and the offline private producer.
The acquired April 6 sources contain 370,695,919 option records and 9,786,851
stock records. Each complete file produced 75 closed capture snapshots
(74 frozen decision cuts and one closing valuation). These are raw observations,
not replay frames or performance. Stock event-clock regressions are retained;
the exporter must resolve their compatibility with replay quote times.
The first stock run rejected a nonzero native depth; the correction preserves
that field and withholds book usability rather than inventing its meaning.
The independent Python stock reconstruction matches every produced snapshot,
original field and aggregate counter after consuming and hashing all 9,786,851
proven native vectors. Cycle 33 completed the same independent reconstruction
for all 370,695,919 option records and all 370,650 option observations. All
count, hash, EOF, counter and source-stability checks passed.

`internal/capturejoin` now joins bounded, disjoint instrument universes at
identical closed cuts. Dataset IDs remain separate, callbacks cannot mutate
producer state or accounting, and a missing/misaligned capture, extra packet,
wrapped EOF, reader failure or cancellation invalidates provisional output.
Only instrument IDs are sorted for membership; clocks and records are unchanged.
The focused Linux package checks pass. The private file wrapper preserves
every field in the 21/23-column compact projections and verifies their full
compressed/plain hashes, gzip EOF, descriptor identity and before/after input
stability. It does not reconstruct omitted native wire bytes. Their integrity
remains bound to the original source and native audit.

The actual April 6 join contains 75 sets, 300 source snapshots and 371,775
observations. Independent comparison against all four original compact files
matches every row. No replay frame, execution interval or performance was
produced. The next adapter must derive every intermediate bid/size minimum
and quiet-period freshness from the original streams. Endpoint snapshots
cannot prove the replay engine's one-second intervals and five-second dwell.

Cycle 34 adds [native execution intervals](simulation-execution-intervals.md).
The bounded reducer captures transient bid/size minima and separate receive/event
freshness, and its first-day option intervals have independent native comparisons.
The six-second validation windows still leave full pending-order/GFD coverage,
dated tradability and deliverables, normalizer integration and the other source
gates pending.

References: [Databento common fields and conventions](https://databento.com/docs/standards-and-conventions/common-fields-enums-types),
[MBP-1 and CMBP-1 schemas](https://databento.com/docs/schemas-and-data-formats/mbp-1).
