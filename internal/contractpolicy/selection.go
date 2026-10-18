// Package contractpolicy contains the fixed strike-distance policy shared by
// production integration and historical decision-parity checks. It cannot place orders.
package contractpolicy

import (
	"cmp"
	"errors"
	"slices"
	"time"
)

const maxPrice int64 = 1_000_000_000

type Book struct {
	Bid int64 `json:"bid_cents"`
	Ask int64 `json:"ask_cents"`
}

type Contract struct {
	ID     string `json:"id"`
	Symbol string `json:"symbol"`
	Expiry string `json:"expiry"`
	Kind   string `json:"kind"`
	Strike int64  `json:"strike_cents"`
}

// Choose selects identity before option liquidity is considered. A bad quote
// for that identity must skip the opening, rather than select a different strike.
// Spot is the exact bid/ask midpoint, represented as twice its value in cents.
func Choose(mode, kind, day, symbol string, stock Book, menu []Contract) (*Contract, error) {
	if !slices.Contains([]string{"conservative", "balanced", "aggressive"}, mode) ||
		!slices.Contains([]string{"put", "call"}, kind) || mode == "aggressive" && kind == "put" ||
		symbol == "" || slices.Contains([]string{"QCOM", "META", "FB"}, symbol) ||
		stock.Bid <= 0 || stock.Ask < stock.Bid || stock.Ask > maxPrice {
		return nil, errors.New("invalid fixed-policy input")
	}
	start, err := time.Parse(time.DateOnly, day)
	if err != nil || start.Format(time.DateOnly) != day {
		return nil, errors.New("invalid fixed-policy session date")
	}
	spot2 := stock.Bid + stock.Ask
	seen := make(map[string]bool, len(menu))
	var eligible []Contract
	for _, c := range menu {
		expiry, err := time.Parse(time.DateOnly, c.Expiry)
		if err != nil || expiry.Format(time.DateOnly) != c.Expiry || c.ID == "" || seen[c.ID] ||
			c.Strike <= 0 || c.Strike > maxPrice || !slices.Contains([]string{"put", "call"}, c.Kind) {
			return nil, errors.New("ambiguous fixed-policy contract menu")
		}
		seen[c.ID] = true
		dte := int(expiry.Sub(start) / (24 * time.Hour))
		if c.Symbol == symbol && c.Kind == kind && dte >= 21 && dte <= 45 &&
			4*spot2 <= 10*c.Strike && 10*c.Strike <= 6*spot2 {
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
	num, den := int64(9), int64(10)
	if mode == "balanced" && kind == "put" {
		num, den = 19, 20
	}
	if kind == "call" {
		num, den = 21, 20
		if mode == "balanced" {
			num, den = 103, 100
		}
	}
	var selected *Contract
	for _, c := range eligible {
		within := 2*c.Strike*den <= spot2*num
		if kind == "call" {
			within = 2*c.Strike*den >= spot2*num
		}
		if c.Expiry != expiry || !within {
			continue
		}
		better := selected == nil
		if selected != nil {
			better = c.Strike > selected.Strike
			if kind == "call" {
				better = c.Strike < selected.Strike
			}
			if c.Strike == selected.Strike {
				better = c.ID < selected.ID
			}
		}
		if better {
			selected = new(c)
		}
	}
	return selected, nil
}
