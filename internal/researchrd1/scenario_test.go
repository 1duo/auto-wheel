package researchrd1

import (
	"strings"
	"testing"
	"time"

	"autowheel/internal/researchcampaign"
	"autowheel/internal/wheel"
)

// Synthetic source/mechanics checks, not charged historical cases, admitted
// market data, cost assumptions for RD1, or strategy-performance measurements.
type scenarioTestSource struct {
	ReplaySource
	c       Contract
	changed bool
	missing bool
	ref     int64
}

func (s scenarioTestSource) Sample(at time.Time) (Observation, error) {
	bid := int64(100)
	if at.In(wheel.Eastern).Format(time.DateOnly) == "2021-01-05" {
		bid = 200
	}
	obs := Observation{At: at, SourceSHA: strings.Repeat("a", 64),
		Stocks:  map[string]Book{"AAPL": {Bid: 10000, Ask: 10002}},
		Options: map[string]Book{s.c.ID: {Bid: bid, Ask: bid + 1}},
		Menus:   map[string][]Contract{"AAPL": {s.c}}}
	if s.missing && at.In(wheel.Eastern).Format("2006-01-02 15:04") == "2021-01-05 15:30" {
		obs.Options = nil
	}
	return obs, nil
}

func (s scenarioTestSource) EntryUniverse(at time.Time) ([]string, Profile, error) {
	p := Profile{Mode: "conservative", Exit: "hold"}
	if s.changed && at.In(wheel.Eastern).Format(time.DateOnly) > "2021-01-04" {
		p.Mode = "balanced"
	}
	return []string{"AAPL"}, p, nil
}

func (s scenarioTestSource) Tariff(at time.Time) (Tariff, error) {
	zero := researchcampaign.SaleFeeRule{NotionalDenominator: 1, ShareDenominator: 1}
	return Tariff{Fees: researchcampaign.DatedFees{Date: at.In(wheel.Eastern).Format(time.DateOnly),
		KnownAt: time.Date(2020, 12, 1, 0, 0, 0, 0, time.UTC), SourceSHA: strings.Repeat("a", 64), StockSale: zero},
		PhysicalStockSale: zero}, nil
}

func (scenarioTestSource) Standard(Contract, time.Time) error    { return nil }
func (scenarioTestSource) ReviewHoldings(State, time.Time) error { return nil }
func (scenarioTestSource) BeforeSession(researchcampaign.ExchangeDay, State) ([]Effect, error) {
	return nil, nil
}
func (s scenarioTestSource) Reference(_ string, day researchcampaign.ExchangeDay) (TradeReference, error) {
	price := s.ref
	if price == 0 {
		price = 80_000_000
	}
	return TradeReference{At: day.CloseAt, PriceMicros: price, SourceSHA: strings.Repeat("a", 64)}, nil
}

func TestRD1ScenarioBoundaries(t *testing.T) {
	at := func(value string) time.Time {
		t.Helper()
		v, err := time.ParseInLocation("2006-01-02 15:04", value, wheel.Eastern)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	var exchange []researchcampaign.ExchangeDay
	var clearing []researchcampaign.ClearingDay
	for d := at("2021-01-01 00:00"); !d.After(at("2021-02-12 00:00")); d = d.AddDate(0, 0, 1) {
		day := researchcampaign.ExchangeDay{Date: d.Format(time.DateOnly)}
		bank := researchcampaign.ClearingDay{Date: day.Date}
		if d.Weekday() != time.Saturday && d.Weekday() != time.Sunday && day.Date != "2021-01-01" && day.Date != "2021-01-18" {
			day.OpenAt, day.CloseAt = d.Add(9*time.Hour+30*time.Minute), d.Add(16*time.Hour)
			bank.Open, bank.AvailableAt = true, day.OpenAt
		}
		exchange, clearing = append(exchange, day), append(clearing, bank)
	}
	sessions, err := researchcampaign.NewSessionCalendar(exchange)
	if err != nil {
		t.Fatal(err)
	}
	banks, err := researchcampaign.NewSettlementCalendar(clearing)
	if err != nil {
		t.Fatal(err)
	}
	var days []researchcampaign.ExchangeDay
	for _, d := range exchange {
		if d.Date >= "2021-01-04" && d.Date <= "2021-01-05" && !d.OpenAt.IsZero() {
			days = append(days, d)
		}
	}
	c := Contract{ID: "synthetic-put", Symbol: "AAPL", Kind: "put", Strike: 9000, Expiry: "2021-02-05"}
	source := scenarioTestSource{c: c}
	for _, scenario := range []Scenario{Primary, ExecutionFive, ExecutionNext, ExtraHalfSpread, DailyITM, PinExercise, PinNoExercise, AvailabilityNext} {
		t.Run(string(scenario), func(t *testing.T) {
			var records []Record
			a, err := NewModelAccount(banks, sessions, func(r Record) error { records = append(records, r); return nil }, scenario.AccountModel())
			if err != nil {
				t.Fatal(err)
			}
			result, err := Run(a, source, days, func(Diagnostic) error { return nil }, scenario)
			if err != nil || result.Status != "complete" || result.Scenario != scenario {
				t.Fatalf("%+v %v", result, err)
			}
			var sale *Effect
			for _, r := range records {
				if r.Effect.Action == "sell_put" {
					sale = new(r.Effect)
				}
			}
			if sale == nil {
				t.Fatal("synthetic entry absent")
			}
			wantAt, wantPrice := at("2021-01-04 15:30"), int64(100)
			if scenario == ExecutionFive {
				wantAt = at("2021-01-04 15:34")
			}
			if scenario == ExecutionNext {
				wantAt, wantPrice = at("2021-01-05 15:30"), 200
			}
			if scenario == ExtraHalfSpread {
				wantPrice = 99
			}
			if !sale.At.Equal(wantAt) || sale.Price != wantPrice {
				t.Fatalf("stale/wrong execution: %+v", sale)
			}
			if scenario == DailyITM && a.View().Positions["AAPL"].Shares != 100 {
				t.Fatal("daily early assignment absent")
			}
		})
	}
	t.Run("delayed_censor_and_policy_change", func(t *testing.T) {
		a, _ := NewAccount(banks, sessions, func(Record) error { return nil })
		result, err := Run(a, source, days[:1], func(Diagnostic) error { return nil }, ExecutionNext)
		if err != nil || len(result.Pending) != 1 || len(result.State.Positions) != 0 || !result.Pending[0].At.Equal(at("2021-01-05 15:30")) {
			t.Fatalf("pending attempt was backdated or lost: %+v %v", result, err)
		}
		a, _ = NewAccount(banks, sessions, func(Record) error { return nil })
		changed := source
		changed.changed = true
		result, err = Run(a, changed, days, func(Diagnostic) error { return nil }, ExecutionNext)
		if err != nil || len(result.State.Positions) != 0 || result.Counts["unfilled:opening_policy_changed_before_delayed_attempt"] != 1 {
			t.Fatalf("stale policy executed: %+v %v", result, err)
		}
		e := Effect{Action: "close_short", Symbol: "AAPL", Contract: c, At: at("2021-01-05 15:30")}
		logged := false
		err = executeScenario(a, source, Observation{At: e.At}, e, func(_ time.Time, _, stage, reason string, _ *Contract, _ string) error {
			logged = stage == "unfilled" && reason == "contract_resolved_before_delayed_attempt"
			return nil
		}, ExecutionNext)
		if err != nil || !logged {
			t.Fatal("resolved delayed close failed instead of remaining unfilled", err)
		}
		a, _ = NewAccount(banks, sessions, func(Record) error { return nil })
		absent := source
		absent.missing = true
		result, err = Run(a, absent, days, func(Diagnostic) error { return nil }, ExecutionNext)
		if err != nil || len(result.State.Positions) != 0 || result.Counts["unfilled:missing_execution_book"] != 1 {
			t.Fatalf("missing later side reused a prior quote: %+v %v", result, err)
		}
	})
	t.Run("model_binding_and_pin_path", func(t *testing.T) {
		writes := 0
		a, _ := NewModelAccount(banks, sessions, func(Record) error { writes++; return nil }, AvailabilityNext.AccountModel())
		if _, err := RunPrimary(a, source, days, func(Diagnostic) error { return nil }); err == nil || writes != 0 {
			t.Fatal("primary path silently inherited slower availability")
		}
		var expiryDays []researchcampaign.ExchangeDay
		for _, d := range exchange {
			if d.Date >= "2021-02-05" && d.Date <= "2021-02-08" && !d.OpenAt.IsZero() {
				expiryDays = append(expiryDays, d)
			}
		}
		for _, scenario := range []Scenario{Primary, PinNoExercise, PinExercise} {
			a, _ = NewAccount(banks, sessions, func(Record) error { return nil })
			e := Effect{Ref: "seeded-short", Action: "sell_put", Symbol: "AAPL", Contract: c,
				At: at("2021-01-04 15:30"), SignalAt: at("2021-01-04 15:29"), Profile: Profile{Mode: "conservative", Exit: "hold"}, Price: 100, SourceSHA: strings.Repeat("a", 64)}
			if err := a.Apply(e); err != nil {
				t.Fatal(err)
			}
			pin := source
			pin.ref = 89_950_000
			result, err := Run(a, pin, expiryDays, func(Diagnostic) error { return nil }, scenario)
			if err != nil {
				t.Fatal(err)
			}
			wantShares := 100
			if scenario == PinNoExercise {
				wantShares = 0
			}
			if result.State.Positions["AAPL"].Shares != wantShares {
				t.Fatalf("%s pin did not alter delivery: %+v", scenario, result)
			}
		}
	})
	t.Run("exact_pin_daily_and_cost_boundaries", func(t *testing.T) {
		call := Contract{Kind: "call", Strike: 9000}
		for _, row := range []struct {
			s     Scenario
			price int64
			want  string
		}{
			{Primary, 90_000_100, "expired"}, {DailyITM, 90_000_100, "called_away"},
			{PinExercise, 89_950_000, "called_away"}, {PinNoExercise, 90_090_000, "expired"},
			{PinNoExercise, 90_100_000, "called_away"},
		} {
			got, err := row.s.resolution(call, row.price)
			if err != nil || got != row.want {
				t.Fatalf("%s %d: %s %v", row.s, row.price, got, err)
			}
		}
		if price, err := ExtraHalfSpread.policyPrice(Book{Bid: 100, Ask: 101}, true); err != nil || price != 102 {
			t.Fatal("adverse cent rounding differs", price, err)
		}
		if _, err := ExtraHalfSpread.policyPrice(Book{Bid: 101, Ask: 100}, true); err == nil {
			t.Fatal("crossed half-spread invented")
		}
		if Scenario("primary+execution_5m").Validate() == nil {
			t.Fatal("combined sensitivity admitted")
		}
	})
	t.Run("notice_and_dividend_availability_delay", func(t *testing.T) {
		a, _ := NewModelAccount(banks, sessions, func(Record) error { return nil }, AvailabilityNext.AccountModel())
		put := c
		put.Expiry = "2021-01-08"
		e := Effect{Ref: "put", Action: "sell_put", Symbol: "AAPL", Contract: put,
			At: at("2021-01-04 15:30"), SignalAt: at("2021-01-04 15:29"), Profile: Profile{Mode: "conservative", Exit: "hold"}, Price: 100, SourceSHA: strings.Repeat("a", 64)}
		if err := a.Apply(e); err != nil {
			t.Fatal(err)
		}
		e.Ref, e.Action, e.At, e.Price = "lapse", "resolve", at("2021-01-08 16:00"), 0
		e.Outcome, e.NoticeAt = "expired", at("2021-01-11 09:35")
		if err := a.Apply(e); err != nil {
			t.Fatal(err)
		}
		locks := a.View().Locks
		if !locks[len(locks)-1].Until.Equal(at("2021-01-12 09:35")) {
			t.Fatal("lapse notice availability not delayed with its clock")
		}
		var journal []Record
		a, _ = NewModelAccount(banks, sessions, func(r Record) error { journal = append(journal, r); return nil }, AvailabilityNext.AccountModel())
		e = Effect{Ref: "stock", Action: "buy_stock", Symbol: "AAPL", At: at("2021-01-04 15:30"),
			SignalAt: at("2021-01-04 15:29"), Profile: Profile{Mode: "aggressive", Exit: "hold"}, Price: 10000, SourceSHA: strings.Repeat("a", 64)}
		if err := a.Apply(e); err != nil {
			t.Fatal(err)
		}
		e.Ref, e.Action, e.At, e.Price = "div", "dividend", at("2021-01-08 09:30"), 2050
		e.DividendID, e.PayAt, e.AvailableAt = "div", at("2021-01-11 09:30"), at("2021-01-11 09:30")
		if err := a.Apply(e); err != nil {
			t.Fatal(err)
		}
		if !a.View().Dividends["div"].AvailableAt.Equal(at("2021-01-12 09:30")) || !journal[len(journal)-1].Effect.AvailableAt.Equal(at("2021-01-12 09:30")) {
			t.Fatal("modeled dividend delay lost from state or journal")
		}
		e.Ref, e.Action, e.At = "paid", "pay_dividend", e.PayAt
		if err := a.Apply(e); err != nil {
			t.Fatal(err)
		}
		locks = a.View().Locks
		if !locks[len(locks)-1].Until.Equal(at("2021-01-12 09:30")) || !a.View().Dividends["div"].Paid {
			t.Fatal("paid dividend became spendable before its delayed availability")
		}
	})
}
