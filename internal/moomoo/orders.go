package moomoo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/url"
	"strconv"
	"time"
)

// OrderEvidence retains the original provider decimals and instrument fields.
// This is an audit input, not a normalized execution receipt or ownership proof.
type OrderEvidence struct {
	ID        string          `json:"order_id"`
	Code      string          `json:"code"`
	Side      string          `json:"side"`
	Type      string          `json:"order_type"`
	Status    string          `json:"order_status"`
	Remark    *string         `json:"remark"` // nil means the provider omitted a reference; never infer ownership.
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
	Raw       json.RawMessage `json:"provider_order"`
}

type OrderAudit struct {
	AccountID          string          `json:"account_id"`
	At                 time.Time       `json:"at"`
	Market             string          `json:"market"`
	HistoryFrom        time.Time       `json:"history_from"`
	HistoryTo          time.Time       `json:"history_to"`
	Recent             []OrderEvidence `json:"recent_orders"`
	History            []OrderEvidence `json:"historical_orders"`
	RecentPages        int             `json:"recent_pages"`
	HistoryPages       int             `json:"history_pages"`
	ExecutionQualified bool            `json:"execution_qualified"`
	CoverageWarning    string          `json:"coverage_warning"`
}

// AuditOrders exhausts both documented cursors without filtering by symbol,
// status or order type. The history interval is explicit: Moomoo otherwise
// silently defaults to 90 days. Completion flags describe these query results
// only; they do not prove account-wide conditional-order/settlement coverage.
func (c *Client) AuditOrders(ctx context.Context, accountID string, since time.Time) (*OrderAudit, error) {
	now := time.Now().UTC()
	if since.IsZero() || since.Before(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)) || !since.Before(now) {
		return nil, errors.New("Moomoo order audit requires an explicit past history start")
	}
	if _, err := c.authorizedUSAccount(ctx, accountID); err != nil {
		return nil, err
	}
	recent, recentPages, err := collectOrderPages(ctx, func(ctx context.Context, query url.Values) (json.RawMessage, error) {
		return c.readGET(ctx, "/api/v1.0/accounts/"+accountID+"/orders", query)
	}, url.Values{"trd_market": {"US"}, "page_size": {"100"}})
	if err != nil {
		return nil, err
	}
	history, historyPages, err := collectOrderPages(ctx, func(ctx context.Context, query url.Values) (json.RawMessage, error) {
		return c.readGET(ctx, "/api/v1.0/accounts/"+accountID+"/orders_history", query)
	}, url.Values{"trd_market": {"US"}, "page_size": {"100"}, "start": {strconv.FormatInt(since.UnixMicro(), 10)}, "end": {strconv.FormatInt(now.UnixMicro(), 10)}})
	if err != nil {
		return nil, err
	}
	for _, order := range history {
		if order.CreatedAt.Before(since) || order.CreatedAt.After(now) {
			return nil, errors.New("Moomoo historical order lies outside the requested interval")
		}
	}
	return &OrderAudit{AccountID: accountID, At: now, Market: "US", HistoryFrom: since.UTC(), HistoryTo: now,
		Recent: recent, History: history, RecentPages: recentPages, HistoryPages: historyPages,
		CoverageWarning: "US order-query evidence only. No guarantee of atomic reads, dormant conditional orders, other-market obligations, assignment or settlement completeness."}, nil
}

type orderPageReader func(context.Context, url.Values) (json.RawMessage, error)

func collectOrderPages(ctx context.Context, read orderPageReader, base url.Values) ([]OrderEvidence, int, error) {
	orders := []OrderEvidence{}
	seenIDs, seenCursors := map[string]bool{}, map[string]bool{}
	cursor, totalBytes := "", 0
	for pageIndex := range 64 {
		query := maps.Clone(base)
		if cursor != "" {
			query.Set("page_flag", cursor)
		}
		raw, err := read(ctx, query)
		if err != nil {
			return nil, 0, err
		}
		totalBytes += len(raw)
		if totalBytes > 16<<20 {
			return nil, 0, errors.New("Moomoo order audit exceeds evidence size limit")
		}
		var page struct {
			Orders    []json.RawMessage `json:"orders"`
			Cursor    *string           `json:"page_flag"`
			Completed *bool             `json:"completed"`
		}
		if exactJSON(raw, &page) != nil || page.Orders == nil || page.Completed == nil || page.Cursor == nil || len(page.Orders) > 100 {
			return nil, 0, errors.New("Moomoo order pagination/completion facts missing or invalid")
		}
		for _, rawOrder := range page.Orders {
			order, err := parseOrderEvidence(rawOrder)
			if err != nil {
				return nil, 0, err
			}
			if seenIDs[order.ID] {
				return nil, 0, errors.New("Moomoo order ID duplicated across pages; query is ambiguous")
			}
			seenIDs[order.ID] = true
			orders = append(orders, order)
		}
		if *page.Completed {
			return orders, pageIndex + 1, nil
		}
		if *page.Cursor == "" || seenCursors[*page.Cursor] {
			return nil, 0, errors.New("Moomoo nonterminal order cursor missing or repeated")
		}
		cursor = *page.Cursor
		seenCursors[cursor] = true
	}
	return nil, 0, errors.New("Moomoo order pagination exceeds limit")
}

func parseOrderEvidence(raw json.RawMessage) (OrderEvidence, error) {
	var row struct {
		ID      string      `json:"order_id"`
		Code    string      `json:"code"`
		Side    string      `json:"side"`
		Type    string      `json:"order_type"`
		Status  string      `json:"order_status"`
		Remark  *string     `json:"remark"`
		Created json.Number `json:"create_time"`
		Updated json.Number `json:"updated_time"`
	}
	if exactJSON(raw, &row) != nil || row.ID == "" || row.Code == "" || row.Side == "" || row.Type == "" || row.Status == "" {
		return OrderEvidence{}, errors.New("Moomoo order identity facts missing")
	}
	created, err := row.Created.Int64()
	if err != nil {
		return OrderEvidence{}, errors.New("Moomoo order creation time is not an exact integer")
	}
	updated, err := row.Updated.Int64()
	if err != nil || created < time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC).UnixMicro() || updated < created || updated > time.Now().Add(2*time.Second).UnixMicro() {
		return OrderEvidence{}, errors.New("Moomoo order timestamp units, range or ordering is ambiguous")
	}
	return OrderEvidence{ID: row.ID, Code: row.Code, Side: row.Side, Type: row.Type, Status: row.Status, Remark: row.Remark,
		CreatedAt: time.UnixMicro(created).UTC(), UpdatedAt: time.UnixMicro(updated).UTC(), Raw: bytes.Clone(raw)}, nil
}
