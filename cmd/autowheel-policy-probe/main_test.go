package main

import (
	"context"
	"errors"
	"testing"
)

type deniedClient struct{ calls int }

func (*deniedClient) Supports(_, _ string) bool { return true }
func (c *deniedClient) Call(context.Context, string, map[string]any) (any, error) {
	c.calls++
	return nil, errors.New("unexpected provider call")
}

func TestObservationClientDeniesNonMarketOperations(t *testing.T) {
	provider := &deniedClient{}
	client := &recordingMarketClient{client: provider, write: func(string, any) error {
		t.Fatal("denied call reached disk")
		return nil
	}}
	for _, tool := range []string{"get_accounts", "review_option_order", "place_option_order", "cancel_option_order", "get_trade_approvals", "unknown"} {
		if _, err := client.Call(t.Context(), tool, nil); err == nil {
			t.Fatalf("accepted non-market operation %s", tool)
		}
	}
	if provider.calls != 0 || client.calls != 0 {
		t.Fatal("denied operation reached the provider")
	}
}
