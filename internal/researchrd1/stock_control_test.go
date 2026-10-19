package researchrd1

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"autowheel/internal/researchcampaign"
	"autowheel/internal/wheel"
)

func stockTime(value string) time.Time {
	clock, err := time.ParseInLocation("2006-01-02 15:04", value, wheel.Eastern)
	if err != nil {
		panic("invalid synthetic control clock")
	}
	return clock
}

// Synthetic mechanics only: no historical prices, admission or account case.
func stockControlFixture() StockPacket {
	p := StockPacket{CashPacket: CashPacket{Schema: "AutoWheelRD1StockControlPacketV1", Control: StockBuyHold, ProtocolSHA: ProtocolSHA256, AdmissionStatus: "admitted_stock_control_under_RD1", AdmissionSHA: strings.Repeat("b", 64), CalendarSHA: strings.Repeat("a", 64), First: "2023-01-03", Last: "2025-12-31"}, ClearingSHA: strings.Repeat("a", 64), SelectionSHA: strings.Repeat("a", 64), Cutoff: "2022-12-31", Symbols: []string{"BIG", "AAPL", "MSFT", "AMD"}}
	p.PurchaseFee = StockPurchaseFee{Date: p.First, KnownAt: stockTime("2022-12-30 09:30"), Fee: new(int64(65)), SourceSHA: strings.Repeat("a", 64)}
	for _, symbol := range p.Symbols {
		p.Identities = append(p.Identities, StockIdentity{Symbol: symbol, Kind: "US_common_stock", From: stockTime("2022-01-01 00:00"), Through: stockTime("2025-12-31 23:59"), KnownAt: stockTime("2022-01-01 00:00"), SourceSHA: strings.Repeat("a", 64)})
	}
	for d := time.Date(2023, 1, 1, 0, 0, 0, 0, wheel.Eastern); d.Year() <= 2025; d = d.AddDate(0, 0, 1) {
		day := researchcampaign.ExchangeDay{Date: d.Format(time.DateOnly)}
		clearing := researchcampaign.ClearingDay{Date: day.Date}
		if d.Weekday() != time.Saturday && d.Weekday() != time.Sunday && day.Date != "2023-01-02" {
			day.OpenAt, day.CloseAt = d.Add(9*time.Hour+30*time.Minute), d.Add(16*time.Hour)
			clearing.Open, clearing.AvailableAt = true, day.OpenAt
			for _, minutes := range []time.Duration{31, 30, 1} {
				p.Observations = append(p.Observations, StockObservation{At: day.CloseAt.Add(-minutes * time.Minute), SourceSHA: strings.Repeat("a", 64), Stocks: map[string]Book{"BIG": {Bid: 30000, Ask: 30001}, "AAPL": {Bid: 10000, Ask: 10001}, "MSFT": {Bid: 10000, Ask: 10001}, "AMD": {Bid: 10000, Ask: 10001}}})
			}
		}
		p.Calendar, p.Clearing = append(p.Calendar, day), append(p.Clearing, clearing)
	}
	p.Dividends = []ScheduledDividend{{ID: "synthetic-cash", Symbol: "AAPL", ExAt: stockTime("2023-01-05 09:30"), KnownAt: stockTime("2022-12-30 09:30"), Amount: 2050, PayAt: stockTime("2023-01-06 09:30"), AvailableAt: stockTime("2023-01-06 09:30"), SourceSHA: strings.Repeat("c", 64)}}
	return p
}

func TestStockControlAllocationDividendsAndRetainedNames(t *testing.T) {
	p := stockControlFixture()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	p, err = DecodeStockPacket(raw)
	if err != nil {
		t.Fatal("stock-only wire format failed to decode", err)
	}
	var journal []Record
	result, err := RunStock(p, func(r Record) error { journal = append(journal, r); return nil }, func(Diagnostic) error { return nil })
	if err != nil || result.Status != "complete" || result.Control != StockBuyHold || result.NetProfit == nil || *result.NetProfit != 1555 || result.State.Fees != 195 || len(result.State.Positions) != 3 || len(result.State.Sales) != 0 || result.Counts["modeled_fill:buy_stock"] != 3 || result.Counts["risk_rejection:initial_stock_capacity"] != 1 {
		t.Fatalf("stock control changed its original allocation, fees or dividends: %v %+v", err, result)
	}
	buys, paid := 0, 0
	for _, r := range journal {
		if r.Effect.Action == "buy_stock" {
			buys++
			if r.Effect.At.In(wheel.Eastern).Format(time.DateOnly) != p.First || r.Effect.Contract.ID != "" {
				t.Fatal("control reentered or required a fabricated option")
			}
		}
		if r.Effect.Action == "pay_dividend" {
			paid++
			if r.Effect.SourceSHA != p.Dividends[0].SourceSHA {
				t.Fatal("dividend payment lost its original source binding")
			}
		}
	}
	if buys != 3 || paid != 1 || len(journal) != 3*result.SessionsMarked+5 {
		t.Fatal("unexpected control transaction or mark count")
	}
}

func TestStockControlUnknownInputsCannotManufactureCashOrReplaceRanks(t *testing.T) {
	for _, failure := range []string{"fee", "initial_book", "held_mark", "action", "wheel_scope"} {
		t.Run(failure, func(t *testing.T) {
			p := stockControlFixture()
			switch failure {
			case "fee":
				p.PurchaseFee.Fee = nil
			case "initial_book":
				delete(p.Observations[0].Stocks, "BIG")
			case "held_mark":
				delete(p.Observations[5].Stocks, "AAPL")
			case "action":
				p.Unsupported = []UnsupportedAction{{Symbol: "AAPL", At: stockTime("2023-01-09 12:00"), Reason: "unqualified structural action", SourceSHA: strings.Repeat("a", 64)}}
			case "wheel_scope":
				p.Schema, p.AdmissionStatus = "AutoWheelRD1ReplayPacketV1", "admitted_under_RD1"
			}
			result, err := RunStock(p, func(Record) error { return nil }, func(Diagnostic) error { return nil })
			if err == nil || result.Status != "incomplete" || result.NetProfit != nil {
				t.Fatal("unknown control input created a complete cash/stock result")
			}
			if failure == "initial_book" && len(result.State.Positions) != 0 {
				t.Fatal("unknown first rank was replaced with an easier name")
			}
		})
	}
}

func TestDividendPaymentUsesTermSourceAndRejectsUnboundReceivable(t *testing.T) {
	d := ScheduledDividend{ID: "div", Symbol: "AAPL", Amount: 2050, ExAt: stockTime("2021-02-05 09:30"), PayAt: stockTime("2021-02-11 09:30"), SourceSHA: strings.Repeat("c", 64)}
	source := &PacketSource{packet: Packet{AdmissionSHA: strings.Repeat("b", 64), Dividends: []ScheduledDividend{d}}}
	state := flatSeedState()
	state.At = stockTime("2021-02-10 15:59")
	state.Dividends[d.ID] = Dividend{Symbol: d.Symbol, Amount: d.Amount, PayAt: d.PayAt, AvailableAt: d.PayAt}
	day := researchcampaign.ExchangeDay{Date: "2021-02-11", OpenAt: d.PayAt}
	events, err := source.BeforeSession(day, state)
	if err != nil || len(events) != 1 || events[0].SourceSHA != d.SourceSHA || events[0].Action != "pay_dividend" {
		t.Fatal("later payment used an admission hash instead of the original dividend source", err)
	}
	for _, failure := range []string{"missing", "changed"} {
		if failure == "missing" {
			source.packet.Dividends = nil
		} else {
			d.Amount++
			source.packet.Dividends = []ScheduledDividend{d}
		}
		if _, err := source.BeforeSession(day, state); err == nil {
			t.Fatal("unbound or changed dividend payment was accepted")
		}
	}
}
