package moomoo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"autowheel/internal/contractpolicy"
	"autowheel/internal/wheel"
)

// This is a synthetic qualified broker, not a model of undocumented REST fields.
type executionFixtureSource struct {
	account string
	at      time.Time
	orders  []wheel.Order
	alter   func(*wheel.Snapshot)
}

func (s *executionFixtureSource) Snapshot(context.Context, wheel.State) (wheel.Snapshot, error) {
	snapshot := wheel.Snapshot{Account: s.account, At: s.at, OrderCoveragePolicy: wheel.CompleteMoomooOrders, AvailableCash: 8_000_000, Orders: s.orders}
	if s.alter != nil {
		s.alter(&snapshot)
	}
	return snapshot, nil
}
func (s *executionFixtureSource) Observe(_ context.Context, kind, symbol string, _ func() time.Time) (wheel.FixedSignalEvidence, error) {
	contract := contractpolicy.Contract{ID: "US." + symbol + s.at.AddDate(0, 0, 31).Format("060102") + "P100000", Symbol: symbol, Kind: kind, Expiry: s.at.AddDate(0, 0, 31).Format(time.DateOnly), Strike: 10000}
	return wheel.FixedSignalEvidence{Contract: contract, Menu: []contractpolicy.Contract{contract}, Stock: contractpolicy.Book{Bid: 12000, Ask: 12010}, Option: contractpolicy.Book{Bid: 200, Ask: 205}, BidAt: s.at, AskAt: s.at, OptionAt: s.at, BidSize: 1, Tick: 1}, nil
}
func (s *executionFixtureSource) Refresh(ctx context.Context, contract contractpolicy.Contract, clock func() time.Time) (wheel.FixedExecutionEvidence, error) {
	input, _ := s.Observe(ctx, contract.Kind, contract.Symbol, clock)
	return wheel.FixedExecutionEvidence{Market: input, ChainID: "US." + contract.Symbol, Deliverable100: true}, nil
}
func (s *executionFixtureSource) Review(_ context.Context, a wheel.Candidate) (ExecutionReview, error) {
	side := "SELL"
	if a.Closing {
		side = "BUY"
	}
	return ExecutionReview{At: s.at, AccountID: s.account, CandidateSHA256: candidateDigest(a), Side: side, FeeCents: 100, PhysicalFeeCents: 1000, Deliverable100: true, OptionsAuthorized: true}, nil
}
func (s *executionFixtureSource) CloseQuote(context.Context, wheel.State, string) (wheel.Candidate, error) {
	return wheel.Candidate{}, errors.New("no owned option in fixture")
}

func executionFixture(t *testing.T, outcome string) (*Execution, *State, *int) {
	t.Helper()
	// A future reviewed exchange session keeps the transport context alive;
	// the in-memory clock is injected into both native guards and the outbox.
	at := time.Now().In(wheel.Eastern)
	for day := 1; day <= 10; day++ {
		d := at.AddDate(0, 0, day)
		// ASAP openings are claimed only from close-30m.
		atCandidate := time.Date(d.Year(), d.Month(), d.Day(), 15, 31, 0, 0, wheel.Eastern)
		if opens, ok := wheel.ASAPEntryOpens(atCandidate); ok && !atCandidate.Before(opens) {
			at = atCandidate
			break
		}
	}
	if !wheel.TradingWindow(at) {
		t.Skip("no future reviewed calendar session")
	}
	c := DefaultConfig()
	c.AccountID, c.Strategy.Account = "123456", "123456"
	c.Strategy.Symbols = []string{"AAPL"}
	c.Strategy.OrderCoveragePolicy = wheel.CompleteMoomooOrders
	c.Strategy.ExclusiveOrdersSince = time.Now().Add(-time.Hour)
	c.Strategy.FixedPolicy = &wheel.FixedPolicyConfig{Spec: wheel.FixedPolicySpec{ID: wheel.FixedConservativeASAPPolicy, Account: c.AccountID, Limits: contractpolicy.CapitalLimits{Account: 8_000_000, Symbol: 8_000_000, Slots: 5}, OptionFeeBudget: 100, PhysicalFeeBudget: 1000, MaxQuoteAge: 30, FeeSourceSHA: strings.Repeat("a", 64), FeeKnownAt: time.Now().Add(-time.Hour)}, Roster: wheel.FixedRoster{Symbols: c.Strategy.Symbols, SourceSHA: strings.Repeat("b", 64), KnownAt: time.Now().Add(-time.Hour), From: at.Format(time.DateOnly), Through: at.AddDate(0, 0, 40).Format(time.DateOnly)}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	state, err := OpenState(dir, c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	state.clock = func() time.Time { return at }
	grant, _ := nativeGrant(t)
	source := &executionFixtureSource{account: c.AccountID, at: at}
	b := &Broker{Config: c, Store: state, Source: source, Writer: NewTradingClient(grant, c.AccountID), Clock: state.clock}
	execution := &Execution{Broker: b}
	activation := nativeActivation(t)
	activation.Binding.AccountID = c.AccountID
	activation.Binding.ConfigSHA256 = ConfigDigest(c)
	activation.CheckReady = execution.CheckReady
	b.Activation = activation
	hits := new(int)
	b.Writer.http.Transport = reviewTransport(func(req *http.Request) (*http.Response, error) {
		*hits++
		var body map[string]string
		if json.NewDecoder(req.Body).Decode(&body) != nil {
			t.Fatal("invalid prepared request")
		}
		var stored wheel.State
		raw, err := os.ReadFile(filepath.Join(dir, "journal.json"))
		if err != nil || json.Unmarshal(raw, &stored) != nil {
			t.Fatal("dispatch preceded durable engine intent")
		}
		intent := stored.Intents["AAPL"]
		if intent.Ref != body["remark"] || intent.OrderID != "" {
			t.Fatal("dispatch preceded its exact durable reference")
		}
		var attempted placementAttempt
		raw, err = os.ReadFile(filepath.Join(dir, "moomoo-order-attempts", intent.Ref+".json"))
		if err != nil || json.Unmarshal(raw, &attempted) != nil || attempted.Phase != "attempt_started" {
			t.Fatal("dispatch preceded durable attempt fence")
		}
		if outcome == "unknown" {
			return nil, errors.New("timeout")
		}
		source.orders = []wheel.Order{{ID: "broker-order", Ref: intent.Ref, Symbol: "AAPL", OptionID: intent.Candidate.ID, Kind: "put", Strike: 10000, Quantity: 1, State: "submitted", Side: "sell", Effect: "open", CreatedAt: at}}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"s":"ok","d":{"order_id":"broker-order"}}`))}, nil
	})
	return execution, state, hits
}

func TestExecutionNativeCyclePersistsBeforeSingleDispatch(t *testing.T) {
	e, state, hits := executionFixture(t, "ack")
	if err := e.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := e.Broker.Activation.Enable(); err != nil {
		t.Fatal(err)
	}
	if err := e.Cycle(t.Context()); err != nil {
		t.Fatal(err)
	}
	stored, err := state.JournalState()
	if err != nil {
		t.Fatal(err)
	}
	if *hits != 1 || stored.Intents["AAPL"].OrderID != "broker-order" || stored.Fixed == nil || !stored.Fixed.Replenishment.Initial {
		t.Fatal("native engine did not retain its exact acknowledgement and fresh policy")
	}
	view := e.View()
	if view.Broker == nil || view.Intents["AAPL"].OrderID != "broker-order" || len(view.Positions) != 0 {
		t.Fatal("native operator view did not retain the acknowledged intent")
	}
	if err := e.Cycle(t.Context()); !errors.Is(err, ErrAwaitingBrokerReconciliation) {
		t.Fatal(fmt.Sprintf("working opening should wait without a second dispatch: %v", err))
	}
	if *hits != 1 {
		t.Fatal("working opening was dispatched again")
	}
}

func TestExecutionUnknownAndOrphanFencesSurviveRestart(t *testing.T) {
	e, state, hits := executionFixture(t, "unknown")
	if err := e.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := e.Broker.Activation.Enable(); err != nil {
		t.Fatal(err)
	}
	if e.Cycle(t.Context()) == nil {
		t.Fatal("timeout became an acknowledgement")
	}
	if e.Cycle(t.Context()) == nil || *hits != 1 {
		t.Fatal("unknown order was replayed")
	}
	if state.CheckAttemptFence() == nil {
		t.Fatal("unknown durable attempt did not fence the engine")
	}
	dir := filepath.Dir(state.path)
	cfg := e.Broker.Config
	clock := state.clock
	state.Close()
	reopened, err := OpenState(dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopened.clock = clock
	if reopened.CheckAttemptFence() == nil {
		t.Fatal("restart lost the unknown outcome")
	}
}

func TestExecutionIndependentCoverageAndExternalQuarantine(t *testing.T) {
	for _, name := range []string{"quarantined", "owned-looking-shares", "wrong-account", "robinhood-coverage", "missing-coverage", "negative-cash", "future", "stale", "retirement"} {
		t.Run(name, func(t *testing.T) {
			e, state, hits := executionFixture(t, "ack")
			source := e.Broker.Source.(*executionFixtureSource)
			source.alter = func(snapshot *wheel.Snapshot) {
				snapshot.ExternalSymbols, snapshot.ExternalCapital = []string{"AAPL"}, 100_000
				switch name {
				case "owned-looking-shares":
					snapshot.Shares = []wheel.Shares{{Symbol: "AAPL", Quantity: 100, Available: 100, Cost: 10000}}
				case "wrong-account":
					snapshot.Account = "654321"
				case "robinhood-coverage":
					snapshot.OrderCoveragePolicy = "exclusive_standard"
				case "missing-coverage":
					snapshot.OrderCoveragePolicy = ""
				case "negative-cash":
					snapshot.AvailableCash = -1
				case "future":
					snapshot.At = snapshot.At.Add(time.Second)
				case "stale":
					snapshot.At = snapshot.At.Add(-31 * time.Second)
				}
			}
			if name == "retirement" {
				e.Broker.Config.Strategy.RetiredTradePulse = "/another/broker/seal"
			}
			err := e.Prepare(t.Context())
			if (err == nil) != (name == "quarantined") {
				t.Fatalf("coverage/ownership gate: %v", err)
			}
			if name == "quarantined" {
				if err := e.Broker.Activation.Enable(); err != nil {
					t.Fatal(err)
				}
				if err := e.Cycle(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			stored, err := state.JournalState()
			if err != nil || *hits != 0 || len(stored.Intents) != 0 || len(stored.Positions) != 0 {
				t.Fatal("external inventory or unqualified snapshot became managed ownership")
			}
		})
	}
}
