package quotehistory

import (
	"fmt"
	"time"
)

// IntervalSummary is bounded offline evidence of raw book continuity over
// (From, Through] for one raw OSI symbol, including actual state carried at
// From. It is NOT a qualified full normalizer, NOT a coverage claim, and
// makes no P&L, fill, or source/transport claim. Continuous
// source/transport and standard/tradable attributes remain separate caller
// qualification.
//
// SeedPresent reports whether Snapshot at From held an observation for the
// symbol. When false, no seed is invented: BooksValid and QuotesFresh are
// false, minima are zero, and Start/End quote times are zero unless an
// interval quote creates End state. Trade-only intervals never create
// End state on their own.
//
// BooksValid is true only when a seed was present, the seed book was valid,
// and every selected-symbol A/C/M/R transition in (From, Through] was
// valid. Transient dips recovered before the endpoint still latch false.
// Minima are conservative: any absent/invalid state forces zero; otherwise
// they are the minimum over the seed and every valid interval quote.
//
// QuotesFresh is true only when BooksValid holds for the whole interval and
// every quiet segment between valid quotes (including From to first quote
// and last quote to Through) satisfies age <= maxAge. Exact boundary is
// allowed; crossing maxAge before recovery latches false even when the
// endpoint is fresh. T events never refresh.
//
// StartAt/StartEventAt preserve the seed capture/OPRA consolidator times without
// retiming to From. EndAt/EndEventAt preserve the latest interval quote
// capture/consolidator times (or the seed when quiet). EndObservation is the actual
// end state with Status recomputed for Through; EndPresent is false when no
// seed and no interval A/C/M/R created state.
type IntervalSummary struct {
	Symbol         string
	From           time.Time
	Through        time.Time
	SeedPresent    bool
	BooksValid     bool
	QuotesFresh    bool
	MinBidNano     int64
	MinBidSize     int64
	BookEvents     int
	TradeEvents    int
	StartAt        time.Time
	StartEventAt   time.Time
	EndAt          time.Time
	EndEventAt     time.Time
	EndObservation Observation
	EndPresent     bool
}

// SummarizeInterval folds validated events for symbol over (from, through],
// including actual state carried at from. It calls Snapshot once at from
// and then makes a single pass in file order; it never re-snapshots per
// event. Events with TsRecv after through are excluded from state and
// counts but were already validated fail-closed. Same-capture transitions
// are processed in file order. T never modifies or refreshes book state.
func SummarizeInterval(events []Event, symbol string, from, through time.Time, maxAge time.Duration) (IntervalSummary, error) {
	var out IntervalSummary
	if err := validateSymbol(symbol); err != nil {
		return IntervalSummary{}, fmt.Errorf("%w: bad interval symbol: %v", ErrInvalidRecord, err)
	}
	if from.IsZero() || through.IsZero() {
		return IntervalSummary{}, fmt.Errorf("%w: zero interval bound", ErrInvalidRecord)
	}
	if !through.After(from) {
		return IntervalSummary{}, fmt.Errorf("%w: non-positive interval", ErrInvalidRecord)
	}
	if maxAge < 0 {
		return IntervalSummary{}, fmt.Errorf("%w: negative maxAge", ErrInvalidRecord)
	}
	if len(events) > MaxEvents {
		return IntervalSummary{}, ErrTooManyRecords
	}
	if err := validateEvents(events); err != nil {
		return IntervalSummary{}, err
	}

	seedMap, err := Snapshot(events, from, maxAge)
	if err != nil {
		return IntervalSummary{}, err
	}

	out.Symbol = symbol
	out.From = from
	out.Through = through

	booksValid := false
	quotesFresh := false
	var minBidNano int64
	var minBidSize int64
	var lastValid time.Time

	if seed, ok := seedMap[symbol]; ok {
		out.SeedPresent = true
		out.StartAt = seed.At
		out.StartEventAt = seed.EventAt
		out.EndAt = seed.At
		out.EndEventAt = seed.EventAt
		out.EndObservation = seed
		out.EndPresent = true
		if seed.Status == StatusFresh || seed.Status == StatusStale {
			booksValid = true
			quotesFresh = seed.Status == StatusFresh
			minBidNano = seed.BidPxNano
			minBidSize = seed.BidSz
			lastValid = seed.At
		}
	}

	for _, ev := range events {
		if ev.Symbol != symbol {
			continue
		}
		if !ev.TsRecv.After(from) {
			continue
		}
		if ev.TsRecv.After(through) {
			break
		}
		switch ev.Action {
		case ActionTrade:
			out.TradeEvents++
			if out.EndPresent {
				obs := out.EndObservation
				obs.TradeAt = ev.TsRecv
				obs.TradeEventAt = ev.TsEvent
				out.EndObservation = obs
			}
		case ActionAdd, ActionChange, ActionModify:
			out.BookEvents++
			reason, valid := classifyBook(ev.Book)
			var tradeAt, tradeEventAt time.Time
			if out.EndPresent {
				tradeAt = out.EndObservation.TradeAt
				tradeEventAt = out.EndObservation.TradeEventAt
			}
			if !valid {
				booksValid = false
				quotesFresh = false
				out.EndAt = ev.TsRecv
				out.EndEventAt = ev.TsEvent
				out.EndObservation = Observation{
					Symbol:        symbol,
					InstrumentID:  ev.InstrumentID,
					PublisherID:   ev.PublisherID,
					At:            ev.TsRecv,
					EventAt:       ev.TsEvent,
					TradeAt:       tradeAt,
					TradeEventAt:  tradeEventAt,
					Status:        StatusInvalid,
					InvalidReason: reason,
				}
				out.EndPresent = true
				continue
			}
			if quotesFresh {
				if ev.TsRecv.Sub(lastValid) > maxAge {
					quotesFresh = false
				}
			}
			if booksValid {
				minBidNano = min(minBidNano, ev.Book.BidPxNano)
				minBidSize = min(minBidSize, ev.Book.BidSz)
			}
			lastValid = ev.TsRecv
			out.EndAt = ev.TsRecv
			out.EndEventAt = ev.TsEvent
			out.EndObservation = Observation{
				Symbol:       symbol,
				InstrumentID: ev.InstrumentID,
				PublisherID:  ev.PublisherID,
				At:           ev.TsRecv,
				EventAt:      ev.TsEvent,
				TradeAt:      tradeAt,
				TradeEventAt: tradeEventAt,
				BidPxNano:    ev.Book.BidPxNano,
				AskPxNano:    ev.Book.AskPxNano,
				BidSz:        ev.Book.BidSz,
				AskSz:        ev.Book.AskSz,
				Status:       StatusFresh,
			}
			out.EndPresent = true
		case ActionReset:
			out.BookEvents++
			var tradeAt, tradeEventAt time.Time
			if out.EndPresent {
				tradeAt = out.EndObservation.TradeAt
				tradeEventAt = out.EndObservation.TradeEventAt
			}
			booksValid = false
			quotesFresh = false
			out.EndAt = ev.TsRecv
			out.EndEventAt = ev.TsEvent
			out.EndObservation = Observation{
				Symbol:        symbol,
				InstrumentID:  ev.InstrumentID,
				PublisherID:   ev.PublisherID,
				At:            ev.TsRecv,
				EventAt:       ev.TsEvent,
				TradeAt:       tradeAt,
				TradeEventAt:  tradeEventAt,
				Status:        StatusInvalid,
				InvalidReason: ReasonReset,
			}
			out.EndPresent = true
		default:
			return IntervalSummary{}, fmt.Errorf("%w: unknown action %q", ErrInvalidRecord, string(ev.Action))
		}
	}

	if quotesFresh {
		if through.Sub(lastValid) > maxAge {
			quotesFresh = false
		}
	}

	out.BooksValid = booksValid
	out.QuotesFresh = quotesFresh
	if booksValid {
		out.MinBidNano = minBidNano
		out.MinBidSize = minBidSize
	}

	if out.EndPresent && out.EndObservation.Status != StatusInvalid {
		if through.Sub(out.EndAt) > maxAge {
			obs := out.EndObservation
			obs.Status = StatusStale
			out.EndObservation = obs
		} else {
			obs := out.EndObservation
			obs.Status = StatusFresh
			out.EndObservation = obs
		}
	}

	return out, nil
}
