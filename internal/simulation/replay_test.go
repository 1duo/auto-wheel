package simulation

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"autowheel/internal/wheel"
)

func TestReplayPhysicalWheelAndBelowCostExit(t *testing.T) {
	r := Replay(AccountingFixture())
	if r.Status != "fixture" || r.Performance == nil {
		t.Fatalf("replay blocked: %+v", r.Blockers)
	}
	m := r.Performance
	if m.Fills != 2 || m.Assignments != 1 || m.CallAways != 1 || m.Premiums != 40000 || m.Fees != 200 || m.StockRealized != -50000 || m.NetPnL != -10200 || m.EndingEquity != 7989800 {
		t.Fatalf("accounting: %+v", m)
	}
	if len(r.EndState.Positions) != 0 || r.ObservedDecisions != 370 || r.ObservedValuations != 5 {
		t.Fatalf("ownership/coverage: %+v", r)
	}
	for _, trade := range r.Trades {
		if trade.Action == "sell_call" && (trade.OptionID != "fixture-NVDA-call" || trade.At.In(time.FixedZone("EDT", -4*3600)).Day() != 8) {
			t.Fatalf("delta priority or 24h delay lost: %+v", trade)
		}
	}
}

func mutateFixture(t *testing.T, change func(int, map[string]any)) io.Reader {
	t.Helper()
	source, _ := io.ReadAll(AccountingFixture())
	var out bytes.Buffer
	for i, line := range bytes.Split(bytes.TrimSpace(source), []byte{'\n'}) {
		var v map[string]any
		if err := json.Unmarshal(line, &v); err != nil {
			t.Fatal(err)
		}
		change(i, v)
		if err := json.NewEncoder(&out).Encode(v); err != nil {
			t.Fatal(err)
		}
	}
	return &out
}

func TestReplayRejectsMissingAndFutureData(t *testing.T) {
	for _, name := range []string{"missing_decision", "future_oi", "incomplete_chain", "corporate_action", "missing_listed_quote"} {
		t.Run(name, func(t *testing.T) {
			input := mutateFixture(t, func(i int, v map[string]any) {
				if i != 1 {
					return
				}
				switch name {
				case "missing_decision":
					v["kind"] = "market"
				case "future_oi":
					v["quotes"].([]any)[0].(map[string]any)["open_interest_published_at"] = "2026-04-07T09:35:00-04:00"
				case "incomplete_chain":
					v["complete_chain_symbols"] = []string{"NVDA"}
				case "corporate_action":
					v["unsupported_corporate_action"] = "unverified split"
				case "missing_listed_quote":
					v["quotes"] = v["quotes"].([]any)[1:] // Full roster still requires this ID.
				}
			})
			r := Replay(input)
			if r.Status != "blocked" || r.Performance != nil || len(r.Equity) > 0 || len(r.Trades) > 0 || len(r.Blockers) == 0 {
				t.Fatalf("invalid history produced returns: %+v", r)
			}
		})
	}
}

func TestReplayMarksOpenShortAtAsk(t *testing.T) {
	input := mutateFixture(t, func(i int, v map[string]any) {
		if i == 0 {
			v["end_date"] = "2026-04-06"
		}
		delete(v, "lifecycles")
		if v["kind"] == "valuation" {
			for _, q := range v["quotes"].([]any) {
				quote := q.(map[string]any)
				if quote["id"] == "fixture-NVDA-put" {
					quote["bid_cents"] = 300
					quote["ask_cents"] = 310
				}
			}
		}
	})
	data, _ := io.ReadAll(input)
	var b bytes.Buffer
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte{'\n'}) {
		if strings.Contains(string(line), "2026-04-07") {
			break
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	r := Replay(&b)
	if r.Performance == nil || r.Performance.Premiums != 10000 || r.Performance.NetPnL != -21100 || r.Equity[0].OptionLiability != 31000 {
		t.Fatalf("premium credited as profit or liability omitted: %+v", r)
	}
}

func TestReplayProtectsPeerCashFromForcedDebit(t *testing.T) {
	r := replay{cash: 50000, report: Report{Spec: Spec{PeerReserve: 40000}}}
	if r.changeCash(-10001) == nil || r.cash != 50000 {
		t.Fatal("a forced close spent peer funds")
	}
}

func TestReplayGFDCanFillAfterEntryCutoff(t *testing.T) {
	var spec Spec
	if err := json.NewDecoder(AccountingFixture()).Decode(&spec); err != nil {
		t.Fatal(err)
	}
	r := replay{report: Report{Spec: spec}}
	if err := r.init(); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 4, 6, 15, 40, 0, 0, wheel.Eastern)
	r.now = at
	f := fixtureFrame(at, "decision")
	for _, q := range f.Quotes {
		r.quotes[q.ID] = q
	}
	// Keep an existing, admitted GFD order working beyond the daemon's entry
	// cutoff. Production has no automatic cancellation at 15:45.
	q := f.Quotes[0].Quote
	q.Spot = 10500
	a, ok := wheel.Select(spec.Config, "NVDA", "put", []wheel.Quote{q}, 0, at)
	if !ok {
		t.Fatal("fixture candidate absent")
	}
	if _, err := (simBroker{r: &r}).Submit(t.Context(), a, "test"); err != nil {
		t.Fatal(err)
	}
	for second := 0; second <= 5; second++ {
		r.now = time.Date(2026, 4, 6, 15, 50, second, 0, wheel.Eastern)
		if second == 0 {
			r.pending.LastQuote = r.now.Add(-time.Second)
		}
		if wheel.TradingWindow(r.now) {
			t.Fatal("expected entry cutoff")
		}
		if err := r.fill(fixtureFrame(r.now, "market")); err != nil {
			t.Fatal(err)
		}
	}
	if r.metrics.Fills != 1 {
		t.Fatal("incorrectly canceled GFD at the strategy entry cutoff")
	}
}

func TestReplayRequiresSustainedBidAndGlobalIntentFence(t *testing.T) {
	var spec Spec
	if err := json.NewDecoder(AccountingFixture()).Decode(&spec); err != nil {
		t.Fatal(err)
	}
	r := replay{report: Report{Spec: spec}}
	if err := r.init(); err != nil {
		t.Fatal(err)
	}
	at := r.decisions[0]
	f := fixtureFrame(at, "decision")
	f.CompleteSymbols = spec.Config.Symbols
	if err := r.step(f); err != nil {
		t.Fatal(err)
	}
	for second := 1; second <= 6; second++ {
		f := fixtureFrame(at.Add(time.Duration(second)*time.Second), "market")
		for i := range f.Quotes {
			f.Quotes[i].BidSize = 0
		}
		if err := r.step(f); err != nil {
			t.Fatal(err)
		}
	}
	if r.metrics.Fills != 0 || r.pending == nil {
		t.Fatal("filled without a sized bid")
	}
	// Isolate Plan's global intent fence here; a full stream must separately
	// provide every execution observation through this next decision.
	r.now = at.Add(5 * time.Minute)
	f = fixtureFrame(r.now, "decision")
	if err := r.decide(f); err != nil {
		t.Fatal(err)
	}
	if r.metrics.Submissions != 1 || r.metrics.BlockedCycles != 1 {
		t.Fatal("retried a pending order")
	}
	r.expireOrder()
	if err := wheel.Reconcile(&r.state, r.snapshot(), r.now); err != nil {
		t.Fatal(err)
	}
	if len(r.state.Intents) != 0 || r.metrics.ExpiredOrders != 1 {
		t.Fatal("GFD terminal outcome not reconciled")
	}
}

func TestReplayRejectsMissingExecutionCoverage(t *testing.T) {
	input := mutateFixture(t, func(i int, v map[string]any) {
		if i == 2 || i == 3 {
			var quotes []any
			for _, q := range v["quotes"].([]any) {
				if q.(map[string]any)["id"] != "fixture-NVDA-put" {
					quotes = append(quotes, q)
				}
			}
			v["quotes"] = quotes
		}
	})
	r := Replay(input)
	if r.Status != "blocked" || r.Performance != nil || !strings.Contains(r.Blockers[0], "execution NBBO coverage") {
		t.Fatalf("source gap became no-fill: %+v", r.Blockers)
	}
}

func TestReplayPeerReservationsAndNoPeerCalls(t *testing.T) {
	for _, symbolReservation := range []bool{false, true} {
		input := mutateFixture(t, func(i int, v map[string]any) {
			if i == 0 {
				v["end_date"] = "2026-04-06"
				if symbolReservation {
					v["peer_reserved_symbols"] = []string{"NVDA"}
				} else {
					v["peer_reserved_cash_cents"] = 7500000
				}
			}
			delete(v, "lifecycles")
		})
		data, _ := io.ReadAll(input)
		var b bytes.Buffer
		for _, line := range bytes.Split(bytes.TrimSpace(data), []byte{'\n'}) {
			if strings.Contains(string(line), "2026-04-07") {
				break
			}
			b.Write(line)
			b.WriteByte('\n')
		}
		r := Replay(&b)
		if r.Status != "fixture" || r.Performance == nil || r.Performance.Submissions != 0 {
			t.Fatalf("peer reservation ignored: %+v", r.Blockers)
		}
	}
}
