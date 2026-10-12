package robinhood

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"autowheel/internal/wheel"
)

type standardSnapshotClient struct {
	*obligationsClient
	asset        string
	missingAsset bool
}

func (c standardSnapshotClient) Call(ctx context.Context, tool string, args object) (any, error) {
	v, err := c.obligationsClient.Call(ctx, tool, args)
	if tool == "get_portfolio" && err == nil {
		for _, key := range []string{"crypto_value", "event_contracts_value", "fixed_income_value", "futures_value", "mutual_funds_value"} {
			v.(object)[key] = "0"
		}
		if c.asset != "" {
			v.(object)["event_contracts_value"] = c.asset
		}
		if c.missingAsset {
			delete(v.(object), "futures_value")
		}
	}
	return v, err
}

func TestExplicitStandardSnapshotKeepsOtherObligationGates(t *testing.T) {
	for _, name := range []string{"missing_advanced_allowed", "default_missing_advanced_blocked", "pending_approval", "crypto_hold", "new_crypto_order", "crypto_missing_time", "settlement", "invalid_policy", "unsupported_asset", "unproven_asset"} {
		t.Run(name, func(t *testing.T) {
			fake := &obligationsClient{settlementSnapshotClient: settlementSnapshotClient{position: object{"quantity": "0", "pending_assignment_quantity": "0", "pending_expiration_quantity": "0", "pending_exercise_quantity": "0"}}, account: object{"rhc_account_number": "RHC-test", "rhs_account_number": "87654321"}, orders: []any{}, positions: []any{}, approvalFlag: false, approvals: []any{}}
			c := wheel.DefaultConfig()
			c.Account = "12345678"
			c.OrderCoveragePolicy = "exclusive_standard"
			c.ExclusiveOrdersSince = time.Now().Add(-time.Hour)
			c.TradePulseStateDir = "/private/retired-peer"
			c.RetiredTradePulse = filepath.Join(c.TradePulseStateDir, "supervisor", "cross-host-handoff-sealed.json")
			switch name {
			case "default_missing_advanced_blocked":
				c.OrderCoveragePolicy = ""
				c.ExclusiveOrdersSince = time.Time{}
			case "pending_approval":
				fake.approvals = []any{object{"state": "APPROVAL_STATE_PENDING"}}
			case "crypto_hold":
				fake.positions = []any{object{"quantity": "0", "quantity_transferable": "0", "quantity_held_for_sell": "1"}}
			case "new_crypto_order":
				fake.orders = []any{object{"state": "filled", "created_at": time.Now().Format(time.RFC3339Nano), "updated_at": time.Now().Format(time.RFC3339Nano)}}
			case "crypto_missing_time":
				fake.orders = []any{object{"state": "filled"}}
			case "settlement":
				fake.position["pending_assignment_quantity"] = "1"
			case "invalid_policy":
				c.RetiredTradePulse = ""
			}
			client := standardSnapshotClient{obligationsClient: fake}
			if name == "unsupported_asset" {
				client.asset = "1"
			}
			if name == "unproven_asset" {
				client.missingAsset = true
			}
			b := &Broker{MCP: client, Config: c}
			s, err := b.Snapshot(t.Context())
			if name == "missing_advanced_allowed" {
				if err != nil || !b.AccountInfo.ExecutionSnapshotUsable || b.AccountInfo.SnapshotComplete || b.AccountInfo.CoverageWarning != wheel.StandardOrderWarning || s.OrderCoveragePolicy != c.OrderCoveragePolicy {
					t.Fatal("restricted facts mislabelled or blocked", s, err)
				}
				if fake.cryptoReads != 2 {
					t.Fatal("linked cash reads omitted")
				}
			} else if err == nil || b.AccountInfo != nil && b.AccountInfo.ExecutionSnapshotUsable {
				t.Fatal("unsupported facts admitted", name, err)
			}
		})
	}
}
