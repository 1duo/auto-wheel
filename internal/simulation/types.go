// Package simulation replays immutable market observations without a broker or
// a production state directory. Selection, allocation and ownership inference
// use the production wheel package; fills and lifecycle outcomes are models.
package simulation

import (
	"time"

	"autowheel/internal/wheel"
)

type Spec struct {
	Schema                  string       `json:"schema_version"`
	Mode                    string       `json:"mode"`                       // fixture or historical_quote_replay
	ResearchVariant         string       `json:"research_variant,omitempty"` // Empty preserves the frozen baseline.
	Start                   string       `json:"start_date"`
	End                     string       `json:"end_date"`
	InitialCash             wheel.Cents  `json:"initial_cash_cents"`
	PeerReserve             wheel.Cents  `json:"peer_reserved_cash_cents"`
	PeerSymbols             []string     `json:"peer_reserved_symbols,omitempty"`
	Source                  string       `json:"source"`
	CorporateActionsAudited bool         `json:"corporate_actions_audited"`
	Config                  wheel.Config `json:"strategy_config"`
}

// ResearchPutRiskCap is an offline study, never a production configuration.
const ResearchPutRiskCap = "weekly_put_risk_cap_v1"

type Quote struct {
	wheel.Quote
	ListedAt   time.Time `json:"listed_at"`
	OIAt       time.Time `json:"open_interest_published_at"`
	GreeksAt   time.Time `json:"greeks_at"`
	ObservedAt time.Time `json:"observed_at,omitzero"` // Current book snapshot time; never replaces Quote.At.
}

// ExecutionCoverage describes every book state in (From, Through], including
// the state carried into the interval. It is a normalizer declaration, not proof:
// historical performance additionally requires independent source review bound
// to SourceSHA256 and the exact replay input. Minima must include transient dips.
// ValidBooks includes price/size, standard deliverable and tradability continuity.
// QuotesFresh includes quote age throughout the interval, not just its endpoint.
type ExecutionCoverage struct {
	OptionID     string      `json:"option_id"`
	From         time.Time   `json:"from"`
	Through      time.Time   `json:"through"`
	SourceSHA256 string      `json:"source_sha256"`
	Complete     bool        `json:"complete"`
	ValidBooks   bool        `json:"valid_books"`
	QuotesFresh  bool        `json:"quotes_fresh"`
	MinBid       wheel.Cents `json:"min_bid_cents"`
	MinBidSize   int         `json:"min_bid_size"`
}

// NoRecentBook is a bounded research-only non-price observation proving
// NEW-selection freshness ineligibility under proven COMPLETE CLOSED
// capture-time [decisionT-MaxQuoteAge,T] coverage with zero BOOK updates.
// Trades do not refresh quotes. Future events are excluded. It carries no
// market price, size, delta, open interest or fabricated quote timestamp.
// Only scheduled decision frames may use it. Existing historical source gates
// and independently bound normalized-input review remain required;
// attestation/hash alone is not proof.
type NoRecentBook struct {
	OptionID     string    `json:"option_id"`
	From         time.Time `json:"from"`
	Through      time.Time `json:"through"`
	SourceSHA256 string    `json:"source_sha256"`
	Complete     bool      `json:"complete"`
}

// SelectionOmission is a research-only declaration that a listed contract's
// ranking inputs (price/size/OI/delta/freshness) are causally irrelevant to any
// NEW order at this decision. It carries no market data and never becomes a
// quote, catalog entry, fill, mark, or execution-coverage book. Every listed ID
// must still be exactly one real Quote, NoRecentBook, or validated
// SelectionOmission with full Chains roster identity retained.
type SelectionOmission struct {
	OptionID string `json:"option_id"`
	Reason   string `json:"reason"` // inactive_kind, global_intent, full_slots
}

// SelectionProof binds an omission set to the exact post-fill/lifecycle/Reconcile
// portfolio that proves causal irrelevance. Hashes use sha256 over deterministic
// Go json.Marshal (sorted map keys) with a version prefix; see the exported
// SelectionStateHash/SelectionSnapshotHash/SelectionConfigHash helpers for the
// exact domains so an acquisition adapter can reproduce them. Hash/flag alone
// qualifies nothing; independent input/source audit remains required.
type SelectionProof struct {
	At             time.Time `json:"at"`
	StateSHA256    string    `json:"state_sha256"`
	SnapshotSHA256 string    `json:"snapshot_sha256"`
	ConfigSHA256   string    `json:"config_sha256"`
	SourceSHA256   string    `json:"source_sha256"`
	Complete       bool      `json:"complete"`
}

type Stock struct {
	Symbol string      `json:"symbol"`
	At     time.Time   `json:"at"`
	Bid    wheel.Cents `json:"bid_cents"`
	Ask    wheel.Cents `json:"ask_cents"`
}

// Chain is the independent point-in-time instrument roster, including contracts
// that fail liquidity. A symbol label alone cannot establish nearest expiry.
type Chain struct {
	Symbol    string    `json:"symbol"`
	Kind      string    `json:"kind"`
	Expiry    string    `json:"expiry"`
	At        time.Time `json:"as_of"`
	Weekly    bool      `json:"weekly"`
	Contracts []string  `json:"contract_ids"`
}

// Lifecycle is an explicit counterfactual assumption, never a fabricated
// historical broker assignment receipt. SettledAt controls share/cash reuse.
type Lifecycle struct {
	OptionID   string      `json:"option_id"`
	Outcome    string      `json:"outcome"` // expired, assigned, called_away, forced_closed
	SettledAt  time.Time   `json:"settled_at"`
	ClosePrice wheel.Cents `json:"close_price_cents,omitzero"`
	Assumption string      `json:"assumption"`
}

type Frame struct {
	At                  time.Time           `json:"at"`
	Kind                string              `json:"kind"` // market, decision, valuation, lifecycle, session_end
	Quotes              []Quote             `json:"quotes,omitempty"`
	Stocks              []Stock             `json:"stocks,omitempty"`
	CompleteSymbols     []string            `json:"complete_chain_symbols,omitempty"`
	Chains              []Chain             `json:"listed_chains,omitempty"`
	NoRecentBook        []NoRecentBook      `json:"no_recent_book,omitempty"`
	SelectionOmissions  []SelectionOmission `json:"selection_omissions,omitempty"`
	SelectionProof      *SelectionProof     `json:"selection_proof,omitzero"`
	Lifecycles          []Lifecycle         `json:"lifecycles,omitempty"`
	Dividend            wheel.Cents         `json:"dividend_cents,omitzero"`
	DividendSymbol      string              `json:"dividend_symbol,omitempty"`
	DividendEvidence    string              `json:"dividend_evidence,omitempty"`
	DividendAvailableAt time.Time           `json:"dividend_available_at,omitzero"`
	UnsupportedAction   string              `json:"unsupported_corporate_action,omitempty"`
	ExecutionCoverage   *ExecutionCoverage  `json:"execution_coverage,omitzero"`
}

type Trade struct {
	At         time.Time   `json:"at"`
	Symbol     string      `json:"symbol"`
	OptionID   string      `json:"option_id"`
	Action     string      `json:"action"`
	Price      wheel.Cents `json:"price_cents"`
	CashFlow   wheel.Cents `json:"cash_flow_cents"`
	Fee        wheel.Cents `json:"fee_cents"`
	Assumption string      `json:"assumption,omitempty"`
}

type Point struct {
	At              time.Time   `json:"at"`
	Equity          wheel.Cents `json:"equity_cents"`
	Cash            wheel.Cents `json:"economic_cash_cents"`
	Available       wheel.Cents `json:"available_cash_cents"`
	StockValue      wheel.Cents `json:"stock_value_cents"`
	OptionLiability wheel.Cents `json:"option_liability_cents"`
	Committed       wheel.Cents `json:"committed_capital_cents"`
	Active          int         `json:"active_symbols"`
}

type Metrics struct {
	EndingEquity   wheel.Cents `json:"ending_equity_cents"`
	NetPnL         wheel.Cents `json:"net_pnl_cents"`
	ReturnPct      float64     `json:"return_pct"`
	MaxDrawdownPct float64     `json:"max_drawdown_pct"`
	Premiums       wheel.Cents `json:"premiums_cents"`
	Fees           wheel.Cents `json:"fees_cents"`
	StockRealized  wheel.Cents `json:"stock_realized_cents"`
	Dividends      wheel.Cents `json:"dividends_cents"`
	Submissions    int         `json:"submissions"`
	Fills          int         `json:"fills"`
	ExpiredOrders  int         `json:"expired_orders"`
	Assignments    int         `json:"assignments"`
	CallAways      int         `json:"call_aways"`
	BlockedCycles  int         `json:"blocked_cycles"`
}

type Report struct {
	Schema             string       `json:"schema_version"`
	Status             string       `json:"status"` // blocked, fixture, counterfactual
	Spec               Spec         `json:"spec"`
	InputSHA256        string       `json:"input_sha256,omitempty"`
	StrategySHA256     string       `json:"strategy_sha256,omitempty"`
	EngineSHA256       string       `json:"simulation_engine_sha256,omitempty"`
	Blockers           []string     `json:"blockers,omitempty"`
	Assumptions        []string     `json:"assumptions"`
	ExpectedDecisions  int          `json:"expected_decisions"`
	ObservedDecisions  int          `json:"observed_decisions"`
	ExpectedValuations int          `json:"expected_valuations"`
	ObservedValuations int          `json:"observed_valuations"`
	Performance        *Metrics     `json:"performance,omitempty"`
	Equity             []Point      `json:"equity_curve,omitempty"`
	Trades             []Trade      `json:"trades,omitempty"`
	EndState           *wheel.State `json:"end_state,omitempty"`
}
