package wheel

import (
	"context"
	"errors"
	"time"
)

// OrderTerms keeps closing orders distinct throughout review, receipt and history.
func (a Candidate) OrderTerms() (side, effect, direction string, limit Cents) {
	if a.Closing {
		return "buy", "close", "debit", a.CloseLimit
	}
	return "sell", "open", "credit", a.Bid
}

type CloseBroker interface {
	Snapshot(context.Context) (Snapshot, error)
	CloseQuote(context.Context, State, string) (Candidate, error)
	Review(context.Context, Candidate) error
	Submit(context.Context, Candidate, string) (string, error)
}

// CheckClose allows only profitable reductions of an exactly owned short put.
// Existing collateral can exceed today's entry cap; no new exposure is created.
func CheckClose(c Config, st State, s Snapshot, a Candidate, now time.Time) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if err := st.Validate(); err != nil {
		return err
	}
	if err := CheckOrderScope(c, st, s); err != nil {
		return err
	}
	if c.OrderCoveragePolicy != "exclusive_standard" || c.CoordinatorSocket != "" || len(st.Intents) != 0 || len(st.CompletedOrders) >= 1024 {
		return errors.New("close requires exclusive ownership and no unresolved orders")
	}
	p := st.Positions[a.Symbol]
	if !a.Closing || a.Kind != "put" || a.Quantity != 1 || a.ID == "" || a.ChainID == "" || p.PutID != a.ID || p.PutExpiry != a.Expiry || p.PutStrike != a.Strike || p.OwnedShares != 0 || p.CallID != "" || !p.MissingSince.IsZero() {
		return errors.New("close contract differs from owned short put")
	}
	count := 0
	for _, op := range s.Options {
		if op.Symbol == a.Symbol {
			if op != a.Option {
				return errors.New("close broker exposure changed")
			}
			count++
		}
	}
	if count != 1 {
		return errors.New("close requires exactly one broker short contract")
	}
	for _, h := range s.Shares {
		if h.Symbol == a.Symbol && h.Quantity != 0 {
			return errors.New("close overlaps stock inventory")
		}
	}
	for _, o := range s.Orders {
		if !o.Terminal() {
			return errors.New("working order blocks close")
		}
	}
	if !a.Standard || !a.Tradable || a.Tick <= 0 || a.CloseLimit <= 0 || a.CloseLimit > MaxMoney/100 || a.CloseLimit%a.Tick != 0 || a.Ask <= 0 || a.Ask > a.CloseLimit || a.Bid < 0 || a.Bid > a.Ask {
		return errors.New("invalid close quote or limit")
	}
	debit := a.CloseLimit * 100
	if a.EntryCredit <= 2*c.Fee || a.EntryCredit > MaxMoney || debit >= a.EntryCredit-2*c.Fee {
		return errors.New("close does not preserve a profit after fee allowances")
	}
	if s.AvailableCash < c.Reserve || debit > s.AvailableCash-c.Reserve-c.Fee {
		return errors.New("insufficient available cash for close")
	}
	if !TradingWindow(now) || a.Expiry < now.In(Eastern).Format("2006-01-02") || !fresh(s.At, now, c.MaxQuoteAgeSeconds) || !fresh(a.At, now, c.MaxQuoteAgeSeconds) || !fresh(a.UnderlyingAt, now, c.MaxQuoteAgeSeconds) {
		return errors.New("close window or market data expired")
	}
	return nil
}

func ExecuteClose(ctx context.Context, c Config, st *State, store Store, b CloseBroker, approved Candidate, now func() time.Time) error {
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
	a, err := b.CloseQuote(ctx, *st, approved.Symbol)
	if err != nil {
		return err
	}
	if !approved.Closing || a.Option != approved.Option || a.EntryCredit != approved.EntryCredit {
		return errors.New("approved close identity changed")
	}
	a.CloseLimit = approved.CloseLimit // Never raise a user's limit or silently select another contract.
	if err = CheckClose(c, *st, s, a, now()); err != nil {
		return err
	}
	if err = b.Review(ctx, a); err != nil {
		return err
	}
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
	if err = CheckClose(c, *st, s, a, now()); err != nil {
		return err
	}
	return persistAndSubmit(ctx, c, st, store, b, a, s, now)
}
