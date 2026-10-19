package researchrd1

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"autowheel/internal/researchcampaign"
	"autowheel/internal/wheel"
)

// Synthetic schedule checks mechanics only; it is not a source-admitted case.
func TestRD1CashControlBoundary(t *testing.T) {
	p := CashPacket{Schema: "AutoWheelRD1CashControlPacketV1", Control: CashZeroInterest,
		ProtocolSHA: ProtocolSHA256, AdmissionStatus: "admitted_cash_control_under_RD1", AdmissionSHA: strings.Repeat("a", 64),
		CalendarSHA: strings.Repeat("b", 64), Unqualified: []string{}, First: "2023-01-03", Last: "2025-12-31"}
	for day := time.Date(2023, 1, 1, 0, 0, 0, 0, wheel.Eastern); day.Year() <= 2025; day = day.AddDate(0, 0, 1) {
		ds := day.Format(time.DateOnly)
		row := researchcampaign.ExchangeDay{Date: ds}
		if day.Weekday() != time.Saturday && day.Weekday() != time.Sunday && ds != "2023-01-02" && ds != "2024-01-01" && ds != "2025-01-01" {
			closeHour := 16
			if ds == "2024-07-03" {
				closeHour = 13
			}
			row.OpenAt = time.Date(day.Year(), day.Month(), day.Day(), 9, 30, 0, 0, wheel.Eastern)
			row.CloseAt = time.Date(day.Year(), day.Month(), day.Day(), closeHour, 0, 0, 0, wheel.Eastern)
		}
		p.Calendar = append(p.Calendar, row)
	}
	marks, early := 0, 0
	result, err := RunCash(p, func(r Record) error {
		marks++
		if r.Effect.Action != "mark" || r.CashAfter != 8_404_717 || r.FeesAfter != 0 || r.CashDelta != 0 || r.ReceivableDelta != 0 || r.Mark.NAV != 8_404_717 || len(r.Mark.Stocks) != 0 || len(r.Mark.Options) != 0 {
			t.Fatalf("cash control emitted a transaction, price or changed capital: %+v", r)
		}
		if r.Effect.At.Format(time.DateOnly) == "2024-07-03" {
			if r.Effect.At.Hour() != 12 {
				t.Fatal("early-close cash observation used a normal-day clock")
			}
			early++
		}
		return nil
	})
	if err != nil || result.Status != "complete" || result.Control != CashZeroInterest || result.NetProfit == nil || *result.NetProfit != 0 || len(result.State.Positions) != 0 || len(result.State.Sales) != 0 || marks != result.SessionsMarked*3 || early != 3 {
		t.Fatalf("cash-only invariants or schedule failed: %+v %v", result, err)
	}
	failed := errors.New("cash journal unavailable")
	attempts := 0
	partial, err := RunCash(p, func(Record) error {
		attempts++
		if attempts == 3 {
			return failed
		}
		return nil
	})
	if !errors.Is(err, failed) || partial.Status != "incomplete" || partial.NetProfit != nil || partial.State.Sequence != 2 || partial.SessionsMarked != 0 {
		t.Fatalf("cash journal failure published a complete result: %+v %v", partial, err)
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeCashPacket(raw); err != nil {
		t.Fatal(err)
	}
	if _, err := DecodePacket(raw); err == nil {
		t.Fatal("cash packet accepted as a wheel packet")
	}
	for _, suffix := range []string{`,"control":"cash_zero_interest"}`, `,"tariffs":[]}`} {
		if _, err := DecodeCashPacket(append(append([]byte{}, raw[:len(raw)-1]...), []byte(suffix)...)); err == nil {
			t.Fatal("duplicate control or undeclared trading input accepted")
		}
	}
}
