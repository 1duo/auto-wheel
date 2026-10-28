package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"autowheel/internal/contractpolicy"
	"autowheel/internal/moomoo"
	aruntime "autowheel/internal/runtime"
	"autowheel/internal/wheel"
)

// Reviewed Moomoo fee allowances: one-contract option orders measured at
// $2.98–$3.02, assignment/expiration legs free, call-away sales under $1.
const (
	moomooOptionFeeBudget   wheel.Cents = 400
	moomooPhysicalFeeBudget wheel.Cents = 1000
)

type rosterFlags struct {
	symbols, source, knownAt, from, through string
}

func (r rosterFlags) roster() (wheel.FixedRoster, error) {
	known, err := time.Parse(time.RFC3339Nano, r.knownAt)
	if err != nil || r.symbols == "" {
		return wheel.FixedRoster{}, errors.New("execution configuration requires the explicit reviewed roster, its source SHA256 and known-at time")
	}
	return wheel.FixedRoster{Symbols: strings.Split(r.symbols, ","), SourceSHA: r.source, KnownAt: known.UTC(), From: r.from, Through: r.through}, nil
}

// configureExecution binds the five-slot conservative ASAP policy to this
// account. The caller holds the state lock with the service stopped. It writes
// HALT first; it never requests trading consent or activates trading.
func configureExecution(ctx context.Context, configPath, dir string, c moomoo.Config, client *moomoo.Client, flags rosterFlags) error {
	if c.AccountID == "" || c.Strategy.FixedPolicy != nil {
		return errors.New("execution configuration requires a sealed account and no existing financial policy")
	}
	for _, name := range []string{"LIVE.json", "journal.json", "moomoo-trading.token.json"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			return errors.New("execution configuration requires no activation, trading journal or trading credential")
		}
	}
	roster, err := flags.roster()
	if err != nil {
		return err
	}
	if err := aruntime.AtomicJSON(filepath.Join(dir, "HALT"), map[string]string{"reason": "execution policy configuration requires native activation"}); err != nil {
		return err
	}
	audit, err := client.FullOrderAudit(ctx, c.AccountID)
	if err != nil {
		return err
	}
	if err := aruntime.AtomicJSON(filepath.Join(dir, "moomoo-cutoff-order-audit.json"), audit); err != nil {
		return err
	}
	fees, err := client.CollectFeeEvidence(ctx, c.AccountID, append(slices.Clone(audit.Recent), audit.History...), moomooOptionFeeBudget)
	if err != nil {
		return err
	}
	if err := aruntime.AtomicJSON(filepath.Join(dir, "moomoo-fee-evidence.json"), fees); err != nil {
		return err
	}
	ceiling := c.Strategy.MaxCollateral
	if ceiling <= 0 || ceiling > wheel.MaxMoney {
		return errors.New("execution requires the existing explicit capital ceiling")
	}
	next := c
	next.RefreshSeconds = 30 // Three snapshots per cycle stay below 10 order reads per 30 seconds.
	s := &next.Strategy
	s.Symbols, s.MaxSymbols, s.SymbolLimitScope = slices.Clone(roster.Symbols), 5, "managed"
	s.Fee, s.MaxQuoteAgeSeconds, s.StrategyPreset, s.SelectionMode = moomooOptionFeeBudget, 30, "", ""
	s.OrderCoveragePolicy, s.ExclusiveOrdersSince = wheel.CompleteMoomooOrders, audit.At
	s.FixedPolicy = &wheel.FixedPolicyConfig{Roster: roster, Spec: wheel.FixedPolicySpec{ID: wheel.FixedConservativeASAPPolicy, Account: c.AccountID,
		Limits:          contractpolicy.CapitalLimits{Account: int64(ceiling), Symbol: int64(ceiling), Reserve: int64(s.Reserve), Slots: 5},
		OptionFeeBudget: moomooOptionFeeBudget, PhysicalFeeBudget: moomooPhysicalFeeBudget, MaxQuoteAge: 30, FeeSourceSHA: fees.SHA256, FeeKnownAt: fees.At}}
	next.ExecutionPolicy = wheel.FixedConservativeASAPPolicy
	if err := next.Validate(); err != nil {
		return err
	}
	if _, err := moomoo.NewSource(client, next); err != nil {
		return err
	}
	return aruntime.AtomicJSON(configPath, next)
}

type executionAudit struct {
	At       time.Time         `json:"at"`
	Release  string            `json:"release"`
	Snapshot *wheel.Snapshot   `json:"snapshot,omitempty"`
	Error    string            `json:"snapshot_error,omitempty"`
	Symbols  []symbolAudit     `json:"symbols"`
	Policy   string            `json:"policy_id"`
	Limits   map[string]string `json:"limits"`
}

type symbolAudit struct {
	Symbol   string                   `json:"symbol"`
	Error    string                   `json:"error,omitempty"`
	Contract *contractpolicy.Contract `json:"contract,omitzero"`
	Stock    contractpolicy.Book      `json:"stock_book"`
	Option   contractpolicy.Book      `json:"option_book"`
	Tick     wheel.Cents              `json:"tick_cents"`
	Menu     int                      `json:"menu_contracts"`
	Review   *moomoo.ExecutionReview  `json:"review,omitzero"`
}

// auditExecution runs every production read the engine would use, including
// the account-capacity review, and never prepares, persists or sends an order.
func auditExecution(ctx context.Context, dir string, c moomoo.Config, client *moomoo.Client, store *moomoo.State, only string) error {
	source, err := moomoo.NewSource(client, c)
	if err != nil {
		return err
	}
	if only != "" && !slices.Contains(c.Strategy.FixedPolicy.Roster.Symbols, only) {
		return errors.New("execution audit symbol is not on the bound roster")
	}
	owned, err := store.JournalState()
	if err != nil {
		return err
	}
	report := executionAudit{At: time.Now().UTC(), Release: version, Policy: c.ExecutionPolicy, Limits: map[string]string{
		"account": wheel.Cents(c.Strategy.FixedPolicy.Spec.Limits.Account).Dollars(), "reserve": wheel.Cents(c.Strategy.FixedPolicy.Spec.Limits.Reserve).Dollars(),
		"option_fee_budget": c.Strategy.FixedPolicy.Spec.OptionFeeBudget.Dollars()}}
	snapshot, err := source.Snapshot(ctx, owned)
	if err != nil {
		report.Error = err.Error()
	} else {
		report.Snapshot = &snapshot
	}
	for i, symbol := range c.Strategy.FixedPolicy.Roster.Symbols {
		if only != "" && symbol != only {
			continue
		}
		if i > 0 && only == "" {
			// Stay under Moomoo's ten chain/capacity reads per 30 seconds.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(7 * time.Second):
			}
		}
		entry := symbolAudit{Symbol: symbol}
		if report.Snapshot != nil && slices.Contains(report.Snapshot.ExternalSymbols, symbol) {
			entry.Error = "external holding quarantines this symbol"
			report.Symbols = append(report.Symbols, entry)
			continue
		}
		signal, err := source.Observe(ctx, "put", symbol, time.Now)
		if err != nil {
			entry.Error = err.Error()
			report.Symbols = append(report.Symbols, entry)
			continue
		}
		entry.Contract, entry.Stock, entry.Option, entry.Tick, entry.Menu = &signal.Contract, signal.Stock, signal.Option, signal.Tick, len(signal.Menu)
		candidate := wheel.Candidate{Quote: wheel.Quote{Option: wheel.Option{ID: signal.Contract.ID, ChainID: "US." + symbol, Symbol: symbol, Kind: "put",
			Expiry: signal.Contract.Expiry, Strike: wheel.Cents(signal.Contract.Strike), Quantity: 1}, Bid: wheel.Cents(signal.Option.Bid), Ask: wheel.Cents(signal.Option.Ask)}}
		if candidate.Bid > 0 {
			review, err := source.Review(ctx, candidate)
			if err != nil {
				entry.Error = err.Error()
			} else {
				entry.Review = &review
			}
		} else {
			entry.Error = "no bid for the selected contract"
		}
		report.Symbols = append(report.Symbols, entry)
	}
	if err := aruntime.AtomicJSON(filepath.Join(dir, "moomoo-execution-audit.json"), report); err != nil {
		return err
	}
	ready := 0
	for _, s := range report.Symbols {
		if s.Review != nil {
			ready++
		}
	}
	fmt.Printf("Read-only execution audit saved privately: snapshot ok=%t, %d of %d roster symbols reviewed. No order was prepared or sent.\n", report.Snapshot != nil, ready, len(report.Symbols))
	if report.Snapshot == nil {
		return errors.New(report.Error)
	}
	return nil
}
