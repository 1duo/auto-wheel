package researchrd1

import (
	"strings"
	"testing"
)

func TestOriginalDDEntryStressBudget(t *testing.T) {
	s := &ExperimentSource{entryRiskRule: OriginalDDMinus40, riskPeak: 8_404_717}
	state := flatSeedState()
	f := ExperimentCosts{Scale: 1}.tariff(stockTime("2021-01-04 15:30"))
	e := Effect{Action: "sell_put", Symbol: "AAPL", Profile: Profile{Mode: "conservative", Exit: "hold"}, Contract: Contract{Symbol: "AAPL", Kind: "put", Strike: 11500}, Price: 175, Fee: 65}
	obs := Observation{Stocks: map[string]Book{"AAPL": {Bid: 12907}, "BA": {Bid: 22800}}}
	if allowed, err := entryStressAllowed(s, state, e, obs, f); err != nil || !allowed {
		t.Fatal("known lower-risk put rejected", allowed, err)
	}
	e.Symbol, e.Contract.Symbol, e.Contract.Strike, e.Price = "BA", "BA", 20500, 100
	if allowed, err := entryStressAllowed(s, state, e, obs, f); err != nil || allowed {
		t.Fatal("large one-lot shock passed", allowed, err)
	}
	e.Symbol, e.Contract.Symbol, e.Contract.Strike = "AAPL", "AAPL", 5000
	obs.Stocks["AAPL"] = Book{Bid: 5000}
	s.riskPeak = entryBudgetDenominator
	for _, scale := range []int64{1, 2} {
		f = ExperimentCosts{Scale: scale}.tariff(stockTime("2021-01-04 15:30"))
		e.Price, e.Fee = scale, 65*scale
		credit := 100*e.Price - e.Fee
		boundary := entryBudgetDenominator - entryBudgetNumerator + 200000 + 100*scale - credit
		for _, delta := range []int64{0, -1} {
			state.Cash = boundary + delta
			allowed, err := entryStressAllowed(s, state, e, obs, f)
			if err != nil || allowed != (delta == 0) {
				t.Fatalf("exact budget boundary scale=%d delta=%d: %v %v", scale, delta, allowed, err)
			}
		}
		state.Dividends["earned"] = Dividend{Amount: 1}
		if allowed, err := entryStressAllowed(s, state, e, obs, f); err != nil || !allowed {
			t.Fatal("earned receivable not included exactly once", allowed, err)
		}
		state.Cash++
		state.Dividends["earned"] = Dividend{Amount: 1, Paid: true}
		if allowed, err := entryStressAllowed(s, state, e, obs, f); err != nil || !allowed {
			t.Fatal("payment changed economic entry budget", allowed, err)
		}
		clear(state.Dividends)
	}
	delete(obs.Stocks, "AAPL")
	if allowed, err := entryStressAllowed(s, state, e, obs, f); err == nil || allowed {
		t.Fatal("missing risk input converted to a funded trade")
	}
	if allowed, err := entryStressAllowed(&ExperimentSource{}, State{}, Effect{}, Observation{}, Tariff{}); err != nil || !allowed {
		t.Fatal("legacy model acquired a new risk dependency")
	}
	for _, p := range []ExperimentPacket{{EntryRiskRule: "unknown"}, {EntryRiskRule: OriginalDDMinus40, ContractAssumption: PilotStandardContractModel}, {EntryRiskRule: OriginalDDMinus40, ContractAssumption: ConditionalPathContractModel}} {
		if _, _, _, _, err := NewExperimentSource(p); err == nil || !strings.Contains(err.Error(), "entry risk rule") {
			t.Fatal("entry experiment escaped its source boundary", err)
		}
	}
}
