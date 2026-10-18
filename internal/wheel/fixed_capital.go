package wheel

import (
	"errors"
	"time"
)

// RebindFixedCapital changes capital ceilings and the physical fee allowance before the first native
// cohort. The caller holds the journal lock under pause, archives the original
// state, persists this result, and obtains fresh broker readiness afterward.
// Existing inherited puts/shares, receipts, metadata and import evidence stay
// intact. This operation neither creates a signal nor authorizes trading.
func RebindFixedCapital(s State, config Config, now time.Time) (State, error) {
	if err := s.Validate(); err != nil {
		return State{}, err
	}
	if err := config.Validate(); err != nil {
		return State{}, err
	}
	if s.Fixed == nil || config.FixedPolicy == nil || s.Account != config.Account ||
		now.IsZero() || now.Before(s.Fixed.BoundAt) ||
		len(s.Intents) != 0 || s.Fixed.Batch != nil ||
		config.FixedPolicy.ImportLegacyStateSHA != fixedLegacyBinding(s.Fixed) {
		return State{}, errors.New("capital rebind requires the same owned pre-cohort policy and no pending signal or submission")
	}
	for symbol, p := range s.Positions {
		if !s.Fixed.inherited(symbol, p) {
			return State{}, errors.New("capital rebind cannot reclassify native or external inventory")
		}
	}
	previous, next := s.Fixed.Spec, config.FixedPolicy.Spec
	if previous.Digest() == next.Digest() || previous.Limits.Reserve != next.Limits.Reserve {
		return State{}, errors.New("capital rebind requires a ceiling change and preserves the cash reserve")
	}
	previous.Limits = next.Limits
	previous.PhysicalFeeBudget = next.PhysicalFeeBudget
	if previous.Digest() != next.Digest() || next.FeeKnownAt.Before(previous.FeeKnownAt) ||
		next.FeeKnownAt.After(now) || !fixedPhysicalFeeCovered(next, next.Limits.Symbol/100) {
		return State{}, errors.New("capital rebind preserves strategy and option fees, and requires current sufficient delivery-fee evidence")
	}
	f := *s.Fixed
	f.Spec, f.PolicySHA, f.BoundAt = next, next.Digest(), now
	s.Fixed = &f
	if err := s.Validate(); err != nil {
		return State{}, err
	}
	return s, nil
}
