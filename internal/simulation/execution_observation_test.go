package simulation

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// This invented path exercises timing/coverage checks, not historical results.
func executionReplay(t *testing.T) (*replay, time.Time) {
	t.Helper()
	var spec Spec
	if err := json.NewDecoder(AccountingFixture()).Decode(&spec); err != nil {
		t.Fatal(err)
	}
	r := &replay{report: Report{Spec: spec}}
	if err := r.init(); err != nil {
		t.Fatal(err)
	}
	at := r.decisions[0]
	f := fixtureFrame(at, "decision")
	f.CompleteSymbols = spec.Config.Symbols
	if err := r.step(f); err != nil {
		t.Fatal(err)
	}
	if r.pending == nil {
		t.Fatal("fixture did not submit")
	}
	return r, at
}

func coveredExecutionFrame(r *replay, at, quoteAt time.Time) Frame {
	f := fixtureFrame(at, "market")
	f.ExecutionCoverage = &ExecutionCoverage{
		OptionID: r.pending.Candidate.ID, From: r.pending.LastQuote, Through: at,
		SourceSHA256: strings.Repeat("a", 64), Complete: true, ValidBooks: true, QuotesFresh: true,
		MinBid: r.pending.Candidate.Bid, MinBidSize: 1,
	}
	for i := range f.Quotes {
		if f.Quotes[i].ID == r.pending.Candidate.ID {
			f.Quotes[i].At = quoteAt
			f.Quotes[i].ObservedAt = at
		}
	}
	return f
}

func TestExecutionQuietBookPreservesQuoteTime(t *testing.T) {
	r, at := executionReplay(t)
	r.report.Spec.Mode = "historical_quote_replay"
	for second := 1; second <= 6; second++ {
		f := coveredExecutionFrame(r, at.Add(time.Duration(second)*time.Second), at)
		if err := r.step(f); err != nil {
			t.Fatal(err)
		}
		if r.quotes["fixture-NVDA-put"].At != at {
			t.Fatal("re-observation retimed the actual quote")
		}
		if second < 6 && r.metrics.Fills != 0 {
			t.Fatal("filled before latency plus five-second dwell")
		}
	}
	if r.metrics.Fills != 1 {
		t.Fatal("complete quiet-book interval never filled")
	}
}

func TestExecutionRejectsUnverifiedObservation(t *testing.T) {
	for _, name := range []string{"missing", "incomplete", "wrong_start", "wrong_contract", "bad_hash", "missing_observed_at", "future_observed_at", "impossible_minimum"} {
		t.Run(name, func(t *testing.T) {
			r, at := executionReplay(t)
			r.report.Spec.Mode = "historical_quote_replay"
			f := coveredExecutionFrame(r, at.Add(time.Second), at)
			switch name {
			case "missing":
				f.ExecutionCoverage = nil
			case "incomplete":
				f.ExecutionCoverage.Complete = false
			case "wrong_start":
				f.ExecutionCoverage.From = at.Add(time.Nanosecond)
			case "wrong_contract":
				f.ExecutionCoverage.OptionID = "other-contract"
			case "bad_hash":
				f.ExecutionCoverage.SourceSHA256 = strings.Repeat("x", 64)
			case "impossible_minimum":
				f.ExecutionCoverage.MinBid = r.pending.Candidate.Bid + 1
			default:
				for i := range f.Quotes {
					if f.Quotes[i].ID == r.pending.Candidate.ID {
						f.Quotes[i].ObservedAt = time.Time{}
						if name == "future_observed_at" {
							f.Quotes[i].ObservedAt = f.At.Add(time.Nanosecond)
						}
					}
				}
			}
			if err := r.step(f); err == nil || r.metrics.Fills != 0 {
				t.Fatal("unverified interval accepted")
			}
		})
	}
}

func TestExecutionTransientLossResetsDwell(t *testing.T) {
	for _, loss := range []string{"price", "size", "invalid_book", "transient_stale_quote"} {
		t.Run(loss, func(t *testing.T) {
			r, at := executionReplay(t)
			for second := 1; second <= 9; second++ {
				f := coveredExecutionFrame(r, at.Add(time.Duration(second)*time.Second), at)
				if second == 3 {
					// Endpoint book is good, but a transient loss inside the
					// interval must not disappear between observation points.
					switch loss {
					case "price":
						f.ExecutionCoverage.MinBid--
					case "size":
						f.ExecutionCoverage.MinBidSize = 0
					case "invalid_book":
						f.ExecutionCoverage.ValidBooks = false
					case "transient_stale_quote":
						f.ExecutionCoverage.QuotesFresh = false
					}
				}
				if err := r.step(f); err != nil {
					t.Fatal(err)
				}
				if second < 9 && r.metrics.Fills != 0 {
					t.Fatal("transient loss did not restart dwell")
				}
			}
			if r.metrics.Fills != 1 {
				t.Fatal("fresh uninterrupted dwell never filled")
			}
		})
	}
}

func TestExecutionObservationCannotRefreshStaleQuote(t *testing.T) {
	r, at := executionReplay(t)
	for second := 1; second <= 35; second++ {
		f := coveredExecutionFrame(r, at.Add(time.Duration(second)*time.Second), at)
		if second < 27 {
			f.ExecutionCoverage.MinBidSize = 0
		}
		if err := r.step(f); err != nil {
			t.Fatal(err)
		}
	}
	if r.metrics.Fills != 0 || r.pending == nil {
		t.Fatal("new observations made a stale actual quote fill")
	}
}

func TestExecutionGFDRequiresFinalInterval(t *testing.T) {
	for _, complete := range []bool{false, true} {
		r, at := executionReplay(t)
		r.report.Spec.Mode = "historical_quote_replay"
		closeAt := sessionClose(at)
		// Isolate the terminal interval; this is not a complete replay stream.
		r.decisions, r.valuations = nil, nil
		r.decisionIndex, r.valuationIndex = 0, 0
		r.now, r.pending.LastQuote = closeAt.Add(-time.Second), closeAt.Add(-time.Second)
		f := coveredExecutionFrame(r, closeAt, closeAt.Add(-time.Second))
		if !complete {
			f.ExecutionCoverage = nil
		}
		err := r.step(f)
		if complete {
			if err != nil || r.pending != nil || r.metrics.ExpiredOrders != 1 {
				t.Fatalf("verified GFD expiry failed: %v", err)
			}
		} else if err == nil || r.pending == nil || r.metrics.ExpiredOrders != 0 {
			t.Fatal("unverified final interval became an assumed expiry")
		}
	}
}
