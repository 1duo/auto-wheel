package moomoo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"autowheel/internal/wheel"
)

type QuotePacket struct {
	At         time.Time                  `json:"at"`
	FinishedAt time.Time                  `json:"finished_at"`
	Path       string                     `json:"path"`
	Codes      []string                   `json:"codes"`
	Bytes      []byte                     `json:"source_bytes"`
	SHA256     string                     `json:"source_sha256"`
	Rows       map[string]json.RawMessage `json:"rows"`
}

type InstrumentFacts struct {
	Code         string `json:"code"`
	Type         string `json:"stock_type"`
	Owner        string `json:"stock_owner,omitempty"`
	Exchange     string `json:"exchange"`
	State        string `json:"state"`
	Suspended    bool   `json:"suspension"`
	LotSize      int64  `json:"lot_size"`
	ContractSize *int64 `json:"contract_size,omitempty"`
}

// QuoteAt is the provider's generic snapshot clock, not an independently
// attested bid/ask clock. PriceSpread is retained without claiming it is a
// permissible order-price increment. Neither fact authorizes native execution.
type MarketQuoteFacts struct {
	Code        string       `json:"code"`
	QuoteAt     time.Time    `json:"quote_at"`
	DataDate    string       `json:"data_date"`
	Bid         wheel.Cents  `json:"bid_cents"`
	Ask         wheel.Cents  `json:"ask_cents"`
	BidSize     int64        `json:"bid_size"`
	AskSize     int64        `json:"ask_size"`
	PriceSpread *wheel.Cents `json:"price_spread_cents,omitempty"`
	Status      string       `json:"sec_status"`
	Suspended   bool         `json:"suspension"`
	Option      bool         `json:"option_valid"`
}

type MarketAudit struct {
	AccountID          string                      `json:"account_id"`
	At                 time.Time                   `json:"at"`
	Basic              *QuotePacket                `json:"basic"`
	Snapshot           *QuotePacket                `json:"snapshot"`
	Instruments        map[string]InstrumentFacts  `json:"instruments"`
	Books              map[string]MarketQuoteFacts `json:"books"`
	ExecutionQualified bool                        `json:"execution_qualified"`
}

func validQuoteCodes(codes []string) bool {
	if len(codes) < 1 || len(codes) > 400 {
		return false
	}
	seen := map[string]bool{}
	for _, code := range codes {
		if len(code) < 4 || len(code) > 128 || !strings.HasPrefix(code, "US.") || seen[code] {
			return false
		}
		for _, ch := range code[3:] {
			if !(ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '.' || ch == '-') {
				return false
			}
		}
		seen[code] = true
	}
	return true
}

func rejectQuoteAliases(fields map[string]json.RawMessage, keys ...string) error {
	for key := range fields {
		for _, canonical := range keys {
			if key != canonical && strings.EqualFold(key, canonical) {
				return errors.New("Moomoo quote contains a case alias")
			}
		}
	}
	return nil
}

// readQuoteBatch is a closed allowlist of non-mutating POST resources. It
// cannot be used as an arbitrary REST writer or invoke an MCP financial tool.
func (c *Client) readQuoteBatch(ctx context.Context, path string, codes []string) (*QuotePacket, error) {
	list := ""
	switch path {
	case "/api/v1.0/quote/stock-basicinfo":
		list = "basic_list"
	case "/api/v1.0/quote/snapshot":
		list = "snapshot_list"
	default:
		return nil, errors.New("Moomoo quote reader blocks this resource")
	}
	if !validQuoteCodes(codes) {
		return nil, errors.New("Moomoo quote batch requires 1–400 distinct explicit US codes")
	}
	at := time.Now().UTC()
	token, err := c.tokens.Access(ctx, c.http)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(struct {
		Codes []string `json:"code_list"`
	}{codes})
	if err != nil {
		return nil, errors.New("invalid Moomoo quote request")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, restEndpoint+path, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("invalid Moomoo quote request")
	}
	req.GetBody = nil
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	response, err := c.http.Do(req)
	if err != nil {
		return nil, errors.New("Moomoo quote transport failed; no automatic replay")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("Moomoo quote HTTP response failed; no automatic replay")
	}
	raw, err := readBounded(response.Body, 8<<20)
	if err != nil {
		return nil, err
	}
	_, data, err := quoteData(raw)
	if err != nil {
		return nil, err
	}
	if rejectQuoteAliases(data, list) != nil {
		return nil, errors.New("Moomoo quote data is missing or ambiguous")
	}
	var rows []json.RawMessage
	if exactJSON(data[list], &rows) != nil || rows == nil || len(rows) != len(codes) {
		return nil, errors.New("Moomoo quote batch omitted a requested instrument")
	}
	indexed := make(map[string]json.RawMessage, len(rows))
	for _, rawRow := range rows {
		var row map[string]json.RawMessage
		var code string
		if exactJSON(rawRow, &row) != nil || rejectQuoteAliases(row, "code") != nil || exactJSON(row["code"], &code) != nil || !slices.Contains(codes, code) || indexed[code] != nil {
			return nil, errors.New("Moomoo quote identity is missing, foreign or duplicated")
		}
		indexed[code] = bytes.Clone(rawRow)
	}
	hash := sha256.Sum256(raw)
	return &QuotePacket{At: at, FinishedAt: time.Now().UTC(), Path: path, Codes: slices.Clone(codes), Bytes: raw, SHA256: hex.EncodeToString(hash[:]), Rows: indexed}, nil
}

func quoteData(raw []byte) (map[string]json.RawMessage, map[string]json.RawMessage, error) {
	var envelope map[string]json.RawMessage
	if exactJSON(raw, &envelope) != nil || rejectQuoteAliases(envelope, "ret_code", "data", "ret_msg", "error", "pagination") != nil || string(bytes.TrimSpace(envelope["ret_code"])) != "0" ||
		len(envelope["error"]) > 0 && string(bytes.TrimSpace(envelope["error"])) != "null" {
		return nil, nil, errors.New("Moomoo quote envelope failed or is ambiguous")
	}
	var data map[string]json.RawMessage
	if exactJSON(envelope["data"], &data) != nil || data == nil {
		return nil, nil, errors.New("Moomoo quote data is missing or ambiguous")
	}
	return envelope, data, nil
}

func quoteFacts(raw json.RawMessage, keys ...string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if exactJSON(raw, &fields) != nil || rejectQuoteAliases(fields, keys...) != nil {
		return nil, errors.New("invalid Moomoo quote facts")
	}
	for _, key := range keys {
		if len(fields[key]) == 0 || string(bytes.TrimSpace(fields[key])) == "null" {
			return nil, errors.New("required Moomoo quote fact is unavailable: " + key)
		}
	}
	return fields, nil
}

func instrumentFacts(raw json.RawMessage) (InstrumentFacts, error) {
	fields, err := quoteFacts(raw, "code", "stock_type", "exchange", "state", "suspension", "lot_size")
	if err != nil {
		return InstrumentFacts{}, err
	}
	var facts InstrumentFacts
	if err := rejectQuoteAliases(fields, "contract_size", "stock_owner"); err != nil {
		return InstrumentFacts{}, err
	}
	if exactJSON(raw, &facts) != nil || facts.Type == "" || facts.Exchange == "" || facts.State == "" || facts.LotSize < 1 || facts.LotSize > 1_000_000 ||
		facts.ContractSize != nil && (*facts.ContractSize < 0 || *facts.ContractSize > 1_000_000) {
		return InstrumentFacts{}, errors.New("Moomoo instrument metadata is invalid")
	}
	if facts.Type == "DRVT" {
		if err := rejectQuoteAliases(fields, "contract_size", "stock_owner"); err != nil || facts.ContractSize == nil || *facts.ContractSize < 1 || !validQuoteCodes([]string{facts.Owner}) {
			return InstrumentFacts{}, errors.New("Moomoo derivative deliverable metadata is missing")
		}
	}
	return facts, nil
}

func marketQuoteFacts(raw json.RawMessage, now time.Time) (MarketQuoteFacts, error) {
	fields, err := quoteFacts(raw, "code", "update_time", "data_date", "bid_price", "ask_price", "bid_vol", "ask_vol", "sec_status", "suspension", "option_valid")
	if err != nil {
		return MarketQuoteFacts{}, err
	}
	if err := rejectQuoteAliases(fields, "price_spread"); err != nil {
		return MarketQuoteFacts{}, err
	}
	var row struct {
		Code      string      `json:"code"`
		Updated   int64       `json:"update_time"`
		DataDate  string      `json:"data_date"`
		Bid       json.Number `json:"bid_price"`
		Ask       json.Number `json:"ask_price"`
		BidSize   int64       `json:"bid_vol"`
		AskSize   int64       `json:"ask_vol"`
		Status    string      `json:"sec_status"`
		Suspended bool        `json:"suspension"`
		Option    bool        `json:"option_valid"`
	}
	if exactJSON(raw, &row) != nil {
		return MarketQuoteFacts{}, errors.New("Moomoo snapshot facts have invalid types")
	}
	// Price scalars must actually be JSON numbers; quoted numbers are not the
	// documented schema even though encoding/json can decode them into Number.
	for _, key := range []string{"bid_price", "ask_price", "price_spread"} {
		if value := bytes.TrimSpace(fields[key]); len(value) > 0 && (value[0] == '"' || bytes.Equal(value, []byte("null"))) {
			return MarketQuoteFacts{}, errors.New("Moomoo snapshot price has an invalid type")
		}
	}
	bid, bidErr := wheel.ParseCents(row.Bid.String())
	ask, askErr := wheel.ParseCents(row.Ask.String())
	at := time.UnixMilli(row.Updated).UTC()
	_, dateErr := time.Parse(time.DateOnly, row.DataDate)
	if bidErr != nil || askErr != nil || bid > ask || row.BidSize < 0 || row.AskSize < 0 || row.BidSize > 1_000_000_000_000 || row.AskSize > 1_000_000_000_000 || row.Status == "" || dateErr != nil ||
		at.Before(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)) || at.After(now.Add(2*time.Second)) {
		return MarketQuoteFacts{}, errors.New("Moomoo snapshot book, units or clock is unqualified")
	}
	facts := MarketQuoteFacts{Code: row.Code, QuoteAt: at, DataDate: row.DataDate, Bid: bid, Ask: ask, BidSize: row.BidSize, AskSize: row.AskSize, Status: row.Status, Suspended: row.Suspended, Option: row.Option}
	if len(fields["price_spread"]) > 0 {
		spread, err := wheel.ParseCents(string(bytes.TrimSpace(fields["price_spread"])))
		if err != nil {
			return MarketQuoteFacts{}, errors.New("Moomoo snapshot price spread is invalid")
		}
		facts.PriceSpread = new(spread)
	}
	return facts, nil
}

func (c *Client) AuditMarket(ctx context.Context, account string, codes []string) (*MarketAudit, error) {
	if !validQuoteCodes(codes) {
		return nil, errors.New("market audit requires 1–400 distinct explicit US quote codes")
	}
	if _, err := c.authorizedUSAccount(ctx, account); err != nil {
		return nil, err
	}
	at := time.Now().UTC()
	basic, err := c.readQuoteBatch(ctx, "/api/v1.0/quote/stock-basicinfo", codes)
	if err != nil {
		return nil, err
	}
	snapshot, err := c.readQuoteBatch(ctx, "/api/v1.0/quote/snapshot", codes)
	if err != nil {
		return nil, err
	}
	audit := &MarketAudit{AccountID: account, At: at, Basic: basic, Snapshot: snapshot, Instruments: map[string]InstrumentFacts{}, Books: map[string]MarketQuoteFacts{}}
	for _, code := range codes {
		instrument, err := instrumentFacts(basic.Rows[code])
		if err != nil {
			return nil, err
		}
		book, err := marketQuoteFacts(snapshot.Rows[code], time.Now().UTC())
		if err != nil {
			return nil, err
		}
		audit.Instruments[code], audit.Books[code] = instrument, book
	}
	return audit, nil
}
