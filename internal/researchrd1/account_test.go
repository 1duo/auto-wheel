package researchrd1

import (
	"errors"
	"strings"
	"testing"
	"time"

	"autowheel/internal/researchcampaign"
)

func TestRD1Accounting(t *testing.T) {
	// Fixed synthetic transitions check the money kernel. They are not RD1
	// historical replay cases, source admission or strategy performance.
	zone, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	at := func(value string) time.Time {
		t.Helper()
		v, err := time.ParseInLocation("2006-01-02 15:04", value, zone)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	var sessions []researchcampaign.ExchangeDay
	var clearing []researchcampaign.ClearingDay
	for d := at("2021-01-01 00:00"); !d.After(at("2021-04-09 00:00")); d = d.AddDate(0, 0, 1) {
		open := d.Weekday() != time.Saturday && d.Weekday() != time.Sunday && !strings.Contains("2021-01-01 2021-01-18 2021-02-15 2021-04-02", d.Format(time.DateOnly))
		exchange := researchcampaign.ExchangeDay{Date: d.Format(time.DateOnly)}
		bank := researchcampaign.ClearingDay{Date: exchange.Date, Open: open}
		if open {
			exchange.OpenAt, exchange.CloseAt = d.Add(9*time.Hour+30*time.Minute), d.Add(16*time.Hour)
			bank.AvailableAt = exchange.OpenAt
		}
		sessions, clearing = append(sessions, exchange), append(clearing, bank)
	}
	sc, err := researchcampaign.NewSessionCalendar(sessions)
	if err != nil {
		t.Fatal(err)
	}
	cc, err := researchcampaign.NewSettlementCalendar(clearing)
	if err != nil {
		t.Fatal(err)
	}
	source := strings.Repeat("a", 64)
	profile := Profile{Mode: "balanced", Exit: "tp50"}
	contract := Contract{ID: "synthetic-put", Symbol: "AAPL", Expiry: "2021-01-29", Kind: "put", Strike: 10000}
	effect := func(ref, action, when string, c Contract, price, fee int64) Effect {
		return Effect{Ref: ref, Action: action, Symbol: "AAPL", At: at(when), SignalAt: at(when).Add(-time.Minute), Profile: profile, Contract: c, Price: price, Fee: fee, SourceSHA: source}
	}
	t.Run("exact_money_lifecycle_and_missing_mark", func(t *testing.T) {
		var records []Record
		a, err := NewAccount(cc, sc, func(r Record) error { records = append(records, r); return nil })
		if err != nil {
			t.Fatal(err)
		}
		apply := func(e Effect) {
			t.Helper()
			if err := a.Apply(e); err != nil {
				t.Fatalf("%s: %v", e.Ref, err)
			}
		}
		apply(effect("put1", "sell_put", "2021-01-04 15:30", contract, 250, 1))
		settled, reserved, _, err := a.View().Resources(at("2021-01-04 15:31"))
		if err != nil || settled != 8_404_716 || reserved != 1_000_000 {
			t.Fatalf("premium must remain unavailable and fee debited: %d %d %v", settled, reserved, err)
		}
		if err := a.Apply(effect("same-day", "close_short", "2021-01-04 15:34", contract, 100, 1)); err == nil {
			t.Fatal("second policy transaction on one session accepted")
		}
		sequence := a.View().Sequence
		if _, err := a.Mark("missing", at("2021-01-04 15:59"), source, nil, nil); err == nil || a.View().Sequence != sequence {
			t.Fatal("missing short mark erased liability or changed state")
		}
		nav, err := a.Mark("nav1", at("2021-01-04 15:59"), source, nil, map[string]Book{contract.ID: {Bid: 270, Ask: 260}})
		if err != nil || nav != 8_403_716 {
			t.Fatalf("positive crossed ask remains liability: %d %v", nav, err)
		}
		apply(effect("close1", "close_short", "2021-01-05 15:30", contract, 100, 1))
		if err := a.Apply(effect("bad-entry-day", "sell_put", "2021-01-06 15:30", contract, 200, 1)); err == nil {
			t.Fatal("nonweekly reentry accepted")
		}
		apply(effect("put2", "sell_put", "2021-01-11 15:30", contract, 200, 1))
		exercise := effect("assign", "resolve", "2021-01-29 16:00", contract, 0, 0)
		exercise.Outcome, exercise.NoticeAt = "assigned", at("2021-02-01 09:35")
		apply(exercise)
		if a.View().Positions["AAPL"].AvailableAt != at("2021-02-02 09:30") {
			t.Fatal("T+2 assignment share availability changed")
		}
		call := Contract{ID: "synthetic-call", Symbol: "AAPL", Expiry: "2021-03-05", Kind: "call", Strike: 10500}
		if err := a.Apply(effect("unsettled-call", "sell_call", "2021-02-01 15:30", call, 100, 1)); err == nil {
			t.Fatal("unsettled assignment shares used for call")
		}
		dividend := effect("div", "dividend", "2021-02-05 09:30", Contract{}, 2050, 0)
		dividend.DividendID, dividend.PayAt, dividend.AvailableAt = "synthetic-dividend", at("2021-02-11 09:30"), at("2021-02-11 09:30")
		apply(dividend)
		if receivable, _ := a.View().Receivables(); receivable != 2050 {
			t.Fatal("$0.205 per share must become exactly $20.50 for 100 shares")
		}
		apply(effect("call1", "sell_call", "2021-02-08 15:30", call, 100, 1))
		payment := effect("paid", "pay_dividend", "2021-02-11 09:30", Contract{}, 2050, 0)
		payment.DividendID = dividend.DividendID
		apply(payment)
		exercise = effect("call-away", "resolve", "2021-03-05 16:00", call, 0, 3)
		exercise.Outcome, exercise.NoticeAt = "called_away", at("2021-03-08 09:35")
		apply(exercise)
		s := a.View()
		if s.Cash != 8_501_760 || s.Fees != 7 || s.StockPnL != 49_997 || len(s.Positions) != 0 || len(s.Sales) != 3 {
			t.Fatalf("fee/dividend/lifecycle reconciliation differs: %+v", s)
		}
		settled, reserved, _, err = s.Resources(at("2021-03-08 15:30"))
		if err != nil || settled != 7_451_760 || reserved != 0 {
			t.Fatalf("call-away proceeds reused before T+2: %d %d %v", settled, reserved, err)
		}
		cash := int64(8_404_717)
		for i, r := range records {
			cash += r.CashDelta
			if cash != r.CashAfter || r.Sequence != i+1 {
				t.Fatal("independent journal cash sum differs")
			}
		}
	})
	t.Run("next_session_execution_and_slower_settlement", func(t *testing.T) {
		a, err := NewModelAccount(cc, sc, func(Record) error { return nil }, Model{SettlementExtraDays: 1})
		if err != nil {
			t.Fatal(err)
		}
		e := effect("delayed-buy", "buy_stock", "2021-01-05 15:30", Contract{}, 10000, 0)
		e.Profile, e.SignalAt = Profile{Mode: "aggressive", Exit: "hold"}, at("2021-01-04 15:29")
		if err := a.Apply(e); err != nil {
			t.Fatal(err)
		}
		if a.View().Positions["AAPL"].AvailableAt != at("2021-01-08 09:30") {
			t.Fatal("Tuesday execution must settle T+2 plus one business day on Friday")
		}
	})
	t.Run("aggressive_stock_and_uncertain_journal", func(t *testing.T) {
		fail := false
		a, err := NewAccount(cc, sc, func(Record) error {
			if fail {
				return errors.New("synthetic durable append failure")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		e := effect("buy", "buy_stock", "2021-01-04 15:30", Contract{}, 10001, 1)
		e.Profile = Profile{Mode: "aggressive", Exit: "hold"}
		if err := a.Apply(e); err != nil {
			t.Fatal(err)
		}
		call := Contract{ID: "aggressive-call", Symbol: "AAPL", Expiry: "2021-02-05", Kind: "call", Strike: 10300}
		e.Ref, e.Action, e.Contract, e.Price = "same-day-call", "sell_call", call, 100
		if err := a.Apply(e); err == nil {
			t.Fatal("same-day buy and CC accepted")
		}
		if nav, err := a.Mark("stock-nav", at("2021-01-04 15:59"), source, map[string]Book{"AAPL": {Bid: 10000, Ask: 10001}}, nil); err != nil || nav != 8_404_616 {
			t.Fatalf("stock debit/fee/NAV differs: %d %v", nav, err)
		}
		e.Ref, e.At, e.SignalAt = "covered-call", at("2021-01-11 15:30"), at("2021-01-11 15:29")
		if err := a.Apply(e); err != nil {
			t.Fatal(err)
		}
		before := a.View()
		fail = true
		if _, err := a.Mark("failed-journal", at("2021-01-11 15:59"), source, map[string]Book{"AAPL": {Bid: 10000, Ask: 10001}}, map[string]Book{call.ID: {Bid: 99, Ask: 100}}); err == nil || a.View().Sequence != before.Sequence {
			t.Fatal("journal failure committed mark")
		}
		fail = false
		if err := a.Apply(effect("poisoned", "close_short", "2021-01-12 15:30", call, 50, 1)); err == nil {
			t.Fatal("uncertain journal allowed more effects")
		}
	})
	t.Run("unknown_day_is_not_a_holiday", func(t *testing.T) {
		truncated, err := researchcampaign.NewSessionCalendar(sessions[4:]) // Starts Tuesday Jan 5.
		if err != nil {
			t.Fatal(err)
		}
		if _, err := truncated.FirstSessionOfWeek(at("2021-01-05 15:30")); err == nil {
			t.Fatal("missing Monday treated as explicit holiday")
		}
		if first, err := sc.FirstSessionOfWeek(at("2021-01-19 15:30")); err != nil || !first {
			t.Fatal("explicit Monday holiday did not admit Tuesday")
		}
	})
}
