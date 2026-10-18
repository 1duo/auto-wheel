package contractpolicy

import "errors"

const maxCapital = 100 * maxPrice

// EntryBookUsable is the frozen midpoint-relative spread check. It applies to
// both the underlying and option; identity selection itself does not use it.
func EntryBookUsable(b Book) bool {
	return b.Bid > 0 && b.Ask >= b.Bid && b.Ask <= maxPrice &&
		(b.Ask-b.Bid)*20_000 <= 1500*(b.Ask+b.Bid)
}

// StockCostCallAllowed tests the selected strike without replacing it. The
// acquisition amount includes the stock-acquisition fee; premiums and dividends
// cannot reduce it. Call-away fees are either known costs or an explicit bound.
// This arithmetic does not prove ownership, settlement or the supplied costs.
func StockCostCallAllowed(strike, acquisition, callAwayFee int64) (bool, error) {
	if strike <= 0 || strike > maxPrice || acquisition <= 0 || acquisition > maxCapital ||
		callAwayFee < 0 || callAwayFee > maxCapital-acquisition {
		return false, errors.New("call floor needs bounded acquisition and delivery costs")
	}
	return 100*strike >= acquisition+callAwayFee, nil
}

type CapitalLimits struct {
	Account int64 `json:"account_cents"`
	Symbol  int64 `json:"symbol_cents"`
	Reserve int64 `json:"reserve_cents"`
}

type CapitalUse struct {
	// Available is settled cash after existing holds, before this opening.
	// Premium expected from this order is not available funding.
	Available       int64 `json:"available_cents"`
	NewCollateral   int64 `json:"new_collateral_cents"`
	Debit           int64 `json:"debit_cents"`
	AccountExposure int64 `json:"account_exposure_cents"`
	SymbolExposure  int64 `json:"symbol_exposure_cents"`
	Addition        int64 `json:"addition_cents"`
	ActiveSymbols   int   `json:"active_symbols"`
	NewSymbol       bool  `json:"new_symbol"`
}

// CanOpen matches the research cash/exposure arithmetic after existing holds
// have been removed exactly once. Limits must be declared; zero never means an
// unlimited account in this policy. The caller proves the actual account facts.
func CanOpen(l CapitalLimits, u CapitalUse) bool {
	for _, n := range []int64{l.Account, l.Symbol, l.Reserve, u.Available, u.NewCollateral, u.Debit, u.AccountExposure, u.SymbolExposure, u.Addition} {
		if n < 0 || n > maxCapital {
			return false
		}
	}
	if l.Account == 0 || l.Symbol == 0 || l.Symbol > l.Account || l.Reserve >= maxCapital ||
		u.ActiveSymbols < 0 || u.ActiveSymbols > 3 || u.NewSymbol && u.ActiveSymbols >= 3 ||
		u.NewCollateral > u.Available || u.Debit > u.Available-u.NewCollateral {
		return false
	}
	return u.Available-u.NewCollateral-u.Debit >= l.Reserve &&
		u.Addition <= l.Account && u.AccountExposure <= l.Account-u.Addition &&
		u.Addition <= l.Symbol && u.SymbolExposure <= l.Symbol-u.Addition
}
