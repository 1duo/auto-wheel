package moomoo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"autowheel/internal/contractpolicy"
	"autowheel/internal/wheel"
)

type ChainContract struct {
	contractpolicy.Contract
	Owner        string `json:"owner"`
	StandardType string `json:"standard_type"`
	LotSize      int64  `json:"lot_size"`
}

type ChainPagination struct {
	HasMore *bool   `json:"has_more,omitempty"`
	NextKey *string `json:"next_key,omitempty"`
	Total   *int64  `json:"total,omitempty"`
}

type ChainPage struct {
	At         time.Time        `json:"at"`
	FinishedAt time.Time        `json:"finished_at"`
	From       string           `json:"from"`
	Through    string           `json:"through"`
	Query      string           `json:"query"`
	Bytes      []byte           `json:"source_bytes"`
	SHA256     string           `json:"source_sha256"`
	Pagination *ChainPagination `json:"pagination,omitempty"`
}

type OptionChainAudit struct {
	AccountID          string          `json:"account_id"`
	Symbol             string          `json:"symbol"`
	Session            string          `json:"session"`
	From               string          `json:"from"`
	Through            string          `json:"through"`
	At                 time.Time       `json:"at"`
	FinishedAt         time.Time       `json:"finished_at"`
	Pages              []ChainPage     `json:"pages"`
	Contracts          []ChainContract `json:"contracts"`
	Complete           bool            `json:"complete"`
	Reason             string          `json:"reason,omitempty"`
	ExecutionQualified bool            `json:"execution_qualified"`
	checked            bool
	selectionSymbol    string
	selectionDay       string
	selectionAt        time.Time
	selectionMenu      []contractpolicy.Contract
}

// SelectionMenu is available only on a complete, fresh collector result. A
// deserialized private report's Complete flag cannot construct this capability.
// This is identity evidence only, never financial or order authority.
func (a *OptionChainAudit) SelectionMenu() ([]contractpolicy.Contract, error) {
	if a == nil || !a.checked || !a.Complete || !sourceFresh(a.selectionAt, time.Now().UTC()) {
		return nil, errors.New("Moomoo option menu lacks checked complete pagination")
	}
	return slices.Clone(a.selectionMenu), nil
}

func (a *OptionChainAudit) ChooseIdentity(kind string, stock contractpolicy.Book) (*contractpolicy.Contract, error) {
	menu, err := a.SelectionMenu()
	if err != nil {
		return nil, err
	}
	return contractpolicy.Choose("conservative", kind, a.selectionDay, a.selectionSymbol, stock, menu)
}

func validChainSymbol(symbol string) bool {
	return len(symbol) >= 1 && len(symbol) <= 5 && strings.Trim(symbol, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") == ""
}

func parseChainContract(raw json.RawMessage, symbol, from, through string) (ChainContract, error) {
	fields, err := quoteFacts(raw, "code", "stock_type", "stock_owner", "option_type", "strike_time", "strike_price", "lot_size", "option_standard_type", "index_option_type")
	if err != nil {
		return ChainContract{}, err
	}
	var row struct {
		Code     string      `json:"code"`
		Type     string      `json:"stock_type"`
		Owner    string      `json:"stock_owner"`
		Kind     string      `json:"option_type"`
		Expiry   string      `json:"strike_time"`
		Strike   json.Number `json:"strike_price"`
		LotSize  int64       `json:"lot_size"`
		Standard string      `json:"option_standard_type"`
		Index    string      `json:"index_option_type"`
	}
	if exactJSON(raw, &row) != nil || row.Type != "DRVT" || row.Owner != "US."+symbol || !slices.Contains([]string{"CALL", "PUT"}, row.Kind) || row.Standard != "STANDARD" || row.LotSize != 100 || row.Index != "N/A" {
		return ChainContract{}, errors.New("Moomoo chain row is not the requested standard US equity option")
	}
	expiry, err := time.Parse(time.DateOnly, row.Expiry)
	if err != nil || expiry.Format(time.DateOnly) != row.Expiry || row.Expiry < from || row.Expiry > through || expiry.Year() < 2000 || expiry.Year() > 2099 {
		return ChainContract{}, errors.New("Moomoo option expiry is ambiguous or outside its requested window")
	}
	price := bytes.TrimSpace(fields["strike_price"])
	if len(price) == 0 || price[0] == '"' {
		return ChainContract{}, errors.New("Moomoo option strike is not a JSON number")
	}
	strike, err := wheel.ParseCents(row.Strike.String())
	if err != nil || strike < 1 || strike > wheel.MaxMoney/100 {
		return ChainContract{}, errors.New("Moomoo option strike cannot be mapped to exact bounded cents")
	}
	// Do not guess a wire-price scale. The returned decimal must agree with
	// the provider's standard code, including root, date, right and strike.
	expected := "US." + symbol + expiry.Format("060102") + row.Kind[:1] + strconv.FormatInt(int64(strike)*10, 10)
	if row.Code != expected {
		return ChainContract{}, errors.New("Moomoo option code contradicts its date, side or exact strike")
	}
	return ChainContract{Contract: contractpolicy.Contract{ID: row.Code, Symbol: symbol, Expiry: row.Expiry, Kind: strings.ToLower(row.Kind), Strike: int64(strike)}, Owner: row.Owner, StandardType: row.Standard, LotSize: row.LotSize}, nil
}

func (c *Client) readChainPage(ctx context.Context, symbol, from, through, cursor string) (ChainPage, []json.RawMessage, error) {
	start, startErr := time.Parse(time.DateOnly, from)
	end, endErr := time.Parse(time.DateOnly, through)
	if !validChainSymbol(symbol) || startErr != nil || endErr != nil || start.Format(time.DateOnly) != from || end.Format(time.DateOnly) != through || end.Before(start) || end.Sub(start) > 19*24*time.Hour || len(cursor) > 2048 {
		return ChainPage{}, nil, errors.New("invalid bounded Moomoo chain resource or date window")
	}
	// The documented chain resource has only start/end/filter parameters. A
	// cursor is sent only if a provider page ever returns one.
	query := url.Values{"start": {from}, "end": {through}, "filter_standard": {"STANDARD"}}
	if cursor != "" {
		query.Set("next_key", cursor)
	}
	page := ChainPage{At: time.Now().UTC(), From: from, Through: through, Query: query.Encode()}
	token, err := c.tokens.Access(ctx, c.http)
	if err != nil {
		return page, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, restEndpoint+"/api/v1.0/quote/US."+symbol+"/option-chain?"+query.Encode(), nil)
	if err != nil {
		return page, nil, errors.New("invalid Moomoo chain request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	response, err := c.http.Do(req)
	if err != nil {
		return page, nil, errors.New("Moomoo option-chain transport failed; no automatic replay")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return page, nil, errors.New("Moomoo option-chain HTTP response failed")
	}
	page.Bytes, err = readBounded(response.Body, 8<<20)
	if err != nil {
		return page, nil, err
	}
	envelope, data, err := quoteData(page.Bytes)
	if err != nil {
		return page, nil, err
	}
	var rows []json.RawMessage
	if rejectQuoteAliases(data, "option_chain") != nil || exactJSON(data["option_chain"], &rows) != nil || rows == nil || len(rows) > 20000 {
		return page, nil, errors.New("Moomoo option-chain list is missing, ambiguous or exceeds its limit")
	}
	if raw := bytes.TrimSpace(envelope["pagination"]); len(raw) != 0 && !bytes.Equal(raw, []byte("null")) {
		var fields map[string]json.RawMessage
		if exactJSON(raw, &fields) != nil || rejectQuoteAliases(fields, "has_more", "next_key", "total") != nil || exactJSON(raw, &page.Pagination) != nil || page.Pagination == nil || page.Pagination.HasMore == nil ||
			page.Pagination.Total != nil && (*page.Pagination.Total < 0 || *page.Pagination.Total > 20000) {
			return page, nil, errors.New("Moomoo chain pagination facts are invalid")
		}
	}
	hash := sha256.Sum256(page.Bytes)
	page.SHA256, page.FinishedAt = hex.EncodeToString(hash[:]), time.Now().UTC()
	return page, rows, nil
}

func (c *Client) AuditOptionChain(ctx context.Context, account, symbol, day string) (*OptionChainAudit, error) {
	session, err := time.Parse(time.DateOnly, day)
	if !validChainSymbol(symbol) || err != nil || session.Format(time.DateOnly) != day || session.Year() < 2000 || session.Year() > 2099 {
		return nil, errors.New("option-chain audit requires an explicit equity symbol and canonical session date")
	}
	if _, err := c.authorizedUSAccount(ctx, account); err != nil {
		return nil, err
	}
	return c.collectOptionChain(ctx, account, symbol, session)
}

// collectOptionChain is the chain read without its own account check. The
// execution source calls it only after a same-cycle authorized snapshot.
func (c *Client) collectOptionChain(ctx context.Context, account, symbol string, session time.Time) (*OptionChainAudit, error) {
	day := session.Format(time.DateOnly)
	audit := &OptionChainAudit{AccountID: account, Symbol: symbol, Session: day, From: session.AddDate(0, 0, 21).Format(time.DateOnly), Through: session.AddDate(0, 0, 45).Format(time.DateOnly), At: time.Now().UTC(), Complete: true, Contracts: []ChainContract{}, selectionSymbol: symbol, selectionDay: day}
	seen, totalBytes := map[string]bool{}, 0
	// At most twenty expiry dates can fit in a twenty-calendar-day subrange.
	// This avoids the backend's twenty-expiry cap without assuming weekends or
	// inventing a smaller identity-selection window.
	for _, span := range [][2]int{{21, 40}, {41, 45}} {
		from, through := session.AddDate(0, 0, span[0]).Format(time.DateOnly), session.AddDate(0, 0, span[1]).Format(time.DateOnly)
		cursor, cursors, count, complete := "", map[string]bool{}, 0, false
		var total *int64
		for range 64 {
			if len(audit.Pages) >= 64 {
				return nil, errors.New("Moomoo option-chain audit exceeds page limit")
			}
			page, rows, err := c.readChainPage(ctx, symbol, from, through, cursor)
			if err != nil {
				return nil, err
			}
			totalBytes += len(page.Bytes)
			if totalBytes > 32<<20 || len(audit.Contracts)+len(rows) > 20000 {
				return nil, errors.New("Moomoo option-chain audit exceeds total evidence limit")
			}
			audit.Pages = append(audit.Pages, page)
			for _, raw := range rows {
				fact, err := parseChainContract(raw, symbol, from, through)
				if err != nil {
					return nil, err
				}
				if seen[fact.ID] {
					return nil, errors.New("Moomoo option identity is duplicated across chain pages")
				}
				seen[fact.ID] = true
				audit.Contracts = append(audit.Contracts, fact)
			}
			count += len(rows)
			if page.Pagination == nil {
				// The option-chain reference documents no pagination: one response
				// is the whole date range, capped at twenty expiries. Each subrange
				// spans at most twenty calendar dates, so the cap cannot truncate it.
				// A response after a cursor without its own facts stays incomplete.
				if cursor != "" {
					audit.Complete, audit.Reason = false, "Moomoo continuation page omitted its pagination facts"
					break
				}
				complete = true
				break
			}
			if page.Pagination.Total != nil {
				if total != nil && *total != *page.Pagination.Total {
					return nil, errors.New("Moomoo chain total changed during pagination")
				}
				total = page.Pagination.Total
			}
			if !*page.Pagination.HasMore {
				if total != nil && int64(count) != *total {
					return nil, errors.New("Moomoo chain terminal page contradicts its total")
				}
				complete = true
				break
			}
			if page.Pagination.NextKey == nil || *page.Pagination.NextKey == "" || *page.Pagination.NextKey == "-1" || len(*page.Pagination.NextKey) > 2048 || cursors[*page.Pagination.NextKey] {
				return nil, errors.New("Moomoo chain next cursor is missing or repeated")
			}
			cursor = *page.Pagination.NextKey
			cursors[cursor] = true
		}
		if !complete && audit.Complete {
			return nil, errors.New("Moomoo option-chain pagination did not terminate")
		}
	}
	slices.SortFunc(audit.Contracts, func(a, b ChainContract) int { return strings.Compare(a.ID, b.ID) })
	if audit.Complete {
		audit.checked = true
		audit.selectionAt = audit.At
		for _, fact := range audit.Contracts {
			audit.selectionMenu = append(audit.selectionMenu, fact.Contract)
		}
	}
	audit.FinishedAt = time.Now().UTC()
	return audit, nil
}
