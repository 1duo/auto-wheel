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

// Reads run at most three at once; only this collector changes the journal.
// Each result is saved as it arrives, preserving partial capture across a crash.
func captureFixed(ctx context.Context, control *wheel.FixedController, st *wheel.State, b fixedMarketReader, clock func() time.Time) error {
	batch := st.Fixed.Batch
	remaining := batch.ExecutionAt.Sub(clock())
	if remaining <= 0 {
		return control.Recover(clock())
	}
	ctx, cancel := context.WithTimeout(ctx, remaining)
	defer cancel()
	tasks := make(chan wheel.FixedSignal, len(batch.Signals))
	type result struct {
		symbol      string
		observation robinhood.FixedMarketObservation
		err         error
	}
	results := make(chan result, len(batch.Signals))
	for _, signal := range batch.Signals {
		if signal.Phase == "unobserved" {
			tasks <- signal
		}
	}
	close(tasks)
	var group sync.WaitGroup
	for range min(3, len(batch.Signals)) {
		group.Go(func() {
			for signal := range tasks {
				observation, err := b.ObserveFixedPolicy(ctx, "conservative", signal.Kind, signal.Symbol, clock)
				results <- result{signal.Symbol, observation, err}
			}
		})
	}
	go func() { group.Wait(); close(results) }()
	for result := range results {
		at := clock()
		if !at.Before(batch.ExecutionAt) {
			continue // Recovery abstains omitted inputs; never record a late price.
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
	if !at.Before(batch.ExecutionAt) {
		return control.Recover(at)
	}
	return control.Seal(at)
}

func fixedPreflight(ctx context.Context, config wheel.Config, control *wheel.FixedController, st *wheel.State, b *robinhood.Broker, s wheel.Snapshot) liveReadiness {
	r := liveReadiness{At: time.Now(), Policy: wheel.FixedConservativePolicy, Scope: "integration_only; orders require fresh market data",
		OrderCoveragePolicy: "exclusive_standard", CoverageWarning: wheel.StandardOrderWarning, Unavailable: map[string]string{}}
	day := r.At.In(wheel.Eastern).Format(time.DateOnly)
	roster := config.FixedPolicy.Roster
	if !wheel.CalendarKnown(r.At) || roster.KnownAt.After(r.At) || day < roster.From || day > roster.Through || len(st.Intents) != 0 {
		r.Reason = "fixed readiness lacks a current reviewed calendar, roster or resolved account"
		return r
	}
	symbols := slices.Sorted(maps.Keys(st.Positions))
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
		r.VerifiedSymbols = []string{symbol}
		return r
	}
	r.Reason = "no fixed-policy opening passed identity, deliverable, fresh account funding and fee compatibility review"
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
