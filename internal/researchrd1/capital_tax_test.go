package researchrd1

import "testing"

func TestExpandedCapitalStillRequiresCashAndThreeSlots(t *testing.T) {
	a := &Account{accountCap: 20_000_000, symbolCap: 20_000_000}
	if !a.capacity(8_404_717, 4_700_000, 0, 0, 100, 4_700_000, 0, true) {
		t.Fatal("funded $47,000 lot rejected by obsolete one-third allocation")
	}
	if Capacity(8_404_717, 4_700_000, 0, 0, 100, 4_700_000, 0, true) {
		t.Fatal("frozen legacy capacity changed")
	}
	if a.capacity(8_404_717, 8_000_000, 0, 0, 100, 8_000_000, 0, true) {
		t.Fatal("$200,000 ceiling fabricated available cash")
	}
	if a.capacity(30_000_000, 5_000_000, 15_000_001, 0, 100, 5_000_000, 1, true) {
		t.Fatal("account ceiling exceeded by a cent")
	}
	if a.capacity(30_000_000, 1_000_000, 0, 0, 100, 1_000_000, 3, true) {
		t.Fatal("fourth symbol admitted")
	}
	if a.capacity(30_000_000, 0, 0, 0, MaxMoney, 0, 0, false) {
		t.Fatal("unfunded debit accepted")
	}
}
