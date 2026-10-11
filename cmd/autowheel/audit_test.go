package main

import "testing"

func TestAccountAuditRefusesFinancialAndReviewTools(t *testing.T) {
	reads := &auditedReads{}
	for _, name := range []string{"place_option_order", "place_equity_order", "cancel_equity_order", "exercise_option", "review_option_order", "preview_crypto_order", "add_to_watchlist"} {
		if _, err := reads.Call(t.Context(), name, nil); err == nil {
			t.Fatal("non-read audit tool admitted", name)
		}
	}
	if len(reads.calls) != 0 {
		t.Fatal("non-read transport or evidence was created")
	}
}
