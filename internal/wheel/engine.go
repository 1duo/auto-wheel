package wheel

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"time"
)

type Broker interface {
	Snapshot(context.Context) (Snapshot, error)
	Quotes(context.Context, string, string) ([]Quote, error)
	Review(context.Context, Candidate) error
	Submit(context.Context, Candidate, string) (string, error)
}

type Store interface{ Save(State) error }

// ErrNotSubmitted is returned only when no order transport was invoked. It must
// never classify a timeout, application error or malformed placement receipt.
var ErrNotSubmitted = errors.New("order transport was not invoked")

// Reconcile never interprets order-history absence as rejection. A pre-send
// crash and a lost response both retain their intent until the broker proves it.
func Reconcile(st *State, s Snapshot, now time.Time) error {
	if st.Account != s.Account {
		return errors.New("journal belongs to another account")
	}
	// Validate exposure before consuming any intent or changing ownership. An ID
	// match cannot prove a contract whose broker metadata changed or duplicated.
	seen := map[string]bool{}
	for _, op := range s.Options {
		p, in := st.Positions[op.Symbol], st.Intents[op.Symbol]
		valid := op.ID != "" && op.Quantity == 1 && !seen[op.Symbol] && op.Strike > 0 && op.Strike <= MaxMoney/100
		if _, err := time.Parse("2006-01-02", op.Expiry); err != nil {
			valid = false
		}
		switch {
		case op.ID == in.Candidate.ID && in.Candidate.ID != "":
			q := in.Candidate.Option
			valid = valid && op.Symbol == q.Symbol && op.Kind == q.Kind && op.Strike == q.Strike && op.Expiry == q.Expiry && (q.ChainID == "" || op.ChainID == q.ChainID)
		case op.ID == p.PutID && p.PutID != "":
			valid = valid && op.Kind == "put" && op.Strike == p.PutStrike && (p.PutExpiry == "" || op.Expiry == p.PutExpiry)
		case op.ID == p.CallID && p.CallID != "":
			valid = valid && op.Kind == "call" && p.OwnedShares == 100 && (p.CallExpiry == "" || op.Expiry == p.CallExpiry)
		default:
			valid = false
		}
		if !valid {
			return fmt.Errorf("%s option exposure identity or quantity unproven; operator review required", op.Symbol)
		}
		seen[op.Symbol] = true
	}
	for symbol, in := range st.Intents {
		var matches []Order
		for _, o := range s.Orders {
			if (in.OrderID != "" && o.ID == in.OrderID) || (in.OrderID == "" && o.Ref == in.Ref) {
				matches = append(matches, o)
			}
		}
		if len(matches) != 1 {
			return errors.New("submission ref_id missing or duplicated; operator investigation required")
		}
		o := matches[0]
		side, effect, _, _ := in.Candidate.OrderTerms()
		if o.Symbol != symbol || o.OptionID != in.Candidate.ID || o.Side != side || o.Effect != effect || o.Quantity != 1 || o.Filled < 0 || o.Filled > 1 {
			return errors.New("broker order differs from durable intent")
		}
		if in.OrderID != "" && (o.ID != in.OrderID || o.Ref != "" && o.Ref != in.Ref) {
			return errors.New("broker order ID differs from durable receipt")
		}
		if !o.Terminal() {
			continue
		}
		if s.OrderCoveragePolicy == "exclusive_standard" && (o.ID == "" || len(st.CompletedOrders) >= 1024) {
			return errors.New("completed order ownership cannot be retained; freeze")
		}
		if o.Filled == 1 && in.Candidate.Closing {
			p := st.Positions[symbol]
			if p.PutID != in.Candidate.ID || p.OwnedShares != 0 || p.CallID != "" {
				return errors.New("close fill ownership mismatch")
			}
			for _, h := range s.Shares {
				if h.Symbol == symbol && h.Quantity != 0 {
					return errors.New("close fill overlaps assignment inventory; freeze")
				}
			}
			stillOpen := false
			for _, op := range s.Options {
				if op.Symbol == symbol {
					stillOpen = true
				}
			}
			// Wait for broker position reporting; never free the slot early.
			if stillOpen {
				continue
			}
			delete(st.Positions, symbol)
		} else if o.Filled == 1 {
			p := st.Positions[symbol]
			if in.Candidate.Kind == "put" {
				p.PutID = in.Candidate.ID
				p.PutExpiry = in.Candidate.Expiry
				p.PutStrike = in.Candidate.Strike
				p.PutOpenedAt = in.At
				p.Provenance = "autowheel put fill " + in.Ref
				p.MissingSince = time.Time{}
			}
			if in.Candidate.Kind == "call" {
				p.CallID = in.Candidate.ID
				p.CallExpiry = in.Candidate.Expiry
			}
			st.Positions[symbol] = p
		} else if o.State == "filled" {
			return errors.New("filled order has no filled quantity")
		}
		if s.OrderCoveragePolicy == "exclusive_standard" {
			if st.CompletedOrders == nil {
				st.CompletedOrders = map[string]OrderRecord{}
			}
			st.CompletedOrders[o.ID] = OrderRecord{Ref: in.Ref, Symbol: symbol, OptionID: in.Candidate.ID, At: in.At, Closing: in.Candidate.Closing}
		}
		delete(st.Intents, symbol)
	}
	for _, o := range s.Orders {
		if o.Terminal() || o.OptionID == "" {
			continue
		}
		in, ok := st.Intents[o.Symbol]
		if !ok || (in.OrderID != "" && o.ID != in.OrderID) || (in.OrderID == "" && o.Ref != in.Ref) {
			return fmt.Errorf("%s untracked working option order; operator review required", o.Symbol)
		}
	}
	for symbol, p := range st.Positions {
		if slices.Contains(s.ExternalSymbols, symbol) {
			return fmt.Errorf("%s managed inventory became fractional or unsupported; freeze", symbol)
		}
		var held Shares
		for _, h := range s.Shares {
			if h.Symbol == symbol {
				held = h
			}
		}
		var open bool
		for _, o := range s.Options {
			if o.Symbol == symbol {
				open = true
			}
		}
		for _, o := range s.Orders {
			if o.Symbol == symbol && !o.Terminal() {
				open = true
			}
		}
		if p.PutID != "" {
			if open {
				p.MissingSince = time.Time{}
				st.Positions[symbol] = p
				continue
			}
			if p.MissingSince.IsZero() {
				p.MissingSince = now
				st.Positions[symbol] = p
				continue
			}
			// Give expiration/assignment reporting and settlement at least 24 hours.
			if now.Sub(p.MissingSince) < 24*time.Hour {
				continue
			}
			if held.Quantity == 0 {
				elapsed, err := expiryDayElapsed(p.PutExpiry, now)
				if err != nil {
					return fmt.Errorf("%s put expiry unproven; operator investigation required", symbol)
				}
				if !elapsed {
					continue
				}
				delete(st.Positions, symbol)
				continue
			}
			// In an exclusively managed account, a confirmed own put fill,
			// disappearance of that short, exactly 100 settled shares at its
			// strike, and no intervening equity order support assignment adoption.
			// This remains a disclosed inference, not a fabricated assignment receipt.
			manual := p.PutOpenedAt.IsZero()
			for _, o := range s.Orders {
				if o.Symbol == symbol && o.OptionID == "" && (o.LastTransactionAt.IsZero() || !o.CreatedAt.Before(p.PutOpenedAt) || !o.LastTransactionAt.Before(p.PutOpenedAt)) {
					manual = true
				}
			}
			if held.Quantity != 100 || held.Available != 100 || held.Cost != p.PutStrike || manual {
				return fmt.Errorf("%s assignment facts ambiguous; adoption receipt required", symbol)
			}
			st.Positions[symbol] = Position{OwnedShares: 100, Cost: p.PutStrike, PutOpenedAt: p.PutOpenedAt, InventoryAt: now, Provenance: p.Provenance + "; assignment inferred from settled broker inventory and no equity orders"}
		}
		if p.OwnedShares > 0 {
			if p.InventoryAt.IsZero() {
				return fmt.Errorf("%s inventory verification timestamp missing; re-adopt", symbol)
			}
			for _, o := range s.Orders {
				if o.Symbol == symbol && o.OptionID == "" && (o.LastTransactionAt.IsZero() || !o.CreatedAt.Before(p.InventoryAt) || !o.LastTransactionAt.Before(p.InventoryAt)) {
					return fmt.Errorf("%s equity activity timing or ownership unproven; freeze", symbol)
				}
			}
			if held.Quantity > 0 && (held.Quantity != 100 || held.Cost != p.Cost) {
				return fmt.Errorf("%s share ownership or cost drift; freeze", symbol)
			}
			if p.CallID != "" && !open {
				if p.MissingSince.IsZero() {
					p.MissingSince = now
					st.Positions[symbol] = p
					continue
				}
				if now.Sub(p.MissingSince) < 24*time.Hour {
					continue
				}
				if held.Quantity == 0 {
					delete(st.Positions, symbol)
					continue
				}
				if held.Quantity != 100 || held.Available != 100 {
					return fmt.Errorf("%s call settlement ambiguous; freeze", symbol)
				}
				elapsed, err := expiryDayElapsed(p.CallExpiry, now)
				if err != nil {
					return fmt.Errorf("%s call expiry unproven; operator investigation required", symbol)
				}
				if !elapsed {
					continue
				}
				p.CallID = ""
				p.CallExpiry = ""
				p.MissingSince = time.Time{}
				st.Positions[symbol] = p
			} else if held.Quantity != 100 {
				return fmt.Errorf("%s shares disappeared without settled own call; freeze", symbol)
			} else if open && !p.MissingSince.IsZero() {
				p.MissingSince = time.Time{}
				st.Positions[symbol] = p
			}
		}
	}
	return nil
}

// Absence alone cannot mean expiration before the contract's expiry day ends.
// Early assignment/call-away use the separate inventory transition above.
// Legacy journals without an expiry must not guess it from today's date.
func expiryDayElapsed(expiry string, now time.Time) (bool, error) {
	expires, err := time.ParseInLocation("2006-01-02", expiry, Eastern)
	if err != nil {
		return false, err
	}
	return !now.Before(expires.AddDate(0, 0, 1)), nil
}

// Execute is serialized across the account. Re-read holdings/cash and obtain
// broker review before every submission. Only scans run in parallel.
func Execute(ctx context.Context, c Config, st *State, store Store, b Broker, a Candidate, now func() time.Time) error {
	s, err := b.Snapshot(ctx)
	if err != nil {
		return err
	}
	if err = ReconcileForConfig(c, st, s, now()); err != nil {
		return err
	}
	if err = store.Save(*st); err != nil {
		return err
	}
	quotes, err := b.Quotes(ctx, a.Symbol, a.Kind)
	if err != nil {
		return err
	}
	cost := st.Positions[a.Symbol].Cost
	a, ok := SelectFunded(c, a.Symbol, a.Kind, quotes, cost, s.AvailableCash, now())
	if !ok {
		return errors.New("candidate no longer eligible")
	}
	plan, err := Plan(c, *st, s, []Candidate{a}, now())
	if err != nil {
		return err
	}
	if len(plan) != 1 || !TradingWindow(now()) {
		return errors.New("candidate blocked by pre-submit checks")
	}
	if err = b.Review(ctx, a); err != nil {
		return err
	}
	if c.OrderCoveragePolicy == "exclusive_standard" {
		// Review is not a reservation. Re-read all supported account facts after
		// review and before writing the intent; no partial read can authorize it.
		s, err = b.Snapshot(ctx)
		if err != nil {
			return err
		}
		if err = ReconcileForConfig(c, st, s, now()); err != nil {
			return err
		}
		if err = store.Save(*st); err != nil {
			return err
		}
		plan, err = Plan(c, *st, s, []Candidate{a}, now())
		if err != nil || len(plan) != 1 {
			return errors.Join(errors.New("account changed during broker review"), err)
		}
	}
	if !fresh(a.At, now(), c.MaxQuoteAgeSeconds) || !fresh(a.UnderlyingAt, now(), c.MaxQuoteAgeSeconds) || !fresh(s.At, now(), c.MaxQuoteAgeSeconds) || !TradingWindow(now()) {
		return errors.New("pre-submit data expired during review")
	}
	return persistAndSubmit(ctx, c, st, store, b, a, s, now)
}

func persistAndSubmit(ctx context.Context, c Config, st *State, store Store, b interface {
	Submit(context.Context, Candidate, string) (string, error)
}, a Candidate, s Snapshot, now func() time.Time) error {
	ref, err := newRef()
	if err != nil {
		return err
	}
	st.Intents[a.Symbol] = Intent{Ref: ref, Candidate: a, At: now()}
	if err = store.Save(*st); err != nil {
		return err
	}
	if !fresh(a.At, now(), c.MaxQuoteAgeSeconds) || !fresh(a.UnderlyingAt, now(), c.MaxQuoteAgeSeconds) || !fresh(s.At, now(), c.MaxQuoteAgeSeconds) || !TradingWindow(now()) || ctx.Err() != nil {
		delete(st.Intents, a.Symbol)
		return errors.Join(ErrNotSubmitted, store.Save(*st))
	}
	id, err := b.Submit(ctx, a, ref)
	if err != nil {
		if errors.Is(err, ErrNotSubmitted) {
			delete(st.Intents, a.Symbol)
			return errors.Join(err, store.Save(*st))
		}
		return errors.New("submission outcome unknown; intent retained, no retry")
	}
	in := st.Intents[a.Symbol]
	in.OrderID = id
	st.Intents[a.Symbol] = in
	if err = store.Save(*st); err != nil {
		return err
	}
	return nil
}

func newRef() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

// Adopt records user-verified assignment provenance without placing an order.
func Adopt(c Config, st *State, s Snapshot, symbol, receipt string, cost Cents) error {
	if !slices.Contains(c.Symbols, symbol) || len(receipt) < 8 || cost <= 0 || cost > MaxMoney/100 || s.Account != c.Account || st.Account != c.Account {
		return errors.New("invalid assignment adoption")
	}
	if len(st.Intents) > 0 {
		return errors.New("cannot adopt while an order is unresolved")
	}
	for _, o := range s.Options {
		if o.Symbol == symbol {
			return errors.New("cannot adopt with open options")
		}
	}
	for _, o := range s.Orders {
		if o.Symbol == symbol && !o.Terminal() {
			return errors.New("cannot adopt with pending orders")
		}
	}
	for _, h := range s.Shares {
		if h.Symbol == symbol && h.Quantity == 100 && h.Available == 100 && h.Cost == cost {
			st.Positions[symbol] = Position{OwnedShares: 100, Cost: cost, InventoryAt: time.Now(), Provenance: receipt}
			return nil
		}
	}
	return errors.New("assignment adoption requires exactly 100 available broker-confirmed shares")
}
