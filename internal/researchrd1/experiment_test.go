package researchrd1

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"autowheel/internal/researchcampaign"
)

type explicitTariffSource struct {
	ReplaySource
	tariff Tariff
}

func (s explicitTariffSource) Tariff(time.Time) (Tariff, error) { return s.tariff, nil }

func TestStockCostCallFloor(t *testing.T) {
	clock := stockTime("2021-01-04 15:29")
	s := &ExperimentSource{costs: ExperimentCosts{PlanSHA: strings.Repeat("b", 64), DesignedAt: stockTime("2026-10-17 15:00"), Scale: 1}, callEligibility: StockCostCallFloor}
	c := Contract{Kind: "call", Strike: 10000}
	for _, scale := range []int64{1, 2} {
		s.costs.Scale = scale
		boundary := 100*c.Strike - 100*scale
		for _, delta := range []int64{0, 1} {
			p := Position{Shares: 100, StockCost: boundary + delta, AvailableAt: clock}
			allowed, err := s.CallAllowed(p, c, clock)
			if err != nil || allowed != (delta == 0) {
				t.Fatalf("physical fee/basis boundary: scale=%d delta=%d allowed=%v err=%v", scale, delta, allowed, err)
			}
		}
	}
	for _, p := range []Position{{}, {Shares: 100, StockCost: -1}, {Shares: 100, StockCost: MaxMoney}, {Shares: 100, StockCost: 1, AvailableAt: clock.Add(time.Minute)}} {
		if _, err := s.CallAllowed(p, c, clock); err == nil {
			t.Fatal("unknown, unsettled or overflowing basis admitted", p)
		}
	}
	if allowed, err := (&ExperimentSource{}).CallAllowed(Position{}, Contract{}, clock); err != nil || !allowed {
		t.Fatal("default packet behavior changed", allowed, err)
	}
	for _, p := range []ExperimentPacket{{CallEligibility: "unknown"}, {CallEligibility: StockCostCallFloor, ContractAssumption: PilotStandardContractModel}, {CallEligibility: StockCostCallFloor, ContractAssumption: ConditionalFitContractModel}} {
		if _, _, _, _, err := NewExperimentSource(p); err == nil || !strings.Contains(err.Error(), "call eligibility") {
			t.Fatal("new rule expanded an old assumption boundary", err)
		}
	}
}

func TestSimpleResearchCostAndSelectionBoundaries(t *testing.T) {
	clock := time.Date(2021, 1, 4, 20, 29, 0, 0, time.UTC)
	model := ExperimentCosts{PlanSHA: strings.Repeat("b", 64), DesignedAt: time.Date(2026, 10, 17, 15, 0, 0, 0, time.UTC), Scale: 1}
	source := &ExperimentSource{costs: model}
	fee, err := reviewedTariff(source, clock)
	if err != nil || !fee.Fees.KnownAt.Equal(model.DesignedAt) || fee.Fees.OptionOpen != 65 || fee.StockBuyFee != 100 || fee.Fees.PhysicalExercise != 100 || fee.PhysicalStockSale.FlatFee != 0 {
		t.Fatalf("explicit costs lost their real design clock or all-in delivery fee: %+v %v", fee, err)
	}
	// Serializing a tariff cannot turn a 2026 experiment into causal 2021 fees.
	raw, err := json.Marshal(fee)
	if err != nil {
		t.Fatal(err)
	}
	var historical Tariff
	if err := json.Unmarshal(raw, &historical); err != nil {
		t.Fatal(err)
	}
	if _, err := reviewedTariff(explicitTariffSource{tariff: historical}, clock); err == nil {
		t.Fatal("future modeled costs were accepted as historical fees")
	}
	changed := fee
	changed.StockBuyFee++
	if _, err := reviewedTariff(explicitTariffSource{tariff: changed}, clock); err == nil {
		t.Fatal("modified experimental costs accepted")
	}
	source.costs.Scale = 2
	doubled, err := reviewedTariff(source, clock)
	if err != nil || doubled.Fees.OptionOpen != 130 || doubled.StockBuyFee != 200 || doubled.Fees.PhysicalExercise != 200 {
		t.Fatal("doubled-cost scenario differs", doubled, err)
	}
	stock := Book{Bid: 10000, Ask: 10000}
	menu := []Contract{{ID: "one", Symbol: "AAPL", Kind: "call", Strike: 10100, Expiry: "2021-02-05"},
		{ID: "five", Symbol: "AAPL", Kind: "call", Strike: 10500, Expiry: "2021-02-05"}}
	profile := Profile{Mode: "aggressive", Exit: "hold"}
	old, err := Choose(profile, "call", "2021-01-04", "AAPL", stock, menu)
	if err != nil || old == nil || old.ID != "one" {
		t.Fatal("frozen RD1 selection changed", old, err)
	}
	chosen, err := source.Choose(profile, "call", "2021-01-04", "AAPL", stock, menu)
	if err != nil || chosen == nil || chosen.ID != "five" || profile.Mode != "aggressive" {
		t.Fatal("growth-focused call target or profile differs", chosen, err)
	}
	// A missing strike in the closest expiry must not select a farther expiry.
	menu[1].Expiry = "2021-02-12"
	if chosen, err := source.Choose(profile, "call", "2021-01-04", "AAPL", stock, menu); err != nil || chosen != nil {
		t.Fatal("selection replaced the closest expiry", chosen, err)
	}
	p := Packet{Schema: "AutoWheelSimpleResearchDataV1", ProtocolSHA: model.PlanSHA, AdmissionStatus: "reviewed_sources_under_explicit_modeled_costs"}
	if _, _, _, _, err := NewPacketSource(p); err == nil {
		t.Fatal("strict RD1 admitted a modeled-cost experiment")
	}
	calendar := stockControlFixture()
	p.AdmissionSHA, p.CalendarSHA, p.ClearingSHA = strings.Repeat("a", 64), strings.Repeat("a", 64), strings.Repeat("a", 64)
	p.First, p.Last, p.Calendar, p.Clearing = calendar.First, calendar.Last, calendar.Calendar, calendar.Clearing
	p.Policies = []OpeningPolicy{{From: "2023-01-01", Through: "2025-12-31", Symbols: []string{"AAPL"}, Profile: profile, SourceSHA: strings.Repeat("a", 64)}}
	p.Terms = []Terms{{Symbol: "AAPL", From: stockTime("2022-01-01 00:00"), Through: stockTime("2025-12-31 23:59"), KnownAt: stockTime("2022-01-01 00:00"), Shares: 100, Multiplier: 100, SourceSHA: strings.Repeat("a", 64)}}
	e := ExperimentPacket{Schema: "AutoWheelSimpleResearchPacketV1", Costs: model, Data: p}
	if _, _, _, _, err := NewExperimentSource(e); err != nil {
		t.Fatal("experimental framing rejected its separate explicit costs", err)
	}
	t.Run("retained names can have menus outside the opening list", func(t *testing.T) {
		copy := e
		observation := Observation{At: stockTime("2023-01-03 15:29"), SourceSHA: strings.Repeat("a", 64), Menus: map[string][]Contract{}}
		for i := range 13 {
			observation.Menus[fmt.Sprintf("S%d", i)] = nil
		}
		copy.Data.Observations = []Observation{observation}
		source, _, _, _, err := NewExperimentSource(copy)
		if err != nil {
			t.Fatal("retained-name menu coverage was rejected", err)
		}
		universe, _, err := source.EntryUniverse(observation.At)
		if err != nil || !slices.Equal(universe, []string{"AAPL"}) {
			t.Fatal("extra source menus expanded the opening list", universe, err)
		}
		if _, _, _, _, err := newPacketSource(copy.Data, false); err == nil || !strings.Contains(err.Error(), "observation") {
			t.Fatal("strict RD1 source envelope changed", err)
		}
		for i := 13; i < 51; i++ {
			observation.Menus[fmt.Sprintf("S%d", i)] = nil
		}
		if _, _, _, _, err := NewExperimentSource(copy); err == nil {
			t.Fatal("experimental menus escaped their bounded source envelope")
		}
	})
	e.Data.Policies[0].Symbols = []string{"QCOM"}
	if _, _, _, _, err := NewExperimentSource(e); err == nil {
		t.Fatal("excluded experimental symbol accepted")
	}
	e.Data.Policies[0].Symbols = []string{"AAPL"}
	e.Data.Unqualified = []string{"unknown_deliverable"}
	if _, _, _, _, err := NewExperimentSource(e); err == nil {
		t.Fatal("modeled fees erased a source qualification gap")
	}
	e.Data.Unqualified = nil
	e.Data.Terms = nil
	if _, _, _, _, err := NewExperimentSource(e); err == nil {
		t.Fatal("missing experimental deliverable terms accepted")
	}
	e.Data.Terms = p.Terms
	e.Data.Tariffs = []Tariff{historical}
	if _, _, _, _, err := NewExperimentSource(e); err == nil {
		t.Fatal("experimental packet mixed historical and modeled tariffs")
	}
}

func TestExploratoryPilotContractBoundary(t *testing.T) {
	p := Packet{Schema: "AutoWheelSimpleResearchDataV1", ProtocolSHA: strings.Repeat("b", 64),
		AdmissionStatus: "observed_pilot_prices_with_explicit_contract_assumption", AdmissionSHA: strings.Repeat("a", 64),
		CalendarSHA: strings.Repeat("a", 64), ClearingSHA: strings.Repeat("a", 64),
		First: "2021-01-04", Last: "2021-04-07",
		Policies: []OpeningPolicy{{From: "2021-01-01", Through: "2021-12-31", Symbols: []string{"AAPL", "MSFT"},
			Profile: Profile{Mode: "aggressive", Exit: "hold"}, SourceSHA: strings.Repeat("a", 64)}}}
	// Synthetic framing only, never historical source admission.
	for day := stockTime("2021-01-01 00:00"); !day.After(stockTime("2021-04-16 00:00")); day = day.AddDate(0, 0, 1) {
		exchange := researchcampaign.ExchangeDay{Date: day.Format(time.DateOnly)}
		bank := researchcampaign.ClearingDay{Date: exchange.Date}
		if day.Weekday() != time.Saturday && day.Weekday() != time.Sunday && !slices.Contains([]string{"2021-01-01", "2021-01-18", "2021-02-15", "2021-04-02"}, exchange.Date) {
			exchange.OpenAt, exchange.CloseAt = day.Add(9*time.Hour+30*time.Minute), day.Add(16*time.Hour)
			bank.Open, bank.AvailableAt = true, exchange.OpenAt
		}
		p.Calendar, p.Clearing = append(p.Calendar, exchange), append(p.Clearing, bank)
	}
	e := ExperimentPacket{Schema: "AutoWheelSimpleResearchPacketV1", Costs: ExperimentCosts{PlanSHA: p.ProtocolSHA,
		DesignedAt: stockTime("2026-10-17 09:00"), Scale: 1}, ContractAssumption: PilotStandardContractModel, Data: p}
	source, _, _, _, err := NewExperimentSource(e)
	if err != nil {
		t.Fatal(err)
	}
	c := Contract{ID: "AAPL  210205C00130000", Symbol: "AAPL", Kind: "call", Strike: 13000, Expiry: "2021-02-05"}
	if err := source.Standard(c, stockTime("2021-01-04 15:29")); err != nil {
		t.Fatal("explicit pilot model unavailable", err)
	}
	source.packet.Unsupported = []UnsupportedAction{{Symbol: "AAPL", At: stockTime("2021-01-05 00:00"), Reason: "synthetic action", SourceSHA: strings.Repeat("a", 64)}}
	if err := source.Standard(c, stockTime("2021-01-05 15:29")); err == nil {
		t.Fatal("known action ignored under the contract model")
	}
	e.ContractAssumption = ""
	if _, _, _, _, err := NewExperimentSource(e); err == nil {
		t.Fatal("pilot assumption became reviewed historical terms")
	}
	e.ContractAssumption = PilotStandardContractModel
	e.Data.Last = "2021-04-08"
	if _, _, _, _, err := NewExperimentSource(e); err == nil {
		t.Fatal("exploratory assumption escaped the fixed pilot")
	}
}

func TestConditionalFitContractBoundary(t *testing.T) {
	p := Packet{Schema: "AutoWheelSimpleResearchDataV1", ProtocolSHA: strings.Repeat("b", 64),
		AdmissionStatus: "conditional_observed_prices_pending_contract_action_review", AdmissionSHA: strings.Repeat("a", 64),
		CalendarSHA: strings.Repeat("a", 64), ClearingSHA: strings.Repeat("a", 64), First: "2021-01-04", Last: "2022-12-30",
		Policies: []OpeningPolicy{{From: "2021-01-01", Through: "2022-12-31", Symbols: []string{"AMD"},
			Profile: Profile{Mode: "aggressive", Exit: "hold"}, SourceSHA: strings.Repeat("a", 64)}}}
	for day := stockTime("2021-01-01 00:00"); !day.After(stockTime("2023-01-10 00:00")); day = day.AddDate(0, 0, 1) {
		exchange := researchcampaign.ExchangeDay{Date: day.Format(time.DateOnly)}
		bank := researchcampaign.ClearingDay{Date: exchange.Date}
		if day.Weekday() != time.Saturday && day.Weekday() != time.Sunday {
			exchange.OpenAt, exchange.CloseAt = day.Add(9*time.Hour+30*time.Minute), day.Add(16*time.Hour)
			bank.Open, bank.AvailableAt = true, exchange.OpenAt
		}
		p.Calendar, p.Clearing = append(p.Calendar, exchange), append(p.Clearing, bank)
	}
	e := ExperimentPacket{Schema: "AutoWheelSimpleResearchPacketV1", Costs: ExperimentCosts{PlanSHA: p.ProtocolSHA,
		DesignedAt: stockTime("2026-10-17 09:00"), Scale: 1}, ContractAssumption: ConditionalFitContractModel, Data: p}
	source, _, _, _, err := NewExperimentSource(e)
	if err != nil {
		t.Fatal(err)
	}
	c := Contract{ID: "AMD   210205C00100000", Symbol: "AMD", Kind: "call", Strike: 10000, Expiry: "2021-02-05"}
	if err := source.Standard(c, stockTime("2021-01-04 15:29")); err != nil {
		t.Fatal(err)
	}
	source.packet.Unsupported = []UnsupportedAction{{Symbol: "AMD", At: stockTime("2021-01-05 00:00"), Reason: "synthetic action", SourceSHA: strings.Repeat("a", 64)}}
	if err := source.Standard(c, stockTime("2021-01-05 15:29")); err == nil {
		t.Fatal("conditional fit ignored an unsupported action")
	}
	e.ContractAssumption = ""
	if _, _, _, _, err := NewExperimentSource(e); err == nil {
		t.Fatal("conditional fit became reviewed history")
	}
	e.ContractAssumption = ConditionalFitContractModel
	e.Data.Last = "2023-01-03"
	if _, _, _, _, err := NewExperimentSource(e); err == nil {
		t.Fatal("conditional fit escaped its date boundary")
	}
	e.Data.Last = "2022-12-30"
	e.Data.Policies[0].Symbols = []string{"QCOM"}
	if _, _, _, _, err := NewExperimentSource(e); err == nil {
		t.Fatal("conditional fit accepted an excluded symbol")
	}
	t.Run("later path inventory keeps a distinct assumption and fixed boundary", func(t *testing.T) {
		e.ContractAssumption = ConditionalPathContractModel
		e.Data.Policies[0].Symbols = []string{"CRM"}
		e.Data.Policies[0].Through = "2023-01-03"
		e.Data.Last = "2023-01-03"
		source, _, _, _, err := NewExperimentSource(e)
		if err != nil {
			t.Fatal(err)
		}
		c := Contract{ID: "CRM   230203C00150000", Symbol: "CRM", Kind: "call", Strike: 15000, Expiry: "2023-02-03"}
		clock := stockTime("2023-01-03 15:29")
		if err := source.Standard(c, clock); err != nil {
			t.Fatal("later fixed-cohort path unavailable", err)
		}
		for _, name := range []string{"QCOM", "META", "FB", "TSLA"} {
			c.Symbol = name
			if err := source.Standard(c, clock); err == nil {
				t.Fatal("path review escaped its frozen cohort", name)
			}
		}
		c.Symbol = "CRM"
		source.packet.Unsupported = []UnsupportedAction{{Symbol: "CRM", At: clock, Reason: "synthetic action", SourceSHA: strings.Repeat("a", 64)}}
		if err := source.Standard(c, clock); err == nil {
			t.Fatal("path review ignored a known action")
		}
		e.Data.Terms = []Terms{{Symbol: "CRM"}}
		if _, _, _, _, err := NewExperimentSource(e); err == nil {
			t.Fatal("path assumption manufactured reviewed terms")
		}
		e.Data.Terms = nil
		e.Data.Last = "2026-10-12"
		if _, _, _, _, err := NewExperimentSource(e); err == nil {
			t.Fatal("path review escaped the fixed historical cutoff")
		}
	})
}
