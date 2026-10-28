package moomoo

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"
)

func auditOrderFixture(id string) string {
	return fmt.Sprintf(`{"order_id":%q,"code":"US.AAPL261030P100000","side":"SELL_SHORT","order_type":"STOP_LIMIT","order_status":"SUBMITTED","remark":"durable-intent","create_time":%d,"updated_time":%d,"qty":"1","price":"1.2345"}`, id, time.Now().Add(-time.Hour).UnixMicro(), time.Now().Add(-time.Minute).UnixMicro())
}

func TestOrderAuditPaginationRetainsExactEvidence(t *testing.T) {
	base := url.Values{"trd_market": {"US"}, "page_size": {"100"}}
	calls := 0
	orders, pages, err := collectOrderPages(t.Context(), func(_ context.Context, query url.Values) (json.RawMessage, error) {
		calls++
		if query.Get("trd_market") != "US" || query.Get("code") != "" || query.Get("order_status") != "" {
			t.Fatal("order coverage was narrowed")
		}
		if calls == 1 {
			if query.Get("page_flag") != "" {
				t.Fatal("first cursor")
			}
			return json.RawMessage(`{"orders":[` + auditOrderFixture("9007199254740993") + `],"page_flag":"second","completed":false}`), nil
		}
		if query.Get("page_flag") != "second" {
			t.Fatal("cursor not followed")
		}
		return json.RawMessage(`{"orders":[` + auditOrderFixture("9007199254740994") + `],"page_flag":"","completed":true}`), nil
	}, base)
	if err != nil || pages != 2 || calls != 2 || len(orders) != 2 {
		t.Fatalf("full query evidence: %v %#v %d %d", err, orders, pages, calls)
	}
	if orders[0].ID != "9007199254740993" || !strings.Contains(string(orders[0].Raw), `"price":"1.2345"`) || orders[0].Type != "STOP_LIMIT" || orders[0].Remark == nil || *orders[0].Remark != "durable-intent" {
		t.Fatal("order identity, conditional type, reference or decimal precision lost")
	}
	if base.Get("page_flag") != "" {
		t.Fatal("pagination mutated caller's base query")
	}
}

func TestOrderAuditRejectsIncompleteOrAmbiguousPages(t *testing.T) {
	cases := map[string]string{
		"missing-completion":       `{"orders":[],"page_flag":""}`,
		"missing-list":             `{"page_flag":"","completed":true}`,
		"null-list":                `{"orders":null,"page_flag":"","completed":true}`,
		"missing-cursor":           `{"orders":[],"completed":false}`,
		"empty-nonterminal-cursor": `{"orders":[],"page_flag":"","completed":false}`,
		"repeated-cursor":          `{"orders":[],"page_flag":"same","completed":false}`,
		"duplicate-order":          `{"orders":[` + auditOrderFixture("same") + `,` + auditOrderFixture("same") + `],"page_flag":"","completed":true}`,
		"missing-clocks":           `{"orders":[{"order_id":"id","code":"US.AAPL","side":"BUY","order_type":"LIMIT","order_status":"FILLED_ALL"}],"page_flag":"","completed":true}`,
		"millisecond-clock":        fmt.Sprintf(`{"orders":[{"order_id":"id","code":"US.AAPL","side":"BUY","order_type":"LIMIT","order_status":"FILLED_ALL","remark":"","create_time":%d,"updated_time":%d}],"page_flag":"","completed":true}`, time.Now().UnixMilli(), time.Now().UnixMilli()),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			orders, _, err := collectOrderPages(t.Context(), func(context.Context, url.Values) (json.RawMessage, error) { return json.RawMessage(raw), nil }, url.Values{"trd_market": {"US"}})
			if err == nil || orders != nil {
				t.Fatalf("partial/ambiguous audit accepted: %#v %v", orders, err)
			}
		})
	}
}

func TestOrderEvidenceRetainsUnavailableReference(t *testing.T) {
	raw := strings.Replace(auditOrderFixture("historical-order"), `"remark":"durable-intent",`, "", 1)
	order, err := parseOrderEvidence(json.RawMessage(raw))
	if err != nil || order.Remark != nil || string(order.Raw) != raw {
		t.Fatalf("missing historical remark was lost or fabricated: %v", err)
	}
}
