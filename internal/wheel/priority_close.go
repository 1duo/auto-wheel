package wheel

import (
	"context"
	"errors"
	"time"
)

// ProfitCloseTarget binds a single lifecycle, not future positions in the same
// symbol or contract. Configuration alone never supplies live authorization.
type ProfitCloseTarget struct {
	Symbol   string    `json:"symbol"`
	OptionID string    `json:"option_id"`
	OpenedAt time.Time `json:"opened_at"`
	MaxLimit Cents     `json:"max_limit_cents"`
}

// PriorityClosePending is evaluated after broker reconciliation. Filled evidence
// is durable so a restart cannot replay the action or require another prompt.
func PriorityClosePending(c Config, st State) (bool, error) {
	target := c.PriorityProfitClose
	if target == nil {
		return false, nil
	}
	if len(st.Intents) > 0 {
		return true, errors.New("priority profit close awaits broker reconciliation; no retry")
	}
	p := st.Positions[target.Symbol]
	owned := p.PutID == target.OptionID && p.PutOpenedAt.Equal(target.OpenedAt) && p.OwnedShares == 0 && p.CallID == "" && p.MissingSince.IsZero()
	attempted, filled := false, false
	for _, r := range st.CompletedOrders {
		if r.Closing && r.Symbol == target.Symbol && r.OptionID == target.OptionID && r.At.After(target.OpenedAt) {
			attempted = true
			filled = filled || r.Filled == 1
		}
	}
	if attempted {
		if filled && !owned {
			return false, nil
		}
		return true, errors.New("priority close already attempted without a reconciled fill; no automatic resubmission")
	}
	if !owned {
		return true, errors.New("priority close lifecycle changed without proven completion; entries blocked")
	}
	return true, nil
}

// RunPriorityClose consumes the cycle whenever a target is pending, including
// quote failures and non-live operation. The caller must not fall through to
// entry scanning. ExecuteClose retains all durable-intent and transport fences.
func RunPriorityClose(ctx context.Context, c Config, st *State, store Store, b CloseBroker, s Snapshot, live bool, now func() time.Time) (handled bool, candidate *Candidate, err error) {
	pending, err := PriorityClosePending(c, *st)
	if err != nil || !pending {
		return pending, nil, err
	}
	target := c.PriorityProfitClose
	a, err := b.CloseQuote(ctx, *st, target.Symbol)
	if err != nil {
		return true, nil, err
	}
	if a.Symbol != target.Symbol || a.ID != target.OptionID {
		return true, nil, errors.New("priority close quote does not match the exact target")
	}
	a.CloseLimit = target.MaxLimit
	if err = CheckClose(c, *st, s, a, now()); err != nil {
		return true, nil, err
	}
	if live {
		err = ExecuteClose(ctx, c, st, store, b, a, now)
	}
	return true, new(a), err
}
