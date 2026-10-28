package moomoo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"autowheel/internal/wheel"
)

const restEndpoint = "https://webapi.moomoo.com"

// These are observation types, not execution snapshots. In particular,
// available_funds is not qualified as settled, unleveraged cash for orders.
type Account struct {
	ID       string `json:"account_id"`
	Firm     string `json:"security_firm"`
	Type     string `json:"acc_type"`
	Last4    string `json:"account_last4"`
	USMarket bool   `json:"us_market"`
}

type Holding struct {
	Symbol       string `json:"symbol"`
	Quantity     string `json:"quantity"`
	Available    string `json:"available"`
	Side         string `json:"side"`
	SecurityType string `json:"security_type,omitempty"`
	Currency     string `json:"currency"`
	Ownership    string `json:"ownership"`
}

type AccountInfo struct {
	At                      time.Time    `json:"at"`
	Last4                   string       `json:"account_last4"`
	OptionsLevel            string       `json:"options_level"`
	AvailableFunds          *wheel.Cents `json:"available_cash_cents,omitempty"`
	ReportedFunds           string       `json:"reported_funds"`
	BalanceLabel            string       `json:"balance_label"`
	Holdings                []Holding    `json:"holdings"`
	HoldingsScope           string       `json:"holdings_scope"`
	HoldingsComplete        bool         `json:"holdings_complete"`
	SnapshotComplete        bool         `json:"snapshot_complete"`
	ExecutionSnapshotUsable bool         `json:"execution_snapshot_usable"`
	CoverageWarning         string       `json:"coverage_warning"`
	AccountType             string       `json:"account_type"`
	HoldingsSourceTimeKnown bool         `json:"holdings_source_time_known"`
}

// readGET has no public arbitrary URL/method escape hatch. Every caller below
// uses a documented query endpoint on the pinned official API origin.
func (c *Client) readGET(ctx context.Context, path string, query url.Values) (json.RawMessage, error) {
	return c.readFrom(ctx, restEndpoint, path, query)
}

func (c *Client) readFrom(ctx context.Context, origin, path string, query url.Values) (json.RawMessage, error) {
	token, err := c.tokens.Access(ctx, c.http)
	if err != nil {
		return nil, err
	}
	u := origin + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, errors.New("invalid Moomoo observation request")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, errors.New("Moomoo observation transport failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Moomoo observation HTTP %d; no automatic replay", resp.StatusCode)
	}
	const limit = 8 << 20
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || len(raw) > limit {
		return nil, errors.New("Moomoo observation is truncated or exceeds limit")
	}
	var envelope struct {
		Status  string          `json:"s"`
		Data    json.RawMessage `json:"d"`
		NextKey json.RawMessage `json:"next_key"`
	}
	if err := exactJSON(raw, &envelope); err != nil || envelope.Status != "ok" || len(envelope.Data) == 0 || bytes.Equal(bytes.TrimSpace(envelope.Data), []byte("null")) {
		return nil, errors.New("Moomoo observation returned invalid or unavailable data")
	}
	if len(envelope.NextKey) != 0 && string(envelope.NextKey) != `""` && string(envelope.NextKey) != "null" {
		return nil, errors.New("Moomoo observation pagination is unqualified")
	}
	return envelope.Data, nil
}

func (c *Client) Accounts(ctx context.Context) ([]Account, error) {
	raw, err := c.readGET(ctx, "/api/v1.0/accounts/authorized_trd_accs", nil)
	if err != nil {
		return nil, err
	}
	return parseAccounts(raw)
}

func parseAccounts(raw []byte) ([]Account, error) {
	var data struct {
		Accounts []struct {
			ID     json.Number `json:"account_id"`
			Firm   string      `json:"security_firm"`
			Type   string      `json:"acc_type"`
			Card   string      `json:"account_card_number"`
			Market []int       `json:"enable_market"`
		} `json:"accounts"`
	}
	if err := exactJSON(raw, &data); err != nil || data.Accounts == nil || len(data.Accounts) > 100 {
		return nil, errors.New("Moomoo authorized account list is missing or invalid")
	}
	seen := map[string]bool{}
	result := make([]Account, 0, len(data.Accounts))
	for _, a := range data.Accounts {
		id := a.ID.String()
		if !validAccountID(id) || seen[id] || a.Firm == "" || a.Type == "" {
			return nil, errors.New("Moomoo account identity is missing or duplicated")
		}
		seen[id] = true
		last4 := id[len(id)-4:]
		if len(a.Card) >= 4 {
			last4 = a.Card[len(a.Card)-4:]
		}
		result = append(result, Account{ID: id, Firm: a.Firm, Type: a.Type, Last4: last4, USMarket: slices.Contains(a.Market, 2)})
	}
	return result, nil
}

// Observe verifies the configured business ID against fresh authorized US
// account facts before reading funds or holdings. It never selects an account.
func (c *Client) Observe(ctx context.Context, accountID string) (*AccountInfo, error) {
	evidence, err := c.AuditAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	return evidence.Observation(), nil
}

func (c *Client) authorizedUSAccount(ctx context.Context, accountID string) (Account, error) {
	if !validAccountID(accountID) {
		return Account{}, errors.New("explicit Moomoo business account binding required")
	}
	accounts, err := c.Accounts(ctx)
	if err != nil {
		return Account{}, err
	}
	index := slices.IndexFunc(accounts, func(a Account) bool { return a.ID == accountID })
	if index < 0 || accounts[index].Firm != "FUTUINC" || !accounts[index].USMarket {
		return Account{}, errors.New("configured Moomoo account is not an authorized US brokerage account")
	}
	return accounts[index], nil
}

type reportedFunds struct {
	Text  string
	Cents *wheel.Cents
}

func parseFunds(raw []byte) (reportedFunds, error) {
	var funds struct {
		Currency  string `json:"currency"`
		Available string `json:"available_funds"`
	}
	if err := exactJSON(raw, &funds); err != nil || funds.Currency != "USD" {
		return reportedFunds{}, errors.New("Moomoo USD funds are missing or ambiguous")
	}
	negative := strings.HasPrefix(funds.Available, "-")
	unsigned := strings.TrimPrefix(funds.Available, "-")
	whole, fraction, dot := strings.Cut(unsigned, ".")
	if whole == "" || len(fraction) > 12 || dot && fraction == "" {
		return reportedFunds{}, errors.New("Moomoo reported funds are not a bounded exact decimal")
	}
	for _, part := range []string{whole, fraction} {
		for _, digit := range part {
			if digit < '0' || digit > '9' {
				return reportedFunds{}, errors.New("Moomoo reported funds are not a bounded exact decimal")
			}
		}
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	limit := int64(wheel.MaxMoney) / 100
	if err != nil || w > limit || w == limit && strings.Trim(fraction, "0") != "" {
		return reportedFunds{}, errors.New("Moomoo reported funds exceed observation limits")
	}
	result := reportedFunds{Text: funds.Available}
	// A margin account's reported funds can change by fractions of a cent.
	// Retain that exact display value without rounding or representing it as
	// executable cents. Holdings observation remains independent of this precision.
	if len(fraction) > 2 && strings.Trim(fraction[2:], "0") != "" {
		return result, nil
	}
	value, err := wheel.ParseCents(unsigned)
	if err != nil {
		return reportedFunds{}, errors.New("Moomoo reported funds cannot be represented exactly")
	}
	if negative {
		value = -value
	}
	result.Cents = new(value)
	return result, nil
}

func parseHoldings(raw []byte) ([]Holding, error) {
	var rows []struct {
		Code         string `json:"code"`
		Quantity     string `json:"qty"`
		Available    string `json:"can_sell_qty"`
		Side         string `json:"position_side"`
		Currency     string `json:"currency"`
		SecurityType string `json:"security_type"`
	}
	if err := exactJSON(raw, &rows); err != nil || rows == nil || len(rows) > 20000 {
		return nil, errors.New("Moomoo holding list is missing or exceeds limit")
	}
	result := make([]Holding, 0, len(rows))
	seen := map[string]bool{}
	for _, row := range rows {
		identity := row.Code + "/" + row.Side
		if row.Code == "" || len(row.Code) > 128 || strings.TrimSpace(row.Code) != row.Code || seen[identity] || row.Currency == "" || row.Side != "LONG" && row.Side != "SHORT" {
			return nil, errors.New("Moomoo holding identity is missing, ambiguous or duplicated")
		}
		for _, value := range []string{row.Quantity, row.Available} {
			if _, err := quantityMicros(value); err != nil {
				return nil, errors.New("Moomoo holding quantity is not an exact bounded decimal")
			}
		}
		seen[identity] = true
		result = append(result, Holding{Symbol: row.Code, Quantity: row.Quantity, Available: row.Available,
			Side: row.Side, SecurityType: row.SecurityType, Currency: row.Currency, Ownership: "external_unclaimed"})
	}
	slices.SortFunc(result, func(a, b Holding) int { return strings.Compare(a.Symbol+"/"+a.Side, b.Symbol+"/"+b.Side) })
	return result, nil
}

// Quantities are parsed exactly even for observation; no float64 conversion or
// silent rounding is permitted. Native position units still need qualification.
func quantityMicros(s string) (int64, error) {
	negative := strings.HasPrefix(s, "-")
	u := strings.TrimPrefix(s, "-")
	whole, fraction, _ := strings.Cut(u, ".")
	if whole == "" || len(fraction) > 6 {
		return 0, errors.New("quantity precision")
	}
	for _, part := range []string{whole, fraction} {
		for _, r := range part {
			if r < '0' || r > '9' {
				return 0, errors.New("quantity format")
			}
		}
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || w > 1_000_000_000_000 {
		return 0, errors.New("quantity range")
	}
	f, _ := strconv.ParseInt((fraction + "000000")[:6], 10, 64)
	value := w*1_000_000 + f
	if negative {
		value = -value
	}
	return value, nil
}
