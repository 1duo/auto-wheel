package main

// Offline Robinhood US retail disclosures checked 2026-10-13. One stock-option
// contract / one 100-share delivery, Jan 1–Oct 9, 2026; not a live fee bound.
// https://robinhood.com/us/en/support/articles/trading-fees-on-robinhood/
// https://cdn.robinhood.com/assets/robinhood/legal/RHF%20Fee%20Schedule.pdf
type portfolioFeeSchedule struct {
	Fixed     int64
	Robinhood bool
}

// Validated prices <= 1,000,000 cents, fixed quantity 100: principal*206 is
// <= 20,600,000,000, safely within int64. Round each SEC sale fee UP to cents.
func robinhoodSEC(date string, principal int64) int64 {
	if date < "2026-04-04" {
		return 0
	}
	return (principal*206 + 9_999_999) / 10_000_000
}

func (f portfolioFeeSchedule) optionSale(date string, price int64) int64 {
	if !f.Robinhood {
		return f.Fixed
	}
	// Zero commission; 4c combined ORF/OCC. Single-contract TAF ($0.00329)
	// and CAT ($0.0003) each round down to zero under Robinhood's rules.
	return 4 + robinhoodSEC(date, price*100)
}

func (f portfolioFeeSchedule) stockDeliverySale(date string, strike int64) int64 {
	if !f.Robinhood {
		return 0
	}
	// No assignment commission. Apply equity-sale regulatory rules to the
	// 100-share call-away proceeds: a model inference, not invoice evidence.
	// CAT is below 1c and rounds to zero.
	principal, sec, taf := strike*100, int64(0), int64(0)
	if principal > 50_000 { // Robinhood waives SEC on equity sales <= $500.
		sec = robinhoodSEC(date, principal)
	}
	if date < "2026-10-01" { // TAF paused Oct 1–Dec 31 in Robinhood support.
		taf = 2 // 100 * $0.000195 = 1.95c, rounded to nearest penny.
	}
	return sec + taf
}

func robinhoodFeeDisclosure() map[string]any {
	return map[string]any{
		"id": "robinhood-us-retail-2026-v1", "checked_on": "2026-10-13",
		"window":                  []string{"2026-01-01", "2026-10-09"},
		"sources":                 []string{"https://robinhood.com/us/en/support/articles/trading-fees-on-robinhood/", "https://cdn.robinhood.com/assets/robinhood/legal/RHF%20Fee%20Schedule.pdf", "https://robinhood.com/us/en/support/articles/options-investing/"},
		"option_commission_cents": 0, "orf_occ_cents_per_contract": 4,
		"sec_before_2026_04_04": "0", "sec_from_2026_04_04_per_million_usd": "20.60",
		"sec_rounding": "ceil each sale to cents", "single_contract_taf_cat_cents": 0,
		"stock_sale_taf_100_shares_before_2026_10_01_cents": 2,
		"taf_pause":             []string{"2026-10-01", "2026-12-31"},
		"stock_delivery_policy": "Published stock-sale rules applied to call-away proceeds; modeled inference, not invoice verification. No exercise/assignment commission.",
		"estimate_not_invoice":  true,
	}
}
