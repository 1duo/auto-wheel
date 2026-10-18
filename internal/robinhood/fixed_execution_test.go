package robinhood

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"autowheel/internal/contractpolicy"
	"autowheel/internal/wheel"
)

type fixedNativeClient struct {
	*fixedMarketClient
	wrongDeliverable bool
	reviews          int
}

func (c *fixedNativeClient) Supports(name, key string) bool {
	return key == "account_number" || c.fixedMarketClient.Supports(name, key)
}

func (c *fixedNativeClient) Call(ctx context.Context, name string, args object) (any, error) {
	if name == "get_option_instruments" && args["type"] == "call" {
		meta := object{"id": "proof-call", "chain_id": "chain", "chain_symbol": "AAPL", "type": "call", "expiration_date": "2026-10-30", "strike_price": "105.00", "trade_value_multiplier": "100", "underlying_type": "equity"}
		return object{"data": object{"instruments": []any{meta}}}, nil
	}
	if name == "review_option_order" {
		c.reviews++
		if args["account_number"] != "12345678" {
			return nil, errors.New("review was not account scoped")
		}
		instrument := "stock-id"
		if c.wrongDeliverable {
			instrument = "peer-stock"
		}
		return object{"data": object{"account_number": "12345678", "direction": "credit", "type": "limit", "time_in_force": "gfd", "market_hours": "regular_hours", "quantity": "1", "price": "0.05",
			"legs": []any{object{"option_id": "proof-call", "side": "sell", "position_effect": "open", "ratio_quantity": "1"}},
			"collateral": object{"account_number": "12345678", "equities": []any{
				object{"quantity": "0", "uncovered_shares": "100", "symbol": "AAPL", "direction": "debit", "instrument_id": instrument},
			}}}}, nil
	}
	return c.fixedMarketClient.Call(ctx, name, args)
}

func TestFixedNativeRefreshRequiresActualDeliverable(t *testing.T) {
	at := time.Date(2026, 10, 2, 16, 0, 0, 0, time.UTC)
	for _, wrong := range []bool{false, true} {
		client := &fixedNativeClient{fixedMarketClient: fixedMarketFixture(at), wrongDeliverable: wrong}
		client.chain["underlying_instruments"] = []any{object{"instrument": "/instruments/stock-id/", "symbol": "AAPL"}}
		config := wheel.DefaultConfig()
		config.Account = "12345678"
		b := &Broker{MCP: client, Config: config}
		chosen := contractpolicy.Contract{ID: "put-90", Symbol: "AAPL", Kind: "put", Expiry: "2026-10-30", Strike: 9000}
		got, err := b.RefreshForFixed(t.Context(), chosen, func() time.Time { return at })
		if wrong {
			if err == nil || got.Deliverable100 {
				t.Fatal("multiplier alone authorized an adjusted/foreign deliverable")
			}
		} else if err != nil || !got.Deliverable100 || got.Market.Contract != chosen || got.ChainID != "chain" || client.reviews != 1 {
			t.Fatal("native identity refresh did not verify account deliverable", err, got)
		}
	}
}

func TestFixedPreviewRetainsOldClocksWithoutEnablingRefresh(t *testing.T) {
	at := time.Date(2026, 10, 2, 16, 0, 0, 0, time.UTC)
	client := &fixedNativeClient{fixedMarketClient: fixedMarketFixture(at)}
	client.chain["underlying_instruments"] = []any{object{"instrument": "/instruments/stock-id/", "symbol": "AAPL"}}
	config := wheel.DefaultConfig()
	config.Account = "12345678"
	b := &Broker{MCP: client, Config: config}
	chosen := contractpolicy.Contract{ID: "put-90", Symbol: "AAPL", Kind: "put", Expiry: "2026-10-30", Strike: 9000}
	clock := func() time.Time { return at.Add(24 * time.Hour) }
	preview, err := b.PreviewForFixed(t.Context(), chosen, clock)
	if err != nil || !preview.Deliverable100 || !preview.Market.OptionAt.Equal(at) {
		t.Fatal("compatibility changed source clocks or skipped actual deliverable proof", err)
	}
	if _, err := b.RefreshForFixed(t.Context(), chosen, clock); err == nil {
		t.Fatal("old compatibility book became executable refresh evidence")
	}
}

func TestFixedNativeTransportRequiresNativeControls(t *testing.T) {
	for _, scenario := range []string{"no_activation_fence", "no_mutex", "shared_writer", "no_deadline"} {
		t.Run(scenario, func(t *testing.T) {
			client := fixedMarketFixture(time.Now())
			config := wheel.DefaultConfig()
			config.OrderCoveragePolicy = "exclusive_standard"
			config.FixedPolicy = new(wheel.FixedPolicyConfig)
			b := &Broker{MCP: client, Config: config, SubmitMu: new(sync.Mutex), BeforeSubmit: func() error { return nil }}
			deadline := time.Now()
			switch scenario {
			case "no_activation_fence":
				b.BeforeSubmit = nil
			case "no_mutex":
				b.SubmitMu = nil
			case "shared_writer":
				b.Config.CoordinatorSocket = "/other-writer"
			case "no_deadline":
				deadline = time.Time{}
			}
			if _, err := b.SubmitFixed(t.Context(), wheel.Candidate{}, "ref", deadline, func() error { return nil }); !errors.Is(err, wheel.ErrNotSubmitted) || len(client.calls) != 0 {
				t.Fatal("fixed adapter bypassed native controls", err, client.calls)
			}
		})
	}
}

func TestFixedNativeDeadlineUsesActualSession(t *testing.T) {
	for _, day := range []time.Time{
		time.Date(2026, 10, 19, 0, 0, 0, 0, wheel.Eastern),
		time.Date(2028, 7, 3, 0, 0, 0, 0, wheel.Eastern), // First session, 13:00 early close.
	} {
		_, execution, latest, known := wheel.FixedEntryTimes(day)
		if !known || !fixedDeadlineAllows(latest, execution) || !fixedDeadlineAllows(latest, latest) ||
			fixedDeadlineAllows(latest, execution.Add(-time.Nanosecond)) || fixedDeadlineAllows(latest, latest.Add(time.Nanosecond)) ||
			fixedDeadlineAllows(latest.Add(time.Minute), execution) || fixedDeadlineAllows(latest, execution.AddDate(0, 0, 1)) {
			t.Fatal("transport deadline does not match the actual weekly session", day)
		}
	}
}

func TestFixedTP75DeadlineIsDailyAndPolicyBound(t *testing.T) {
	config := wheel.DefaultConfig()
	config.FixedPolicy = &wheel.FixedPolicyConfig{Spec: wheel.FixedPolicySpec{ID: wheel.FixedConservativeTP75Policy}}
	for _, day := range []time.Time{time.Date(2026, 10, 20, 0, 0, 0, 0, wheel.Eastern), time.Date(2026, 11, 27, 0, 0, 0, 0, wheel.Eastern)} {
		_, execution, latest, known := wheel.FixedCloseTimes(day)
		a := wheel.Candidate{Closing: true}
		if !known || !fixedOrderDeadlineAllows(config, a, latest, execution) || fixedOrderDeadlineAllows(config, a, latest, execution.Add(-time.Nanosecond)) || fixedOrderDeadlineAllows(config, a, latest, latest.Add(time.Nanosecond)) || fixedOrderDeadlineAllows(config, a, latest.Add(time.Minute), execution) {
			t.Fatal("daily TP deadline failed", day)
		}
		config.FixedPolicy.Spec.ID = wheel.FixedConservativePolicy
		if fixedOrderDeadlineAllows(config, a, latest, execution) {
			t.Fatal("hold authorized a daily close")
		}
		config.FixedPolicy.Spec.ID = wheel.FixedConservativeTP75Policy
	}
}
