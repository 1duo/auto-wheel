package moomoo

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"autowheel/internal/contractpolicy"
	"autowheel/internal/wheel"
)

const (
	sourceAccount = "123456"
	ownedPut      = "US.AAPL261120P90000"
	ownedRef      = "0a9bd3b2-6c1e-4f4e-9a55-1c2d3e4f5a6b"
)

func sourceStrategy(cutoff time.Time) wheel.Config {
	c := DefaultConfig()
	c.AccountID, c.Strategy.Account = sourceAccount, sourceAccount
	c.Strategy.Symbols = []string{"AAPL"}
	c.Strategy.OrderCoveragePolicy, c.Strategy.ExclusiveOrdersSince = wheel.CompleteMoomooOrders, cutoff
	c.Strategy.FixedPolicy = &wheel.FixedPolicyConfig{Spec: wheel.FixedPolicySpec{ID: wheel.FixedConservativeASAPPolicy, Account: sourceAccount,
		Limits: contractpolicy.CapitalLimits{Account: 8_000_000, Symbol: 8_000_000, Slots: 5}, OptionFeeBudget: 100, PhysicalFeeBudget: 1000,
		MaxQuoteAge: 30, FeeSourceSHA: strings.Repeat("a", 64), FeeKnownAt: cutoff.Add(-time.Hour)},
		Roster: wheel.FixedRoster{Symbols: []string{"AAPL"}, SourceSHA: strings.Repeat("b", 64), KnownAt: cutoff.Add(-time.Hour), From: "2026-01-01", Through: "2026-12-31"}}
	return c.Strategy
}

// providerOrder mirrors the real REST order shape observed for this account.
func providerOrder(t *testing.T, fields map[string]any) orderRow {
	t.Helper()
	row := map[string]any{"order_type": "LIMIT", "order_status": "FILLED_ALL", "security_type": "OPTION", "exchange": "US", "currency": "USD",
		"qty": "1", "price": "0", "dealt_qty": "1", "dealt_avg_price": "0", "time_in_force": "DAY", "session": "RTH"}
	maps.Copy(row, fields)
	if _, ok := row["updated_time"]; !ok {
		row["updated_time"] = row["create_time"]
	}
	raw, _ := json.Marshal(row)
	var parsed receiptRow
	if err := exactJSON(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	return orderRow{receiptRow: parsed, Raw: raw}
}

func micros(at time.Time) int64 { return at.UnixMicro() }

// cashOf is an account with no broker holds and fully settled cash.
func cashOf(n wheel.Cents) *CashInputs {
	return &CashInputs{USDCashFloor: n, LiquidityComponent: n, NetCashPowerFloor: n, NetCashPowerKnown: true}
}

func ownedPutJournal(opened time.Time) wheel.State {
	st := wheel.NewState()
	st.Account = sourceAccount
	st.Positions["AAPL"] = wheel.Position{PutID: ownedPut, PutExpiry: "2026-11-20", PutStrike: 9000, PutOpenedAt: opened, Provenance: "autowheel put fill " + ownedRef}
	st.CompletedOrders = map[string]wheel.OrderRecord{"FU-open": {Ref: ownedRef, Symbol: "AAPL", OptionID: ownedPut, At: opened, Filled: 1}}
	return st
}

func TestSnapshotQuarantinesExternalAndReservesEveryObligation(t *testing.T) {
	at := time.Date(2026, 10, 28, 15, 0, 0, 0, time.UTC)
	cutoff, opened := at.Add(-48*time.Hour), at.Add(-24*time.Hour)
	strategy := sourceStrategy(cutoff)
	base := []positionRow{
		{Code: "US.SPYG", Side: "LONG", SecurityType: "STOCK", Quantity: "250", Available: "250", Cost: "57.734", Currency: "USD"},
		{Code: "US.MRVL", Side: "LONG", SecurityType: "STOCK", Quantity: "0", Available: "0", Cost: "0", Currency: "USD"},
		{Code: "US.XYZ261218P50000", Side: "SHORT", SecurityType: "OPTION", Quantity: "-1", Available: "0", Cost: "1.2", Currency: "USD"},
		{Code: ownedPut, Side: "SHORT", SecurityType: "OPTION", Quantity: "-1", Available: "0", Cost: "1.5", Currency: "USD"},
	}
	target := providerOrder(t, map[string]any{"order_id": "FU-tp", "code": ownedPut, "side": "BUY_BACK", "order_status": "SUBMITTED", "remark": ownedRef,
		"price": "0.20", "dealt_qty": "0", "dealt_avg_price": "", "time_in_force": "GTC", "create_time": micros(at.Add(-time.Hour))})
	snapshot, err := buildSnapshot(sourceAccount, strategy, ownedPutJournal(opened), cashOf(10_000_000), base, []orderRow{target}, at)
	if err != nil {
		t.Fatal(err)
	}
	// $50 external put + $90 owned put + $0.20 target and its fee allowance.
	if snapshot.AvailableCash != 10_000_000-500_000-900_000-2_000-100 || snapshot.ExternalCapital != 1_443_350 {
		t.Fatalf("settled cash or external capital: %d %d", snapshot.AvailableCash, snapshot.ExternalCapital)
	}
	if !slices.Equal(snapshot.ExternalSymbols, []string{"SPYG", "XYZ"}) || len(snapshot.Shares) != 0 {
		t.Fatal("external holdings were not quarantined", snapshot.ExternalSymbols, snapshot.Shares)
	}
	want := wheel.Option{ID: ownedPut, ChainID: "US.AAPL", Symbol: "AAPL", Kind: "put", Expiry: "2026-11-20", Strike: 9000, Quantity: 1}
	if len(snapshot.Options) != 1 || snapshot.Options[0] != want || snapshot.OrderCoveragePolicy != wheel.CompleteMoomooOrders {
		t.Fatal("owned contract identity was not mapped exactly", snapshot.Options)
	}
	o := snapshot.Orders[0]
	if len(snapshot.Orders) != 1 || o.Ref != ownedRef || o.Side != "buy" || o.Effect != "close" || o.TimeInForce != "gtc" || o.Type != "limit" ||
		o.Limit != 20 || o.Terminal() || o.Symbol != "AAPL" || o.OptionID != ownedPut || o.Kind != "put" || o.Strike != 9000 {
		t.Fatal("working resting target was not mapped exactly", o)
	}
	for name, mutate := range map[string]func([]positionRow) []positionRow{
		"uncovered-external-call": func(p []positionRow) []positionRow {
			return append(p, positionRow{Code: "US.SPYG261218C60000", Side: "SHORT", SecurityType: "OPTION", Quantity: "-3", Available: "0", Cost: "1", Currency: "USD"})
		},
		"short-stock": func(p []positionRow) []positionRow {
			return append(p, positionRow{Code: "US.TSLA", Side: "SHORT", SecurityType: "STOCK", Quantity: "-10", Available: "0", Cost: "1", Currency: "USD"})
		},
		"foreign-currency": func(p []positionRow) []positionRow {
			return append(p, positionRow{Code: "HK.00700", Side: "LONG", SecurityType: "STOCK", Quantity: "100", Available: "100", Cost: "1", Currency: "HKD"})
		},
		"ambiguous-option": func(p []positionRow) []positionRow {
			return append(p, positionRow{Code: "US.AMD1261204P200000", Side: "LONG", SecurityType: "OPTION", Quantity: "1", Available: "1", Cost: "1", Currency: "USD"})
		},
		"long-owned": func(p []positionRow) []positionRow {
			p = slices.Clone(p)
			p[3].Side, p[3].Quantity = "LONG", "1"
			return p
		},
	} {
		if _, err := buildSnapshot(sourceAccount, strategy, ownedPutJournal(opened), cashOf(10_000_000), mutate(base), nil, at); err == nil {
			t.Fatal(name, "unbounded or out-of-scope obligation produced a snapshot")
		}
	}
	covered := append(slices.Clone(base), positionRow{Code: "US.SPYG261218C60000", Side: "SHORT", SecurityType: "OPTION", Quantity: "-2", Available: "0", Cost: "1", Currency: "USD"})
	if _, err := buildSnapshot(sourceAccount, strategy, ownedPutJournal(opened), cashOf(10_000_000), covered, nil, at); err != nil {
		t.Fatal("covered external call blocked execution", err)
	}
	if _, err := buildSnapshot(sourceAccount, strategy, ownedPutJournal(opened), cashOf(1_399_999), base, nil, at); err == nil {
		t.Fatal("obligations beyond settled cash produced a snapshot; that would require borrowing")
	}
	market := providerOrder(t, map[string]any{"order_id": "FU-mkt", "code": "US.SPYG", "security_type": "STOCK", "side": "BUY", "order_type": "MARKET",
		"order_status": "SUBMITTED", "qty": "10", "dealt_qty": "0", "create_time": micros(at.Add(-time.Minute))})
	if _, err := buildSnapshot(sourceAccount, strategy, ownedPutJournal(opened), cashOf(10_000_000), base, []orderRow{market}, at); err == nil {
		t.Fatal("an unbounded working buy order did not block")
	}
}

func TestSnapshotRecognizesOnlyOwnedSettlementLegs(t *testing.T) {
	at := time.Date(2026, 11, 22, 15, 0, 0, 0, time.UTC)
	cutoff, opened := time.Date(2026, 10, 27, 17, 0, 0, 0, time.UTC), time.Date(2026, 10, 28, 15, 0, 0, 0, time.UTC)
	assigned := time.Date(2026, 11, 21, 7, 2, 39, 0, time.UTC)
	strategy := sourceStrategy(cutoff)
	legs := []orderRow{
		providerOrder(t, map[string]any{"order_id": "FU-leg-option", "code": ownedPut, "side": "BUY_BACK", "create_time": micros(assigned)}),
		providerOrder(t, map[string]any{"order_id": "FU-leg-stock", "code": "US.AAPL", "security_type": "STOCK", "side": "BUY", "qty": "100", "price": "90",
			"dealt_qty": "100", "dealt_avg_price": "90", "create_time": micros(assigned)}),
	}
	stock := []positionRow{{Code: "US.AAPL", Side: "LONG", SecurityType: "STOCK", Quantity: "100", Available: "100", Cost: "88.35", Currency: "USD"}}
	snapshot, err := buildSnapshot(sourceAccount, strategy, ownedPutJournal(opened), cashOf(1_000_000), stock, legs, at)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Orders) != 0 || len(snapshot.Shares) != 1 || snapshot.Shares[0] != (wheel.Shares{Symbol: "AAPL", Quantity: 100, Available: 100, Cost: 9000}) {
		t.Fatal("owned assignment legs were not recognized with the broker assignment price", snapshot.Orders, snapshot.Shares)
	}
	for name, change := range map[string]map[string]any{
		"foreign-contract": {"code": "US.AAPL261120P85000"},
		"has-remark":       {"remark": "FA|1|2|3"},
		"nonzero-price":    {"price": "0.05"},
		"fractional-clock": {"create_time": micros(assigned) + 7},
		"before-opening":   {"create_time": micros(opened.Add(-time.Hour))},
	} {
		fields := map[string]any{"order_id": "FU-leg-option", "code": ownedPut, "side": "BUY_BACK", "create_time": micros(assigned)}
		maps.Copy(fields, change)
		rows := []orderRow{providerOrder(t, fields), legs[1]}
		snapshot, err := buildSnapshot(sourceAccount, strategy, ownedPutJournal(opened), cashOf(1_000_000), stock, rows, at)
		if err == nil && len(snapshot.Orders) == 0 {
			t.Fatal(name, "an unproven settlement leg was hidden from order-scope review")
		}
	}
	wrongPrice := []orderRow{legs[0], providerOrder(t, map[string]any{"order_id": "FU-leg-stock", "code": "US.AAPL", "security_type": "STOCK", "side": "BUY",
		"qty": "100", "price": "89", "dealt_qty": "100", "dealt_avg_price": "89", "create_time": micros(assigned)})}
	snapshot, err = buildSnapshot(sourceAccount, strategy, ownedPutJournal(opened), cashOf(1_000_000), stock, wrongPrice, at)
	if err != nil || len(snapshot.Orders) != 2 || snapshot.Shares[0].Cost == 9000 {
		t.Fatal("a stock leg at a different price became assignment evidence or hid its option leg", err)
	}
	old := providerOrder(t, map[string]any{"order_id": "FU-old", "code": "US.NBIS", "security_type": "STOCK", "side": "BUY", "qty": "100", "price": "117",
		"dealt_qty": "100", "dealt_avg_price": "117", "create_time": micros(cutoff.Add(-30 * 24 * time.Hour))})
	working := providerOrder(t, map[string]any{"order_id": "FU-working", "code": "US.NBIS", "security_type": "STOCK", "side": "SELL", "qty": "100", "price": "200",
		"order_status": "SUBMITTED", "dealt_qty": "0", "dealt_avg_price": "", "time_in_force": "GTC", "create_time": micros(cutoff.Add(-30 * 24 * time.Hour))})
	snapshot, err = buildSnapshot(sourceAccount, strategy, ownedPutJournal(opened), cashOf(1_000_000), stock, append(slices.Clone(legs), old, working), at)
	if err != nil || len(snapshot.Orders) != 1 || snapshot.Orders[0].ID != "FU-working" {
		t.Fatal("pre-cutoff terminal history was not excluded or a still-working order was hidden", err)
	}
}

func TestSnapshotRecognizesOwnedCallAway(t *testing.T) {
	at := time.Date(2026, 12, 20, 15, 0, 0, 0, time.UTC)
	cutoff, opened := time.Date(2026, 10, 27, 17, 0, 0, 0, time.UTC), time.Date(2026, 11, 24, 15, 0, 0, 0, time.UTC)
	call := "US.AAPL261218C95000"
	st := wheel.NewState()
	st.Account = sourceAccount
	st.Positions["AAPL"] = wheel.Position{OwnedShares: 100, Cost: 9000, CallID: call, CallExpiry: "2026-12-18", CallOpenedAt: opened,
		PutOpenedAt: opened.Add(-30 * 24 * time.Hour), InventoryAt: opened.Add(-time.Hour), Provenance: "autowheel put fill " + ownedRef}
	st.CompletedOrders = map[string]wheel.OrderRecord{"FU-call": {Ref: ownedRef, Symbol: "AAPL", OptionID: call, At: opened, Filled: 1}}
	away := time.Date(2026, 12, 19, 5, 32, 4, 0, time.UTC)
	legs := []orderRow{
		providerOrder(t, map[string]any{"order_id": "FU-away-option", "code": call, "side": "BUY_BACK", "create_time": micros(away)}),
		providerOrder(t, map[string]any{"order_id": "FU-away-stock", "code": "US.AAPL", "security_type": "STOCK", "side": "SELL", "qty": "100", "price": "95",
			"dealt_qty": "100", "dealt_avg_price": "95", "create_time": micros(away)}),
	}
	snapshot, err := buildSnapshot(sourceAccount, sourceStrategy(cutoff), st, cashOf(1_000_000), nil, legs, at)
	if err != nil || len(snapshot.Orders) != 0 || len(snapshot.Shares) != 0 || len(snapshot.Options) != 0 {
		t.Fatal("owned call-away legs were not recognized", err, snapshot.Orders)
	}
	legs[1] = providerOrder(t, map[string]any{"order_id": "FU-away-stock", "code": "US.AAPL", "security_type": "STOCK", "side": "BUY", "qty": "100", "price": "95",
		"dealt_qty": "100", "dealt_avg_price": "95", "create_time": micros(away)})
	if snapshot, err := buildSnapshot(sourceAccount, sourceStrategy(cutoff), st, cashOf(1_000_000), nil, legs, at); err != nil || len(snapshot.Orders) != 2 {
		t.Fatal("a call leg paired with a stock purchase was treated as call-away", err)
	}
}

// The owned assignment must reconcile through the real fixed controller,
// without freezing on Moomoo's settlement orders, and adopt exactly 100 shares.
// The opening itself is created by a real native ASAP cycle.
func TestOwnedAssignmentReconcilesThroughFixedController(t *testing.T) {
	e, state, hits := executionFixture(t, "ack")
	if err := e.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := e.Broker.Activation.Enable(); err != nil {
		t.Fatal(err)
	}
	if err := e.Cycle(t.Context()); err != nil || *hits != 1 {
		t.Fatal("native opening was not dispatched once", err)
	}
	fixture := e.Broker.Source.(*executionFixtureSource)
	intent := mustJournal(t, state).Intents["AAPL"]
	fixture.orders[0].State, fixture.orders[0].Filled = "filled", 1
	fixture.alter = func(s *wheel.Snapshot) { s.Options = []wheel.Option{intent.Candidate.Option} }
	if err := e.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	opened := mustJournal(t, state)
	if opened.Positions["AAPL"].PutID != intent.Candidate.ID {
		t.Fatal("filled native opening did not become owned")
	}
	config := e.Broker.Config
	code, strike := intent.Candidate.ID, intent.Candidate.Strike
	at := intent.At
	open := providerOrder(t, map[string]any{"order_id": "broker-order", "code": code, "side": "SELL_SHORT", "remark": intent.Ref, "price": intent.Candidate.Bid.Dollars(),
		"dealt_avg_price": intent.Candidate.Bid.Dollars(), "create_time": micros(at), "updated_time": micros(at.Add(time.Second))})
	expiry, _ := time.ParseInLocation(time.DateOnly, intent.Candidate.Expiry, wheel.Eastern)
	assigned := expiry.Add(31*time.Hour + 2*time.Minute).Truncate(time.Second)
	legs := []orderRow{open,
		providerOrder(t, map[string]any{"order_id": "FU-leg-option", "code": code, "side": "BUY_BACK", "create_time": micros(assigned)}),
		providerOrder(t, map[string]any{"order_id": "FU-leg-stock", "code": "US.AAPL", "security_type": "STOCK", "side": "BUY", "qty": "100", "price": strike.Dollars(),
			"dealt_qty": "100", "dealt_avg_price": strike.Dollars(), "create_time": micros(assigned)}),
	}
	stock := []positionRow{{Code: "US.AAPL", Side: "LONG", SecurityType: "STOCK", Quantity: "100", Available: "100", Cost: "97.35", Currency: "USD"}}
	reconcile := func(now time.Time, rows []orderRow) error {
		snapshot, err := buildSnapshot(config.AccountID, config.Strategy, mustJournal(t, state), cashOf(2_000_000), stock, rows, now)
		if err != nil {
			return err
		}
		return e.controller.ReconcileNative(config.Strategy, snapshot, now)
	}
	first := assigned.Add(2 * time.Hour)
	if err := reconcile(first, legs); err != nil {
		t.Fatal("Moomoo settlement orders froze an owned assignment", err)
	}
	if err := reconcile(first.Add(25*time.Hour), legs); err != nil {
		t.Fatal("settled assignment did not reconcile", err)
	}
	if p := mustJournal(t, state).Positions["AAPL"]; p.OwnedShares != 100 || p.Cost != strike || p.PutID != "" || p.InventoryAt.IsZero() {
		t.Fatal("assignment was not adopted as exactly 100 shares at the strike", p)
	}
	if err := reconcile(first.Add(26*time.Hour), legs); err != nil {
		t.Fatal("adopted inventory lost its broker assignment basis on a later cycle", err)
	}
	// A later outside equity order on the managed symbol still freezes.
	manual := providerOrder(t, map[string]any{"order_id": "FU-manual", "code": "US.AAPL", "security_type": "STOCK", "side": "SELL", "qty": "100", "price": "95",
		"dealt_qty": "100", "dealt_avg_price": "95", "remark": "FA|1|2|3", "time_in_force": "GTC", "create_time": micros(first.Add(26 * time.Hour))})
	if err := reconcile(first.Add(27*time.Hour), append(legs, manual)); err == nil {
		t.Fatal("outside equity activity on owned inventory did not freeze")
	}
}

func mustJournal(t *testing.T, state *State) wheel.State {
	t.Helper()
	st, err := state.JournalState()
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestOptionTickIsValidInEveryConsistentRegime(t *testing.T) {
	for _, c := range []struct {
		price wheel.Cents
		quote []wheel.Cents
		want  wheel.Cents
	}{
		{150, []wheel.Cents{150, 155}, 5},  // No proof: standard nickel below $3.
		{123, []wheel.Cents{123, 125}, 1},  // Off-nickel quote proves penny below $3.
		{320, []wheel.Cents{320, 330}, 10}, // No proof: standard dime at/above $3.
		{320, []wheel.Cents{320, 325}, 5},  // 5¢ above $3 proves a penny class.
		{90, []wheel.Cents{320, 325}, 1},   // That class quotes pennies below $3.
		{350, []wheel.Cents{351, 352}, 1},  // Pennies above $3: all-penny class.
		{350, []wheel.Cents{123, 125}, 5},  // Penny below $3 does not prove pennies above.
	} {
		if got := optionTick(c.price, c.quote...); got != c.want || c.price%got != 0 && c.price == c.quote[0] {
			t.Fatalf("tick for %d with %v = %d, want %d", c.price, c.quote, got, c.want)
		}
	}
}

func TestFeeBoundsCoverThisAccountsActualFees(t *testing.T) {
	// Actual one-contract order-fee documents read from this account.
	for _, c := range []struct {
		sell   bool
		limit  wheel.Cents
		actual wheel.Cents
	}{{true, 100, 300}, {true, 300, 300}, {true, 1925, 302}} {
		if bound := optionFeeBound(c.sell, c.limit); bound < c.actual || bound > 400 {
			t.Fatalf("fee bound %d does not cover actual %d within the 400 cent budget", bound, c.actual)
		}
	}
	if optionFeeBound(false, 20) > 400 || physicalFeeBound(24250) < 52 || physicalFeeBound(5450) < 13 || physicalFeeBound(100_000) > 1000 {
		t.Fatal("delivery or buy-back fee bounds disagree with account evidence or budget")
	}
}

func TestParseOptionCodeRequiresExactSingleLegIdentity(t *testing.T) {
	code, err := parseOptionCode("US.AMD261204P200000")
	if err != nil || code != (optionCode{Root: "AMD", Expiry: "2026-12-04", Kind: "put", Strike: 20000}) {
		t.Fatal("standard code was not parsed exactly", code, err)
	}
	for _, bad := range []string{"US.AAPL270319C100/105/110", "US.AMD261304P200000", "US.AMD261204P200005", "HK.AMD261204P200000", "US.AMD261204X200000", "US.AMD"} {
		if _, err := parseOptionCode(bad); err == nil {
			t.Fatal("ambiguous code accepted", bad)
		}
	}
}

func TestReviewRequiresExactAccountCapacity(t *testing.T) {
	for _, capacity := range []string{"0", "1"} {
		c := DefaultConfig()
		c.AccountID, c.Strategy = sourceAccount, sourceStrategy(time.Now().Add(-time.Hour))
		client := NewClient(evidenceCredential(t))
		client.http.Transport = reviewTransport(func(r *http.Request) (*http.Response, error) {
			body := ""
			switch r.URL.Path {
			case "/api/v1.0/quote/stock-basicinfo":
				body = `{"ret_code":0,"ret_msg":"","data":{"basic_list":[{"code":"US.AAPL261120P90000","stock_type":"DRVT","stock_owner":"US.AAPL","exchange":"US","state":"NORMAL","suspension":false,"lot_size":100,"contract_size":100}]}}`
			case "/api/v1.0/quote/snapshot":
				now := time.Now().UnixMilli()
				body = `{"ret_code":0,"ret_msg":"","data":{"snapshot_list":[` +
					`{"code":"US.AAPL","update_time":` + jsonInt(now) + `,"data_date":"2026-10-28","bid_price":100,"ask_price":100.02,"bid_vol":3,"ask_vol":2,"sec_status":"NORMAL","suspension":false,"option_valid":false},` +
					`{"code":"US.AAPL261120P90000","update_time":` + jsonInt(now) + `,"data_date":"2026-10-28","bid_price":1.23,"ask_price":1.26,"bid_vol":4,"ask_vol":5,"sec_status":"NORMAL","suspension":false,"option_valid":true}]}}`
			case "/api/v1.0/accounts/123456/acctradinginfo":
				q := r.URL.Query()
				if q.Get("code") != ownedPut || q.Get("price") != "1.23" || q.Get("order_type") != "LIMIT" {
					t.Fatal("capacity query was not for the exact limit order")
				}
				body = `{"s":"ok","d":{"max_cash_buy":"5","max_cash_and_margin_buy":"5","max_position_sell":"0","max_sell_short":"` + capacity + `","max_buy_back":"0","long_required_im":"0","short_required_im":"0"}}`
			default:
				t.Fatal("review reached an unexpected resource", r.URL.Path)
			}
			if r.Method == http.MethodPost && r.URL.Path == "/api/v1.0/accounts/123456/orders" {
				t.Fatal("review placed an order")
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		})
		source := &Source{Client: client, Config: c}
		candidate := wheel.Candidate{Quote: wheel.Quote{Option: wheel.Option{ID: ownedPut, ChainID: "US.AAPL", Symbol: "AAPL", Kind: "put", Expiry: "2026-11-20", Strike: 9000, Quantity: 1}, Bid: 123, Ask: 126}}
		review, err := source.Review(t.Context(), candidate)
		if capacity == "0" {
			if err == nil {
				t.Fatal("zero account capacity reviewed as authorized")
			}
			continue
		}
		if err != nil || review.Side != "SELL_SHORT" || !review.OptionsAuthorized || !review.Deliverable100 || review.AccountID != sourceAccount ||
			review.CandidateSHA256 != candidateDigest(candidate) || review.FeeCents > 400 || review.FeeCents < 300 {
			t.Fatal("account review did not bind exact terms, capacity and fee bound", review, err)
		}
	}
}

func jsonInt(n int64) string { raw, _ := json.Marshal(n); return string(raw) }

func TestObservationWindowRotatesWithinTheChainReadBudget(t *testing.T) {
	roster := []string{"AMD", "ORCL", "INTC", "CRM", "BAC", "XOM", "WMT", "BA", "JNJ", "CVX"}
	seen := map[string]bool{}
	for cursor := range 3 {
		window := observationWindow(roster, cursor*maxObservationsPerCycle)
		if len(window) != maxObservationsPerCycle {
			t.Fatal("observation window exceeded the per-cycle chain budget", window)
		}
		for _, symbol := range window {
			seen[symbol] = true
		}
	}
	if len(seen) != len(roster) || !slices.Equal(observationWindow(roster[:3], 7), roster[:3]) {
		t.Fatal("rotation starved a roster entry or reordered a short roster", seen)
	}
	if observationWindow(roster, 0)[0] != "AMD" || observationWindow(roster, 1)[0] != "ORCL" {
		t.Fatal("rotation is not a deterministic offset")
	}
	if roster[0] != "AMD" {
		t.Fatal("rotation mutated the controller's ordering")
	}
}

// Obligations are removed exactly once whether Moomoo shows its cash-secured
// hold in net cash power, withdrawable cash, both or neither.
func TestAvailableCashRemovesObligationsExactlyOnce(t *testing.T) {
	for _, c := range []struct {
		name                    string
		ledger, withdraw, power wheel.Cents
		known                   bool
		reserved                wheel.Cents
		want                    wheel.Cents
		fails                   bool
	}{
		{"no-broker-hold", 100_000, 96_000, 100_000, true, 50_000, 46_000, false},
		{"hold-in-power-and-withdrawable", 100_000, 46_000, 50_000, true, 50_000, 46_000, false},
		{"hold-in-power-only", 100_000, 96_000, 50_000, true, 50_000, 50_000, false},
		{"hold-in-withdrawable-only", 100_000, 46_000, 100_000, true, 50_000, 0, false},
		{"power-unknown", 100_000, 96_000, 0, false, 50_000, 46_000, false},
		{"borrowing", 40_000, 40_000, 40_000, true, 50_000, 0, true},
		{"negative-power", 100_000, 96_000, 0, true, 0, 0, false},
	} {
		cash := &CashInputs{USDCashFloor: c.ledger, LiquidityComponent: min(c.ledger, c.withdraw), NetCashPowerFloor: c.power, NetCashPowerKnown: c.known}
		got, err := availableCash(cash, c.reserved)
		if (err != nil) != c.fails || got != c.want || got > c.ledger-min(c.ledger, c.reserved) {
			t.Fatalf("%s: available %d err %v, want %d", c.name, got, err, c.want)
		}
	}
	// The real funds document maps net cash power; a blank value stays unknown.
	raw := []byte(`{"currency":"USD","us_cash":"50000.25","us_avl_withdrawal_cash":"47500.50","us_net_cash_power":"50000.25","frozen_cash":"0","pending_asset":"0.6","hk_cash":"0","jp_cash":"0","cn_cash":"0","sg_cash":"0","kr_cash":"","my_cash":"","au_cash":""}`)
	cash, err := mapCashInputs(sourceAccount, time.Now(), raw)
	if err != nil || !cash.NetCashPowerKnown || cash.NetCashPowerFloor != 5_000_025 || cash.LiquidityComponent != 4_749_990 {
		t.Fatal("net cash power or liquidity was not mapped exactly", cash, err)
	}
	if got, _ := availableCash(cash, 0); got != 4_749_990 {
		t.Fatal("unencumbered account did not keep settled withdrawable cash", got)
	}
}

func TestSnapshotFundsCoveredCallOrdersAndFreezesUncoveredOnes(t *testing.T) {
	at := time.Date(2026, 12, 1, 15, 0, 0, 0, time.UTC)
	cutoff := time.Date(2026, 10, 27, 17, 0, 0, 0, time.UTC)
	st := wheel.NewState()
	st.Account = sourceAccount
	st.Positions["AAPL"] = wheel.Position{OwnedShares: 100, Cost: 9000, PutOpenedAt: cutoff.Add(time.Hour), InventoryAt: cutoff.Add(48 * time.Hour), Provenance: "autowheel put fill " + ownedRef}
	call := providerOrder(t, map[string]any{"order_id": "FU-call", "code": "US.AAPL261231C95000", "side": "SELL_SHORT", "order_status": "SUBMITTED", "remark": ownedRef,
		"price": "1.10", "dealt_qty": "0", "dealt_avg_price": "", "create_time": micros(at.Add(-time.Minute))})
	shares := []positionRow{{Code: "US.AAPL", Side: "LONG", SecurityType: "STOCK", Quantity: "100", Available: "0", Cost: "90", Currency: "USD"}}
	snapshot, err := buildSnapshot(sourceAccount, sourceStrategy(cutoff), st, cashOf(1_000_000), shares, []orderRow{call}, at)
	if err != nil || snapshot.AvailableCash != 1_000_000 || len(snapshot.Orders) != 1 || snapshot.Orders[0].Kind != "call" {
		t.Fatal("a working covered call blocked the snapshot or reserved cash", err)
	}
	if _, err := buildSnapshot(sourceAccount, sourceStrategy(cutoff), st, cashOf(1_000_000), nil, []orderRow{call}, at); err == nil {
		t.Fatal("a working call without stock coverage did not freeze")
	}
	held := append(slices.Clone(shares), positionRow{Code: "US.AAPL261231C99000", Side: "SHORT", SecurityType: "OPTION", Quantity: "-1", Available: "0", Cost: "1", Currency: "USD"})
	if _, err := buildSnapshot(sourceAccount, sourceStrategy(cutoff), st, cashOf(1_000_000), held, []orderRow{call}, at); err == nil {
		t.Fatal("one lot of stock covered both a short call and a working call")
	}
}

// The attempt marker is synced before any send. Only records proving no
// dispatch release their intent; a possibly sent one stays fenced.
func TestRecoveryDropsOnlyNeverDispatchedIntents(t *testing.T) {
	for _, phase := range []string{"missing", "prepared", "not_submitted", "unknown"} {
		t.Run(phase, func(t *testing.T) {
			s, c, dir, terms := attemptFixture(t)
			var handle *preparedPlacement
			if phase != "missing" {
				handle = mustPrepareAttempt(t, s, terms)
			}
			switch phase {
			case "not_submitted":
				if _, err := s.attemptPlacement(t.Context(), handle, func(context.Context) error { return errors.New("denied") }, func(context.Context, PlacementRequest) (int, []byte, error) {
					t.Fatal("a denied guard reached the transport")
					return 0, nil, nil
				}); !errors.Is(err, wheel.ErrNotSubmitted) {
					t.Fatal("denied preflight was not recorded as unsent", err)
				}
			case "unknown":
				if _, err := s.attemptPlacement(t.Context(), handle, attemptGuard, func(context.Context, PlacementRequest) (int, []byte, error) {
					return 0, nil, errors.New("timeout")
				}); !errors.Is(err, ErrPlacementOutcomeUnknown) {
					t.Fatal("transport timeout was not retained as unknown", err)
				}
			}
			st := wheel.NewState()
			st.Account = terms.AccountID
			st.Intents["AAPL"] = wheel.Intent{Ref: terms.Ref, At: terms.CreatedAt, Candidate: wheel.Candidate{Quote: wheel.Quote{
				Option: wheel.Option{ID: terms.Code, Symbol: "AAPL", Kind: "put", Expiry: "2026-10-30", Strike: 10000, Quantity: 1}, Bid: terms.LimitCents}}}
			if err := s.Save(st); err != nil {
				t.Fatal(err)
			}
			s.Close()
			reopened, err := OpenState(dir, c)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			b := &Broker{Store: reopened}
			err = b.recoverIntents(t.Context(), mustJournal(t, reopened))
			_, kept := mustJournal(t, reopened).Intents["AAPL"]
			if phase == "unknown" {
				if err == nil || !kept || reopened.CheckAttemptFence() == nil {
					t.Fatal("a possibly dispatched order was released without its receipt")
				}
				return
			}
			if err != nil || kept || reopened.CheckAttemptFence() != nil {
				t.Fatal("a never-dispatched intent fenced trading after restart", err)
			}
		})
	}
}

func TestOrderHistoryReadsOnlyTheNeededWindowAndKeepsTerminalOrders(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	cutoff := now.Add(-10 * time.Hour)
	working := now.Add(-5 * time.Hour)
	row := func(id string, created time.Time, status string) map[string]any {
		return map[string]any{"order_id": id, "code": "US.AAPL261231P90000", "side": "BUY_BACK", "order_type": "LIMIT", "order_status": status, "remark": ownedRef,
			"create_time": created.UnixMicro(), "updated_time": created.Add(time.Second).UnixMicro(), "security_type": "OPTION", "exchange": "US", "currency": "USD",
			"qty": "1", "price": "0.20", "time_in_force": "GTC", "dealt_qty": "0", "dealt_avg_price": ""}
	}
	var starts []int64
	history := [][]map[string]any{
		{row("FU-old", cutoff.Add(time.Hour), "FILLED_ALL"), row("FU-tp", working, "SUBMITTED")},
		{func() map[string]any {
			r := row("FU-tp", working, "FILLED_ALL")
			r["updated_time"] = now.UnixMicro()
			return r
		}()},
		{},
	}
	client := NewClient(evidenceCredential(t))
	client.http.Transport = reviewTransport(func(r *http.Request) (*http.Response, error) {
		orders := []map[string]any{}
		if strings.HasSuffix(r.URL.Path, "/orders_history") {
			start, _ := strconv.ParseInt(r.URL.Query().Get("start"), 10, 64)
			starts = append(starts, start)
			orders = history[len(starts)-1]
		}
		raw, _ := json.Marshal(map[string]any{"s": "ok", "d": map[string]any{"orders": orders, "page_flag": "", "completed": true}})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
	})
	source := &Source{Client: client}
	for i, at := range []time.Time{now, now.Add(10 * time.Second), now.Add(30 * time.Second), now.Add(60 * time.Second)} {
		rows, err := source.accountOrders(t.Context(), sourceAccount, cutoff, at)
		if err != nil || len(rows) != 2 {
			t.Fatal("history cache lost or invented an order", i, err, len(rows))
		}
		if i == 3 && (rows[1].ID != "FU-tp" || !rows[1].terminal()) {
			t.Fatal("an order that completed after its first read stayed working")
		}
	}
	if len(starts) != 3 || starts[0] != cutoff.UnixMicro() || starts[1] != working.UnixMicro() || starts[2] != now.Add(30*time.Second).Add(-time.Hour).UnixMicro() {
		t.Fatal("history did not read the full window once, then only back to open orders or the last hour", starts)
	}
}

// Only this transport's own pre-send denial proves nothing was sent; the
// same public sentinel from any other dispatcher stays unknown.
func TestTransportPreSendDenialIsRecordedUnsent(t *testing.T) {
	for _, own := range []bool{true, false} {
		s, _, _, terms := attemptFixture(t)
		handle := mustPrepareAttempt(t, s, terms)
		_, err := s.attemptPlacement(t.Context(), handle, attemptGuard, func(context.Context, PlacementRequest) (int, []byte, error) {
			if own {
				return 0, nil, errTransportNotInvoked
			}
			return 0, nil, wheel.ErrNotSubmitted
		})
		if own != errors.Is(err, wheel.ErrNotSubmitted) || own != (s.CheckAttemptFence() == nil) {
			t.Fatalf("own transport denial %t: %v", own, err)
		}
	}
	// The real transport denies before sending when the cycle is already over.
	client := NewTradingClient(evidenceCredential(t), "9007199254740993")
	client.http.Transport = reviewTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("a cancelled cycle reached the order endpoint")
		return nil, nil
	})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	terms, _, _, _ := receiptFixture(t)
	request, _ := PreparePlacement(terms, time.Now())
	if _, _, err := client.sendPlacement(ctx, request, func() error { return nil }); !errors.Is(err, errTransportNotInvoked) {
		t.Fatal("cancelled transport did not prove it was not invoked", err)
	}
}

// Snapshot recovery drops a never-dispatched intent; preparation must continue
// from that durable journal rather than re-save its earlier copy.
func TestPrepareDoesNotResurrectRecoveredIntent(t *testing.T) {
	e, state, hits := executionFixture(t, "ack")
	at := state.now()
	expiry := at.AddDate(0, 0, 31).Format(time.DateOnly)
	code := "US.AAPL" + at.AddDate(0, 0, 31).Format("060102") + "P100000"
	terms := OrderIntentTerms{AccountID: "123456", Ref: ownedRef, Code: code, Side: "SELL_SHORT", Quantity: 1, LimitCents: 200, CreatedAt: at.Add(-time.Minute).UTC()}
	if _, err := state.preparePlacement(terms); err != nil {
		t.Fatal(err)
	}
	delete(state.preparing, terms.Ref) // As after a restart: no live dispatch handle remains.
	st := wheel.NewState()
	st.Account = "123456"
	st.Intents["AAPL"] = wheel.Intent{Ref: terms.Ref, At: terms.CreatedAt, Candidate: wheel.Candidate{Quote: wheel.Quote{
		Option: wheel.Option{ID: code, Symbol: "AAPL", Kind: "put", Expiry: expiry, Strike: 10000, Quantity: 1}, Bid: 200}}}
	if err := state.Save(st); err != nil {
		t.Fatal(err)
	}
	if err := e.Prepare(t.Context()); err != nil {
		t.Fatal("recovered journal did not bind", err)
	}
	if stored := mustJournal(t, state); len(stored.Intents) != 0 || stored.Fixed == nil || *hits != 0 {
		t.Fatal("a never-dispatched intent was resurrected or sent", stored.Intents)
	}
}

// A live request waiting behind a cycle gives up at its own deadline and never
// publishes that cancellation as broker unreadiness.
func TestPrepareWaitsOnlyUntilItsDeadline(t *testing.T) {
	e, _, _ := executionFixture(t, "ack")
	if err := e.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	before := e.Readiness()
	e.mu.Lock()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := e.Prepare(ctx)
	e.mu.Unlock()
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 2*time.Second {
		t.Fatal("activation preparation did not stop at its deadline", err)
	}
	if after := e.Readiness(); after.At != before.At || after.Ready != before.Ready {
		t.Fatal("a cancelled activation published broker unreadiness")
	}
}

func TestFilledOrderRequiresItsFillFacts(t *testing.T) {
	at := time.Date(2026, 11, 2, 15, 0, 0, 0, time.UTC)
	for name, fields := range map[string]map[string]any{
		"missing":  {"dealt_qty": nil},
		"empty":    {"dealt_qty": ""},
		"partial":  {"qty": "2", "dealt_qty": "1"},
		"cancel-0": {"order_status": "CANCELLED_PART", "dealt_qty": "0"},
	} {
		row := map[string]any{"order_id": "FU-x", "code": ownedPut, "side": "SELL_SHORT", "remark": ownedRef, "price": "1.5", "dealt_avg_price": "1.5", "create_time": micros(at)}
		maps.Copy(row, fields)
		if _, err := providerOrder(t, row).wheelOrder(); err == nil {
			t.Fatal(name, "terminal order without exact fill facts was mapped")
		}
	}
	if o, err := providerOrder(t, map[string]any{"order_id": "FU-x", "code": ownedPut, "side": "SELL_SHORT", "remark": ownedRef, "price": "1.5",
		"dealt_avg_price": "1.5", "create_time": micros(at)}).wheelOrder(); err != nil || o.Filled != 1 || o.State != "filled" {
		t.Fatal("an exact one-contract fill was not mapped", err)
	}
}

// A provider application error resolves an attempt only after a complete,
// later audit shows no order with its reference; anything else stays unknown.
func TestProviderRejectionResolvesOnlyWithLaterAbsence(t *testing.T) {
	rejection := []byte(`{"s":"error","errcode":-1200,"errmsg":"insufficient buying power"}`)
	confirm := []byte(`{"s":"error","errcode":-2100,"errmsg":"order confirmation required","need_order_confirm":true,"confirm_id":"c1"}`)
	browser := []byte(`{"s":"error","errcode":-2101,"errmsg":"browser action required","need_order_confirm":false,"jump_url":"https://example.invalid"}`)
	for _, c := range []struct {
		name   string
		status int
		body   []byte
		err    error
		later  time.Duration
		order  bool
		ok     bool
	}{
		{"rejection-then-absent", 200, rejection, nil, 2 * time.Minute, false, true},
		{"audit-too-soon", 200, rejection, nil, 30 * time.Second, false, false},
		{"order-exists", 200, rejection, nil, 2 * time.Minute, true, false},
		{"timeout", 0, nil, errors.New("timeout"), 2 * time.Minute, false, false},
		{"server-error", 502, rejection, nil, 2 * time.Minute, false, false},
		{"confirmation-required", 200, confirm, nil, 2 * time.Minute, false, false},
		{"browser-action", 200, browser, nil, 2 * time.Minute, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, _, _, terms := attemptFixture(t)
			handle := mustPrepareAttempt(t, s, terms)
			if _, err := s.attemptPlacement(t.Context(), handle, attemptGuard, func(context.Context, PlacementRequest) (int, []byte, error) {
				return c.status, c.body, c.err
			}); !errors.Is(err, ErrPlacementOutcomeUnknown) {
				t.Fatal("provider error was not retained as unknown at send time", err)
			}
			now := time.Now().UTC().Add(c.later)
			audit := &OrderAudit{AccountID: terms.AccountID, At: now.Add(-time.Second), Market: "US", HistoryFrom: terms.CreatedAt.Add(-time.Minute),
				HistoryTo: now.Add(-time.Second), RecentPages: 1, HistoryPages: 1, Recent: []OrderEvidence{}, History: []OrderEvidence{}}
			if c.order {
				_, _, withOrder, _ := receiptFixture(t)
				audit.Recent = withOrder.Recent
			}
			err := s.rejectPlacement(terms.Ref, audit, now)
			if (err == nil) != c.ok || (s.CheckAttemptFence() == nil) != c.ok {
				t.Fatalf("rejection resolved %t, want %t: %v", err == nil, c.ok, err)
			}
		})
	}
}

func TestInterruptedAtomicWriteDoesNotFenceTrading(t *testing.T) {
	s, _, dir, terms := attemptFixture(t)
	handle := mustPrepareAttempt(t, s, terms)
	if _, err := s.attemptPlacement(t.Context(), handle, attemptGuard, func(context.Context, PlacementRequest) (int, []byte, error) {
		return 200, []byte(`{"s":"ok","d":{"order_id":"FU1"}}`), nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/moomoo-order-attempts/.autowheel-123456", []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckAttemptFence(); err != nil {
		t.Fatal("an interrupted atomic write fenced trading", err)
	}
	if s.attemptRecorded(nextAttemptRef) || !s.attemptRecorded(terms.Ref) {
		t.Fatal("attempt record presence was misreported")
	}
}

// The rejection path must work through real recovery, not only in isolation:
// a provider error plus a later complete audit without the reference releases
// the intent; the same audit without the provider error does not.
func TestRecoveryResolvesProviderRejectionEndToEnd(t *testing.T) {
	for _, rejected := range []bool{true, false} {
		s, c, dir, terms := attemptFixture(t)
		s.clock = func() time.Time { return time.Now().Add(-2 * time.Minute) }
		handle := mustPrepareAttempt(t, s, terms)
		body := []byte(`{"s":"error","errcode":-1200,"errmsg":"rejected"}`)
		if !rejected {
			body = nil
		}
		if _, err := s.attemptPlacement(t.Context(), handle, attemptGuard, func(context.Context, PlacementRequest) (int, []byte, error) {
			if !rejected {
				return 0, nil, errors.New("timeout")
			}
			return 200, body, nil
		}); !errors.Is(err, ErrPlacementOutcomeUnknown) {
			t.Fatal(err)
		}
		st := wheel.NewState()
		st.Account = terms.AccountID
		st.Intents["AAPL"] = wheel.Intent{Ref: terms.Ref, At: terms.CreatedAt, Candidate: wheel.Candidate{Quote: wheel.Quote{
			Option: wheel.Option{ID: terms.Code, Symbol: "AAPL", Kind: "put", Expiry: "2026-10-30", Strike: 10000, Quantity: 1}, Bid: terms.LimitCents}}}
		if err := s.Save(st); err != nil {
			t.Fatal(err)
		}
		s.Close()
		reopened, err := OpenState(dir, c)
		if err != nil {
			t.Fatal(err)
		}
		reader := NewClient(evidenceCredential(t))
		reader.http.Transport = reviewTransport(func(r *http.Request) (*http.Response, error) {
			body := `{"s":"ok","d":{"orders":[],"page_flag":"","completed":true}}`
			if r.URL.Path == "/api/v1.0/accounts/authorized_trd_accs" {
				body = `{"s":"ok","d":{"accounts":[{"account_id":9007199254740993,"security_firm":"FUTUINC","acc_type":"margin","enable_market":[2]}]}}`
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		})
		b := &Broker{Store: reopened, Reader: reader, Config: c}
		err = b.recoverIntents(t.Context(), mustJournal(t, reopened))
		_, kept := mustJournal(t, reopened).Intents["AAPL"]
		if rejected && (err != nil || kept || reopened.CheckAttemptFence() != nil) {
			t.Fatal("a proven provider rejection still fenced trading", err)
		}
		if !rejected && (err == nil || !kept || reopened.CheckAttemptFence() == nil) {
			t.Fatal("absence alone released a possibly sent order")
		}
		reopened.Close()
	}
}

func TestUnreadableCurrencyBalancesBlockButBlankOnesDoNot(t *testing.T) {
	base := `"currency":"USD","us_cash":"100","us_avl_withdrawal_cash":"100","us_net_cash_power":"100","frozen_cash":"0","pending_asset":"0","hk_cash":"0","jp_cash":"0","cn_cash":"0","sg_cash":"0","kr_cash":"","my_cash":""`
	for doc, unreadable := range map[string]bool{
		`{` + base + `,"au_cash":""}`:                      false,
		`{` + base + `,"au_cash":"-5x"}`:                   true,
		`{` + base + `,"au_cash":"","zz_cash":"-1"}`:       true,
		`{` + base + `,"au_cash":"0.0000000000001234567"}`: true,
	} {
		cash, err := mapCashInputs(sourceAccount, time.Now(), []byte(doc))
		if err != nil || (len(cash.UnreadableCurrencyFields) != 0) != unreadable {
			t.Fatal("currency balance classification", doc, cash, err)
		}
	}
}
