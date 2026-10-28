package moomoo

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"autowheel/internal/wheel"
)

func receiptFixture(t *testing.T) (OrderIntentTerms, time.Time, *OrderAudit, map[string]any) {
	t.Helper()
	now := time.Date(2026, time.October, 26, 19, 0, 0, 0, time.UTC)
	want := OrderIntentTerms{AccountID: "9007199254740993", Ref: "638d9553-7520-49b0-b691-5662506e6c66", Code: "US.AAPL261030P100000", Side: "SELL", Quantity: 1, LimitCents: 125, CreatedAt: now.Add(-20 * time.Second)}
	row := map[string]any{"order_id": "broker-id", "code": want.Code, "side": want.Side, "order_type": "LIMIT", "order_status": "SUBMITTED", "remark": want.Ref,
		"create_time": now.Add(-19 * time.Second).UnixMicro(), "updated_time": now.Add(-5 * time.Second).UnixMicro(), "security_type": "OPTION", "exchange": "US", "currency": "USD",
		"qty": "1.000000", "price": "1.2500", "time_in_force": "DAY", "session": "RTH", "dealt_qty": "0", "dealt_avg_price": "", "aux_price": "0.000", "trail_type": "NONE", "algo_info": nil, "multi_leg_info": nil, "extra_info": map[string]bool{"is_ec_combo": false}}
	audit := &OrderAudit{AccountID: want.AccountID, At: now.Add(-time.Second), Market: "US", HistoryFrom: want.CreatedAt.Add(-time.Minute), HistoryTo: now.Add(-time.Second), RecentPages: 1, HistoryPages: 1, Recent: []OrderEvidence{}, History: []OrderEvidence{}}
	audit.Recent = append(audit.Recent, receiptEvidence(t, row))
	return want, now, audit, row
}

func receiptEvidence(t *testing.T, row map[string]any) OrderEvidence {
	t.Helper()
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	var remark *string
	if value, ok := row["remark"].(string); ok {
		remark = new(value)
	}
	return OrderEvidence{ID: row["order_id"].(string), Code: row["code"].(string), Side: row["side"].(string), Type: row["order_type"].(string), Status: row["order_status"].(string), Remark: remark,
		CreatedAt: time.UnixMicro(row["create_time"].(int64)), UpdatedAt: time.UnixMicro(row["updated_time"].(int64)), Raw: raw}
}

func TestReceiptMatchesExactDuplicateWithoutMutatingAudit(t *testing.T) {
	want, now, audit, row := receiptFixture(t)
	row["price"], row["qty"] = "1.25", "1"
	audit.History = append(audit.History, receiptEvidence(t, row))
	before, _ := json.Marshal(audit)
	matched, err := MatchOrderReceipt(audit, want, now)
	if err != nil || matched.ID != "broker-id" || matched.Remark == nil || *matched.Remark != want.Ref {
		t.Fatalf("exact receipt rejected: %v", err)
	}
	matched.Raw[0] = 'x'
	*matched.Remark = "changed"
	after, _ := json.Marshal(audit)
	if string(before) != string(after) {
		t.Fatal("returned receipt shared mutable evidence with the audit")
	}
}

func TestReceiptAbsenceDuplicatesAndConflictsStayUnresolved(t *testing.T) {
	for _, mode := range []string{"absent", "two-ids", "conflicting-clock", "conflicting-price", "summary-tamper", "duplicate-within-query"} {
		t.Run(mode, func(t *testing.T) {
			want, now, audit, row := receiptFixture(t)
			switch mode {
			case "absent":
				audit.Recent = []OrderEvidence{}
			case "two-ids":
				row["order_id"] = "second-id"
				audit.History = append(audit.History, receiptEvidence(t, row))
			case "conflicting-clock":
				row["updated_time"] = now.Add(-4 * time.Second).UnixMicro()
				audit.History = append(audit.History, receiptEvidence(t, row))
			case "conflicting-price":
				row["price"] = "1.26"
				audit.History = append(audit.History, receiptEvidence(t, row))
			case "summary-tamper":
				audit.Recent[0].Remark = nil
			case "duplicate-within-query":
				audit.Recent = append(audit.Recent, audit.Recent[0])
			}
			_, err := MatchOrderReceipt(audit, want, now)
			if err == nil || errors.Is(err, wheel.ErrNotSubmitted) {
				t.Fatal("unresolved receipt authorized retry or was accepted")
			}
		})
	}
}

func TestReceiptRejectsMissingDifferentAndConditionalTerms(t *testing.T) {
	for key, value := range map[string]any{"code": "US.AAPL261030C100000", "side": "BUY", "qty": "2", "price": "1.25001", "time_in_force": "GTC", "session": "ALL_DAY", "currency": "HKD", "exchange": "SEHK", "security_type": "MULTILEG_OPTION", "order_type": "STOP_LIMIT", "order_status": "UNKNOWN", "aux_price": "1", "trail_value": "0.01", "trail_type": "AMOUNT", "order_class": "MLEG", "position_open_type": "ENABLE_POSITION_OPEN", "expire_time": int64(1), "sub_acc_id": "other", "rfq_id": "rfq", "algo_info": map[string]any{"start_time": 1}, "multi_leg_info": map[string]any{"leg_infos": []any{1}}, "extra_info": map[string]any{"is_ec_combo": nil}, "dealt_qty": "0.5", "dealt_avg_price": "0.001"} {
		t.Run(key, func(t *testing.T) {
			want, now, audit, row := receiptFixture(t)
			row[key] = value
			audit.Recent[0] = receiptEvidence(t, row)
			if _, err := MatchOrderReceipt(audit, want, now); err == nil {
				t.Fatal("mismatched or conditional receipt accepted")
			}
		})
	}
	for _, key := range []string{"price", "qty", "currency", "exchange", "security_type", "time_in_force", "session", "dealt_qty", "dealt_avg_price"} {
		t.Run("missing-"+key, func(t *testing.T) {
			want, now, audit, row := receiptFixture(t)
			delete(row, key)
			audit.Recent[0] = receiptEvidence(t, row)
			if _, err := MatchOrderReceipt(audit, want, now); err == nil {
				t.Fatal("missing receipt fact inferred")
			}
		})
	}
}

func TestReceiptRequiresFreshCompleteBoundAudit(t *testing.T) {
	for _, mode := range []string{"account", "stale", "future", "history-gap", "missing-completion", "nil-history", "future-intent", "invalid-ref", "known-id-mismatch"} {
		t.Run(mode, func(t *testing.T) {
			want, now, audit, _ := receiptFixture(t)
			switch mode {
			case "account":
				audit.AccountID = "9007199254740994"
			case "stale":
				audit.At = now.Add(-31 * time.Second)
			case "future":
				audit.At = now.Add(time.Second)
			case "history-gap":
				audit.HistoryFrom = want.CreatedAt.Add(time.Second)
			case "missing-completion":
				audit.HistoryPages = 0
			case "nil-history":
				audit.History = nil
			case "future-intent":
				want.CreatedAt = now.Add(time.Second)
			case "invalid-ref":
				want.Ref = strings.Repeat("x", 65)
			case "known-id-mismatch":
				want.OrderID = "different-id"
			}
			if _, err := MatchOrderReceipt(audit, want, now); err == nil {
				t.Fatal("unproven audit/intent accepted")
			}
		})
	}
}

func TestReceiptRequiresConsistentWholeContractFill(t *testing.T) {
	want, now, audit, row := receiptFixture(t)
	row["order_status"], row["dealt_qty"], row["dealt_avg_price"] = "FILLED_ALL", "1.00000000", "1.25"
	audit.Recent[0] = receiptEvidence(t, row)
	if _, err := MatchOrderReceipt(audit, want, now); err != nil {
		t.Fatal(err)
	}
	row["dealt_avg_price"] = "1.24"
	audit.Recent[0] = receiptEvidence(t, row)
	if _, err := MatchOrderReceipt(audit, want, now); err == nil {
		t.Fatal("sell fill below limit accepted")
	}
	row["dealt_avg_price"] = "1.25"
	row["dealt_qty"] = "0"
	audit.Recent[0] = receiptEvidence(t, row)
	if _, err := MatchOrderReceipt(audit, want, now); err == nil {
		t.Fatal("filled status with zero contracts accepted")
	}
}

func TestResolveOrderReceiptUsesOnlyFreshAccountScopedReads(t *testing.T) {
	want, _, audit, row := receiptFixture(t)
	now := time.Now()
	want.CreatedAt = now.Add(-20 * time.Second)
	row["create_time"], row["updated_time"] = now.Add(-19*time.Second).UnixMicro(), now.Add(-5*time.Second).UnixMicro()
	raw := receiptEvidence(t, row).Raw
	client := NewClient(evidenceCredential(t))
	calls := 0
	client.http = &http.Client{Transport: reviewTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Method != http.MethodGet || req.URL.Host != "webapi.moomoo.com" || req.URL.Scheme != "https" {
			t.Fatal("receipt recovery attempted an order transport or foreign origin")
		}
		body := `{"accounts":[{"account_id":9007199254740993,"security_firm":"FUTUINC","acc_type":"cash","enable_market":[2]}]}`
		if req.URL.Path != "/api/v1.0/accounts/authorized_trd_accs" {
			if req.URL.Path != "/api/v1.0/accounts/"+want.AccountID+"/orders" && req.URL.Path != "/api/v1.0/accounts/"+want.AccountID+"/orders_history" {
				t.Fatal("recovery read was not bound to the intent account")
			}
			q := req.URL.Query()
			if q.Get("trd_market") != "US" || q.Get("code") != "" || q.Get("order_status") != "" {
				t.Fatal("recovery narrowed its order coverage")
			}
			if strings.HasSuffix(req.URL.Path, "/orders_history") && q.Get("start") != strconv.FormatInt(want.CreatedAt.UnixMicro(), 10) {
				t.Fatal("history omitted the durable intent interval")
			}
			body = `{"orders":[` + string(raw) + `],"page_flag":"","completed":true}`
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"s":"ok","d":` + body + `}`))}, nil
	})}
	matched, err := client.ResolveOrderReceipt(t.Context(), want)
	if err != nil || matched.ID != audit.Recent[0].ID || calls != 3 {
		t.Fatalf("fresh scoped recovery failed: calls=%d error=%v", calls, err)
	}
	want.Ref = "invalid"
	if _, err := client.ResolveOrderReceipt(t.Context(), want); err == nil || calls != 3 {
		t.Fatal("invalid intent reached the broker")
	}
}
