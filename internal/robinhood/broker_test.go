package robinhood

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	aruntime "autowheel/internal/runtime"
	"autowheel/internal/wheel"
)

type settlementSnapshotClient struct{ position object }

func (c settlementSnapshotClient) Supports(name, key string) bool {
	return key == "account_number" || name == "get_advanced_orders" && key == "" || name == "get_option_instruments" && key == "ids"
}

func (c settlementSnapshotClient) Call(_ context.Context, name string, _ object) (any, error) {
	switch name {
	case "get_accounts":
		return object{"accounts": []any{object{"account_number": "12345678", "agentic_allowed": true, "state": "active", "type": "cash", "option_level": "2", "unsettled_funds": "0.00"}}}, nil
	case "get_portfolio":
		return object{"cash": "50000.00", "buying_power": object{"unleveraged_buying_power": "50000.00"}, "pending_deposits": "0.00"}, nil
	case "get_equity_positions":
		return object{"positions": []any{}}, nil
	case "get_option_positions":
		return object{"positions": []any{c.position}}, nil
	case "get_option_instruments":
		return object{"instruments": []any{object{"id": "own-put", "chain_symbol": "NVDA", "type": "put", "expiration_date": "2026-10-16", "strike_price": "200.00", "trade_value_multiplier": "100.0000"}}}, nil
	case "get_option_orders", "get_equity_orders", "get_advanced_orders":
		return object{"orders": []any{}}, nil
	case "get_trade_approval_setting":
		return object{"setting": object{"account_number": "12345678", "human_must_approve_trades": false}}, nil
	case "get_trade_approvals":
		return object{"approvals": []any{}}, nil
	default:
		return nil, fmt.Errorf("unexpected snapshot tool %s", name)
	}
}

func TestSnapshotPendingSettlementBlocksOpenAndClosedPositions(t *testing.T) {
	fields := []string{"pending_assignment_quantity", "pending_expiration_quantity", "pending_exercise_quantity"}
	for _, quantity := range []string{"0", "1"} {
		for _, field := range fields {
			for _, value := range []string{"0E-8", "1", "missing", "1.5", "invalid"} {
				t.Run(quantity+"/"+field+"/"+value, func(t *testing.T) {
					p := object{"quantity": quantity, "type": "short", "option_id": "own-put"}
					for _, key := range fields {
						p[key] = "0.0000"
					}
					if value == "missing" {
						delete(p, field)
					} else {
						p[field] = value
					}
					c := wheel.DefaultConfig()
					c.Account = "12345678"
					b := &Broker{MCP: settlementSnapshotClient{position: p}, Config: c}
					s, err := b.Snapshot(t.Context())
					if value == "0E-8" {
						wantOptions := 0
						if quantity == "1" {
							wantOptions = 1
						}
						if err != nil || len(s.Options) != wantOptions || !b.AccountInfo.SnapshotComplete {
							t.Fatalf("settled snapshot rejected: %+v, %v", s, err)
						}
					} else if err == nil || !strings.Contains(err.Error(), "settlement") || b.AccountInfo.SnapshotComplete {
						t.Fatalf("unproven settlement accepted as complete: %+v, %v", s, err)
					}
				})
			}
		}
	}
}

func previewFixture(t *testing.T) (object, wheel.Config, wheel.Candidate) {
	t.Helper()
	c := wheel.DefaultConfig()
	c.Account = "12345678"
	a := wheel.Candidate{Quote: wheel.Quote{Option: wheel.Option{ID: "option", Symbol: "NVDA", Kind: "put", Strike: 23000, Expiry: "2026-10-09"}, Bid: 170}, Collateral: 2300000}
	raw := `{"account_number":"12345678","direction":"credit","type":"limit","time_in_force":"gfd","market_hours":"regular_hours","quantity":"1","price":"1.70","legs":[{"option_id":"option","side":"sell","position_effect":"open","ratio_quantity":1}],"order_checks":{},"fees":{"total_fee":"0.04"},"collateral":{"account_number":"12345678","cash":{"amount":"23000.0000","direction":"debit","infinite":false},"equities":[]},"option_quotes":[{"instrument_id":"option","bid_price":"1.70","updated_at":"2026-10-02T19:59:59Z"}]}`
	v, err := decode([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	o := v.(object)
	o["option_quotes"].([]any)[0].(object)["updated_at"] = time.Now().Format(time.RFC3339Nano)
	return o, c, a
}

func TestObservedQuoteShapesAndDeliverableProof(t *testing.T) {
	schemas, err := os.ReadFile("testdata/inputs.json")
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := decode(schemas)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	expiry := now.In(wheel.Eastern).AddDate(0, 0, 1)
	for expiry.Weekday() != time.Friday {
		expiry = expiry.AddDate(0, 0, 1)
	}
	date := expiry.Format("2006-01-02")
	stamp := now.Format(time.RFC3339Nano)
	ticks := object{"above_tick": "0.05", "below_tick": "0.01", "cutoff_price": "3.00"}
	var adjusted, missingQuote atomic.Bool
	var malformed atomic.Value
	malformed.Store("")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req object
		json.NewDecoder(r.Body).Decode(&req)
		var result any
		switch req["method"] {
		case "initialize":
			result = object{"protocolVersion": protocol}
		case "notifications/initialized":
			w.WriteHeader(202)
			return
		case "tools/list":
			result = object{"tools": inventory}
		case "tools/call":
			params := req["params"].(object)
			args := params["arguments"].(object)
			var data object
			switch params["name"] {
			case "get_option_chains":
				data = object{"chains": []any{object{"id": "chain", "symbol": "NVDA", "can_open_position": true, "trade_value_multiplier": "100.0000", "cash_component": nil, "expiration_dates": []string{date}, "underlying_instruments": []any{object{"instrument": "http://edge-internal.region.rh/instruments/equity/", "symbol": ""}}}}}
			case "get_option_instruments":
				if args["expiration_dates"] != date {
					t.Error("expiration filter must be a string")
				}
				kind := args["type"].(string)
				meta := object{"id": kind, "chain_id": "chain", "chain_symbol": "NVDA", "type": kind, "expiration_date": date, "strike_price": "230.0000", "trade_value_multiplier": "100.0000", "underlying_type": "equity", "tradability": "tradable", "state": "active", "min_ticks": ticks}
				second := maps.Clone(meta)
				second["id"] = kind + "-second"
				switch malformed.Load().(string) {
				case "strike":
					second["strike_price"] = "invalid"
				case "zero_strike":
					second["strike_price"] = "0.00"
				case "missing_ticks":
					delete(second, "min_ticks")
				case "invalid_cutoff":
					second["min_ticks"] = object{"cutoff_price": "invalid"}
				case "zero_tick":
					second["min_ticks"] = object{"cutoff_price": "3.00", "below_tick": "0.00"}
				}
				data = object{"next": "", "instruments": []any{meta, second}}
				if malformed.Load() == "empty_chain" {
					data["instruments"] = []any{}
				}
			case "get_equity_quotes":
				data = object{"results": []any{object{"quote": object{"symbol": "NVDA", "last_trade_price": "233.9912", "venue_last_trade_time": stamp, "has_traded": true, "state": "active"}}}}
			case "get_option_quotes":
				quote := object{"instrument_id": "put", "bid_price": "1.70", "ask_price": "1.76", "delta": "-0.308273", "open_interest": 19561, "bid_size": 4, "updated_at": stamp}
				second := maps.Clone(quote)
				second["instrument_id"] = "put-second"
				switch field := malformed.Load().(string); field {
				case "updated_at", "bid_price", "ask_price", "delta", "open_interest", "bid_size":
					delete(second, field)
				case "nan_delta":
					second["delta"] = "NaN"
				case "infinite_delta":
					second["delta"] = "-Inf"
				case "delta_out_of_range":
					second["delta"] = "-1.01"
				case "delta_wrong_sign":
					second["delta"] = "0.3"
				case "crossed_book":
					second["ask_price"] = "1.69"
				case "zero_liquidity":
					second["open_interest"], second["bid_size"] = 0, 0
				}
				var q any = quote
				if missingQuote.Load() {
					q = nil
				}
				data = object{"results": []any{object{"quote": q}, object{"quote": second}}}
			case "review_option_order":
				quantity := "100.00000000"
				if adjusted.Load() {
					quantity = "150.00000000"
				}
				data = object{"account_number": args["account_number"], "direction": "credit", "quantity": args["quantity"], "price": args["price"], "type": args["type"], "time_in_force": args["time_in_force"], "market_hours": args["market_hours"], "legs": args["legs"], "collateral": object{"account_number": args["account_number"], "cash": object{"amount": "0.0000", "direction": "debit", "infinite": true}, "equities": []any{object{"symbol": "NVDA", "instrument_id": "equity", "quantity": "0E-8", "uncovered_shares": quantity, "direction": "debit"}}}}
			default:
				t.Errorf("unexpected tool %v", params["name"])
			}
			result = object{"structuredContent": object{"data": data}}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(object{"jsonrpc": "2.0", "id": req["id"], "result": result})
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "token.json")
	if err := aruntime.AtomicJSON(path, Credential{Resource: Endpoint, ClientID: "test", AccessToken: "test", RefreshToken: "test", ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	client := NewClient(path, false)
	client.endpoint = server.URL
	client.http = server.Client()
	if err := client.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	c := wheel.DefaultConfig()
	c.Account = "12345678"
	b := &Broker{MCP: client, Config: c}
	qs, err := b.Quotes(t.Context(), "NVDA", "put")
	if err != nil || len(qs) != 2 || !qs[0].Standard || qs[0].Spot != 23400 || qs[0].OI != 19561 {
		t.Fatal("observed broker shapes rejected", qs, err)
	}
	for _, field := range []string{"empty_chain", "strike", "zero_strike", "updated_at", "bid_price", "ask_price", "delta", "open_interest", "bid_size", "missing_ticks", "invalid_cutoff", "zero_tick", "nan_delta", "infinite_delta", "delta_out_of_range", "delta_wrong_sign", "crossed_book"} {
		t.Run(field, func(t *testing.T) {
			malformed.Store(field)
			// A valid first strike cannot justify a partial ranking when another
			// contract in the same nearest expiry has unproven inputs.
			if partial, err := b.Quotes(t.Context(), "NVDA", "put"); err == nil || len(partial) != 0 {
				t.Fatal("malformed nearest-expiry contract yielded a partial chain", err)
			}
		})
	}
	malformed.Store("zero_liquidity")
	if qs, err := b.Quotes(t.Context(), "NVDA", "put"); err != nil || len(qs) != 2 || qs[1].OI != 0 || qs[1].BidSize != 0 {
		t.Fatal("known zero liquidity must remain distinguishable from missing facts", err)
	}
	malformed.Store("")
	adjusted.Store(true)
	b.proofs = nil
	if _, err := b.Quotes(t.Context(), "NVDA", "put"); err == nil {
		t.Fatal("adjusted 150-share deliverable admitted")
	}
	adjusted.Store(false)
	missingQuote.Store(true)
	b.proofs = nil
	if _, err := b.Quotes(t.Context(), "NVDA", "put"); err == nil {
		t.Fatal("missing quote admitted")
	}
}

func TestActualReviewAndReceiptContracts(t *testing.T) {
	o, c, a := previewFixture(t)
	if err := reviewIdentity(o, c.Account, a); err != nil {
		t.Fatal(err)
	}
	if err := validateReview(o, c, a, true); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []func(object){
		func(o object) { o["order_checks"] = object{"alertType": "OPTION_WIDE_BID_ASK_SPREAD"} },
		func(o object) { o["fees"] = object{"total_fee": "1.01"} },
		func(o object) { o["collateral"].(object)["cash"].(object)["infinite"] = true },
		func(o object) { o["collateral"].(object)["cash"].(object)["amount"] = "22999.99" },
		func(o object) {
			o["option_quotes"].([]any)[0].(object)["updated_at"] = time.Now().Add(-time.Hour).Format(time.RFC3339)
		},
	} {
		sample, _, _ := previewFixture(t)
		mutation(sample)
		if err := validateReview(sample, c, a, true); err == nil {
			t.Fatal("unsafe review accepted")
		}
	}
	a.Kind = "call"
	o["collateral"].(object)["cash"].(object)["amount"] = "0.0000"
	equity := object{"symbol": "NVDA", "instrument_id": "equity", "quantity": "100.00000000", "uncovered_shares": "0E-8", "direction": "debit"}
	o["collateral"].(object)["equities"] = []any{equity}
	if err := validateReview(o, c, a, true); err != nil {
		t.Fatal("fully covered call rejected", err)
	}
	equity["uncovered_shares"] = "1.00000000"
	if err := validateReview(o, c, a, true); err == nil {
		t.Fatal("partially uncovered call accepted")
	}
	a.Kind = "put"
	order := object{"id": "order", "created_at": time.Now().Format(time.RFC3339Nano), "placed_agent": "agentic", "chain_symbol": "NVDA", "direction": "credit", "quantity": "1", "price": "1.70", "premium": "170.00", "trade_value_multiplier": "100.0000", "type": "limit", "trigger": "immediate", "time_in_force": "gfd", "market_hours": "regular_hours", "state": "confirmed", "legs": []any{object{"option_id": "option", "option_type": "put", "strike_price": "230.0000", "expiration_date": "2026-10-09", "side": "sell", "position_effect": "open", "ratio_quantity": json.Number("1")}}}
	if id, err := receipt(object{"data": object{"order": order}}, a); err != nil || id != "order" {
		t.Fatal("actual wrapper receipt rejected", err)
	}
	bad := maps.Clone(order)
	bad["premium"] = "1.70"
	if _, err := receipt(object{"data": object{"order": bad}}, a); err == nil {
		t.Fatal("wrong premium units accepted")
	}
	order["legs"].([]any)[0].(object)["option_id"] = "other"
	if _, err := receipt(object{"data": object{"order": order}}, a); err == nil {
		t.Fatal("wrong receipt contract accepted")
	}
	if n, err := ceilingPrice("65.632400"); err != nil || n != 6564 {
		t.Fatal("sub-cent stock price not conservatively parsed")
	}
	if n, err := positionCapital("6.107717", "214.455814"); err != nil || n != 130984 {
		t.Fatal("fractional external inventory capital not rounded conservatively", n, err)
	}
}

func TestSnapshotRequiresCashAccount(t *testing.T) {
	inventory := []object{{"name": "get_accounts", "inputSchema": object{"type": "object"}}}
	for _, name := range []string{"get_portfolio", "get_equity_positions", "get_option_positions", "get_option_orders", "get_equity_orders", "get_advanced_orders", "get_trade_approval_setting", "get_trade_approvals"} {
		inventory = append(inventory, object{"name": name, "inputSchema": object{
			"type": "object", "properties": object{"account_number": object{"type": "string"}}, "required": []string{"account_number"},
		}})
	}
	for _, tc := range []struct {
		name    string
		change  func(object)
		allowed bool
	}{
		{"cash", func(object) {}, true},
		{"cash_margin_disabled", func(a object) { a["margin_enabled"] = false }, true},
		{"margin_with_cash", func(a object) { a["type"] = "margin" }, false},
		{"missing_type", func(a object) { delete(a, "type") }, false},
		{"unknown_type", func(a object) { a["type"] = "unknown" }, false},
		{"conflicting_margin_flag", func(a object) { a["margin_enabled"] = true }, false},
		{"unproven_margin_flag", func(a object) { a["margin_enabled"] = "false" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := wheel.DefaultConfig()
			c.Account = "12345678"
			account := object{"account_number": c.Account, "agentic_allowed": true, "state": "active", "type": "cash", "brokerage_account_type": "individual", "option_level": "2", "unsettled_funds": "0.00"}
			tc.change(account)
			var reads atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req object
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					return
				}
				var result any
				switch req["method"] {
				case "initialize":
					result = object{"protocolVersion": protocol}
				case "notifications/initialized":
					w.WriteHeader(http.StatusAccepted)
					return
				case "tools/list":
					result = object{"tools": inventory}
				case "tools/call":
					params := req["params"].(object)
					name := params["name"].(string)
					reads.Add(1)
					var data object
					if name != "get_accounts" && params["arguments"].(object)["account_number"] != c.Account {
						t.Error("read escaped account binding")
					}
					switch name {
					case "get_accounts":
						data = object{"accounts": []any{account}, "next": ""}
					case "get_portfolio":
						data = object{"cash": "50000.00", "buying_power": object{"unleveraged_buying_power": "50000.00"}, "pending_deposits": "0.00"}
					case "get_equity_positions", "get_option_positions":
						data = object{"positions": []any{}, "next": ""}
					case "get_option_orders", "get_equity_orders", "get_advanced_orders":
						data = object{"orders": []any{}, "next": ""}
					case "get_trade_approval_setting":
						data = object{"setting": object{"account_number": c.Account, "human_must_approve_trades": false}}
					case "get_trade_approvals":
						data = object{"approvals": []any{}, "next": ""}
					default:
						t.Errorf("unexpected tool %s", name)
					}
					result = object{"structuredContent": object{"data": data}}
				}
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(object{"jsonrpc": "2.0", "id": req["id"], "result": result})
			}))
			defer server.Close()
			path := filepath.Join(t.TempDir(), "token.json")
			if err := aruntime.AtomicJSON(path, Credential{Resource: Endpoint, ClientID: "test", AccessToken: "test", RefreshToken: "test", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
				t.Fatal(err)
			}
			client := NewClient(path, false)
			client.endpoint, client.http = server.URL, server.Client()
			if err := client.Connect(t.Context()); err != nil {
				t.Fatal(err)
			}
			b := &Broker{MCP: client, Config: c}
			s, err := b.Snapshot(t.Context())
			if tc.allowed {
				if err != nil || s.AvailableCash != 5_000_000 || b.AccountInfo == nil || !b.AccountInfo.SnapshotComplete {
					t.Fatalf("cash snapshot rejected: %+v, %v", s, err)
				}
			} else if err == nil || reads.Load() != 1 {
				t.Fatalf("unproven cash account admitted or read beyond account gate: reads=%d, err=%v", reads.Load(), err)
			}
		})
	}
}
