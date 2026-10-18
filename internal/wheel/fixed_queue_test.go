package wheel

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"autowheel/internal/contractpolicy"
)

type fixedMemoryJournal struct {
	raw         []byte
	failSave    bool
	failArchive bool
	archives    int
}

func (m *fixedMemoryJournal) Save(s State) error {
	if m.failSave {
		return errors.New("injected journal sync failure")
	}
	if err := s.Validate(); err != nil {
		return err
	}
	b, err := json.Marshal(s)
	m.raw = b
	return err
}

func (m *fixedMemoryJournal) ArchiveFixedBatch(b FixedBatch) (string, error) {
	if m.failArchive {
		return "", errors.New("injected immutable archive failure")
	}
	for _, entry := range b.Signals {
		if entry.Phase == "ready" || entry.Phase == "unobserved" {
			return "", errors.New("nonterminal archive")
		}
	}
	m.archives++
	return fixedDigest(b), nil
}

func fixedQueueFixture(t *testing.T) (*FixedController, *State, *fixedMemoryJournal, time.Time, FixedRoster) {
	t.Helper()
	at := time.Date(2026, 10, 19, 15, 29, 0, 0, Eastern)
	spec := FixedPolicySpec{ID: FixedConservativePolicy, Account: "12345678",
		Limits:          contractpolicy.CapitalLimits{Account: 8_000_000, Symbol: 2_666_666, Reserve: 1_680_944},
		OptionFeeBudget: 100, PhysicalFeeBudget: 100, MaxQuoteAge: 30,
		FeeSourceSHA: strings.Repeat("a", 64), FeeKnownAt: at.Add(-24 * time.Hour)}
	st := new(NewState())
	st.Account = spec.Account
	store := &fixedMemoryJournal{}
	c, err := NewFixedController(spec, st, store)
	if err != nil {
		t.Fatal(err)
	}
	bound := at.Add(-time.Hour)
	if err = c.Bind(Snapshot{Account: st.Account, At: bound}, bound); err != nil {
		t.Fatal(err)
	}
	roster := FixedRoster{Symbols: []string{"MSFT", "AAPL"}, SourceSHA: strings.Repeat("b", 64), KnownAt: at.Add(-time.Hour), From: "2026-01-01", Through: "2026-12-31"}
	return c, st, store, at, roster
}

func fixedQueueEvidence(symbol, kind string, at time.Time) FixedSignalEvidence {
	strike := int64(9000)
	if kind == "call" {
		strike = 10500
	}
	chosen := contractpolicy.Contract{ID: symbol + "-" + kind, Symbol: symbol, Kind: kind, Expiry: "2026-11-20", Strike: strike}
	return FixedSignalEvidence{Contract: chosen, Menu: []contractpolicy.Contract{chosen},
		Stock: contractpolicy.Book{Bid: 9999, Ask: 10001}, Option: contractpolicy.Book{Bid: 100, Ask: 110},
		BidAt: at, AskAt: at, OptionAt: at, BidSize: 1, Tick: 5}
}

func restartFixedQueue(t *testing.T, prior *FixedController, store *fixedMemoryJournal) (*FixedController, *State) {
	t.Helper()
	st := new(NewState())
	if err := json.Unmarshal(store.raw, st); err != nil {
		t.Fatal(err)
	}
	c, err := NewFixedController(prior.spec, st, store)
	if err != nil {
		t.Fatal(err)
	}
	return c, st
}

func TestFixedQueueRestartOrderAndSingleClaim(t *testing.T) {
	c, st, store, at, roster := fixedQueueFixture(t)
	if err := c.Begin(roster, at); err != nil {
		t.Fatal(err)
	}
	for _, symbol := range roster.Symbols {
		if err := c.Record(symbol, fixedQueueEvidence(symbol, "put", at), at); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Seal(at.Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	c, st = restartFixedQueue(t, c, store)
	if err := c.Begin(roster, at.Add(15*time.Second)); err == nil {
		t.Fatal("restart recaptured a later signal price")
	}
	if _, err := c.Claim("MSFT", at.Add(59*time.Second)); err == nil {
		t.Fatal("claimed before primary execution clock")
	}
	if _, err := c.Claim("AAPL", at.Add(time.Minute)); err == nil {
		t.Fatal("alphabetical/ROI sorting replaced frozen roster order")
	}
	claimed, err := c.Claim("MSFT", at.Add(time.Minute))
	if err != nil || claimed.Contract.ID != "MSFT-put" {
		t.Fatal("first claim failed", err)
	}
	claimed.Contract.Strike = 1
	if err = st.Validate(); err != nil {
		t.Fatal("returned claim aliases durable state", err)
	}
	c, st = restartFixedQueue(t, c, store)
	if _, err = c.Claim("MSFT", at.Add(2*time.Minute)); err == nil {
		t.Fatal("restart repeated an attempted signal")
	}
	q := st.Fixed.Batch.Signals[0].Contract
	st.Intents["MSFT"] = Intent{Ref: "durable-ref", At: at.Add(time.Minute), Candidate: Candidate{Quote: Quote{
		Option: Option{ID: q.ID, Symbol: q.Symbol, Kind: q.Kind, Strike: Cents(q.Strike), Expiry: q.Expiry, Quantity: 1}}}}
	if _, err = c.Claim("AAPL", at.Add(2*time.Minute)); err == nil {
		t.Fatal("unresolved submission did not freeze the account")
	}
	delete(st.Intents, "MSFT") // Model broker-confirmed reconciliation, not a retry.
	if _, err = c.Claim("AAPL", at.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
}

func TestFixedQueueInterruptedCaptureExpiryAndSaveFailure(t *testing.T) {
	c, _, store, at, roster := fixedQueueFixture(t)
	if err := c.Begin(roster, at); err != nil {
		t.Fatal(err)
	}
	if err := c.Record("MSFT", fixedQueueEvidence("MSFT", "put", at), at); err != nil {
		t.Fatal(err)
	}
	if err := c.Seal(at.Add(time.Second)); err == nil {
		t.Fatal("omitted source silently passed batch seal")
	}
	c, st := restartFixedQueue(t, c, store)
	if err := c.Recover(at.Add(20 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if st.Fixed.Batch.Signals[1].Reason != "capture_interrupted" {
		t.Fatal("interrupted source observation was recaptured")
	}
	if err := c.Record("AAPL", fixedQueueEvidence("AAPL", "put", at), at.Add(25*time.Second)); err == nil {
		t.Fatal("later price replaced an interrupted source")
	}
	store.failSave = true
	claim, err := c.Claim("MSFT", at.Add(time.Minute))
	if err == nil || claim.Contract != nil {
		t.Fatal("claim escaped failed durable save")
	}
	store.failSave = false
	if _, err = c.Claim("MSFT", at.Add(2*time.Minute)); err == nil {
		t.Fatal("save failure fence was bypassed")
	}
	c, st = restartFixedQueue(t, c, store) // Reopen the actual previous durable state.
	if err = c.Recover(at.Add(6 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	if st.Fixed.Batch.Signals[0].Phase != "expired" {
		t.Fatal("missed execution window left an order opportunity")
	}
	if _, err = c.Claim("MSFT", at.Add(6*time.Minute)); err == nil {
		t.Fatal("expired signal was claimed")
	}
	store.failArchive = true
	if err = c.Begin(roster, at.AddDate(0, 0, 7)); err == nil {
		t.Fatal("new cohort erased an unarchived old batch")
	}
	store.failArchive = false
	if err = c.Begin(roster, at.AddDate(0, 0, 7)); err != nil || store.archives != 1 || st.Fixed.Batch.PreviousSHA == "" {
		t.Fatal("weekly archive chain was not retained", err)
	}
}

func TestFixedQueueFlatBindingAndLegacyFence(t *testing.T) {
	c, st, store, at, roster := fixedQueueFixture(t)
	spec := c.spec
	for _, scenario := range []string{"stock", "option", "working_order", "old_journal", "account", "stale"} {
		t.Run(scenario, func(t *testing.T) {
			old := NewState()
			old.Account = spec.Account
			s := Snapshot{Account: spec.Account, At: at}
			switch scenario {
			case "stock":
				s.Shares = []Shares{{Symbol: "AVGO", Quantity: 100}}
			case "option":
				s.Options = []Option{{ID: "old", Symbol: "HOOD", Quantity: 1}}
			case "working_order":
				s.Orders = []Order{{State: "queued"}}
			case "old_journal":
				old.Positions["MSTR"] = Position{OwnedShares: 100, Cost: 11200, Provenance: "old ownership"}
			case "account":
				s.Account = "87654321"
			case "stale":
				s.At = at.Add(-31 * time.Second)
			}
			j := &fixedMemoryJournal{}
			next, err := NewFixedController(spec, &old, j)
			if err != nil {
				t.Fatal(err)
			}
			if next.Bind(s, at) == nil || old.Fixed != nil || len(j.raw) != 0 {
				t.Fatal("old lifecycle or incomplete account was adopted")
			}
		})
	}
	if err := c.Begin(roster, at); err != nil {
		t.Fatal(err)
	}
	if err := c.Recover(at.Add(6 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	st.Positions["BA"] = Position{OwnedShares: 100, Cost: 8000, Provenance: "autowheel put fill new-ref", PutOpenedAt: at.Add(time.Minute), InventoryAt: at.AddDate(0, 0, 2)}
	st.Positions["AMD"] = st.Positions["BA"]
	if err := c.Begin(roster, at.AddDate(0, 0, 7)); err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, entry := range st.Fixed.Batch.Signals {
		order = append(order, entry.Symbol)
	}
	if !slices.Equal(order, []string{"AMD", "BA", "MSFT", "AAPL"}) {
		t.Fatal("retained managed/stock roster ordering changed", order)
	}
	if _, err := Plan(DefaultConfig(), *st, Snapshot{}, nil, at); err == nil {
		t.Fatal("legacy plan accepted a fixed-policy journal")
	}
	if err := ReconcileForConfig(DefaultConfig(), st, Snapshot{}, at); err == nil {
		t.Fatal("legacy reconciliation accepted a fixed-policy journal")
	}
	bad := spec
	bad.OptionFeeBudget++
	if _, err := NewFixedController(bad, st, store); err == nil {
		t.Fatal("financial policy changed under existing inventory")
	}
}
