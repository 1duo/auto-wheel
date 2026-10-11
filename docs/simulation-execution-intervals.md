# Native execution intervals

Status: implemented outside the frozen replay engine. The targeted Linux
interval tests and nine independent synthetic controls pass. Complete Go passes
consume all 370,695,919 original option records and 9,786,851 stock records.
The three independent option comparisons match every one of the 2,194,248
interval observations; they consume and hash all original native bytes and
independently reduce 8,821,132 records within the selected windows. The stock
pass produces 6,660 observations. Its independent full-native MBP reference
reconstructs all 9,786,851 records, re-verifies complete receive ordering, and
matches all 6,660 observations and aggregate counters. Sixteen stock controls
cover quiet aging, separate clocks, partial events across boundaries, working
books completed by trades, unknown depth, numeric type drift, and gap receive
regressions. No replay frames, fills, source qualification or performance were
produced.

The frozen replay requires a source-bound interval for the pending contract at
each observation, with duration at most one second. Its first eligible interval
starts the dwell clock; a fill requires five subsequent uninterrupted seconds.
A temporary bid or size loss, an invalid book, or stale quote resets that clock.
The order stays pending until a modeled fill or verified GFD expiry. Six seconds
after a decision are sufficient to exercise the earliest fill path, but do not
establish coverage through the end of the session.

The new component derives raw interval facts outside the frozen replay engine.
It accepts an attested native instrument universe and a bounded, sorted plan of
nonoverlapping intervals in one UTC partition. Adjacent intervals are allowed.
It consumes the entire original native source in receive order and requires
complete native byte count, digest, EOF and cancellation checks. Source IDs and
record clocks remain unchanged.

Each interval carries the closed book state at its start, every completed state
change after that start, and the closed state at its end. Separate completed
updates sharing a receive timestamp still contribute their minima. A partial
event spanning positive receive time invalidates continuity; a partial event
completed at that same receive time publishes only its completed state. A
pending endpoint stays invalid. Trades retain their original fields and never
refresh the book timestamp. A changed BBO carried by a trade introduces
ambiguity until a real book update. Resets invalidate the resting book.

Bid and size minima retain native values with an explicit presence flag; missing
or reset state cannot create a zero-priced quote. Invalid nonreset books retain
their fields and prevent a valid-book declaration. Receive freshness and event
freshness are distinct. Quiet periods age the original quote, including the
period immediately before an update restores endpoint freshness. Undefined,
zero or leading event clocks cannot become fresh execution quotes.

Raw book validity supplies only price, size and native observation semantics.
The replay's `ValidBooks` additionally requires standard deliverable and
tradability continuity from independently dated contract evidence. Mapping
identity alone does not prove listing or tradability. Decimal conversion,
quote-time compatibility, Greeks, dividend/rate inputs and lifecycle assumptions
remain separate normalizer requirements. Raw intervals cannot qualify a replay
or stand in for a broker execution receipt.

The first historical validation plan covers six consecutive one-second
intervals after each of the 74 frozen April 6 decisions: 444 intervals per
source. Its acceptance requires focused synthetic counterexamples, a complete
Go pass over the original acquired sources, and an independent reduction of
each emitted option interval. The independent reference may initialize from
the already verified closed decision captures, then reduce original native
records within those windows. That optimization relies on the exact source,
mapping, seed-capture and complete-source digest bindings; it cannot skip a
window's intermediate states or derive identities from produced intervals.

The stock reference consumes every native MBP record with its original
23-field vector. It retains working and published book states separately and
needs no closed seeds or receive-order assumptions from another reducer.
Equal-time event parts publish only at LAST; pending state at the start, during
positive time, or at the endpoint prevents valid continuous-book coverage.

All output is provisional until outer acquisition, metadata, decompressor,
descriptor, compressed-source hash and before/after stability checks pass.
Original licensed records and derived intervals stay private on MSI. Research
reports expose only aggregate counts and integrity evidence.
