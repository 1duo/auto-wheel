package moomoo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// TransactionAudit preserves complete query pages, including cash records
// without IDs. It never deduplicates those records or infers settled money,
// assignment, fill ownership or an executable wheel snapshot from descriptions.
type TransactionAudit struct {
	AccountID                  string            `json:"account_id"`
	At                         time.Time         `json:"at"`
	FinishedAt                 time.Time         `json:"finished_at"`
	HistoryFrom                time.Time         `json:"history_from"`
	HistoryThrough             time.Time         `json:"history_through"`
	FillMarket                 string            `json:"fill_market"`
	TodayFills                 []TransactionPage `json:"today_fill_pages"`
	HistoryFills               []TransactionPage `json:"history_fill_pages"`
	CashLogs                   []TransactionPage `json:"cash_log_pages"`
	FillClockOrderingQualified bool              `json:"fill_clock_ordering_qualified"`
	CashValuesAreDisplayText   bool              `json:"cash_values_are_display_text"`
	ExecutionQualified         bool              `json:"execution_qualified"`
	CoverageWarning            string            `json:"coverage_warning"`
}

// Data retains the exact validated REST envelope's d bytes. Base64 encoding
// prevents a later JSON pretty-printer from changing their byte-level hash.
type TransactionPage struct {
	Data   []byte `json:"data_base64"`
	SHA256 string `json:"sha256"`
}

// AuditTransactions uses the already authorized OAuth REST connection, with
// fresh exact US-account authorization before any account-scoped GET. History
// filters refer to fill update time, not creation time. Today's results and
// historical results remain separate so revisions cannot be silently merged.
func (c *Client) AuditTransactions(ctx context.Context, accountID string, since time.Time) (*TransactionAudit, error) {
	now := time.Now().UTC()
	if since.IsZero() || since.Before(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)) || !since.Before(now) {
		return nil, errors.New("Moomoo transaction audit requires an explicit past history start")
	}
	if _, err := c.authorizedUSAccount(ctx, accountID); err != nil {
		return nil, err
	}
	read := func(path string, base url.Values, key string, size int, validate func(json.RawMessage) (string, error)) ([]TransactionPage, error) {
		return collectTransactionPages(ctx, func(ctx context.Context, query url.Values) (json.RawMessage, error) {
			return c.readGET(ctx, "/api/v1.0/accounts/"+accountID+path, query)
		}, base, key, size, validate)
	}
	fill := func(raw json.RawMessage) (string, error) {
		return validateFillRecord(raw, time.Time{}, time.Now().UTC())
	}
	today, err := read("/order_fills", url.Values{"trd_market": {"US"}, "page_size": {"100"}}, "order_fills", 100, fill)
	if err != nil {
		return nil, err
	}
	history, err := read("/fills_history", url.Values{"trd_market": {"US"}, "page_size": {"50"},
		"start": {strconv.FormatInt(since.UnixMicro(), 10)}, "end": {strconv.FormatInt(now.UnixMicro(), 10)}}, "order_fills", 50,
		func(raw json.RawMessage) (string, error) { return validateFillRecord(raw, since, now) })
	if err != nil {
		return nil, err
	}
	cash, err := read("/cash_logs", url.Values{"page_size": {"20"}, "begin_time": {strconv.FormatInt(since.UnixMicro(), 10)},
		"end_time": {strconv.FormatInt(now.UnixMicro(), 10)}}, "cash_logs", 20, validateCashRecord)
	if err != nil {
		return nil, err
	}
	return &TransactionAudit{AccountID: accountID, At: now, FinishedAt: time.Now().UTC(), HistoryFrom: since.UTC(), HistoryThrough: now,
		FillMarket: "US", TodayFills: today, HistoryFills: history, CashLogs: cash, CashValuesAreDisplayText: true,
		CoverageWarning: "Non-atomic query-page evidence only. Today's boundary timezone and fill creation/update ordering are unqualified. Never infer event ordering or an incremental checkpoint from these clocks. Cash amounts/balances are display strings, not parsed financial values, with no qualified stable ID, currency, timezone or settlement meaning. Never merge separate runs by description. Fills do not prove assignment, ownership, settled collateral or complete obligations."}, nil
}

func collectTransactionPages(ctx context.Context, read orderPageReader, base url.Values, key string, size int,
	validate func(json.RawMessage) (string, error)) ([]TransactionPage, error) {
	pages := []TransactionPage{}
	seenIDs, seenCursors := map[string]bool{}, map[string]bool{}
	cursor, total := "", 0
	for range 64 {
		query := maps.Clone(base)
		if cursor != "" {
			query.Set("page_flag", cursor)
		}
		raw, err := read(ctx, query)
		if err != nil {
			return nil, err
		}
		total += len(raw)
		if total > 16<<20 {
			return nil, errors.New("Moomoo transaction audit exceeds evidence size limit")
		}
		var page map[string]json.RawMessage
		if exactJSON(raw, &page) != nil || page[key] == nil || page["completed"] == nil || page["page_flag"] == nil {
			return nil, errors.New("Moomoo transaction page is missing exact list/completion/cursor fields")
		}
		var rows []json.RawMessage
		var completed *bool
		var next *string
		if exactJSON(page[key], &rows) != nil || rows == nil || len(rows) > size ||
			exactJSON(page["completed"], &completed) != nil || completed == nil ||
			exactJSON(page["page_flag"], &next) != nil || next == nil || len(*next) > 4096 {
			return nil, errors.New("Moomoo transaction pagination facts are invalid or exceed limits")
		}
		for _, row := range rows {
			id, err := validate(row)
			if err != nil {
				return nil, err
			}
			if id != "" && seenIDs[id] {
				return nil, errors.New("Moomoo fill ID is duplicated within one query; revisions remain unresolved")
			}
			if id != "" {
				seenIDs[id] = true
			}
		}
		digest := sha256.Sum256(raw)
		pages = append(pages, TransactionPage{Data: bytes.Clone(raw), SHA256: hex.EncodeToString(digest[:])})
		if *completed {
			return pages, nil
		}
		if *next == "" || seenCursors[*next] {
			return nil, errors.New("Moomoo transaction cursor is missing or repeated")
		}
		cursor = *next
		seenCursors[cursor] = true
	}
	return nil, errors.New("Moomoo transaction pagination exceeds page limit")
}

func recordString(fields map[string]json.RawMessage, key string) string {
	var value string
	if json.Unmarshal(fields[key], &value) != nil {
		return ""
	}
	return value
}

func evidenceDecimal(value string) bool {
	whole, fraction, dot := strings.Cut(value, ".")
	if len(whole) < 1 || len(whole) > 16 || len(fraction) > 12 || dot && fraction == "" {
		return false
	}
	for _, char := range whole + fraction {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true // Source text only; no conversion, rounding or ledger arithmetic.
}

func validateFillRecord(raw json.RawMessage, from, through time.Time) (string, error) {
	var fields map[string]json.RawMessage
	if exactJSON(raw, &fields) != nil {
		return "", errors.New("invalid Moomoo fill evidence")
	}
	id, orderID, code := recordString(fields, "deal_id"), recordString(fields, "order_id"), recordString(fields, "code")
	var created json.Number
	createdErr := json.Unmarshal(fields["create_time"], &created)
	create, createErr := created.Int64()
	update, updateErr := fillUpdateMicros(fields)
	if id == "" || len(id) > 128 || strings.TrimSpace(id) != id || orderID == "" || len(orderID) > 128 || strings.TrimSpace(orderID) != orderID ||
		!strings.HasPrefix(code, "US.") || len(code) <= 3 || len(code) > 128 || strings.TrimSpace(code) != code ||
		!slices.Contains([]string{"BUY", "SELL", "SELL_SHORT", "BUY_BACK"}, recordString(fields, "trd_side")) ||
		!slices.Contains([]string{"OK", "CANCELLED", "CHANGED"}, recordString(fields, "status")) ||
		!evidenceDecimal(recordString(fields, "qty")) || !evidenceDecimal(recordString(fields, "price")) ||
		createdErr != nil || createErr != nil || updateErr != nil ||
		create < time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC).UnixMicro() || create > through.UnixMicro() ||
		update < time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC).UnixMicro() ||
		update > through.UnixMicro() || !from.IsZero() && update < from.UnixMicro() {
		return "", errors.New("Moomoo fill identity, decimal text, status or microsecond update interval is invalid")
	}
	return id, nil
}

func fillUpdateMicros(fields map[string]json.RawMessage) (int64, error) {
	// The reference documents updated_time; authenticated REST fills currently
	// return update_time. Accept these explicit spellings, require exact integer
	// agreement if both appear, and never substitute the creation timestamp.
	// Actual fill update clocks can precede creation clocks: both are retained
	// as independent source facts, never as proven event order or a checkpoint.
	var result int64
	seen := false
	for _, key := range []string{"updated_time", "update_time"} {
		raw, present := fields[key]
		if !present {
			continue
		}
		var number json.Number
		err := json.Unmarshal(raw, &number)
		value, parseErr := number.Int64()
		if err != nil || parseErr != nil || seen && value != result {
			return 0, errors.New("Moomoo fill update clock is invalid or conflicting")
		}
		result, seen = value, true
	}
	if !seen {
		return 0, errors.New("Moomoo fill update clock is missing")
	}
	return result, nil
}

func validateCashRecord(raw json.RawMessage) (string, error) {
	var fields map[string]json.RawMessage
	if exactJSON(raw, &fields) != nil {
		return "", errors.New("invalid Moomoo cash-log evidence")
	}
	name, created := recordString(fields, "transaction_name"), recordString(fields, "created_time")
	change, balance := recordString(fields, "cash_change"), recordString(fields, "balance")
	if name == "" || len(name) > 512 || created == "" || len(created) > 64 ||
		change == "" || len(change) > 128 || balance == "" || len(balance) > 256 {
		return "", errors.New("Moomoo cash-log description, time or display strings are missing or exceed limits")
	}
	// Actual cash values include grouping, explicit plus signs and labelled
	// balances. Preserve display text; do not strip labels or infer a currency,
	// numeric cash balance, borrow-free collateral or settlement from it.
	// The published schema has no stable event ID, currency or timezone.
	// Empty ID deliberately preserves every record, including identical rows.
	return "", nil
}
