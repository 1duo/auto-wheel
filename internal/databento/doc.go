// Package databento provides a minimal historical-data adapter for the
// Databento historical API with a durable lifetime spending ledger.
//
// Scope: research CLI use only. This package never trades, never holds
// brokerage state, and never imports production wheel packages. It uses only
// the standard library plus golang.org/x/sys/unix.
//
// Fixed production endpoints (no overrides):
//
//	BaseURL = https://hist.databento.com/v0
//	POST /metadata.get_cost      (free cost quote for one exact query)
//	POST /timeseries.get_range   (potentially billable download)
//	GET  /metadata.list_datasets (free listing)
//
// Offline tests inject a fake http.RoundTripper; the URL host is always
// hist.databento.com so tests never touch production.
//
// Money: costs are parsed from exact decimal strings into integer
// nanodollars (1 USD = 1_000_000_000) rounded UP, without binary floating
// point. Default lifetime authorization is $10 (10_000_000_000 nanodollars),
// cumulative across a fixed ledger outside checkouts. The legacy file remains
// version 1 with lifetime_nano 10_000_000_000 and no authorization_mode; new
// files are created capped. An explicit user-authorized migration via
// Ledger.EnableUncapped rewrites the same single file to version 2 with
// lifetime_nano 0 and authorization_mode "uncapped"; that triple is the only
// durable no-spending-limit policy. Every other version/lifetime/mode
// combination is rejected, and migrated files persist uncapped across reopen.
// Each download reserves
// max(quoted cost, ceil(limit * 1020 * fixed dataset/schema ceiling / decimal GB))
// + 10% rounded up. Fixed ceilings are $6 for OPRA definition, $12 for OPRA
// statistics, $0.25 for OPRA cmbp-1, and $2 for EQUS.MINI mbp-1. The inline
// fresh unit-price gate and the checked basis share the same dataset/schema
// qualified ceiling. Observed rates never reduce this fixed basis.
// A zero-cost estimate still reserves the positive record-cap bound. Old holds
// retain their original amount; there is no repricing of existing entries.
// Reservations are conservative budget holds, not measured invoice charges.
// Measured/estimated provider costs remain separate from the authorization
// decision to remove the cap.
//
// Ledger: a single JSON file guarded by an exclusive non-blocking flock held
// for the handle lifetime, written atomically (temp file + fsync + rename +
// directory fsync). Every potentially billable request reserves BEFORE any
// HTTP is sent. Reservation identity is SHA256(canonical query || 0x00 ||
// billable endpoint); a repeated identity is rejected before sending. There
// is no release or refund API: an interrupted, timed-out, or failed billable
// operation stays charged/unknown with its full hold retained forever (status
// stays "reserved"; never faked complete) and the same canonical operation is
// never sent twice. A different cost-qualified operation may still proceed
// within the remaining cumulative holds when capped, or without a cumulative
// check when uncapped. The per-request record-cap bound at
// the schema's fixed ceiling and retained margin cover cumulative actuals within $10 under the
// documented provider contract when capped. The record ceiling is unchanged at 10,000.
// Successful bodies receive a durable complete/hash/byte-count record without
// releasing the reservation. Concurrent processes are
// rejected via flock; in-process goroutines are serialized with a mutex.
// When uncapped, the cumulative $10 check is removed while exact integers,
// margin, MaxInt64 sum/individual overflow checks, no retries, durable
// journal, ownership, and 1MiB evidence capacity remain. ValidateReservation
// is authorization-independent; the legacy amount cap is enforced in
// validateLedgerState and Reserve for capped ledgers only. Reserve preserves
// the stored authorization header instead of recreating version 1.
// Ledger.Remaining on an uncapped handle returns representable int64 headroom
// (MaxInt64 minus total); use Ledger.IsUncapped to distinguish real budget
// from integer capacity.
//
// Quotes: GetCost returns a Quote bound to one exact canonical query
// (dataset/schema/symbols/stype_in/start/end/limit). Downloads reject an
// absent, stale (older than QuoteTTL), future-dated, or mutated quote
// (any canonical byte difference, including symbol order), an excessive
// cost beyond remaining budget when capped, and any malformed ledger state.
//
// Cost qualification: estimates alone and local read limits are insufficient
// to cap billing. Downloads currently allow only OPRA.PILLAR definition,
// statistics and cmbp-1 plus the alternative EQUS.MINI mbp-1; <=24 hours,
// <=10,000 total records per request, and a freshly verified
// historical-streaming unit price at or below the dataset/schema's fixed
// ceiling queried at the actual qualified dataset. Duplicate JSON fields and
// ambiguous streaming rows reject before reserve/send. OPRA symbol scope is one
// symbol, or a narrow selective batch of 2..128 raw_symbol contracts that are
// distinct strict 21-byte ASCII OSI symbols sharing one padded root and one
// YYMMDD expiry (calls and puts may mix). Broader parents, instrument-ID lists,
// and arbitrary multi-root or multi-expiry batches remain unqualified. The MINI
// alternative instead requires stype_in raw_symbol and 1..15 exact distinct
// uppercase members of the frozen universe NFLX TSLA NVDA NBIS IREN NOW AMD AMZN
// GOOGL META MSFT PLTR UBER AVGO ORCL. Dataset/schema cross products and other
// stocks reject before unit-price/ledger/send. DBN records contain at most 255
// four-byte words. The fixed dataset/schema ceiling, decimal-GB divisor and 10%
// margin bound that request without relying on a transient lower unit rate.
// Adding symbols does not raise the cap. A selective same-expiry sample within
// 10,000 records stays within the same bound. This bound is acquisition
// financial qualification only: it does not qualify trading inputs, corpus
// affordability, source, freshness, performance, or a complete-study claim. MINI
// mbp-1 BBO is proprietary-venue BBO, not SIP/Robinhood. A quote must have been
// issued by this same Client, not reconstructed by a caller. The provider's
// billing semantics and honored record limit remain external assumptions; the
// portal's monthly limit is additional protection.
//
// Secrets: the API key is read once from a regular non-symlink file owned by
// the current UID with mode 0600 inside a private parent directory, kept in
// memory only, sent via Basic auth, its stored byte buffer cleared on Close, and never
// included in errors or logs. Provider response bodies are hashed for
// receipts but never logged or embedded in errors. Proxies, redirects, and
// automatic financial retries are disabled. Free quote and unit-price requests
// precede each one-shot financial request. Bodies are retained only as private
// research artifacts.
package databento
