package wheel

// fixedPhysicalFeeMinimum bounds a standard 100-share call delivery using the
// reviewed SEC rate ($20.60/million) and normal 2026 stock TAF/CAT ceilings (2 and 1 cents).
// Recheck the selected sale notional: appreciation can exceed acquisition caps.
func fixedPhysicalFeeMinimum(strike int64) (Cents, bool) {
	if strike <= 0 || strike > int64(MaxMoney)/100 {
		return 0, false
	}
	notional := strike * 100
	return Cents((notional*206+9_999_999)/10_000_000 + 3), true
}

func fixedPhysicalFeeCovered(spec FixedPolicySpec, strike int64) bool {
	minimum, ok := fixedPhysicalFeeMinimum(strike)
	return ok && spec.PhysicalFeeBudget >= minimum
}
