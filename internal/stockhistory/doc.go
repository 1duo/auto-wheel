// Package stockhistory is a bounded pure-Go MINI MBP-1 causal stock
// reader/reducer for source-normalization groundwork.
//
// It is NOT a qualified full normalizer, NOT a replay input, NOT a coverage
// declaration, and makes no P&L, fill, freshness, performance, or money-flow
// claim. It never trades, never touches financial controls, brokerage,
// credentials, network, builds, or services, and performs no data-purchase
// work. Scope is source normalization groundwork for the full Apr6-Oct2
// fifteen-symbol frozen study, without full source qualification or money
// flows.
//
// Minimal API:
//
//	series, err := stockhistory.ReadMiniMBP(r)
//	obs, err := series.Snapshot(asOf, maxAge)
//	events := series.Events() // cloned audit copy, never an alias
//
// ReadMiniMBP parses one JSON object per line with local resource guards
// matching the existing 10k/16MiB pilot (MaxRecords, MaxFileBytes,
// MaxLineBytes). Snapshot folds validated events into per-symbol
// observations keyed by exact frozen raw_symbol. The returned map holds
// observations only and never a coverage declaration or replay input.
// Series is immutable: its events are private, Events returns a clone for
// audit, and Snapshot never mutates Series or caller-visible data and is
// causal and repeatable. Original file order is preserved; events are never
// sorted or rewritten.
//
// Wire scope mirrors the actual NFLX pilot JSONL shape (pretty_px=false,
// pretty_ts=false): root{ts_recv, hd{ts_event, rtype, publisher_id,
// instrument_id}, action, side, depth, price, size, flags, ts_in_delta,
// sequence, levels[{bid_px, ask_px, bid_sz, ask_sz, bid_ct, ask_ct}],
// symbol}. Every required outer/header field is validated and omitted/null
// never silently becomes zero, except book price/size cells where
// missing/null explicitly invalidates the newest quote (see below).
// Duplicate, non-ASCII, noncanonical and unknown keys at any level fail
// closed; wrong JSON types and malformed integer strings fail.
//
// Namespace is exact and frozen: symbol must be one of NFLX TSLA NVDA NBIS
// IREN NOW AMD AMZN GOOGL META MSFT PLTR UBER AVGO ORCL, publisher_id 95,
// rtype 1, instrument_id 1..4294967295 with a stable one-to-one
// symbol/ID mapping within the file. Capture (ts_recv) is nondecreasing in
// file order (equal okay, including across symbols); ts_event must not
// exceed ts_recv. Depth, sequence, ts_in_delta, bid_ct, and ask_ct must be
// exactly zero per MINI docs (sequence/delta/counts are zero; quotes are
// aggregated proprietary BBO, not SIP). Side must be A/B/N; action must be
// A/C/M/R/T.
//
// Flags follow the primary source checked today
// (https://raw.githubusercontent.com/databento/dbn/main/rust/dbn/src/flags.rs):
// LAST=128, TOB=64, SNAPSHOT=32, MBP=16, BAD_TS_RECV=8, MAYBE_BAD_BOOK=4,
// PUBLISHER_SPECIFIC=2. IMPORTANT: 192 is LAST+TOB, not SNAPSHOT. Only
// structural TOB/MBP bits and LAST are accepted: intermediate records may
// have 0/16/64/80, and completion records 128/144/192/208. Snapshot, bad
// time/gap, publisher-specific and unknown flags fail with
// ErrUnqualifiedFlags. Per-symbol records are folded atomically through
// LAST. EOF with an unfinished event fails with ErrIncompleteEvent; an
// asOf cutoff inside an event emits invalid "incomplete_event" without
// consulting its future completion record.
//
// Money and clocks: timestamps and nanoprices are exact integer strings
// parsed with strconv, never float; no midpoints, cents conversion, or
// fabricated defaults. Raw source clocks and exact integer nanoprices are
// preserved on Event (TsRecvRaw, TsEventRaw, PriceRaw, BidRaw, AskRaw).
// Raw "9223372036854775807" (INT64_MAX) is the undefined-price sentinel:
// preserved as MaxInt64 with an undefined flag and always classified
// invalid, never valid. Timestamps must be 1..MaxInt64-1; zero, sentinel,
// negatives, and formatted times fail.
//
// Book rules: A/C/M use complete levels[0]; exactly one level is required.
// R always invalidates with reason "reset" even when levels still print a
// positive payload. Missing/null bid_px, ask_px, bid_sz, or ask_sz
// invalidates the newest quote (reason "missing_price" or "zero_size"
// handling) rather than carrying an older valid book; malformed px/sz
// representations fail the file. Invalid, undefined, missing, nonpositive,
// crossed (bid>ask), or zero-size books invalidate and zero eligible output
// prices, retaining source identity and reason. Locked (bid==ask) books
// stay valid. Top-level price/size/side are validated and retained for
// audit but do not define the book.
//
// Freshness: Snapshot includes only capture<=asOf with sourceEvent<=capture
// (the latter is already enforced fail-closed at read). maxAge bounds fresh
// vs stale with exact boundary age==maxAge fresh; later stale. Events with
// capture after asOf are excluded from state but were already validated
// fail-closed, so a future malformed row never silently passes. No future
// row selection. At is the original quote capture; AvailableAt is the
// event completion capture. They remain distinct: quote age uses At,
// eligibility waits for AvailableAt. EventAt is the quote engine timestamp,
// not a Robinhood arrival clock. Trade capture/source times are tracked
// separately as TradeAt/TradeEventAt and never refresh At/EventAt.
//
// Conservative trade-ambiguity rule (intentionally explicit pending
// supplier-wide qualification): T never refreshes quote At/EventAt and
// never creates an observation on its own (no prior quote means no eligible
// book). A same-book T (valid BBO exactly equal in bid/ask px/sz to the
// retained valid quote book) preserves prior quote clock/status and only
// advances TradeAt/TradeEventAt; it never restores an invalid book. A T
// that carries a different BBO (including a missing/invalid BBO) from the
// retained valid book invalidates with explicit reason
// "trade_book_ambiguity", zeroing prices while preserving prior At/EventAt
// (the sale is never used as a quote and no quote-update time is invented)
// and advancing TradeAt/TradeEventAt. A later complete A/C/M can restore
// state. This does not claim universal historical equivalence between trade
// BBO and quote books; supplier-wide qualification of trade BBO semantics
// is still pending, so the reducer stays fail-closed on any difference
// rather than guessing that trades echo quotes.
//
// Same-capture ties: capture ties across symbols are independent and okay.
// Intermediate records within one completed source event retain source
// order. Separate completed events at the same completion capture have
// no qualified sequence. Conflicting or invalid book outcomes remain
// "tie_ambiguity" until a later quote resolves them. Equivalent valid
// outcomes retain an original witness with the latest actual quote capture
// (first witness when those captures also tie), with TieCount counting
// completed events and TieEquivalent=true. R/quote ties and trade/quote
// ties with differing BBO never create eligible state. File order is
// preserved; no clocks are rewritten.
//
// Absent symbols are simply absent: never synthesized, never labeled
// delisted/fresh, never given guessed prices (invalid emits zero
// prices/sizes). Nil readers and zero asOf fail cleanly; nonpositive maxAge
// fails.
//
// Limitations: single-file bounded groundwork only; no full Apr6-Oct2
// fifteen-symbol qualification, no full source qualification, no money
// flows, no SIP/Robinhood equivalence, no replay/P&L/coverage claim, no
// corporate-action/adjustment/venue-loss proof. Trade BBO equivalence and
// supplier-wide T semantics remain unqualified by design.
package stockhistory
