package wheel

import (
	"testing"
	"time"
)

func legacyImportFixture(t *testing.T, stock, call bool) (*FixedController, *State, *fixedMemoryJournal, Config, Snapshot, time.Time) {
	t.Helper()
	base, _, _, config, _, clock := fixedExecutionFixture(t)
	now := *clock
	opened := time.Date(2026, 10, 16, 14, 7, 0, 0, Eastern) // Legacy, outside the fixed weekly bucket.
	st := new(NewState())
	st.Account = config.Account
	st.Positions["MSFT"] = Position{PutID: "old-put", PutExpiry: "2026-10-23", PutStrike: 9000,
		PutOpenedAt: opened, Provenance: "autowheel put fill old-ref"}
	st.CompletedOrders = map[string]OrderRecord{"old-order": {Ref: "old-ref", Symbol: "MSFT", OptionID: "old-put", At: opened, Filled: 1}}
	s := Snapshot{Account: st.Account, At: now, OrderCoveragePolicy: "exclusive_standard", AvailableCash: 7_000_000,
		Options: []Option{{ID: "old-put", ChainID: "chain", Symbol: "MSFT", Kind: "put", Strike: 9000, Expiry: "2026-10-23", Quantity: 1}},
		Orders: []Order{{ID: "old-order", Ref: "old-ref", Symbol: "MSFT", OptionID: "old-put", Kind: "put", Strike: 9000,
			Quantity: 1, Filled: 1, State: "filled", Side: "sell", Effect: "open", CreatedAt: opened}}}
	if stock {
		st.Positions["MSFT"] = Position{OwnedShares: 100, Cost: 9000, PutOpenedAt: opened, InventoryAt: opened.Add(time.Hour),
			Provenance: "autowheel put fill old-ref; assignment inferred from settled broker inventory and no equity orders"}
		s.Options = nil
		s.Shares = []Shares{{Symbol: "MSFT", Quantity: 100, Available: 100, Cost: 9000}}
	}
	if call {
		p := st.Positions["MSFT"]
		p.CallID, p.CallExpiry = "old-call", "2026-10-23"
		st.Positions["MSFT"] = p
		st.CompletedOrders["call-order"] = OrderRecord{Ref: "call-ref", Symbol: "MSFT", OptionID: "old-call", At: opened.Add(2 * time.Hour), Filled: 1}
		s.Options = []Option{{ID: "old-call", ChainID: "chain", Symbol: "MSFT", Kind: "call", Strike: 10500, Expiry: "2026-10-23", Quantity: 1}}
		s.Orders = append(s.Orders, Order{ID: "call-order", Ref: "call-ref", Symbol: "MSFT", OptionID: "old-call", Kind: "call", Strike: 10500,
			Quantity: 1, Filled: 1, State: "filled", Side: "sell", Effect: "open", CreatedAt: opened.Add(2 * time.Hour)})
		s.Shares[0].Available = 0
	}
	digest, err := FixedLegacyDigest(*st)
	if err != nil {
		t.Fatal(err)
	}
	config.FixedPolicy.ImportLegacyStateSHA = digest
	store := &fixedMemoryJournal{}
	c, err := NewFixedController(base.spec, st, store)
	if err != nil {
		t.Fatal(err)
	}
	return c, st, store, config, s, now
}

func TestFixedImportOwnPutAndStockSurviveRestart(t *testing.T) {
	for _, kind := range []string{"put", "stock", "covered_call"} {
		t.Run(kind, func(t *testing.T) {
			c, st, store, config, s, now := legacyImportFixture(t, kind != "put", kind == "covered_call")
			before := st.Positions["MSFT"]
			if err := c.BindNative(config, s, now); err != nil {
				t.Fatal(err)
			}
			if st.Positions["MSFT"] != before || st.Fixed.Legacy.StateSHA != config.FixedPolicy.ImportLegacyStateSHA || st.Fixed.Batch != nil {
				t.Fatal("import rewrote the lifecycle or fabricated a signal")
			}
			c, st = restartFixedQueue(t, c, store)
			if err := c.BindNative(config, s, now); err != nil {
				t.Fatal("restart repeated or lost the import", err)
			}
			if err := c.ReconcileNative(config, s, now); err != nil || fixedDigest(st.Positions["MSFT"]) != fixedDigest(before) {
				t.Fatal("import did not preserve native reconciliation", err)
			}
			config.FixedPolicy.ImportLegacyStateSHA = ""
			if c.BindNative(config, s, now) == nil {
				t.Fatal("configuration stripped the durable import binding")
			}
		})
	}
}

func TestFixedImportRejectsUnprovenOrChangedInventory(t *testing.T) {
	for _, scenario := range []string{"no_binding", "changed_journal", "no_receipt", "unowned_option", "wrong_strike", "missing_option", "extra_shares", "put_stock_overlap", "working_order", "stale_account", "save_failure"} {
		t.Run(scenario, func(t *testing.T) {
			c, st, store, config, s, now := legacyImportFixture(t, false, false)
			switch scenario {
			case "no_binding":
				config.FixedPolicy.ImportLegacyStateSHA = ""
			case "changed_journal":
				p := st.Positions["MSFT"]
				p.PutStrike++
				st.Positions["MSFT"] = p
			case "no_receipt":
				delete(st.CompletedOrders, "old-order")
			case "unowned_option":
				s.Options[0].ID = "peer-option"
			case "wrong_strike":
				s.Options[0].Strike++
			case "missing_option":
				s.Options = nil
			case "extra_shares":
				s.Shares = []Shares{{Symbol: "AAPL", Quantity: 100, Available: 100, Cost: 9000}}
			case "put_stock_overlap":
				s.Shares = []Shares{{Symbol: "MSFT", Quantity: 100, Available: 100, Cost: 9000}}
			case "working_order":
				s.Orders[0].State = "confirmed"
			case "stale_account":
				s.At = now.Add(-31 * time.Second)
			case "save_failure":
				store.failSave = true
			}
			if c.BindNative(config, s, now) == nil || len(store.raw) != 0 {
				t.Fatal("invalid handover acquired durable authority")
			}
		})
	}
}

func TestFixedImportedPutAssignmentAndNextCoveredCall(t *testing.T) {
	c, st, store, config, s, now := legacyImportFixture(t, false, false)
	if err := c.BindNative(config, s, now); err != nil {
		t.Fatal(err)
	}
	now = time.Date(2026, 10, 23, 16, 1, 0, 0, Eastern)
	s.At, s.Options = now, nil
	s.Shares = []Shares{{Symbol: "MSFT", Quantity: 100, Available: 100, Cost: 9000}}
	if err := c.ReconcileNative(config, s, now); err != nil || st.Positions["MSFT"].OwnedShares != 0 {
		t.Fatal("import bypassed assignment reporting delay", err)
	}
	now = now.Add(24 * time.Hour)
	s.At = now
	if err := c.ReconcileNative(config, s, now); err != nil || st.Positions["MSFT"].OwnedShares != 100 {
		t.Fatal("own imported put could not become settled owned stock", err)
	}
	c, st = restartFixedQueue(t, c, store)
	now = time.Date(2026, 10, 26, 15, 29, 0, 0, Eastern)
	if err := c.Begin(config.FixedPolicy.Roster, now); err != nil {
		t.Fatal(err)
	}
	if err := c.Record("MSFT", fixedQueueEvidence("MSFT", "call", now), now); err != nil {
		t.Fatal("imported stock could not prepare a new policy covered call", err)
	}
	if st.Fixed.Batch.Signals[0].Kind != "call" || st.Fixed.Batch.Signals[0].AcquisitionCeiling != 900100 {
		t.Fatal("covered call lost its own assignment basis or ordering")
	}
	// The import exception is confined to this original lifecycle.
	p := st.Positions["MSFT"]
	p.PutOpenedAt = p.PutOpenedAt.Add(time.Minute)
	st.Positions["MSFT"] = p
	if st.Validate() == nil {
		t.Fatal("old import authorized a different, unsignalled lifecycle")
	}
}

func TestFixedCompatibilityPreviewCannotAuthorizeStaleExecution(t *testing.T) {
	c, _, _, config, b, now := fixedExecutionFixture(t)
	b.evidence.Market.BidAt = now.Add(-48 * time.Hour)
	b.evidence.Market.AskAt = now.Add(-48 * time.Hour)
	b.evidence.Market.OptionAt = now.Add(-48 * time.Hour)
	if _, err := c.PreviewNative(config, b.evidence, b.snapshotWithTime(), *now); err != nil {
		t.Fatal("old known books could not qualify non-trading compatibility", err)
	}
	if err := c.ExecuteNative(t.Context(), config, b, "MSFT", func() time.Time { return *now }); err == nil || b.submits != 0 {
		t.Fatal("compatibility preview let stale quotes reach financial transport", err)
	}
}

func TestFixedImportedStockAboveEntryBudgetCanBeCovered(t *testing.T) {
	for _, price := range []Cents{35000, 95000} {
		c, st, _, config, s, now := legacyImportFixture(t, true, false)
		p := st.Positions["MSFT"]
		p.Cost = price
		st.Positions["MSFT"] = p
		s.Shares[0].Cost, s.Orders[0].Strike = price, price
		digest, err := FixedLegacyDigest(*st)
		if err != nil {
			t.Fatal(err)
		}
		config.FixedPolicy.ImportLegacyStateSHA = digest
		if err := c.BindNative(config, s, now); err != nil {
			t.Fatal(err)
		}
		call := fixedQueueEvidence("MSFT", "call", now)
		call.Stock.Bid, call.Stock.Ask = int64(price)-1, int64(price)+1
		call.Contract.Strike = int64(price) * 11 / 10
		e := FixedExecutionEvidence{Market: call, ChainID: "chain", Deliverable100: true}
		if _, err := c.PreviewNative(config, e, s, now); err != nil {
			t.Fatal("entry ceilings prevented management of a verified inherited lot", err)
		}
		e.Market = fixedQueueEvidence("AAPL", "put", now)
		if _, err := c.PreviewNative(config, e, s, now); price == 95000 && err == nil {
			t.Fatal("legacy management exception funded a new put beyond the account ceiling")
		}
		s.AvailableCash = config.Reserve
		e.Market = call
		if _, err := c.PreviewNative(config, e, s, now); err == nil {
			t.Fatal("legacy management bypassed fee/reserve funding")
		}
	}
}

func (b *fixedExecutionBroker) snapshotWithTime() Snapshot {
	s := b.snapshot
	s.At = *b.now
	return s
}
