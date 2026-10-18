package wheel

import (
	"errors"
	"fmt"
	"time"
)

const StandardOrderWarning = "Restricted standard orders only. Advanced/OCO visibility is unknown. Do not place manual, contingent or other-strategy orders in this account. Observed outside activity blocks execution."

// CheckOrderScope applies before reconciliation can infer assignment ownership.
// A software lease cannot prove absence of broker-side contingent orders; this
// explicitly selected policy uses observed standard activity and cash/stock
// availability, rather than labelling partial order coverage as complete.
func CheckOrderScope(c Config, st State, s Snapshot) error {
	if st.Fixed != nil {
		return errors.New("fixed-policy journal requires its qualified native controller; legacy execution is fenced")
	}
	if c.OrderCoveragePolicy != "exclusive_standard" {
		if s.OrderCoveragePolicy == "exclusive_standard" {
			return errors.New("restricted order snapshot cannot satisfy full coverage policy")
		}
		return nil
	}
	if err := c.Validate(); err != nil {
		return err
	}
	if s.OrderCoveragePolicy != c.OrderCoveragePolicy || s.Account != c.Account || st.Account != c.Account {
		return errors.New("exclusive standard snapshot/account policy mismatch")
	}
	if len(s.ExternalSymbols) != 0 || s.ExternalCapital != 0 {
		return errors.New("outside or unsupported inventory in exclusive standard account")
	}
	seen := map[string]bool{}
	for _, o := range s.Orders {
		if o.ID == "" || seen[o.ID] || o.CreatedAt.IsZero() {
			return errors.New("order history identity or timing is ambiguous")
		}
		seen[o.ID] = true
		if o.OptionID == "" {
			if !o.Terminal() || o.LastTransactionAt.IsZero() || !o.CreatedAt.Before(c.ExclusiveOrdersSince) || !o.LastTransactionAt.Before(c.ExclusiveOrdersSince) {
				return fmt.Errorf("%s outside equity activity since native retirement; freeze", o.Symbol)
			}
			continue
		}
		in, pending := st.Intents[o.Symbol]
		known := pending && ((in.OrderID != "" && in.OrderID == o.ID) || (in.OrderID == "" && in.Ref == o.Ref))
		r, completed := st.CompletedOrders[o.ID]
		if known {
			r = OrderRecord{Ref: in.Ref, Symbol: o.Symbol, OptionID: in.Candidate.ID, At: in.At, Closing: in.Candidate.Closing}
		}
		if !known && !completed {
			return fmt.Errorf("%s option order not owned by AutoWheel; freeze", o.Symbol)
		}
		side, effect := "sell", "open"
		if r.Closing {
			side, effect = "buy", "close"
		}
		if o.Symbol != r.Symbol || o.OptionID != r.OptionID || o.Ref != "" && o.Ref != r.Ref || o.Side != side || o.Effect != effect || o.Quantity != 1 || o.Filled < 0 || o.Filled > 1 || o.CreatedAt.Before(r.At.Add(-2*time.Second)) || completed && !known && !o.Terminal() {
			return errors.New("option order differs from durable AutoWheel ownership")
		}
	}
	for _, h := range s.Shares {
		if h.Quantity == 0 {
			continue
		}
		p := st.Positions[h.Symbol]
		if h.Quantity != 100 || p.OwnedShares != 100 && !(p.PutID != "" && h.Cost == p.PutStrike) {
			return fmt.Errorf("%s outside stock inventory in exclusive account; freeze", h.Symbol)
		}
	}
	return nil
}

func ReconcileForConfig(c Config, st *State, s Snapshot, now time.Time) error {
	if err := CheckOrderScope(c, *st, s); err != nil {
		return err
	}
	return Reconcile(st, s, now)
}
