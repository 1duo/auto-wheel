// Package quotehistory is a bounded offline OPRA CMBP-1 event-state reducer
// for actual-source window-vs-continuous audit.
//
// It is NOT a qualified full normalizer, NOT a replay input, and makes no
// P&L, fill, or coverage claim. It never trades, never touches financial
// controls, and performs no network, credential, or data-purchase work.
//
// Minimal API:
//
//	events, err := quotehistory.ReadCMBP(r)
//	obs, err := quotehistory.Snapshot(events, asOf, maxAge)
//
// ReadCMBP parses one JSON object per line with local resource bounds
// matching the existing 10k/16MiB pilot (MaxEvents, MaxFileBytes,
// MaxLineBytes). Snapshot folds validated events into per-raw-symbol
// observations keyed by raw OSI symbol. The returned map is deterministic:
// callers sort keys; no logic depends on map iteration order.
//
// Assumptions (bounded, documented, pending broader audit):
//   - Record shape is root{ts_recv, hd{ts_event, rtype, publisher_id,
//     instrument_id}, action, side, price, size, flags, ts_in_delta,
//     levels[{bid_px, ask_px, bid_sz, ask_sz, bid_pb, ask_pb}], symbol}.
//     Extra vendor fields are deliberately ignored; every required field
//     above is validated and omitted/null never silently becomes zero.
//   - Book state comes solely from levels[0]; exactly one level is required.
//     Top-level price/size/side are validated and retained on Event for audit
//     but do not define the book.
//   - Only flags 192 is supported; any other value fails with
//     ErrUnqualifiedFlags. Bit meanings are not guessed.
//   - Actions A/C/M replace the two-sided book (including invalidation);
//     R always invalidates with reason "reset" even when levels still print
//     an old book; T never modifies/refreshes prior quotes and never creates
//     an observation on its own.
//   - Money is exact int64 nanodollars parsed from JSON string fields with
//     strconv, never float; pretty_px=false means integer nanodollar strings,
//     without decimal rescaling. pretty_ts=false means integer nanosecond
//     strings; formatted RFC3339 timestamps are not this wire format. Raw "9223372036854775807" (INT64_MAX) is the
//     undefined-price sentinel: preserved as MaxInt64 with an undefined flag
//     and always classified invalid, never valid.
//   - Symbol must be a 21-byte ASCII OSI with a nonempty uppercase/alphanumeric
//     padded root, valid 2000+YY expiry, C/P right and eight-digit strike.
//   - Capture timestamps must be nondecreasing (equal okay); ts_event must
//     not exceed ts_recv. Snapshot excludes events with ts_recv after asOf
//     from state but still validates their schema/ordering fail-closed, so a
//     future malformed event never silently passes.
//   - At is quote capture-availability (last included quote ts_recv);
//     EventAt is OPRA's consolidator-processed timestamp (last quote ts_event),
//     not an exchange matching-engine timestamp. Trade capture/source event
//     times are tracked separately as TradeAt/TradeEventAt and
//     never refresh At.
//   - Status is "fresh" (valid and asOf-At <= maxAge), "stale" (valid but
//     older), or "invalid" (missing/undefined/nonpositive/zero-size/crossed/
//     reset). Absent symbols are simply absent: never synthesized, never
//     labeled delisted/fresh, never given guessed prices (invalid emits zero
//     prices/sizes). Locked (bid==ask) valid quotes are accepted.
package quotehistory
