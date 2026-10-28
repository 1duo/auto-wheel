package moomoo

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"slices"
	"time"

	"autowheel/internal/wheel"
)

// AccountEvidence is a private read-only qualification input, not a wheel
// Snapshot. It retains the provider's exact funds/positions fields and
// decimals verbatim and never claims execution qualification.
type AccountEvidence struct {
	AccountID          string          `json:"account_id"`
	Account            Account         `json:"account"`
	At                 time.Time       `json:"at"`
	FinishedAt         time.Time       `json:"finished_at"`
	Source             string          `json:"source"`
	Funds              json.RawMessage `json:"funds_raw"`
	Positions          json.RawMessage `json:"positions_raw"`
	FundsBytes         []byte          `json:"funds_bytes"`
	PositionsBytes     []byte          `json:"positions_bytes"`
	AvailableFunds     *wheel.Cents    `json:"available_cash_cents,omitempty"`
	ReportedFunds      string          `json:"reported_funds"`
	Holdings           []Holding       `json:"holdings"`
	ExecutionQualified bool            `json:"execution_qualified"`
	CoverageWarning    string          `json:"coverage_warning"`
	CashInputs         *CashInputs     `json:"cash_inputs,omitempty"`
	CashInputError     string          `json:"cash_input_error,omitempty"`
}

const accountCoverageWarning = "Non-atomic observation only. Settled cash, order coverage, option deliverables, effective ownership and assignment/settlement reporting are not qualified."

// Observation exposes the display-only facts from the same read window.
func (e *AccountEvidence) Observation() *AccountInfo {
	return &AccountInfo{At: e.At, Last4: e.Account.Last4, OptionsLevel: "unverified", AvailableFunds: e.AvailableFunds, ReportedFunds: e.ReportedFunds,
		BalanceLabel: "Reported available funds", Holdings: slices.Clone(e.Holdings), HoldingsScope: "all_positions", HoldingsComplete: true,
		AccountType: e.Account.Type, CoverageWarning: e.CoverageWarning}
}

// AuditAccount collects fresh authorized US account evidence for qualification
// review. It performs an exact US-account authorization before any read, then
// fetches pinned official USD funds and unfiltered positions. Both payloads
// must parse exactly before any evidence is returned; no partial packet is
// ever produced. This is evidence only: no tokens, credentials or control
// writes occur, and nothing here qualifies execution.
func (c *Client) AuditAccount(ctx context.Context, accountID string) (*AccountEvidence, error) {
	started := time.Now().UTC()
	a, err := c.authorizedUSAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	funds, err := c.readGET(ctx, "/api/v1.0/accounts/"+accountID+"/funds", url.Values{"currency": {"USD"}})
	if err != nil {
		return nil, err
	}
	available, err := parseFunds(funds)
	if err != nil {
		return nil, err
	}
	positions, err := c.readGET(ctx, "/api/v1.0/accounts/"+accountID+"/positions", nil)
	if err != nil {
		return nil, err
	}
	holdings, err := parseHoldings(positions)
	if err != nil {
		return nil, err
	}
	evidence := &AccountEvidence{AccountID: accountID, Account: a, At: started, FinishedAt: time.Now().UTC(),
		Source: restEndpoint, Funds: funds, Positions: positions, AvailableFunds: available.Cents, ReportedFunds: available.Text, Holdings: holdings,
		FundsBytes: bytes.Clone(funds), PositionsBytes: bytes.Clone(positions),
		ExecutionQualified: false, CoverageWarning: accountCoverageWarning}
	// Incomplete collateral facts must not suppress independent display holdings
	// or silently turn unavailable fields into zero balances.
	evidence.CashInputs, err = mapCashInputs(accountID, started, funds)
	if err != nil {
		evidence.CashInputError = err.Error()
	}
	return evidence, nil
}
