package moomoo

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"autowheel/internal/wheel"
)

type ExecutionReadiness struct {
	At     time.Time `json:"at"`
	Ready  bool      `json:"ready"`
	Reason string    `json:"reason,omitempty"`
}

// ExecutionView is an immutable copy for the native operator. Observation
// balances and external holdings are never promoted into this execution view.
type ExecutionView struct {
	Broker    *wheel.Snapshot
	Positions map[string]wheel.Position
	Intents   map[string]wheel.Intent
}

const maxObservationsPerCycle = 4

// readinessWindow is the default refresh interval plus the 25-second cycle and
// 20-second observation budgets.
const readinessWindow = 75 * time.Second

var ErrAwaitingBrokerReconciliation = errors.New("acknowledged Moomoo opening awaits broker fill/position reconciliation")

// Execution serializes the five-slot controller/journal. Market reads can be
// supplied by a qualified broker source; no model participates in this loop.
type Execution struct {
	Broker     *Broker
	mu         sync.Mutex
	readyMu    sync.Mutex
	readiness  ExecutionReadiness
	view       ExecutionView
	state      wheel.State
	snapshot   *wheel.Snapshot
	controller *wheel.FixedController
	scanCursor int
	// scanWarning is reported beside readiness for two minutes; it never
	// blocks trading.
	scanWarning   string
	scanWarningAt time.Time
}

func (e *Execution) View() ExecutionView {
	e.readyMu.Lock()
	defer e.readyMu.Unlock()
	return e.view
}

func (e *Execution) Readiness() ExecutionReadiness {
	e.readyMu.Lock()
	defer e.readyMu.Unlock()
	r := e.readiness
	// Readiness spans the refresh interval so status does not flap between
	// cycles. Every order still requires its own fresh snapshot, quotes and
	// review in the final guard.
	window := readinessWindow
	if e.Broker != nil && e.Broker.Config.RefreshSeconds > 0 {
		window = time.Duration(e.Broker.Config.RefreshSeconds)*time.Second + 45*time.Second
	}
	if age := time.Now().UTC().Sub(r.At); r.Ready && (r.At.IsZero() || age < 0 || age > window) {
		r.Ready, r.Reason = false, "Moomoo execution readiness is stale"
	}
	if r.Ready && time.Since(e.scanWarningAt) <= 2*time.Minute {
		r.Reason = e.scanWarning
	}
	return r
}

func (e *Execution) CheckReady() error {
	r := e.Readiness()
	if !r.Ready {
		return errors.New("Moomoo native activation requires fresh qualified execution readiness")
	}
	return nil
}

func (e *Execution) publish(err error) {
	e.readyMu.Lock()
	defer e.readyMu.Unlock()
	e.readiness = ExecutionReadiness{At: time.Now().UTC(), Ready: err == nil}
	e.view = ExecutionView{Positions: maps.Clone(e.state.Positions), Intents: maps.Clone(e.state.Intents)}
	if e.snapshot != nil {
		copy := *e.snapshot
		copy.Shares, copy.Options = slices.Clone(copy.Shares), slices.Clone(copy.Options)
		copy.Orders, copy.ExternalSymbols = slices.Clone(copy.Orders), slices.Clone(copy.ExternalSymbols)
		e.view.Broker = &copy
	}
	if err != nil {
		e.readiness.Reason = err.Error()
	}
}

func (e *Execution) prepare(ctx context.Context) (wheel.Snapshot, error) {
	b := e.Broker
	if b == nil || b.Store == nil {
		return wheel.Snapshot{}, errors.New("Moomoo locked execution store is unavailable")
	}
	state, err := b.Store.JournalState()
	if err != nil {
		return wheel.Snapshot{}, err
	}
	e.state = state
	if b.configured() != nil {
		return wheel.Snapshot{}, errors.New("Moomoo broker-source qualification and its account-bound financial policy are required")
	}
	if !wheel.CalendarKnown(b.now()) || b.Writer == nil || b.Writer.CheckAccess(ctx) != nil {
		return wheel.Snapshot{}, errors.New("Moomoo trading permission or reviewed market calendar is missing")
	}
	if state.Account == "" {
		state.Account = b.Config.AccountID
	}
	if state.Account != b.Config.AccountID {
		return wheel.Snapshot{}, errors.New("Moomoo engine journal account differs")
	}
	snapshot, err := b.Snapshot(ctx)
	if err != nil {
		return wheel.Snapshot{}, err
	}
	// Snapshot recovery can drop never-dispatched intents. Continue from the
	// durable journal, never the copy read before that recovery.
	if state, err = b.Store.JournalState(); err != nil {
		return wheel.Snapshot{}, err
	}
	if state.Account == "" {
		state.Account = b.Config.AccountID
	}
	if state.Account != b.Config.AccountID {
		return wheel.Snapshot{}, errors.New("Moomoo engine journal account differs")
	}
	e.state = state
	e.snapshot = &snapshot
	if e.controller == nil {
		if err := b.Store.Save(e.state); err != nil {
			return wheel.Snapshot{}, err
		}
		e.controller, err = wheel.NewFixedController(b.Config.Strategy.FixedPolicy.Spec, &e.state, b.Store)
		if err != nil {
			return wheel.Snapshot{}, err
		}
		if err := e.controller.BindNative(b.Config.Strategy, snapshot, b.now()); err != nil {
			e.controller = nil
			return wheel.Snapshot{}, err
		}
	}
	if err := e.controller.ReconcileNative(b.Config.Strategy, snapshot, b.now()); err != nil {
		return wheel.Snapshot{}, err
	}
	if err := e.controller.CheckPendingOrders(snapshot); err != nil {
		known := len(e.state.Intents) > 0
		for _, intent := range e.state.Intents {
			if intent.OrderID == "" {
				known = false
			}
		}
		if known {
			return wheel.Snapshot{}, ErrAwaitingBrokerReconciliation
		}
		return wheel.Snapshot{}, err
	}
	return snapshot, nil
}

// Prepare reconciles only. It cannot capture, claim, submit or enable activation.
func (e *Execution) Prepare(ctx context.Context) error {
	// Activation can wait behind a running cycle. Give up with the request's
	// deadline, so an operator halt never queues behind it, and never publish
	// that cancellation as broker unreadiness.
	for !e.mu.TryLock() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	defer e.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := e.prepare(ctx)
	if err != nil && ctx.Err() != nil {
		return err // The request ended; that is not a broker fact.
	}
	e.publish(err)
	return err
}

func (e *Execution) Cycle(ctx context.Context) (cycleErr error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	defer func() { e.publish(cycleErr) }()
	snapshot, err := e.prepare(ctx)
	e.publish(err)
	if err != nil {
		return err
	}
	b := e.Broker
	if b.Activation == nil || !b.Activation.Enabled() {
		e.setScanWarning("") // Scan results describe live trading only.
		return nil
	}
	// Existing owned targets have priority. At most one successful dispatch
	// per account cycle, with all transport serialized by native activation.
	for _, symbol := range e.controller.PendingRestingTakeProfit() {
		err := e.controller.ExecuteRestingTakeProfit(ctx, b.Config.Strategy, b, symbol, b.now)
		if err == nil || !errors.Is(err, wheel.ErrRestingTPDeferred) {
			return err
		}
	}
	allowed, err := e.controller.BeginASAPScan(b.Config.Strategy, snapshot, b.now())
	if err != nil || !allowed {
		return err
	}
	// Moomoo allows about ten chain reads per 30 seconds and each observation
	// uses two. Read at most four symbols per cycle, rotating the start so
	// every roster entry is eventually observed.
	symbols := observationWindow(e.controller.ASAPSymbols(b.Config.Strategy, snapshot, b.now()), e.scanCursor)
	e.scanCursor++
	e.setScanWarning("")
	failed, firstErr := 0, error(nil)
	for _, symbol := range symbols {
		kind := "put"
		if e.state.Positions[symbol].OwnedShares == 100 {
			kind = "call"
		}
		input, err := b.Source.Observe(ctx, kind, symbol, b.now)
		if err != nil {
			failed, firstErr = failed+1, cmp.Or(firstErr, err)
			continue // No claim was created; the next roster entry may be read.
		}
		err = e.controller.ExecuteASAP(ctx, b.Config.Strategy, b, input, b.now)
		if err == nil || !errors.Is(err, wheel.ErrASAPDeferred) {
			return err
		}
	}
	// Failed observations are not account facts, so readiness stays true; a
	// scan where every read failed is reported so it cannot pass silently.
	if len(symbols) > 0 && failed == len(symbols) {
		e.setScanWarning("every roster observation failed this scan: " + firstErr.Error())
		slog.Warn("Moomoo roster observations all failed", "reason", firstErr.Error())
	}
	return nil
}

func (e *Execution) setScanWarning(warning string) {
	e.readyMu.Lock()
	defer e.readyMu.Unlock()
	e.scanWarning, e.scanWarningAt = warning, time.Now()
}

// observationWindow returns at most maxObservationsPerCycle symbols, starting
// at a rotating offset so a long roster cannot starve its later entries.
func observationWindow(symbols []string, cursor int) []string {
	if len(symbols) <= maxObservationsPerCycle {
		return symbols
	}
	start := cursor % len(symbols)
	return append(slices.Clone(symbols[start:]), symbols[:start]...)[:maxObservationsPerCycle]
}
