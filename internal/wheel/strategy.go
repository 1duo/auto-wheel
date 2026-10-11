package wheel

import (
	"cmp"
	"errors"
	"math"
	"slices"
	"time"
)

var Eastern = func() *time.Location {
	l, err := time.LoadLocation("America/New_York")
	if err != nil {
		panic(err)
	}
	return l
}()

func TradingWindow(now time.Time) bool {
	t := now.In(Eastern)
	if t.Weekday() == time.Saturday || t.Weekday() == time.Sunday {
		return false
	}
	if !CalendarKnown(now) {
		return false
	}
	date := t.Format("2006-01-02")
	if isExchangeHoliday(date) {
		return false
	}
	m := t.Hour()*60 + t.Minute()
	if isEarlyClose(date) {
		return m >= 575 && m < 765
	}
	return m >= 575 && m < 945
}

// Select uses the earliest listed eligible expiry. Failed liquidity checks do
// not silently move to a later week. Calls use delta first, then time-value ROI.
func Select(c Config, symbol, kind string, quotes []Quote, cost Cents, now time.Time) (Candidate, bool) {
	return selectCandidate(c, symbol, kind, quotes, cost, now, true)
}

// PreviewCandidate ignores quote age only for non-trading compatibility review.
// Its timestamps are retained; Execute never accepts it without fresh re-selection.
func PreviewCandidate(c Config, symbol, kind string, quotes []Quote, cost Cents, now time.Time) (Candidate, bool) {
	return selectCandidate(c, symbol, kind, quotes, cost, now, false)
}

// selectionModeFor resolves the active discipline without clamping legacy
// empty-mode custom bands. Explicit named modes require their exact preset
// put bands; unknown modes are rejected even when Validate was bypassed.
func selectionModeFor(c Config) (SelectionMode, bool) {
	switch c.SelectionMode {
	case "":
		return SelectionBalanced, true
	case SelectionConservative:
		if c.PutMinDelta != .05 || c.PutMaxDelta != .15 {
			return "", false
		}
		return SelectionConservative, true
	case SelectionBalanced:
		if c.PutMinDelta != .15 || c.PutMaxDelta != .35 {
			return "", false
		}
		return SelectionBalanced, true
	default:
		return "", false
	}
}

func selectCandidate(c Config, symbol, kind string, quotes []Quote, cost Cents, now time.Time, requireFresh bool) (Candidate, bool) {
	mode, ok := selectionModeFor(c)
	if !ok {
		return Candidate{}, false
	}
	date := now.In(Eastern).Format("2006-01-02")
	today, _ := time.Parse("2006-01-02", date)
	nearest := ""
	for _, q := range quotes {
		if q.Symbol != symbol || q.Kind != kind {
			continue
		}
		exp, err := time.Parse("2006-01-02", q.Expiry)
		if err != nil {
			continue
		}
		days := int(exp.Sub(today).Hours() / 24)
		if days < 1 || days > c.MaxDays {
			continue
		}
		// Thursday supports Friday exchange holidays. Other daily expiries are calls only.
		if kind == "put" && exp.Weekday() != time.Friday && !(exp.Weekday() == time.Thursday && q.Weekly) {
			continue
		}
		if nearest == "" || q.Expiry < nearest {
			nearest = q.Expiry
		}
	}
	var candidates []Candidate
	if nearest == "" {
		return Candidate{}, false
	}
	for _, q := range quotes {
		if q.Expiry != nearest || q.Symbol != symbol || q.Kind != kind || !q.Standard || !q.Tradable || q.Strike <= 0 || q.Strike > MaxMoney/100 || q.Spot <= 0 || q.Bid < c.MinBid || q.Ask < q.Bid || q.Ask > MaxMoney/100 || q.OI < c.MinOI || q.BidSize < 1 || q.Tick < 1 || math.IsNaN(q.Delta) || math.IsInf(q.Delta, 0) {
			continue
		}
		if requireFresh && (!fresh(q.At, now, c.MaxQuoteAgeSeconds) || !fresh(q.UnderlyingAt, now, c.MaxQuoteAgeSeconds)) {
			continue
		}
		if int64(q.Ask-q.Bid)*10000 > int64(q.Ask)*int64(c.MaxSpreadBPS) {
			continue
		}
		delta := math.Abs(q.Delta)
		if kind == "put" && (q.Delta >= 0 || delta < c.PutMinDelta || delta > c.PutMaxDelta || q.Strike >= q.Spot) {
			continue
		}
		if kind == "call" && (q.Delta <= 0 || delta < c.CallMinDelta || delta > c.CallMaxDelta || (!c.AllowBelowCost && q.Strike < cost)) {
			continue
		}
		bid := q.Bid / q.Tick * q.Tick
		premium := bid*100 - c.Fee
		if premium <= 0 {
			continue
		}
		exp, _ := time.Parse("2006-01-02", q.Expiry)
		days := exp.Sub(today).Hours() / 24
		q.Bid = bid
		a := Candidate{Quote: q, NetPremium: premium, Collateral: q.Strike * 100, Extrinsic: bid}
		a.Quantity = 1
		if kind == "call" {
			a.Extrinsic = max(0, bid-max(0, q.Spot-q.Strike))
			a.CallAwayPnL = (q.Strike-cost)*100 + premium
			a.ReturnPerDay = float64(a.Extrinsic*100-c.Fee) / float64(q.Spot*100) / days
		} else {
			a.ReturnPerDay = float64(premium) / float64(a.Collateral) / days
		}
		candidates = append(candidates, a)
	}
	slices.SortFunc(candidates, func(a, b Candidate) int {
		if kind == "call" {
			if d := cmp.Compare(b.Delta, a.Delta); d != 0 {
				return d
			}
		} else if mode == SelectionConservative {
			if d := cmp.Compare(math.Abs(a.Delta), math.Abs(b.Delta)); d != 0 {
				return d
			}
		}
		return cmp.Or(cmp.Compare(b.ReturnPerDay, a.ReturnPerDay), cmp.Compare(a.Collateral, b.Collateral), cmp.Compare(a.ID, b.ID))
	})
	if len(candidates) == 0 {
		return Candidate{}, false
	}
	return candidates[0], true
}

func fresh(at, now time.Time, seconds int) bool {
	age := now.Sub(at)
	return !at.IsZero() && age >= -2*time.Second && age <= time.Duration(seconds)*time.Second
}

// Plan counts all account exposure, including unknown positions and equity
// orders. Only explicitly owned inventory can receive a new covered call.
func Plan(c Config, st State, s Snapshot, candidates []Candidate, now time.Time) ([]Candidate, error) {
	mode, ok := selectionModeFor(c)
	if !ok {
		return nil, errors.New("unknown selection mode")
	}
	if s.Account != c.Account || st.Account != c.Account || !fresh(s.At, now, c.MaxQuoteAgeSeconds) {
		return nil, errors.New("account binding or snapshot freshness failed")
	}
	if s.AvailableCash < 0 || s.AvailableCash > MaxMoney {
		return nil, errors.New("invalid cash")
	}
	active := map[string]bool{}
	busy := map[string]bool{}
	shares := map[string]Shares{}
	if s.ExternalCapital < 0 || s.ExternalCapital > MaxMoney {
		return nil, errors.New("invalid external inventory capital")
	}
	commitment := s.ExternalCapital
	addCommitment := func(amount Cents) error {
		if amount < 0 || amount > MaxMoney-commitment {
			return errors.New("account capital is out of range")
		}
		commitment += amount
		return nil
	}
	for _, symbol := range s.ExternalSymbols {
		if symbol == "" {
			return nil, errors.New("external position identity missing")
		}
		busy[symbol] = true
		if c.SymbolLimitScope != "managed" {
			active[symbol] = true
		}
	}
	for _, p := range s.Shares {
		if p.Symbol == "" || p.Quantity < 0 || p.Quantity > 1_000_000 || p.Available < 0 || p.Available > p.Quantity || p.Cost < 0 || p.Cost > MaxMoney || (p.Quantity > 0 && p.Cost == 0) {
			return nil, errors.New("invalid share position")
		}
		if _, ok := shares[p.Symbol]; ok {
			return nil, errors.New("duplicate share position")
		}
		shares[p.Symbol] = p
		if p.Quantity > 0 {
			if p.Cost > MaxMoney/Cents(p.Quantity) {
				return nil, errors.New("share capital is out of range")
			}
			if err := addCommitment(p.Cost * Cents(p.Quantity)); err != nil {
				return nil, err
			}
			if c.SymbolLimitScope != "managed" || st.Positions[p.Symbol].OwnedShares > 0 || st.Positions[p.Symbol].PutID != "" {
				active[p.Symbol] = true
			} else {
				busy[p.Symbol] = true
			}
		}
	}
	for _, p := range s.Options {
		if p.Symbol == "" || p.Quantity <= 0 || p.Strike <= 0 || p.Strike > MaxMoney/100 || (p.Kind != "put" && p.Kind != "call") {
			return nil, errors.New("unsupported option exposure")
		}
		active[p.Symbol] = true
		busy[p.Symbol] = true
		if p.Kind == "call" && (p.Quantity > 3 || shares[p.Symbol].Quantity < p.Quantity*100) {
			return nil, errors.New("existing short call lacks share coverage")
		}
		if p.Kind == "put" {
			if p.Quantity > 3 {
				return nil, errors.New("put quantity exceeds account scope")
			}
			if err := addCommitment(p.Strike * 100 * Cents(p.Quantity)); err != nil {
				return nil, err
			}
		}
	}
	for _, o := range s.Orders {
		if o.Terminal() {
			continue
		}
		active[o.Symbol] = true
		busy[o.Symbol] = true
		if o.Kind == "put" && o.Side == "sell" && o.Effect == "open" {
			if o.Strike <= 0 || o.Strike > MaxMoney/100 || o.Quantity < 1 || o.Quantity > 3 || o.Filled < 0 || o.Filled > o.Quantity {
				return nil, errors.New("invalid pending put collateral")
			}
			if err := addCommitment(o.Strike * 100 * Cents(o.Quantity-o.Filled)); err != nil {
				return nil, err
			}
		}
	}
	// An uncertain submission globally freezes new risk until reconciliation.
	if len(st.Intents) > 0 {
		return nil, errors.New("order submission awaiting broker reconciliation")
	}
	for symbol, p := range st.Positions {
		if p.PutID != "" {
			present := false
			for _, o := range s.Options {
				if o.ID == p.PutID && o.Kind == "put" {
					present = true
				}
			}
			if !present && shares[symbol].Quantity == 0 {
				if p.PutStrike <= 0 || p.PutStrike > MaxMoney/100 {
					return nil, errors.New("unsettled put capital is out of range")
				}
				if err := addCommitment(p.PutStrike * 100); err != nil {
					return nil, err
				}
			}
		}
		if p.PutID != "" || p.OwnedShares > 0 || !p.MissingSince.IsZero() {
			active[symbol] = true
		}
		if p.PutID != "" || !p.MissingSince.IsZero() {
			busy[symbol] = true
		}
	}
	if len(active) > c.MaxSymbols {
		return nil, errors.New("account exceeds active symbol limit")
	}
	slices.SortFunc(candidates, func(a, b Candidate) int {
		priority := func(kind string) int {
			if kind == "call" {
				return 0
			}
			return 1
		}
		if p := cmp.Compare(priority(a.Kind), priority(b.Kind)); p != 0 {
			return p
		}
		if mode == SelectionConservative && a.Kind == "put" && b.Kind == "put" {
			if d := cmp.Compare(math.Abs(a.Delta), math.Abs(b.Delta)); d != 0 {
				return d
			}
		}
		return cmp.Or(cmp.Compare(b.ReturnPerDay, a.ReturnPerDay), cmp.Compare(a.Symbol, b.Symbol))
	})
	cash := max(0, s.AvailableCash-c.Reserve)
	budget := max(0, c.MaxCollateral-commitment)
	var out []Candidate
	for _, a := range candidates {
		if !slices.Contains(c.Symbols, a.Symbol) || busy[a.Symbol] || !fresh(a.At, now, c.MaxQuoteAgeSeconds) || !fresh(a.UnderlyingAt, now, c.MaxQuoteAgeSeconds) {
			continue
		}
		if a.Kind == "call" {
			p := st.Positions[a.Symbol]
			h := shares[a.Symbol]
			if p.OwnedShares != 100 || h.Quantity != 100 || h.Available < 100 || p.Cost <= 0 {
				continue
			}
		} else if a.Kind == "put" {
			if active[a.Symbol] || shares[a.Symbol].Quantity != 0 || len(active) >= c.MaxSymbols || a.Collateral+c.Fee > budget || a.Collateral+c.Fee > cash {
				continue
			}
			cash -= a.Collateral + c.Fee
			budget -= a.Collateral + c.Fee
		} else {
			continue
		}
		active[a.Symbol] = true
		busy[a.Symbol] = true
		out = append(out, a)
	}
	return out, nil
}
