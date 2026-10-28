package moomoo

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"autowheel/internal/wheel"
)

func TestCashBoundsExactDirectionalRoundingAndLimits(t *testing.T) {
	for _, tc := range []struct {
		value    string
		floor    wheel.Cents
		ceiling  wheel.Cents
		negative bool
	}{
		{"0", 0, 0, false}, {"-0.000", 0, 0, false}, {"12.34", 1234, 1234, false},
		{"12.340000000000", 1234, 1234, false}, {"12.340000000001", 1234, 1235, false},
		{"-0.000000000001", 0, 1, true}, {"-12.349", 1234, 1235, true},
		{"999999999.999999999999", wheel.MaxMoney - 1, wheel.MaxMoney, false},
		{"1000000000.000000000000", wheel.MaxMoney, wheel.MaxMoney, false},
	} {
		floor, ceiling, negative, err := cashBounds(tc.value)
		if err != nil || floor != tc.floor || ceiling != tc.ceiling || negative != tc.negative {
			t.Fatalf("%q: %d %d %v %v", tc.value, floor, ceiling, negative, err)
		}
	}
	for _, value := range []string{"", "-", ".1", "1.", "1e2", "+1", "1,000.00", "NaN", "Inf", " 1.00", "1.0.0", "0.0000000000001", "1000000000.000000000001", "1000000001", strings.Repeat("9", 100)} {
		if _, _, _, err := cashBounds(value); err == nil {
			t.Fatalf("ambiguous/out-of-range decimal accepted: %q", value)
		}
	}
}

func cashInputFixture() map[string]any {
	fields := map[string]any{"currency": "USD", "us_avl_withdrawal_cash": "12.3456", "frozen_cash": "0.001", "pending_asset": "1.005"}
	for _, key := range cashCurrencyFields {
		fields[key] = "0.00"
	}
	fields["us_cash"] = "17.129999999999"
	return fields
}

func TestCashInputsUseOnlyDocumentedCashAndPreserveUnknowns(t *testing.T) {
	for _, name := range []string{"complete", "blank-currency", "missing-currency", "negative-currency", "tiny-currency-debit", "new-currency", "new-withdrawable-currency", "overlapping-holds", "large-buying-power"} {
		t.Run(name, func(t *testing.T) {
			fields := cashInputFixture()
			switch name {
			case "blank-currency":
				fields["kr_cash"] = ""
			case "missing-currency":
				delete(fields, "kr_cash")
			case "negative-currency":
				fields["hk_cash"] = "-0.01"
			case "tiny-currency-debit":
				fields["hk_cash"] = "-0.000000000001"
			case "new-currency":
				fields["ca_cash"] = "-0.01"
			case "new-withdrawable-currency":
				fields["ca_avl_withdrawal_cash"] = "1.00"
			case "overlapping-holds":
				fields["frozen_cash"], fields["pending_asset"] = "100.00", "100.00"
			case "large-buying-power":
				fields["power"], fields["available_funds"], fields["max_withdrawal"] = "1000000000.00", "1000000000.00", "1000000000.00"
			}
			raw, _ := json.Marshal(fields)
			at := time.Now().UTC()
			input, err := mapCashInputs("123456", at, raw)
			if err != nil {
				t.Fatal(err)
			}
			hash := sha256.Sum256(raw)
			if input.AccountID != "123456" || !input.At.Equal(at) || input.FundsSHA256 != hex.EncodeToString(hash[:]) || input.USDCashFloor != 1712 || input.USDWithdrawableFloor != 1234 || input.ExecutionQualified {
				t.Fatal("cash mapping altered identity/source or invented execution readiness")
			}
			want := wheel.Cents(1132)
			if name == "overlapping-holds" {
				want = 0
			}
			if input.LiquidityComponent != want {
				t.Fatalf("liquidity component %d, want %d", input.LiquidityComponent, want)
			}
			incomplete := slices.Contains([]string{"blank-currency", "missing-currency", "new-currency", "new-withdrawable-currency"}, name)
			if input.CurrencyBalancesComplete == incomplete {
				t.Fatal("unavailable/new currency was treated as a complete balance")
			}
			wantDebit := name == "negative-currency" || name == "tiny-currency-debit"
			if (len(input.CurrencyDebitFields) != 0) != wantDebit {
				t.Fatal("currency debit detection disagrees with exact decimal sign")
			}
			if name == "negative-currency" && !slices.Contains(input.CurrencyDebitFields, "hk_cash") {
				t.Fatal("negative currency cash was discarded")
			}
		})
	}
}

func TestCashInputsRejectAmbiguousRequiredFacts(t *testing.T) {
	for _, name := range []string{"foreign-currency", "missing-cash", "blank-cash", "numeric-cash", "negative-cash", "negative-withdrawable", "negative-transit", "alias", "duplicate", "trailing"} {
		t.Run(name, func(t *testing.T) {
			fields := cashInputFixture()
			switch name {
			case "foreign-currency":
				fields["currency"] = "HKD"
			case "missing-cash":
				delete(fields, "us_cash")
			case "blank-cash":
				fields["us_cash"] = ""
			case "numeric-cash":
				fields["us_cash"] = 123
			case "negative-cash":
				fields["us_cash"] = "-0.00001"
			case "negative-withdrawable":
				fields["us_avl_withdrawal_cash"] = "-1.00"
			case "negative-transit":
				fields["pending_asset"] = "-1.00"
			case "alias":
				fields["US_CASH"] = "100.00"
			}
			raw, _ := json.Marshal(fields)
			if name == "duplicate" {
				raw = append([]byte(`{"us_cash":"1",`), raw[1:]...)
			} else if name == "trailing" {
				raw = append(raw, []byte(` {}`)...)
			}
			if input, err := mapCashInputs("123456", time.Now().UTC(), raw); err == nil || input != nil {
				t.Fatal("ambiguous cash became collateral inputs")
			}
		})
	}
}
