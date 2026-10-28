package moomoo

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"autowheel/internal/wheel"
)

// CashInputs maps the documented cash fields, not an executable cash budget.
// LiquidityComponent still needs complete order, inventory and settlement
// reconciliation. In particular, it must never populate Snapshot.AvailableCash
// merely because every numeric field parsed successfully.
type CashInputs struct {
	AccountID                 string      `json:"account_id"`
	At                        time.Time   `json:"at"`
	FundsSHA256               string      `json:"funds_sha256"`
	USDCashFloor              wheel.Cents `json:"usd_cash_floor_cents"`
	USDWithdrawableFloor      wheel.Cents `json:"usd_withdrawable_floor_cents"`
	FundsHoldCeiling          wheel.Cents `json:"funds_hold_ceiling_cents"`
	AssetTransitCeiling       wheel.Cents `json:"asset_transit_ceiling_cents"`
	LiquidityComponent        wheel.Cents `json:"liquidity_component_cents"`
	NetCashPowerFloor         wheel.Cents `json:"usd_net_cash_power_floor_cents"`
	NetCashPowerKnown         bool        `json:"usd_net_cash_power_known"`
	CurrencyBalancesComplete  bool        `json:"currency_balances_complete"`
	UnavailableCurrencyFields []string    `json:"unavailable_currency_fields,omitempty"`
	UnreadableCurrencyFields  []string    `json:"unreadable_currency_fields,omitempty"`
	CurrencyDebitFields       []string    `json:"currency_debit_fields,omitempty"`
	ExecutionQualified        bool        `json:"execution_qualified"`
}

// cashBounds parses every digit into magnitude bounds plus an exact sign.
// Nonnegative assets round down and deductions round up; fractional cents
// never round to nearest. Negative required cash inputs are rejected by callers.
// It uses a whole-cent value plus a remainder bit, avoiding large scale products
// and binary floating point even for a billion dollars with twelve decimals.
func cashBounds(value string) (floor, ceiling wheel.Cents, negative bool, err error) {
	if value == "" || len(value) > 32 {
		return 0, 0, false, errors.New("cash decimal is missing or exceeds precision limits")
	}
	unsigned, signed := strings.CutPrefix(value, "-")
	whole, fraction, dot := strings.Cut(unsigned, ".")
	if whole == "" || len(fraction) > 12 || dot && fraction == "" {
		return 0, 0, false, errors.New("cash decimal format or precision is invalid")
	}
	for _, part := range []string{whole, fraction} {
		for _, char := range part {
			if char < '0' || char > '9' {
				return 0, 0, false, errors.New("cash decimal must contain exact digits")
			}
		}
	}
	w, parseErr := strconv.ParseInt(whole, 10, 64)
	if parseErr != nil || w > int64(wheel.MaxMoney)/100 {
		return 0, 0, false, errors.New("cash decimal exceeds money limits")
	}
	cents, _ := strconv.ParseInt((fraction + "00")[:2], 10, 64)
	floor = wheel.Cents(w*100 + cents)
	ceiling = floor
	if len(fraction) > 2 && strings.Trim(fraction[2:], "0") != "" {
		ceiling++
	}
	if ceiling > wheel.MaxMoney {
		return 0, 0, false, errors.New("cash decimal exceeds money limits")
	}
	return floor, ceiling, signed && ceiling != 0, nil
}

var cashCurrencyFields = []string{"us_cash", "hk_cash", "jp_cash", "cn_cash", "sg_cash", "kr_cash", "my_cash", "au_cash"}

func mapCashInputs(account string, at time.Time, raw json.RawMessage) (*CashInputs, error) {
	if !validAccountID(account) || at.IsZero() {
		return nil, errors.New("cash mapping requires its authenticated account and read clock")
	}
	var fields map[string]json.RawMessage
	if exactJSON(raw, &fields) != nil || fields == nil {
		return nil, errors.New("invalid Moomoo cash input document")
	}
	keys := []string{"currency", "us_avl_withdrawal_cash", "us_net_cash_power", "frozen_cash", "pending_asset"}
	keys = append(keys, cashCurrencyFields...)
	for key := range fields {
		for _, expected := range keys {
			if key != expected && strings.EqualFold(key, expected) {
				return nil, errors.New("Moomoo cash input contains a case alias")
			}
		}
	}
	textField := func(key string) (string, error) {
		var value string
		if len(fields[key]) == 0 || exactJSON(fields[key], &value) != nil || value == "" {
			return "", errors.New("Moomoo cash field is missing or unavailable: " + key)
		}
		return value, nil
	}
	currency, err := textField("currency")
	if err != nil || currency != "USD" {
		return nil, errors.New("Moomoo cash deductions require explicit USD display currency")
	}
	bounds := map[string][2]wheel.Cents{}
	for _, key := range []string{"us_cash", "us_avl_withdrawal_cash", "frozen_cash", "pending_asset"} {
		value, err := textField(key)
		if err != nil {
			return nil, err
		}
		floor, ceiling, negative, err := cashBounds(value)
		if err != nil || negative {
			return nil, errors.New("Moomoo cash field is negative, ambiguous or out of range: " + key)
		}
		bounds[key] = [2]wheel.Cents{floor, ceiling}
	}
	hash := sha256.Sum256(raw)
	input := &CashInputs{AccountID: account, At: at, FundsSHA256: hex.EncodeToString(hash[:]),
		USDCashFloor: bounds["us_cash"][0], USDWithdrawableFloor: bounds["us_avl_withdrawal_cash"][0],
		FundsHoldCeiling: bounds["frozen_cash"][1], AssetTransitCeiling: bounds["pending_asset"][1]}
	input.LiquidityComponent = min(input.USDCashFloor, input.USDWithdrawableFloor)
	// Pending asset and holds may overlap the withdrawal restriction. Subtract
	// both again rather than crediting an undocumented offset or incoming asset.
	for _, deduction := range []wheel.Cents{input.FundsHoldCeiling, input.AssetTransitCeiling} {
		input.LiquidityComponent -= min(input.LiquidityComponent, deduction)
	}
	// Net cash power is where Moomoo applies its own cash-secured holds.
	// A negative value means no cash power; an unavailable one stays unknown.
	if value, err := textField("us_net_cash_power"); err == nil {
		if floor, _, negative, err := cashBounds(value); err == nil {
			input.NetCashPowerKnown = true
			if !negative {
				input.NetCashPowerFloor = floor
			}
		}
	}
	for _, key := range cashCurrencyFields {
		value, err := textField(key)
		if err != nil {
			input.UnavailableCurrencyFields = append(input.UnavailableCurrencyFields, key)
			continue
		}
		_, _, negative, err := cashBounds(value)
		if err != nil {
			input.UnavailableCurrencyFields = append(input.UnavailableCurrencyFields, key)
			input.UnreadableCurrencyFields = append(input.UnreadableCurrencyFields, key)
		} else if negative {
			input.CurrencyDebitFields = append(input.CurrencyDebitFields, key)
		}
	}
	for key := range fields {
		withdrawalCurrency, withdrawal := strings.CutSuffix(key, "_avl_withdrawal_cash")
		knownWithdrawal := withdrawal && slices.Contains(cashCurrencyFields, withdrawalCurrency+"_cash")
		if strings.HasSuffix(key, "_cash") && key != "frozen_cash" && !knownWithdrawal && !slices.Contains(cashCurrencyFields, key) {
			input.UnavailableCurrencyFields = append(input.UnavailableCurrencyFields, key)
			input.UnreadableCurrencyFields = append(input.UnreadableCurrencyFields, key)
		}
	}
	slices.Sort(input.UnavailableCurrencyFields)
	input.CurrencyBalancesComplete = len(input.UnavailableCurrencyFields) == 0
	// Presence of all currency balances is necessary but cannot establish all
	// borrowing/obligations. ExecutionQualified intentionally remains false.
	return input, nil
}
