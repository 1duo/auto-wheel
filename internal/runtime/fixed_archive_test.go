package runtime

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"autowheel/internal/contractpolicy"
	"autowheel/internal/wheel"
)

func fixedJournalFixture(t *testing.T) (*Journal, *wheel.State, *wheel.FixedController, wheel.FixedPolicySpec, wheel.FixedRoster, time.Time) {
	t.Helper()
	j, state, err := OpenJournal(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	state.Account = "12345678"
	at := time.Date(2026, 10, 19, 15, 29, 0, 0, wheel.Eastern)
	spec := wheel.FixedPolicySpec{ID: wheel.FixedConservativePolicy, Account: state.Account,
		Limits:          contractpolicy.CapitalLimits{Account: 8_000_000, Symbol: 2_666_666, Reserve: 1_680_944},
		OptionFeeBudget: 100, PhysicalFeeBudget: 100, MaxQuoteAge: 30,
		FeeSourceSHA: strings.Repeat("a", 64), FeeKnownAt: at.Add(-24 * time.Hour)}
	c, err := wheel.NewFixedController(spec, &state, j)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Bind(wheel.Snapshot{Account: state.Account, At: at.Add(-time.Hour)}, at.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	roster := wheel.FixedRoster{Symbols: []string{"MSFT", "AAPL"}, SourceSHA: strings.Repeat("b", 64), KnownAt: at.Add(-time.Hour), From: "2026-01-01", Through: "2026-12-31"}
	if err = c.Begin(roster, at); err != nil {
		t.Fatal(err)
	}
	return j, &state, c, spec, roster, at
}

func TestFixedJournalRestartAndImmutableArchive(t *testing.T) {
	j, state, c, spec, roster, at := fixedJournalFixture(t)
	dir := filepath.Dir(j.path)
	put := contractpolicy.Contract{ID: "MSFT-put", Symbol: "MSFT", Kind: "put", Expiry: "2026-11-20", Strike: 9000}
	e := wheel.FixedSignalEvidence{Contract: put, Menu: []contractpolicy.Contract{put},
		Stock: contractpolicy.Book{Bid: 9999, Ask: 10001}, Option: contractpolicy.Book{Bid: 100, Ask: 110},
		BidAt: at, AskAt: at, OptionAt: at, BidSize: 1, Tick: 5}
	if err := c.Record("MSFT", e, at); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, loaded, err := OpenJournal(dir)
	if err != nil {
		t.Fatal("new policy journal failed real reopen", err)
	}
	defer reopened.Close()
	state = &loaded
	c, err = wheel.NewFixedController(spec, state, reopened)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Recover(at.Add(20 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if state.Fixed.Batch.Signals[1].Reason != "capture_interrupted" {
		t.Fatal("crash recovery invented a missing signal")
	}
	if _, err = c.Claim("MSFT", at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	pin, err := reopened.ArchiveFixedBatch(*state.Fixed.Batch)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := reopened.ArchiveFixedBatch(*state.Fixed.Batch); err != nil || again != pin {
		t.Fatal("immutable archive was not idempotent", err)
	}
	path := filepath.Join(dir, "fixed-signal-archive", state.Fixed.Batch.Day+"-"+pin+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("archive is not private", err)
	}
	// Simulate external corruption only inside this disposable test directory.
	if err = os.WriteFile(path, append(raw, ' '), 0600); err != nil {
		t.Fatal(err)
	}
	if err = c.Begin(roster, at.AddDate(0, 0, 7)); err == nil || state.Fixed.Batch.Day != "2026-10-19" {
		t.Fatal("corrupted archive allowed old cohort replacement")
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, append(raw, ' ')) {
		t.Fatal("archive corruption was silently overwritten", err)
	}
	// The old strict schema must fail closed on a journal it cannot manage.
	type legacyState struct {
		Version         int                          `json:"version"`
		Account         string                       `json:"account_number,omitempty"`
		Positions       map[string]wheel.Position    `json:"positions"`
		Intents         map[string]wheel.Intent      `json:"intents"`
		CompletedOrders map[string]wheel.OrderRecord `json:"completed_orders,omitempty"`
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	d := json.NewDecoder(bytes.NewReader(encoded))
	d.DisallowUnknownFields()
	var old legacyState
	if err = d.Decode(&old); err == nil || !strings.Contains(err.Error(), "fixed_policy") {
		t.Fatal("old binary accepted a new policy journal")
	}
}

func TestFixedArchiveRejectsSymlinkDirectory(t *testing.T) {
	j, state, c, _, _, at := fixedJournalFixture(t)
	defer j.Close()
	for _, symbol := range []string{"MSFT", "AAPL"} {
		if err := c.Abstain(symbol, "source_unavailable", at); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Seal(at); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(filepath.Dir(j.path), "fixed-signal-archive")); err != nil {
		t.Fatal(err)
	}
	if _, err := j.ArchiveFixedBatch(*state.Fixed.Batch); err == nil {
		t.Fatal("archive directory symlink accepted")
	}
}

func TestJournalRejectsOversizeStateBeforeReplace(t *testing.T) {
	j, st, err := OpenJournal(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	st.Account = "12345678"
	if err = j.Save(st); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(j.path)
	if err != nil {
		t.Fatal(err)
	}
	st.Positions["AMD"] = wheel.Position{OwnedShares: 100, Cost: 10000, Provenance: strings.Repeat("x", 1<<20)}
	if err = j.Save(st); err == nil {
		t.Fatal("wrote a journal that restart cannot read")
	}
	f, err := os.Open(j.path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	after, err := io.ReadAll(f)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("failed save replaced the last restartable journal", err)
	}
}
