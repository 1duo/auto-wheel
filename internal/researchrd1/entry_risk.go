package researchrd1

import "errors"

const OriginalDDMinus40 = "original_dd_minus40_v1"

// The frozen budget is the original conservative 2021–2022 sampled drawdown.
// The denominator times MaxMoney fits int64; this is not a tunable risk grid.
const entryBudgetNumerator int64 = 618445
const entryBudgetDenominator int64 = 8892202

// entryStressAllowed models one -40% contractual terminal payoff, not an
// immediate option valuation or a bound on subsequent market drawdown.
func entryStressAllowed(source ReplaySource, state State, e Effect, obs Observation, f Tariff) (bool, error) {
	s, ok := source.(*ExperimentSource)
	if !ok || s.entryRiskRule == "" || e.Action != "sell_put" {
		return true, nil
	}
	if s.entryRiskRule != OriginalDDMinus40 || e.Profile.Mode != "conservative" ||
		s.riskPeak <= 0 || s.riskPeak > MaxMoney || e.Contract.Kind != "put" ||
		e.Contract.Strike <= 0 || e.Contract.Strike > MaxMoney/100 || e.Price <= 0 || e.Price > MaxMoney/100 ||
		e.Fee < 0 || e.Fee > 10000 || f.Fees.PhysicalExercise < 0 || f.Fees.PhysicalExercise > 10000 {
		return false, errors.New("entry stress requires known conservative budget, inventory and fees")
	}
	if _, held := state.Positions[e.Symbol]; held {
		return false, errors.New("entry stress put starts over owned inventory")
	}
	receivables, err := state.Receivables()
	if err != nil {
		return false, err
	}
	terminal, err := sum(state.Cash, receivables)
	if err != nil {
		return false, err
	}
	stockBid := func(symbol string) (int64, error) {
		b, known := obs.Stocks[symbol]
		if !known || b.Bid <= 0 || b.Bid > MaxMoney/100 {
			return 0, errors.New("missing or invalid underlying bid for entry stress")
		}
		return b.Bid, nil
	}
	for symbol, p := range state.Positions {
		bid, err := stockBid(symbol)
		if err != nil {
			return false, err
		}
		if p.Shares != 0 && p.Shares != 100 {
			return false, errors.New("entry stress requires standard100-share lots")
		}
		if p.Shares == 100 {
			terminal, err = sum(terminal, 60*bid)
			if err != nil {
				return false, err
			}
		}
		if p.Short == nil {
			continue
		}
		c := p.Short.Contract
		if c.Strike <= 0 || c.Strike > MaxMoney/100 || c.Kind != "put" && c.Kind != "call" ||
			c.Kind == "put" && p.Shares != 0 || c.Kind == "call" && p.Shares != 100 {
			return false, errors.New("invalid covered inventory in entry stress")
		}
		intrinsic := max(int64(0), 100*c.Strike-60*bid)
		if c.Kind == "call" {
			intrinsic = max(int64(0), 60*bid-100*c.Strike)
		}
		if intrinsic > 0 {
			terminal, err = sum(terminal, -intrinsic-int64(f.Fees.PhysicalExercise))
			if err != nil {
				return false, err
			}
		}
	}
	bid, err := stockBid(e.Symbol)
	if err != nil {
		return false, err
	}
	terminal, err = sum(terminal, 100*e.Price-e.Fee)
	if err != nil {
		return false, err
	}
	intrinsic := max(int64(0), 100*e.Contract.Strike-60*bid)
	if intrinsic > 0 {
		terminal, err = sum(terminal, -intrinsic-int64(f.Fees.PhysicalExercise))
		if err != nil {
			return false, err
		}
	}
	return terminal*entryBudgetDenominator >= s.riskPeak*(entryBudgetDenominator-entryBudgetNumerator), nil
}
