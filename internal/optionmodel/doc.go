// Package optionmodel is a deterministic EUROPEAN Black-Scholes-Merton
// pricer for historical replay where Databento bars lack Greeks.
//
// Scope: European options with continuous dividend yield only. It does NOT
// handle American exercise or discrete dividends; that mismatch disqualifies
// naive reuse and Main must qualify American applicability separately.
// This package is standalone and is NOT wired into historical replay.
//
// Money enters and leaves as integer cents; float64 is used only for the
// mathematical Greek computation, never for a cash ledger. Every market
// input carries a causal available-at timestamp; future or stale (>30s for
// spot/option) inputs, missing provenance (including assumed zero r/q),
// and arbitrage/bracket violations are rejected. It performs no historical
// performance measurement and no price imputation.
package optionmodel
