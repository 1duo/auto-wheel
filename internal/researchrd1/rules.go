// Package researchrd1 contains only the frozen RD1 offline model rules.
// It has no broker, production configuration, Greek or OI dependency.
package researchrd1

import (
	"cmp"
	"errors"
	"slices"
	"strings"
	"time"
)

const (
	MaxMoney   int64 = 100_000_000_000
	Reserve    int64 = 1_680_944
	SymbolCap  int64 = 2_666_666
	AccountCap int64 = 8_000_000
)

type Profile struct {
	Mode string `json:"mode"`
	Exit string `json:"exit"`
}

func (p Profile) Validate() error {
	if !slices.Contains([]string{"conservative", "balanced", "aggressive"}, p.Mode) || !slices.Contains([]string{"hold", "tp50", "tp75", "tp90", "puttp50", "puttp75", "puttp90"}, p.Exit) {
		return errors.New("unknown frozen RD1 profile")
	}
	return nil
}

type Book struct {
	Bid int64 `json:"bid_cents"`
	Ask int64 `json:"ask_cents"`
}

func (b Book) Valid() bool {
	return b.Bid >= 0 && b.Ask > 0 && b.Ask >= b.Bid && b.Ask <= MaxMoney/100
}

func (b Book) EntryValid() bool {
	return b.Valid() && b.Bid > 0 && (b.Ask-b.Bid)*20_000 <= 1500*(b.Ask+b.Bid)
}

type Contract struct {
	ID     string `json:"id"`
	Symbol string `json:"symbol"`
	Expiry string `json:"expiry"`
	Kind   string `json:"kind"`
	Strike int64  `json:"strike_cents"`
}

// Choose uses only the observed identity menu and signal stock midpoint. The
// chosen contract is never replaced because its later quote is missing or bad.
func Choose(p Profile, kind, day, symbol string, stock Book, menu []Contract) (*Contract, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	start, err := time.Parse(time.DateOnly, day)
	if err != nil || start.Format(time.DateOnly) != day || !stock.Valid() || stock.Bid == 0 || !slices.Contains([]string{"put", "call"}, kind) || p.Mode == "aggressive" && kind == "put" {
		return nil, errors.New("invalid RD1 selection input")
	}
	spot2 := stock.Bid + stock.Ask
	var eligible []Contract
	seen := make(map[string]bool, len(menu))
	for _, c := range menu {
		expiry, err := time.Parse(time.DateOnly, c.Expiry)
		if err != nil || expiry.Format(time.DateOnly) != c.Expiry || c.ID == "" || seen[c.ID] || c.Strike <= 0 || c.Strike > MaxMoney/100 || !slices.Contains([]string{"put", "call"}, c.Kind) {
			return nil, errors.New("ambiguous observed contract identity")
		}
		seen[c.ID] = true
		dte := int(expiry.Sub(start) / (24 * time.Hour))
		if c.Symbol == symbol && c.Kind == kind && dte >= 21 && dte <= 45 && 4*spot2 <= 10*c.Strike && 10*c.Strike <= 6*spot2 {
			eligible = append(eligible, c)
		}
	}
	if len(eligible) == 0 {
		return nil, nil
	}
	distance := func(c Contract) int {
		expiry, _ := time.Parse(time.DateOnly, c.Expiry)
		days := int(expiry.Sub(start)/(24*time.Hour)) - 30
		return max(days, -days)
	}
	expiry := slices.MinFunc(eligible, func(a, b Contract) int {
		return cmp.Or(cmp.Compare(distance(a), distance(b)), cmp.Compare(a.Expiry, b.Expiry))
	}).Expiry
	numerator, denominator := int64(9), int64(10)
	if kind == "put" && p.Mode == "balanced" {
		numerator, denominator = 19, 20
	}
	if kind == "call" {
		switch p.Mode {
		case "conservative":
			numerator, denominator = 21, 20
		case "balanced":
			numerator, denominator = 103, 100
		case "aggressive":
			numerator, denominator = 101, 100
		}
	}
	var chosen *Contract
	for _, c := range eligible {
		within := 2*c.Strike*denominator <= spot2*numerator
		if kind == "call" {
			within = 2*c.Strike*denominator >= spot2*numerator
		}
		if c.Expiry != expiry || !within {
			continue
		}
		better := chosen == nil
		if chosen != nil {
			better = c.Strike > chosen.Strike
			if kind == "call" {
				better = c.Strike < chosen.Strike
			}
			if c.Strike == chosen.Strike {
				better = c.ID < chosen.ID
			}
		}
		if better {
			chosen = new(c)
		}
	}
	return chosen, nil
}

// Capacity checks entry debits and reserved capital without using future sale
// credits. reserved must include the proposed put's collateral. Exposure is
// strike collateral plus stock acquisition capital/fees.
func Capacity(settled, reserved, exposure, symbolExposure, debit, addition int64, active int, newSymbol bool) bool {
	return capacityForLimits(AccountCap, SymbolCap, settled, reserved, exposure, symbolExposure, debit, addition, active, newSymbol)
}

func (a *Account) capacity(settled, reserved, exposure, symbolExposure, debit, addition int64, active int, newSymbol bool) bool {
	if a.accountCap == 0 && a.symbolCap == 0 {
		return Capacity(settled, reserved, exposure, symbolExposure, debit, addition, active, newSymbol)
	}
	return capacityForLimits(a.accountCap, a.symbolCap, settled, reserved, exposure, symbolExposure, debit, addition, active, newSymbol)
}

func capacityForLimits(accountCap, symbolCap, settled, reserved, exposure, symbolExposure, debit, addition int64, active int, newSymbol bool) bool {
	if accountCap <= 0 || accountCap > MaxMoney || symbolCap <= 0 || symbolCap > accountCap {
		return false
	}
	for _, n := range []int64{settled, reserved, exposure, symbolExposure, debit, addition} {
		if n < 0 || n > MaxMoney {
			return false
		}
	}
	if active < 0 || active > 3 || newSymbol && active >= 3 || debit > settled || reserved > settled-debit {
		return false
	}
	return settled-debit-reserved >= Reserve && addition <= accountCap && exposure <= accountCap-addition && addition <= symbolCap && symbolExposure <= symbolCap-addition
}

// profitProfile keeps call inventory and its original policy intact when the
// preregistered exit closes puts only. It changes no opening selection.
func profitProfile(p Profile, kind string) Profile {
	if kind == "call" && strings.HasPrefix(p.Exit, "puttp") {
		p.Exit = "hold"
	}
	return p
}

func TakeProfit(p Profile, openingNetCredit, signalAsk, closeFee int64) (bool, error) {
	if err := p.Validate(); err != nil {
		return false, err
	}
	if openingNetCredit <= 0 || openingNetCredit > MaxMoney || signalAsk <= 0 || signalAsk > MaxMoney/100 || closeFee < 0 || closeFee > MaxMoney {
		return false, errors.New("missing bounded profit-close price or credit")
	}
	if p.Exit == "hold" {
		return false, nil
	}
	target := int64(50)
	switch strings.TrimPrefix(p.Exit, "put") {
	case "tp75":
		target = 75
	case "tp90":
		target = 90
	}
	return (openingNetCredit-100*signalAsk-closeFee)*100 >= target*openingNetCredit, nil
}

type Holding struct {
	Shares int   `json:"shares"`
	Stock  *Book `json:"stock,omitzero"`
	Short  *Book `json:"short,omitzero"`
}

// NAV never substitutes intrinsic value, future books or zero for a held mark.
func NAV(cash, receivables int64, holdings []Holding) (int64, error) {
	if cash < 0 || cash > MaxMoney || receivables < 0 || receivables > MaxMoney-cash || len(holdings) > 3 {
		return 0, errors.New("RD1 cash/receivable envelope")
	}
	nav := cash + receivables
	for _, h := range holdings {
		if h.Shares != 0 && h.Shares != 100 {
			return 0, errors.New("RD1 one-lot stock envelope")
		}
		if h.Shares > 0 {
			if h.Stock == nil || !h.Stock.Valid() || h.Stock.Bid <= 0 {
				return 0, errors.New("missing held stock bid")
			}
			amount := 100 * h.Stock.Bid
			if nav > MaxMoney-amount {
				return 0, errors.New("RD1 NAV overflow")
			}
			nav += amount
		}
		if h.Short != nil {
			// The frozen RD1 mark needs the positive short-liability ask. A
			// crossed bid still makes an entry book invalid; it cannot erase
			// that liability or justify a negative half-spread cost shock.
			if h.Short.Ask <= 0 || h.Short.Ask > MaxMoney/100 {
				return 0, errors.New("missing positive held option ask")
			}
			nav -= 100 * h.Short.Ask
		}
	}
	if nav < -MaxMoney || nav > MaxMoney {
		return 0, errors.New("RD1 signed NAV envelope")
	}
	return nav, nil
}
