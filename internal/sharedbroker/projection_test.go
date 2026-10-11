package sharedbroker

import (
	"encoding/json"
	"testing"
)

func TestOwnedProjectionCannotRewriteOrDuplicateBrokerRows(t *testing.T) {
	full := Facts{Account: Object{"account_number": "1234"}, Accounts: Object{"accounts": []any{}}, Portfolio: Object{"equity": "10000.00"}, Positions: []Object{{"symbol": "AAA", "quantity": "1.00", "shares_available_for_sells": "1.00"}}}
	owned := Object{"account": full.Account, "accounts": full.Accounts, "portfolio": full.Portfolio, "positions": Object{"positions": []any{full.Positions[0]}}, "orders": Object{"orders": []any{}}, "option_positions": Object{"positions": []any{}}, "option_orders": Object{"orders": []any{}}}
	if err := ValidateProjection(full, owned); err != nil {
		t.Fatal(err)
	}
	copyOwned := func() Object { raw, _ := json.Marshal(owned); var o Object; json.Unmarshal(raw, &o); return o }
	changed := copyOwned()
	changed["portfolio"].(map[string]any)["equity"] = "100.00"
	if ValidateProjection(full, changed) == nil {
		t.Fatal("virtual NAV accepted")
	}
	changed = copyOwned()
	row := changed["positions"].(map[string]any)["positions"].([]any)[0].(map[string]any)
	row["shares_available_for_sells"] = "2.00"
	if ValidateProjection(full, changed) == nil {
		t.Fatal("raw inventory terms changed")
	}
	changed = copyOwned()
	changed["positions"].(map[string]any)["positions"] = []any{full.Positions[0], full.Positions[0]}
	if ValidateProjection(full, changed) == nil {
		t.Fatal("duplicated inventory accepted")
	}
	changed = copyOwned()
	delete(changed, "option_positions")
	if ValidateProjection(full, changed) == nil {
		t.Fatal("missing collection accepted")
	}
}
