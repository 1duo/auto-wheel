package sharedbroker

import (
	"bytes"
	"encoding/json"
	"errors"
)

// ValidateProjection proves that a strategy view contains exact broker rows
// from the complete account. It never fabricates a strategy-specific NAV.
func ValidateProjection(full Facts, owned Object) error {
	for name, expected := range map[string]any{"account": full.Account, "accounts": full.Accounts, "portfolio": full.Portfolio} {
		actual, present := owned[name]
		if !present || !sameJSON(expected, actual) {
			return errors.New("owned projection changed complete account identity or NAV")
		}
	}
	for _, collection := range []struct {
		name, key, id string
		full          []Object
	}{
		{"positions", "positions", "symbol", full.Positions},
		{"orders", "orders", "id", full.Orders},
		{"option_positions", "positions", "option_id", full.Options},
		{"option_orders", "orders", "id", full.OptionOrders},
	} {
		container, ok := owned[collection.name].(map[string]any)
		if !ok || len(container) != 1 {
			return errors.New("owned projection lacks complete inventory collections")
		}
		rows, ok := container[collection.key].([]any)
		if !ok {
			return errors.New("owned projection collection is missing")
		}
		index := make(map[string]Object)
		for _, row := range collection.full {
			id := Text(row, collection.id)
			if id == "" || index[id] != nil {
				return errors.New("complete account row identity is missing or duplicated")
			}
			index[id] = row
		}
		seen := make(map[string]bool)
		for _, value := range rows {
			row, ok := value.(map[string]any)
			id := Text(row, collection.id)
			if !ok || seen[id] || index[id] == nil || !sameJSON(row, index[id]) {
				return errors.New("owned row is duplicated or differs from complete account evidence")
			}
			seen[id] = true
		}
	}
	return nil
}

func sameJSON(a, b any) bool {
	left, err := json.Marshal(a)
	if err != nil {
		return false
	}
	right, err := json.Marshal(b)
	return err == nil && bytes.Equal(left, right)
}
