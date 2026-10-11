// Package sharedbroker collects complete account facts without inferring who
// owns them. Ownership comes exclusively from the coordinator's durable ledger.
package sharedbroker

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"strings"
	"time"
)

type Object = map[string]any

type Facts struct {
	At              time.Time           `json:"at"`
	AccountHash     string              `json:"account_identity_hash"`
	Accounts        any                 `json:"accounts"`
	Account         Object              `json:"account"`
	Portfolio       Object              `json:"portfolio"`
	Positions       []Object            `json:"equity_positions"`
	Orders          []Object            `json:"equity_orders"`
	Options         []Object            `json:"option_positions"`
	OptionOrders    []Object            `json:"option_orders"`
	CryptoPositions []Object            `json:"crypto_positions"`
	CryptoOrders    []Object            `json:"crypto_orders"`
	TaxLots         map[string][]Object `json:"tax_lots"`
	EligibleCash    int64               `json:"eligible_cash_cents"`
	Capital         int64               `json:"stock_capital_cents"`
	Digest          string              `json:"digest"`
}

type Backend struct {
	Client  ToolClient
	Account string
}

type ToolClient interface {
	Call(context.Context, string, map[string]any) (any, error)
	Supports(string, string) bool
}

func Text(o Object, key string) string {
	v, _ := o[key].(string)
	return v
}

func Data(v any) (Object, error) {
	o, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("broker object missing")
	}
	if inner, present := o["data"]; present {
		o, ok = inner.(map[string]any)
		if !ok {
			return nil, errors.New("broker data object missing")
		}
	}
	return o, nil
}

// Decimal accepts exact nonnegative plain decimals, including the broker's
// documented zero spelling. Exponents and binary floating point are excluded.
func Decimal(s string) (*big.Rat, error) {
	if s == "0E-8" {
		s = "0"
	}
	whole, fraction, _ := strings.Cut(s, ".")
	if whole == "" || len(s) > 40 || len(fraction) > 12 {
		return nil, errors.New("invalid exact nonnegative decimal")
	}
	for _, r := range whole + fraction {
		if r < '0' || r > '9' {
			return nil, errors.New("invalid exact nonnegative decimal")
		}
	}
	v, ok := new(big.Rat).SetString(s)
	if !ok || v.Sign() < 0 {
		return nil, errors.New("invalid exact decimal")
	}
	return v, nil
}

func scaled(v *big.Rat, scale int64, ceil bool) (int64, error) {
	r := new(big.Rat).Mul(v, big.NewRat(scale, 1))
	n, rem := new(big.Int), new(big.Int)
	n.QuoRem(r.Num(), r.Denom(), rem)
	if ceil && rem.Sign() > 0 {
		n.Add(n, big.NewInt(1))
	}
	if n.Sign() < 0 || !n.IsInt64() || n.Int64() > 100_000_000_000_000 {
		return 0, errors.New("scaled decimal exceeds checked range")
	}
	return n.Int64(), nil
}

func Money(s string, ceil bool) (int64, error) {
	v, err := Decimal(s)
	if err != nil {
		return 0, err
	}
	return scaled(v, 100, ceil)
}

func Cost(quantity, price string) (int64, error) {
	q, err := Decimal(quantity)
	if err != nil {
		return 0, err
	}
	p, err := Decimal(price)
	if err != nil {
		return 0, err
	}
	return scaled(new(big.Rat).Mul(q, p), 100, true)
}

func Micros(s string) (int64, error) {
	v, err := Decimal(s)
	if err != nil {
		return 0, err
	}
	r := new(big.Rat).Mul(v, big.NewRat(1_000_000, 1))
	if !r.IsInt() {
		return 0, errors.New("share quantity loses microshare precision")
	}
	return scaled(v, 1_000_000, false)
}

func Terminal(state string) bool {
	switch state {
	case "filled", "cancelled", "canceled", "partially_filled_rest_cancelled", "rejected", "failed", "voided":
		return true
	default:
		return false
	}
}

// Collection completes pagination and rejects duplicate/repeated cursors. A
// missing collection never means an empty account.
func (b *Backend) Collection(ctx context.Context, name, key string, args Object) ([]Object, error) {
	if args == nil {
		args = Object{}
	}
	if name == "get_crypto_orders" || name == "get_crypto_positions" {
		if !b.Client.Supports(name, "rhs_account_number") || Text(args, "rhs_account_number") == "" {
			return nil, errors.New("linked crypto account scoping unavailable")
		}
	} else if name != "get_accounts" {
		if !b.Client.Supports(name, "account_number") {
			return nil, fmt.Errorf("%s lacks account scoping", name)
		}
		args["account_number"] = b.Account
	}
	out := make([]Object, 0)
	seen := map[string]bool{}
	for range 32 {
		v, err := b.Client.Call(ctx, name, args)
		if err != nil {
			return nil, err
		}
		o, err := Data(v)
		if err != nil {
			return nil, err
		}
		if name == "get_crypto_orders" && Text(o, "rhs_account_number") != Text(args, "rhs_account_number") {
			return nil, errors.New("crypto account response binding mismatch")
		}
		if name == "get_equity_tax_lots" && Text(o, "symbol") != Text(args, "symbol") {
			return nil, errors.New("tax lot symbol response binding mismatch")
		}
		values, ok := o[key].([]any)
		if !ok {
			return nil, fmt.Errorf("%s lacks complete %s collection", name, key)
		}
		for _, row := range values {
			r, ok := row.(map[string]any)
			if !ok {
				return nil, errors.New("broker collection contains a missing row")
			}
			out = append(out, r)
		}
		if len(out) > 10000 {
			return nil, errors.New("broker collection exceeds row limit")
		}
		next := Text(o, "next")
		if x, present := o["next"]; present && x != nil && x != "" && next == "" {
			return nil, errors.New("broker cursor is malformed")
		}
		if next == "" {
			return out, nil
		}
		if seen[next] || !b.Client.Supports(name, "cursor") {
			return nil, errors.New("broker pagination is incomplete")
		}
		seen[next] = true
		args["cursor"] = next
	}
	return nil, errors.New("broker pagination exceeds page limit")
}

func (b *Backend) Snapshot(ctx context.Context) (Facts, error) {
	f := Facts{At: time.Now().UTC(), AccountHash: fmt.Sprintf("%x", sha256.Sum256([]byte(b.Account)))}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	accounts, err := b.Collection(ctx, "get_accounts", "accounts", nil)
	if err != nil {
		return f, err
	}
	f.Accounts = Object{"accounts": accounts}
	for _, row := range accounts {
		if Text(row, "account_number") == b.Account {
			if f.Account != nil {
				return f, errors.New("duplicate account binding")
			}
			f.Account = row
		}
	}
	a := f.Account
	if a == nil || a["agentic_allowed"] != true || Text(a, "type") != "cash" || Text(a, "state") != "active" || a["deactivated"] != false || a["permanently_deactivated"] != false {
		return f, errors.New("account is not an active agentic cash account")
	}
	if enabled, present := a["margin_enabled"]; present && enabled != false {
		return f, errors.New("account margin permission unproven")
	}
	level := Text(a, "option_level")
	if level != "option_level_2" && level != "option_level_3" {
		return f, errors.New("options approval missing")
	}
	if own, present := a["user_option_level"]; present && own != "option_level_2" && own != "option_level_3" {
		return f, errors.New("caller options approval missing")
	}
	v, err := b.Client.Call(ctx, "get_portfolio", Object{"account_number": b.Account})
	if err != nil {
		return f, err
	}
	f.Portfolio, err = Data(v)
	if err != nil {
		return f, err
	}
	if Text(f.Portfolio, "currency") != "USD" {
		return f, errors.New("account currency is not USD")
	}
	for _, field := range []string{"crypto_value", "futures_value", "event_contracts_value", "mutual_funds_value", "fixed_income_value"} {
		value, err := Decimal(Text(f.Portfolio, field))
		if err != nil || value.Sign() != 0 {
			return f, fmt.Errorf("unsupported or unproven account exposure: %s", field)
		}
	}
	cash, err := Money(Text(f.Portfolio, "cash"), false)
	if err != nil {
		return f, err
	}
	bp, ok := f.Portfolio["buying_power"].(map[string]any)
	if !ok || Text(bp, "display_currency") != "USD" {
		return f, errors.New("cash-only buying power unavailable")
	}
	eligible, err := Money(Text(bp, "unleveraged_buying_power"), false)
	if err != nil {
		return f, err
	}
	pending, err := Money(Text(f.Portfolio, "pending_deposits"), true)
	if err != nil {
		return f, err
	}
	unsettled, err := Money(Text(a, "unsettled_funds"), true)
	if err != nil {
		return f, errors.New("cash settlement amount unknown")
	}
	f.EligibleCash = max(0, min(cash, eligible)-pending-unsettled)
	f.Positions, err = b.Collection(ctx, "get_equity_positions", "positions", nil)
	if err != nil {
		return f, err
	}
	seen := map[string]bool{}
	for _, row := range f.Positions {
		symbol := Text(row, "symbol")
		q, err := Micros(Text(row, "quantity"))
		if err != nil || symbol == "" || seen[symbol] {
			return f, errors.New("equity identity or quantity ambiguous")
		}
		seen[symbol] = true
		available, err := Micros(Text(row, "shares_available_for_sells"))
		if err != nil || available > q {
			return f, errors.New("available stock quantity unproven")
		}
		if q > 0 {
			cost, err := Cost(Text(row, "quantity"), Text(row, "average_buy_price"))
			if err != nil || cost <= 0 || cost > 8_000_000 || f.Capital > 8_000_000-cost {
				return f, errors.New("stock capital missing or exceeds account ceiling")
			}
			f.Capital += cost
		}
	}
	for _, collection := range []struct {
		name, key string
		target    *[]Object
	}{
		{"get_equity_orders", "orders", &f.Orders},
		{"get_option_positions", "positions", &f.Options},
		{"get_option_orders", "orders", &f.OptionOrders},
	} {
		*collection.target, err = b.Collection(ctx, collection.name, collection.key, nil)
		if err != nil {
			return f, err
		}
	}
	// A linked crypto account can reserve the same pool even with zero inventory.
	if Text(a, "rhc_account_number") != "" {
		cryptoArgs := Object{"rhs_account_number": Text(a, "rhs_account_number")}
		f.CryptoOrders, err = b.Collection(ctx, "get_crypto_orders", "results", cryptoArgs)
		if err != nil {
			return f, err
		}
		f.CryptoPositions, err = b.Collection(ctx, "get_crypto_positions", "results", Object{"rhs_account_number": Text(a, "rhs_account_number")})
		if err != nil {
			return f, err
		}
		for _, order := range f.CryptoOrders {
			if !Terminal(Text(order, "state")) {
				return f, errors.New("unsupported working crypto order")
			}
		}
		for _, position := range f.CryptoPositions {
			quantity, err := Decimal(Text(position, "quantity"))
			if err != nil || quantity.Sign() != 0 {
				return f, errors.New("unsupported crypto inventory")
			}
		}
	}
	f.TaxLots = make(map[string][]Object)
	for _, position := range f.Positions {
		q, _ := Micros(Text(position, "quantity"))
		if q == 0 {
			continue
		}
		symbol := Text(position, "symbol")
		lots, err := b.Collection(ctx, "get_equity_tax_lots", "tax_lots", Object{"symbol": symbol})
		if err != nil {
			return f, err
		}
		var total int64
		ids := make(map[string]bool)
		for _, lot := range lots {
			id := Text(lot, "open_lot_id")
			quantity, err := Micros(Text(lot, "quantity"))
			if err != nil || id == "" || ids[id] || total > 100_000_000_000_000-quantity {
				return f, errors.New("stock tax lot identity or quantity ambiguous")
			}
			ids[id] = true
			total += quantity
			if _, err := Money(Text(lot, "tax_cost_basis"), true); err != nil {
				return f, errors.New("stock tax lot cost basis unproven")
			}
		}
		if total != q {
			return f, errors.New("stock tax lots disagree with current inventory")
		}
		f.TaxLots[symbol] = lots
	}
	if time.Since(f.At) > 30*time.Second {
		return f, errors.New("complete account collection is stale")
	}
	raw, err := json.Marshal(f)
	if err != nil {
		return f, err
	}
	f.Digest = fmt.Sprintf("%x", sha256.Sum256(raw))
	return f, nil
}

func (b *Backend) Underlying(ctx context.Context, symbol string) (string, error) {
	v, err := b.Client.Call(ctx, "get_option_chains", Object{"underlying_symbol": symbol})
	if err != nil {
		return "", err
	}
	o, err := Data(v)
	if err != nil {
		return "", err
	}
	chains, ok := o["chains"].([]any)
	if !ok || len(chains) != 1 {
		return "", errors.New("underlying instrument cannot be resolved uniquely")
	}
	chain, ok := chains[0].(map[string]any)
	if !ok || Text(chain, "symbol") != symbol {
		return "", errors.New("underlying symbol mapping mismatch")
	}
	underlyings, ok := chain["underlying_instruments"].([]any)
	if !ok || len(underlyings) != 1 {
		return "", errors.New("underlying instrument mapping ambiguous")
	}
	underlying, ok := underlyings[0].(map[string]any)
	if !ok {
		return "", errors.New("underlying identity missing")
	}
	u, err := url.Parse(Text(underlying, "instrument"))
	if err != nil || u.Scheme != "https" || u.Host != "api.robinhood.com" {
		return "", errors.New("underlying instrument origin mismatch")
	}
	id, ok := strings.CutPrefix(strings.TrimSuffix(u.Path, "/"), "/instruments/")
	if !ok || id == "" || strings.Contains(id, "/") {
		return "", errors.New("underlying UUID unavailable")
	}
	return id, nil
}
