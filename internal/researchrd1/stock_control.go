package researchrd1

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"autowheel/internal/researchcampaign"
)

const StockBuyHold = "stock_buy_hold"

// StockIdentity is a dated common-share identity/action interval. It conveys no
// option contract, multiplier or deliverable qualification.
type StockIdentity struct {
	Symbol    string    `json:"symbol"`
	Kind      string    `json:"security_kind"`
	From      time.Time `json:"from"`
	Through   time.Time `json:"through"`
	KnownAt   time.Time `json:"known_at"`
	SourceSHA string    `json:"source_sha256"`
}

type StockObservation struct {
	At        time.Time       `json:"at"`
	SourceSHA string          `json:"source_packet_sha256"`
	Stocks    map[string]Book `json:"stock_books"`
}

type StockPurchaseFee struct {
	Date      string    `json:"date"`
	KnownAt   time.Time `json:"known_at"`
	Fee       *int64    `json:"fee_cents"`
	SourceSHA string    `json:"source_sha256"`
}

// StockPacket is scoped to one initial allocation followed by ownership, marks
// and declared dividends. It contains no option menu, quote or fee fields.
type StockPacket struct {
	CashPacket
	ClearingSHA  string                         `json:"clearing_source_sha256"`
	SelectionSHA string                         `json:"stock_selection_sha256"`
	Cutoff       string                         `json:"stock_selection_cutoff"`
	Symbols      []string                       `json:"ranked_symbols"`
	Clearing     []researchcampaign.ClearingDay `json:"clearing_days"`
	Observations []StockObservation             `json:"observations"`
	PurchaseFee  StockPurchaseFee               `json:"initial_purchase_fee"`
	Identities   []StockIdentity                `json:"identity_intervals"`
	Dividends    []ScheduledDividend            `json:"dividends"`
	Unsupported  []UnsupportedAction            `json:"unsupported_actions"`
}

func DecodeStockPacket(raw []byte) (StockPacket, error) {
	return decodePacket[StockPacket](raw)
}

// RunStock never sells, writes an option or replaces a retained name. The
// aggressive/hold profile on its private Account is solely the existing funded
// 100-share ownership encoding; Control, not that profile, labels this account.
// Missing/invalid initial inputs stop the control, so a later easier name cannot
// silently replace a name whose affordability or identity is unknown.
func RunStock(p StockPacket, journal func(Record) error, diagnostic func(Diagnostic) error) (ReplayResult, error) {
	result := ReplayResult{Scenario: Primary, Control: StockBuyHold, Status: "incomplete", Counts: map[string]int{}}
	if journal == nil || diagnostic == nil || p.Schema != "AutoWheelRD1StockControlPacketV1" || p.Control != StockBuyHold || p.ProtocolSHA != ProtocolSHA256 || p.AdmissionStatus != "admitted_stock_control_under_RD1" || !sourceHash(p.AdmissionSHA) || !sourceHash(p.CalendarSHA) || !sourceHash(p.ClearingSHA) || !sourceHash(p.SelectionSHA) || p.Cutoff != "2022-12-31" || len(p.Unqualified) != 0 || p.First != "2023-01-03" || p.Last != "2025-12-31" || len(p.Calendar) != 1096 || p.Calendar[0].Date != "2023-01-01" || p.Calendar[len(p.Calendar)-1].Date != p.Last || len(p.Symbols) > 10 || len(p.Observations) > 10000 || len(p.Identities) > 1000 || len(p.Dividends) > 2000 || len(p.Unsupported) > 1000 {
		return result, errors.New("RD1 stock control requires its exact independently admitted 2023-2025 envelope")
	}
	fee := p.PurchaseFee
	if fee.Date != p.First || fee.KnownAt.IsZero() || fee.Fee == nil || *fee.Fee < 0 || *fee.Fee > 10000 || !sourceHash(fee.SourceSHA) {
		return result, errors.New("RD1 initial stock purchase fee is unknown")
	}
	sessions, err := researchcampaign.NewSessionCalendar(p.Calendar)
	if err != nil {
		return result, err
	}
	clearing, err := researchcampaign.NewSettlementCalendar(p.Clearing)
	if err != nil {
		return result, err
	}
	if err := validateDividends(p.Dividends, sessions); err != nil {
		return result, err
	}
	seen := map[string]bool{}
	for _, symbol := range p.Symbols {
		if symbol == "" || len(symbol) > 5 || strings.Trim(symbol, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "" || seen[symbol] {
			return result, errors.New("RD1 stock control has an ambiguous original universe")
		}
		seen[symbol] = true
	}
	for _, term := range p.Identities {
		if !seen[term.Symbol] || term.Kind != "US_common_stock" || term.From.IsZero() || term.Through.Before(term.From) || term.KnownAt.IsZero() || !sourceHash(term.SourceSHA) {
			return result, errors.New("RD1 stock identity interval is unsupported")
		}
	}
	for _, action := range p.Unsupported {
		if !seen[action.Symbol] || action.At.IsZero() || action.Reason == "" || !sourceHash(action.SourceSHA) {
			return result, errors.New("RD1 stock action lacks exact reviewed identity")
		}
	}
	samples := map[time.Time]StockObservation{}
	for _, obs := range p.Observations {
		if obs.At.IsZero() || !sourceHash(obs.SourceSHA) || len(obs.Stocks) > 10 {
			return result, errors.New("RD1 stock observation is unbounded or unknown")
		}
		if _, duplicate := samples[obs.At.UTC()]; duplicate {
			return result, errors.New("RD1 duplicate stock observation")
		}
		samples[obs.At.UTC()] = obs
	}
	a, err := NewAccount(clearing, sessions, journal)
	if err != nil {
		return result, err
	}
	finish := func(err error) (ReplayResult, error) {
		result.State = a.View()
		if err != nil {
			result.Reason = err.Error()
		} else {
			result.Status, result.Reason = "complete", "original-universe first-three-funded stock control; independent verification pending"
			result.NetProfit = new(*result.LastNAV - result.State.Seed)
		}
		return result, err
	}
	identity := func(symbol string, clock time.Time) error {
		for _, action := range p.Unsupported {
			if action.Symbol == symbol && !clock.Before(action.At) {
				return fmt.Errorf("RD1 stock control unsupported action: %s", action.Reason)
			}
		}
		matches := 0
		for _, term := range p.Identities {
			if term.Symbol == symbol && !clock.Before(term.From) && !clock.After(term.Through) && !clock.Before(term.KnownAt) {
				matches++
			}
		}
		if matches != 1 {
			return errors.New("RD1 stock identity/action interval is unknown or ambiguous")
		}
		return nil
	}
	review := func(clock time.Time) error {
		for symbol := range a.View().Positions {
			if err := identity(symbol, clock); err != nil {
				return err
			}
		}
		return nil
	}
	log := func(clock time.Time, symbol, stage, reason, source string) error {
		if err := diagnostic(Diagnostic{At: clock, Symbol: symbol, Stage: stage, Reason: reason, SourceSHA: source}); err != nil {
			return err
		}
		result.Counts[stage+":"+reason]++
		return nil
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
		if err := review(day.OpenAt); err != nil {
			return finish(err)
		}
		events, err := dividendEffects(p.Dividends, day, a.View())
		if err != nil {
			return finish(err)
		}
		for _, e := range events {
			if err := a.Apply(e); err != nil {
				return finish(err)
			}
		}
		signalAt := day.CloseAt.Add(-31 * time.Minute)
		for _, sample := range []struct {
			label   string
			minutes time.Duration
		}{{"signal", 31}, {"execution", 30}, {"NAV", 1}} {
			clock := day.CloseAt.Add(-sample.minutes * time.Minute)
			obs, known := samples[clock.UTC()]
			if !known {
				return finish(errors.New("RD1 stock control sampled bucket absent"))
			}
			if err := review(clock); err != nil {
				return finish(err)
			}
			books := map[string]Book{}
			for symbol := range a.View().Positions {
				if book, known := obs.Stocks[symbol]; known {
					books[symbol] = book
				}
			}
			nav, err := a.Mark(sample.label+"-"+day.Date, clock, obs.SourceSHA, books, map[string]Book{})
			if err != nil {
				return finish(err)
			}
			if day.Date == p.First && sample.label == "execution" {
				for _, symbol := range p.Symbols {
					state := a.View()
					if len(state.Positions) == 3 {
						break
					}
					before, known := samples[signalAt.UTC()]
					if !known || fee.KnownAt.After(signalAt) {
						return finish(errors.New("RD1 initial allocation signal/fee is not known"))
					}
					if err := identity(symbol, signalAt); err != nil {
						return finish(err)
					}
					if err := identity(symbol, clock); err != nil {
						return finish(err)
					}
					prior, signalKnown := before.Stocks[symbol]
					book, executionKnown := obs.Stocks[symbol]
					if !signalKnown || !executionKnown || !prior.EntryValid() || !book.EntryValid() {
						return finish(errors.New("RD1 initial stock book unknown/invalid; original rank cannot be replaced"))
					}
					for _, halt := range day.Halts {
						if !clock.Before(halt.From) && clock.Before(halt.Through) {
							return finish(errors.New("RD1 initial stock allocation halted; no later automatic entry"))
						}
					}
					settled, reserved, exposure, err := state.Resources(clock)
					if err != nil {
						return finish(err)
					}
					cost := 100*book.Ask + *fee.Fee
					if !Capacity(settled, reserved, exposure, 0, cost, cost, len(state.Positions), true) {
						if err := log(clock, symbol, "risk_rejection", "initial_stock_capacity", obs.SourceSHA); err != nil {
							return finish(err)
						}
						continue
					}
					e := Effect{Ref: "stock-control-" + day.Date + "-" + symbol, Action: "buy_stock", Symbol: symbol, At: clock, SignalAt: signalAt, Profile: Profile{Mode: "aggressive", Exit: "hold"}, Price: book.Ask, Fee: *fee.Fee, SourceSHA: obs.SourceSHA}
					if err := a.Apply(e); err != nil {
						return finish(err)
					}
					if err := log(clock, symbol, "modeled_fill", "buy_stock", obs.SourceSHA); err != nil {
						return finish(err)
					}
				}
			}
			if sample.label == "NAV" {
				result.LastNAV, result.LastNAVAt = new(nav), clock
				result.SessionsMarked++
			}
		}
	}
	if first != p.First || last != p.Last || result.LastNAV == nil {
		return finish(errors.New("RD1 stock control omits a declared boundary session"))
	}
	return finish(nil)
}
