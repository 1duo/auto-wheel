package moomoo

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	aruntime "autowheel/internal/runtime"
)

func TestObservationExactAndMissingFacts(t *testing.T) {
	for _, raw := range []string{`{}`, `{"currency":"USD"}`, `{"currency":"HKD","available_funds":"1.00"}`, `{"currency":"USD","available_funds":"1e3"}`, `{"currency":"USD","available_funds":"1.0000000000001"}`, `{"currency":"USD","available_funds":"1000000000.0001"}`, `{"currency":"USD","available_funds":"+1.00"}`} {
		if _, err := parseFunds([]byte(raw)); err == nil {
			t.Fatalf("ambiguous funds accepted: %s", raw)
		}
	}
	if value, err := parseFunds([]byte(`{"currency":"USD","available_funds":"-12.34"}`)); err != nil || value.Cents == nil || *value.Cents != -1234 || value.Text != "-12.34" {
		t.Fatalf("signed observed funds: %v %v", value, err)
	}
	if value, err := parseFunds([]byte(`{"currency":"USD","available_funds":"12.3456789"}`)); err != nil || value.Cents != nil || value.Text != "12.3456789" {
		t.Fatalf("sub-cent reported funds were rounded or lost: %#v %v", value, err)
	}
	for _, raw := range []string{`null`, `{}`, `[{"code":"US.AAPL","qty":"1.0000001","can_sell_qty":"1","position_side":"LONG","currency":"USD"}]`, `[{"code":"US.AAPL","qty":"NaN","can_sell_qty":"0","position_side":"LONG","currency":"USD"}]`} {
		if _, err := parseHoldings([]byte(raw)); err == nil {
			t.Fatalf("ambiguous holdings accepted: %s", raw)
		}
	}
	holdings, err := parseHoldings([]byte(`[{"code":"US.AAPL261030P100000","qty":"-1","can_sell_qty":"0","position_side":"SHORT","currency":"USD","security_type":"OPTION"},{"code":"US.AAPL","qty":"100.125","can_sell_qty":"100","position_side":"LONG","currency":"USD"}]`))
	if err != nil || len(holdings) != 2 || holdings[0].Quantity != "100.125" || holdings[1].Quantity != "-1" || holdings[0].Ownership != "external_unclaimed" {
		t.Fatalf("lossless unclaimed stock/options: %#v %v", holdings, err)
	}
	if _, err := parseAccounts([]byte(`{"accounts":[{"account_id":9007199254740993,"security_firm":"FUTUINC","acc_type":"cash","enable_market":[2]},{"account_id":9007199254740993,"security_firm":"FUTUINC","acc_type":"cash","enable_market":[2]}]}`)); err == nil {
		t.Fatal("duplicate business account accepted")
	}
	accounts, err := parseAccounts([]byte(`{"accounts":[{"account_id":9007199254740993,"security_firm":"FUTUINC","acc_type":"cash","enable_market":[2]}]}`))
	if err != nil || len(accounts) != 1 || accounts[0].ID != "9007199254740993" {
		t.Fatalf("uint64 account precision lost: %#v %v", accounts, err)
	}
}

func TestReadOnlyCredentialAndMCPBoundary(t *testing.T) {
	for _, scope := range []string{"trade:read", "quote:read trade:read trade:write", "quote:read trade:read quote:write", "quote:read trade:read accid:../other", "quote:read trade:read unknown"} {
		if readOnlyScopes(scope) {
			t.Fatalf("unsafe scopes accepted: %s", scope)
		}
	}
	if !readOnlyScopes("quote:read trade:read accid:123456") {
		t.Fatal("read-only account grant rejected")
	}
	client := NewClient(filepath.Join(t.TempDir(), "missing"))
	if _, err := client.rpc(t.Context(), "tools/call", nil, false); err == nil {
		t.Fatal("tool invocation not blocked before authentication")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")
	for _, resource := range []string{"https://agent.robinhood.com/mcp/trading", "https://mcp.moomoo.com.evil.test"} {
		credential := Credential{Resource: resource, ClientID: "client", AccessToken: "secret", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour), Scope: "quote:read trade:read"}
		if err := aruntime.AtomicJSON(path, credential); err != nil {
			t.Fatal(err)
		}
		if _, err := (&Tokens{Path: path}).Access(t.Context(), secureHTTP()); err == nil {
			t.Fatal("another resource's credential accepted")
		}
	}
}

func TestMCPInventoryPaginationAndIdentity(t *testing.T) {
	for _, broken := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "wrong-response-id"}[broken], func(t *testing.T) {
			var methods []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					ID     int64             `json:"id"`
					Method string            `json:"method"`
					Params map[string]string `json:"params"`
				}
				// initialize has non-string capabilities, so decode it separately.
				var raw map[string]json.RawMessage
				if json.NewDecoder(r.Body).Decode(&raw) != nil {
					t.Error("request JSON")
				}
				json.Unmarshal(raw["id"], &req.ID)
				json.Unmarshal(raw["method"], &req.Method)
				_ = json.Unmarshal(raw["params"], &req.Params)
				methods = append(methods, req.Method)
				if req.Method == "notifications/initialized" {
					w.WriteHeader(http.StatusAccepted)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				var result any = map[string]any{"protocolVersion": protocol}
				if req.Method == "tools/list" {
					name, cursor := "get_funds", "page2"
					if req.Params["cursor"] == "page2" {
						name, cursor = "place_order", ""
					}
					result = map[string]any{"tools": []any{map[string]any{"name": name, "inputSchema": map[string]string{"type": "object"}}}, "nextCursor": cursor}
				}
				if broken {
					req.ID++
				}
				json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
			}))
			defer server.Close()
			path := filepath.Join(t.TempDir(), "token.json")
			if err := aruntime.AtomicJSON(path, Credential{Resource: Resource, ClientID: "client", AccessToken: "secret", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour), Scope: "quote:read trade:read"}); err != nil {
				t.Fatal(err)
			}
			client := NewClient(path)
			client.endpoint = server.URL
			err := client.Connect(t.Context())
			if broken {
				if err == nil || len(client.Inventory()) != 0 {
					t.Fatal("mismatched response accepted")
				}
				return
			}
			if err != nil || len(client.Inventory()) != 2 || len(methods) != 4 {
				t.Fatalf("full inventory: %v %#v %#v", err, client.Inventory(), methods)
			}
		})
	}
}
