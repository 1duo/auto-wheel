package researchrd1

import (
	"errors"
	"time"

	"autowheel/internal/researchcampaign"
)

const CashZeroInterest = "cash_zero_interest"

// CashPacket admits only the zero-transaction control's observation schedule.
// It has no prices, policy, fee rates or settlement declarations to reuse for
// wheel transactions. External dispatch must bind the exact scoped admission.
type CashPacket struct {
	Schema          string                         `json:"schema"`
	Control         string                         `json:"control"`
	ProtocolSHA     string                         `json:"protocol_sha256"`
	AdmissionStatus string                         `json:"admission_status"`
	AdmissionSHA    string                         `json:"admission_receipt_sha256"`
	Unqualified     []string                       `json:"unqualified_inputs"`
	CalendarSHA     string                         `json:"calendar_source_sha256"`
	First           string                         `json:"first_session"`
	Last            string                         `json:"last_session"`
	Calendar        []researchcampaign.ExchangeDay `json:"exchange_days"`
}

func DecodeCashPacket(raw []byte) (CashPacket, error) {
	return decodePacket[CashPacket](raw)
}

// RunCash cannot emit a transaction. Its private account never escapes this
// function; only Mark is called, so there is no clearing or customer-tariff
// dependency and no invented zero rate for a hypothetical future trade.
func RunCash(p CashPacket, journal func(Record) error) (ReplayResult, error) {
	result := ReplayResult{Scenario: Primary, Control: CashZeroInterest, Status: "incomplete", Counts: map[string]int{}}
	if journal == nil || p.Schema != "AutoWheelRD1CashControlPacketV1" || p.Control != CashZeroInterest || p.ProtocolSHA != ProtocolSHA256 || p.AdmissionStatus != "admitted_cash_control_under_RD1" || !sourceHash(p.AdmissionSHA) || !sourceHash(p.CalendarSHA) || len(p.Unqualified) != 0 || p.First != "2023-01-03" || p.Last != "2025-12-31" || len(p.Calendar) != 1096 || p.Calendar[0].Date != "2023-01-01" || p.Calendar[len(p.Calendar)-1].Date != "2025-12-31" {
		return result, errors.New("RD1 cash control requires its exact admitted full 2023-2025 schedule")
	}
	sessions, err := researchcampaign.NewSessionCalendar(p.Calendar)
	if err != nil {
		return result, err
	}
	a := &Account{state: flatSeedState(), sessions: sessions, journal: journal}
	finish := func(err error) (ReplayResult, error) {
		result.State = a.View()
		if err != nil {
			result.Reason = err.Error()
		} else {
			result.Status, result.Reason = "complete", "zero-transaction zero-interest cash control; independent verification pending"
			result.NetProfit = new(int64(0))
		}
		return result, err
	}
	first, last := "", ""
	for _, day := range p.Calendar {
		if day.OpenAt.IsZero() {
			continue
		}
		if first == "" {
			first = day.Date
		}
		last = day.Date
		for _, sample := range []struct {
			label   string
			minutes time.Duration
		}{{"signal", 31}, {"execution", 30}, {"NAV", 1}} {
			clock := day.CloseAt.Add(-sample.minutes * time.Minute)
			nav, err := a.Mark(sample.label+"-"+day.Date, clock, p.CalendarSHA, map[string]Book{}, map[string]Book{})
			if err != nil {
				return finish(err)
			}
			if sample.label == "NAV" {
				result.LastNAV, result.LastNAVAt = new(nav), clock
				result.SessionsMarked++
			}
		}
	}
	if first != p.First || last != p.Last || result.LastNAV == nil {
		return finish(errors.New("RD1 cash control omits a declared boundary session"))
	}
	return finish(nil)
}
