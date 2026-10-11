# Native streaming for the full historical replay

The frozen study remains all 15 symbols, 126 sessions, 9,324 scheduled
decisions, and 126 closing valuations. Native acquisition, lossless decoding,
and causal replay qualification are separate requirements. This component
implements decoding only; all source gates remain pending.

`internal/nativebook.Stream` processes uncompressed native book records with a
64 KiB input buffer and one reusable record. It preserves unsigned 64-bit
timestamps, undefined sentinels, signed prices, flags, venue fields, reserved
bytes, and original file order. It accepts AS_IS versions 1–3 and exactly
CMBP-1 or MBP-1. There is no record slice, timestamp correction, symbol
inference, Greek fallback, or financial write capability.

The expected metadata prefix must be attested by the pinned official decoder.
The reader verifies that prefix's length and SHA256, each record's type and
width, the exact complete row count, the complete native SHA256, and EOF.
Callbacks are provisional until successful completion. An output writer must
stage its work and discard it on failure. The outer acquisition boundary must
still check private descriptors, compressed source identity, decompressor exit
status, acquisition proof, and source stability.

The research integration harness independently parses the original metadata
with `databento-dbn==0.71.0`, binds its dated instrument mapping, and streams
native bytes to Go. Go hashes the original 21/23 canonical vector fields
without storing another vector file. Its digest must equal the complete
independent Python reference. The harness checks source and code hashes before
and after and rejects partial results. This is an integrity comparison, not
proof that any quote was usable at a strategy decision time.

The existing pilot readers retain their 10,000-record and 16 MiB limits.
Further full normalization must explicitly handle capture regressions, clock
leads, equal-time ambiguity, update/reset/trade semantics, source availability,
quiet intervals, causal contract rosters, and the complete decision coverage.
Missing ranking-relevant Greeks must continue to block qualification.

## Storage remains a separate acceptance requirement

MSI's internal ext4 storage had 29,342,900,224 free bytes at the cycle-29
inspection. The external exFAT volume had 797,491,331,072 free bytes but presents
regular files with mode 0700 under its current mount options. Existing private
source guards require exact mode 0600. No permission guard was relaxed, user
data removed, volume reformatted, or mount configuration changed.

Native streaming avoids mandatory retention of a second JSON corpus. It does
not yet make it safe to evict existing source-bound files: current proofs and
completion checks require their original paths. Archival would require a
validated, fail-closed restoration layer or a private POSIX storage volume.
The complete 126-session compressed footprint is still unverified; native
billable size is not compressed storage size. A first-day extrapolation is not
a capacity proof. A file-backed POSIX volume is a possible design, subject to
mount privileges, underlying free space, disconnection recovery, and actual
permission/durability checks. It has not been created or qualified.

Layout references: [official DBN specification](https://databento.com/docs/knowledge-base/new-users/dbn-encoding),
[pinned v0.71.0 record definitions](https://github.com/databento/dbn/blob/v0.71.0/rust/dbn/src/record.rs),
[official native layout tests](https://github.com/databento/dbn/blob/v0.71.0/rust/dbn/src/record/layout_tests.rs).
