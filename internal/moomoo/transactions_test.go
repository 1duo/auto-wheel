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
	"strings"
	"testing"
	"time"
)

func transactionFill(id string, created, updated time.Time) string {
	return fmt.Sprintf(`{"deal_id":%q,"order_id":"external-order","code":"US.AAPL","trd_side":"BUY","status":"CHANGED","qty":"100.125000","price":"1.234567890123","create_time":%d,"updated_time":%d}`, id, created.UnixMicro(), updated.UnixMicro())
}

const transactionCash = `{"transaction_name":"Unqualified description","created_time":"10/26/2026 15:53:16","cash_change":"-1.234567890123","balance":"-10.123456789012","symbol":""}`

func TestTransactionCashDisplayTextIsNeverNormalized(t *testing.T) {
	raw := json.RawMessage(`{"transaction_name":"Unqualified description","created_time":"10/26/2026 15:53:16","cash_change":"+1,234.56","balance":"Balance: 10,000.00 USD","symbol":""}`)
	before := bytes.Clone(raw)
	if id, err := validateCashRecord(raw); err != nil || id != "" || !bytes.Equal(raw, before) {
		t.Fatal("cash display values were normalized or given a fabricated event ID")
	}
}

func TestTransactionPagesPreservePrecisionCaseAndCashDuplicates(t *testing.T) {
	now := time.Now().UTC()
	rowA := transactionFill("AbC", now.Add(-time.Hour), now.Add(-time.Minute))
	rowB := transactionFill("abc", now.Add(-time.Hour), now.Add(-time.Minute))
	base := url.Values{"trd_market": {"US"}, "page_size": {"100"}}
	calls := 0
	pages, err := collectTransactionPages(t.Context(), func(_ context.Context, query url.Values) (json.RawMessage, error) {
		calls++
		if query.Get("trd_market") != "US" || query.Get("code") != "" || base.Get("page_flag") != "" {
			t.Fatal("coverage narrowed or base query mutated")
		}
		if calls == 1 {
			return json.RawMessage(`{"order_fills":[` + rowA + `],"completed":false,"page_flag":"next"}`), nil
		}
		if query.Get("page_flag") != "next" {
			t.Fatal("cursor not followed")
		}
		return json.RawMessage(`{"order_fills":[` + rowB + `],"completed":true,"page_flag":""}`), nil
	}, base, "order_fills", 100, func(raw json.RawMessage) (string, error) { return validateFillRecord(raw, now.Add(-24*time.Hour), now) })
	if err != nil || len(pages) != 2 || calls != 2 || !strings.Contains(string(pages[0].Data), `"price":"1.234567890123"`) {
		t.Fatalf("exact complete fill pages lost: %v", err)
	}
	saved, err := json.MarshalIndent(pages, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	var decoded []TransactionPage
	if err := json.Unmarshal(saved, &decoded); err != nil || !bytes.Equal(decoded[0].Data, pages[0].Data) || decoded[0].SHA256 != pages[0].SHA256 {
		t.Fatal("private JSON persistence changed original page bytes/hash")
	}
	pages, err = collectTransactionPages(t.Context(), func(context.Context, url.Values) (json.RawMessage, error) {
		return json.RawMessage(`{"cash_logs":[` + transactionCash + `,` + transactionCash + `],"completed":true,"page_flag":""}`), nil
	}, url.Values{}, "cash_logs", 20, validateCashRecord)
	if err != nil || len(pages) != 1 || strings.Count(string(pages[0].Data), "Unqualified description") != 2 {
		t.Fatalf("cash records without IDs were deduplicated: %v", err)
	}
}

func TestTransactionHistoryUsesUpdateTimeAndRejectsMissingFacts(t *testing.T) {
	now := time.Now().UTC()
	from := now.Add(-time.Hour)
	raw := transactionFill("case-sensitive-ID", from.Add(-24*time.Hour), from.Add(time.Minute))
	if _, err := validateFillRecord(json.RawMessage(raw), from, now); err != nil {
		t.Fatalf("creation predating update-time filter rejected: %v", err)
	}
	live := strings.Replace(raw, `"updated_time":`, `"update_time":`, 1)
	if _, err := validateFillRecord(json.RawMessage(live), from, now); err != nil {
		t.Fatalf("authenticated REST update-time spelling rejected: %v", err)
	}
	nonmonotonic := transactionFill("independent-clocks", now.Add(-time.Minute), from.Add(time.Minute))
	if _, err := validateFillRecord(json.RawMessage(nonmonotonic), from, now); err != nil {
		t.Fatalf("independent reported clocks were used as an unsupported ordering gate: %v", err)
	}
	conflict := strings.TrimSuffix(raw, "}") + fmt.Sprintf(`,"update_time":%d}`, now.UnixMicro())
	if _, err := validateFillRecord(json.RawMessage(conflict), from, now); err == nil {
		t.Fatal("conflicting documented/live update clocks accepted")
	}
	for key, value := range map[string]any{"updated_time": from.Add(-time.Minute).UnixMicro(), "create_time": now.UnixMilli(),
		"deal_id": "", "order_id": "", "code": "HK.00700", "qty": "1e2", "price": "1/2", "status": "ASSIGNED", "trd_side": "unknown"} {
		// Unmarshal timestamps as raw bytes for mutation; avoid float conversion.
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &fields); err != nil {
			t.Fatal(err)
		}
		fields[key], _ = json.Marshal(value)
		changed, _ := json.Marshal(fields)
		if _, err := validateFillRecord(changed, from, now); err == nil {
			t.Fatalf("invalid %s accepted", key)
		}
	}
}

func TestTransactionPagesNeverReturnPartialCoverage(t *testing.T) {
	now := time.Now()
	row := transactionFill("same-id", now.Add(-time.Hour), now.Add(-time.Minute))
	for name, raw := range map[string]string{
		"missing-completed": `{"order_fills":[],"page_flag":""}`,
		"null-completed":    `{"order_fills":[],"page_flag":"","completed":null}`,
		"wrong-case":        `{"order_fills":[],"page_flag":"","Completed":true}`,
		"null-list":         `{"order_fills":null,"page_flag":"","completed":true}`,
		"missing-cursor":    `{"order_fills":[],"completed":true}`,
		"empty-cursor":      `{"order_fills":[],"page_flag":"","completed":false}`,
		"repeated-cursor":   `{"order_fills":[],"page_flag":"same","completed":false}`,
		"duplicate-id":      `{"order_fills":[` + row + `,` + row + `],"completed":true,"page_flag":""}`,
	} {
		t.Run(name, func(t *testing.T) {
			pages, err := collectTransactionPages(t.Context(), func(context.Context, url.Values) (json.RawMessage, error) { return json.RawMessage(raw), nil },
				url.Values{}, "order_fills", 100, func(raw json.RawMessage) (string, error) { return validateFillRecord(raw, time.Time{}, now) })
			if err == nil || pages != nil {
				t.Fatal("incomplete or ambiguous pagination produced evidence packet")
			}
		})
	}
	calls := 0
	pages, err := collectTransactionPages(t.Context(), func(context.Context, url.Values) (json.RawMessage, error) {
		calls++
		return json.RawMessage(fmt.Sprintf(`{"order_fills":[],"completed":false,"page_flag":"page-%d"}`, calls)), nil
	}, url.Values{}, "order_fills", 100, nil)
	if err == nil || pages != nil || calls != 64 {
		t.Fatal("page cap returned truncated coverage")
	}
}

func TestTransactionAuditBoundAccountAndReadOnlyRequests(t *testing.T) {
	client := NewClient(evidenceCredential(t))
	since := time.Now().Add(-24 * time.Hour).Truncate(time.Microsecond)
	calls := []string{}
	client.http = &http.Client{Transport: reviewTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.Host != "webapi.moomoo.com" || r.URL.Query().Get("code") != "" || r.URL.Query().Get("direction") != "" {
			t.Fatal("audit performed writes, left pinned origin or narrowed coverage")
		}
		calls = append(calls, r.URL.Path)
		body := `{"order_fills":[],"completed":true,"page_flag":""}`
		switch r.URL.Path {
		case "/api/v1.0/accounts/authorized_trd_accs":
			body = `{"accounts":[{"account_id":9007199254740993,"security_firm":"FUTUINC","acc_type":"margin","enable_market":[2]}]}`
		case "/api/v1.0/accounts/9007199254740993/order_fills":
			if r.URL.Query().Get("page_size") != "100" {
				t.Fatal("today size")
			}
		case "/api/v1.0/accounts/9007199254740993/fills_history":
			if r.URL.Query().Get("start") != fmt.Sprint(since.UnixMicro()) || r.URL.Query().Get("end") == "" || r.URL.Query().Get("page_size") != "50" {
				t.Fatal("explicit update-time history missing")
			}
		case "/api/v1.0/accounts/9007199254740993/cash_logs":
			if r.URL.Query().Get("begin_time") != fmt.Sprint(since.UnixMicro()) || r.URL.Query().Get("end_time") == "" || r.URL.Query().Get("page_size") != "20" {
				t.Fatal("explicit cash interval missing")
			}
			body = `{"cash_logs":[` + transactionCash + `],"completed":true,"page_flag":""}`
		default:
			t.Fatal("unexpected read")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"s":"ok","d":` + body + `}`))}, nil
	})}
	got, err := client.AuditTransactions(t.Context(), "9007199254740993", since)
	if err != nil || got == nil || len(calls) != 4 || got.ExecutionQualified || got.FillClockOrderingQualified || !got.CashValuesAreDisplayText || len(got.CashLogs) != 1 || got.FinishedAt.Before(got.At) {
		t.Fatalf("read-only complete audit: %v", err)
	}
	client.http.Transport = reviewTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/api/v1.0/accounts/authorized_trd_accs" {
			t.Fatal("scoped read before authorization")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"s":"ok","d":{"accounts":[]}}`))}, nil
	})
	if got, err := client.AuditTransactions(t.Context(), "9007199254740993", since); err == nil || got != nil {
		t.Fatal("foreign account produced an audit")
	}
	client.http.Transport = reviewTransport(func(*http.Request) (*http.Response, error) { return nil, errors.New("network must not be reached") })
	for _, from := range []time.Time{{}, time.Now().Add(time.Hour)} {
		if got, err := client.AuditTransactions(t.Context(), "9007199254740993", from); err == nil || got != nil {
			t.Fatal("unbounded time interval admitted")
		}
	}
}
