package main

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sync"
	"time"

	"autowheel/internal/robinhood"
	"autowheel/internal/wheel"
)

type fixedMarketReader interface {
	ObserveFixedPolicy(context.Context, string, string, string, func() time.Time) (robinhood.FixedMarketObservation, error)
}

type fixedCloseReader interface {
	CloseQuote(context.Context, wheel.State, string) (wheel.Candidate, error)
}

// Reads run at most three at once; only this collector changes the journal.
// Each result is saved as it arrives, preserving partial capture across a crash.
func captureFixed(ctx context.Context, control *wheel.FixedController, st *wheel.State, b fixedMarketReader, clock func() time.Time) error {
	return captureFixedSources(ctx, control, st, b, nil, true, false, clock)
}

func captureFixedCohorts(ctx context.Context, control *wheel.FixedController, st *wheel.State, b *robinhood.Broker, entry, tp bool, clock func() time.Time) error {
	return captureFixedSources(ctx, control, st, b, b, entry, tp, clock)
}

func captureFixedSources(ctx context.Context, control *wheel.FixedController, st *wheel.State, b fixedMarketReader, closer fixedCloseReader, entry, tp bool, clock func() time.Time) error {
	if !entry && !tp {
		return nil
	}
	execution := time.Time{}
	if entry {
		execution = st.Fixed.Batch.ExecutionAt
	}
	if tp {
		execution = st.Fixed.TakeProfit.ExecutionAt
	}
	recover := func() error {
		if entry {
			if err := control.Recover(clock()); err != nil {
				return err
			}
		}
		if tp {
			return control.RecoverTakeProfit(clock())
		}
		return nil
	}
	remaining := execution.Sub(clock())
	if remaining <= 0 {
		return recover()
	}
	ctx, cancel := context.WithTimeout(ctx, remaining)
	defer cancel()
	type task struct {
		symbol, kind string
		closing      bool
	}
	tasks := make(chan task, 16)
	type result struct {
		symbol      string
		closing     bool
		observation robinhood.FixedMarketObservation
		candidate   wheel.Candidate
		err         error
	}
	results := make(chan result, 16)
	if tp {
		for _, signal := range st.Fixed.TakeProfit.Signals {
			if signal.Phase == "unobserved" {
				tasks <- task{symbol: signal.Symbol, closing: true}
			}
		}
	}
	if entry {
		for _, signal := range st.Fixed.Batch.Signals {
			if signal.Phase == "unobserved" {
				tasks <- task{symbol: signal.Symbol, kind: signal.Kind}
			}
		}
	}
	count := len(tasks)
	close(tasks)
	var group sync.WaitGroup
	// Workers use an immutable ownership snapshot; only this collector saves.
	owned := *st
	for range min(3, count) {
		group.Go(func() {
			for signal := range tasks {
				r := result{symbol: signal.symbol, closing: signal.closing}
				if signal.closing {
					r.candidate, r.err = closer.CloseQuote(ctx, owned, signal.symbol)
				} else {
					r.observation, r.err = b.ObserveFixedPolicy(ctx, "conservative", signal.kind, signal.symbol, clock)
				}
				results <- r
			}
		})
	}
	go func() { group.Wait(); close(results) }()
	for result := range results {
		at := clock()
		if !at.Before(execution) {
			continue // Recovery abstains omitted inputs; never record a late price.
		}
		if result.closing {
			if err := control.RecordTakeProfit(result.symbol, result.candidate, result.err, at); err != nil {
				return err
			}
			continue
		}
		evidence, err := result.observation.SignalEvidence()
		if result.err != nil || err != nil {
			reason := "source_unavailable"
			if result.err == nil && result.observation.Choice == nil {
				reason = "no_target"
			}
			err = control.Abstain(result.symbol, reason, at)
		} else {
			err = control.Record(result.symbol, evidence, at)
		}
		if err != nil && !errors.Is(err, wheel.ErrFixedSignalAbstained) {
			return err
		}
	}
	at := clock()
	if !at.Before(execution) {
		return recover()
	}
	if tp {
		if err := control.RecoverTakeProfit(at); err != nil {
			return err
		}
	}
	if entry {
		return control.Seal(at)
	}
	return nil
}

func fixedPreflight(ctx context.Context, config wheel.Config, control *wheel.FixedController, st *wheel.State, b *robinhood.Broker, s wheel.Snapshot) liveReadiness {
	r := liveReadiness{At: time.Now(), Policy: config.FixedPolicy.Spec.ID, Scope: "integration_only; orders require fresh market data",
		OrderCoveragePolicy: "exclusive_standard", CoverageWarning: wheel.StandardOrderWarning, Unavailable: map[string]string{}}
	day := r.At.In(wheel.Eastern).Format(time.DateOnly)
	roster := config.FixedPolicy.Roster
	if !wheel.CalendarKnown(r.At) || roster.KnownAt.After(r.At) || day < roster.From || day > roster.Through || len(st.Intents) != 0 {
		r.Reason = "fixed readiness lacks a current reviewed calendar, roster or resolved account"
		return r
	}
	symbols := slices.Sorted(maps.Keys(st.Positions))
	if config.FixedPolicy.Spec.ID == wheel.FixedConservativeTP75Policy {
		for _, symbol := range symbols {
			p := st.Positions[symbol]
			if p.PutID == "" && p.CallID == "" {
				continue
			}
			a, err := b.PreviewClose(ctx, *st, symbol)
			if err == nil {
				s, err = b.Snapshot(ctx)
				if err == nil {
					err = control.PreviewTakeProfit(config, a, s, time.Now())
				}
				if err == nil {
					err = b.PreviewCheck(ctx, a)
				}
			}
			if err != nil {
				r.Ready = false
				r.Reason = "TP75 owned close compatibility: " + symbol + ": " + err.Error()
				return r
			}
			r.Ready, r.Preview = true, new(a)
			r.VerifiedSymbols = append(r.VerifiedSymbols, symbol)
		}
	}
	for _, symbol := range roster.Symbols {
		if !slices.Contains(symbols, symbol) {
			symbols = append(symbols, symbol)
		}
	}
	for _, symbol := range symbols {
		p := st.Positions[symbol]
		if p.PutID != "" || p.CallID != "" || !p.MissingSince.IsZero() {
			r.Unavailable[symbol] = "own lifecycle busy or settlement unresolved"
			continue
		}
		kind := "put"
		if p.OwnedShares == 100 {
			kind = "call"
		}
		observation, err := b.ObserveFixedPolicy(ctx, "conservative", kind, symbol, time.Now)
		if err != nil || observation.Choice == nil {
			r.Unavailable[symbol] = "fixed identity menu or target unavailable"
			continue
		}
		e, err := b.PreviewForFixed(ctx, *observation.Choice, time.Now)
		if err != nil {
			r.Unavailable[symbol] = err.Error()
			continue
		}
		// Scans and deliverable proof can outlive available cash or ownership.
		s, err = b.Snapshot(ctx)
		if err != nil {
			r.Reason = err.Error()
			return r
		}
		a, err := control.PreviewNative(config, e, s, time.Now())
		if err == nil {
			err = b.PreviewCheck(ctx, a)
		}
		if err != nil {
			r.Unavailable[symbol] = err.Error()
			continue
		}
		r.At, r.Ready, r.Preview = time.Now(), true, new(a)
		r.VerifiedSymbols = append(r.VerifiedSymbols, symbol)
		return r
	}
	if !r.Ready {
		r.Reason = "no fixed-policy opening or owned close passed account, identity and fee compatibility review"
	}
	r.At = time.Now()
	return r
}

// Poll normally for account reporting, but wake exactly for the two weekly
// clocks. During the bounded execution window reconcile one in-flight order at
// a time; no timer or wake-up can recreate a consumed signal.
func cycleDelay(config wheel.Config, now time.Time) time.Duration {
	delay := time.Duration(config.PollSeconds) * time.Second
	if config.FixedPolicy == nil {
		return delay
	}
	signal, execution, latest, known := wheel.FixedEntryTimes(now)
	if config.FixedPolicy.Spec.ID == wheel.FixedConservativeTP75Policy {
		signal, execution, latest, known = wheel.FixedCloseTimes(now)
	}
	if !known {
		return delay
	}
	if now.Before(signal) {
		return min(delay, signal.Sub(now))
	}
	if now.Before(execution) {
		return execution.Sub(now)
	}
	if !now.After(latest) {
		return min(delay, 15*time.Second)
	}
	return delay
}
