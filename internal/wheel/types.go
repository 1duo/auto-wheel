package wheel

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Cents represents dollars in cents. It never passes through binary floating point.
type Cents int64

const MaxMoney Cents = 1_000_000_000_00

func ParseCents(s string) (Cents, error) {
	whole, fraction, _ := strings.Cut(s, ".")
	if whole == "" || strings.HasPrefix(whole, "-") || len(fraction) > 12 {
		return 0, errors.New("invalid nonnegative money")
	}
	for _, part := range []string{whole, fraction} {
		for _, c := range part {
			if c < '0' || c > '9' {
				return 0, errors.New("invalid decimal money")
			}
		}
	}
	if len(fraction) > 2 && strings.Trim(fraction[2:], "0") != "" {
		return 0, errors.New("money has sub-cent precision")
	}
	fraction = (fraction + "00")[:2]
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || w > int64(MaxMoney)/100 {
		return 0, errors.New("money out of range")
	}
	f, _ := strconv.ParseInt(fraction, 10, 64)
	n := Cents(w*100 + f)
	if n > MaxMoney {
		return 0, errors.New("money out of range")
	}
	return n, nil
}

func (c Cents) Dollars() string { return fmt.Sprintf("%d.%02d", c/100, c%100) }

type SelectionMode string

const (
	SelectionConservative SelectionMode = "conservative"
	SelectionBalanced     SelectionMode = "balanced"
)

type Config struct {
	Account              string             `json:"account_number"`
	OrderCoveragePolicy  string             `json:"order_coverage_policy,omitempty"`
	ExclusiveOrdersSince time.Time          `json:"exclusive_orders_since,omitzero"`
	TradePulseStateDir   string             `json:"tradepulse_state_dir,omitempty"`
	RetiredTradePulse    string             `json:"retired_tradepulse_handoff,omitempty"`
	CoordinatorSocket    string             `json:"coordinator_socket,omitempty"`
	CoordinatorPolicy    string             `json:"coordinator_policy_hash,omitempty"`
	Symbols              []string           `json:"symbols"`
	MaxSymbols           int                `json:"max_symbols"`
	SymbolLimitScope     string             `json:"symbol_limit_scope"`
	MaxCollateral        Cents              `json:"max_collateral_cents"`
	Reserve              Cents              `json:"cash_reserve_cents"`
	Fee                  Cents              `json:"fee_per_contract_cents"`
	MinBid               Cents              `json:"min_bid_cents"`
	MinOI                int                `json:"min_open_interest"`
	MaxSpreadBPS         int                `json:"max_spread_bps"`
	PutMinDelta          float64            `json:"put_min_delta"`
	PutMaxDelta          float64            `json:"put_max_delta"`
	CallMinDelta         float64            `json:"call_min_delta"`
	CallMaxDelta         float64            `json:"call_max_delta"`
	AllowBelowCost       bool               `json:"allow_calls_below_cost"`
	MaxQuoteAgeSeconds   int                `json:"max_quote_age_seconds"`
	MaxDays              int                `json:"max_dte_days"`
	PollSeconds          int                `json:"poll_seconds"`
	SelectionMode        SelectionMode      `json:"selection_mode,omitempty"`
	StrategyPreset       string             `json:"strategy_preset,omitempty"`
	FixedPolicy          *FixedPolicyConfig `json:"fixed_policy,omitzero"`

	PriorityProfitClose *ProfitCloseTarget `json:"priority_profit_close,omitempty"`
}

func DefaultConfig() Config {
	return Config{Symbols: []string{"NVDA", "IREN", "AMD", "AMZN", "GOOGL", "AVGO", "NBIS", "COIN", "MSTR", "HOOD"},
		MaxSymbols: 3, SymbolLimitScope: "managed", MaxCollateral: 8_000_000, Fee: 100, MinBid: 5, MinOI: 100, MaxSpreadBPS: 2000,
		PutMinDelta: .15, PutMaxDelta: .35, CallMinDelta: .50, CallMaxDelta: .85, AllowBelowCost: true,
		MaxQuoteAgeSeconds: 30, MaxDays: 8, PollSeconds: 300}
}

// A zero ceiling uses broker available cash, without a fixed per-slot allocation.
// Positive ceilings retain the existing equal-slot allocation.
func (c Config) SymbolCollateralLimit() Cents {
	if c.FixedPolicy != nil {
		return Cents(c.FixedPolicy.Spec.Limits.Symbol)
	}
	if c.MaxCollateral < 0 || c.MaxCollateral > MaxMoney || c.MaxSymbols < 1 || c.MaxSymbols > 3 {
		return 0
	}
	if c.MaxCollateral == 0 {
		return MaxMoney
	}
	return c.MaxCollateral / Cents(c.MaxSymbols)
}

// EntryCashLimit includes fees and excludes reserves; broker cash already
// excludes collateral holds. Never subtract those holds a second time.
func (c Config) EntryCashLimit(available Cents) Cents {
	if available < 0 || available > MaxMoney || c.Reserve < 0 || c.Reserve > available {
		return 0
	}
	return min(available-c.Reserve, c.SymbolCollateralLimit())
}

// FitsSymbolCollateral includes the entry fee without overflowing money.
func FitsSymbolCollateral(collateral, fee, limit Cents) bool {
	return collateral > 0 && limit > 0 && limit <= MaxMoney && fee >= 0 && fee <= limit && collateral <= limit-fee
}

// EffectiveSelectionMode reports the active put-selection discipline.
// Empty preserves legacy balanced behavior for existing configs.
func (c Config) EffectiveSelectionMode() SelectionMode {
	if c.FixedPolicy != nil {
		return SelectionConservative
	}
	if c.SelectionMode == "" {
		return SelectionBalanced
	}
	return c.SelectionMode
}

// WithSelectionMode returns a copy with the named put-selection discipline
// and its exact put delta bands. All other settings are unchanged.
func (c Config) WithSelectionMode(mode SelectionMode) (Config, error) {
	if c.FixedPolicy != nil {
		return Config{}, errors.New("fixed policy requires an explicit configuration transition")
	}
	c.StrategyPreset = ""
	switch mode {
	case SelectionConservative:
		c.SelectionMode = SelectionConservative
		c.PutMinDelta = .05
		c.PutMaxDelta = .15
		return c, nil
	case SelectionBalanced:
		c.SelectionMode = SelectionBalanced
		c.PutMinDelta = .15
		c.PutMaxDelta = .35
		return c, nil
	default:
		return Config{}, errors.New("unknown selection mode")
	}
}

func (c Config) Validate() error {
	if c.OrderCoveragePolicy != "" && c.OrderCoveragePolicy != "full" && c.OrderCoveragePolicy != "exclusive_standard" {
		return errors.New("unknown order coverage policy")
	}
	if c.OrderCoveragePolicy == "exclusive_standard" {
		if c.Account == "" || c.RetiredTradePulse == "" || c.CoordinatorSocket != "" || c.ExclusiveOrdersSince.IsZero() || c.ExclusiveOrdersSince.After(time.Now()) {
			return errors.New("exclusive standard orders require a bound account, native retirement and its activity cutoff")
		}
	} else if !c.ExclusiveOrdersSince.IsZero() {
		return errors.New("activity cutoff requires exclusive standard order policy")
	}
	if !c.validStrategyPreset() {
		return errors.New("unknown or modified strategy preset")
	}
	switch c.SelectionMode {
	case "", SelectionConservative, SelectionBalanced:
	default:
		return errors.New("unknown selection mode")
	}
	switch c.SelectionMode {
	case SelectionConservative:
		if c.PutMinDelta != .05 || c.PutMaxDelta != .15 {
			return errors.New("conservative selection requires put delta bands 0.05-0.15")
		}
	case SelectionBalanced:
		if c.PutMinDelta != .15 || c.PutMaxDelta != .35 {
			return errors.New("balanced selection requires put delta bands 0.15-0.35")
		}
	}
	if c.CoordinatorSocket != "" || c.CoordinatorPolicy != "" {
		if !filepath.IsAbs(c.CoordinatorSocket) || len(c.CoordinatorPolicy) != 64 {
			return errors.New("shared coordinator requires an absolute socket and SHA256 policy")
		}
		for _, r := range c.CoordinatorPolicy {
			if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
				return errors.New("shared coordinator policy digest invalid")
			}
		}
	}
	if c.TradePulseStateDir != "" && !filepath.IsAbs(c.TradePulseStateDir) {
		return errors.New("TradePulse state directory must be an absolute path")
	}
	if c.RetiredTradePulse != "" && (c.CoordinatorSocket != "" || !filepath.IsAbs(c.TradePulseStateDir) || c.RetiredTradePulse != filepath.Join(c.TradePulseStateDir, "supervisor", "cross-host-handoff-sealed.json")) {
		return errors.New("exclusive handover requires the canonical TradePulse retirement seal and no shared coordinator")
	}
	if c.SymbolLimitScope != "" && c.SymbolLimitScope != "account" && c.SymbolLimitScope != "managed" {
		return errors.New("symbol limit scope must be account or managed")
	}
	if c.MaxSymbols < 1 || c.MaxSymbols > 3 || len(c.Symbols) < 1 || len(c.Symbols) > 15 {
		return errors.New("require 1–15 symbols and 1–3 slots")
	}
	seen := map[string]bool{}
	for _, s := range c.Symbols {
		if s == "" || len(s) > 5 || seen[s] {
			return errors.New("invalid or duplicate symbol")
		}
		for _, r := range s {
			if r < 'A' || r > 'Z' {
				return errors.New("symbols must be uppercase US equity tickers")
			}
		}
		seen[s] = true
	}
	if p := c.PriorityProfitClose; p != nil {
		if c.OrderCoveragePolicy != "exclusive_standard" || c.CoordinatorSocket != "" || !seen[p.Symbol] || p.OptionID == "" || len(p.OptionID) > 128 || strings.TrimSpace(p.OptionID) != p.OptionID || p.OpenedAt.IsZero() || p.OpenedAt.After(time.Now()) || p.MaxLimit <= 0 || p.MaxLimit > MaxMoney/100 {
			return errors.New("invalid exclusive priority profit-close target")
		}
	}
	if c.MaxCollateral < 0 || c.MaxCollateral > MaxMoney || c.Reserve < 0 || c.Reserve > MaxMoney || (c.MaxCollateral > 0 && c.Reserve >= c.MaxCollateral) {
		return errors.New("invalid collateral ceiling or reserve")
	}
	if c.CoordinatorSocket != "" && (c.MaxCollateral == 0 || c.MaxCollateral > 8_000_000) {
		return errors.New("shared coordinator requires its qualified fixed collateral ceiling")
	}
	if c.Fee < 0 || c.Fee > 10000 || c.MinBid < 1 || c.MinOI < 1 || c.MaxSpreadBPS < 1 || c.MaxSpreadBPS > 3000 {
		return errors.New("invalid liquidity or fee limits")
	}
	if !(c.PutMinDelta > 0 && c.PutMinDelta <= c.PutMaxDelta && c.PutMaxDelta <= .5 && c.CallMinDelta >= .5 && c.CallMinDelta <= c.CallMaxDelta && c.CallMaxDelta < 1) {
		return errors.New("invalid delta limits")
	}
	if c.MaxQuoteAgeSeconds < 1 || c.MaxQuoteAgeSeconds > 60 || c.MaxDays < 1 || c.MaxDays > 8 || c.PollSeconds < 60 || c.PollSeconds > 3600 {
		return errors.New("invalid age, DTE or poll interval")
	}
	if c.Account != "" {
		if len(c.Account) < 4 || len(c.Account) > 32 {
			return errors.New("invalid account number")
		}
		for _, r := range c.Account {
			if r < '0' || r > '9' {
				return errors.New("account number must contain digits")
			}
		}
	}
	if c.FixedPolicy != nil {
		return c.FixedPolicy.validate(c)
	}
	return nil
}

type Shares struct {
	Symbol    string `json:"symbol"`
	Quantity  int    `json:"quantity"`
	Available int    `json:"available"`
	Cost      Cents  `json:"cost_cents"`
}

type Option struct {
	ID       string `json:"id"`
	ChainID  string `json:"chain_id,omitempty"`
	Symbol   string `json:"symbol"`
	Kind     string `json:"kind"`
	Expiry   string `json:"expiry"`
	Strike   Cents  `json:"strike_cents"`
	Quantity int    `json:"quantity"`
}

type Order struct {
	ID                string    `json:"id"`
	Ref               string    `json:"ref_id"`
	Symbol            string    `json:"symbol"`
	OptionID          string    `json:"option_id,omitempty"`
	Kind              string    `json:"kind,omitempty"`
	Strike            Cents     `json:"strike_cents,omitzero"`
	Quantity          int       `json:"quantity"`
	Filled            int       `json:"filled"`
	State             string    `json:"state"`
	Side              string    `json:"side"`
	Effect            string    `json:"effect,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	LastTransactionAt time.Time `json:"last_transaction_at,omitzero"`
}

func (o Order) Terminal() bool {
	switch o.State {
	case "filled", "cancelled", "canceled", "rejected", "failed", "voided":
		return true
	default:
		return false
	}
}

type Snapshot struct {
	OrderCoveragePolicy string    `json:"order_coverage_policy,omitempty"`
	At                  time.Time `json:"at"`
	Account             string    `json:"-"`
	AvailableCash       Cents     `json:"available_cash_cents"`
	Shares              []Shares  `json:"shares"`
	Options             []Option  `json:"options"`
	Orders              []Order   `json:"orders"`
	ExternalSymbols     []string  `json:"external_symbols,omitempty"`
	ExternalCapital     Cents     `json:"external_capital_cents"`
}

type Quote struct {
	Option
	At           time.Time `json:"at"`
	UnderlyingAt time.Time `json:"underlying_at"`
	Spot         Cents     `json:"spot_cents"`
	Bid          Cents     `json:"bid_cents"`
	Ask          Cents     `json:"ask_cents"`
	Tick         Cents     `json:"tick_cents"`
	Delta        float64   `json:"delta"`
	OI           int       `json:"open_interest"`
	BidSize      int       `json:"bid_size"`
	Standard     bool      `json:"standard"`
	Tradable     bool      `json:"tradable"`
	Weekly       bool      `json:"weekly"`
}

type Candidate struct {
	Closing     bool  `json:"closing,omitzero"`
	CloseLimit  Cents `json:"close_limit_cents,omitzero"`
	EntryCredit Cents `json:"entry_credit_cents,omitzero"`
	Quote
	NetPremium   Cents   `json:"net_premium_cents"`
	Collateral   Cents   `json:"collateral_cents"`
	Extrinsic    Cents   `json:"extrinsic_cents"`
	ReturnPerDay float64 `json:"return_per_day"`
	CallAwayPnL  Cents   `json:"call_away_pnl_cents,omitzero"`
}

type Position struct {
	PutID        string    `json:"put_id,omitempty"`
	PutExpiry    string    `json:"put_expiry,omitempty"`
	PutStrike    Cents     `json:"put_strike_cents,omitzero"`
	PutOpenedAt  time.Time `json:"put_opened_at,omitzero"`
	OwnedShares  int       `json:"owned_shares"`
	CallID       string    `json:"call_id,omitempty"`
	CallExpiry   string    `json:"call_expiry,omitempty"`
	InventoryAt  time.Time `json:"inventory_at,omitzero"`
	Cost         Cents     `json:"cost_cents,omitzero"`
	MissingSince time.Time `json:"missing_since,omitzero"`
	Provenance   string    `json:"provenance,omitempty"`
}

type Intent struct {
	Ref       string    `json:"ref_id"`
	OrderID   string    `json:"order_id,omitempty"`
	Candidate Candidate `json:"candidate"`
	At        time.Time `json:"at"`
}

type State struct {
	Version         int                    `json:"version"`
	Account         string                 `json:"account_number,omitempty"`
	Positions       map[string]Position    `json:"positions"`
	Intents         map[string]Intent      `json:"intents"`
	CompletedOrders map[string]OrderRecord `json:"completed_orders,omitempty"`
	Fixed           *FixedPolicyState      `json:"fixed_policy,omitzero"`
}

// Completed receipts survive expiration and call-away. History from another
// writer must never be mistaken for this daemon's own terminal order.
type OrderRecord struct {
	Filled   int       `json:"filled,omitzero"`
	Closing  bool      `json:"closing,omitzero"`
	Ref      string    `json:"ref_id"`
	Symbol   string    `json:"symbol"`
	OptionID string    `json:"option_id"`
	At       time.Time `json:"at"`
}

func NewState() State {
	return State{Version: 1, Positions: map[string]Position{}, Intents: map[string]Intent{}}
}

func (s State) Validate() error {
	if s.Version != 1 || s.Positions == nil || s.Intents == nil || len(s.Positions) > 3 || len(s.Intents) > 3 {
		return errors.New("invalid journal schema or position count")
	}
	if len(s.CompletedOrders) > 1024 {
		return errors.New("completed order ledger requires operator archival; never discard ownership automatically")
	}
	for id, r := range s.CompletedOrders {
		if id == "" || r.Ref == "" || r.Symbol == "" || r.OptionID == "" || r.At.IsZero() || r.Filled < 0 || r.Filled > 1 {
			return errors.New("invalid completed order ownership record")
		}
	}
	for symbol, p := range s.Positions {
		if symbol == "" || len(symbol) > 5 || p.OwnedShares < 0 || (p.OwnedShares != 0 && p.OwnedShares != 100) || p.Cost < 0 || p.Cost > MaxMoney/100 {
			return errors.New("invalid journal inventory")
		}
		if p.OwnedShares == 100 && (p.Cost == 0 || p.Provenance == "" || p.PutID != "") {
			return errors.New("invalid journal share ownership")
		}
		if p.PutID != "" && (p.PutStrike <= 0 || p.PutStrike > MaxMoney/100 || p.PutOpenedAt.IsZero() || p.Provenance == "") {
			return errors.New("invalid journal put ownership")
		}
		for _, expiry := range []string{p.PutExpiry, p.CallExpiry} {
			if expiry != "" {
				if _, err := time.Parse("2006-01-02", expiry); err != nil {
					return errors.New("invalid journal contract expiry")
				}
			}
		}
	}
	for symbol, in := range s.Intents {
		if !in.Candidate.Closing && (in.Candidate.CloseLimit != 0 || in.Candidate.EntryCredit != 0) {
			return errors.New("opening intent contains close terms")
		}
		if in.Candidate.Closing && (in.Candidate.Kind != "put" || in.Candidate.CloseLimit <= 0 || in.Candidate.CloseLimit > MaxMoney/100 || in.Candidate.EntryCredit <= 0 || in.Candidate.EntryCredit > MaxMoney) {
			return errors.New("invalid close intent")
		}
		if symbol != in.Candidate.Symbol || in.Ref == "" || in.Candidate.ID == "" || in.Candidate.Strike <= 0 || in.Candidate.Strike > MaxMoney/100 || in.At.IsZero() {
			return errors.New("invalid journal submission intent")
		}
	}
	if s.Fixed != nil {
		return s.Fixed.validate(s)
	}
	return nil
}
