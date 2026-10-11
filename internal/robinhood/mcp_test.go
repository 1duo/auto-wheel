package robinhood

import (
	aruntime "autowheel/internal/runtime"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestMCPReadBoundaryResponseIdentityAndSSE(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.json")
	if err := aruntime.AtomicJSON(path, Credential{Resource: Endpoint, ClientID: "test-client", AccessToken: "test-access", RefreshToken: "test-refresh", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	mismatch := false
	applicationError := false
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-access" {
			t.Error("token header missing")
		}
		var body object
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		id := body["id"]
		method := body["method"]
		var result any
		switch method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "test-session")
			result = object{"protocolVersion": protocol}
		case "notifications/initialized":
			w.WriteHeader(202)
			return
		case "tools/list":
			result = object{"tools": []object{{"name": "get_accounts", "inputSchema": object{"type": "object"}}}}
		case "tools/call":
			name := body["params"].(map[string]any)["name"]
			if name == "place_option_order" {
				writes++
			}
			result = object{"structuredContent": object{"accounts": []any{}}, "isError": applicationError}
		}
		if mismatch {
			id = 9999
		}
		w.Header().Set("Content-Type", "text/event-stream")
		b, _ := json.Marshal(object{"jsonrpc": "2.0", "id": id, "result": result})
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
	}))
	defer server.Close()
	c := NewClient(path, false)
	c.endpoint = server.URL
	c.http = server.Client()
	if err := c.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Call(t.Context(), "get_accounts", object{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Call(t.Context(), "place_option_order", object{}); err == nil || writes != 0 {
		t.Fatal("read-only transport submitted")
	}
	mismatch = true
	if _, err := c.Call(t.Context(), "get_accounts", object{}); err == nil {
		t.Fatal("wrong RPC ID accepted")
	}
	mismatch = false
	applicationError = true
	if _, err := c.Call(t.Context(), "get_accounts", object{}); err == nil {
		t.Fatal("tool error accepted as data")
	}
}

func TestProviderCompletenessAndDecimalGuards(t *testing.T) {
	if _, _, err := rows(object{"data": object{}}, "positions"); err == nil {
		t.Fatal("missing collection treated as empty")
	}
	got, next, err := rows(object{"data": object{"results": []any{}, "next": "https://api.robinhood.com/positions/?cursor=next-page"}}, "positions")
	if err != nil || len(got) != 0 || next != "next-page" {
		t.Fatal("pagination cursor lost")
	}
	if _, err := integer(object{"quantity": "99.5"}, "quantity"); err == nil {
		t.Fatal("fractional coverage rounded")
	}
	if _, err := money(object{"cash": "1e5"}, "cash"); err == nil {
		t.Fatal("exponential money accepted")
	}
	if _, err := instrument(object{"chain_symbol": "NVDA", "type": "put", "expiration_date": "2026-10-09", "strike_price": "200", "trade_value_multiplier": "10"}, "contract"); err == nil {
		t.Fatal("adjusted multiplier accepted")
	}
}

func TestLiveSchemaUnionsAndExternalEquityHistory(t *testing.T) {
	schema := object{"type": []any{"null", "array"}, "items": object{"type": "string"}}
	if err := validateValue(schema, []string{"NVDA"}); err != nil {
		t.Fatal("Robinhood nullable symbol array rejected:", err)
	}
	if err := validateValue(schema, 100); err == nil {
		t.Fatal("nullable schema accepted the wrong input type")
	}
	legSchema := object{"type": []any{"null", "array"}, "items": object{"type": "object", "properties": object{"side": object{"type": "string"}}, "required": []any{"side"}}}
	if err := validateValue(legSchema, []object{{}}); err == nil {
		t.Fatal("type union bypassed required nested leg properties")
	}
	b := &Broker{}
	order := object{"id": "external-order", "state": "filled", "symbol": "AAPL", "quantity": "7.747945", "cumulative_quantity": "7.747945", "created_at": "2026-10-01T14:00:00Z"}
	got, err := b.order(t.Context(), order, false)
	if err != nil || got.Symbol != "AAPL" || got.CreatedAt.IsZero() || got.Quantity != 0 {
		t.Fatal("fractional equity history was rounded or lost its quarantine evidence", got, err)
	}
	order["state"] = "queued"
	if _, err := b.order(t.Context(), order, false); err == nil {
		t.Fatal("working external equity order did not block")
	}
}
