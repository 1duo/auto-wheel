package sharedbroker

import (
	"context"
	"errors"
	"testing"
)

func TestExactAccounting(t *testing.T) {
	for _, tc := range []struct {
		quantity, price string
		want            int64
	}{{"6.107717", "121.42", 74160}, {"0.000001", "0.01", 1}, {"100", "0.001", 10}} {
		got, err := Cost(tc.quantity, tc.price)
		if err != nil || got != tc.want {
			t.Fatalf("exact cost %s * %s: got %d, %v", tc.quantity, tc.price, got, err)
		}
	}
	for _, raw := range []string{"-1", "1e6", "1.0000001", "NaN", ".5", "1.2.3"} {
		if _, err := Micros(raw); err == nil {
			t.Fatalf("accepted inexact or invalid quantity %q", raw)
		}
	}
	if q, err := Micros("1.292084"); err != nil || q != 1_292_084 {
		t.Fatalf("fractional inventory changed: %d %v", q, err)
	}
}

type scripted struct {
	rows  map[string][]any
	next  map[string]string
	calls int
}

func (s *scripted) Supports(_, _ string) bool { return true }
func (s *scripted) Call(_ context.Context, name string, _ map[string]any) (any, error) {
	s.calls++
	row, ok := s.rows[name]
	if !ok || len(row) == 0 {
		return nil, errors.New("unscripted read")
	}
	s.rows[name] = row[1:]
	return row[0], nil
}

func TestCollectionRequiresEveryPage(t *testing.T) {
	fake := &scripted{rows: map[string][]any{"get_equity_orders": {
		Object{"orders": []any{Object{"id": "first"}}, "next": "opaque"},
		Object{"orders": []any{Object{"id": "second"}}, "next": ""},
	}}}
	b := Backend{Client: fake, Account: "1234"}
	rows, err := b.Collection(t.Context(), "get_equity_orders", "orders", nil)
	if err != nil || len(rows) != 2 || fake.calls != 2 {
		t.Fatalf("incomplete collection: %v %v", rows, err)
	}
	fake.rows["get_equity_orders"] = []any{
		Object{"orders": []any{}, "next": "same"},
		Object{"orders": []any{}, "next": "same"},
	}
	if _, err = b.Collection(t.Context(), "get_equity_orders", "orders", nil); err == nil {
		t.Fatal("repeated cursor accepted")
	}
	fake.rows["get_equity_orders"] = []any{Object{"next": ""}}
	if _, err = b.Collection(t.Context(), "get_equity_orders", "orders", nil); err == nil {
		t.Fatal("missing orders interpreted as an empty account")
	}
}

func TestCryptoScopingUsesNumericBrokerageIdentity(t *testing.T) {
	fake := &scripted{rows: map[string][]any{"get_crypto_orders": {
		Object{"rhs_account_number": "another", "results": []any{}},
	}}}
	b := Backend{Client: fake, Account: "1234"}
	if _, err := b.Collection(t.Context(), "get_crypto_orders", "results", Object{"rhs_account_number": "1234"}); err == nil {
		t.Fatal("wrong crypto account response accepted")
	}
	if _, err := b.Collection(t.Context(), "get_crypto_orders", "results", nil); err == nil {
		t.Fatal("missing numeric identity accepted")
	}
}
